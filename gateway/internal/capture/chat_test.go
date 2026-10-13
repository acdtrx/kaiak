package capture

import (
	"maps"
	"reflect"
	"slices"
	"testing"

	"kaiak/internal/provider"
)

// Chat Completions content (docs/specs/GATEWAY.md, Request capture → Chat Completions,
// Streams): a corpus of requests and answers in the shapes OpenAI, vLLM and
// llama-server send, each answer given whole and streamed.

// chatContent is the content attributes of a Chat Completions exchange.
func chatContent(t *testing.T, request, response string, stream bool, status int) map[string]any {
	t.Helper()
	return apiContent(t, provider.ChatCompletions, request, response, stream, status)
}

var chatCorpus = []contentCase{
	{
		name: "plain text, OpenAI",
		request: `{"model":"gpt-4.1","messages":[
			{"role":"system","content":"Be brief."},
			{"role":"developer","content":"Answer <in> English & French."},
			{"role":"user","name":"ann","content":"Hi"}],
			"max_completion_tokens":50,"max_tokens":99,"temperature":0.7,"top_p":1,"top_k":40,
			"frequency_penalty":0,"presence_penalty":-0.5,"stop":"\n\n","seed":42,"n":1,"user":"u-1"}`,
		answer: `{"id":"chatcmpl-1","object":"chat.completion","created":1700000000,"model":"gpt-4.1",
			"choices":[{"index":0,"message":{"role":"assistant","content":"Hello & <welcome>!","refusal":null,
			"annotations":[]},"logprobs":null,"finish_reason":"stop"}],
			"usage":{"prompt_tokens":20,"completion_tokens":5,"total_tokens":25},"service_tier":"default",
			"system_fingerprint":"fp_1"}`,
		stream: events(
			`{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1700000000,"model":"gpt-4.1","service_tier":"default","system_fingerprint":"fp_1","choices":[{"index":0,"delta":{"role":"assistant","content":"","refusal":null},"logprobs":null,"finish_reason":null}],"usage":null}`,
			`{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1700000000,"model":"gpt-4.1","service_tier":"default","system_fingerprint":"fp_1","choices":[{"index":0,"delta":{"content":"Hello & "},"logprobs":null,"finish_reason":null}],"usage":null}`,
			`{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1700000000,"model":"gpt-4.1","service_tier":"default","system_fingerprint":"fp_1","choices":[{"index":0,"delta":{"content":"<welcome>!"},"logprobs":null,"finish_reason":null}],"usage":null}`,
			`{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1700000000,"model":"gpt-4.1","service_tier":"default","system_fingerprint":"fp_1","choices":[{"index":0,"delta":{},"logprobs":null,"finish_reason":"stop"}],"usage":null}`,
			`{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1700000000,"model":"gpt-4.1","service_tier":"default","system_fingerprint":"fp_1","choices":[],"usage":{"prompt_tokens":20,"completion_tokens":5,"total_tokens":25}}`,
			`[DONE]`),
		want: map[string]any{
			"gen_ai.request.max_tokens": int64(50), "gen_ai.request.temperature": 0.7, "gen_ai.request.top_p": 1.0,
			"gen_ai.request.top_k": int64(40), "gen_ai.request.frequency_penalty": 0.0,
			"gen_ai.request.presence_penalty": -0.5, "gen_ai.request.stop_sequences": []string{"\n\n"},
			"gen_ai.request.seed": int64(42),
			"gen_ai.input.messages": `[
				{"role":"system","parts":[{"type":"text","content":"Be brief."}]},
				{"role":"developer","parts":[{"type":"text","content":"Answer <in> English & French."}]},
				{"role":"user","parts":[{"type":"text","content":"Hi"}],"name":"ann"}]`,
			"gen_ai.response.id": "chatcmpl-1", "gen_ai.response.model": "gpt-4.1",
			"gen_ai.response.finish_reasons": []string{"stop"},
			"gen_ai.output.messages":         `[{"role":"assistant","parts":[{"type":"text","content":"Hello & <welcome>!"}]}]`,
		},
	},
	{
		name: "images by data URL and by URL, vLLM",
		request: `{"model":"qwen-vl","stop":["END","STOP"],"messages":[{"role":"user","content":[
			{"type":"text","text":"Compare these."},
			{"type":"image_url","image_url":{"url":"data:image/PNG;base64,iVBORw0KGgo="}},
			{"type":"image_url","image_url":{"url":"https://example.com/cat.jpg","detail":"high"}},
			{"type":"image_url","image_url":{"url":"data:;base64,R0lGOD=="}}]}]}`,
		answer: `{"id":"chatcmpl-2","object":"chat.completion","created":1,"model":"qwen-vl","choices":[{"index":0,
			"message":{"role":"assistant","reasoning":null,"content":"Both are cats.","tool_calls":[]},"logprobs":null,
			"finish_reason":"stop","stop_reason":null}],"usage":{"prompt_tokens":900,"total_tokens":904,
			"completion_tokens":4},"prompt_logprobs":null,"kv_transfer_params":null}`,
		stream: events(
			`{"id":"chatcmpl-2","object":"chat.completion.chunk","created":1,"model":"qwen-vl","choices":[{"index":0,"delta":{"role":"assistant","content":""},"logprobs":null,"finish_reason":null}]}`,
			`{"id":"chatcmpl-2","object":"chat.completion.chunk","created":1,"model":"qwen-vl","choices":[{"index":0,"delta":{"content":"Both are"},"logprobs":null,"finish_reason":null}]}`,
			`{"id":"chatcmpl-2","object":"chat.completion.chunk","created":1,"model":"qwen-vl","choices":[{"index":0,"delta":{"content":" cats."},"logprobs":null,"finish_reason":null}]}`,
			`{"id":"chatcmpl-2","object":"chat.completion.chunk","created":1,"model":"qwen-vl","choices":[{"index":0,"delta":{"content":""},"logprobs":null,"finish_reason":"stop","stop_reason":null}]}`,
			`[DONE]`),
		want: map[string]any{
			"gen_ai.request.stop_sequences": []string{"END", "STOP"},
			"gen_ai.input.messages": `[{"role":"user","parts":[
				{"type":"text","content":"Compare these."},
				{"type":"blob","modality":"image","mime_type":"image/png","content":"iVBORw0KGgo="},
				{"type":"uri","modality":"image","uri":"https://example.com/cat.jpg"},
				{"type":"blob","modality":"image","content":"R0lGOD=="}]}]`,
			"gen_ai.response.id": "chatcmpl-2", "gen_ai.response.model": "qwen-vl",
			"gen_ai.response.finish_reasons": []string{"stop"},
			"gen_ai.output.messages":         `[{"role":"assistant","parts":[{"type":"text","content":"Both are cats."}]}]`,
		},
	},
	{
		name: "audio and files",
		request: `{"model":"gpt-4o-audio","messages":[{"role":"user","content":[
			{"type":"input_audio","input_audio":{"data":"UklGRiQ=","format":"wav"}},
			{"type":"input_audio","input_audio":{"data":"SUQz"}},
			{"type":"file","file":{"file_id":"file-abc"}},
			{"type":"file","file":{"file_data":"data:application/pdf;base64,JVBERi0=","filename":"a.pdf"}},
			{"type":"file","file":{"file_data":"data:image/jpeg;base64,/9j/4A=="}},
			{"type":"text","text":"Transcribe and summarise."}]}]}`,
		answer: `{"id":"chatcmpl-3","object":"chat.completion","created":1,"model":"gpt-4o-audio","choices":[{"index":0,
			"message":{"role":"assistant","content":"Done.","refusal":null},"logprobs":null,"finish_reason":"stop"}]}`,
		stream: events(
			`{"id":"chatcmpl-3","object":"chat.completion.chunk","created":1,"model":"gpt-4o-audio","choices":[{"index":0,"delta":{"role":"assistant","content":"","refusal":null},"finish_reason":null}]}`,
			`{"id":"chatcmpl-3","object":"chat.completion.chunk","created":1,"model":"gpt-4o-audio","choices":[{"index":0,"delta":{"content":"Done."},"finish_reason":null}]}`,
			`{"id":"chatcmpl-3","object":"chat.completion.chunk","created":1,"model":"gpt-4o-audio","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			`[DONE]`),
		want: map[string]any{
			"gen_ai.input.messages": `[{"role":"user","parts":[
				{"type":"blob","modality":"audio","mime_type":"audio/wav","content":"UklGRiQ="},
				{"type":"blob","modality":"audio","content":"SUQz"},
				{"type":"file","modality":"document","file_id":"file-abc"},
				{"type":"blob","modality":"document","mime_type":"application/pdf","content":"JVBERi0="},
				{"type":"blob","modality":"image","mime_type":"image/jpeg","content":"/9j/4A=="},
				{"type":"text","content":"Transcribe and summarise."}]}]`,
			"gen_ai.response.id": "chatcmpl-3", "gen_ai.response.model": "gpt-4o-audio",
			"gen_ai.response.finish_reasons": []string{"stop"},
			"gen_ai.output.messages":         `[{"role":"assistant","parts":[{"type":"text","content":"Done."}]}]`,
		},
	},
	{
		name: "tool calls, vLLM",
		request: `{"model":"qwen","tool_choice":"auto","messages":[{"role":"user","content":"Weather in Oslo, and the time?"}],
			"tools":[
			{"type":"function","function":{"name":"get_weather","description":"Weather for a city",
				"parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]},"strict":true}},
			{"type":"function","function":{"name":"now","parameters":null}}]}`,
		answer: `{"id":"chatcmpl-4","object":"chat.completion","created":1,"model":"qwen","choices":[{"index":0,"message":{
			"role":"assistant","content":null,"refusal":null,"annotations":null,"audio":null,"function_call":null,
			"tool_calls":[
				{"id":"chatcmpl-tool-a1","type":"function","function":{"name":"get_weather","arguments":"{\"city\": \"Oslo\"}"}},
				{"id":"chatcmpl-tool-b2","type":"function","function":{"name":"now","arguments":"{}"}}],
			"reasoning":null},"logprobs":null,"finish_reason":"tool_calls","stop_reason":null}]}`,
		stream: events(
			`{"id":"chatcmpl-4","object":"chat.completion.chunk","created":1,"model":"qwen","choices":[{"index":0,"delta":{"role":"assistant","content":""},"logprobs":null,"finish_reason":null}]}`,
			`{"id":"chatcmpl-4","object":"chat.completion.chunk","created":1,"model":"qwen","choices":[{"index":0,"delta":{"tool_calls":[{"id":"chatcmpl-tool-a1","type":"function","index":0,"function":{"name":"get_weather","arguments":""}}]},"logprobs":null,"finish_reason":null}]}`,
			`{"id":"chatcmpl-4","object":"chat.completion.chunk","created":1,"model":"qwen","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\": "}}]},"logprobs":null,"finish_reason":null}]}`,
			`{"id":"chatcmpl-4","object":"chat.completion.chunk","created":1,"model":"qwen","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"Oslo\"}"}}]},"logprobs":null,"finish_reason":null}]}`,
			`{"id":"chatcmpl-4","object":"chat.completion.chunk","created":1,"model":"qwen","choices":[{"index":0,"delta":{"tool_calls":[{"id":"chatcmpl-tool-b2","type":"function","index":1,"function":{"name":"now","arguments":"{}"}}]},"logprobs":null,"finish_reason":null}]}`,
			`{"id":"chatcmpl-4","object":"chat.completion.chunk","created":1,"model":"qwen","choices":[{"index":0,"delta":{"content":""},"logprobs":null,"finish_reason":"tool_calls","stop_reason":null}]}`,
			`[DONE]`),
		want: map[string]any{
			"gen_ai.input.messages": `[{"role":"user","parts":[{"type":"text","content":"Weather in Oslo, and the time?"}]}]`,
			"gen_ai.tool.definitions": `[
				{"type":"function","name":"get_weather","description":"Weather for a city",
				 "parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}},
				{"type":"function","name":"now"}]`,
			"gen_ai.response.id": "chatcmpl-4", "gen_ai.response.model": "qwen",
			"gen_ai.response.finish_reasons": []string{"tool_calls"},
			"gen_ai.output.messages": `[{"role":"assistant","parts":[
				{"type":"tool_call","id":"chatcmpl-tool-a1","name":"get_weather","arguments":{"city":"Oslo"}},
				{"type":"tool_call","id":"chatcmpl-tool-b2","name":"now","arguments":{}}]}]`,
		},
	},
	{
		name: "tool results and the follow-up, llama-server",
		request: `{"model":"qwen","messages":[
			{"role":"user","content":"Weather in Oslo?"},
			{"role":"assistant","content":null,"tool_calls":[
				{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Oslo\"}"}},
				{"id":"call_2","type":"function","function":{"name":"now","arguments":"not json"}}]},
			{"role":"tool","tool_call_id":"call_1","content":"4 °C, rain"},
			{"role":"tool","tool_call_id":"call_2","content":[{"type":"text","text":"12:00"}]}]}`,
		answer: `{"choices":[{"finish_reason":"stop","index":0,"message":{"role":"assistant","content":"4 °C and rain at noon."}}],
			"created":1,"model":"qwen","system_fingerprint":"b10802-d1a92352c","object":"chat.completion",
			"usage":{"completion_tokens":9,"prompt_tokens":60,"total_tokens":69},"id":"chatcmpl-ll1",
			"timings":{"prompt_n":60,"predicted_n":9}}`,
		stream: events(
			`{"choices":[{"finish_reason":null,"index":0,"delta":{"role":"assistant","content":null}}],"created":1,"id":"chatcmpl-ll1","model":"qwen","system_fingerprint":"b10802-d1a92352c","object":"chat.completion.chunk"}`,
			`{"choices":[{"finish_reason":null,"index":0,"delta":{"content":"4 °C and rain"}}],"created":1,"id":"chatcmpl-ll1","model":"qwen","system_fingerprint":"b10802-d1a92352c","object":"chat.completion.chunk"}`,
			`{"choices":[{"finish_reason":null,"index":0,"delta":{"content":" at noon."}}],"created":1,"id":"chatcmpl-ll1","model":"qwen","system_fingerprint":"b10802-d1a92352c","object":"chat.completion.chunk"}`,
			`{"choices":[{"finish_reason":"stop","index":0,"delta":{}}],"created":1,"id":"chatcmpl-ll1","model":"qwen","system_fingerprint":"b10802-d1a92352c","object":"chat.completion.chunk","usage":{"completion_tokens":9,"prompt_tokens":60,"total_tokens":69},"timings":{"prompt_n":60,"predicted_n":9}}`,
			`[DONE]`),
		want: map[string]any{
			"gen_ai.input.messages": `[
				{"role":"user","parts":[{"type":"text","content":"Weather in Oslo?"}]},
				{"role":"assistant","parts":[
					{"type":"tool_call","id":"call_1","name":"get_weather","arguments":{"city":"Oslo"}},
					{"type":"tool_call","id":"call_2","name":"now","arguments":"not json"}]},
				{"role":"tool","parts":[{"type":"tool_call_response","id":"call_1","response":"4 °C, rain"}]},
				{"role":"tool","parts":[{"type":"tool_call_response","id":"call_2","response":[{"type":"text","text":"12:00"}]}]}]`,
			"gen_ai.response.id": "chatcmpl-ll1", "gen_ai.response.model": "qwen",
			"gen_ai.response.finish_reasons": []string{"stop"},
			"gen_ai.output.messages":         `[{"role":"assistant","parts":[{"type":"text","content":"4 °C and rain at noon."}]}]`,
		},
	},
	{
		name: "reasoning, vLLM",
		request: `{"model":"qwen3","messages":[
			{"role":"user","content":"Hello"},
			{"role":"assistant","reasoning":"A greeting.","content":"Hi."},
			{"role":"user","content":"Again"}]}`,
		answer: `{"id":"chatcmpl-5","object":"chat.completion","created":1,"model":"qwen3","choices":[{"index":0,"message":{
			"role":"assistant","content":"Hi again!","refusal":null,"tool_calls":[],"reasoning":"The user greets. "},
			"logprobs":null,"finish_reason":"stop","stop_reason":null}]}`,
		stream: events(
			`{"id":"chatcmpl-5","object":"chat.completion.chunk","created":1,"model":"qwen3","choices":[{"index":0,"delta":{"role":"assistant","content":""},"logprobs":null,"finish_reason":null}]}`,
			`{"id":"chatcmpl-5","object":"chat.completion.chunk","created":1,"model":"qwen3","choices":[{"index":0,"delta":{"reasoning":"The user"},"logprobs":null,"finish_reason":null}]}`,
			`{"id":"chatcmpl-5","object":"chat.completion.chunk","created":1,"model":"qwen3","choices":[{"index":0,"delta":{"reasoning":" greets. "},"logprobs":null,"finish_reason":null}]}`,
			`{"id":"chatcmpl-5","object":"chat.completion.chunk","created":1,"model":"qwen3","choices":[{"index":0,"delta":{"content":"Hi again!"},"logprobs":null,"finish_reason":null}]}`,
			`{"id":"chatcmpl-5","object":"chat.completion.chunk","created":1,"model":"qwen3","choices":[{"index":0,"delta":{"content":""},"logprobs":null,"finish_reason":"stop","stop_reason":null}]}`,
			`[DONE]`),
		want: map[string]any{
			"gen_ai.input.messages": `[
				{"role":"user","parts":[{"type":"text","content":"Hello"}]},
				{"role":"assistant","parts":[{"type":"reasoning","content":"A greeting."},{"type":"text","content":"Hi."}]},
				{"role":"user","parts":[{"type":"text","content":"Again"}]}]`,
			"gen_ai.response.id": "chatcmpl-5", "gen_ai.response.model": "qwen3",
			"gen_ai.response.finish_reasons": []string{"stop"},
			"gen_ai.output.messages": `[{"role":"assistant","parts":[
				{"type":"reasoning","content":"The user greets. "},{"type":"text","content":"Hi again!"}]}]`,
		},
	},
	{
		name:    "reasoning, llama-server",
		request: `{"model":"qwen3","messages":[{"role":"user","content":"2+2?"}],"top_k":20}`,
		answer: `{"choices":[{"finish_reason":"stop","index":0,"message":{"role":"assistant","content":"4",
			"reasoning_content":"Simple sum."}}],"created":1,"model":"qwen3","system_fingerprint":"b10802-d1a92352c",
			"object":"chat.completion","id":"chatcmpl-ll2"}`,
		stream: events(
			`{"choices":[{"finish_reason":null,"index":0,"delta":{"role":"assistant","content":null}}],"created":1,"id":"chatcmpl-ll2","model":"qwen3","system_fingerprint":"b10802-d1a92352c","object":"chat.completion.chunk"}`,
			`{"choices":[{"finish_reason":null,"index":0,"delta":{"reasoning_content":"Simple"}}],"created":1,"id":"chatcmpl-ll2","model":"qwen3","system_fingerprint":"b10802-d1a92352c","object":"chat.completion.chunk"}`,
			`{"choices":[{"finish_reason":null,"index":0,"delta":{"reasoning_content":" sum."}}],"created":1,"id":"chatcmpl-ll2","model":"qwen3","system_fingerprint":"b10802-d1a92352c","object":"chat.completion.chunk"}`,
			`{"choices":[{"finish_reason":null,"index":0,"delta":{"content":"4"}}],"created":1,"id":"chatcmpl-ll2","model":"qwen3","system_fingerprint":"b10802-d1a92352c","object":"chat.completion.chunk"}`,
			`{"choices":[{"finish_reason":"stop","index":0,"delta":{}}],"created":1,"id":"chatcmpl-ll2","model":"qwen3","system_fingerprint":"b10802-d1a92352c","object":"chat.completion.chunk"}`,
			`[DONE]`),
		want: map[string]any{
			"gen_ai.request.top_k":  int64(20),
			"gen_ai.input.messages": `[{"role":"user","parts":[{"type":"text","content":"2+2?"}]}]`,
			"gen_ai.response.id":    "chatcmpl-ll2", "gen_ai.response.model": "qwen3",
			"gen_ai.response.finish_reasons": []string{"stop"},
			"gen_ai.output.messages": `[{"role":"assistant","parts":[
				{"type":"reasoning","content":"Simple sum."},{"type":"text","content":"4"}]}]`,
		},
	},
	{
		name:    "reasoning under both names",
		request: `{"model":"qwen3","messages":[{"role":"user","content":"Why?"}]}`,
		answer: `{"id":"chatcmpl-6","object":"chat.completion","created":1,"model":"qwen3","choices":[{"index":0,"message":{
			"role":"assistant","content":"Because.","reasoning_content":"Think.","reasoning":"Think."},"finish_reason":"stop"}]}`,
		stream: events(
			`{"id":"chatcmpl-6","object":"chat.completion.chunk","created":1,"model":"qwen3","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`,
			`{"id":"chatcmpl-6","object":"chat.completion.chunk","created":1,"model":"qwen3","choices":[{"index":0,"delta":{"reasoning_content":"Think.","reasoning":"Think."},"finish_reason":null}]}`,
			`{"id":"chatcmpl-6","object":"chat.completion.chunk","created":1,"model":"qwen3","choices":[{"index":0,"delta":{"content":"Because."},"finish_reason":null}]}`,
			`{"id":"chatcmpl-6","object":"chat.completion.chunk","created":1,"model":"qwen3","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			`[DONE]`),
		want: map[string]any{
			"gen_ai.input.messages": `[{"role":"user","parts":[{"type":"text","content":"Why?"}]}]`,
			"gen_ai.response.id":    "chatcmpl-6", "gen_ai.response.model": "qwen3",
			"gen_ai.response.finish_reasons": []string{"stop"},
			"gen_ai.output.messages": `[{"role":"assistant","parts":[
				{"type":"reasoning","content":"Think."},{"type":"text","content":"Because."}]}]`,
		},
	},
	{
		name:    "two choices",
		request: `{"model":"gpt-4.1","n":2,"max_tokens":3,"messages":[{"role":"user","content":"Name a letter."}]}`,
		// Choices answered out of order are written in index order.
		answer: `{"id":"chatcmpl-7","object":"chat.completion","created":1,"model":"gpt-4.1","choices":[
			{"index":1,"message":{"role":"assistant","content":"Bee is"},"finish_reason":"length"},
			{"index":0,"message":{"role":"assistant","content":"A."},"finish_reason":"stop"}]}`,
		// Chunks interleave the choices, and one chunk carries both.
		stream: events(
			`{"id":"chatcmpl-7","object":"chat.completion.chunk","created":1,"model":"gpt-4.1","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`,
			`{"id":"chatcmpl-7","object":"chat.completion.chunk","created":1,"model":"gpt-4.1","choices":[{"index":1,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`,
			`{"id":"chatcmpl-7","object":"chat.completion.chunk","created":1,"model":"gpt-4.1","choices":[{"index":1,"delta":{"content":"Bee"},"finish_reason":null}]}`,
			`{"id":"chatcmpl-7","object":"chat.completion.chunk","created":1,"model":"gpt-4.1","choices":[{"index":0,"delta":{"content":"A."},"finish_reason":null},{"index":1,"delta":{"content":" is"},"finish_reason":null}]}`,
			`{"id":"chatcmpl-7","object":"chat.completion.chunk","created":1,"model":"gpt-4.1","choices":[{"index":1,"delta":{},"finish_reason":"length"}]}`,
			`{"id":"chatcmpl-7","object":"chat.completion.chunk","created":1,"model":"gpt-4.1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			`[DONE]`),
		want: map[string]any{
			"gen_ai.request.max_tokens": int64(3), "gen_ai.request.choice.count": int64(2),
			"gen_ai.input.messages": `[{"role":"user","parts":[{"type":"text","content":"Name a letter."}]}]`,
			"gen_ai.response.id":    "chatcmpl-7", "gen_ai.response.model": "gpt-4.1",
			"gen_ai.response.finish_reasons": []string{"stop", "length"},
			"gen_ai.output.messages": `[
				{"role":"assistant","parts":[{"type":"text","content":"A."}]},
				{"role":"assistant","parts":[{"type":"text","content":"Bee is"}]}]`,
		},
	},
	{
		name: "a refusal",
		request: `{"model":"gpt-4.1","messages":[
			{"role":"user","content":"Something bad."},
			{"role":"assistant","content":[{"type":"refusal","refusal":"No."}]},
			{"role":"user","content":"Please?"}],
			"response_format":{"type":"json_schema","json_schema":{"name":"x","schema":{"type":"object"}}}}`,
		answer: `{"id":"chatcmpl-8","object":"chat.completion","created":1,"model":"gpt-4.1","choices":[{"index":0,"message":{
			"role":"assistant","content":null,"refusal":"I can't help with that."},"logprobs":null,"finish_reason":"stop"}]}`,
		stream: events(
			`{"id":"chatcmpl-8","object":"chat.completion.chunk","created":1,"model":"gpt-4.1","choices":[{"index":0,"delta":{"role":"assistant","content":null,"refusal":""},"logprobs":null,"finish_reason":null}]}`,
			`{"id":"chatcmpl-8","object":"chat.completion.chunk","created":1,"model":"gpt-4.1","choices":[{"index":0,"delta":{"refusal":"I can't"},"logprobs":null,"finish_reason":null}]}`,
			`{"id":"chatcmpl-8","object":"chat.completion.chunk","created":1,"model":"gpt-4.1","choices":[{"index":0,"delta":{"refusal":" help with that."},"logprobs":null,"finish_reason":null}]}`,
			`{"id":"chatcmpl-8","object":"chat.completion.chunk","created":1,"model":"gpt-4.1","choices":[{"index":0,"delta":{},"logprobs":null,"finish_reason":"stop"}]}`,
			`[DONE]`),
		want: map[string]any{
			"gen_ai.input.messages": `[
				{"role":"user","parts":[{"type":"text","content":"Something bad."}]},
				{"role":"assistant","parts":[{"type":"text","content":"No."}]},
				{"role":"user","parts":[{"type":"text","content":"Please?"}]}]`,
			"gen_ai.response.id": "chatcmpl-8", "gen_ai.response.model": "gpt-4.1",
			"gen_ai.response.finish_reasons": []string{"stop"},
			"gen_ai.output.messages":         `[{"role":"assistant","parts":[{"type":"text","content":"I can't help with that."}]}]`,
		},
	},
	{
		name: "parts and tools the mapping does not know",
		request: `{"model":"qwen-omni","messages":[{"role":"user","content":[
			{"type":"text","text":"What happens?"},
			{"type":"video_url","video_url":{"url":"https://example.com/v.mp4"}},
			{"zeta":1,"type":"x_custom","alpha":[true,null],"s":"<&>"}]}],
			"tools":[{"type":"custom","custom":{"name":"grep","description":"Search","format":{"type":"text"}}}]}`,
		answer: `{"id":"chatcmpl-9","object":"chat.completion","created":1,"model":"qwen-omni","choices":[{"index":0,
			"message":{"role":"assistant","content":"A ball rolls."},"finish_reason":"stop"}]}`,
		// A keep-alive comment and CRLF line endings change nothing.
		stream: ": ping\r\n\r\n" +
			"data: " + `{"id":"chatcmpl-9","object":"chat.completion.chunk","created":1,"model":"qwen-omni","choices":[{"index":0,"delta":{"role":"assistant","content":"A ball"},"finish_reason":null}]}` + "\r\n\r\n" +
			"data: " + `{"id":"chatcmpl-9","object":"chat.completion.chunk","created":1,"model":"qwen-omni","choices":[{"index":0,"delta":{"content":" rolls."},"finish_reason":"stop"}]}` + "\r\n\r\n" +
			"data: [DONE]\r\n\r\n",
		want: map[string]any{
			"gen_ai.input.messages": `[{"role":"user","parts":[
				{"type":"text","content":"What happens?"},
				{"type":"video_url","video_url":{"url":"https://example.com/v.mp4"}},
				{"zeta":1,"type":"x_custom","alpha":[true,null],"s":"<&>"}]}]`,
			"gen_ai.tool.definitions": `[{"type":"custom","custom":{"name":"grep","description":"Search","format":{"type":"text"}},"name":"grep"}]`,
			"gen_ai.response.id":      "chatcmpl-9", "gen_ai.response.model": "qwen-omni",
			"gen_ai.response.finish_reasons": []string{"stop"},
			"gen_ai.output.messages":         `[{"role":"assistant","parts":[{"type":"text","content":"A ball rolls."}]}]`,
		},
	},
	{
		// A custom tool's call is carried as the answer has it, generic; streamed, it is
		// rebuilt from its fragments — its name, its input in pieces — into the same.
		name: "a custom tool's call, OpenAI",
		request: `{"model":"gpt-5","messages":[{"role":"user","content":"Find the rain."}],
			"tools":[{"type":"custom","custom":{"name":"grep","description":"Search","format":{"type":"text"}}}]}`,
		answer: `{"id":"chatcmpl-10","object":"chat.completion","created":1,"model":"gpt-5","choices":[{"index":0,"message":{
			"role":"assistant","content":null,"refusal":null,"annotations":[],
			"tool_calls":[{"id":"call_g1","type":"custom","custom":{"name":"grep","input":"rain <&> \"wet\""}}]},
			"logprobs":null,"finish_reason":"tool_calls"}]}`,
		stream: events(
			`{"id":"chatcmpl-10","object":"chat.completion.chunk","created":1,"model":"gpt-5","choices":[{"index":0,"delta":{"role":"assistant","content":null,"refusal":null},"logprobs":null,"finish_reason":null}]}`,
			`{"id":"chatcmpl-10","object":"chat.completion.chunk","created":1,"model":"gpt-5","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_g1","type":"custom","custom":{"name":"grep","input":""}}]},"logprobs":null,"finish_reason":null}]}`,
			`{"id":"chatcmpl-10","object":"chat.completion.chunk","created":1,"model":"gpt-5","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"custom":{"input":"rain <&"}}]},"logprobs":null,"finish_reason":null}]}`,
			`{"id":"chatcmpl-10","object":"chat.completion.chunk","created":1,"model":"gpt-5","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"custom":{"input":"> \"wet\""}}]},"logprobs":null,"finish_reason":null}]}`,
			`{"id":"chatcmpl-10","object":"chat.completion.chunk","created":1,"model":"gpt-5","choices":[{"index":0,"delta":{},"logprobs":null,"finish_reason":"tool_calls"}]}`,
			`[DONE]`),
		want: map[string]any{
			"gen_ai.input.messages":   `[{"role":"user","parts":[{"type":"text","content":"Find the rain."}]}]`,
			"gen_ai.tool.definitions": `[{"type":"custom","custom":{"name":"grep","description":"Search","format":{"type":"text"}},"name":"grep"}]`,
			"gen_ai.response.id":      "chatcmpl-10", "gen_ai.response.model": "gpt-5",
			"gen_ai.response.finish_reasons": []string{"tool_calls"},
			"gen_ai.output.messages": `[{"role":"assistant","parts":[
				{"id":"call_g1","type":"custom","custom":{"name":"grep","input":"rain <&> \"wet\""}}]}]`,
		},
	},
}

