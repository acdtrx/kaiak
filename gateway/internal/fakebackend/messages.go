package fakebackend

import (
	"encoding/json"
	"net/http"
)

// Anthropic Messages answers, shaped as Anthropic's API and the self-hosted servers
// answer them (the recorded answers in captures/ show the real ones).

// messagesUsage is a Usage as a Messages answer reports it: input_tokens leaves out
// the tokens read from and written to the cache, which are reported apart.
func messagesUsage(u Usage, withOutput bool) map[string]any {
	body := map[string]any{"input_tokens": u.PromptTokens - u.CachedTokens - u.CacheWriteTokens}
	if u.CachedTokens > 0 {
		body["cache_read_input_tokens"] = u.CachedTokens
	}
	if u.CacheWriteTokens > 0 {
		body["cache_creation_input_tokens"] = u.CacheWriteTokens
	}
	if withOutput {
		body["output_tokens"] = u.CompletionTokens
	}
	return body
}

// stopReason is a Messages answer's stop_reason: max_tokens when the output limit cut
// it short, else tool_use when it calls tools.
func stopReason(cut bool, calls []ToolCall) string {
	switch {
	case cut:
		return "max_tokens"
	case len(calls) > 0:
		return "tool_use"
	}
	return "end_turn"
}

// toolUseBlock is a call as a Messages tool_use block, its input whole — {} for a call
// without arguments.
func toolUseBlock(c ToolCall) map[string]any {
	input := json.RawMessage(c.arguments())
	if len(input) == 0 {
		input = json.RawMessage("{}")
	}
	return map[string]any{"type": "tool_use", "id": c.ID, "name": c.Name, "input": input}
}

// thinkingSignature is the signature of every thinking block the backend writes.
const thinkingSignature = "fake-signature"

// writeMessage answers a Messages request: a thinking block holding reasoning when
// there is any, then the text block, then a tool_use block per call.
func writeMessage(w http.ResponseWriter, model, reasoning, text string, calls []ToolCall, cut bool, usage Usage,
	omitUsage bool) {
	content := []any{map[string]any{"type": "text", "text": text}}
	if reasoning != "" {
		content = append([]any{map[string]any{"type": "thinking", "thinking": reasoning, "signature": thinkingSignature}},
			content...)
	}
	for _, c := range calls {
		content = append(content, toolUseBlock(c))
	}
	answer := map[string]any{
		"id": "msg_fake_1", "type": "message", "role": "assistant", "model": model, "content": content,
		"stop_reason": stopReason(cut, calls), "stop_sequence": nil,
	}
	if !omitUsage {
		answer["usage"] = messagesUsage(usage, true)
	}
	writeJSON(w, http.StatusOK, answer)
}

// messagesErrorEvent is the error event a Messages stream sends when the backend gives
// up: errorType, or overloaded_error (what Anthropic's API sends mid-stream) when it
// is "".
func messagesErrorEvent(errorType string) []byte {
	if errorType == "" {
		errorType = "overloaded_error"
	}
	return []byte(`{"type":"error","error":{"type":"` + errorType + `","message":"Overloaded"}}`)
}

// writeMessagesStream streams a Messages answer: message_start; with reasoning, a
// thinking block — a delta per piece, then its signature; one text block with a delta
// per chunk; a tool_use block per call — its start with an empty input, an
// input_json_delta per argument piece; message_delta with the stop reason and output
// usage, message_stop. The reply's stream fault applies with messagesErrorEvent as the
// error event.
func (b *Backend) writeMessagesStream(w http.ResponseWriter, r *http.Request, req *Request, reply Reply,
	model string, chunks []string, cut bool, usage Usage) {
	s, ok := b.startStream(w, r, req, reply, messagesErrorEvent)
	if !ok {
		return
	}
	event := func(name string, v any) bool {
		payload, _ := json.Marshal(v) // test values always encode
		return s.send(name, payload)
	}
	if !s.interrupt(0) {
		return
	}
	message := map[string]any{"id": "msg_fake_1", "type": "message", "role": "assistant", "content": []any{},
		"model": model, "stop_reason": nil, "stop_sequence": nil}
	if !reply.OmitUsage {
		message["usage"] = messagesUsage(usage, false)
	}
	if !event("message_start", map[string]any{"type": "message_start", "message": message}) {
		return
	}
	index := 0
	if len(reply.Reasoning) > 0 {
		if !event("content_block_start", map[string]any{"type": "content_block_start", "index": index,
			"content_block": map[string]any{"type": "thinking", "thinking": ""}}) {
			return
		}
		for _, piece := range reply.Reasoning {
			if !event("content_block_delta", map[string]any{"type": "content_block_delta", "index": index,
				"delta": map[string]any{"type": "thinking_delta", "thinking": piece}}) {
				return
			}
		}
		if !event("content_block_delta", map[string]any{"type": "content_block_delta", "index": index,
			"delta": map[string]any{"type": "signature_delta", "signature": thinkingSignature}}) ||
			!event("content_block_stop", map[string]any{"type": "content_block_stop", "index": index}) {
			return
		}
		index++
	}
	if !event("content_block_start", map[string]any{"type": "content_block_start", "index": index,
		"content_block": map[string]any{"type": "text", "text": ""}}) {
		return
	}
	for i, text := range chunks {
		if !s.interrupt(i) {
			return
		}
		if !event("content_block_delta", map[string]any{"type": "content_block_delta", "index": index,
			"delta": map[string]any{"type": "text_delta", "text": text}}) {
			return
		}
	}
	if !event("content_block_stop", map[string]any{"type": "content_block_stop", "index": index}) {
		return
	}
	for _, c := range reply.ToolCalls {
		index++
		start := toolUseBlock(c)
		start["input"] = map[string]any{}
		if !event("content_block_start", map[string]any{"type": "content_block_start", "index": index,
			"content_block": start}) {
			return
		}
		for _, piece := range c.Arguments {
			if !event("content_block_delta", map[string]any{"type": "content_block_delta", "index": index,
				"delta": map[string]any{"type": "input_json_delta", "partial_json": piece}}) {
				return
			}
		}
		if !event("content_block_stop", map[string]any{"type": "content_block_stop", "index": index}) {
			return
		}
	}
	delta := map[string]any{"type": "message_delta",
		"delta": map[string]any{"stop_reason": stopReason(cut, reply.ToolCalls), "stop_sequence": nil}}
	if !reply.OmitUsage {
		delta["usage"] = map[string]any{"output_tokens": usage.CompletionTokens}
	}
	if !event("message_delta", delta) {
		return
	}
	event("message_stop", map[string]any{"type": "message_stop"})
}

// messagesErrorBody is an error in Anthropic's shape.
func messagesErrorBody(message string) map[string]any {
	return map[string]any{"type": "error", "error": map[string]any{"type": "invalid_request_error", "message": message}}
}
