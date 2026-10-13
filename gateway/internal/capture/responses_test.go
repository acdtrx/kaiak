package capture

import (
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"testing"

	"kaiak/internal/fakebackend"
	"kaiak/internal/provider"
)

// Responses content (docs/specs/GATEWAY.md, Request capture → Responses, Streams): a
// corpus of requests and answers in the shapes OpenAI's API, vLLM and llama-server
// send — the self-hosted servers' streams as recorded — each answer given whole and
// streamed, and streamed without its terminal event.

// responsesContent is the content attributes of a Responses exchange.
func responsesContent(t *testing.T, request, response string, stream bool, status int) map[string]any {
	t.Helper()
	return apiContent(t, provider.Responses, request, response, stream, status)
}

// withoutTerminalEvent is stream without its terminal event: a stream that ended
// before it.
func withoutTerminalEvent(stream string) string {
	var kept []string
	for data := range streamEvents([]byte(stream)) {
		var e struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal(data, &e) // a malformed event is kept
		if !slices.Contains(responsesTerminalEvents, e.Type) {
			kept = append(kept, string(data))
		}
	}
	return typedEvents(kept...)
}

// terminalResponse is the response object a stream's terminal event carries: the
// same answer unstreamed.
func terminalResponse(stream []byte) string {
	var last []byte
	for data := range streamEvents(stream) {
		last = data
	}
	var e struct {
		Response json.RawMessage `json:"response"`
	}
	_ = json.Unmarshal(last, &e) // a recorded stream ends with its terminal event
	return string(e.Response)
}

