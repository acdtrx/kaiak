package accounting

import (
	"encoding/json"
)

// messagesUsage reads Anthropic Messages responses (docs/specs/GATEWAY.md, Accounting
// → Messages usage): a body's top-level usage; a stream's message_start
// message.usage, then each message_delta usage, the latest value of each field
// winning — vLLM repeats input_tokens in message_delta, llama-server sends only
// output_tokens there. Anthropic's input_tokens already leaves out the tokens read
// from and written to the cache, so the three input units come straight from the
// report; output_tokens includes thinking, which the format does not report apart.
type messagesUsage struct {
	in, cacheRead, cacheWrite, out *int64
	// contentBytes counts generated content seen in stream events.
	contentBytes int64
}

// messagesUsageReport is the Messages usage object.
type messagesUsageReport struct {
	InputTokens              *int64 `json:"input_tokens"`
	CacheCreationInputTokens *int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     *int64 `json:"cache_read_input_tokens"`
	OutputTokens             *int64 `json:"output_tokens"`
}

// merge takes the fields raw reports, each replacing the one held. A null, missing
// or malformed usage changes nothing.
func (u *messagesUsage) merge(raw json.RawMessage) {
	if len(raw) == 0 || raw[0] != '{' {
		return
	}
	var r messagesUsageReport
	if json.Unmarshal(raw, &r) != nil {
		return
	}
	for _, f := range []struct{ held, reported **int64 }{
		{&u.in, &r.InputTokens}, {&u.cacheWrite, &r.CacheCreationInputTokens},
		{&u.cacheRead, &r.CacheReadInputTokens}, {&u.out, &r.OutputTokens},
	} {
		if *f.reported != nil {
			*f.held = *f.reported
		}
	}
}

// messagesEvent is what the meter reads of one Messages stream event.
type messagesEvent struct {
	Type    string `json:"type"`
	Message *struct {
		Usage json.RawMessage `json:"usage"`
	} `json:"message"`
	Usage        json.RawMessage `json:"usage"`
	ContentBlock *messagesBlock  `json:"content_block"`
	Delta        *struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		Thinking    string `json:"thinking"`
		PartialJSON string `json:"partial_json"`
	} `json:"delta"`
}

// messagesBlock is a content block: generated text, thinking, or a tool call's input.
type messagesBlock struct {
	Type     string          `json:"type"`
	Text     string          `json:"text"`
	Thinking string          `json:"thinking"`
	Input    json.RawMessage `json:"input"`
}

// streamEvent reads one stream event by its data's type: usage from message_start and
// message_delta, generated content from content blocks and their deltas. Content
// blocks may interleave (llama-server opens one before closing the previous), so each
// event counts on its own.
func (u *messagesUsage) streamEvent(payload []byte) {
	var ev messagesEvent
	// A member of an unexpected type is skipped; the others are still read. Invalid
	// JSON fills nothing.
	_ = json.Unmarshal(payload, &ev)
	switch ev.Type {
	case "message_start":
		if ev.Message != nil {
			u.merge(ev.Message.Usage)
		}
	case "message_delta":
		u.merge(ev.Usage)
	case "content_block_start":
		if ev.ContentBlock != nil {
			u.contentBytes += int64(len(ev.ContentBlock.Text) + len(ev.ContentBlock.Thinking))
		}
	case "content_block_delta":
		if d := ev.Delta; d != nil {
			switch d.Type {
			case "text_delta":
				u.contentBytes += int64(len(d.Text))
			case "thinking_delta":
				u.contentBytes += int64(len(d.Thinking))
			case "input_json_delta":
				u.contentBytes += int64(len(d.PartialJSON))
			}
		}
	}
}

func (u *messagesUsage) contentMember() string { return "content" }

func (u *messagesUsage) bodyUsage(raw json.RawMessage) { u.merge(raw) }

// bodyContentBytes counts a Messages body's generated content: text, thinking and
// tool-call inputs (their JSON as sent). Signatures and block structure do not count.
func (u *messagesUsage) bodyContentBytes(raw json.RawMessage) int64 {
	var blocks []json.RawMessage
	if json.Unmarshal(raw, &blocks) != nil {
		return int64(len(raw))
	}
	var n int64
	for _, b := range blocks {
		var block messagesBlock
		// Unmarshal fills every field it can before reporting a type mismatch.
		_ = json.Unmarshal(b, &block)
		n += int64(len(block.Text) + len(block.Thinking))
		if block.Type == "tool_use" {
			n += int64(len(block.Input))
		}
	}
	return n
}

// reported maps the fields held onto the units: input_tokens → tokens_in,
// cache_read_input_tokens → tokens_cached, cache_creation_input_tokens →
// tokens_cache_write, output_tokens → tokens_out; tokens_reasoning 0. Missing fields
// count 0. No report until input_tokens or output_tokens arrived.
func (u *messagesUsage) reported() Units {
	if u.in == nil && u.out == nil {
		return nil
	}
	return tokenUnits(nonNegative(u.in), nonNegative(u.cacheRead), nonNegative(u.cacheWrite), nonNegative(u.out), 0)
}

func (u *messagesUsage) streamContentBytes() int64 { return u.contentBytes }
