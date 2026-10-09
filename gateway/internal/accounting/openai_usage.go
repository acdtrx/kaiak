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
	latestReport
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
	u.keep(u.parse(chunk.Usage))
	for _, c := range chunk.Choices {
		u.contentBytes += choiceContentBytes(c)
	}
}

func (u *openAIUsage) contentMember() string { return "choices" }

func (u *openAIUsage) bodyUsage(raw json.RawMessage) { u.keep(u.parse(raw)) }

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

// parse maps a usage object onto the units: prompt_tokens includes the cache and
// completion_tokens the reasoning (inclusiveUnits). A null, missing or malformed
// usage, or one with neither token count, is no report. Embeddings count prompt
// tokens only, and a usage without them is no report (promptOnlyUnits).
func (u *openAIUsage) parse(raw json.RawMessage) (Units, bool) {
	if u.endpoint == provider.Embeddings {
		return promptOnlyUnits(raw)
	}
	r, ok := decodeUsageReport(raw)
	if !ok {
		return nil, false
	}
	var cached, written, reasoning int64
	if d := r.PromptTokensDetails; d != nil {
		cached, written = d.CachedTokens, d.CacheWriteTokens
	}
	if d := r.CompletionTokensDetails; d != nil {
		reasoning = d.ReasoningTokens
	}
	return inclusiveUnits(r.PromptTokens, r.CompletionTokens, cached, written, reasoning)
}

// decodeUsageReport decodes an OpenAI-shaped usage object; ok is false for a null,
// missing or malformed one.
func decodeUsageReport(raw json.RawMessage) (usageReport, bool) {
	var r usageReport
	if len(raw) == 0 || raw[0] != '{' || json.Unmarshal(raw, &r) != nil {
		return usageReport{}, false
	}
	return r, true
}

// promptOnlyUnits maps the OpenAI-shaped usage object of an answer that generates
// nothing — embeddings, rerank (docs/specs/GATEWAY.md, Accounting) — onto the units:
// prompt_tokens as tokens_in, every other unit 0. prompt_tokens is the only count
// such an answer has: a usage without it (absent or null) is no report whatever else
// it carries, completion_tokens included, as is a null, missing or malformed usage.
func promptOnlyUnits(raw json.RawMessage) (Units, bool) {
	r, ok := decodeUsageReport(raw)
	if !ok || r.PromptTokens == nil {
		return nil, false
	}
	return withEveryTokenUnit(Units{config.UnitTokensIn: nonNegative(r.PromptTokens)}), true
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