// Each case's request and answer give the corpus' attributes — the answer whole and
// streamed alike: every streamed answer assembles to its unstreamed twin.
func TestChatCompletionsContent(t *testing.T) { expectCorpus(t, provider.ChatCompletions, chatCorpus) }

// A stream cut short keeps what arrived: the text finished, a tool call's
// half-written arguments as a string, a custom tool's call with its input so far; the
// choice that never finished has the finish reason error. A block the stream ends
// inside was never whole and is not read. A non-stream answer cut short is no valid
// JSON, and gives nothing.
func TestChatCompletionsCutShort(t *testing.T) {
	const request = `{"model":"qwen","stream":true,"messages":[{"role":"user","content":"Weather?"}],
		"tools":[{"type":"function","function":{"name":"get_weather","parameters":{"type":"object"}}}]}`
	stream := events(
		`{"id":"chatcmpl-c","object":"chat.completion.chunk","created":1,"model":"qwen","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`,
		`{"id":"chatcmpl-c","object":"chat.completion.chunk","created":1,"model":"qwen","choices":[{"index":0,"delta":{"content":"Checking."},"finish_reason":null}]}`,
		`{"id":"chatcmpl-c","object":"chat.completion.chunk","created":1,"model":"qwen","choices":[{"index":0,"delta":{"tool_calls":[{"id":"call_9","type":"function","index":0,"function":{"name":"get_weather","arguments":""}}]},"finish_reason":null}]}`,
		`{"id":"chatcmpl-c","object":"chat.completion.chunk","created":1,"model":"qwen","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\": \"Os"}}]},"finish_reason":null}]}`) +
		`data: {"id":"chatcmpl-c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"lo\"}"}}]}}]}`
	want := map[string]any{
		"gen_ai.input.messages":   `[{"role":"user","parts":[{"type":"text","content":"Weather?"}]}]`,
		"gen_ai.tool.definitions": `[{"type":"function","name":"get_weather","parameters":{"type":"object"}}]`,
		"gen_ai.response.id":      "chatcmpl-c", "gen_ai.response.model": "qwen",
		"gen_ai.response.finish_reasons": []string{"error"},
		"gen_ai.output.messages": `[{"role":"assistant","parts":[
			{"type":"text","content":"Checking."},
			{"type":"tool_call","id":"call_9","name":"get_weather","arguments":"{\"city\": \"Os"}]}]`,
	}
	expectAttributes(t, "cut stream", chatContent(t, request, stream, true, 200), want)

	custom := events(
		`{"id":"chatcmpl-g","model":"gpt-5","choices":[{"index":0,"delta":{"role":"assistant","content":null},"finish_reason":null}]}`,
		`{"id":"chatcmpl-g","model":"gpt-5","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_g","type":"custom","custom":{"name":"grep","input":""}}]},"finish_reason":null}]}`,
		`{"id":"chatcmpl-g","model":"gpt-5","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"custom":{"input":"ra"}}]},"finish_reason":null}]}`)
	expectAttributes(t, "cut custom call", chatContent(t, `{"model":"gpt-5"}`, custom, true, 200), map[string]any{
		"gen_ai.response.id": "chatcmpl-g", "gen_ai.response.model": "gpt-5",
		"gen_ai.response.finish_reasons": []string{"error"},
		"gen_ai.output.messages": `[{"role":"assistant","parts":[
			{"id":"call_g","type":"custom","custom":{"name":"grep","input":"ra"}}]}]`})

	whole := `{"id":"chatcmpl-c","object":"chat.completion","model":"qwen","choices":[{"index":0,"message":{"role":"assistant","content":"Check`
	expectAttributes(t, "cut body", chatContent(t, request, whole, false, 200), map[string]any{
		"gen_ai.input.messages": want["gen_ai.input.messages"], "gen_ai.tool.definitions": want["gen_ai.tool.definitions"]})
}

