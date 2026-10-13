package capture

import (
	"strings"
	"testing"

	"kaiak/internal/fakebackend"
	"kaiak/internal/provider"
)

// Messages content (docs/specs/GATEWAY.md, Request capture → Messages, Streams): a
// corpus of requests and answers in the shapes Anthropic's API, vLLM and llama-server
// send — the self-hosted servers' streams as recorded — each answer given whole and
// streamed.

// messagesContent is the content attributes of a Messages exchange.
func messagesContent(t *testing.T, request, response string, stream bool, status int) map[string]any {
	t.Helper()
	return apiContent(t, provider.Messages, request, response, stream, status)
}

// recordedToolRequest is the request the recorded tool streams answered.
const recordedToolRequest = `{"model":"qwen","max_tokens":300,"stream":true,
	"tools":[{"name":"get_weather","description":"Weather for a city",
		"input_schema":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}],
	"messages":[{"role":"user","content":"What is the weather in Paris? Use the tool."}]}`

// recordedToolWant is what a recorded tool stream's request gives, beside the answer.
var recordedToolWant = map[string]any{
	"gen_ai.request.max_tokens": int64(300),
	"gen_ai.input.messages":     `[{"role":"user","parts":[{"type":"text","content":"What is the weather in Paris? Use the tool."}]}]`,
	"gen_ai.tool.definitions": `[{"type":"function","name":"get_weather","description":"Weather for a city",
		"parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}]`,
}

