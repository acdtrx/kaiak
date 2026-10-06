package server

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"kaiak/internal/config"
	"kaiak/internal/fakebackend"
	"kaiak/internal/limits"
)

// withLimits swaps in the test config with limits on the top-level group research
// and its child eval, the workload key's group (JSON arrays, "" = none).
func withLimits(t *testing.T, g *testGateway, team, workload string) {
	t.Helper()
	g.holder.Swap(testSnapshotWith(t, g.backend.URL(), func(doc string) string {
		if team != "" {
			doc = strings.Replace(doc, `"research": {}`, `"research": { "limits": `+team+` }`, 1)
		}
		if workload != "" {
			doc = strings.Replace(doc, `"allowed_models": ["*"] }`, `"allowed_models": ["*"], "limits": `+workload+` }`, 1)
		}
		return doc
	}))
}

// counterUsed is the count of group's ("" = global) counter of type typ.
func counterUsed(t *testing.T, g *testGateway, group string, typ config.LimitType) int64 {
	t.Helper()
	for _, u := range g.limiter.Usage() {
		if u.Group == group && u.Type == typ {
			return u.Used
		}
	}
	t.Fatalf("no %s counter for group %q", typ, group)
	return 0
}

func TestLimitRefusalIs429WithRateLimitHeaders(t *testing.T) {
	g := newTestGateway(t)
	withLimits(t, g, "", `[{ "type": "requests_per_minute", "value": 1 }, { "type": "tokens_per_minute", "value": 100000 }]`)
	chat := call{method: "POST", path: "/v1/chat/completions", key: workloadKey, body: chatBody}

	w := do(t, g.h, chat)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	// Admitted responses carry the headers too; "open" has no output limit, so the
	// reservation is the input estimate alone.
	estimate := (int64(len(chatBody)) + 3) / 4
	for name, want := range map[string]string{
		"x-ratelimit-limit-requests":     "1",
		"x-ratelimit-remaining-requests": "0",
		"x-ratelimit-reset-requests":     "1m0s",
		"x-ratelimit-limit-tokens":       "100000",
		"x-ratelimit-remaining-tokens":   strconv.FormatInt(100000-estimate, 10),
	} {
		if got := w.Header().Get(name); got != want {
			t.Errorf("admitted: %s = %q, want %q", name, got, want)
		}
	}

	w = do(t, g.h, chat)
	expectError(t, w, http.StatusTooManyRequests, "rate_limit_exceeded")
	if typ, _, _ := openAIError(t, w); typ != "requests" {
		t.Errorf("type %q, want requests", typ)
	}
	retry, err := strconv.Atoi(w.Header().Get("Retry-After"))
	if err != nil || retry < 1 || retry > 60 {
		t.Errorf("Retry-After %q, want 1–60 seconds", w.Header().Get("Retry-After"))
	}
	if w.Header().Get("x-ratelimit-remaining-requests") != "0" || w.Header().Get("x-ratelimit-limit-tokens") != "100000" {
		t.Errorf("refusal headers %v", w.Header())
	}
	body := w.Body.String()
	if !strings.Contains(body, "group limit") || strings.Contains(body, "eval") || strings.Contains(body, "research") {
		t.Errorf("message must name the scope kind and no IDs: %s", body)
	}
	// Refused before routing: no usage record, and the backend never saw it.
	if n := len(g.usage.all()); n != 1 {
		t.Errorf("%d usage records, want 1", n)
	}
	if n := len(g.backend.Requests()); n != 1 {
		t.Errorf("backend got %d requests, want 1", n)
	}
	if !strings.Contains(g.logText(), `"error.type":"rate_limit_exceeded"`) {
		t.Errorf("refusal not logged:\n%s", g.logText())
	}
}

