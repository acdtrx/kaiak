package fakebackend

import (
	"encoding/json"
	"net/http"
)

// OpenAI Responses answers, shaped as OpenAI's API and the self-hosted servers answer
// them (the recorded answers in captures/ show the real ones).

// responsesUsage is a Usage as a Responses answer reports it: input_tokens includes
// the cached and written tokens, reported apart in input_tokens_details as OpenAI
// does; reasoning tokens in output_tokens_details.
func responsesUsage(u Usage) map[string]any {
	inputDetails := map[string]any{"cached_tokens": u.CachedTokens}
	if u.CacheWriteTokens > 0 {
		inputDetails["cache_write_tokens"] = u.CacheWriteTokens
	}
	return map[string]any{
		"input_tokens": u.PromptTokens, "input_tokens_details": inputDetails,
		"output_tokens":         u.CompletionTokens,
		"output_tokens_details": map[string]any{"reasoning_tokens": u.ReasoningTokens},
		"total_tokens":          u.PromptTokens + u.CompletionTokens,
	}
}

// responseObject is a Responses response object; output and usage are added when the
// answer is done. It reports the request's store and service_tier back, as OpenAI's
// response object does: store true when the request leaves it out (OpenAI's
// default), service_tier only when the request sets one.
func responseObject(model, status string, top map[string]json.RawMessage) map[string]any {
	response := map[string]any{"id": "resp_fake_1", "object": "response", "created_at": created, "model": model,
		"status": status, "output": []any{}, "store": true}
	var store bool
	if json.Unmarshal(top["store"], &store) == nil {
		response["store"] = store
	}
	var tier string
	if json.Unmarshal(top["service_tier"], &tier) == nil && tier != "" {
		response["service_tier"] = tier
	}
	return response
}

// messageItem is the answer's one output message, holding text.
func messageItem(text string) map[string]any {
	return map[string]any{"id": "msg_fake_1", "type": "message", "role": "assistant", "status": "completed",
		"content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}}}
}

// finishResponse completes a response object: its output, its status (incomplete with
// its reason when the output limit cut it short) and its usage.
func finishResponse(response map[string]any, text string, cut bool, usage Usage, omitUsage bool) {
	response["output"] = []any{messageItem(text)}
	response["status"] = "completed"
	if cut {
		response["status"] = "incomplete"
		response["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
	}
	if !omitUsage {
		response["usage"] = responsesUsage(usage)
	}
}

func writeResponse(w http.ResponseWriter, top map[string]json.RawMessage, model, text string, cut bool, usage Usage,
	omitUsage bool) {
	response := responseObject(model, "in_progress", top)
	finishResponse(response, text, cut, usage, omitUsage)
	writeJSON(w, http.StatusOK, response)
}

// responsesError is the error event a Responses stream sends when the backend gives
// up: code, or server_error when it is "".
func responsesError(code string) []byte {
	if code == "" {
		code = "server_error"
	}
	return []byte(`{"type":"error","code":"` + code + `","message":"The server had an error.","param":null}`)
}

// writeResponsesStream streams a Responses answer: response.created and
// response.in_progress, one message item with one output_text part and a delta per
// chunk, then response.completed — response.incomplete when the output limit cut it
// short — carrying the whole response and its usage. The reply's stream fault
// applies with responsesError as the error event.
func (b *Backend) writeResponsesStream(w http.ResponseWriter, r *http.Request, req *Request, reply Reply,
	top map[string]json.RawMessage, model string, chunks []string, cut bool, usage Usage) {
	s, ok := b.startStream(w, r, req, reply, responsesError)
	if !ok {
		return
	}
	event := func(v map[string]any) bool {
		payload, _ := json.Marshal(v) // test values always encode
		return s.send(v["type"].(string), payload)
	}
	if !s.interrupt(0) {
		return
	}
	response := responseObject(model, "in_progress", top)
	item := map[string]any{"id": "msg_fake_1", "type": "message", "role": "assistant", "status": "in_progress", "content": []any{}}
	part := map[string]any{"type": "output_text", "text": "", "annotations": []any{}}
	if !event(map[string]any{"type": "response.created", "response": response}) ||
		!event(map[string]any{"type": "response.in_progress", "response": response}) ||
		!event(map[string]any{"type": "response.output_item.added", "output_index": 0, "item": item}) ||
		!event(map[string]any{"type": "response.content_part.added", "item_id": "msg_fake_1", "output_index": 0,
			"content_index": 0, "part": part}) {
		return
	}
	text := ""
	for i, chunk := range chunks {
		if !s.interrupt(i) {
			return
		}
		if !event(map[string]any{"type": "response.output_text.delta", "item_id": "msg_fake_1", "output_index": 0,
			"content_index": 0, "delta": chunk}) {
			return
		}
		text += chunk
	}
	if !event(map[string]any{"type": "response.output_text.done", "item_id": "msg_fake_1", "output_index": 0,
		"content_index": 0, "text": text}) ||
		!event(map[string]any{"type": "response.output_item.done", "output_index": 0, "item": messageItem(text)}) {
		return
	}
	finishResponse(response, text, cut, usage, reply.OmitUsage)
	final := "response.completed"
	if cut {
		final = "response.incomplete"
	}
	event(map[string]any{"type": final, "response": response})
}
