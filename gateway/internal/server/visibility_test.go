package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"kaiak/internal/config"
	"kaiak/internal/fakebackend"
	"kaiak/internal/limits"
)

// logFields decodes request id's log line.
func logFields(t *testing.T, g *testGateway, id string) map[string]any {
	t.Helper()
	var fields map[string]any
	if err := json.Unmarshal([]byte(logLine(t, g, id)), &fields); err != nil {
		t.Fatal(err)
	}
	return fields
}

// expectFields checks that fields hold want (numbers as JSON decodes them) and none of
// absent.
func expectFields(t *testing.T, fields map[string]any, want map[string]any, absent ...string) {
	t.Helper()
	for k, v := range want {
		if got, ok := fields[k]; !ok || got != v {
			t.Errorf("%s = %v (present %v), want %v", k, got, ok, v)
		}
	}
	for _, k := range absent {
		if _, ok := fields[k]; ok {
			t.Errorf("%s = %v, want it absent", k, fields[k])
		}
	}
	if t.Failed() {
		t.Logf("line: %v", fields)
	}
}

func postAs(t *testing.T, g *testGateway, key, id, body string) *httptest.ResponseRecorder {
	t.Helper()
	return do(t, g.h, call{method: "POST", path: "/v1/chat/completions", key: key, body: body,
		header: map[string]string{"X-Request-Id": id}})
}