var responsesCorpus = []contentCase{
	{
		name: "plain text, a string input, OpenAI",
		request: `{"model":"gpt-4.1","instructions":"Be <brief> & kind.","input":"Hi","max_output_tokens":50,
			"temperature":0.7,"top_p":1,"store":false,"user":"u-1"}`,
		answer: `{"id":"resp_1","object":"response","created_at":1,"status":"completed","model":"gpt-4.1",
			"output":[{"id":"msg_1","type":"message","status":"completed","role":"assistant",
				"content":[{"type":"output_text","text":"Hello & <welcome>!","annotations":[],"logprobs":[]}]}],
			"usage":{"input_tokens":20,"output_tokens":5,"total_tokens":25}}`,
		stream: typedEvents(
			`{"type":"response.created","sequence_number":0,"response":{"id":"resp_1","object":"response","created_at":1,"status":"in_progress","model":"gpt-4.1","output":[]}}`,
			`{"type":"response.in_progress","sequence_number":1,"response":{"id":"resp_1","object":"response","created_at":1,"status":"in_progress","model":"gpt-4.1","output":[]}}`,
			`{"type":"response.output_item.added","sequence_number":2,"output_index":0,"item":{"id":"msg_1","type":"message","status":"in_progress","role":"assistant","content":[]}}`,
			`{"type":"response.content_part.added","sequence_number":3,"item_id":"msg_1","output_index":0,"content_index":0,"part":{"type":"output_text","text":"","annotations":[]}}`,
			`{"type":"response.output_text.delta","sequence_number":4,"item_id":"msg_1","output_index":0,"content_index":0,"delta":"Hello & "}`,
			`{"type":"response.output_text.delta","sequence_number":5,"item_id":"msg_1","output_index":0,"content_index":0,"delta":"<welcome>!"}`,
			`{"type":"response.output_text.done","sequence_number":6,"item_id":"msg_1","output_index":0,"content_index":0,"text":"Hello & <welcome>!"}`,
			`{"type":"response.content_part.done","sequence_number":7,"item_id":"msg_1","output_index":0,"content_index":0,"part":{"type":"output_text","text":"Hello & <welcome>!","annotations":[]}}`,
			`{"type":"response.output_item.done","sequence_number":8,"output_index":0,"item":{"id":"msg_1","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"Hello & <welcome>!","annotations":[],"logprobs":[]}]}}`,
			`{"type":"response.completed","sequence_number":9,"response":{"id":"resp_1","object":"response","created_at":1,"status":"completed","model":"gpt-4.1","output":[{"id":"msg_1","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"Hello & <welcome>!","annotations":[],"logprobs":[]}]}],"usage":{"input_tokens":20,"output_tokens":5,"total_tokens":25}}}`),
		want: map[string]any{
			"gen_ai.request.max_tokens": int64(50), "gen_ai.request.temperature": 0.7, "gen_ai.request.top_p": 1.0,
			"gen_ai.system_instructions": `[{"type":"text","content":"Be <brief> & kind."}]`,
			"gen_ai.input.messages":      `[{"role":"user","parts":[{"type":"text","content":"Hi"}]}]`,
			"gen_ai.response.id":         "resp_1", "gen_ai.response.model": "gpt-4.1",
			"gen_ai.response.finish_reasons": []string{"completed"},
			"gen_ai.output.messages":         `[{"role":"assistant","parts":[{"type":"text","content":"Hello & <welcome>!"}]}]`,
		},
	},
	{
		name: "message items, images and files",
		request: `{"model":"gpt-4.1","input":[
			{"role":"system","content":"Be careful."},
			{"type":"message","role":"developer","content":[{"type":"input_text","text":"Answer in English."}]},
			{"role":"user","content":[{"type":"input_text","text":"Compare these."},
				{"type":"input_image","image_url":"data:image/PNG;base64,iVBORw0KGgo=","detail":"auto"},
				{"type":"input_image","image_url":"https://example.com/cat.jpg"},
				{"type":"input_image","file_id":"file-img","image_url":null},
				{"type":"input_file","file_data":"data:application/pdf;base64,JVBERi0=","filename":"a.pdf"},
				{"type":"input_file","file_url":"https://example.com/a.pdf"},
				{"type":"input_file","file_id":"file-abc"},
				{"type":"input_audio","input_audio":{"data":"UklGRiQ=","format":"wav"}}]},
			{"type":"message","role":"assistant","id":"msg_0","status":"completed","content":[
				{"type":"output_text","text":"Earlier answer.","annotations":[]},{"type":"refusal","refusal":"Not that."}]}]}`,
		answer: `{"id":"resp_2","object":"response","status":"completed","model":"gpt-4.1","output":[{"id":"msg_2",
			"type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"Both are cats.",
			"annotations":[]}]}]}`,
		stream: typedEvents(
			`{"type":"response.created","response":{"id":"resp_2","object":"response","status":"in_progress","model":"gpt-4.1","output":[]}}`,
			`{"type":"response.output_item.added","output_index":0,"item":{"id":"msg_2","type":"message","status":"in_progress","role":"assistant","content":[]}}`,
			`{"type":"response.output_text.delta","item_id":"msg_2","output_index":0,"content_index":0,"delta":"Both are"}`,
			`{"type":"response.output_text.delta","item_id":"msg_2","output_index":0,"content_index":0,"delta":" cats."}`,
			`{"type":"response.output_item.done","output_index":0,"item":{"id":"msg_2","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"Both are cats.","annotations":[]}]}}`,
			`{"type":"response.completed","response":{"id":"resp_2","object":"response","status":"completed","model":"gpt-4.1","output":[{"id":"msg_2","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"Both are cats.","annotations":[]}]}]}}`),
		want: map[string]any{
			"gen_ai.input.messages": `[
				{"role":"system","parts":[{"type":"text","content":"Be careful."}]},
				{"role":"developer","parts":[{"type":"text","content":"Answer in English."}]},
				{"role":"user","parts":[
					{"type":"text","content":"Compare these."},
					{"type":"blob","modality":"image","mime_type":"image/png","content":"iVBORw0KGgo="},
					{"type":"uri","modality":"image","uri":"https://example.com/cat.jpg"},
					{"type":"file","modality":"image","file_id":"file-img"},
					{"type":"blob","modality":"document","mime_type":"application/pdf","content":"JVBERi0="},
					{"type":"uri","modality":"document","uri":"https://example.com/a.pdf"},
					{"type":"file","modality":"document","file_id":"file-abc"},
					{"type":"input_audio","input_audio":{"data":"UklGRiQ=","format":"wav"}}]},
				{"role":"assistant","parts":[{"type":"text","content":"Earlier answer."},{"type":"text","content":"Not that."}]}]`,
			"gen_ai.response.id": "resp_2", "gen_ai.response.model": "gpt-4.1",
			"gen_ai.response.finish_reasons": []string{"completed"},
			"gen_ai.output.messages":         `[{"role":"assistant","parts":[{"type":"text","content":"Both are cats."}]}]`,
		},
	},
	{
		name: "a function call round trip",
		request: `{"model":"gpt-4.1","tool_choice":"auto","tools":[
				{"type":"function","name":"get_weather","description":"Weather for a city","strict":true,
					"parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}},
				{"type":"function","name":"now","parameters":null},
				{"type":"custom","name":"grep","description":"Search","format":{"type":"text"}}],
			"input":[
				{"role":"user","content":"Weather in Oslo, and the time?"},
				{"type":"function_call","id":"fc_1","call_id":"call_1","name":"get_weather","arguments":"{\"city\":\"Oslo\"}","status":"completed"},
				{"type":"function_call","call_id":"call_2","name":"now","arguments":"not json"},
				{"type":"function_call_output","call_id":"call_1","output":"4 °C, rain"},
				{"type":"function_call_output","call_id":"call_2","output":[{"type":"input_text","text":"12:00"}]},
				{"type":"custom_tool_call","call_id":"call_3","name":"grep","input":"rain"},
				{"type":"custom_tool_call_output","call_id":"call_3","output":"no match"}]}`,
		answer: `{"id":"resp_3","object":"response","status":"completed","model":"gpt-4.1","output":[
			{"id":"msg_3","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"Let me check Bergen too.","annotations":[]}]},
			{"id":"fc_2","type":"function_call","status":"completed","call_id":"call_4","name":"get_weather","arguments":"{\"city\": \"Bergen\"}"}]}`,
		stream: typedEvents(
			`{"type":"response.created","response":{"id":"resp_3","object":"response","status":"in_progress","model":"gpt-4.1","output":[]}}`,
			`{"type":"response.in_progress","response":{"id":"resp_3","object":"response","status":"in_progress","model":"gpt-4.1","output":[]}}`,
			`{"type":"response.output_item.added","output_index":0,"item":{"id":"msg_3","type":"message","status":"in_progress","role":"assistant","content":[]}}`,
			`{"type":"response.output_text.delta","item_id":"msg_3","output_index":0,"content_index":0,"delta":"Let me check Bergen too."}`,
			`{"type":"response.output_item.done","output_index":0,"item":{"id":"msg_3","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"Let me check Bergen too.","annotations":[]}]}}`,
			`{"type":"response.output_item.added","output_index":1,"item":{"id":"fc_2","type":"function_call","status":"in_progress","call_id":"call_4","name":"get_weather","arguments":""}}`,
			`{"type":"response.function_call_arguments.delta","item_id":"fc_2","output_index":1,"delta":"{\"city\": "}`,
			`{"type":"response.function_call_arguments.delta","item_id":"fc_2","output_index":1,"delta":"\"Bergen\"}"}`,
			`{"type":"response.function_call_arguments.done","item_id":"fc_2","output_index":1,"arguments":"{\"city\": \"Bergen\"}"}`,
			`{"type":"response.output_item.done","output_index":1,"item":{"id":"fc_2","type":"function_call","status":"completed","call_id":"call_4","name":"get_weather","arguments":"{\"city\": \"Bergen\"}"}}`,
			`{"type":"response.completed","response":{"id":"resp_3","object":"response","status":"completed","model":"gpt-4.1","output":[{"id":"msg_3","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"Let me check Bergen too.","annotations":[]}]},{"id":"fc_2","type":"function_call","status":"completed","call_id":"call_4","name":"get_weather","arguments":"{\"city\": \"Bergen\"}"}]}}`),
		want: map[string]any{
			"gen_ai.tool.definitions": `[
				{"type":"function","name":"get_weather","description":"Weather for a city",
				 "parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}},
				{"type":"function","name":"now"},
				{"type":"custom","name":"grep","description":"Search","format":{"type":"text"}}]`,
			"gen_ai.input.messages": `[
				{"role":"user","parts":[{"type":"text","content":"Weather in Oslo, and the time?"}]},
				{"role":"assistant","parts":[{"type":"tool_call","id":"call_1","name":"get_weather","arguments":{"city":"Oslo"}}]},
				{"role":"assistant","parts":[{"type":"tool_call","id":"call_2","name":"now","arguments":"not json"}]},
				{"role":"tool","parts":[{"type":"tool_call_response","id":"call_1","response":"4 °C, rain"}]},
				{"role":"tool","parts":[{"type":"tool_call_response","id":"call_2","response":[{"type":"input_text","text":"12:00"}]}]},
				{"role":"assistant","parts":[{"type":"custom_tool_call","call_id":"call_3","name":"grep","input":"rain"}]},
				{"role":"tool","parts":[{"type":"custom_tool_call_output","call_id":"call_3","output":"no match"}]}]`,
			"gen_ai.response.id": "resp_3", "gen_ai.response.model": "gpt-4.1",
			"gen_ai.response.finish_reasons": []string{"completed"},
			"gen_ai.output.messages": `[{"role":"assistant","parts":[
				{"type":"text","content":"Let me check Bergen too."},
				{"type":"tool_call","id":"call_4","name":"get_weather","arguments":{"city":"Bergen"}}]}]`,
		},
	},
	{
		name: "reasoning items, OpenAI's summaries and vLLM's reasoning text",
		request: `{"model":"o4-mini","reasoning":{"effort":"low","summary":"auto"},"input":[
			{"role":"user","content":"2+2?"},
			{"type":"reasoning","id":"rs_0","summary":[{"type":"summary_text","text":"Simple sum."}],"encrypted_content":"gAAAA"},
			{"role":"assistant","content":[{"type":"output_text","text":"4"}]},
			{"type":"reasoning","id":"rs_1","summary":[],"content":[{"type":"reasoning_text","text":"vLLM's thought."}]},
			{"role":"user","content":"And 3+3?"}]}`,
		answer: `{"id":"resp_4","object":"response","status":"completed","model":"o4-mini","output":[
			{"id":"rs_2","type":"reasoning","summary":[{"type":"summary_text","text":"Add three"},{"type":"summary_text","text":" and three."}],"encrypted_content":null},
			{"id":"msg_4","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"6","annotations":[]}]}]}`,
		stream: typedEvents(
			`{"type":"response.created","response":{"id":"resp_4","object":"response","status":"in_progress","model":"o4-mini","output":[]}}`,
			`{"type":"response.output_item.added","output_index":0,"item":{"id":"rs_2","type":"reasoning","summary":[]}}`,
			`{"type":"response.reasoning_summary_part.added","item_id":"rs_2","output_index":0,"summary_index":0,"part":{"type":"summary_text","text":""}}`,
			`{"type":"response.reasoning_summary_text.delta","item_id":"rs_2","output_index":0,"summary_index":0,"delta":"Add three"}`,
			`{"type":"response.reasoning_summary_text.done","item_id":"rs_2","output_index":0,"summary_index":0,"text":"Add three"}`,
			`{"type":"response.reasoning_summary_part.added","item_id":"rs_2","output_index":0,"summary_index":1,"part":{"type":"summary_text","text":""}}`,
			`{"type":"response.reasoning_summary_text.delta","item_id":"rs_2","output_index":0,"summary_index":1,"delta":" and three."}`,
			`{"type":"response.output_item.done","output_index":0,"item":{"id":"rs_2","type":"reasoning","summary":[{"type":"summary_text","text":"Add three"},{"type":"summary_text","text":" and three."}],"encrypted_content":null}}`,
			`{"type":"response.output_item.added","output_index":1,"item":{"id":"msg_4","type":"message","status":"in_progress","role":"assistant","content":[]}}`,
			`{"type":"response.output_text.delta","item_id":"msg_4","output_index":1,"content_index":0,"delta":"6"}`,
			`{"type":"response.output_item.done","output_index":1,"item":{"id":"msg_4","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"6","annotations":[]}]}}`,
			`{"type":"response.completed","response":{"id":"resp_4","object":"response","status":"completed","model":"o4-mini","output":[{"id":"rs_2","type":"reasoning","summary":[{"type":"summary_text","text":"Add three"},{"type":"summary_text","text":" and three."}],"encrypted_content":null},{"id":"msg_4","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"6","annotations":[]}]}]}}`),
		want: map[string]any{
			"gen_ai.input.messages": `[
				{"role":"user","parts":[{"type":"text","content":"2+2?"}]},
				{"role":"assistant","parts":[{"type":"reasoning","content":"Simple sum."}]},
				{"role":"assistant","parts":[{"type":"text","content":"4"}]},
				{"role":"assistant","parts":[{"type":"reasoning","content":"vLLM's thought."}]},
				{"role":"user","parts":[{"type":"text","content":"And 3+3?"}]}]`,
			"gen_ai.response.id": "resp_4", "gen_ai.response.model": "o4-mini",
			"gen_ai.response.finish_reasons": []string{"completed"},
			"gen_ai.output.messages": `[{"role":"assistant","parts":[
				{"type":"reasoning","content":"Add three"},{"type":"reasoning","content":" and three."},
				{"type":"text","content":"6"}]}]`,
		},
	},
	{
		name: "items the mapping does not know",
		request: `{"model":"gpt-4.1","tools":[{"type":"local_shell"}],"input":[
			{"role":"user","content":[{"type":"input_text","text":"What happens?"},{"zeta":1,"type":"x_custom","alpha":[true,null],"s":"<&>"}]},
			{"type":"local_shell_call","id":"lsc_1","call_id":"call_9","action":{"type":"exec","command":["ls"]},"status":"completed"},
			{"type":"local_shell_call_output","id":"lsc_1","output":"a.txt"},
			{"type":"x_item","data":1}]}`,
		answer: `{"id":"resp_5","object":"response","status":"completed","model":"gpt-4.1","output":[
			{"type":"x_output","id":"xo_1","payload":{"a":1}},
			{"id":"msg_5","type":"message","status":"completed","role":"assistant","content":[
				{"type":"output_text","text":"A ball rolls.","annotations":[]},{"type":"x_part","v":1}]}]}`,
		stream: typedEvents(
			`{"type":"response.created","response":{"id":"resp_5","object":"response","status":"in_progress","model":"gpt-4.1","output":[]}}`,
			`{"type":"response.output_item.added","output_index":0,"item":{"type":"x_output","id":"xo_1","payload":{}}}`,
			`{"type":"response.output_item.done","output_index":0,"item":{"type":"x_output","id":"xo_1","payload":{"a":1}}}`,
			`{"type":"response.output_item.added","output_index":1,"item":{"id":"msg_5","type":"message","status":"in_progress","role":"assistant","content":[]}}`,
			`{"type":"response.output_text.delta","item_id":"msg_5","output_index":1,"content_index":0,"delta":"A ball rolls."}`,
			`{"type":"response.output_item.done","output_index":1,"item":{"id":"msg_5","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"A ball rolls.","annotations":[]},{"type":"x_part","v":1}]}}`,
			`{"type":"response.completed","response":{"id":"resp_5","object":"response","status":"completed","model":"gpt-4.1","output":[{"type":"x_output","id":"xo_1","payload":{"a":1}},{"id":"msg_5","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"A ball rolls.","annotations":[]},{"type":"x_part","v":1}]}]}}`),
		want: map[string]any{
			"gen_ai.tool.definitions": `[{"type":"local_shell"}]`,
			"gen_ai.input.messages": `[
				{"role":"user","parts":[{"type":"text","content":"What happens?"},{"zeta":1,"type":"x_custom","alpha":[true,null],"s":"<&>"}]},
				{"role":"assistant","parts":[{"type":"local_shell_call","id":"lsc_1","call_id":"call_9","action":{"type":"exec","command":["ls"]},"status":"completed"}]},
				{"role":"tool","parts":[{"type":"local_shell_call_output","id":"lsc_1","output":"a.txt"}]},
				{"role":"assistant","parts":[{"type":"x_item","data":1}]}]`,
			"gen_ai.response.id": "resp_5", "gen_ai.response.model": "gpt-4.1",
			"gen_ai.response.finish_reasons": []string{"completed"},
			"gen_ai.output.messages": `[{"role":"assistant","parts":[
				{"type":"x_output","id":"xo_1","payload":{"a":1}},
				{"type":"text","content":"A ball rolls."},{"type":"x_part","v":1}]}]`,
		},
	},
	{
		name:    "an incomplete response",
		request: `{"model":"gpt-4.1","max_output_tokens":4,"input":"Count to fifty."}`,
		answer: `{"id":"resp_6","object":"response","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},
			"model":"gpt-4.1","output":[{"id":"msg_6","type":"message","status":"incomplete","role":"assistant",
			"content":[{"type":"output_text","text":"One, two,","annotations":[]}]}]}`,
		stream: typedEvents(
			`{"type":"response.created","response":{"id":"resp_6","object":"response","status":"in_progress","model":"gpt-4.1","output":[]}}`,
			`{"type":"response.output_item.added","output_index":0,"item":{"id":"msg_6","type":"message","status":"in_progress","role":"assistant","content":[]}}`,
			`{"type":"response.output_text.delta","item_id":"msg_6","output_index":0,"content_index":0,"delta":"One,"}`,
			`{"type":"response.output_text.delta","item_id":"msg_6","output_index":0,"content_index":0,"delta":" two,"}`,
			`{"type":"response.output_item.done","output_index":0,"item":{"id":"msg_6","type":"message","status":"incomplete","role":"assistant","content":[{"type":"output_text","text":"One, two,","annotations":[]}]}}`,
			`{"type":"response.incomplete","response":{"id":"resp_6","object":"response","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"model":"gpt-4.1","output":[{"id":"msg_6","type":"message","status":"incomplete","role":"assistant","content":[{"type":"output_text","text":"One, two,","annotations":[]}]}]}}`),
		want: map[string]any{
			"gen_ai.request.max_tokens": int64(4),
			"gen_ai.input.messages":     `[{"role":"user","parts":[{"type":"text","content":"Count to fifty."}]}]`,
			"gen_ai.response.id":        "resp_6", "gen_ai.response.model": "gpt-4.1",
			"gen_ai.response.finish_reasons": []string{"max_output_tokens"},
			"gen_ai.output.messages":         `[{"role":"assistant","parts":[{"type":"text","content":"One, two,"}]}]`,
		},
	},
	{
		name:    "a failed response",
		request: `{"model":"gpt-4.1","input":"Hi"}`,
		answer: `{"id":"resp_7","object":"response","status":"failed","error":{"code":"server_error","message":"The server had an error."},
			"model":"gpt-4.1","output":[]}`,
		stream: typedEvents(
			`{"type":"response.created","response":{"id":"resp_7","object":"response","status":"in_progress","model":"gpt-4.1","output":[]}}`,
			`{"type":"response.in_progress","response":{"id":"resp_7","object":"response","status":"in_progress","model":"gpt-4.1","output":[]}}`,
			`{"type":"response.failed","response":{"id":"resp_7","object":"response","status":"failed","error":{"code":"server_error","message":"The server had an error."},"model":"gpt-4.1","output":[]}}`),
		want: map[string]any{
			"gen_ai.input.messages": `[{"role":"user","parts":[{"type":"text","content":"Hi"}]}]`,
			"gen_ai.response.id":    "resp_7", "gen_ai.response.model": "gpt-4.1",
			"gen_ai.response.finish_reasons": []string{"failed"},
			"gen_ai.output.messages":         `[{"role":"assistant","parts":[]}]`,
		},
	},
}

