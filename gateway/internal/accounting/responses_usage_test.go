package accounting

import (
	"strings"
	"testing"

	"kaiak/internal/provider"
)

// Responses usage comes from the response that response.completed (or
// response.incomplete) carries: input_tokens includes the cache, so the cached and
// written tokens come out of it; reasoning is the share of output_tokens its details
// report (docs/specs/GATEWAY.md, Accounting → Responses usage).
func TestResponsesUsage(t *testing.T) {
	for _, c := range []struct {
		server, name string
		want         Units
	}{
		{"vllm", "responses-stream.sse", tokenUnits(62, 0, 0, 98, 91)},
		{"vllm", "responses-stream-tool.sse", tokenUnits(315, 0, 0, 61, 33)},
		// vLLM ends a stream cut by max_output_tokens with response.completed.
		{"vllm", "responses-stream-incomplete.sse", tokenUnits(56, 0, 0, 16, 16)},
		// llama-server reports no output details: no reasoning share.
		{"llama-server", "responses-stream.sse", tokenUnits(24, 0, 0, 333, 0)},
		{"llama-server", "responses-stream-tool.sse", tokenUnits(4, 273, 0, 59, 0)},
		{"llama-server", "responses-stream-incomplete.sse", tokenUnits(4, 10, 0, 16, 0)},
	} {
		t.Run(c.server+"/"+c.name, func(t *testing.T) {
			m := recordedStreamMeter(t, provider.Responses, c.server, c.name)
			if !m.StreamContentSeen() {
				t.Error("no content seen")
			}
			units, flags := m.Settle(true)
			expect(t, units, flags, c.want, Flags{})
		})
	}

	t.Run("response.incomplete, cache read and written", func(t *testing.T) {
		m := streamMeter(provider.Responses, 40,
			`{"type":"response.created","response":{"usage":null}}`,
			`{"type":"response.output_text.delta","delta":"hi"}`,
			`{"type":"response.incomplete","response":{"status":"incomplete","usage":{"input_tokens":30,`+
				`"input_tokens_details":{"cached_tokens":20,"cache_write_tokens":7},"output_tokens":12,`+
				`"output_tokens_details":{"reasoning_tokens":5}}}}`)
		units, flags := m.Settle(true)
		expect(t, units, flags, tokenUnits(3, 20, 7, 12, 5), Flags{})
	})
	t.Run("clamped", func(t *testing.T) {
		m := streamMeter(provider.Responses, 40,
			`{"type":"response.completed","response":{"usage":{"input_tokens":10,`+
				`"input_tokens_details":{"cached_tokens":8,"cache_write_tokens":9},"output_tokens":3,`+
				`"output_tokens_details":{"reasoning_tokens":7}}}}`)
		units, flags := m.Settle(true)
		expect(t, units, flags, tokenUnits(0, 8, 2, 3, 3), Flags{})
	})
	t.Run("body", func(t *testing.T) {
		body := `{"id":"r","object":"response","output":[{"type":"message","content":[{"type":"output_text","text":"x"}]}],` +
			`"usage":{"input_tokens":5,"input_tokens_details":{"cached_tokens":2},"output_tokens":4}}`
		units, flags := bodyMeter(provider.Responses, 40, 200, body).Settle(true)
		expect(t, units, flags, tokenUnits(3, 2, 0, 4, 0), Flags{})
	})
}

// Without a usage report, Responses output is estimated from the generated content
// alone: answer text and refusals, reasoning text and summaries, tool-call names,
// arguments and inputs — streamed as deltas or in a body's output items; structure
// and IDs do not count.
func TestResponsesEstimatedOutput(t *testing.T) {
	t.Run("stream", func(t *testing.T) {
		m := streamMeter(provider.Responses, 40,
			`{"type":"response.created","response":{"id":"resp_1","output":[]}}`,
			`{"type":"response.reasoning_text.delta","item_id":"rs_1","delta":"abcd"}`,
			`{"type":"response.output_item.added","item":{"type":"function_call","name":"f","arguments":""}}`,
			`{"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":"{\"a\":1}"}`,
			`{"type":"response.output_text.delta","item_id":"msg_1","delta":"héllo"}`,
			`{"type":"response.completed","response":{"id":"resp_1","status":"completed"}}`)
		// abcd 4 + {"a":1} 7 + héllo 6 = 17 bytes → 5.
		units, flags := m.Settle(true)
		expect(t, units, flags, tokenUnits(10, 0, 0, 5, 0), Flags{Estimated: true})
	})
	t.Run("body", func(t *testing.T) {
		body := `{"output":[{"type":"reasoning","summary":[{"type":"summary_text","text":"ab"}],` +
			`"content":[{"type":"reasoning_text","text":"cd"}]},` +
			`{"type":"message","content":[{"type":"output_text","text":"efgh"},{"type":"refusal","refusal":"no"}]},` +
			`{"type":"function_call","name":"f","arguments":"{\"a\":1}","call_id":"c"},` +
			`{"type":"custom_tool_call","name":"g","input":"xyz"}]}`
		// ab 2 + cd 2 + efgh 4 + no 2 + f 1 + {"a":1} 7 + g 1 + xyz 3 = 22 bytes → 6.
		units, flags := bodyMeter(provider.Responses, 40, 200, body).Settle(true)
		expect(t, units, flags, tokenUnits(10, 0, 0, 6, 0), Flags{Estimated: true})
	})
}

// A Responses input_image or input_file part counts as one media item, whatever it
// carries; other parts are text.
func TestEstimateResponsesMedia(t *testing.T) {
	const m = InlineMediaTokens
	payload := strings.Repeat("QUJD", 100_000)
	text := func(s string) int64 { return EstimateTokens(int64(len(s))) }
	for _, c := range []struct {
		name  string
		body  string
		total int64
	}{
		{"image by data URL", `{"input":[{"role":"user","content":[{"type":"input_text","text":"hi"},{"type":"input_image","image_url":"data:image/png;base64,` + payload + `"}]}]}`,
			text(`{"input":[{"role":"user","content":[{"type":"input_text","text":"hi"},]}]}`) + m},
		{"image by file ID, type last", `{"input":[{"role":"user","content":[{"file_id":"file_1","detail":"auto","type":"input_image"}]}]}`,
			text(`{"input":[{"role":"user","content":[]}]}`) + m},
		{"file by URL", `{"input":[{"role":"user","content":[{"type":"input_file","file_url":"https://example.com/a.pdf"}]}]}`,
			text(`{"input":[{"role":"user","content":[]}]}`) + m},
		{"file by data", `{"input":[{"role":"user","content":[{"type":"input_file","filename":"a.pdf","file_data":"data:application/pdf;base64,` + payload + `"}]}]}`,
			text(`{"input":[{"role":"user","content":[]}]}`) + m},
		{"text content", `{"input":[{"role":"user","content":"just text"}],"instructions":"be brief"}`,
			text(`{"input":[{"role":"user","content":"just text"}],"instructions":"be brief"}`)},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := EstimateInput(provider.Responses, []byte(c.body))
			if got.Total != c.total || got.LargestPrompt != c.total {
				t.Errorf("estimate %+v, want %d", got, c.total)
			}
		})
	}
}