func TestReservationSettlesToActualUsage(t *testing.T) {
	g := newTestGateway(t)
	withLimits(t, g, `[{ "type": "tokens_per_minute", "value": 100000 }, { "type": "requests_per_minute", "value": 100 }]`, "")
	g.backend.SetReply(fakebackend.Reply{Usage: &fakebackend.Usage{PromptTokens: 100, CachedTokens: 40, CompletionTokens: 10}})

	// "pair" has an output limit (default 256): the reservation is estimate + 256.
	w := do(t, g.h, call{method: "POST", path: "/v1/chat/completions", key: workloadKey, body: `{"model":"pair","messages":[]}`})
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("x-ratelimit-remaining-tokens"); got != strconv.Itoa(100000-8-256) {
		t.Errorf("remaining tokens at admission %q, want the reservation of 8 + 256 taken", got)
	}
	// Settled: 60 uncached input + 10 output; the 40 read from the cache do not count.
	if got := counterUsed(t, g, "research", config.LimitTokensPerMinute); got != 70 {
		t.Errorf("team tokens %d after settlement, want 70", got)
	}

	// An upstream failure settles with zero units: its reservation is released, and
	// the request still counts.
	w = do(t, g.h, call{method: "POST", path: "/v1/chat/completions", key: workloadKey, body: `{"model":"down"}`})
	expectError(t, w, http.StatusBadGateway, "upstream_unavailable")
	if got := counterUsed(t, g, "research", config.LimitTokensPerMinute); got != 70 {
		t.Errorf("team tokens %d after an upstream failure, want 70", got)
	}
	if got := counterUsed(t, g, "research", config.LimitRequestsPerMinute); got != 2 {
		t.Errorf("team requests %d, want 2", got)
	}
}

func TestExhaustedBudgetIs429BudgetExceeded(t *testing.T) {
	g := newTestGateway(t)
	withLimits(t, g, "", `[{ "type": "usd_per_month", "value": 0.0001 }]`)
	g.backend.SetReply(fakebackend.Reply{Usage: &fakebackend.Usage{PromptTokens: 100, CompletionTokens: 10}})
	// "pair" costs 1 USD/M input and 2 USD/M output: 100 + 20 = 120 micro-USD, above
	// the 100 micro-USD budget — the one request allowed to overshoot it.
	chat := call{method: "POST", path: "/v1/chat/completions", key: workloadKey, body: `{"model":"pair","messages":[]}`}
	if w := do(t, g.h, chat); w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	w := do(t, g.h, chat)
	expectError(t, w, http.StatusTooManyRequests, "budget_exceeded")
	if typ, _, _ := openAIError(t, w); typ != "budget" {
		t.Errorf("type %q, want budget", typ)
	}
	if retry, err := strconv.Atoi(w.Header().Get("Retry-After")); err != nil || retry < 1 || retry > 31*24*3600 {
		t.Errorf("Retry-After %q, want seconds to the month's end", w.Header().Get("Retry-After"))
	}
	// The limit names no models, but "open" has no price: it costs nothing, so the
	// spent budget does not refuse it (D6).
	if w := do(t, g.h, call{method: "POST", path: "/v1/chat/completions", key: workloadKey, body: chatBody}); w.Code != http.StatusOK {
		t.Errorf("unpriced model: status %d: %s", w.Code, w.Body.String())
	}
}

// D6 over HTTP: a global USD budget covers every model, yet in an outage only the
// priced model is refused; the free one keeps serving.
func TestOutageRefusesOnlyPricedModels(t *testing.T) {
	lost := time.Now().Add(-time.Hour)
	g := newTestGatewayWith(t, func(h *config.Holder) *limits.Limiter {
		return limits.NewShared(h, time.Now, func() limits.Contact { return limits.Contact{Last: lost} }, nil)
	})
	g.holder.Swap(testSnapshotWith(t, g.backend.URL(), func(doc string) string {
		return strings.Replace(doc, `"global": { `, `"global": { "limits": [{ "type": "usd_per_month", "value": 100 }], `, 1)
	}))
	w := do(t, g.h, call{method: "POST", path: "/v1/chat/completions", key: workloadKey, body: `{"model":"pair","messages":[]}`})
	expectError(t, w, http.StatusServiceUnavailable, "budget_unavailable")
	if w := do(t, g.h, call{method: "POST", path: "/v1/chat/completions", key: workloadKey, body: chatBody}); w.Code != http.StatusOK {
		t.Errorf("unpriced model: status %d: %s", w.Code, w.Body.String())
	}
}