// D6: a limit refusal logs which limit refused — its kind of scope, its group's ID
// (never a key, never a label), its type, the value enforced and the value
// configured, and what was used — and every line names the key's group. The
// refusals are counted by scope kind and limit type, every series present from the
// start.
func TestLimitRefusalsLogTheLimit(t *testing.T) {
	g := newTestGateway(t)
	g.holder.Swap(testSnapshotWith(t, g.backend.URL(), func(doc string) string {
		doc = replaceOnce(t, doc, `"research": {}`, `"research": { "labels": { "cost_center": "label-value-never-logged" }, "limits": [{ "type": "usd_per_month", "value": 0 }] }`)
		doc = replaceOnce(t, doc, `"allowed_models": ["*"] }`, `"allowed_models": ["*"], "limits": [{ "type": "requests_per_minute", "value": 1 }] }`)
		return replaceOnce(t, doc, `"global": { `, `"global": { "limits": [{ "type": "tokens_per_minute", "value": 100 }], `)
	}))

	// The budget first: a refused request counts toward no limit, so eval's request
	// limit is still free for the admitted request after it.
	usd := postAs(t, g, workloadKey, "usd", `{"model":"pair","messages":[]}`)
	expectError(t, usd, http.StatusTooManyRequests, "budget_exceeded")
	// The client learns the kind of scope, never the group's ID or labels.
	if body := usd.Body.String(); !strings.Contains(body, "group limit") || strings.Contains(body, "research") ||
		strings.Contains(body, "label-value-never-logged") {
		t.Errorf("refusal message must name the scope kind only: %s", body)
	}
	expectFields(t, logFields(t, g, "usd"), map[string]any{"error.type": "budget_exceeded",
		"kaiak.limit.scope": "group", "kaiak.limit.group": "research", "kaiak.limit.type": "usd_per_month",
		"kaiak.limit.enforced": float64(0), "kaiak.limit.configured": float64(0), "kaiak.limit.used": float64(0)})

	if w := postAs(t, g, workloadKey, "admitted", chatBody); w.Code != http.StatusOK {
		t.Fatalf("admitted: status %d", w.Code)
	}
	expectFields(t, logFields(t, g, "admitted"), map[string]any{"kaiak.key.group": "eval", "gen_ai.request.stream": false},
		"team", "workload", "user", "kaiak.limit.scope")
	if w := postAs(t, g, userKey, "user-admitted", chatBody); w.Code != http.StatusOK {
		t.Fatalf("user-admitted: status %d", w.Code)
	}
	expectFields(t, logFields(t, g, "user-admitted"), map[string]any{"kaiak.key.group": "ann"}, "team", "workload", "user")

	expectError(t, postAs(t, g, workloadKey, "rpm", chatBody), http.StatusTooManyRequests, "rate_limit_exceeded")
	expectFields(t, logFields(t, g, "rpm"), map[string]any{"error.type": "rate_limit_exceeded",
		"kaiak.limit.scope": "group", "kaiak.limit.group": "eval", "kaiak.limit.type": "requests_per_minute",
		"kaiak.limit.enforced": float64(1), "kaiak.limit.configured": float64(1), "kaiak.limit.used": float64(1),
		"kaiak.key.group": "eval"})

	expectError(t, postAs(t, g, userKey, "tpm", `{"model":"Org/open-7b","messages":[{"role":"user","content":"`+
		strings.Repeat("a prompt well over a hundred tokens ", 20)+`"}]}`),
		http.StatusTooManyRequests, "rate_limit_exceeded")
	tpm := logFields(t, g, "tpm")
	expectFields(t, tpm, map[string]any{"kaiak.limit.scope": "global",
		"kaiak.limit.type": "tokens_per_minute", "kaiak.limit.enforced": float64(100), "kaiak.limit.configured": float64(100),
		"kaiak.key.group": "ann"}, "kaiak.limit.group", "kaiak.limit.id")
	// A token refusal says what the request asked for: here more than the limit
	// allows at all (a request too large, not a full window).
	if requested, ok := tpm["kaiak.limit.requested"].(float64); !ok || requested <= 100 {
		t.Errorf("kaiak.limit.requested %v, want the request's reservation, above the limit", tpm["kaiak.limit.requested"])
	}
	if _, ok := logFields(t, g, "rpm")["kaiak.limit.requested"]; ok {
		t.Error("a request limit's refusal logs kaiak.limit.requested")
	}

	for _, id := range []string{"rpm", "usd", "tpm"} {
		if line := logLine(t, g, id); strings.Contains(line, workloadKey) || strings.Contains(line, userKey) ||
			strings.Contains(line, "label-value-never-logged") {
			t.Errorf("a key or a label reached the log: %s", line)
		}
	}
	expectMetricLines(t, g.metricsText(),
		`kaiak_limit_rejections_total{scope_kind="group",type="requests_per_minute"} 1`,
		`kaiak_limit_rejections_total{scope_kind="group",type="usd_per_month"} 1`,
		`kaiak_limit_rejections_total{scope_kind="global",type="tokens_per_minute"} 1`,
		`kaiak_limit_rejections_total{scope_kind="group",type="tokens_per_hour"} 0`,
		`kaiak_limit_rejections_total{scope_kind="global",type="usd_per_month"} 0`)
}

// D6: a budget refused as unavailable names the USD limit it could not check; its
// spend is unknown, so no used amount. It is no caller's limit hit: not counted
// among the limit rejections.
func TestBudgetUnavailableLogsTheLimit(t *testing.T) {
	lost := time.Now().Add(-time.Hour)
	g := buildTestGateway(t, testOptions{limiter: func(h *config.Holder) *limits.Limiter {
		return limits.NewShared(h, time.Now, func() limits.Contact { return limits.Contact{Last: lost} }, nil)
	}})
	withLimits(t, g, `[{ "type": "usd_per_month", "value": 100 }]`, "")
	expectError(t, postAs(t, g, workloadKey, "unavailable", `{"model":"pair","messages":[]}`),
		http.StatusServiceUnavailable, "budget_unavailable")
	expectFields(t, logFields(t, g, "unavailable"), map[string]any{"error.type": "budget_unavailable",
		"kaiak.limit.scope": "group", "kaiak.limit.group": "research", "kaiak.limit.type": "usd_per_month",
		"kaiak.limit.enforced": float64(100), "kaiak.limit.configured": float64(100), "kaiak.key.group": "eval"},
		"kaiak.limit.used")
	expectMetricLines(t, g.metricsText(), `kaiak_limit_rejections_total{scope_kind="group",type="usd_per_month"} 0`)
}

