package accounting

import (
	"encoding/json"
)

// responsesUsage reads OpenAI Responses answers (docs/specs/GATEWAY.md, Accounting →
// Responses usage): a body's top-level usage; a stream's response.usage in
// response.completed or response.incomplete. input_tokens includes the cache, as
// OpenAI's prompt_tokens does, so the cached and written tokens come out of it; output
// tokens include reasoning, reported apart in output_tokens_details.
type responsesUsage struct {
	// latest is the latest usage the backend reported; nil when none yet.
	latest Units
	// contentBytes counts generated content seen in stream events.
	contentBytes int64
}

// responsesUsageReport is the Responses usage object. Detail fields are optional:
// llama-server reports no output details.
type responsesUsageReport struct {
	InputTokens        *int64 `json:"input_tokens"`
	OutputTokens       *int64 `json:"output_tokens"`
	InputTokensDetails *struct {
		CachedTokens     int64 `json:"cached_tokens"`
		CacheWriteTokens int64 `json:"cache_write_tokens"`
	} `json:"input_tokens_details"`
	OutputTokensDetails *struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
}

// parse maps a usage object onto the units: tokens_cached = cached, tokens_cache_write
// = written, tokens_in = input − cached − written, tokens_out = output (reasoning
// included), tokens_reasoning = the reasoning share of it. Clamped as OpenAI's
// usage is: cached ≤ input first, then written ≤ what remains, reasoning ≤ output. A
// null, missing or malformed usage, or one with neither token count, is no report.
func (u *responsesUsage) parse(raw json.RawMessage) (Units, bool) {
	if len(raw) == 0 || raw[0] != '{' {
		return nil, false
	}
	var r responsesUsageReport
	if json.Unmarshal(raw, &r) != nil || (r.InputTokens == nil && r.OutputTokens == nil) {
		return nil, false
	}
	input, output := nonNegative(r.InputTokens), nonNegative(r.OutputTokens)
	var cached, written, reasoning int64
	if d := r.InputTokensDetails; d != nil {
		cached = min(max(d.CachedTokens, 0), input)
		written = min(max(d.CacheWriteTokens, 0), input-cached)
	}
	if d := r.OutputTokensDetails; d != nil {
		reasoning = min(max(d.ReasoningTokens, 0), output)
	}
	return tokenUnits(input-cached-written, cached, written, output, reasoning), true
}

// responsesEvent is what the meter reads of one Responses stream event.
type responsesEvent struct {
	Type     string `json:"type"`
	Delta    string `json:"delta"`
	Response *struct {
		Usage json.RawMessage `json:"usage"`
	} `json:"response"`
}

// responsesContentDeltas are the stream events whose delta is generated content:
// answer text and refusals, reasoning text and summaries, function-call arguments and
// a custom tool's input.
var responsesContentDeltas = map[string]bool{
	"response.output_text.delta":             true,
	"response.refusal.delta":                 true,
	"response.reasoning_text.delta":          true,
	"response.reasoning_summary_text.delta":  true,
	"response.function_call_arguments.delta": true,
	"response.custom_tool_call_input.delta":  true,
}

// streamEvent reads one stream event by its data's type: usage from the response the
// final event carries, generated content from the deltas. Output items may overlap
// (llama-server adds a function call before its reasoning item is done), so each
// event counts on its own.
func (u *responsesUsage) streamEvent(payload []byte) {
	var ev responsesEvent
	// A member of an unexpected type is skipped; the others are still read. Invalid
	// JSON fills nothing.
	_ = json.Unmarshal(payload, &ev)
	switch {
	case ev.Type == "response.completed" || ev.Type == "response.incomplete":
		if ev.Response != nil {
			if units, ok := u.parse(ev.Response.Usage); ok {
				u.latest = units
			}
		}
	case responsesContentDeltas[ev.Type]:
		u.contentBytes += int64(len(ev.Delta))
	}
}

func (u *responsesUsage) contentMember() string { return "output" }

func (u *responsesUsage) bodyUsage(raw json.RawMessage) {
	if units, ok := u.parse(raw); ok {
		u.latest = units
	}
}

// responsesItem is an output item: a message, a reasoning item, a function call or a
// custom tool call.
type responsesItem struct {
	Type    string `json:"type"`
	Content []struct {
		Text    string `json:"text"`
		Refusal string `json:"refusal"`
	} `json:"content"`
	Summary []struct {
		Text string `json:"text"`
	} `json:"summary"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	Input     string `json:"input"`
}

// bodyContentBytes counts a Responses body's generated content: message text and
// refusals, reasoning text and summaries, and the names and arguments (or input) of
// tool calls, as UTF-8 bytes after JSON decoding. Structure, IDs and statuses do not
// count.
func (u *responsesUsage) bodyContentBytes(raw json.RawMessage) int64 {
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) != nil {
		return int64(len(raw))
	}
	var n int64
	for _, raw := range items {
		var item responsesItem
		// Unmarshal fills every field it can before reporting a type mismatch.
		_ = json.Unmarshal(raw, &item)
		for _, c := range item.Content {
			n += int64(len(c.Text) + len(c.Refusal))
		}
		for _, s := range item.Summary {
			n += int64(len(s.Text))
		}
		n += int64(len(item.Name) + len(item.Arguments) + len(item.Input))
	}
	return n
}

func (u *responsesUsage) reported() (Units, bool) { return u.latest, true }

func (u *responsesUsage) streamContentBytes() int64 { return u.contentBytes }
