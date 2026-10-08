package server

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"kaiak/internal/fakebackend"
)

// Every attempt is counted per deployment by its outcome — the circuit's
// classification, named — and timed per backend to its end (a stream to the end of
// its relay); retries carry the backend of the attempt they followed.
func TestUpstreamAttemptMetrics(t *testing.T) {
	g := newTestGateway(t)

	// A stream whose events come 60 ms apart: its attempt lasts past 100 ms even
	// though the first event came at once.
	g.backend.SetReply(fakebackend.Reply{EventDelay: 60 * time.Millisecond})
	if w := post(t, g, "stream", `{"model":"open","stream":true}`); w.Code != http.StatusOK {
		t.Fatalf("stream: status %d", w.Code)
	}
	g.backend.SetReply(fakebackend.Reply{})
	// A 500 on pair's first deployment (its first turn), then a success on the
	// other.
	g.backend.QueueReplies(fakebackend.Reply{Status: http.StatusInternalServerError})
	if w := post(t, g, "retried", `{"model":"pair"}`); w.Code != http.StatusOK {
		t.Fatalf("retried: status %d", w.Code)
	}
	// The caller's 400, relayed: neutral.
	g.backend.QueueReplies(fakebackend.Reply{Status: http.StatusBadRequest})
	if w := post(t, g, "bad", `{"model":"open"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("bad: status %d", w.Code)
	}
	// A backend 429 from both of pair's deployments: neutral, retried on the other.
	g.backend.QueueReplies(fakebackend.Reply{Status: http.StatusTooManyRequests},
		fakebackend.Reply{Status: http.StatusTooManyRequests})
	post(t, g, "busy", `{"model":"pair"}`)
	// Nothing listening: unavailable; the only deployment, so not retried.
	expectError(t, post(t, g, "down", `{"model":"down"}`), http.StatusBadGateway, "upstream_unavailable")
	// A stream cut after its first event: broke off.
	// The relay cuts the client connection: net/http's abort sentinel, recovered here.
	g.backend.SetReply(fakebackend.Reply{Fault: &fakebackend.StreamFault{At: 1, Kind: fakebackend.Cut}})
	func() {
		defer func() {
			if r := recover(); r != http.ErrAbortHandler {
				t.Errorf("cut stream: recovered %v, want the abort sentinel", r)
			}
		}()
		post(t, g, "cut", `{"model":"open","stream":true}`)
	}()

	text := g.metricsText()
	expectMetricLines(t, text,
		`kaiak_upstream_attempts_total{kaiak_backend_id="local",kaiak_deployment_model="open",kaiak_attempt_outcome="success"} 1`,
		`kaiak_upstream_attempts_total{kaiak_backend_id="local",kaiak_deployment_model="pair-a",kaiak_attempt_outcome="server_error"} 1`,
		`kaiak_upstream_attempts_total{kaiak_backend_id="local-b",kaiak_deployment_model="pair-b",kaiak_attempt_outcome="success"} 1`,
		`kaiak_upstream_attempts_total{kaiak_backend_id="local",kaiak_deployment_model="open",kaiak_attempt_outcome="client_error"} 1`,
		`kaiak_upstream_attempts_total{kaiak_backend_id="local",kaiak_deployment_model="open",kaiak_attempt_outcome="broke_off"} 1`,
		`kaiak_upstream_attempts_total{kaiak_backend_id="local",kaiak_deployment_model="pair-a",kaiak_attempt_outcome="rate_limited"} 1`,
		`kaiak_upstream_attempts_total{kaiak_backend_id="local-b",kaiak_deployment_model="pair-b",kaiak_attempt_outcome="rate_limited"} 1`,
		`kaiak_upstream_attempts_total{kaiak_backend_id="down",kaiak_deployment_model="down",kaiak_attempt_outcome="unavailable"} 1`,
		`kaiak_upstream_attempt_duration_seconds_count{kaiak_backend_id="local"} 5`,
		`kaiak_upstream_attempt_duration_seconds_count{kaiak_backend_id="local-b"} 2`,
		`kaiak_upstream_attempt_duration_seconds_count{kaiak_backend_id="down"} 1`,
		`kaiak_retries_total{gen_ai_request_model="pair",kaiak_backend_id="local",kaiak_attempt_outcome="server_error"} 1`,
		`kaiak_retries_total{gen_ai_request_model="down",kaiak_backend_id="down",kaiak_attempt_outcome="unavailable"} 0`,
	)
	// pair's first attempt went to either deployment (ties take turns).
	if !strings.Contains(text, `kaiak_retries_total{gen_ai_request_model="pair",kaiak_backend_id="local",kaiak_attempt_outcome="rate_limited"} 1`+"\n") &&
		!strings.Contains(text, `kaiak_retries_total{gen_ai_request_model="pair",kaiak_backend_id="local-b",kaiak_attempt_outcome="rate_limited"} 1`+"\n") {
		t.Error("no retry of pair counted under the backend that answered 429")
	}
	// The stream's attempt lasted to the end of its relay, past 100 ms: at most the
	// four other attempts on local fall at or under 0.1 s.
	if !strings.Contains(text, `kaiak_upstream_attempt_duration_seconds_bucket{kaiak_backend_id="local",le="0.1"} 4`+"\n") {
		t.Errorf("the stream's attempt was not timed to the end of its relay:\n%s", text)
	}
}