// Each case's request and answer give the corpus' attributes — the answer whole and
// streamed alike: every streamed answer assembles to its unstreamed twin. Without its
// terminal event, the stream's items still assemble to the same output — only the
// finish reason, which the terminal event carries, is missing.
func TestResponsesContent(t *testing.T) {
	expectCorpus(t, provider.Responses, responsesCorpus)
	for _, c := range responsesCorpus {
		want := maps.Clone(c.want)
		delete(want, "gen_ai.response.finish_reasons")
		expectAttributes(t, c.name+", without its terminal event",
			responsesContent(t, c.request, withoutTerminalEvent(c.stream), true, 200), want)
	}
}

// recordedResponsesToolRequest is the request the recorded tool streams answered.
const recordedResponsesToolRequest = `{"model":"qwen","max_output_tokens":300,"stream":true,
	"tools":[{"type":"function","name":"get_weather","description":"Weather for a city",
		"parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}],
	"input":"What is the weather in Paris? Use the tool."}`

// recordedResponsesToolWant is what the recorded tool streams' request gives.
var recordedResponsesToolWant = map[string]any{
	"gen_ai.request.max_tokens": int64(300),
	"gen_ai.input.messages":     `[{"role":"user","parts":[{"type":"text","content":"What is the weather in Paris? Use the tool."}]}]`,
	"gen_ai.tool.definitions": `[{"type":"function","name":"get_weather","description":"Weather for a city",
		"parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}]`,
}