// D6: a backend error status relayed to the client logs its class as the error code
// and the error code and type its body names — never its message.
func TestRelayedBackendErrorsLogTheirClass(t *testing.T) {
	g := newTestGateway(t)
	caller := `{"error":{"message":"This model's maximum context length is 8192 tokens: my secret prompt","type":"invalid_request_error","param":"messages","code":"context_length_exceeded"}}`
	g.backend.SetReply(fakebackend.Reply{Status: http.StatusBadRequest, Body: caller})
	if w := post(t, g, "client-error", `{"model":"open","messages":[]}`); w.Code != http.StatusBadRequest || w.Body.String() != caller {
		t.Fatalf("400 answer %d %q, want the backend's as it came", w.Code, w.Body.String())
	}
	expectFields(t, logFields(t, g, "client-error"), map[string]any{"http.response.status_code": float64(400),
		"error.type": "upstream_client_error", "kaiak.upstream.error.code": "context_length_exceeded",
		"kaiak.upstream.error.type": "invalid_request_error"})
	if strings.Contains(logLine(t, g, "client-error"), "secret prompt") {
		t.Error("the backend's message reached the log")
	}

	// A 429 of the model's only deployment: no deployment left to retry on, relayed.
	g.backend.SetReply(fakebackend.Reply{Status: http.StatusTooManyRequests,
		Body: `{"error":{"message":"slow down","type":"rate_limit_error","code":"rate_limit_exceeded"}}`})
	if w := post(t, g, "rate-limited", `{"model":"open","messages":[]}`); w.Code != http.StatusTooManyRequests {
		t.Fatalf("429: status %d", w.Code)
	}
	expectFields(t, logFields(t, g, "rate-limited"), map[string]any{"error.type": "upstream_rate_limited",
		"kaiak.upstream.error.code": "rate_limit_exceeded", "kaiak.upstream.error.type": "rate_limit_error",
		"kaiak.retry_refused": "no_deployment_left"})

	g.backend.SetReply(fakebackend.Reply{})
	if w := post(t, g, "ok", `{"model":"open","messages":[]}`); w.Code != http.StatusOK {
		t.Fatalf("ok: status %d", w.Code)
	}
	expectFields(t, logFields(t, g, "ok"), nil, "error.type", "kaiak.upstream.error.code", "kaiak.upstream.error.type")
	expectMetricLines(t, g.metricsText(), `kaiak_errors_total{class="upstream_client_error"} 1`,
		`kaiak_errors_total{class="upstream_rate_limited"} 1`)
}

// D6: time to first token is the answering attempt's — from its send, not from the
// request's arrival: a first attempt that timed out before its first event does not
// count against the backend that then answered. The log line carries the same
// measure and whether the request streamed.
func TestTimeToFirstTokenIsTheAnsweringAttempts(t *testing.T) {
	g := newTestGateway(t)
	s := testSnapshot(t, g.backend.URL())
	const firstEventTimeout = 300 * time.Millisecond
	s.Backends["local"].FirstEventTimeout = firstEventTimeout
	s.Backends["local-b"].FirstEventTimeout = firstEventTimeout
	g.holder.Swap(s)
	g.backend.QueueReplies(fakebackend.Reply{Before: fakebackend.StallFirstByte})
	if w := post(t, g, "ttft", `{"model":"pair","stream":true,"messages":[]}`); w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	fields := logFields(t, g, "ttft")
	expectFields(t, fields, map[string]any{"gen_ai.request.stream": true, "kaiak.attempts": float64(2)})
	ttft, ok := fields["kaiak.time_to_first_token"].(float64)
	if !ok || ttft >= 0.25 {
		t.Errorf("kaiak.time_to_first_token %v, want the answering attempt's (under 0.25 s)", fields["kaiak.time_to_first_token"])
	}
	if duration := fields["kaiak.request.duration"].(float64); duration < firstEventTimeout.Seconds() {
		t.Errorf("kaiak.request.duration %v, want it from arrival (past the first attempt's timeout)", duration)
	}
	text := g.metricsText()
	for _, backend := range []string{"local", "local-b"} {
		if strings.Contains(text, `kaiak_time_to_first_token_seconds_count{model="pair",backend="`+backend+`"} 1`+"\n") &&
			!strings.Contains(text, `kaiak_time_to_first_token_seconds_bucket{model="pair",backend="`+backend+`",le="0.25"} 1`+"\n") {
			t.Errorf("time to first token on %s measured from arrival:\n%s", backend, text)
		}
	}
}