var messagesCorpus = []contentCase{
	{
		name: "plain text, Anthropic",
		request: `{"model":"claude-sonnet","max_tokens":1024,"temperature":0.7,"top_p":0.9,"top_k":40,
			"stop_sequences":["END"],"system":"Be <brief> & kind.","metadata":{"user_id":"u-1"},
			"messages":[{"role":"user","content":"Hi"},{"role":"assistant","content":"Hello."},
				{"role":"user","content":[{"type":"text","text":"Again & <again>","cache_control":{"type":"ephemeral"}}]}]}`,
		answer: `{"id":"msg_01","type":"message","role":"assistant","model":"claude-sonnet",
			"content":[{"type":"text","text":"Hello & <welcome>!"}],"stop_reason":"end_turn","stop_sequence":null,
			"usage":{"input_tokens":20,"output_tokens":5}}`,
		stream: typedEvents(
			`{"type":"message_start","message":{"id":"msg_01","type":"message","role":"assistant","model":"claude-sonnet","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":20,"output_tokens":1}}}`,
			`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			`{"type":"ping"}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello & "}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"<welcome>!"}}`,
			`{"type":"content_block_stop","index":0}`,
			`{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":5}}`,
			`{"type":"message_stop"}`),
		want: map[string]any{
			"gen_ai.request.max_tokens": int64(1024), "gen_ai.request.temperature": 0.7, "gen_ai.request.top_p": 0.9,
			"gen_ai.request.top_k": int64(40), "gen_ai.request.stop_sequences": []string{"END"},
			"gen_ai.system_instructions": `[{"type":"text","content":"Be <brief> & kind."}]`,
			"gen_ai.input.messages": `[
				{"role":"user","parts":[{"type":"text","content":"Hi"}]},
				{"role":"assistant","parts":[{"type":"text","content":"Hello."}]},
				{"role":"user","parts":[{"type":"text","content":"Again & <again>"}]}]`,
			"gen_ai.response.id": "msg_01", "gen_ai.response.model": "claude-sonnet",
			"gen_ai.response.finish_reasons": []string{"end_turn"},
			"gen_ai.output.messages":         `[{"role":"assistant","parts":[{"type":"text","content":"Hello & <welcome>!"}]}]`,
		},
	},
	{
		name: "system blocks, images and documents",
		request: `{"model":"claude-sonnet","max_tokens":100,
			"system":[{"type":"text","text":"You are a vet.","cache_control":{"type":"ephemeral"}},{"type":"text","text":""},
				{"type":"x_note","note":"kept"}],
			"messages":[{"role":"user","content":[
				{"type":"image","source":{"type":"base64","media_type":"Image/PNG","data":"iVBORw0KGgo="}},
				{"type":"image","source":{"type":"url","url":"https://example.com/cat.jpg"}},
				{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"JVBERi0="},"title":"Report"},
				{"type":"document","source":{"type":"url","url":"https://example.com/a.pdf"}},
				{"type":"document","source":{"type":"text","media_type":"text/plain","data":"Plain notes."}},
				{"type":"image","source":{"type":"base64","data":"R0lGOD=="}},
				{"type":"text","text":"Compare these."}]}]}`,
		answer: `{"id":"msg_02","type":"message","role":"assistant","model":"claude-sonnet",
			"content":[{"type":"text","text":"Both are cats."}],"stop_reason":"end_turn","stop_sequence":null}`,
		stream: typedEvents(
			`{"type":"message_start","message":{"id":"msg_02","type":"message","role":"assistant","model":"claude-sonnet","content":[],"stop_reason":null,"stop_sequence":null}}`,
			`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Both are"}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" cats."}}`,
			`{"type":"content_block_stop","index":0}`,
			`{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null}}`,
			`{"type":"message_stop"}`),
		want: map[string]any{
			"gen_ai.request.max_tokens": int64(100),
			"gen_ai.system_instructions": `[{"type":"text","content":"You are a vet."},
				{"type":"x_note","note":"kept"}]`,
			"gen_ai.input.messages": `[{"role":"user","parts":[
				{"type":"blob","modality":"image","mime_type":"image/png","content":"iVBORw0KGgo="},
				{"type":"uri","modality":"image","uri":"https://example.com/cat.jpg"},
				{"type":"blob","modality":"document","mime_type":"application/pdf","content":"JVBERi0="},
				{"type":"uri","modality":"document","uri":"https://example.com/a.pdf"},
				{"type":"document","source":{"type":"text","media_type":"text/plain","data":"Plain notes."}},
				{"type":"blob","modality":"image","content":"R0lGOD=="},
				{"type":"text","content":"Compare these."}]}]`,
			"gen_ai.response.id": "msg_02", "gen_ai.response.model": "claude-sonnet",
			"gen_ai.response.finish_reasons": []string{"end_turn"},
			"gen_ai.output.messages":         `[{"role":"assistant","parts":[{"type":"text","content":"Both are cats."}]}]`,
		},
	},
	{
		name: "a tool round trip",
		request: `{"model":"claude-sonnet","max_tokens":200,"tool_choice":{"type":"auto"},
			"tools":[
				{"name":"get_weather","description":"Weather for a city","cache_control":{"type":"ephemeral"},
					"input_schema":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}},
				{"type":"custom","name":"now","input_schema":{"type":"object"}},
				{"type":"bash_20250124","name":"bash"}],
			"messages":[
				{"role":"user","content":"Weather in Oslo, and the time?"},
				{"role":"assistant","content":[{"type":"text","text":"Checking."},
					{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":"Oslo"}},
					{"type":"tool_use","id":"toolu_2","name":"now","input":{}}]},
				{"role":"user","content":[
					{"type":"tool_result","tool_use_id":"toolu_1","content":"4 °C, rain"},
					{"type":"tool_result","tool_use_id":"toolu_2","content":[{"type":"text","text":"12:00"}],"is_error":false},
					{"type":"tool_result","tool_use_id":"toolu_3","is_error":true}]}]}`,
		answer: `{"id":"msg_03","type":"message","role":"assistant","model":"claude-sonnet","content":[
				{"type":"text","text":"Let me check Bergen too."},
				{"type":"tool_use","id":"toolu_4","name":"get_weather","input":{"city":"Bergen"}},
				{"type":"tool_use","id":"toolu_5","name":"now","input":{}}],
			"stop_reason":"tool_use","stop_sequence":null}`,
		// A tool's input arrives as JSON fragments; one with no arguments sends an empty
		// fragment, and its start's input stands.
		stream: typedEvents(
			`{"type":"message_start","message":{"id":"msg_03","type":"message","role":"assistant","model":"claude-sonnet","content":[],"stop_reason":null,"stop_sequence":null}}`,
			`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Let me check Bergen too."}}`,
			`{"type":"content_block_stop","index":0}`,
			`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_4","name":"get_weather","input":{}}}`,
			`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":""}}`,
			`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"city\": "}}`,
			`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"Bergen\"}"}}`,
			`{"type":"content_block_stop","index":1}`,
			`{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"toolu_5","name":"now","input":{}}}`,
			`{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":""}}`,
			`{"type":"content_block_stop","index":2}`,
			`{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":40}}`,
			`{"type":"message_stop"}`),
		want: map[string]any{
			"gen_ai.request.max_tokens": int64(200),
			"gen_ai.tool.definitions": `[
				{"type":"function","name":"get_weather","description":"Weather for a city",
				 "parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}},
				{"type":"function","name":"now","parameters":{"type":"object"}},
				{"type":"bash_20250124","name":"bash"}]`,
			"gen_ai.input.messages": `[
				{"role":"user","parts":[{"type":"text","content":"Weather in Oslo, and the time?"}]},
				{"role":"assistant","parts":[{"type":"text","content":"Checking."},
					{"type":"tool_call","id":"toolu_1","name":"get_weather","arguments":{"city":"Oslo"}},
					{"type":"tool_call","id":"toolu_2","name":"now","arguments":{}}]},
				{"role":"user","parts":[
					{"type":"tool_call_response","id":"toolu_1","response":"4 °C, rain"},
					{"type":"tool_call_response","id":"toolu_2","response":[{"type":"text","text":"12:00"}]},
					{"type":"tool_call_response","id":"toolu_3","response":null}]}]`,
			"gen_ai.response.id": "msg_03", "gen_ai.response.model": "claude-sonnet",
			"gen_ai.response.finish_reasons": []string{"tool_use"},
			"gen_ai.output.messages": `[{"role":"assistant","parts":[
				{"type":"text","content":"Let me check Bergen too."},
				{"type":"tool_call","id":"toolu_4","name":"get_weather","arguments":{"city":"Bergen"}},
				{"type":"tool_call","id":"toolu_5","name":"now","arguments":{}}]}]`,
		},
	},
	{
		name: "thinking",
		request: `{"model":"claude-sonnet","max_tokens":4000,"thinking":{"type":"enabled","budget_tokens":2000},"messages":[
			{"role":"user","content":"2+2?"},
			{"role":"assistant","content":[{"type":"thinking","thinking":"Simple sum.","signature":"sig1"},
				{"type":"redacted_thinking","data":"EmwKAhgB"},{"type":"text","text":"4"}]},
			{"role":"user","content":"And 3+3?"}]}`,
		answer: `{"id":"msg_04","type":"message","role":"assistant","model":"claude-sonnet","content":[
				{"type":"thinking","thinking":"Add three and three.","signature":"sig2"},{"type":"text","text":"6"}],
			"stop_reason":"end_turn","stop_sequence":null}`,
		stream: typedEvents(
			`{"type":"message_start","message":{"id":"msg_04","type":"message","role":"assistant","model":"claude-sonnet","content":[],"stop_reason":null,"stop_sequence":null}}`,
			`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"Add three"}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":" and three."}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig2"}}`,
			`{"type":"content_block_stop","index":0}`,
			`{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
			`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"6"}}`,
			`{"type":"content_block_stop","index":1}`,
			`{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null}}`,
			`{"type":"message_stop"}`),
		want: map[string]any{
			"gen_ai.request.max_tokens": int64(4000),
			"gen_ai.input.messages": `[
				{"role":"user","parts":[{"type":"text","content":"2+2?"}]},
				{"role":"assistant","parts":[{"type":"reasoning","content":"Simple sum."},
					{"type":"redacted_thinking","data":"EmwKAhgB"},{"type":"text","content":"4"}]},
				{"role":"user","parts":[{"type":"text","content":"And 3+3?"}]}]`,
			"gen_ai.response.id": "msg_04", "gen_ai.response.model": "claude-sonnet",
			"gen_ai.response.finish_reasons": []string{"end_turn"},
			"gen_ai.output.messages": `[{"role":"assistant","parts":[
				{"type":"reasoning","content":"Add three and three."},{"type":"text","content":"6"}]}]`,
		},
	},
	{
		name: "blocks the mapping does not know",
		request: `{"model":"claude-sonnet","max_tokens":100,"messages":[{"role":"user","content":[
			{"type":"text","text":"What happens?"},
			{"type":"search_result","source":"https://example.com","title":"T","content":[{"type":"text","text":"x"}]},
			{"zeta":1,"type":"x_custom","alpha":[true,null],"s":"<&>"}]}]}`,
		answer: `{"id":"msg_05","type":"message","role":"assistant","model":"claude-sonnet","content":[
				{"type":"redacted_thinking","data":"EmwKAhgB"},
				{"type":"text","text":"A ball rolls.","citations":[{"type":"search_result_location","cited_text":"x"}]}],
			"stop_reason":"end_turn","stop_sequence":null}`,
		// A block with no deltas arrives whole in its start; a citations delta is not
		// read, as an unstreamed text block's citations are not.
		stream: typedEvents(
			`{"type":"message_start","message":{"id":"msg_05","type":"message","role":"assistant","model":"claude-sonnet","content":[],"stop_reason":null,"stop_sequence":null}}`,
			`{"type":"content_block_start","index":0,"content_block":{"type":"redacted_thinking","data":"EmwKAhgB"}}`,
			`{"type":"content_block_stop","index":0}`,
			`{"type":"content_block_start","index":1,"content_block":{"type":"text","text":"","citations":[]}}`,
			`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"A ball"}}`,
			`{"type":"content_block_delta","index":1,"delta":{"type":"citations_delta","citation":{"type":"search_result_location","cited_text":"x"}}}`,
			`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":" rolls."}}`,
			`{"type":"content_block_stop","index":1}`,
			`{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null}}`,
			`{"type":"message_stop"}`),
		want: map[string]any{
			"gen_ai.request.max_tokens": int64(100),
			"gen_ai.input.messages": `[{"role":"user","parts":[
				{"type":"text","content":"What happens?"},
				{"type":"search_result","source":"https://example.com","title":"T","content":[{"type":"text","text":"x"}]},
				{"zeta":1,"type":"x_custom","alpha":[true,null],"s":"<&>"}]}]`,
			"gen_ai.response.id": "msg_05", "gen_ai.response.model": "claude-sonnet",
			"gen_ai.response.finish_reasons": []string{"end_turn"},
			"gen_ai.output.messages": `[{"role":"assistant","parts":[
				{"type":"redacted_thinking","data":"EmwKAhgB"},{"type":"text","content":"A ball rolls."}]}]`,
		},
	},
	{
		name:    "a tool call, vLLM (recorded)",
		request: recordedToolRequest,
		answer: `{"id":"chatcmpl-b3396b4cf70eadad","type":"message","role":"assistant","content":[
				{"type":"thinking","thinking":"The user is asking about the weather in Paris, so I'll use the tool.\n","signature":"5ac317b7bc8042e8839ea504dcc72e78"},
				{"type":"tool_use","id":"chatcmpl-tool-aa4a57d04710b585","name":"get_weather","input":{"city":"Paris"}}],
			"model":"unsloth/Qwen3.8-27B-NVFP4","stop_reason":"tool_use","usage":{"input_tokens":315,"output_tokens":46}}`,
		stream: string(fakebackend.Captured("vllm", "messages-stream-tool.sse")),
		want: with(recordedToolWant, map[string]any{
			"gen_ai.response.id": "chatcmpl-b3396b4cf70eadad", "gen_ai.response.model": "unsloth/Qwen3.8-27B-NVFP4",
			"gen_ai.response.finish_reasons": []string{"tool_use"},
			"gen_ai.output.messages": `[{"role":"assistant","parts":[
				{"type":"reasoning","content":"The user is asking about the weather in Paris, so I'll use the tool.\n"},
				{"type":"tool_call","id":"chatcmpl-tool-aa4a57d04710b585","name":"get_weather","arguments":{"city":"Paris"}}]}]`,
		}),
	},
	{
		// Its blocks interleave: the tool call opens before the thinking closes, and the
		// thinking's signature comes after the call's input.
		name:    "a tool call, llama-server (recorded)",
		request: recordedToolRequest,
		answer: `{"id":"chatcmpl-UI4BwYAnyeVXzNGjMhy8tklvNb15l3sO","type":"message","role":"assistant","content":[
				{"type":"thinking","thinking":"The user wants to know the weather in Paris. I have a weather tool available that takes a city as a parameter. Let me call it with \"Paris\".\n","signature":""},
				{"type":"tool_use","id":"nZbas9eDKT810gaJxe8S6Hww3rzy5X0R","name":"get_weather","input":{"city":"Paris"}}],
			"model":"unsloth/Qwen3.8-27B-GGUF:UD-Q4_K_XL","stop_reason":"tool_use","stop_sequence":null,
			"usage":{"cache_read_input_tokens":0,"input_tokens":277,"output_tokens":61}}`,
		stream: string(fakebackend.Captured("llama-server", "messages-stream-tool.sse")),
		want: with(recordedToolWant, map[string]any{
			"gen_ai.response.id":    "chatcmpl-UI4BwYAnyeVXzNGjMhy8tklvNb15l3sO",
			"gen_ai.response.model": "unsloth/Qwen3.8-27B-GGUF:UD-Q4_K_XL", "gen_ai.response.finish_reasons": []string{"tool_use"},
			"gen_ai.output.messages": `[{"role":"assistant","parts":[
				{"type":"reasoning","content":"The user wants to know the weather in Paris. I have a weather tool available that takes a city as a parameter. Let me call it with \"Paris\".\n"},
				{"type":"tool_call","id":"nZbas9eDKT810gaJxe8S6Hww3rzy5X0R","name":"get_weather","arguments":{"city":"Paris"}}]}]`,
		}),
	},
	{
		name:    "cut by the output limit, vLLM (recorded)",
		request: `{"model":"qwen","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"Count to fifty."}]}`,
		answer: `{"id":"chatcmpl-9f51e971d79257a4","type":"message","role":"assistant","content":[
				{"type":"thinking","thinking":"We need answer user's request: \"Count to fifty.\" Simple. Need final","signature":"13003184a89b4c1fafbc17d16e260426"}],
			"model":"unsloth/Qwen3.8-27B-NVFP4","stop_reason":"max_tokens","usage":{"input_tokens":56,"output_tokens":16}}`,
		stream: string(fakebackend.Captured("vllm", "messages-stream-max-tokens.sse")),
		want: map[string]any{
			"gen_ai.request.max_tokens": int64(16),
			"gen_ai.input.messages":     `[{"role":"user","parts":[{"type":"text","content":"Count to fifty."}]}]`,
			"gen_ai.response.id":        "chatcmpl-9f51e971d79257a4", "gen_ai.response.model": "unsloth/Qwen3.8-27B-NVFP4",
			"gen_ai.response.finish_reasons": []string{"max_tokens"},
			"gen_ai.output.messages": `[{"role":"assistant","parts":[
				{"type":"reasoning","content":"We need answer user's request: \"Count to fifty.\" Simple. Need final"}]}]`,
		},
	},
	{
		name:    "cut by the output limit, llama-server (recorded)",
		request: `{"model":"qwen","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"Count to fifty."}]}`,
		answer: `{"id":"chatcmpl-p69L5P4a7O100BwHbNYi3NdCWaeeLP5V","type":"message","role":"assistant","content":[
				{"type":"thinking","thinking":"The user wants me to count to fifty. This is straightforward - I'll list","signature":""}],
			"model":"unsloth/Qwen3.8-27B-GGUF:UD-Q4_K_XL","stop_reason":"max_tokens","stop_sequence":null,
			"usage":{"cache_read_input_tokens":0,"input_tokens":14,"output_tokens":16}}`,
		stream: string(fakebackend.Captured("llama-server", "messages-stream-max-tokens.sse")),
		want: map[string]any{
			"gen_ai.request.max_tokens": int64(16),
			"gen_ai.input.messages":     `[{"role":"user","parts":[{"type":"text","content":"Count to fifty."}]}]`,
			"gen_ai.response.id":        "chatcmpl-p69L5P4a7O100BwHbNYi3NdCWaeeLP5V",
			"gen_ai.response.model":     "unsloth/Qwen3.8-27B-GGUF:UD-Q4_K_XL", "gen_ai.response.finish_reasons": []string{"max_tokens"},
			"gen_ai.output.messages": `[{"role":"assistant","parts":[
				{"type":"reasoning","content":"The user wants me to count to fifty. This is straightforward - I'll list"}]}]`,
		},
	},
}