// The servers' recorded streams give their answer: whole, the terminal event's
// response — the same answer unstreamed; without that event, the items the stream's
// events built. vLLM's terminal response names its function call by another call_id
// than its item events did; llama-server's events name no model before the terminal
// event, and its items interleave (a function call begins before the reasoning item
// is done).
func TestResponsesRecordedStreams(t *testing.T) {
	const countRequest = `{"model":"qwen","max_output_tokens":16,"stream":true,"input":"Count to fifty."}`
	countWant := map[string]any{"gen_ai.request.max_tokens": int64(16),
		"gen_ai.input.messages": `[{"role":"user","parts":[{"type":"text","content":"Count to fifty."}]}]`}
	vllmReasoning := `{"type":"reasoning","content":"The user is asking about the weather in Paris and has explicitly requested to use the tool. I'll call the get_weather function with city set to \"Paris\".\n"}`
	llamaReasoning := `{"type":"reasoning","content":"The user wants to know the weather in Paris and explicitly asked me to use the tool. I'll call the get_weather function with city \"Paris\".\n"}`
	for _, c := range []struct {
		server, file, request string
		// whole is what the stream gives; items what it gives without its terminal event.
		whole, items map[string]any
	}{
		{server: "vllm", file: "responses-stream-tool.sse", request: recordedResponsesToolRequest,
			whole: with(recordedResponsesToolWant, map[string]any{
				"gen_ai.response.id": "resp_a27aa2d242736829", "gen_ai.response.model": "unsloth/Qwen3.8-27B-NVFP4",
				"gen_ai.response.finish_reasons": []string{"completed"},
				"gen_ai.output.messages": `[{"role":"assistant","parts":[` + vllmReasoning + `,
					{"type":"tool_call","id":"chatcmpl-tool-a1ffcc03da294e18","name":"get_weather","arguments":{"city":"Paris"}}]}]`}),
			items: with(recordedResponsesToolWant, map[string]any{
				"gen_ai.response.id": "resp_a27aa2d242736829", "gen_ai.response.model": "unsloth/Qwen3.8-27B-NVFP4",
				"gen_ai.output.messages": `[{"role":"assistant","parts":[` + vllmReasoning + `,
					{"type":"tool_call","id":"call_8cca9c91a69990d7","name":"get_weather","arguments":{"city":"Paris"}}]}]`})},
		{server: "llama-server", file: "responses-stream-tool.sse", request: recordedResponsesToolRequest,
			whole: with(recordedResponsesToolWant, map[string]any{
				"gen_ai.response.id": "resp_H89DjiEz6GawFO2ZhnllTaxYvIJqkmk5", "gen_ai.response.model": "unsloth/Qwen3.8-27B-GGUF:UD-Q4_K_XL",
				"gen_ai.response.finish_reasons": []string{"completed"},
				"gen_ai.output.messages": `[{"role":"assistant","parts":[` + llamaReasoning + `,
					{"type":"tool_call","id":"call_xpGk890Sm9ix7yCPjCRtl1IuuFw440pa","name":"get_weather","arguments":{"city":"Paris"}}]}]`}),
			items: with(recordedResponsesToolWant, map[string]any{
				"gen_ai.response.id": "resp_H89DjiEz6GawFO2ZhnllTaxYvIJqkmk5",
				"gen_ai.output.messages": `[{"role":"assistant","parts":[` + llamaReasoning + `,
					{"type":"tool_call","id":"call_xpGk890Sm9ix7yCPjCRtl1IuuFw440pa","name":"get_weather","arguments":{"city":"Paris"}}]}]`})},
		{server: "vllm", file: "responses-stream-incomplete.sse", request: countRequest,
			whole: with(countWant, map[string]any{
				"gen_ai.response.id": "resp_96dafd9e0305944d", "gen_ai.response.model": "unsloth/Qwen3.8-27B-NVFP4",
				"gen_ai.response.finish_reasons": []string{"max_output_tokens"},
				"gen_ai.output.messages": `[{"role":"assistant","parts":[
					{"type":"reasoning","content":"We need to respond to user: \"Count to fifty.\" Need final answer probably"}]}]`}),
			items: with(countWant, map[string]any{
				"gen_ai.response.id": "resp_96dafd9e0305944d", "gen_ai.response.model": "unsloth/Qwen3.8-27B-NVFP4",
				"gen_ai.output.messages": `[{"role":"assistant","parts":[
					{"type":"reasoning","content":"We need to respond to user: \"Count to fifty.\" Need final answer probably"}]}]`})},
		// A length-capped llama-server stream ends with response.completed, status
		// completed.
		{server: "llama-server", file: "responses-stream-incomplete.sse", request: countRequest,
			whole: with(countWant, map[string]any{
				"gen_ai.response.id": "resp_IikvrrHVUBXLCBj1crvROdW70RCW6Q9z", "gen_ai.response.model": "unsloth/Qwen3.8-27B-GGUF:UD-Q4_K_XL",
				"gen_ai.response.finish_reasons": []string{"completed"},
				"gen_ai.output.messages": `[{"role":"assistant","parts":[
					{"type":"reasoning","content":"The user wants me to count to fifty. This is straightforward - I just need"}]}]`}),
			items: with(countWant, map[string]any{
				"gen_ai.response.id": "resp_IikvrrHVUBXLCBj1crvROdW70RCW6Q9z",
				"gen_ai.output.messages": `[{"role":"assistant","parts":[
					{"type":"reasoning","content":"The user wants me to count to fifty. This is straightforward - I just need"}]}]`})},
	} {
		label := c.server + " " + c.file
		stream := fakebackend.Captured(c.server, c.file)
		streamed := responsesContent(t, c.request, string(stream), true, 200)
		expectAttributes(t, label, streamed, c.whole)
		expectAttributes(t, label+", its terminal response as a body",
			responsesContent(t, c.request, terminalResponse(stream), false, 200), c.whole)
		expectAttributes(t, label+", without its terminal event",
			responsesContent(t, c.request, withoutTerminalEvent(string(stream)), true, 200), c.items)
	}
}

