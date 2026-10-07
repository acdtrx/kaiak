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

// stopReason is a Messages answer's stop_reason.
func stopReason(cut bool) string {
	if cut {
		return "max_tokens"
	}
	return "end_turn"
}

func writeMessage(w http.ResponseWriter, model, text string, cut bool, usage Usage, omitUsage bool) {
	answer := map[string]any{
		"id": "msg_fake_1", "type": "message", "role": "assistant", "model": model,
		"content":     []any{map[string]any{"type": "text", "text": text}},
		"stop_reason": stopReason(cut), "stop_sequence": nil,
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

// writeMessagesStream streams a Messages answer: message_start, one text block with a
// delta per chunk, message_delta with the stop reason and output usage, message_stop.
// The reply's stream fault applies with messagesErrorEvent as the error event.
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
	if !event("message_start", map[string]any{"type": "message_start", "message": message}) ||
		!event("content_block_start", map[string]any{"type": "content_block_start", "index": 0,
			"content_block": map[string]any{"type": "text", "text": ""}}) {
		return
	}
	for i, text := range chunks {
		if !s.interrupt(i) {
			return
		}
		if !event("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0,
			"delta": map[string]any{"type": "text_delta", "text": text}}) {
			return
		}
	}
	delta := map[string]any{"type": "message_delta",
		"delta": map[string]any{"stop_reason": stopReason(cut), "stop_sequence": nil}}
	if !reply.OmitUsage {
		delta["usage"] = map[string]any{"output_tokens": usage.CompletionTokens}
	}
	if !event("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0}) ||
		!event("message_delta", delta) {
		return
	}
	event("message_stop", map[string]any{"type": "message_stop"})
}

// messagesErrorBody is an error in Anthropic's shape.
func messagesErrorBody(message string) map[string]any {
	return map[string]any{"type": "error", "error": map[string]any{"type": "invalid_request_error", "message": message}}
}
