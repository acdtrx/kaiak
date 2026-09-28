package accounting

import (
	"bytes"
	"encoding/json"
	"sync/atomic"

	"kaiak/internal/config"
	"kaiak/internal/provider"
)

// Units maps usage units to amounts (docs/specs/CONTROL-PROTOCOL.md, Prices). Every
// record from a token endpoint carries all four token units, zeros included.
type Units map[config.Unit]int64

func tokenUnits(in, cached, out, reasoning int64) Units {
	return Units{
		config.UnitTokensIn:        in,
		config.UnitTokensCached:    cached,
		config.UnitTokensOut:       out,
		config.UnitTokensReasoning: reasoning,
	}
}

// bytesPerToken is the estimation heuristic: about 4 bytes of text per token. There is
// no tokenizer — that would need a dependency, and per-model vocabularies.
const bytesPerToken = 4

// EstimateTokens estimates the tokens in n bytes of text (docs/specs/GATEWAY.md,
// Limits and Accounting), rounding up. EstimateInput applies it to a request's text.
func EstimateTokens(n int64) int64 {
	if n <= 0 {
		return 0
	}
	return (n + bytesPerToken - 1) / bytesPerToken
}

// Limits on the non-stream body members kept for settlement. A usage object is a few
// hundred bytes; choices are kept only to estimate output when usage is missing, and
// past the limit the estimate falls back to their raw size.
const (
	maxUsageBytes   = 64 << 10
	maxChoicesBytes = 4 << 20
)

// Meter watches one routed attempt's response on its way to the client and works out
// its usage when it is over. It reads responses in the client's format (OpenAI JSON
// and SSE), which every provider hands back. A Meter belongs to one attempt and is
// used from that request's goroutine only — except Sent, which the transport calls.
type Meter struct {
	endpoint provider.Endpoint
	// input is the request's input estimate (EstimateInput), billed when the backend
	// reports no usage.
	input int64

	// sent: the upstream request was written to the backend in full.
	sent atomic.Bool

	answered bool
	status   int
	stream   bool
	// refused: the backend answered by refusing the gateway's credential.
	refused bool

	// reported is the latest usage the backend reported; nil when none yet.
	reported Units
	// contentBytes counts generated content seen in stream chunks.
	contentBytes int64
	// body scans a successful non-stream body.
	body *memberScanner
}

// NewMeter returns the meter for a request to ep whose input is estimated at input
// tokens (EstimateInput).
func NewMeter(ep provider.Endpoint, input int64) *Meter {
	return &Meter{endpoint: ep, input: input}
}

// Answered records that the backend answered with status; stream reports whether the
// body is an event stream.
func (m *Meter) Answered(status int, stream bool) {
	m.answered = true
	m.status = status
	m.stream = stream
	if !stream && m.succeeded() {
		m.body = newMemberScanner(map[string]int{"usage": maxUsageBytes, "choices": maxChoicesBytes})
	}
}

// Sent records that the upstream request was written to the backend in full
// (provider.Request.Sent); safe to call from any goroutine. From then on the backend
// may process the prompt — and bill it, upstream — so an attempt ending before any
// answer counts the input, estimated.
func (m *Meter) Sent() {
	m.sent.Store(true)
}

// Refused records that the backend refused the request outright — the gateway's
// credential, or a model it does not serve: an answer, with nothing processed.
func (m *Meter) Refused() {
	m.refused = true
}

// SentUnanswered reports whether the request reached the backend in full and got no
// answer (a first-event or response timeout, the connection lost, the client gone or the
// drain's cut before the first event): the case Settle bills estimated input for.
func (m *Meter) SentUnanswered() bool {
	return m.sent.Load() && !m.answered && !m.refused
}

// Observe reads one response event, hidden ones (the usage chunk the client did not
// ask for) included. Stream events are read from their payload as the backend sent
// it; body pieces from the bytes relayed.
func (m *Meter) Observe(ev provider.Event) {
	if !m.succeeded() {
		return
	}
	if m.stream {
		m.observeChunk(ev.Payload)
		return
	}
	if m.body != nil {
		m.body.feed(ev.Data)
	}
}

// StreamContentSeen reports whether a successful stream has carried generated content
// yet (text, reasoning, refusal or tool calls — not role-only or usage chunks): the
// moment it turns true is the stream's first token.
func (m *Meter) StreamContentSeen() bool {
	return m.contentBytes > 0
}

func (m *Meter) succeeded() bool {
	return m.status >= 200 && m.status < 300
}

