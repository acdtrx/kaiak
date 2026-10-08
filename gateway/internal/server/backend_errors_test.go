package server

import (
	"net/http"
	"strings"
	"testing"

	"kaiak/internal/config"
	"kaiak/internal/fakebackend"
)

// A backend 5xx is the backend's fault: its body — which may name backend
// internals — is replaced by the gateway's upstream_error answer under the
// backend's status; only its error code and type are logged, never its message.
// A backend 4xx is the caller's actionable error and is relayed as it came.
func TestBackendErrorBodies(t *testing.T) {
	g := newTestGateway(t)
	internals := `{"error":{"message":"CUDA out of memory on gpu-7.cluster.internal (worker pid 4242)","type":"server_error"}}`
	g.backend.SetReply(fakebackend.Reply{Status: http.StatusServiceUnavailable,
		Body: internals + strings.Repeat(" ", 100_000), Header: map[string]string{"Retry-After": "5"}})
	w := do(t, g.h, call{method: "POST", path: "/v1/chat/completions", key: userKey,
		body: `{"model":"open","messages":[]}`, header: map[string]string{"X-Request-Id": "fault"}})
	expectError(t, w, http.StatusServiceUnavailable, "upstream_error")
	if strings.Contains(w.Body.String(), "gpu-7") || strings.Contains(w.Body.String(), "CUDA") {
		t.Errorf("the backend's text reached the client: %s", w.Body.String())
	}
	if w.Header().Get("Retry-After") != "5" {
		t.Errorf("Retry-After %q, want the backend's", w.Header().Get("Retry-After"))
	}
	line := logLine(t, g, "fault")
	if !strings.Contains(line, `"kaiak.upstream.error.type":"server_error"`) || strings.Contains(line, "kaiak.upstream.error.code") ||
		!strings.Contains(line, `"error.type":"upstream_error"`) || !strings.Contains(line, `"http.response.status_code":503`) {
		t.Errorf("log line misses the backend's status and error type: %.3000s", line)
	}
	if strings.Contains(line, "CUDA") || strings.Contains(line, "gpu-7") || strings.Contains(line, "upstream_body") {
		t.Errorf("the backend's message reached the log: %.3000s", line)
	}
	expectMetricLines(t, g.metricsText(), `kaiak_errors_total{kaiak_error_class="upstream_error"} 1`)

	// The caller's 400 passes as the backend sent it.
	caller := `{"error":{"message":"This model's maximum context length is 8192 tokens.","type":"invalid_request_error","param":"messages","code":"context_length_exceeded"}}`
	g.backend.SetReply(fakebackend.Reply{Status: http.StatusBadRequest, Body: caller})
	w = do(t, g.h, call{method: "POST", path: "/v1/chat/completions", key: userKey, body: `{"model":"open","messages":[]}`})
	if w.Code != http.StatusBadRequest || w.Body.String() != caller {
		t.Errorf("4xx answer %d %q, want the backend's as it came", w.Code, w.Body.String())
	}
}

func TestBackendErrorFields(t *testing.T) {
	long := strings.Repeat("x", 100)
	for _, tc := range []struct {
		name, body, code, typ string
	}{
		{"OpenAI shape", `{"error":{"message":"summarize: my secret prompt","type":"server_error","param":null,"code":"overloaded"}}`, "overloaded", "server_error"},
		{"Azure shape", `{"error":{"code":"InternalServerError","message":"Backend returned unexpected response."}}`, "InternalServerError", ""},
		{"vLLM flat shape, integer code", `{"object":"error","message":"echo: my secret prompt","type":"InternalServerError","param":null,"code":500}`, "500", "InternalServerError"},
		{"null code", `{"error":{"message":"m","type":"server_error","code":null}}`, "", "server_error"},
		{"a code that reads as text", `{"error":{"code":"the prompt was: tell me a secret","type":"server error"}}`, "", ""},
		{"non-integer code", `{"error":{"code":1.5,"type":{"nested":"x"}}}`, "", ""},
		{"long identifiers clipped", `{"error":{"code":"` + long + `","type":"` + long + `"}}`, long[:64], long[:64]},
		{"trailing text after the value", `{"error":{"code":"c","type":"t"}}` + strings.Repeat(" ", 5000) + "garbage", "c", "t"},
		{"not JSON", `upstream connect error or disconnect/reset before headers`, "", ""},
		{"cut off", `{"error":{"message":"` + strings.Repeat("m", 5000), "", ""},
		{"empty", ``, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, typ := backendErrorFields([]byte(tc.body))
			if code != tc.code || typ != tc.typ {
				t.Errorf("code %q type %q, want %q %q", code, typ, tc.code, tc.typ)
			}
		})
	}
}

// An error answer whose body breaks before its first byte — cut, or timed out after
// its headers — is still the backend's answer. Accounting settles it as an answer
// (nothing processed: no units, no cost — not the estimated input of a request left
// unanswered), the circuit classifies it by its status, and the client gets the
// gateway's upstream_error under the backend's status with its Retry-After (the body,
// broken, cannot be relayed).
func TestErrorAnswerBrokenBeforeItsBodyIsAnsweredByItsStatus(t *testing.T) {
	for _, c := range []struct {
		name    string
		reply   fakebackend.Reply
		model   string
		outcome string
	}{
		{"400 cut", fakebackend.Reply{Status: http.StatusBadRequest, Before: fakebackend.CutBody}, "pair", "client_error"},
		{"429 cut", fakebackend.Reply{Status: http.StatusTooManyRequests, Before: fakebackend.CutBody}, "pair", "rate_limited"},
		{"500 cut", fakebackend.Reply{Status: http.StatusInternalServerError, Before: fakebackend.CutBody}, "pair", "server_error"},
		{"429 timed out after its headers", fakebackend.Reply{Status: http.StatusTooManyRequests, Before: fakebackend.StallBody}, "slow", "rate_limited"},
	} {
		t.Run(c.name, func(t *testing.T) {
			g := newTestGateway(t)
			s := testSnapshot(t, g.backend.URL())
			s.Models[c.model].MaxAttempts = 1
			g.holder.Swap(s)
			c.reply.Header = map[string]string{"Retry-After": "7"}
			g.backend.SetReply(c.reply)
			w := post(t, g, "broken", `{"model":"`+c.model+`","messages":[{"role":"user","content":"test"}]}`)
			expectError(t, w, c.reply.Status, "upstream_error")
			if w.Header().Get("Retry-After") != "7" {
				t.Errorf("Retry-After %q, want the backend's", w.Header().Get("Retry-After"))
			}
			records := recordsOf(g, "broken")
			if len(records) != 1 {
				t.Fatalf("%d records, want 1", len(records))
			}
			r := records[0]
			if r.Units[config.UnitTokensIn] != 0 || r.CostNanoUSD != 0 || r.Estimated || r.Partial {
				t.Errorf("record %+v, want an answer: no units, no cost, not estimated", r)
			}
			if !strings.Contains(g.metricsText(), `kaiak_attempt_outcome="`+c.outcome+`"} 1`) {
				t.Errorf("no attempt counted as %s:\n%s", c.outcome, g.metricsText())
			}
		})
	}
}