// Each case's request and answer give the corpus' attributes — the answer whole and
// streamed alike: every streamed answer assembles to its unstreamed twin.
func TestMessagesContent(t *testing.T) { expectCorpus(t, provider.Messages, messagesCorpus) }

// The servers' recorded unstreamed answers give their blocks: the thinking as
// reasoning, its signature left out, then the text.
func TestMessagesRecordedAnswers(t *testing.T) {
	const request = `{"model":"qwen","max_tokens":200,"messages":[{"role":"user","content":"Say hello in three words."}]}`
	input := `[{"role":"user","parts":[{"type":"text","content":"Say hello in three words."}]}]`
	for _, c := range []struct {
		server, id, model, output string
	}{
		{"vllm", "chatcmpl-b09f41cffcb0136a", "unsloth/Qwen3.8-27B-NVFP4", `[{"role":"assistant","parts":[
			{"type":"reasoning","content":"We need to answer user: \"Say hello in three words.\" Need output exactly three words? Likely \"Hello there friend\" or \"Hello my friend\". Must be three words. Final only three words.\n"},
			{"type":"text","content":"\n\nHello there friend"}]}]`},
		{"llama-server", "chatcmpl-BPMD8q776EZDnQmbp0czuUVtnFB4PChz", "unsloth/Qwen3.8-27B-GGUF:UD-Q4_K_XL", `[{"role":"assistant","parts":[
			{"type":"reasoning","content":"The user wants me to say hello in exactly three words. Let me think of options:\n\n- \"Hello to you\" - 3 words ✓\n- \"Hi there friend\" - 3 words ✓\n- \"Hello, how are\" - no, that's incomplete\n- \"Greetings to you\" - 3 words ✓\n- \"Hello, my friend\" - 3 words ✓\n\nI'll go with \"Hello to you\" - it's simple, natural, and exactly three words.\n"},
			{"type":"text","content":"Hello to you."}]}]`},
	} {
		got := messagesContent(t, request, string(fakebackend.Captured(c.server, "messages.json")), false, 200)
		expectAttributes(t, c.server, got, map[string]any{"gen_ai.request.max_tokens": int64(200),
			"gen_ai.input.messages": input, "gen_ai.response.id": c.id, "gen_ai.response.model": c.model,
			"gen_ai.response.finish_reasons": []string{"end_turn"}, "gen_ai.output.messages": c.output})
	}
}