// The independent review's finding 5: an attempt's outcome and duration are
// published as it ends, and a retry as it is sent — a failed first attempt is
// visible while the retry still streams, as is the retry's time to first token —
// and each exactly once, whether the request then completes or its client leaves.
func TestAttemptMetricsPublishedAsAttemptsEnd(t *testing.T) {
	for _, end := range []string{"completes", "client leaves"} {
		t.Run(end, func(t *testing.T) {
			g := newTestGateway(t)
			pace := make(chan struct{})
			// A 500 on pair's first deployment (its first turn), then a stream on the
			// other that sends its first event and waits for pace.
			g.backend.QueueReplies(fakebackend.Reply{Status: http.StatusInternalServerError},
				fakebackend.Reply{Pace: pace})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			r := httptest.NewRequestWithContext(ctx, "POST", "/v1/chat/completions",
				strings.NewReader(`{"model":"pair","stream":true,"messages":[]}`))
			r.Header.Set("Authorization", "Bearer "+workloadKey)
			done := make(chan struct{})
			go func() {
				defer close(done)
				g.h.ServeHTTP(httptest.NewRecorder(), r)
			}()

			failed := `kaiak_upstream_attempts_total{backend="local",deployment_model="pair-a",outcome="server_error"} 1`
			whileStreaming := []string{failed,
				`kaiak_upstream_attempt_duration_seconds_count{backend="local"} 1`,
				`kaiak_retries_total{model="pair",backend="local",reason="server_error"} 1`,
				`kaiak_time_to_first_token_seconds_count{model="pair",backend="local-b"} 1`}
			deadline := time.Now().Add(2 * time.Second)
			for {
				text := g.metricsText()
				all := true
				for _, want := range whileStreaming {
					all = all && strings.Contains(text, want+"\n")
				}
				if all {
					break
				}
				if time.Now().After(deadline) {
					expectMetricLines(t, text, whileStreaming...)
					t.Fatal("the failed first attempt is not visible while the retry streams")
				}
				time.Sleep(10 * time.Millisecond)
			}
			expectMetricLines(t, g.metricsText(),
				`kaiak_upstream_attempts_total{backend="local-b",deployment_model="pair-b",outcome="success"} 0`,
				`kaiak_upstream_attempt_duration_seconds_count{backend="local-b"} 0`)

			if end == "completes" {
				close(pace)
			} else {
				cancel()
			}
			<-done
			expectMetricLines(t, g.metricsText(), failed,
				`kaiak_upstream_attempts_total{backend="local-b",deployment_model="pair-b",outcome="success"} 1`,
				`kaiak_upstream_attempt_duration_seconds_count{backend="local"} 1`,
				`kaiak_upstream_attempt_duration_seconds_count{backend="local-b"} 1`,
				`kaiak_retries_total{model="pair",backend="local",reason="server_error"} 1`,
				`kaiak_time_to_first_token_seconds_count{model="pair",backend="local-b"} 1`,
				`kaiak_request_attempts_count{model="pair"} 1`)
		})
	}
}
