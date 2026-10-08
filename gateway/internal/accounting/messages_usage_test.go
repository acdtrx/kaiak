package accounting

import (
	"bytes"
	"strings"
	"testing"

	"kaiak/internal/config"
	"kaiak/internal/fakebackend"
	"kaiak/internal/provider"
	"kaiak/internal/sse"
)

// recordedStreamMeter is a meter for ep fed a recorded stream (fakebackend/captures)
// event by event.
func recordedStreamMeter(t *testing.T, ep provider.Endpoint, server, name string) *Meter {
	t.Helper()
	data := fakebackend.Captured(server, name)
	m := NewMeter(ep, 99)
	m.Answered(200, true)
	events := sse.NewReader(bytes.NewReader(data), 1<<20)
	for {
		block, err := events.Next()
		if err != nil {
			break
		}
		if block.HasData {
			m.Observe(provider.Event{Data: block.Raw, Payload: block.Data})
		}
	}
	return m
}

// Messages usage maps straight onto the units — input_tokens already leaves out the
// cache — reading message_start and then message_delta, the latest value of each
// field winning (docs/specs/GATEWAY.md, Accounting → Messages usage).
func TestMessagesUsage(t *testing.T) {
	for _, c := range []struct {
		server, name string
		want         Units
	}{
		// vLLM repeats input_tokens in message_delta.
		{"vllm", "messages-stream.sse", withEveryTokenUnit(Units{config.UnitTokensIn: 62, config.UnitTokensOut: 171})},
		{"vllm", "messages-stream-tool.sse", withEveryTokenUnit(Units{config.UnitTokensIn: 315, config.UnitTokensOut: 46})},
		// llama-server sends only output_tokens there; cache reads come apart.
		{"llama-server", "messages-stream.sse", withEveryTokenUnit(Units{config.UnitTokensIn: 24, config.UnitTokensOut: 104})},
		{"llama-server", "messages-stream-tool.sse", withEveryTokenUnit(Units{config.UnitTokensIn: 277, config.UnitTokensOut: 61})},
	} {
		t.Run(c.server+"/"+c.name, func(t *testing.T) {
			m := recordedStreamMeter(t, provider.Messages, c.server, c.name)
			if !m.StreamContentSeen() {
				t.Error("no content seen")
			}
			units, flags := m.Settle(true)
			expect(t, units, flags, c.want, Flags{})
		})
	}

	t.Run("cache read and written, the latest value of each field", func(t *testing.T) {
		m := streamMeter(provider.Messages, 40,
			`{"type":"message_start","message":{"usage":{"input_tokens":5,"cache_read_input_tokens":20,"cache_creation_input_tokens":7,"output_tokens":1}}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`,
			`{"type":"message_delta","usage":{"output_tokens":9}}`,
			`{"type":"message_delta","usage":{"output_tokens":12,"cache_read_input_tokens":null}}`,
			`{"type":"message_stop"}`)
		units, flags := m.Settle(true)
		expect(t, units, flags, withEveryTokenUnit(Units{config.UnitTokensIn: 5, config.UnitTokensCached: 20, config.UnitTokensCacheWrite: 7, config.UnitTokensOut: 12}), Flags{})
	})
	t.Run("body", func(t *testing.T) {
		body := `{"id":"m","type":"message","content":[{"type":"text","text":"x"}],` +
			`"usage":{"input_tokens":3,"cache_read_input_tokens":2,"output_tokens":4}}`
		units, flags := bodyMeter(provider.Messages, 40, 200, body).Settle(true)
		expect(t, units, flags, withEveryTokenUnit(Units{config.UnitTokensIn: 3, config.UnitTokensCached: 2, config.UnitTokensOut: 4}), Flags{})
	})
}

// Without a usage report, Messages output is estimated from the generated content
// alone: text, thinking and tool-call input — streamed as deltas (in any block order)
// or in a body's content list; signatures and structure do not count.
func TestMessagesEstimatedOutput(t *testing.T) {
	t.Run("stream", func(t *testing.T) {
		m := streamMeter(provider.Messages, 40,
			`{"type":"message_start","message":{"usage":null}}`,
			`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"abcd"}}`,
			`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"t","name":"f"}}`,
			`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"a\":1}"}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"zzzzzzzzzzzz"}}`,
			`{"type":"content_block_delta","index":2,"delta":{"type":"text_delta","text":"héllo"}}`,
			`{"type":"message_stop"}`)
		// abcd 4 + {"a":1} 7 + héllo 6 = 17 bytes → 5.
		units, flags := m.Settle(true)
		expect(t, units, flags, withEveryTokenUnit(Units{config.UnitTokensIn: 10, config.UnitTokensOut: 5}), Flags{Estimated: true})
	})
	t.Run("body", func(t *testing.T) {
		body := `{"content":[{"type":"thinking","thinking":"abcd","signature":"zzzz"},{"type":"text","text":"efgh"},` +
			`{"type":"tool_use","id":"t","name":"f","input":{"a":1}}]}`
		// abcd 4 + efgh 4 + {"a":1} 7 = 15 bytes → 4.
		units, flags := bodyMeter(provider.Messages, 40, 200, body).Settle(true)
		expect(t, units, flags, withEveryTokenUnit(Units{config.UnitTokensIn: 10, config.UnitTokensOut: 4}), Flags{Estimated: true})
	})
}