// In control-plane mode, past the outage grace, a model covered by a USD limit fails
// closed with 503 budget_unavailable; other models keep serving.
func TestOutageRefusesMoneyLimitedModels(t *testing.T) {
	lost := time.Now().Add(-time.Hour)
	g := newTestGatewayWith(t, func(h *config.Holder) *limits.Limiter {
		return limits.NewShared(h, time.Now, func() limits.Contact { return limits.Contact{Last: lost} }, nil)
	})
	withLimits(t, g, `[{ "type": "usd_per_month", "value": 100 }]`, "")
	w := do(t, g.h, call{method: "POST", path: "/v1/chat/completions", key: workloadKey, body: `{"model":"pair","messages":[]}`})
	expectError(t, w, http.StatusServiceUnavailable, "budget_unavailable")
	if typ, _, _ := openAIError(t, w); typ != "server_error" {
		t.Errorf("type %q, want server_error", typ)
	}
	if w.Header().Get("Retry-After") != "" {
		t.Errorf("Retry-After %q, want none", w.Header().Get("Retry-After"))
	}
	if w := do(t, g.h, call{method: "POST", path: "/v1/chat/completions", key: workloadKey, body: chatBody}); w.Code != http.StatusOK {
		t.Errorf("model without a USD limit: status %d: %s", w.Code, w.Body.String())
	}
	expectMetricLines(t, g.metricsText(), `kaiak_errors_total{class="budget_unavailable"} 1`)
}

func TestModelEndpointsAreNotLimited(t *testing.T) {
	g := newTestGateway(t)
	withLimits(t, g, `[{ "type": "requests_per_minute", "value": 0 }]`, "")
	expectError(t, do(t, g.h, call{method: "POST", path: "/v1/chat/completions", key: workloadKey, body: chatBody}),
		http.StatusTooManyRequests, "rate_limit_exceeded")
	for _, path := range []string{"/v1/models", "/v1/models/open", "/v1/models/open/props"} {
		w := do(t, g.h, call{method: "GET", path: path, key: workloadKey})
		if w.Code != http.StatusOK || w.Header().Get("x-ratelimit-limit-requests") != "" {
			t.Errorf("%s: status %d, headers %v", path, w.Code, w.Header())
		}
	}
}