// The servers' recorded unstreamed answers give their items: the reasoning item's
// reasoning text, then the message's text.
func TestResponsesRecordedAnswers(t *testing.T) {
	const request = `{"model":"qwen","max_output_tokens":200,"input":"Say hello in three words."}`
	input := `[{"role":"user","parts":[{"type":"text","content":"Say hello in three words."}]}]`
	for _, c := range []struct {
		server, id, model, output string
	}{
		{"vllm", "resp_a4aac8e42e64397e", "unsloth/Qwen3.8-27B-NVFP4", `[{"role":"assistant","parts":[
			{"type":"reasoning","content":"We need to answer user: \"Say hello in three words.\" Need final only three words likely \"Hello there friend\" or \"Hi there friend\". Must be three words. Ensure exactly three words. Could be \"Hello to you\" (3). Final: \"Hello to you\".\n"},
			{"type":"text","content":"\n\nHello to you"}]}]`},
		{"llama-server", "resp_mL5CUIvEOAXCJxk2eiqYy5vfAClCxr5N", "unsloth/Qwen3.8-27B-GGUF:UD-Q4_K_XL", `[{"role":"assistant","parts":[
			{"type":"reasoning","content":"The user wants me to say hello in exactly three words. Let me think of options:\n- \"Hello there friend\" - 3 words\n- \"Hi, how are\" - no, incomplete\n- \"Hello, my friend\" - 3 words\n- \"Greetings to you\" - 3 words\n- \"Hello to you\" - 3 words\n\nI'll go with \"Hello, my friend\" - it's warm, natural, and exactly three words.\n"},
			{"type":"text","content":"Hello, my friend."}]}]`},
	} {
		got := responsesContent(t, request, string(fakebackend.Captured(c.server, "responses.json")), false, 200)
		expectAttributes(t, c.server, got, map[string]any{"gen_ai.request.max_tokens": int64(200),
			"gen_ai.input.messages": input, "gen_ai.response.id": c.id, "gen_ai.response.model": c.model,
			"gen_ai.response.finish_reasons": []string{"completed"}, "gen_ai.output.messages": c.output})
	}
}

