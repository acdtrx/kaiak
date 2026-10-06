// Regression tests from the independent pre-merge review of 2026-10-06
// (docs/reviews/2026-10-06/AUDIT-independent.md, [B]).

package accounting

import (
	"strings"
	"testing"

	"kaiak/internal/config"
	"kaiak/internal/provider"
)

// message_start's output count precedes the generated text. It cannot settle all
// later output as exact when the stream ends before message_delta usage arrives.
func TestMessagesPartialOutputAfterInitialUsage(t *testing.T) {
	m := streamMeter(provider.Messages, 400,
		`{"type":"message_start","message":{"usage":{"input_tokens":100,"cache_read_input_tokens":20,"cache_creation_input_tokens":10,"output_tokens":1}}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"`+strings.Repeat("word", 1000)+`"}}`)
	units, flags := m.Settle(false)
	if units[config.UnitTokensOut] < 1000 || !flags.Estimated || !flags.Partial {
		t.Errorf("4000 output bytes after initial usage settled as %v, flags=%+v; want partial estimated output >=1000", units, flags)
	}
	if units[config.UnitTokensIn] != 100 || units[config.UnitTokensCached] != 20 || units[config.UnitTokensCacheWrite] != 10 {
		t.Errorf("known input and cache counts must survive partial-output estimation: %v", units)
	}
}

// A streamed tool call's name counts toward the output estimate, from the event
// adding its item.
func TestResponsesStreamEstimateCountsToolNames(t *testing.T) {
	name := strings.Repeat("f", 64)
	m := streamMeter(provider.Responses, 40,
		`{"type":"response.output_item.added","item":{"type":"function_call","name":"`+name+`","arguments":""}}`,
		`{"type":"response.function_call_arguments.delta","delta":"{}"}`)
	units, flags := m.Settle(false)
	if want := EstimateTokens(int64(len(name) + 2)); units[config.UnitTokensOut] != want {
		t.Errorf("tool name + arguments: output=%d, want %d; flags=%+v", units[config.UnitTokensOut], want, flags)
	}
}

// Tool data that looks like a content part — a tool argument named source, a
// function schema holding a content list — is text: media count only at the
// format's documented content paths.
func TestEstimateToolDataIsNotMedia(t *testing.T) {
	text := strings.Repeat("ordinary source text ", 10000)
	for _, c := range []struct {
		name string
		ep   provider.Endpoint
		body string
	}{
		{"messages tool input source", provider.Messages,
			`{"model":"m","messages":[{"role":"assistant","content":[{"type":"tool_use","id":"t","name":"f","input":{"source":{"type":"url","text":"` + text + `"}}}]}]}`},
		{"responses schema content", provider.Responses,
			`{"model":"m","input":"hi","tools":[{"type":"function","name":"f","parameters":{"type":"object","properties":{"data":{"enum":[{"content":[{"type":"input_file","text":"` + text + `"}]}]}}}}]}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := EstimateInput(c.ep, []byte(c.body)).Total
			want := EstimateTokens(int64(len(c.body)))
			if got != want {
				t.Errorf("ordinary tool JSON counted as media: got %d input tokens, want %d", got, want)
			}
		})
	}
}

// Media in a tool call's output list (a multimodal tool result) count as media.
func TestEstimateResponsesToolOutputMedia(t *testing.T) {
	for _, part := range []string{
		`{"type":"input_image","file_id":"file_image"}`,
		`{"type":"input_file","file_id":"file_document"}`,
		`{"type":"input_file","file_url":"https://example.com/report.pdf"}`,
	} {
		body := `{"model":"m","input":[{"type":"function_call_output","call_id":"call_1","output":[` + part + `]}]}`
		if got := EstimateInput(provider.Responses, []byte(body)).Total; got < InlineMediaTokens {
			t.Errorf("tool output media estimated as %d tokens, want at least %d: %s", got, InlineMediaTokens, part)
		}
	}
}