// A stream cut short keeps what arrived: the blocks finished and the ones in
// progress — a tool call's half-written input as a string — with no finish reason.
// An event the stream ends inside was never whole and is not read. A non-stream
// answer cut short is no valid JSON, and gives nothing.
func TestMessagesCutShort(t *testing.T) {
	const request = `{"model":"claude-sonnet","max_tokens":100,"stream":true,
		"messages":[{"role":"user","content":"Weather?"}],
		"tools":[{"name":"get_weather","input_schema":{"type":"object"}}]}`
	stream := typedEvents(
		`{"type":"message_start","message":{"id":"msg_c","type":"message","role":"assistant","model":"claude-sonnet","content":[],"stop_reason":null}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"Use the tool."}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Checking."}}`,
		`{"type":"content_block_stop","index":1}`,
		`{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"toolu_9","name":"get_weather","input":{}}}`,
		`{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"city\": \"Os"}}`) +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"lo\"}"}}`
	input := `[{"role":"user","parts":[{"type":"text","content":"Weather?"}]}]`
	tools := `[{"type":"function","name":"get_weather","parameters":{"type":"object"}}]`
	expectAttributes(t, "cut stream", messagesContent(t, request, stream, true, 200), map[string]any{
		"gen_ai.request.max_tokens": int64(100), "gen_ai.input.messages": input, "gen_ai.tool.definitions": tools,
		"gen_ai.response.id": "msg_c", "gen_ai.response.model": "claude-sonnet",
		"gen_ai.output.messages": `[{"role":"assistant","parts":[
			{"type":"reasoning","content":"Use the tool."},{"type":"text","content":"Checking."},
			{"type":"tool_call","id":"toolu_9","name":"get_weather","arguments":"{\"city\": \"Os"}]}]`})

	// llama-server's recorded tool stream, cut inside the input's third fragment:
	// the thinking block is still open, its signature to come.
	recorded := string(fakebackend.Captured("llama-server", "messages-stream-tool.sse"))
	marker := `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"Paris"`
	cut := recorded[:strings.Index(recorded, marker)+len(marker)]
	expectAttributes(t, "recorded, cut", messagesContent(t, recordedToolRequest, cut, true, 200),
		with(recordedToolWant, map[string]any{
			"gen_ai.response.id": "chatcmpl-UI4BwYAnyeVXzNGjMhy8tklvNb15l3sO", "gen_ai.response.model": "unsloth/Qwen3.8-27B-GGUF:UD-Q4_K_XL",
			"gen_ai.output.messages": `[{"role":"assistant","parts":[
				{"type":"reasoning","content":"The user wants to know the weather in Paris. I have a weather tool available that takes a city as a parameter. Let me call it with \"Paris\".\n"},
				{"type":"tool_call","id":"nZbas9eDKT810gaJxe8S6Hww3rzy5X0R","name":"get_weather","arguments":"{\"city\":\""}]}]`}))

	// Cut right after message_start: the message, with nothing in it yet.
	opened := typedEvents(`{"type":"message_start","message":{"id":"msg_d","type":"message","role":"assistant","model":"claude-sonnet","content":[]}}`)
	expectAttributes(t, "opened only", messagesContent(t, request, opened, true, 200), map[string]any{
		"gen_ai.request.max_tokens": int64(100), "gen_ai.input.messages": input, "gen_ai.tool.definitions": tools,
		"gen_ai.response.id": "msg_d", "gen_ai.response.model": "claude-sonnet",
		"gen_ai.output.messages": `[{"role":"assistant","parts":[]}]`})

	// An error event ends it: what arrived stays.
	failed := typedEvents(
		`{"type":"message_start","message":{"id":"msg_e","type":"message","role":"assistant","model":"claude-sonnet","content":[]}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Partly"}}`,
		`{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`)
	expectAttributes(t, "error event", messagesContent(t, request, failed, true, 200), map[string]any{
		"gen_ai.request.max_tokens": int64(100), "gen_ai.input.messages": input, "gen_ai.tool.definitions": tools,
		"gen_ai.response.id": "msg_e", "gen_ai.response.model": "claude-sonnet",
		"gen_ai.output.messages": `[{"role":"assistant","parts":[{"type":"text","content":"Partly"}]}]`})

	whole := `{"id":"msg_c","type":"message","role":"assistant","model":"claude-sonnet","content":[{"type":"text","text":"Check`
	expectAttributes(t, "cut body", messagesContent(t, request, whole, false, 200), map[string]any{
		"gen_ai.request.max_tokens": int64(100), "gen_ai.input.messages": input, "gen_ai.tool.definitions": tools})
}

