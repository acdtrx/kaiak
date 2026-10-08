package accounting

import (
	"bytes"
	"encoding/json"

	"kaiak/internal/config"
	"kaiak/internal/provider"
)

// openAIUsage reads OpenAI-format responses (chat completions, completions,
// embeddings): a body's top-level usage, a stream's last non-null usage, and the
// generated content of the choices (docs/specs/GATEWAY.md, Accounting).
type openAIUsage struct {
	endpoint provider.Endpoint
	// latest is the latest usage the backend reported; nil when none yet.
	latest Units
	// contentBytes counts generated content seen in stream chunks.
	contentBytes int64
}

// streamEvent reads one stream chunk: its usage when not null (the last report wins —
// backends that report on every chunk report running totals), and its generated
// content for the estimate.
func (u *openAIUsage) streamEvent(payload []byte) {
	if bytes.Equal(payload, []byte("[DONE]")) {
		return
	}
	var chunk struct {
		Usage   json.RawMessage   `json:"usage"`
		Choices []json.RawMessage `json:"choices"`
	}
	// A member of an unexpected type is skipped; the others are still read. Invalid
	// JSON fills nothing.
	_ = json.Unmarshal(payload, &chunk)
	if units, ok := u.parse(chunk.Usage); ok {
		u.latest = units
	}
	for _, c := range chunk.Choices {
		u.contentBytes += choiceContentBytes(c)
	}
}

func (u *openAIUsage) contentMember() string { return "choices" }

func (u *openAIUsage) bodyUsage(raw json.RawMessage) {
	if units, ok := u.parse(raw); ok {
		u.latest = units
	}
}

// bodyContentBytes counts the choices' generated content. Embeddings generate
// nothing.
func (u *openAIUsage) bodyContentBytes(raw json.RawMessage) int64 {
	if u.endpoint == provider.Embeddings {
		return 0
	}
	var choices []json.RawMessage
	if json.Unmarshal(raw, &choices) != nil {
		return int64(len(raw))
	}
	var n int64
	for _, c := range choices {
		n += choiceContentBytes(c)
	}
	return n
}

func (u *openAIUsage) reported() (Units, bool) { return u.latest, true }

func (u *openAIUsage) streamContentBytes() int64 {
	if u.endpoint == provider.Embeddings {
		return 0
	}
	return u.contentBytes
}

// usageReport is the OpenAI usage object. Detail fields are optional: many backends
// omit them.
type usageReport struct {
	PromptTokens        *int64 `json:"prompt_tokens"`
	CompletionTokens    *int64 `json:"completion_tokens"`
	PromptTokensDetails *struct {
		CachedTokens     int64 `json:"cached_tokens"`
		CacheWriteTokens int64 `json:"cache_write_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails *struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

// parse maps a usage object onto the units (docs/specs/CONTROL-PROTOCOL.md,
// Units and price units): tokens_cached = cached, tokens_cache_write = written,
// tokens_in = prompt − cached − written, tokens_out = completion (reasoning
// included), tokens_reasoning = the reasoning share of it. Cached is clamped to
// prompt first, then written to what remains, so the three input units add up to
// prompt. Embeddings count prompt tokens only. A null, missing or malformed usage, or
// one with neither token count, is no report.
func (u *openAIUsage) parse(raw json.RawMessage) (Units, bool) {
	if len(raw) == 0 || raw[0] != '{' {
		return nil, false
	}
	var r usageReport
	if json.Unmarshal(raw, &r) != nil || (r.PromptTokens == nil && r.CompletionTokens == nil) {
		return nil, false
	}
	prompt := nonNegative(r.PromptTokens)
	if u.endpoint == provider.Embeddings {
		return withEveryTokenUnit(Units{config.UnitTokensIn: prompt}), true
	}
	completion := nonNegative(r.CompletionTokens)
	var cached, written, reasoning int64
	if d := r.PromptTokensDetails; d != nil {
		cached = min(max(d.CachedTokens, 0), prompt)
		written = min(max(d.CacheWriteTokens, 0), prompt-cached)
	}
	if d := r.CompletionTokensDetails; d != nil {
		reasoning = min(max(d.ReasoningTokens, 0), completion)
	}
	return withEveryTokenUnit(Units{
		config.UnitTokensIn:         prompt - cached - written,
		config.UnitTokensCached:     cached,
		config.UnitTokensCacheWrite: written,
		config.UnitTokensOut:        completion,
		config.UnitTokensReasoning:  reasoning,
	}), true
}

// generatedContent is where a choice carries generated text: a chat message (non-
// stream) or delta (stream), or a completion's text.
type generatedContent struct {
	Text    string       `json:"text"`
	Message *chatContent `json:"message"`
	Delta   *chatContent `json:"delta"`
}

type chatContent struct {
	Content string `json:"content"`
	Refusal string `json:"refusal"`
	// Reasoning text from reasoning parsers (vLLM, SGLang); some servers send the
	// same text under both names, so only one is counted.
	Reasoning        string `json:"reasoning"`
	ReasoningContent string `json:"reasoning_content"`
	ToolCalls        []struct {
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	} `json:"tool_calls"`
}

// choiceContentBytes counts the generated content in one choice: text, message or
// delta content, refusal, reasoning text and tool-call names and arguments, as UTF-8
// bytes after JSON decoding. Structure (keys, roles, finish reasons, indexes) is not
// generated content and is not counted. A field of an unexpected type counts nothing;
// the rest of the choice still counts.
func choiceContentBytes(raw json.RawMessage) int64 {
	var c generatedContent
	// Unmarshal fills every field it can before reporting a type mismatch.
	_ = json.Unmarshal(raw, &c)
	n := int64(len(c.Text))
	for _, msg := range []*chatContent{c.Message, c.Delta} {
		if msg == nil {
			continue
		}
		n += int64(len(msg.Content) + len(msg.Refusal) + max(len(msg.Reasoning), len(msg.ReasoningContent)))
		for _, call := range msg.ToolCalls {
			n += int64(len(call.Function.Name) + len(call.Function.Arguments))
		}
	}
	return n
}