// [B]'s probe: a backend's output limit is per generated sequence, so n, best_of and
// a completion's prompt list multiply what one request may generate. The reservation
// counts the output limit once per sequence; n and best_of above global.max_n are
// refused.
func TestOutputMultiplicityMultipliesTheReservation(t *testing.T) {
	const tpm = 1_000_000
	for _, c := range []struct {
		name, path, body, maxN string
		// sequences is how many output limits of 100 the reservation holds; 0 = refused.
		sequences int64
		param     string
		// input is the input estimate; 0 = the body's bytes ÷ 4. A token ID counts
		// one token, its bytes left out of the text (D1).
		input int64
	}{
		{name: "chat, one sequence", path: "/v1/chat/completions", body: `{"model":"pair","messages":[],"max_tokens":100}`, sequences: 1},
		{name: "chat n", path: "/v1/chat/completions", body: `{"model":"pair","messages":[],"max_tokens":100,"n":4}`, sequences: 4},
		{name: "chat n null", path: "/v1/chat/completions", body: `{"model":"pair","messages":[],"max_tokens":100,"n":null}`, sequences: 1},
		{name: "completions prompt list and n", path: "/v1/completions", body: `{"model":"pair","prompt":["a","b","c"],"max_tokens":100,"n":2}`, sequences: 6},
		{name: "completions token IDs are one prompt", path: "/v1/completions", body: `{"model":"pair","prompt":[1,2,3],"max_tokens":100}`, sequences: 1, input: (50-5+3)/4 + 3},
		{name: "completions token-ID prompts and best_of", path: "/v1/completions", body: `{"model":"pair","prompt":[[1],[2]],"max_tokens":100,"best_of":3,"n":1}`, sequences: 6, input: (70-2+3)/4 + 2},
		{name: "n above max_n", path: "/v1/chat/completions", body: `{"model":"pair","messages":[],"max_tokens":100,"n":64}`, param: "n"},
		{name: "best_of above max_n", path: "/v1/completions", body: `{"model":"pair","prompt":"a","max_tokens":100,"best_of":9}`, param: "best_of"},
		{name: "chat best_of", path: "/v1/chat/completions", body: `{"model":"pair","messages":[],"max_tokens":100,"best_of":3,"n":2}`, sequences: 3},
		{name: "chat best_of above max_n", path: "/v1/chat/completions", body: `{"model":"pair","messages":[],"max_tokens":100,"best_of":9}`, param: "best_of"},
		{name: "chat best_of not an integer", path: "/v1/chat/completions", body: `{"model":"pair","messages":[],"best_of":"3"}`, param: "best_of"},
		{name: "n at a raised max_n", path: "/v1/chat/completions", body: `{"model":"pair","messages":[],"max_tokens":100,"n":64}`, maxN: "64", sequences: 64},
	} {
		t.Run(c.name, func(t *testing.T) {
			g := newTestGateway(t)
			g.holder.Swap(testSnapshotWith(t, g.backend.URL(), func(doc string) string {
				doc = strings.Replace(doc, `"research": {}`, `"research": { "limits": [{ "type": "tokens_per_minute", "value": 1000000 }] }`, 1)
				if c.maxN != "" {
					doc = strings.Replace(doc, `"max_request_body_bytes": 1024 }`, `"max_request_body_bytes": 1024, "max_n": `+c.maxN+
						`, "max_sequences_per_request": `+c.maxN+` }`, 1)
				}
				return doc
			}))
			w := do(t, g.h, call{method: "POST", path: c.path, key: workloadKey, body: c.body})
			if c.sequences == 0 {
				code := "n_too_large"
				if strings.Contains(c.name, "not an integer") {
					code = "invalid_type"
				}
				expectError(t, w, http.StatusBadRequest, code)
				if typ, _, param := openAIError(t, w); typ != "invalid_request_error" || param == nil || *param != c.param {
					t.Errorf("type %q, param %v, want invalid_request_error on %s", typ, param, c.param)
				}
				if n := len(g.backend.Requests()); n != 0 {
					t.Errorf("backend got %d requests, want none", n)
				}
				return
			}
			if w.Code != http.StatusOK {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			}
			estimate := (int64(len(c.body)) + 3) / 4
			if c.input != 0 {
				estimate = c.input
			}
			want := strconv.FormatInt(tpm-estimate-100*c.sequences, 10)
			if got := w.Header().Get("x-ratelimit-remaining-tokens"); got != want {
				t.Errorf("remaining tokens at admission %s, want %s (%d sequences of 100)", got, want, c.sequences)
			}
		})
	}
}

// Sequences times the output limit can pass int64 (both at most 2^53 − 1 by the
// config's bounds): the reservation saturates and is capped at 2^53 − 1, so the request is too large for any token limit instead of
// wrapping around to a small one.
func TestOutputMultiplicityReservationSaturates(t *testing.T) {
	g := newTestGateway(t)
	g.holder.Swap(testSnapshotWith(t, g.backend.URL(), func(doc string) string {
		doc = strings.Replace(doc, `"research": {}`, `"research": { "limits": [{ "type": "tokens_per_minute", "value": 1000000 }] }`, 1)
		return strings.Replace(doc, `"max_request_body_bytes": 1024 }`, `"max_request_body_bytes": 1024, "max_n": 9007199254740991, "max_sequences_per_request": 9007199254740991 }`, 1)
	}))
	w := do(t, g.h, call{method: "POST", path: "/v1/completions", key: workloadKey,
		body: `{"model":"pair","prompt":"a","max_tokens":1024,"n":9007199254740991}`})
	expectError(t, w, http.StatusTooManyRequests, "rate_limit_exceeded")
	if !strings.Contains(w.Body.String(), "Request too large: it needs 9007199254740991 tokens") {
		t.Errorf("want a saturated reservation: %s", w.Body.String())
	}
}