// An error answer — the backend's relayed 4xx, the gateway's own error body, as a
// JSON body even for a stream — gives no output: the request's content alone.
func TestMessagesErrorAnswer(t *testing.T) {
	const request = `{"model":"claude-sonnet","max_tokens":10,"system":"Be brief.","messages":[{"role":"user","content":"Hi"}]}`
	want := map[string]any{"gen_ai.request.max_tokens": int64(10),
		"gen_ai.system_instructions": `[{"type":"text","content":"Be brief."}]`,
		"gen_ai.input.messages":      `[{"role":"user","parts":[{"type":"text","content":"Hi"}]}]`}
	for _, c := range []struct {
		name     string
		response string
		stream   bool
		status   int
	}{
		{"relayed 400", `{"type":"error","error":{"type":"invalid_request_error","message":"bad"}}`, false, 400},
		{"recorded 404, vLLM", string(fakebackend.Captured("vllm", "messages-model-missing.json")), false, 404},
		{"gateway's 429 to a stream", `{"type":"error","error":{"type":"rate_limit_error","message":"limit","code":"rate_limited"}}`, true, 429},
		// Even one that looks like an answer: the status decides.
		{"answer-shaped 400", messagesCorpus[0].answer, false, 400},
		{"stream-shaped 529", messagesCorpus[0].stream, true, 529},
	} {
		expectAttributes(t, c.name, messagesContent(t, request, c.response, c.stream, c.status), want)
	}
}