// A stream cut short keeps what arrived: the items finished and the ones in progress
// as their deltas built them — a message's text and refusal, a reasoning item's
// summary and reasoning text, a function call's half-written arguments as a string,
// any other item as it began — with no finish reason. An event the stream ends inside
// was never whole and is not read. A non-stream answer cut short is no valid JSON,
// and gives nothing.
func TestResponsesCutShort(t *testing.T) {
	const request = `{"model":"gpt-4.1","stream":true,"input":"Weather?",
		"tools":[{"type":"function","name":"get_weather","parameters":{"type":"object"}}]}`
	base := map[string]any{"gen_ai.input.messages": `[{"role":"user","parts":[{"type":"text","content":"Weather?"}]}]`,
		"gen_ai.tool.definitions": `[{"type":"function","name":"get_weather","parameters":{"type":"object"}}]`}
	stream := typedEvents(
		`{"type":"response.created","response":{"id":"resp_c","object":"response","status":"in_progress","model":"gpt-4.1","output":[]}}`,
		`{"type":"response.output_item.added","output_index":0,"item":{"id":"rs_c","type":"reasoning","summary":[]}}`,
		`{"type":"response.reasoning_summary_text.delta","item_id":"rs_c","output_index":0,"summary_index":0,"delta":"Use the tool."}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"id":"rs_c","type":"reasoning","summary":[{"type":"summary_text","text":"Use the tool."}]}}`,
		`{"type":"response.output_item.added","output_index":1,"item":{"id":"msg_c","type":"message","status":"in_progress","role":"assistant","content":[]}}`,
		`{"type":"response.output_text.delta","item_id":"msg_c","output_index":1,"content_index":0,"delta":"Checking."}`,
		`{"type":"response.output_item.done","output_index":1,"item":{"id":"msg_c","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"Checking.","annotations":[]}]}}`,
		`{"type":"response.output_item.added","output_index":2,"item":{"id":"fc_c","type":"function_call","status":"in_progress","call_id":"call_9","name":"get_weather","arguments":""}}`,
		`{"type":"response.function_call_arguments.delta","item_id":"fc_c","output_index":2,"delta":"{\"city\": \"Os"}`) +
		"event: response.function_call_arguments.delta\n" +
		`data: {"type":"response.function_call_arguments.delta","item_id":"fc_c","output_index":2,"delta":"lo\"}"}`
	expectAttributes(t, "cut mid tool call", responsesContent(t, request, stream, true, 200), with(base, map[string]any{
		"gen_ai.response.id": "resp_c", "gen_ai.response.model": "gpt-4.1",
		"gen_ai.output.messages": `[{"role":"assistant","parts":[
			{"type":"reasoning","content":"Use the tool."},{"type":"text","content":"Checking."},
			{"type":"tool_call","id":"call_9","name":"get_weather","arguments":"{\"city\": \"Os"}]}]`}))

	// Every kind of item in progress at once, in the order they were added.
	inProgress := typedEvents(
		`{"type":"response.created","response":{"id":"resp_p","object":"response","status":"in_progress","model":"gpt-4.1","output":[]}}`,
		`{"type":"response.output_item.added","output_index":0,"item":{"id":"msg_p","type":"message","status":"in_progress","role":"assistant","content":[]}}`,
		`{"type":"response.output_item.added","output_index":1,"item":{"id":"rs_p","type":"reasoning","summary":[],"content":null}}`,
		`{"type":"response.output_item.added","output_index":2,"item":{"id":"x_p","type":"x_item","status":"in_progress"}}`,
		`{"type":"response.output_item.added","output_index":3,"item":{"id":"fc_p","type":"function_call","call_id":"call_p","name":"now"}}`,
		`{"type":"response.output_text.delta","item_id":"msg_p","output_index":0,"content_index":0,"delta":"Part"}`,
		`{"type":"response.reasoning_text.delta","item_id":"rs_p","output_index":1,"content_index":0,"delta":"Think"}`,
		`{"type":"response.output_text.delta","item_id":"msg_p","output_index":0,"content_index":0,"delta":"ial"}`,
		`{"type":"response.refusal.delta","item_id":"msg_p","output_index":0,"content_index":1,"delta":"No."}`,
		`{"type":"response.reasoning_summary_text.delta","item_id":"rs_p","output_index":1,"summary_index":0,"delta":"Sum"}`,
		`{"type":"error","code":"server_error","message":"The server had an error.","param":null}`)
	expectAttributes(t, "items in progress", responsesContent(t, request, inProgress, true, 200), with(base, map[string]any{
		"gen_ai.response.id": "resp_p", "gen_ai.response.model": "gpt-4.1",
		"gen_ai.output.messages": `[{"role":"assistant","parts":[
			{"type":"text","content":"Partial"},{"type":"text","content":"No."},
			{"type":"reasoning","content":"Sum"},{"type":"reasoning","content":"Think"},
			{"id":"x_p","type":"x_item","status":"in_progress"},
			{"type":"tool_call","id":"call_p","name":"now"}]}]`}))

	// llama-server's recorded tool stream, cut inside the arguments' third fragment:
	// its reasoning item is not done yet.
	recorded := string(fakebackend.Captured("llama-server", "responses-stream-tool.sse"))
	marker := `{"type":"response.function_call_arguments.delta","delta":"Paris"`
	cut := recorded[:strings.Index(recorded, marker)+len(marker)]
	expectAttributes(t, "recorded, cut", responsesContent(t, recordedResponsesToolRequest, cut, true, 200),
		with(recordedResponsesToolWant, map[string]any{
			"gen_ai.response.id": "resp_H89DjiEz6GawFO2ZhnllTaxYvIJqkmk5",
			"gen_ai.output.messages": `[{"role":"assistant","parts":[
				{"type":"reasoning","content":"The user wants to know the weather in Paris and explicitly asked me to use the tool. I'll call the get_weather function with city \"Paris\".\n"},
				{"type":"tool_call","id":"call_xpGk890Sm9ix7yCPjCRtl1IuuFw440pa","name":"get_weather","arguments":"{\"city\":\""}]}]`}))

	whole := `{"id":"resp_c","object":"response","status":"completed","model":"gpt-4.1","output":[{"type":"message","content":[{"type":"output_text","text":"Check`
	expectAttributes(t, "cut body", responsesContent(t, request, whole, false, 200), base)
}