// A request above this gateway's per-minute share but within the full limit waits for
// room; only one above the full limit is too large (M7).
func TestTooLargeMeansAboveTheFullLimit(t *testing.T) {
	share := errLimited(&limits.Rejection{Scope: limits.ScopeGroup, Group: "research", Type: config.LimitTokensPerMinute,
		Measure: limits.MeasureTokens, Limit: 15000, Max: 60000, Used: 16484, Requested: 16484, RetryAfter: 30 * time.Second})
	if strings.Contains(share.message, "too large") || !strings.Contains(share.message, "limit of 15000 tokens per minute") {
		t.Errorf("above the share: %q", share.message)
	}
	full := errLimited(&limits.Rejection{Scope: limits.ScopeGroup, Group: "research", Type: config.LimitTokensPerMinute,
		Measure: limits.MeasureTokens, Limit: 15000, Max: 60000, Requested: 60001})
	if !strings.Contains(full.message, "Request too large") || !strings.Contains(full.message, "limit is 60000 tokens") {
		t.Errorf("above the full limit: %q", full.message)
	}
}

// E9: an output limit the client sets above the model's context length can never be
// honored, so it is refused up front (400 invalid_value, OpenAI's answer to the same
// mistake) — on each chat key and on completions. A value within the context but
// above the ceiling is still lowered to the ceiling. Embeddings have no output limit.
func TestOutputLimitAboveTheContextIsRefused(t *testing.T) {
	for _, c := range []struct {
		name, path, body string
		// param is the refused field; "" = admitted, with sent as the backend's
		// output-limit values (key → value, "" = absent).
		param string
		sent  map[string]string
	}{
		{name: "chat max_tokens", path: "/v1/chat/completions", body: `{"model":"open","messages":[],"max_tokens":8193}`, param: "max_tokens"},
		{name: "chat max_completion_tokens", path: "/v1/chat/completions", body: `{"model":"pair","messages":[],"max_completion_tokens":32769}`, param: "max_completion_tokens"},
		{name: "chat, the second key too large", path: "/v1/chat/completions", body: `{"model":"pair","messages":[],"max_completion_tokens":100,"max_tokens":40000}`, param: "max_tokens"},
		{name: "completions int64 maximum", path: "/v1/completions", body: `{"model":"open","prompt":"a","max_tokens":9223372036854775807}`, param: "max_tokens"},
		{name: "chat at the context length", path: "/v1/chat/completions", body: `{"model":"open","messages":[],"max_tokens":8192}`,
			sent: map[string]string{"max_tokens": "8192"}},
		{name: "chat above the ceiling, within the context", path: "/v1/chat/completions", body: `{"model":"pair","messages":[],"max_completion_tokens":2000}`,
			sent: map[string]string{"max_completion_tokens": "1024"}},
		{name: "completions max_completion_tokens is not owned", path: "/v1/completions", body: `{"model":"open","prompt":"a","max_completion_tokens":99999}`,
			sent: map[string]string{"max_completion_tokens": "99999"}},
		{name: "embeddings", path: "/v1/embeddings", body: `{"model":"open","input":"a","max_tokens":99999}`,
			sent: map[string]string{"max_tokens": "99999"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			g := newTestGateway(t)
			w := do(t, g.h, call{method: "POST", path: c.path, key: workloadKey, body: c.body})
			if c.param != "" {
				expectError(t, w, http.StatusBadRequest, "invalid_value")
				if typ, _, param := openAIError(t, w); typ != "invalid_request_error" || param == nil || *param != c.param {
					t.Errorf("type %q, param %v, want invalid_request_error on %s", typ, param, c.param)
				}
				if !strings.Contains(w.Body.String(), c.param+" is too large") {
					t.Errorf("message: %s", w.Body.String())
				}
				if n := len(g.backend.Requests()); n != 0 {
					t.Errorf("backend got %d requests, want none", n)
				}
				expectMetricLines(t, g.metricsText(), `kaiak_errors_total{class="invalid_request"} 1`)
				return
			}
			if w.Code != http.StatusOK {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			}
			reqs := g.backend.Requests()
			if len(reqs) != 1 {
				t.Fatalf("backend got %d requests", len(reqs))
			}
			var sent map[string]json.RawMessage
			if err := json.Unmarshal(reqs[0].Body, &sent); err != nil {
				t.Fatal(err)
			}
			for key, want := range c.sent {
				if got := string(sent[key]); got != want {
					t.Errorf("backend got %s = %s, want %s", key, got, want)
				}
			}
		})
	}
}