// An error answer — the backend's relayed 4xx, the gateway's own error body, as a
// JSON body even for a stream — gives no output: the request's content alone.
func TestChatCompletionsErrorAnswer(t *testing.T) {
	const request = `{"model":"qwen","temperature":0.5,"messages":[{"role":"user","content":"Hi"}]}`
	want := map[string]any{"gen_ai.request.temperature": 0.5,
		"gen_ai.input.messages": `[{"role":"user","parts":[{"type":"text","content":"Hi"}]}]`}
	for _, c := range []struct {
		name     string
		response string
		stream   bool
		status   int
	}{
		{"relayed 400", `{"error":{"message":"bad","type":"invalid_request_error","param":null,"code":null}}`, false, 400},
		{"gateway's 429 to a stream", `{"error":{"message":"limit","type":"rate_limit_error","code":"rate_limited"}}`, true, 429},
		// Even one that looks like an answer: the status decides.
		{"answer-shaped 400", chatCorpus[0].answer, false, 400},
		{"stream-shaped 502", chatCorpus[0].stream, true, 502},
	} {
		expectAttributes(t, c.name, chatContent(t, request, c.response, c.stream, c.status), want)
	}
}

// The spec's Chat Completions example (docs/specs/GATEWAY.md, Request capture →
// Examples), built through the whole capture into a span: its name and the
// attributes it shows, exactly.
func TestChatCompletionsSpecExample(t *testing.T) {
	const request = `{"model": "qwen3-32b", "stream": true, "temperature": 0.2,
	 "messages": [
	   {"role": "system", "content": "Answer briefly."},
	   {"role": "user", "content": [
	     {"type": "text", "text": "What is in this image?"},
	     {"type": "image_url", "image_url": {"url": "data:image/png;base64,iVBORw0K"}}]}],
	 "tools": [{"type": "function", "function": {"name": "lookup",
	   "description": "Look a word up", "parameters": {"type": "object"}}}]}`
	answer := events(
		`{"id":"chatcmpl-x","object":"chat.completion.chunk","created":1,"model":"qwen3-32b","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`,
		`{"id":"chatcmpl-x","object":"chat.completion.chunk","created":1,"model":"qwen3-32b","choices":[{"index":0,"delta":{"reasoning":"A cat on"},"finish_reason":null}]}`,
		`{"id":"chatcmpl-x","object":"chat.completion.chunk","created":1,"model":"qwen3-32b","choices":[{"index":0,"delta":{"reasoning":" a mat."},"finish_reason":null}]}`,
		`{"id":"chatcmpl-x","object":"chat.completion.chunk","created":1,"model":"qwen3-32b","choices":[{"index":0,"delta":{"content":"A cat."},"finish_reason":null}]}`,
		`{"id":"chatcmpl-x","object":"chat.completion.chunk","created":1,"model":"qwen3-32b","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`[DONE]`)
	exp := newFakeExporter()
	cs := newCaptures(t, 1<<20, exp)
	c := cs.Start(provider.ChatCompletions, []byte(request), nil)
	// The client receives the stream in pieces of any size.
	for piece := range slices.Chunk([]byte(answer), 7) {
		c.Append(piece)
	}
	s := summary("r-example")
	s.Model, s.Stream = "qwen3-32b", true
	c.Hand(s)
	finish(t, cs)
	spans := exp.exportAll()
	if len(spans) != 1 {
		t.Fatalf("%d spans, want 1", len(spans))
	}
	name, attrs, _ := decoded(t, spans[0])
	if name != "chat qwen3-32b" {
		t.Errorf("name %q, want chat qwen3-32b", name)
	}
	shown := map[string]map[string]any{
		"gen_ai.request.temperature": {"doubleValue": 0.2},
		"gen_ai.request.stream":      {"boolValue": true},
		"gen_ai.input.messages": {"stringValue": compact(t, `
			[{"role": "system", "parts": [{"type": "text", "content": "Answer briefly."}]},
			 {"role": "user", "parts": [
			   {"type": "text", "content": "What is in this image?"},
			   {"type": "blob", "modality": "image", "mime_type": "image/png",
			    "content": "iVBORw0K"}]}]`)},
		"gen_ai.tool.definitions": {"stringValue": compact(t, `
			[{"type": "function", "name": "lookup", "description": "Look a word up",
			  "parameters": {"type": "object"}}]`)},
		"gen_ai.output.messages": {"stringValue": compact(t, `
			[{"role": "assistant", "parts": [
			   {"type": "reasoning", "content": "A cat on a mat."},
			   {"type": "text", "content": "A cat."}]}]`)},
		"gen_ai.response.finish_reasons": {"arrayValue": map[string]any{"values": []any{map[string]any{"stringValue": "stop"}}}},
	}
	for k, want := range shown {
		if got := attrs[k]; !reflect.DeepEqual(got, want) {
			t.Errorf("%s = %v, want %v", k, got, want)
		}
	}
	// Besides, the span has the summary's attributes, and the answer's ID and model,
	// which the example does not show.
	if got, want := slices.Sorted(maps.Keys(attrs)), slices.Sorted(slices.Values(slices.Concat(slices.Collect(maps.Keys(shown)),
		[]string{"gen_ai.response.id", "gen_ai.response.model", "gen_ai.operation.name", "gen_ai.provider.name",
			"gen_ai.request.model", "gen_ai.usage.input_tokens", "gen_ai.usage.cache_read.input_tokens",
			"gen_ai.usage.cache_write.input_tokens", "gen_ai.usage.output_tokens", "gen_ai.usage.reasoning.output_tokens",
			"server.address", "server.port", "kaiak.request.id", "kaiak.key.id", "kaiak.key.group", "kaiak.key.root_group",
			"kaiak.backend.id", "kaiak.backend.type", "kaiak.deployment.model", "kaiak.attempts", "kaiak.usage.cost_usd",
			"kaiak.usage.estimated", "kaiak.usage.partial"}))); !slices.Equal(got, want) {
		t.Errorf("attributes\n got %v\nwant %v", got, want)
	}
	if attrs["gen_ai.response.id"]["stringValue"] != "chatcmpl-x" || attrs["gen_ai.response.model"]["stringValue"] != "qwen3-32b" {
		t.Errorf("response ID %v, model %v", attrs["gen_ai.response.id"], attrs["gen_ai.response.model"])
	}
}

// Content without its API's shape where the mapping reads it is left out, and the
// rest captured: wrong types everywhere, truncated JSON, empty input — never a panic.
func TestChatCompletionsMalformed(t *testing.T) {
	for _, c := range []struct {
		name              string
		request, response string
		stream            bool
		want              map[string]any
	}{
		{name: "empty", want: map[string]any{}},
		{name: "truncated request", request: `{"model":"x","messages":[{"role":"user","content":"hi"}`, want: map[string]any{}},
		{name: "request a list", request: `[{"role":"user","content":"hi"}]`, want: map[string]any{}},
		{name: "request a string", request: `"hi"`, want: map[string]any{}},
		{name: "request null", request: `null`, want: map[string]any{}},
		{name: "messages an object", request: `{"messages":{"role":"user","content":"hi"}}`, want: map[string]any{}},
		{name: "messages a string", request: `{"messages":"hi","tools":"none"}`, want: map[string]any{}},
		{name: "whitespace before the body", request: "\n\t {\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}",
			want: map[string]any{"gen_ai.input.messages": `[{"role":"user","parts":[{"type":"text","content":"hi"}]}]`}},
		{name: "messages of every wrong shape", request: `{"messages":[1,null,"x",[],{"content":"no role"},
			{"role":5,"content":"x"},
			{"role":"user","content":5},
			{"role":"user","content":{"text":"x"}},
			{"role":"user","content":[1,"x",null,[],{"type":5},{"text":"no type"},{"type":"text"},{"type":"text","text":["x"]},
				{"type":"text","text":""},{"type":"reasoning","text":"named as the conventions' part"},{"type":"blob"},
				{"type":"image_url","image_url":"https://example.com/a.png"},{"type":"image_url"},
				{"type":"image_url","image_url":{"url":5}},
				{"type":"input_audio","input_audio":{"data":5,"format":"wav"}},{"type":"input_audio","input_audio":"x"},
				{"type":"file","file":{"file_data":"https://example.com/a.pdf"}},{"type":"file","file":"x"},
				{"type":"file","file":{"file_id":5}},{"type":"refusal","refusal":5}]},
			{"role":"assistant","content":"ok","name":7,"tool_calls":{"a":1},"reasoning":5,"reasoning_content":null,"refusal":["x"]},
			{"role":"assistant","content":null,"tool_calls":[1,null,{"function":"x"},{"function":{"name":5}},
				{"id":5,"function":{"name":"f","arguments":{"a":1}}},{"id":"c2","function":{"name":"g","arguments":null}},
				{"type":"custom","id":"c3","custom":{"name":"grep","input":"x"}},{"type":"tool_call","id":"c4"}]},
			{"role":"tool","tool_call_id":5}]}`,
			want: map[string]any{"gen_ai.input.messages": `[
				{"role":"user","parts":[]},
				{"role":"user","parts":[]},
				{"role":"user","parts":[]},
				{"role":"assistant","parts":[{"type":"text","content":"ok"}]},
				{"role":"assistant","parts":[
					{"type":"tool_call","name":"f","arguments":{"a":1}},
					{"type":"tool_call","id":"c2","name":"g"},
					{"type":"custom","id":"c3","custom":{"name":"grep","input":"x"}}]},
				{"role":"tool","parts":[{"type":"tool_call_response","response":null}]}]`}},
		{name: "tools of every wrong shape", request: `{"tools":[1,null,"f",{"type":5},{"type":"function"},
			{"type":"function","function":"f"},{"type":"function","function":{"name":5}},
			{"type":"function","function":{"name":"f","description":5,"parameters":null}},{"function":{"name":"g"}},
			{"type":"x_tool","name":5},{"type":"x_tool","name":null},{"type":"x_tool"},{"type":"custom","custom":{"name":5}},
			{"type":"custom","custom":"grep"},{"type":"x_tool","name":"own","x_tool":{"name":"inner"}}]}`,
			want: map[string]any{"gen_ai.tool.definitions": `[{"type":"function","name":"f"},
				{"type":"x_tool","name":"x_tool"},{"type":"custom","custom":{"name":5},"name":"custom"},
				{"type":"custom","custom":"grep","name":"custom"},{"type":"x_tool","name":"own","x_tool":{"name":"inner"}}]`}},
		{name: "parameters of the wrong types", request: `{"temperature":"0.5","top_p":true,"top_k":1.5,"seed":1e3,
			"max_tokens":null,"max_completion_tokens":"10","frequency_penalty":1e400,"presence_penalty":null,
			"stop":[1,"x"],"n":"2"}`, want: map[string]any{}},
		{name: "parameters at their edges", request: `{"stop":[],"n":0,"seed":-1,"top_k":-1,"max_tokens":99999999999999999999,
			"max_completion_tokens":null,"temperature":0,"top_p":1e-300}`,
			want: map[string]any{"gen_ai.request.stop_sequences": []string{}, "gen_ai.request.choice.count": int64(0),
				"gen_ai.request.seed": int64(-1), "gen_ai.request.top_k": int64(-1), "gen_ai.request.temperature": 0.0,
				"gen_ai.request.top_p": 1e-300}},
		{name: "max_tokens when max_completion_tokens is null", request: `{"max_completion_tokens":null,"max_tokens":7}`,
			want: map[string]any{"gen_ai.request.max_tokens": int64(7)}},
		{name: "a member in another case is not the member", request: `{"Messages":[{"role":"user","content":"hi"}],
			"Temperature":0.5,"messages":[{"Role":"user","content":"x"},{"role":"user","Content":"y"}]}`,
			want: map[string]any{"gen_ai.input.messages": `[{"role":"user","parts":[]}]`}},
		{name: "answer a list", response: `[{"index":0,"message":{"content":"hi"}}]`, want: map[string]any{}},
		{name: "choices an object", response: `{"choices":{"index":0,"message":{"content":"hi"}}}`, want: map[string]any{}},
		{name: "choices of every wrong shape", response: `{"id":5,"model":["m"],"choices":[1,null,
			{"message":{"content":"no index"}},{"index":"0","message":{"content":"x"}},{"index":0},
			{"index":0,"message":"text"},{"index":1.5,"message":{"content":"x"}},
			{"index":2,"message":{"role":5,"content":"ok"},"finish_reason":5}]}`,
			want: map[string]any{"gen_ai.response.finish_reasons": []string{"error"},
				"gen_ai.output.messages": `[{"role":"assistant","parts":[{"type":"text","content":"ok"}]}]`}},
		{name: "whitespace before the answer", response: "\r\n {\"id\":\"a\",\"choices\":[{\"index\":0,\"message\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}]}",
			want: map[string]any{"gen_ai.response.id": "a", "gen_ai.response.finish_reasons": []string{"stop"},
				"gen_ai.output.messages": `[{"role":"assistant","parts":[{"type":"text","content":"hi"}]}]`}},
		{name: "a stream that is a JSON body", stream: true,
			response: `{"id":"x","choices":[{"index":0,"message":{"content":"hi"}}]}`, want: map[string]any{}},
		{name: "a stream of comments", stream: true, response: ": ping\n\n: ping\n\n", want: map[string]any{}},
		{name: "a stream that ends first", stream: true, response: events(`[DONE]`,
			`{"id":"late","choices":[{"index":0,"delta":{"content":"after the end"}}]}`), want: map[string]any{}},
		{name: "a stream of every wrong shape", stream: true, response: events(`not json`, `[1]`, `{"choices":"x"}`,
			`{"choices":[{"index":"0","delta":{"content":"lost"}}]}`,
			`{"id":"s-1","choices":[{"index":0,"delta":"x","finish_reason":"stop"}]}`,
			// One line: an event's data holds no line break.
			`{"choices":[{"index":0,"delta":{"role":5,"content":5,"refusal":{},"reasoning":[],"tool_calls":[{"function":{"name":"no index"}},{"index":0,"function":"x"},{"index":1,"id":"c1","function":{"name":"f","arguments":5}}]}}]}`,
			`{"choices":[{"index":0,"delta":{"tool_calls":"x"}}]}`,
			`[DONE]`, `{"choices":[{"index":0,"delta":{"content":"after the end"}}]}`) +
			"data:    {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"after the end\"}}]}\n\n",
			want: map[string]any{"gen_ai.response.id": "s-1", "gen_ai.response.finish_reasons": []string{"stop"},
				"gen_ai.output.messages": `[{"role":"assistant","parts":[{"type":"tool_call","id":"c1","name":"f"}]}]`}},
		{name: "spaces before an event's data", stream: true,
			response: "data:    {\"id\":\"w\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"x\"}}]}\n\n",
			want: map[string]any{"gen_ai.response.id": "w", "gen_ai.response.finish_reasons": []string{"error"},
				"gen_ai.output.messages": `[{"role":"assistant","parts":[{"type":"text","content":"x"}]}]`}},
		{name: "a custom call's fragments", stream: true, response: events(
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"custom":{"name":"grep","input":"a"}},{"index":1,"type":"custom","custom":{"name":5,"input":5}}]}}]}`,
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"not the call's kind"}},{"index":0,"custom":{"name":"later","input":"b"}}]}}]}`,
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"f"},"custom":{"input":"c"}}]},"finish_reason":"tool_calls"}]}`),
			want: map[string]any{"gen_ai.response.finish_reasons": []string{"tool_calls"},
				"gen_ai.output.messages": `[{"role":"assistant","parts":[
					{"type":"custom","custom":{"name":"grep","input":"abc"}},
					{"type":"custom","custom":{"name":"","input":""}}]}]`}},
	} {
		t.Run(c.name, func(t *testing.T) {
			expectAttributes(t, c.name, chatContent(t, c.request, c.response, c.stream, 200), c.want)
		})
	}
}

// Whatever a client or a backend sends, reading it never panics, and every content
// attribute is valid JSON — the conventions' list — with one finish reason per output
// message when it has any.
func FuzzChatCompletionsContent(f *testing.F) {
	for _, c := range chatCorpus {
		f.Add([]byte(c.request), []byte(c.answer), false)
		f.Add([]byte(c.request), []byte(c.stream), true)
	}
	f.Add([]byte(`{"messages":[{"role":"tool","content":{"a":[1,{"b":null}]}}]}`), []byte(`{"choices":[{"index":0}]}`), false)
	f.Add([]byte(`{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"DATA:Image/PNG;BASE64,"}}]}]}`),
		[]byte("data: {\"choices\":[{\"index\":9223372036854775807,\"delta\":{\"tool_calls\":[{\"index\":-1}]}}]}\r\r"), true)
	f.Fuzz(func(t *testing.T, request, response []byte, stream bool) {
		if len(request)+len(response) > 64<<10 {
			t.Skip()
		}
		expectWellFormed(t, plainAttributes(t, content(provider.ChatCompletions,
			exchange{request: request, response: response, stream: stream, status: 200})))
	})
}
