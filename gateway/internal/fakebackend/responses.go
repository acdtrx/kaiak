package fakebackend

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
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

// reasoningItem is the answer's reasoning, as vLLM gives it: a reasoning item with no
// summary and the reasoning as its reasoning_text content.
func reasoningItem(reasoning string) map[string]any {
	return map[string]any{"id": "rs_fake_1", "type": "reasoning", "summary": []any{}, "status": "completed",
		"content": []any{map[string]any{"type": "reasoning_text", "text": reasoning}}}
}

// functionCallID is the item ID of the answer's function call i.
func functionCallID(i int) string { return fmt.Sprintf("fc_fake_%d", i+1) }

// functionCallItem is the answer's function call i, with arguments and status.
func functionCallItem(i int, c ToolCall, arguments, status string) map[string]any {
	return map[string]any{"id": functionCallID(i), "type": "function_call", "status": status, "call_id": c.ID,
		"name": c.Name, "arguments": arguments}
}

// finishResponse completes a response object: its output — the reasoning item when
// there is reasoning, then the message, then a function call item per call — its
// status (incomplete with its reason when the output limit cut it short) and its
// usage.
func finishResponse(response map[string]any, reasoning, text string, calls []ToolCall, cut bool, usage Usage,
	omitUsage bool) {
	output := []any{messageItem(text)}
	if reasoning != "" {
		output = append([]any{reasoningItem(reasoning)}, output...)
	}
	for i, c := range calls {
		output = append(output, functionCallItem(i, c, c.arguments(), "completed"))
	}
	response["output"] = output
	response["status"] = "completed"
	if cut {
		response["status"] = "incomplete"
		response["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
	}
	if !omitUsage {
		response["usage"] = responsesUsage(usage)
	}
}

func writeResponse(w http.ResponseWriter, top map[string]json.RawMessage, model, reasoning, text string, calls []ToolCall,
	cut bool, usage Usage, omitUsage bool) {
	response := responseObject(model, "in_progress", top)
	finishResponse(response, reasoning, text, calls, cut, usage, omitUsage)
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
// response.in_progress; with reasoning, a reasoning item with a reasoning_text delta
// per piece; one message item with one output_text part and a delta per chunk; a
// function call item per call — added with empty arguments, a
// function_call_arguments delta per argument piece; then response.completed —
// response.incomplete when the output limit cut it short — carrying the whole
// response and its usage. The reply's stream fault applies with responsesError as the
// error event.
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
	if !event(map[string]any{"type": "response.created", "response": response}) ||
		!event(map[string]any{"type": "response.in_progress", "response": response}) {
		return
	}
	index := 0
	reasoning := strings.Join(reply.Reasoning, "")
	if reasoning != "" {
		added := map[string]any{"id": "rs_fake_1", "type": "reasoning", "summary": []any{}, "content": nil,
			"status": "in_progress"}
		if !event(map[string]any{"type": "response.output_item.added", "output_index": index, "item": added}) {
			return
		}
		for _, piece := range reply.Reasoning {
			if !event(map[string]any{"type": "response.reasoning_text.delta", "item_id": "rs_fake_1", "output_index": index,
				"content_index": 0, "delta": piece}) {
				return
			}
		}
		if !event(map[string]any{"type": "response.output_item.done", "output_index": index,
			"item": reasoningItem(reasoning)}) {
			return
		}
		index++
	}
	item := map[string]any{"id": "msg_fake_1", "type": "message", "role": "assistant", "status": "in_progress", "content": []any{}}
	part := map[string]any{"type": "output_text", "text": "", "annotations": []any{}}
	if !event(map[string]any{"type": "response.output_item.added", "output_index": index, "item": item}) ||
		!event(map[string]any{"type": "response.content_part.added", "item_id": "msg_fake_1", "output_index": index,
			"content_index": 0, "part": part}) {
		return
	}
	text := ""
	for i, chunk := range chunks {
		if !s.interrupt(i) {
			return
		}
		if !event(map[string]any{"type": "response.output_text.delta", "item_id": "msg_fake_1", "output_index": index,
			"content_index": 0, "delta": chunk}) {
			return
		}
		text += chunk
	}
	if !event(map[string]any{"type": "response.output_text.done", "item_id": "msg_fake_1", "output_index": index,
		"content_index": 0, "text": text}) ||
		!event(map[string]any{"type": "response.output_item.done", "output_index": index, "item": messageItem(text)}) {
		return
	}
	for i, c := range reply.ToolCalls {
		index++
		if !event(map[string]any{"type": "response.output_item.added", "output_index": index,
			"item": functionCallItem(i, c, "", "in_progress")}) {
			return
		}
		for _, piece := range c.Arguments {
			if !event(map[string]any{"type": "response.function_call_arguments.delta", "item_id": functionCallID(i),
				"output_index": index, "delta": piece}) {
				return
			}
		}
		if !event(map[string]any{"type": "response.function_call_arguments.done", "item_id": functionCallID(i),
			"output_index": index, "arguments": c.arguments()}) ||
			!event(map[string]any{"type": "response.output_item.done", "output_index": index,
				"item": functionCallItem(i, c, c.arguments(), "completed")}) {
			return
		}
	}
	finishResponse(response, reasoning, text, reply.ToolCalls, cut, usage, reply.OmitUsage)
	final := "response.completed"
	if cut {
		final = "response.incomplete"
	}
	event(map[string]any{"type": final, "response": response})
}