// An error answer — the backend's relayed 4xx, the gateway's own error body, as a
// JSON body even for a stream — gives no output: the request's content alone.
func TestResponsesErrorAnswer(t *testing.T) {
	const request = `{"model":"gpt-4.1","temperature":0.5,"instructions":"Be brief.","input":"Hi"}`
	want := map[string]any{"gen_ai.request.temperature": 0.5,
		"gen_ai.system_instructions": `[{"type":"text","content":"Be brief."}]`,
		"gen_ai.input.messages":      `[{"role":"user","parts":[{"type":"text","content":"Hi"}]}]`}
	for _, c := range []struct {
		name     string
		response string
		stream   bool
		status   int
	}{
		{"relayed 400", `{"error":{"message":"bad","type":"invalid_request_error","param":null,"code":null}}`, false, 400},
		{"recorded 404, vLLM", string(fakebackend.Captured("vllm", "responses-model-missing.json")), false, 404},
		{"gateway's 429 to a stream", `{"error":{"message":"limit","type":"rate_limit_error","code":"rate_limited"}}`, true, 429},
		// Even one that looks like an answer: the status decides.
		{"answer-shaped 400", responsesCorpus[0].answer, false, 400},
		{"stream-shaped 502", responsesCorpus[0].stream, true, 502},
	} {
		expectAttributes(t, c.name, responsesContent(t, request, c.response, c.stream, c.status), want)
	}
}

// The spec's Responses example (docs/specs/GATEWAY.md, Request capture → Examples),
// built through the whole capture into a span: its name and the attributes it shows,
// exactly.
func TestResponsesSpecExample(t *testing.T) {
	const request = `{"model": "gpt-4.1", "instructions": "Answer in French.", "max_output_tokens": 500,
	 "input": [
	   {"role": "user", "content": [
	     {"type": "input_text", "text": "Describe it."},
	     {"type": "input_image", "image_url": "https://example.com/cat.png"}]},
	   {"type": "function_call", "call_id": "call_1", "name": "zoom", "arguments": "{\"level\": 2}"},
	   {"type": "function_call_output", "call_id": "call_1", "output": "zoomed"}]}`
	const answer = `{"id":"resp_x","object":"response","created_at":1,"status":"completed","model":"gpt-4.1","output":[
		{"id":"rs_1","type":"reasoning","summary":[{"type":"summary_text","text":"Looked closer."}]},
		{"id":"msg_1","type":"message","status":"completed","role":"assistant","content":[
			{"type":"output_text","text":"Un chat.","annotations":[]}]}],
		"usage":{"input_tokens":90,"output_tokens":12,"total_tokens":102}}`
	expectSpecExample(t, provider.Responses, request, answer, "gpt-4.1", map[string]map[string]any{
		"gen_ai.request.max_tokens":  {"intValue": "500"},
		"gen_ai.system_instructions": {"stringValue": compact(t, `[{"type": "text", "content": "Answer in French."}]`)},
		"gen_ai.input.messages": {"stringValue": compact(t, `
			[{"role": "user", "parts": [
			   {"type": "text", "content": "Describe it."},
			   {"type": "uri", "modality": "image", "uri": "https://example.com/cat.png"}]},
			 {"role": "assistant", "parts": [
			   {"type": "tool_call", "id": "call_1", "name": "zoom", "arguments": {"level": 2}}]},
			 {"role": "tool", "parts": [
			   {"type": "tool_call_response", "id": "call_1", "response": "zoomed"}]}]`)},
		"gen_ai.output.messages": {"stringValue": compact(t, `
			[{"role": "assistant", "parts": [
			   {"type": "reasoning", "content": "Looked closer."},
			   {"type": "text", "content": "Un chat."}]}]`)},
		"gen_ai.response.finish_reasons": {"arrayValue": map[string]any{"values": []any{map[string]any{"stringValue": "completed"}}}},
	}, "resp_x")
}