// The spec's Messages example (docs/specs/GATEWAY.md, Request capture → Examples),
// built through the whole capture into a span: its name and the attributes it shows,
// exactly.
func TestMessagesSpecExample(t *testing.T) {
	const request = `{"model": "claude-sonnet", "max_tokens": 1024, "system": "Be terse.",
	 "tools": [{"name": "get_weather", "description": "Weather for a city",
	   "input_schema": {"type": "object", "properties": {"city": {"type": "string"}}}}],
	 "messages": [
	   {"role": "user", "content": "Weather in Oslo?"},
	   {"role": "assistant", "content": [
	     {"type": "tool_use", "id": "toolu_1", "name": "get_weather", "input": {"city": "Oslo"}}]},
	   {"role": "user", "content": [
	     {"type": "tool_result", "tool_use_id": "toolu_1", "content": "4 °C, rain"}]}]}`
	const answer = `{"id":"msg_x","type":"message","role":"assistant","model":"claude-sonnet",
		"content":[{"type":"text","text":"4 °C and rain."}],"stop_reason":"end_turn","stop_sequence":null,
		"usage":{"input_tokens":50,"output_tokens":7}}`
	expectSpecExample(t, provider.Messages, request, answer, "claude-sonnet", map[string]map[string]any{
		"gen_ai.request.max_tokens":  {"intValue": "1024"},
		"gen_ai.system_instructions": {"stringValue": compact(t, `[{"type": "text", "content": "Be terse."}]`)},
		"gen_ai.input.messages": {"stringValue": compact(t, `
			[{"role": "user", "parts": [{"type": "text", "content": "Weather in Oslo?"}]},
			 {"role": "assistant", "parts": [
			   {"type": "tool_call", "id": "toolu_1", "name": "get_weather",
			    "arguments": {"city": "Oslo"}}]},
			 {"role": "user", "parts": [
			   {"type": "tool_call_response", "id": "toolu_1", "response": "4 °C, rain"}]}]`)},
		"gen_ai.tool.definitions": {"stringValue": compact(t, `
			[{"type": "function", "name": "get_weather", "description": "Weather for a city",
			  "parameters": {"type": "object", "properties": {"city": {"type": "string"}}}}]`)},
		"gen_ai.output.messages": {"stringValue": compact(t, `
			[{"role": "assistant", "parts": [{"type": "text", "content": "4 °C and rain."}]}]`)},
		"gen_ai.response.finish_reasons": {"arrayValue": map[string]any{"values": []any{map[string]any{"stringValue": "end_turn"}}}},
	}, "msg_x")
}