// The follow-up audit's N-M1 reproduction over HTTP: team tokens_per_hour 1000 with
// 10 used, then max_tokens at the int64 maximum on a model with no output limit. A
// wrapped sum would admit it and leave the counter negative, admitting everything
// after it: it is refused, and the counter and the limit are untouched.
func TestHugeMaxTokensDoesNotDisableTokenLimits(t *testing.T) {
	g := newTestGateway(t)
	withLimits(t, g, `[{ "type": "tokens_per_hour", "value": 1000 }]`, "")
	g.backend.SetReply(fakebackend.Reply{Usage: &fakebackend.Usage{PromptTokens: 7, CompletionTokens: 3}})
	if w := do(t, g.h, call{method: "POST", path: "/v1/chat/completions", key: workloadKey, body: chatBody}); w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if got := counterUsed(t, g, "research", config.LimitTokensPerHour); got != 10 {
		t.Fatalf("team tokens %d, want 10", got)
	}
	w := do(t, g.h, call{method: "POST", path: "/v1/chat/completions", key: workloadKey,
		body: `{"model":"open","messages":[],"max_tokens":9223372036854775807}`})
	expectError(t, w, http.StatusBadRequest, "invalid_value")
	if got := counterUsed(t, g, "research", config.LimitTokensPerHour); got != 10 {
		t.Errorf("team tokens %d after the refusal, want 10", got)
	}
	// The limit still holds: a request needing more than the 990 left is refused.
	w = do(t, g.h, call{method: "POST", path: "/v1/chat/completions", key: workloadKey,
		body: `{"model":"open","messages":[],"max_tokens":1000}`})
	expectError(t, w, http.StatusTooManyRequests, "rate_limit_exceeded")
}