// Content without its API's shape where the mapping reads it is left out, and the
// rest captured: wrong types everywhere, truncated JSON, empty input — never a panic.
func TestResponsesMalformed(t *testing.T) {
	for _, c := range []struct {
		name              string
		request, response string
		stream            bool
		want              map[string]any
	}{
		{name: "empty", want: map[string]any{}},
		{name: "truncated request", request: `{"model":"x","input":"hi"`, want: map[string]any{}},
		{name: "request a list", request: `[{"role":"user","content":"hi"}]`, want: map[string]any{}},
		{name: "input an object, instructions a list", request: `{"input":{"role":"user","content":"hi"},
			"instructions":[{"type":"input_text","text":"x"}]}`, want: map[string]any{}},
		{name: "input a number, tools a string", request: `{"input":5,"tools":"none","instructions":5}`, want: map[string]any{}},
		{name: "an empty input and instructions", request: `{"input":"","instructions":""}`,
			want: map[string]any{"gen_ai.input.messages": `[{"role":"user","parts":[]}]`}},
		{name: "input of every wrong shape", request: `{"input":[1,null,"x",[],{"content":"no role, no type"},
			{"type":5,"role":"user","content":"x"},{"type":"message","content":"no role"},{"role":5,"content":"x"},
			{"role":"user","content":5},
			{"role":"user","content":[1,null,{"type":5},{"text":"no type"},{"type":"input_text"},{"type":"input_text","text":5},
				{"type":"input_text","text":""},{"type":"refusal","refusal":5},{"type":"input_image"},
				{"type":"input_image","image_url":5,"file_id":5},
				{"type":"input_file","file_data":"not a data URL","file_id":"not read"},{"type":"input_file"},
				{"type":"output_text","text":"ok"}]},
			{"type":"function_call","call_id":"c1"},{"type":"function_call","name":5},
			{"type":"function_call","call_id":5,"name":"f","arguments":{"a":1}},
			{"type":"function_call_output"},
			{"type":"reasoning"},
			{"type":"reasoning","summary":[1,{"type":5},{"text":"no type"},{"type":"summary_text","text":5},
				{"type":"summary_text","text":""},{"type":"x_entry","v":1}],"content":"x"}]}`,
			want: map[string]any{"gen_ai.input.messages": `[
				{"role":"user","parts":[]},
				{"role":"user","parts":[{"type":"text","content":"ok"}]},
				{"role":"assistant","parts":[{"type":"tool_call","name":"f","arguments":{"a":1}}]},
				{"role":"tool","parts":[{"type":"tool_call_response","response":null}]},
				{"role":"assistant","parts":[]},
				{"role":"assistant","parts":[{"type":"x_entry","v":1}]}]`}},
		{name: "tools of every wrong shape", request: `{"tools":[1,null,"f",{"type":5},{"type":"function"},
			{"type":"function","name":5},{"type":"function","name":"f","description":5,"parameters":null},{"name":"g"}]}`,
			want: map[string]any{"gen_ai.tool.definitions": `[{"type":"function","name":"f"}]`}},
		{name: "parameters of the wrong types", request: `{"max_output_tokens":"10","temperature":"0.5","top_p":true}`,
			want: map[string]any{}},
		{name: "parameters at their edges", request: `{"max_output_tokens":0,"temperature":0,"top_p":1e-300}`,
			want: map[string]any{"gen_ai.request.max_tokens": int64(0), "gen_ai.request.temperature": 0.0,
				"gen_ai.request.top_p": 1e-300}},
		{name: "a member in another case is not the member", request: `{"Input":"hi","Instructions":"x","Max_output_tokens":5,
			"input":[{"Role":"user","content":"x"},{"role":"user","Content":"y"}]}`,
			want: map[string]any{"gen_ai.input.messages": `[{"role":"user","parts":[]}]`}},
		{name: "whitespace before the body", request: "\n\t {\"input\":\"hi\"}",
			want: map[string]any{"gen_ai.input.messages": `[{"role":"user","parts":[{"type":"text","content":"hi"}]}]`}},
		{name: "answer a list", response: `[{"type":"message"}]`, want: map[string]any{}},
		{name: "output an object", response: `{"id":"r","output":{"type":"message"}}`,
			want: map[string]any{"gen_ai.response.id": "r"}},
		{name: "an answer of every wrong shape", response: `{"id":5,"model":["m"],"status":5,"output":[1,null,{"type":5},
			{"id":"no type"},{"type":"function_call","call_id":"c"},{"type":"message","content":5},
			{"type":"message","content":"plain"},{"type":"reasoning","summary":"x"}]}`,
			want: map[string]any{"gen_ai.output.messages": `[{"role":"assistant","parts":[{"type":"text","content":"plain"}]}]`}},
		{name: "incomplete without a reason", response: `{"status":"incomplete","incomplete_details":{"reason":""},"output":[]}`,
			want: map[string]any{"gen_ai.response.finish_reasons": []string{"incomplete"},
				"gen_ai.output.messages": `[{"role":"assistant","parts":[]}]`}},
		{name: "incomplete details a string", response: `{"status":"incomplete","incomplete_details":"x","output":[]}`,
			want: map[string]any{"gen_ai.response.finish_reasons": []string{"incomplete"},
				"gen_ai.output.messages": `[{"role":"assistant","parts":[]}]`}},
		{name: "a reason on a completed response", response: `{"status":"completed","incomplete_details":{"reason":"max_output_tokens"},"output":[]}`,
			want: map[string]any{"gen_ai.response.finish_reasons": []string{"completed"},
				"gen_ai.output.messages": `[{"role":"assistant","parts":[]}]`}},
		{name: "a stream that is a JSON body", stream: true, response: responsesCorpus[0].answer, want: map[string]any{}},
		{name: "a stream of comments", stream: true, response: ": ping\n\n: ping\n\n", want: map[string]any{}},
		{name: "a terminal event without its response", stream: true, response: typedEvents(
			`{"type":"response.created","response":{"id":"r","model":"m","output":[]}}`,
			`{"type":"response.output_item.done","item":{"id":"i","type":"message","content":[{"type":"output_text","text":"kept"}]}}`,
			`{"type":"response.completed","response":5}`),
			want: map[string]any{"gen_ai.response.id": "r", "gen_ai.response.model": "m",
				"gen_ai.output.messages": `[{"role":"assistant","parts":[{"type":"text","content":"kept"}]}]`}},
		{name: "a stream that ends at its terminal event", stream: true, response: typedEvents(
			`{"type":"response.completed","response":{"id":"r","status":"completed","output":[]}}`,
			`{"type":"response.output_item.done","item":{"id":"i","type":"message","content":[{"type":"output_text","text":"after the end"}]}}`),
			want: map[string]any{"gen_ai.response.id": "r", "gen_ai.response.finish_reasons": []string{"completed"},
				"gen_ai.output.messages": `[{"role":"assistant","parts":[]}]`}},
		{name: "a stream of every wrong shape", stream: true, response: typedEvents(
			`{"type":"response.created","response":5}`,
			`{"type":"response.created","response":{"id":"r"}}`,
			`not json`, `[1]`, `{"type":5}`,
			`{"type":"response.output_item.added","item":"x"}`,
			`{"type":"response.output_item.added","item":{"type":"message"}}`,
			`{"type":"response.output_item.added","item":{"id":5,"type":"message"}}`,
			`{"type":"response.output_item.added","item":{"id":"m1","type":"message","role":"assistant","content":[]}}`,
			`{"type":"response.output_text.delta","item_id":"m1","delta":"kept"}`,
			`{"type":"response.output_text.delta","item_id":"m1","delta":5}`,
			`{"type":"response.output_text.delta","item_id":"zz","delta":"no such item"}`,
			`{"type":"response.output_text.delta","delta":"no item named"}`,
			`{"type":"response.function_call_arguments.delta","item_id":"m1","delta":"{}"}`,
			`{"type":"response.reasoning_text.delta","item_id":"m1","delta":"not a message's"}`,
			`{"type":"response.output_item.added","item":{"id":"m1","type":"reasoning"}}`,
			`{"type":"response.output_item.added","item":{"id":"m2","type":"message"}}`,
			`{"type":"response.output_item.done","item":{"id":"m2","type":"message","content":[{"type":"output_text","text":"finished"}]}}`,
			`{"type":"response.output_text.delta","item_id":"m2","delta":"after it was done"}`,
			`{"type":"response.output_item.done","item":{"type":"message","content":[{"type":"output_text","text":"no id"}]}}`,
			`{"type":"response.output_item.added","item":{"id":"f1","type":"function_call","name":5,"arguments":"{}"}}`),
			want: map[string]any{"gen_ai.response.id": "r",
				"gen_ai.output.messages": `[{"role":"assistant","parts":[
					{"type":"text","content":"kept"},{"type":"text","content":"finished"}]}]`}},
	} {
		t.Run(c.name, func(t *testing.T) {
			expectAttributes(t, c.name, responsesContent(t, c.request, c.response, c.stream, 200), c.want)
		})
	}
}

// Whatever a client or a backend sends, reading it never panics, and every content
// attribute is valid JSON — the conventions' list — with one finish reason per output
// message when it has any.
func FuzzResponsesContent(f *testing.F) {
	for _, c := range responsesCorpus {
		f.Add([]byte(c.request), []byte(c.answer), false)
		f.Add([]byte(c.request), []byte(c.stream), true)
		f.Add([]byte(c.request), []byte(withoutTerminalEvent(c.stream)), true)
	}
	f.Add([]byte(`{"input":[{"type":"reasoning","summary":[{"type":"summary_text","text":"x"}],"content":[{"type":"reasoning_text","text":"y"}]}]}`),
		[]byte("data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"\",\"type\":\"function_call\",\"name\":\"f\"}}\n\ndata: {\"type\":\"response.function_call_arguments.delta\",\"item_id\":\"\",\"delta\":\"{\"}\r\r"), true)
	f.Fuzz(func(t *testing.T, request, response []byte, stream bool) {
		if len(request)+len(response) > 64<<10 {
			t.Skip()
		}
		expectWellFormed(t, plainAttributes(t, content(provider.Responses,
			exchange{request: request, response: response, stream: stream, status: 200})))
	})
}