// Content without its API's shape where the mapping reads it is left out, and the
// rest captured: wrong types everywhere, truncated JSON, empty input — never a panic.
func TestMessagesMalformed(t *testing.T) {
	for _, c := range []struct {
		name              string
		request, response string
		stream            bool
		want              map[string]any
	}{
		{name: "empty", want: map[string]any{}},
		{name: "truncated request", request: `{"model":"x","messages":[{"role":"user","content":"hi"}`, want: map[string]any{}},
		{name: "request a list", request: `[{"role":"user","content":"hi"}]`, want: map[string]any{}},
		{name: "messages an object", request: `{"messages":{"role":"user","content":"hi"},"system":5}`, want: map[string]any{}},
		{name: "messages a string", request: `{"messages":"hi","tools":"none","system":{"text":"x"}}`, want: map[string]any{}},
		{name: "system of every wrong shape", request: `{"system":[1,null,"x",{"type":5},{"text":"no type"},{"type":"text"},
			{"type":"text","text":5},{"type":"text","text":""},{"type":"text","text":"kept"}]}`,
			want: map[string]any{"gen_ai.system_instructions": `[{"type":"text","content":"kept"}]`}},
		{name: "an empty system", request: `{"system":"","messages":[]}`, want: map[string]any{}},
		{name: "messages of every wrong shape", request: `{"messages":[1,null,"x",[],{"content":"no role"},
			{"role":5,"content":"x"},
			{"role":"user","content":5},
			{"role":"user","content":{"text":"x"}},
			{"role":"user","content":[1,"x",null,[],{"type":5},{"text":"no type"},{"type":"text"},{"type":"text","text":["x"]},
				{"type":"text","text":""},{"type":"thinking"},{"type":"thinking","thinking":5},{"type":"thinking","thinking":""},
				{"type":"image"},{"type":"image","source":"x"},{"type":"image","source":{}},{"type":"image","source":{"type":5}},
				{"type":"image","source":{"type":"base64","data":5}},{"type":"image","source":{"type":"url"}},
				{"type":"document","source":{"type":"base64","media_type":5,"data":"QQ=="}},
				{"type":"tool_use","id":"t1"},{"type":"tool_use","name":5},
				{"type":"tool_use","id":5,"name":"f","input":"not json"},{"type":"tool_use","name":"g","input":null},
				{"type":"tool_result"},{"type":"tool_result","tool_use_id":5,"content":{"a":1}}]}]}`,
			want: map[string]any{"gen_ai.input.messages": `[
				{"role":"user","parts":[]},
				{"role":"user","parts":[]},
				{"role":"user","parts":[
					{"type":"blob","modality":"document","content":"QQ=="},
					{"type":"tool_call","name":"f","arguments":"not json"},
					{"type":"tool_call","name":"g"},
					{"type":"tool_call_response","response":null},
					{"type":"tool_call_response","response":{"a":1}}]}]`}},
		{name: "tools of every wrong shape", request: `{"tools":[1,null,"f",{"name":"no schema, no type"},
			{"input_schema":{"type":"object"}},{"name":5,"input_schema":{}},{"type":5,"name":"x"},
			{"name":"f","description":5,"input_schema":null,"type":"custom"},{"name":"g","input_schema":{"type":"object"}}]}`,
			want: map[string]any{"gen_ai.tool.definitions": `[
				{"name":"f","description":5,"input_schema":null,"type":"custom"},
				{"type":"function","name":"g","parameters":{"type":"object"}}]`}},
		{name: "parameters of the wrong types", request: `{"max_tokens":"10","temperature":"0.5","top_p":true,"top_k":1.5,
			"stop_sequences":[1,"x"]}`, want: map[string]any{}},
		{name: "parameters at their edges", request: `{"max_tokens":0,"top_k":-1,"temperature":0,"top_p":null,
			"stop_sequences":[]}`,
			want: map[string]any{"gen_ai.request.max_tokens": int64(0), "gen_ai.request.top_k": int64(-1),
				"gen_ai.request.temperature": 0.0, "gen_ai.request.stop_sequences": []string{}}},
		{name: "a member in another case is not the member", request: `{"Messages":[{"role":"user","content":"hi"}],
			"System":"x","Max_tokens":5,"messages":[{"Role":"user","content":"x"},{"role":"user","Content":"y"}]}`,
			want: map[string]any{"gen_ai.input.messages": `[{"role":"user","parts":[]}]`}},
		{name: "whitespace before the body", request: "\n\t {\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}",
			want: map[string]any{"gen_ai.input.messages": `[{"role":"user","parts":[{"type":"text","content":"hi"}]}]`}},
		{name: "answer a list", response: `[{"type":"text","text":"hi"}]`, want: map[string]any{}},
		{name: "answer without content", response: `{"id":"m","model":"x","stop_reason":"end_turn"}`,
			want: map[string]any{"gen_ai.response.id": "m", "gen_ai.response.model": "x"}},
		{name: "content an object", response: `{"content":{"type":"text","text":"hi"}}`, want: map[string]any{}},
		{name: "an answer of every wrong shape", response: `{"id":5,"model":["m"],"stop_reason":5,
			"content":[1,null,{"type":"text","text":5},{"text":"no type"},{"type":"text","text":"ok"}]}`,
			want: map[string]any{"gen_ai.output.messages": `[{"role":"assistant","parts":[{"type":"text","content":"ok"}]}]`}},
		{name: "an empty answer", response: `{"id":"m","content":[],"stop_reason":"end_turn"}`,
			want: map[string]any{"gen_ai.response.id": "m", "gen_ai.response.finish_reasons": []string{"end_turn"},
				"gen_ai.output.messages": `[{"role":"assistant","parts":[]}]`}},
		{name: "a stream that is a JSON body", stream: true,
			response: `{"id":"x","content":[{"type":"text","text":"hi"}]}`, want: map[string]any{}},
		{name: "a stream of comments", stream: true, response: ": ping\n\n: ping\n\n", want: map[string]any{}},
		{name: "a stream without message_start", stream: true, response: typedEvents(
			`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"lost"}}`,
			`{"type":"message_delta","delta":{"stop_reason":"end_turn"}}`), want: map[string]any{}},
		{name: "a stream that ends first", stream: true, response: typedEvents(
			`{"type":"message_start","message":{"id":"m"}}`, `{"type":"message_stop"}`,
			`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":"after the end"}}`,
			`{"type":"message_delta","delta":{"stop_reason":"end_turn"}}`),
			want: map[string]any{"gen_ai.response.id": "m", "gen_ai.output.messages": `[{"role":"assistant","parts":[]}]`}},
		{name: "a stream of every wrong shape", stream: true, response: typedEvents(
			`{"type":"message_start","message":5}`,
			`{"type":"message_start","message":{"id":"second","model":"m"}}`,
			`not json`, `[1]`, `{"type":5}`,
			`{"type":"content_block_start","content_block":{"type":"text","text":"no index"}}`,
			`{"type":"content_block_start","index":"3","content_block":{"type":"text","text":"index a string"}}`,
			`{"type":"content_block_start","index":4,"content_block":"x"}`,
			`{"type":"content_block_start","index":5,"content_block":{"text":"no type"}}`,
			`{"type":"content_block_delta","index":9,"delta":{"type":"text_delta","text":"never started"}}`,
			`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":5}}`,
			`{"type":"content_block_delta","index":0,"delta":"x"}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":5}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"kept"}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"not text"}}`,
			`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"a second start"}}`,
			`{"type":"content_block_stop","index":0}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"after its stop"}}`,
			`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"t","name":5,"input":{}}}`,
			`{"type":"content_block_start","index":2,"content_block":{"type":"thinking","thinking":"deep"}}`,
			`{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{}"}}`,
			`{"type":"message_delta","delta":"x"}`,
			`{"type":"message_delta","delta":{"stop_reason":5}}`),
			want: map[string]any{"gen_ai.output.messages": `[{"role":"assistant","parts":[
				{"type":"text","content":"kept"},{"type":"reasoning","content":"deep"}]}]`}},
	} {
		t.Run(c.name, func(t *testing.T) {
			expectAttributes(t, c.name, messagesContent(t, c.request, c.response, c.stream, 200), c.want)
		})
	}
}

// Whatever a client or a backend sends, reading it never panics, and every content
// attribute is valid JSON — the conventions' list — with one finish reason per output
// message when it has any.
func FuzzMessagesContent(f *testing.F) {
	for _, c := range messagesCorpus {
		f.Add([]byte(c.request), []byte(c.answer), false)
		f.Add([]byte(c.request), []byte(c.stream), true)
	}
	f.Add([]byte(`{"system":[{"type":"text","text":"x"}],"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"IMAGE/png;x=y","data":""}}]}]}`),
		[]byte("data: {\"type\":\"message_start\",\"message\":{}}\n\ndata: {\"type\":\"content_block_start\",\"index\":9223372036854775807,\"content_block\":{\"type\":\"tool_use\",\"name\":\"f\"}}\r\r"), true)
	f.Fuzz(func(t *testing.T, request, response []byte, stream bool) {
		if len(request)+len(response) > 64<<10 {
			t.Skip()
		}
		expectWellFormed(t, plainAttributes(t, content(provider.Messages,
			exchange{request: request, response: response, stream: stream, status: 200})))
	})
}