// observeChunk reads one stream chunk: its usage when not null (the last report wins —
// backends that report on every chunk report running totals), and its generated
// content for the estimate.
func (m *Meter) observeChunk(payload []byte) {
	if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
		return
	}
	var chunk struct {
		Usage   json.RawMessage   `json:"usage"`
		Choices []json.RawMessage `json:"choices"`
	}
	// A member of an unexpected type is skipped; the others are still read. Invalid
	// JSON fills nothing.
	_ = json.Unmarshal(payload, &chunk)
	if units, ok := m.parseUsage(chunk.Usage); ok {
		m.reported = units
	}
	for _, c := range chunk.Choices {
		m.contentBytes += choiceContentBytes(c)
	}
}

// Flags qualify a record's units.
type Flags struct {
	// Estimated: the backend reported no usage; units come from the byte heuristic.
	Estimated bool
	// Partial: the request stopped before its response ended (client disconnect,
	// backend failure); units count what was produced up to then.
	Partial bool
}

// Settle works out the attempt's units once it is over. complete reports whether the
// response ran to its end.
//
//   - Sent in full and no answer (SentUnanswered: a first-event or response timeout, the
//     connection lost, the client gone or the drain's cut before the first event):
//     the input estimated from the client's request body, no output; flagged
//     estimated and partial.
//   - No answer otherwise (connection failure, the request not written in full, a
//     refused credential): the backend never had the prompt, or processed nothing —
//     no units; the record says partial and carries zeros.
//   - A backend error status: the backend generated nothing; no units.
//   - Otherwise the backend's usage report when there is one; else input is estimated
//     from the client's request body and output from the generated content seen, and
//     the record is flagged estimated.
func (m *Meter) Settle(complete bool) (Units, Flags) {
	if m.SentUnanswered() {
		return tokenUnits(m.input, 0, 0, 0), Flags{Estimated: true, Partial: true}
	}
	flags := Flags{Partial: !complete || !m.answered}
	if !m.answered || !m.succeeded() {
		return tokenUnits(0, 0, 0, 0), flags
	}
	if !m.stream && m.body != nil {
		if raw, ok := m.body.member("usage"); ok {
			if units, ok := m.parseUsage(raw); ok {
				m.reported = units
			}
		}
	}
	if m.reported != nil {
		return m.reported, flags
	}
	flags.Estimated = true
	return tokenUnits(m.input, 0, EstimateTokens(m.outputBytes()), 0), flags
}

// outputBytes is the generated content seen: stream chunk content, or the content of a
// non-stream body's choices (their raw size when they could not be read whole).
// Embeddings generate nothing.
func (m *Meter) outputBytes() int64 {
	if m.endpoint == provider.Embeddings {
		return 0
	}
	if m.stream || m.body == nil {
		return m.contentBytes
	}
	raw, ok := m.body.member("choices")
	if !ok {
		return int64(m.body.memberSize("choices"))
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

// usageReport is the OpenAI usage object. Detail fields are optional: many backends
// omit them.
type usageReport struct {
	PromptTokens        *int64 `json:"prompt_tokens"`
	CompletionTokens    *int64 `json:"completion_tokens"`
	PromptTokensDetails *struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails *struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

// parseUsage maps a usage object onto the units (docs/specs/CONTROL-PROTOCOL.md,
// Units and price units): tokens_in = prompt − cached, tokens_cached = cached,
// tokens_out = completion (reasoning included), tokens_reasoning = the reasoning
// share of it. Embeddings count prompt tokens only. A null, missing or malformed
// usage, or one with neither token count, is no report.
func (m *Meter) parseUsage(raw json.RawMessage) (Units, bool) {
	if len(raw) == 0 || raw[0] != '{' {
		return nil, false
	}
	var u usageReport
	if json.Unmarshal(raw, &u) != nil || (u.PromptTokens == nil && u.CompletionTokens == nil) {
		return nil, false
	}
	prompt := nonNegative(u.PromptTokens)
	if m.endpoint == provider.Embeddings {
		return tokenUnits(prompt, 0, 0, 0), true
	}
	completion := nonNegative(u.CompletionTokens)
	var cached, reasoning int64
	if d := u.PromptTokensDetails; d != nil {
		cached = min(max(d.CachedTokens, 0), prompt)
	}
	if d := u.CompletionTokensDetails; d != nil {
		reasoning = min(max(d.ReasoningTokens, 0), completion)
	}
	return tokenUnits(prompt-cached, cached, completion, reasoning), true
}

func nonNegative(n *int64) int64 {
	if n == nil || *n < 0 {
		return 0
	}
	return *n
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