// A negative output limit is refused on every model, 400 invalid_value naming the
// key: llama-server reads -1 as unlimited, so on a model with no output_limit it
// would generate past the reservation. 0 and keys the endpoint does not own pass.
func TestNegativeOutputLimitIsRefused(t *testing.T) {
	for _, c := range []struct {
		name, path, body string
		param            string // "" = admitted
	}{
		{name: "chat max_tokens, no output_limit", path: "/v1/chat/completions", body: `{"model":"open","messages":[],"max_tokens":-1}`, param: "max_tokens"},
		{name: "chat max_completion_tokens, with output_limit", path: "/v1/chat/completions", body: `{"model":"pair","messages":[],"max_completion_tokens":-5}`, param: "max_completion_tokens"},
		{name: "chat, the second key negative", path: "/v1/chat/completions", body: `{"model":"pair","messages":[],"max_completion_tokens":100,"max_tokens":-1}`, param: "max_tokens"},
		{name: "completions max_tokens", path: "/v1/completions", body: `{"model":"open","prompt":"a","max_tokens":-1}`, param: "max_tokens"},
		{name: "zero passes", path: "/v1/chat/completions", body: `{"model":"open","messages":[],"max_tokens":0}`},
		{name: "completions max_completion_tokens is not owned", path: "/v1/completions", body: `{"model":"open","prompt":"a","max_completion_tokens":-1}`},
		{name: "embeddings", path: "/v1/embeddings", body: `{"model":"open","input":"a","max_tokens":-1}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			g := newTestGateway(t)
			w := do(t, g.h, call{method: "POST", path: c.path, key: workloadKey, body: c.body})
			if c.param == "" {
				if w.Code != http.StatusOK {
					t.Fatalf("status %d, want 200: %s", w.Code, w.Body.String())
				}
				return
			}
			expectError(t, w, http.StatusBadRequest, "invalid_value")
			if typ, _, param := openAIError(t, w); typ != "invalid_request_error" || param == nil || *param != c.param {
				t.Errorf("type %q, param %v, want invalid_request_error on %s", typ, param, c.param)
			}
			if !strings.Contains(w.Body.String(), "below minimum value") {
				t.Errorf("message: %s", w.Body.String())
			}
			if n := len(g.backend.Requests()); n != 0 {
				t.Errorf("backend got %d requests, want none", n)
			}
		})
	}
}

// The independent audit's finding 4: 10 000 prompts under max_n 8 were one request —
// one backend slot, 10 000 generation jobs. The sequences a request generates are
// capped by global.max_sequences_per_request, an embeddings request's inputs by
// global.max_embedding_inputs.
func TestBatchSizeIsCappedPerRequest(t *testing.T) {
	snapshot := &config.Snapshot{MaxN: 8, MaxSequencesPerRequest: config.DefaultMaxSequencesPerRequest,
		MaxEmbeddingInputs: config.DefaultMaxEmbeddingInputs}
	for _, c := range []struct {
		name  string
		ep    endpoint
		body  string
		param string // "" = accepted
	}{
		{"10 000 prompts", endpointCompletions, `{"model":"m","n":1,"max_tokens":1,"prompt":[` + strings.Repeat(`"x",`, 9999) + `"x"]}`, "prompt"},
		{"16 sequences", endpointCompletions, `{"model":"m","n":2,"prompt":["a","b","c","d","e","f","g","h"]}`, ""},
		{"17 prompts", endpointCompletions, `{"model":"m","prompt":[` + strings.Repeat(`"x",`, 16) + `"x"]}`, "prompt"},
		{"one prompt, n at max_n", endpointCompletions, `{"model":"m","n":8,"prompt":"a"}`, ""},
		{"token-ID prompts × best_of", endpointCompletions, `{"model":"m","best_of":6,"prompt":[[1],[2],[3]]}`, "prompt"},
		{"2048 embedding inputs", endpointEmbeddings, `{"model":"m","input":[` + strings.Repeat(`"x",`, 2047) + `"x"]}`, ""},
		{"2049 embedding inputs", endpointEmbeddings, `{"model":"m","input":[` + strings.Repeat(`"x",`, 2048) + `"x"]}`, "input"},
		{"2049 token-ID embedding inputs", endpointEmbeddings, `{"model":"m","input":[` + strings.Repeat(`[1],`, 2048) + `[1]]}`, "input"},
		{"one token-ID list is one input", endpointEmbeddings, `{"model":"m","input":[` + strings.Repeat(`1,`, 4000) + `1]}`, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			rq := &request{endpoint: c.ep, snapshot: snapshot, body: []byte(c.body)}
			apiErr := parseOwnedFields(rq)
			if c.param == "" {
				if apiErr != nil {
					t.Fatalf("refused: %+v", apiErr)
				}
				return
			}
			if apiErr == nil {
				t.Fatalf("accepted %d sequences", rq.inbound.Sequences)
			}
			if apiErr.status != http.StatusBadRequest || apiErr.errType != typeInvalidRequest ||
				apiErr.code != "invalid_value" || apiErr.param != c.param {
				t.Errorf("got %+v, want 400 invalid_value on %s", apiErr, c.param)
			}
		})
	}
}

// Every refusal body names the kind of scope that refused — "group" or "global" —
// and never the group's ID or its labels: a token limit spent, a request too large
// for a token limit, a group USD limit whose spend is unknown, and global limits.
func TestRefusalsNameTheScopeKindOnly(t *testing.T) {
	const label = "cc-4711"
	// swap puts groupLimits on research (labeled) and globalLimits on global.
	swap := func(t *testing.T, g *testGateway, groupLimits, globalLimits string) {
		t.Helper()
		g.holder.Swap(testSnapshotWith(t, g.backend.URL(), func(doc string) string {
			research := `"research": { "labels": { "cost_center": "` + label + `" }`
			if groupLimits != "" {
				research += `, "limits": ` + groupLimits
			}
			doc = strings.Replace(doc, `"research": {}`, research+` }`, 1)
			if globalLimits != "" {
				doc = strings.Replace(doc, `"global": { `, `"global": { "limits": `+globalLimits+`, `, 1)
			}
			return doc
		}))
	}
	lost := time.Now().Add(-time.Hour)
	outage := func(h *config.Holder) *limits.Limiter {
		return limits.NewShared(h, time.Now, func() limits.Contact { return limits.Contact{Last: lost} }, nil)
	}
	chat := call{method: "POST", path: "/v1/chat/completions", key: workloadKey, body: chatBody}
	priced := call{method: "POST", path: "/v1/chat/completions", key: workloadKey, body: `{"model":"pair","messages":[]}`}

	for _, tc := range []struct {
		name          string
		outage        bool
		group, global string
		// first is sent before the refused call, when set; want is in the refusal.
		first      *call
		refused    call
		status     int
		code, want string
	}{
		{name: "group tokens spent", group: `[{ "type": "tokens_per_minute", "value": 500 }]`, first: &chat, refused: chat,
			status: http.StatusTooManyRequests, code: "rate_limit_exceeded", want: "group limit of 500 tokens"},
		{name: "group request too large", group: `[{ "type": "tokens_per_minute", "value": 1 }]`, refused: chat,
			status: http.StatusTooManyRequests, code: "rate_limit_exceeded", want: "the group limit is 1 tokens"},
		{name: "group budget unavailable", outage: true, group: `[{ "type": "usd_per_month", "value": 100 }]`, refused: priced,
			status: http.StatusServiceUnavailable, code: "budget_unavailable", want: "a group USD limit"},
		{name: "global requests", global: `[{ "type": "requests_per_minute", "value": 1 }]`, first: &chat, refused: chat,
			status: http.StatusTooManyRequests, code: "rate_limit_exceeded", want: "global limit of 1 requests"},
		{name: "global request too large", global: `[{ "type": "tokens_per_minute", "value": 1 }]`, refused: chat,
			status: http.StatusTooManyRequests, code: "rate_limit_exceeded", want: "the global limit is 1 tokens"},
		{name: "global budget unavailable", outage: true, global: `[{ "type": "usd_per_month", "value": 100 }]`, refused: priced,
			status: http.StatusServiceUnavailable, code: "budget_unavailable", want: "a global USD limit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var g *testGateway
			if tc.outage {
				g = newTestGatewayWith(t, outage)
			} else {
				g = newTestGateway(t)
			}
			swap(t, g, tc.group, tc.global)
			// Settles above the 500-token limit, so the next request finds it spent.
			g.backend.SetReply(fakebackend.Reply{Usage: &fakebackend.Usage{PromptTokens: 1000, CompletionTokens: 10}})
			if tc.first != nil {
				if w := do(t, g.h, *tc.first); w.Code != http.StatusOK {
					t.Fatalf("first call: status %d: %s", w.Code, w.Body.String())
				}
			}
			w := do(t, g.h, tc.refused)
			expectError(t, w, tc.status, tc.code)
			body := w.Body.String()
			if !strings.Contains(body, tc.want) {
				t.Errorf("body %s, want %q", body, tc.want)
			}
			for _, secret := range []string{"research", "eval", label} {
				if strings.Contains(body, secret) {
					t.Errorf("body names %q: %s", secret, body)
				}
			}
		})
	}
}