// message_start's output count is provisional (0 or 1): the count that means
// anything comes in message_delta. A stream that ends before it — the client gone,
// a stall, the drain's cut, the backend dying — or a backend that ends without it
// keeps the reported input and cache units, estimates output from the content seen
// (never below the provisional count), and is flagged estimated
// (docs/specs/GATEWAY.md, Accounting → Messages usage).
func TestMessagesProvisionalOutput(t *testing.T) {
	start := `{"type":"message_start","message":{"usage":{"input_tokens":50,"cache_read_input_tokens":20,"cache_creation_input_tokens":7,"output_tokens":1}}}`
	text := `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"` + strings.Repeat("x", 16000) + `"}}`
	t.Run("cut before message_delta", func(t *testing.T) {
		units, flags := streamMeter(provider.Messages, 40, start, text).Settle(false)
		expect(t, units, flags, withEveryTokenUnit(Units{config.UnitTokensIn: 50, config.UnitTokensCached: 20, config.UnitTokensCacheWrite: 7, config.UnitTokensOut: 4000}), Flags{Estimated: true, Partial: true})
	})
	t.Run("ended without message_delta usage", func(t *testing.T) {
		units, flags := streamMeter(provider.Messages, 40, start, text,
			`{"type":"message_delta","delta":{"stop_reason":"end_turn"}}`, `{"type":"message_stop"}`).Settle(true)
		expect(t, units, flags, withEveryTokenUnit(Units{config.UnitTokensIn: 50, config.UnitTokensCached: 20, config.UnitTokensCacheWrite: 7, config.UnitTokensOut: 4000}), Flags{Estimated: true})
	})
	t.Run("never below the provisional count", func(t *testing.T) {
		units, flags := streamMeter(provider.Messages, 40,
			`{"type":"message_start","message":{"usage":{"input_tokens":50,"output_tokens":3}}}`).Settle(false)
		expect(t, units, flags, withEveryTokenUnit(Units{config.UnitTokensIn: 50, config.UnitTokensOut: 3}), Flags{Estimated: true, Partial: true})
	})
	t.Run("message_delta's count is final", func(t *testing.T) {
		units, flags := streamMeter(provider.Messages, 40, start, text,
			`{"type":"message_delta","usage":{"output_tokens":3900}}`).Settle(false)
		expect(t, units, flags, withEveryTokenUnit(Units{config.UnitTokensIn: 50, config.UnitTokensCached: 20, config.UnitTokensCacheWrite: 7, config.UnitTokensOut: 3900}), Flags{Partial: true})
	})
}

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

// A Messages image or document source counts as one media item when it is data, a
// URL or a file; a text source is text.
func TestEstimateMessagesMedia(t *testing.T) {
	const m = InlineMediaTokens
	payload := strings.Repeat("QUJD", 100_000)
	text := func(s string) int64 { return EstimateTokens(int64(len(s))) }
	for _, c := range []struct {
		name  string
		body  string
		total int64
	}{
		{"base64 image", `{"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + payload + `"}}]}]}`,
			text(`{"messages":[{"role":"user","content":[{"type":"image","source":}]}]}`) + m},
		{"data before type", `{"messages":[{"role":"user","content":[{"type":"document","source":{"data":"` + payload + `","type":"base64"}}]}]}`,
			text(`{"messages":[{"role":"user","content":[{"type":"document","source":}]}]}`) + m},
		{"url", `{"messages":[{"content":[{"type":"image","source":{"type":"url","url":"https://example.com/a.png"}}]}]}`,
			text(`{"messages":[{"content":[{"type":"image","source":}]}]}`) + m},
		{"file", `{"messages":[{"content":[{"type":"document","source":{"type":"file","file_id":"file_1"}}]}]}`,
			text(`{"messages":[{"content":[{"type":"document","source":}]}]}`) + m},
		{"text source is text", `{"messages":[{"content":[{"type":"document","source":{"type":"text","media_type":"text/plain","data":"some text"}}]}]}`,
			text(`{"messages":[{"content":[{"type":"document","source":{"type":"text","media_type":"text/plain","data":"some text"}}]}]}`)},
		{"content source holding an image", `{"messages":[{"content":[{"type":"document","source":{"type":"content","content":[{"type":"image","source":{"type":"base64","data":"` + payload + `"}}]}}]}]}`,
			text(`{"messages":[{"content":[{"type":"document","source":{"type":"content","content":[{"type":"image","source":}]}}]}]}`) + m},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := EstimateInput(provider.Messages, []byte(c.body))
			if got.Total != c.total || got.LargestPrompt != c.total {
				t.Errorf("estimate %+v, want %d", got, c.total)
			}
		})
	}
	// A tool's input member named source is the tool's argument, text: only an image
	// or document block's source is media.
	tool := `{"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"t","name":"fetch","input":{"source":{"type":"url","url":"https://example.com/a"}}}]}]}`
	if got := EstimateInput(provider.Messages, []byte(tool)); got.Total != text(tool) {
		t.Errorf("tool input: %+v, want %d", got, text(tool))
	}
	// Outside Messages, a member named source is plain text.
	body := `{"messages":[{"source":{"type":"base64","data":"` + payload + `"}}]}`
	if got := EstimateInput(provider.ChatCompletions, []byte(body)); got.Total != text(body) {
		t.Errorf("chat body: %+v, want %d", got, text(body))
	}
}
