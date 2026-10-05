package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"kaiak/internal/accounting"
	"kaiak/internal/config"
	"kaiak/internal/fakebackend"
	"kaiak/internal/routing"
)

// newRetryGateway is the test gateway with a model "retry" of two deployments: model
// "first" on backend first, then model "second" on backend second. Backend "local-b"
// is a fake backend of its own, other, so deployments on "local" (g.backend) and
// "local-b" are scripted apart. The research team has request and token limits, so
// counts and reservations show. edit (nil = none) is applied to the config document
// last; the config is applied as cmd/kaiak applies it.
func newRetryGateway(t *testing.T, first, second string, edit func(string) string) (*testGateway, *fakebackend.Backend) {
	t.Helper()
	other := fakebackend.New()
	t.Cleanup(other.Close)
	g := newTestGateway(t)
	s := testSnapshotWith(t, g.backend.URL(), func(doc string) string {
		doc = regexp.MustCompile(`"local-b": \{[^}]*\}`).ReplaceAllLiteralString(doc,
			`"local-b": { "type": "openai-compatible", "base_url": "`+other.URL()+`/v1" }`)
		doc = strings.Replace(doc, `"pair": {`, `"retry": {
      "deployments": [{ "backend": "`+first+`", "model": "first" }, { "backend": "`+second+`", "model": "second" }],
      "metadata": { "context_length": 8192,
        "capabilities": { "streaming": true, "tools": false, "vision": false, "reasoning": false } } },
    "pair": {`, 1)
		doc = strings.Replace(doc, `"research": {}`, `"research": { "limits": [
      { "type": "requests_per_minute", "value": 1000 }, { "type": "tokens_per_minute", "value": 100000 } ] }`, 1)
		if edit != nil {
			doc = edit(doc)
		}
		return doc
	})
	g.holder.Swap(s)
	g.router.Configure(s)
	return g, other
}

// withGlobal adds settings to the config document's global section.
func withGlobal(settings string) func(string) string {
	return func(doc string) string {
		return strings.Replace(doc, `"max_request_body_bytes": 1024 }`, `"max_request_body_bytes": 1024, `+settings+` }`, 1)
	}
}

// post sends a chat completion for model with request ID id through the handler.
func post(t *testing.T, g *testGateway, id, body string) *httptest.ResponseRecorder {
	t.Helper()
	return do(t, g.h, call{method: "POST", path: "/v1/chat/completions", key: workloadKey, body: body,
		header: map[string]string{"X-Request-Id": id}})
}

// recordsOf returns the usage records of request id.
func recordsOf(g *testGateway, id string) []accounting.UsageRecord {
	var out []accounting.UsageRecord
	for _, r := range g.usage.all() {
		if r.RequestID == id {
			out = append(out, r)
		}
	}
	return out
}

func TestRetrySucceedsOnTheOtherDeployment(t *testing.T) {
	for _, c := range []struct {
		name  string
		first string
		// fail is g.backend's scripted answer to the first attempt (backends
		// "local" and "slow" point at it; "down" has nothing listening).
		fail    fakebackend.Reply
		outcome string
		reason  string
		// records: the request's usage records (a timed-out attempt has its own).
		records int
		// stream: the request streams (the first-event timeout applies to streams).
		stream bool
	}{
		{name: "connect refused", first: "down", outcome: "upstream_unavailable", reason: "unavailable", records: 1},
		{name: "backend 500", first: "local", fail: fakebackend.Reply{Status: 500}, outcome: "500", reason: "server_error", records: 1},
		{name: "backend 429", first: "local", fail: fakebackend.Reply{Status: 429}, outcome: "429", reason: "rate_limited", records: 1},
		// The independent audit's finding 3: an error answer broken before its body
		// is retried by its status — no record of its own (it was answered).
		{name: "backend 429 cut before its body", first: "local", fail: fakebackend.Reply{Status: 429, CutBeforeBody: true},
			outcome: "429", reason: "rate_limited", records: 1},
		{name: "backend 500 cut before its body", first: "local", fail: fakebackend.Reply{Status: 500, CutBeforeBody: true},
			outcome: "500", reason: "server_error", records: 1},
		{name: "backend 503 timed out after its headers", first: "slow", fail: fakebackend.Reply{Status: 503, StallBeforeBody: true},
			outcome: "503", reason: "server_error", records: 1},
		{name: "first-event timeout", first: "slow", fail: fakebackend.Reply{StallBeforeFirstByte: true},
			outcome: "upstream_timeout", reason: "timeout", records: 2, stream: true},
		{name: "credential refused", first: "local", fail: fakebackend.Reply{Status: 401}, outcome: "upstream_auth_failed",
			reason: "auth_failed", records: 1},
		{name: "wrong model on the host", first: "local", fail: fakebackend.Reply{Status: 404,
			Body: `{"object":"error","message":"The model ` + "`first`" + ` does not exist.","type":"NotFoundError","param":null,"code":404}`},
			outcome: "upstream_model_missing", reason: "model_missing", records: 1},
		{name: "no endpoint at the host's base_url", first: "local", fail: fakebackend.Reply{Status: 404, Body: "404 page not found\n"},
			outcome: "upstream_path_missing", reason: "path_missing", records: 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			g, other := newRetryGateway(t, c.first, "local-b", nil)
			g.backend.QueueReplies(c.fail)
			w := post(t, g, "r", `{"model":"retry","stream":`+strconv.FormatBool(c.stream)+`}`)
			if w.Code != http.StatusOK {
				t.Fatalf("status %d, want 200 from the second deployment: %s", w.Code, w.Body.String())
			}
			if len(other.Requests()) != 1 {
				t.Errorf("second deployment got %d requests, want 1", len(other.Requests()))
			}
			line := logLine(t, g, "r")
			want := `"kaiak.backend.id":"local-b","kaiak.backend.type":"openai-compatible","kaiak.deployment.model":"second","kaiak.attempts":2,"kaiak.tried":"` + c.first + `/first:` + c.outcome + `,local-b/second:200"`
			if !strings.Contains(line, want) {
				t.Errorf("log line misses %s:\n%s", want, line)
			}
			records := recordsOf(g, "r")
			if len(records) != c.records {
				t.Fatalf("%d records, want %d", len(records), c.records)
			}
			if answer := records[len(records)-1]; answer.Deployment.Backend != "local-b" || answer.Partial || answer.Estimated {
				t.Errorf("answer record %+v", answer)
			}
			if got := counterUsed(t, g, "research", config.LimitRequestsPerMinute); got != 1 {
				t.Errorf("requests counted %d, want 1", got)
			}
			if got, want := reservedTokens(t, g), settledTokens(g); got != want {
				t.Errorf("team tokens %d, want the settled %d", got, want)
			}
			expectMetricLines(t, scrape(g),
				`kaiak_retries_total{model="retry",backend="`+c.first+`",reason="`+c.reason+`"} 1`,
				`kaiak_request_attempts_bucket{model="retry",le="1"} 0`,
				`kaiak_request_attempts_bucket{model="retry",le="2"} 1`)
		})
	}
}

func TestTimedOutAttemptIsRecordedAndLimitsSettleTheSum(t *testing.T) {
	g, other := newRetryGateway(t, "slow", "local-b", nil)
	g.backend.QueueReplies(fakebackend.Reply{StallBeforeFirstByte: true})
	other.SetReply(fakebackend.Reply{Usage: &fakebackend.Usage{PromptTokens: 30, CompletionTokens: 12}})
	body := `{"model":"retry","stream":true,"messages":[{"role":"user","content":"hello there, how are you"}]}`
	if w := post(t, g, "r", body); w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}

	records := recordsOf(g, "r")
	if len(records) != 2 {
		t.Fatalf("%d records, want the timed-out attempt's and the answer's", len(records))
	}
	timedOut, answer := records[0], records[1]
	if timedOut.Deployment != (accounting.Deployment{Backend: "slow", Model: "first"}) || !timedOut.Estimated || !timedOut.Partial {
		t.Errorf("timed-out attempt's record %+v", timedOut)
	}
	expectUnits(t, timedOut, units(accounting.EstimateTokens(int64(len(body))), 0, 0, 0, 0), true, true)
	if answer.Deployment != (accounting.Deployment{Backend: "local-b", Model: "second"}) {
		t.Errorf("answer record %+v", answer)
	}
	expectUnits(t, answer, units(30, 0, 0, 12, 0), false, false)
	if timedOut.RecordID == answer.RecordID {
		t.Error("the two records share a record ID")
	}

	// Limits: one request; tokens settled to both records.
	if got := counterUsed(t, g, "research", config.LimitRequestsPerMinute); got != 1 {
		t.Errorf("requests counted %d, want 1", got)
	}
	want := accounting.EstimateTokens(int64(len(body))) + 30 + 12
	if got := reservedTokens(t, g); got != want {
		t.Errorf("team tokens %d, want %d (both records)", got, want)
	}
	// The log line carries the request's totals, the answer's flags; usage metrics
	// count each record, the ops metrics the one client request.
	line := logLine(t, g, "r")
	if !strings.Contains(line, `"gen_ai.usage.input_tokens":`+strconv.FormatInt(accounting.EstimateTokens(int64(len(body)))+30, 10)+`,`) ||
		!strings.Contains(line, `"gen_ai.usage.output_tokens":12,`) || !strings.Contains(line, `"kaiak.usage.estimated":false,"kaiak.usage.partial":false`) {
		t.Errorf("log line: %s", line)
	}
	expectMetricLines(t, scrape(g),
		`kaiak_usage_records_total{key_group="eval",root_group="research",key_id="k-eval",model="retry",status="partial"} 1`,
		`kaiak_usage_records_total{key_group="eval",root_group="research",key_id="k-eval",model="retry",status="complete"} 1`,
		`kaiak_request_duration_seconds_count{endpoint="chat_completions",model="retry",status_class="2xx"} 1`)
}

// Retries are failover only (D4): a single-deployment model answers the attempt's
// error at once; the client (its SDK) retries.
func TestSingleDeploymentIsNotRetried(t *testing.T) {
	g := newTestGateway(t)
	g.backend.QueueReplies(fakebackend.Reply{Status: 500})
	expectError(t, post(t, g, "r500", `{"model":"open"}`), http.StatusInternalServerError, "upstream_error")
	if n := len(g.backend.Requests()); n != 1 {
		t.Errorf("backend got %d requests, want 1", n)
	}
	if line := logLine(t, g, "r500"); !strings.Contains(line, `"kaiak.attempts":1,"kaiak.retry_refused":"no_deployment_left"`) {
		t.Errorf("log line: %s", line)
	}

	// Connect refused: one attempt, its error answers.
	w := post(t, g, "down", `{"model":"down"}`)
	expectError(t, w, http.StatusBadGateway, "upstream_unavailable")
	if line := logLine(t, g, "down"); !strings.Contains(line, `"kaiak.attempts":1,"kaiak.retry_refused":"no_deployment_left"`) {
		t.Errorf("log line: %s", line)
	}
	if records := recordsOf(g, "down"); len(records) != 1 || !records[0].Partial {
		t.Errorf("records %+v, want one partial", records)
	}
	expectMetricLines(t, scrape(g), `kaiak_retries_total{model="open",backend="local",reason="server_error"} 0`)
}

func TestAllAttemptsFailingAnswerTheLastError(t *testing.T) {
	g, other := newRetryGateway(t, "local", "local-b", nil)
	last := `{"error":{"message":"disk full on host 7","type":"server_error","param":null,"code":null}}`
	g.backend.QueueReplies(fakebackend.Reply{Status: 500})
	other.QueueReplies(fakebackend.Reply{Status: 503, Body: last, Header: map[string]string{"Retry-After": "9"}})
	w := post(t, g, "r", `{"model":"retry"}`)
	// The second attempt's 503 answers, its body replaced by the gateway's (a
	// backend fault's text is not the caller's); its error type is logged, never
	// its message. No deployment is left for a third attempt.
	expectError(t, w, 503, "upstream_error")
	if w.Header().Get("Retry-After") != "9" {
		t.Errorf("Retry-After %q, want the second attempt's", w.Header().Get("Retry-After"))
	}
	if line := logLine(t, g, "r"); !strings.Contains(line, `"kaiak.retry_refused":"no_deployment_left"`) ||
		!strings.Contains(line, `"kaiak.tried":"local/first:500,local-b/second:503"`) ||
		!strings.Contains(line, `"kaiak.upstream.error.type":"server_error"`) || strings.Contains(line, "disk full") {
		t.Errorf("log line: %s", line)
	}
	expectMetricLines(t, scrape(g), `kaiak_retries_total{model="retry",backend="local",reason="server_error"} 1`,
		`kaiak_errors_total{class="upstream_error"} 1`)
}

func TestNotRetried(t *testing.T) {
	t.Run("a single deployment that answered 429", func(t *testing.T) {
		g := newTestGateway(t)
		// Azure says how long to wait in milliseconds too; both are relayed.
		g.backend.QueueReplies(fakebackend.Reply{Status: 429, Header: map[string]string{"Retry-After": "7", "retry-after-ms": "6500"}})
		w := post(t, g, "r", `{"model":"open"}`)
		if w.Code != 429 || w.Header().Get("Retry-After") != "7" || w.Header().Get("Retry-After-Ms") != "6500" {
			t.Errorf("answer %d Retry-After %q retry-after-ms %q, want the backend's 429 relayed", w.Code,
				w.Header().Get("Retry-After"), w.Header().Get("Retry-After-Ms"))
		}
		if n := len(g.backend.Requests()); n != 1 {
			t.Errorf("backend got %d requests, want 1", n)
		}
		line := logLine(t, g, "r")
		if !strings.Contains(line, `"kaiak.attempts":1,"kaiak.retry_refused":"no_deployment_left"`) {
			t.Errorf("log line: %s", line)
		}
	})
	t.Run("a single backend that refused the credential", func(t *testing.T) {
		g := newTestGateway(t)
		g.backend.SetReply(fakebackend.Reply{Status: 403})
		w := post(t, g, "r", `{"model":"open"}`)
		expectError(t, w, http.StatusBadGateway, "upstream_auth_failed")
		if n := len(g.backend.Requests()); n != 1 {
			t.Errorf("backend got %d requests, want 1", n)
		}
	})
	t.Run("a single deployment whose host serves another model", func(t *testing.T) {
		g := newTestGateway(t)
		g.backend.SetReply(fakebackend.Reply{Status: 404,
			Body: `{"error":{"message":"The model ` + "`open`" + ` does not exist.","type":"NotFoundError","param":"model","code":404}}`})
		w := post(t, g, "r", `{"model":"open"}`)
		expectError(t, w, http.StatusBadGateway, "upstream_model_missing")
		if strings.Contains(w.Body.String(), "does not exist") {
			t.Errorf("the backend's answer reached the client: %s", w.Body.String())
		}
		if n := len(g.backend.Requests()); n != 1 {
			t.Errorf("backend got %d requests, want 1 (the deployment is refused for the request)", n)
		}
		expectMetricLines(t, scrape(g), `kaiak_errors_total{class="upstream_error"} 1`)
	})
	t.Run("a 404 that does not name the model", func(t *testing.T) {
		g := newTestGateway(t)
		body := `{"error":{"message":"LoRA adapter foo not found","type":"NotFoundError","param":null,"code":404}}`
		g.backend.QueueReplies(fakebackend.Reply{Status: 404, Body: body})
		if w := post(t, g, "r", `{"model":"open"}`); w.Code != 404 || w.Body.String() != body {
			t.Errorf("answer %d %s, want the 404 relayed", w.Code, w.Body.String())
		}
		if n := len(g.backend.Requests()); n != 1 {
			t.Errorf("backend got %d requests, want 1", n)
		}
	})
	t.Run("a caller's 400", func(t *testing.T) {
		g := newTestGateway(t)
		g.backend.QueueReplies(fakebackend.Reply{Status: 400})
		if w := post(t, g, "r", `{"model":"open"}`); w.Code != 400 {
			t.Errorf("status %d, want the 400 relayed", w.Code)
		}
		if n := len(g.backend.Requests()); n != 1 {
			t.Errorf("backend got %d requests, want 1", n)
		}
	})
	t.Run("after the first event", func(t *testing.T) {
		g := newTestGateway(t)
		g.backend.QueueReplies(fakebackend.Reply{CutAfter: 2})
		url := serveGateway(t, g)
		resp := streamRequest(t, context.Background(), url, `{"model":"open","stream":true}`)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status %d", resp.StatusCode)
		}
		if _, err := io.ReadAll(resp.Body); err == nil {
			t.Error("the stream ended cleanly, want it cut")
		}
		settledRecord(t, g)
		if n := len(g.backend.Requests()); n != 1 {
			t.Errorf("backend got %d requests, want 1", n)
		}
	})
}

func TestMaxAttempts(t *testing.T) {
	for _, c := range []struct {
		name string
		edit func(string) string
		want int
	}{
		{"global", withGlobal(`"retries": { "max_attempts": 2 }`), 2},
		{"model override", func(doc string) string {
			doc = withGlobal(`"retries": { "max_attempts": 2 }`)(doc)
			return strings.Replace(doc, `"down": {
      "deployments"`, `"down": { "retries": { "max_attempts": 4 },
      "deployments"`, 1)
		}, 4},
		{"no retries", withGlobal(`"retries": { "max_attempts": 1 }`), 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			g, _ := newRetryGateway(t, "local", "local-b", func(doc string) string {
				return c.edit(fiveDownDeployments(doc))
			})
			expectError(t, post(t, g, "r", `{"model":"down"}`), http.StatusBadGateway, "upstream_unavailable")
			if line := logLine(t, g, "r"); !strings.Contains(line, `"kaiak.attempts":`+strconv.Itoa(c.want)+`,`) {
				t.Errorf("log line: %s, want %d attempts", line, c.want)
			}
		})
	}
}

// fiveDownDeployments gives model "down" five deployments on backend down (models
// d1…d5), so retries have deployments to fail over to.
func fiveDownDeployments(doc string) string {
	return strings.Replace(doc, `[{ "backend": "down", "model": "down" }]`,
		`[{ "backend": "down", "model": "d1" }, { "backend": "down", "model": "d2" }, { "backend": "down", "model": "d3" },
        { "backend": "down", "model": "d4" }, { "backend": "down", "model": "d5" }]`, 1)
}

func TestEachAttemptFeedsTheCircuitBreaker(t *testing.T) {
	g, other := newRetryGateway(t, "local", "local-b",
		withGlobal(`"circuit": { "failure_threshold": 2, "probe_interval_ms": 3600000 }`))
	g.backend.SetReply(fakebackend.Reply{Status: 500})
	// Ties take turns: each request's first attempt fails on local, its retry
	// succeeds on local-b; the second failure opens local's circuit.
	for i := range 2 {
		if w := post(t, g, "r"+strconv.Itoa(i), `{"model":"retry"}`); w.Code != http.StatusOK {
			t.Fatalf("request %d: status %d", i, w.Code)
		}
		if open := circuitOpen(g, "local", "first"); open != (i == 1) {
			t.Fatalf("after request %d: circuit open %v", i, open)
		}
	}
	if post(t, g, "r2", `{"model":"retry"}`); !strings.Contains(logLine(t, g, "r2"), `"kaiak.attempts":1,`) {
		t.Errorf("with local open, want one attempt on local-b: %s", logLine(t, g, "r2"))
	}
	if n, m := len(g.backend.Requests()), len(other.Requests()); n != 2 || m != 3 {
		t.Errorf("requests local %d, local-b %d; want 2 and 3", n, m)
	}
}

// A retry never goes back to a deployment tried: with the other one open, the first
// attempt's error answers.
func TestNoRetryWhenTheOtherDeploymentIsOpen(t *testing.T) {
	g, other := newRetryGateway(t, "local", "local-b",
		withGlobal(`"circuit": { "failure_threshold": 2, "probe_interval_ms": 3600000 }`))
	m := g.holder.Current().Models["retry"]
	for range 2 {
		slot, _, err := g.router.Acquire(context.Background(), m,
			routing.Avoid{Refused: []routing.DeploymentID{{Backend: "local", Model: "first"}}})
		if err != nil {
			t.Fatal(err)
		}
		slot.Report(routing.Failure, "scripted")
		slot.Release()
	}
	if !circuitOpen(g, "local-b", "second") {
		t.Fatal("local-b not open")
	}
	g.backend.QueueReplies(fakebackend.Reply{Status: 500})
	expectError(t, post(t, g, "r", `{"model":"retry"}`), http.StatusInternalServerError, "upstream_error")
	if n := len(g.backend.Requests()); n != 1 || len(other.Requests()) != 0 {
		t.Errorf("local got %d, local-b %d; want one attempt on local", n, len(other.Requests()))
	}
	if line := logLine(t, g, "r"); !strings.Contains(line, `"kaiak.attempts":1,"kaiak.retry_refused":"no_deployment_left"`) {
		t.Errorf("log line: %s", line)
	}
}

func TestRetryQueuesForACappedBackend(t *testing.T) {
	// local (the retry's untried deployment) takes one request at a time; a stream
	// on model "open" holds its slot.
	// edit (nil = none) is applied after the cap.
	hold := func(t *testing.T, edit func(string) string) (*testGateway, *fakebackend.Backend, *httptest.Server, func()) {
		g, other := newRetryGateway(t, "local-b", "local", func(doc string) string {
			doc = capLocal(doc)
			if edit != nil {
				doc = edit(doc)
			}
			return doc
		})
		other.SetReply(fakebackend.Reply{Status: 500})
		g.backend.SetReply(fakebackend.Reply{HangAfter: 1})
		srv := httptest.NewServer(g.h)
		t.Cleanup(srv.Close)
		_, cancelHolder := holdStream(t, srv.URL, "open")
		g.backend.SetReply(fakebackend.Reply{})
		return g, other, srv, cancelHolder
	}

	t.Run("served once the slot frees", func(t *testing.T) {
		g, other, srv, cancelHolder := hold(t, nil)
		retried := send(context.Background(), srv.URL, "r", `{"model":"retry"}`)
		waitQueued(t, g, "retry", 1)
		if len(other.Requests()) != 1 {
			t.Fatalf("first attempt not sent")
		}
		cancelHolder()
		if a := await(t, retried); a.status != http.StatusOK {
			t.Fatalf("status %d: %s", a.status, a.body)
		}
		srv.Close()
		line := logLine(t, g, "r")
		if !strings.Contains(line, `"kaiak.attempts":2,"kaiak.tried":"local-b/first:500,local/second:200","kaiak.queue.wait_duration":`) {
			t.Errorf("log line: %s", line)
		}
		expectMetricLines(t, scrape(g), `kaiak_queue_wait_seconds_count{model="retry"} 1`)
		if n := g.router.InFlightByBackend(); len(n) != 0 {
			t.Errorf("in flight %v, want none", n)
		}
	})

	t.Run("client gone while the retry waits", func(t *testing.T) {
		g, other, srv, cancelHolder := hold(t, nil)
		defer cancelHolder()
		ctx, leave := context.WithCancel(context.Background())
		retried := send(ctx, srv.URL, "r", `{"model":"retry"}`)
		waitQueued(t, g, "retry", 1)
		leave()
		await(t, retried)
		waitQueued(t, g, "retry", 0)
		cancelHolder()
		srv.Close()
		line := logLine(t, g, "r")
		if !strings.Contains(line, `"http.response.status_code":499`) || !strings.Contains(line, `"error.type":"client_closed"`) ||
			!strings.Contains(line, `"kaiak.attempts":1,`) {
			t.Errorf("log line: %s", line)
		}
		if n := len(other.Requests()); n != 1 {
			t.Errorf("first deployment got %d requests, want 1", n)
		}
		if n := len(g.backend.Requests()); n != 1 {
			t.Errorf("capped backend got %d requests, want the holder's only", n)
		}
		if records := recordsOf(g, "r"); len(records) != 1 || records[0].Units[config.UnitTokensIn] != 0 {
			t.Errorf("records %+v, want the first attempt's, zero units", records)
		}
		if got, want := reservedTokens(t, g), settledTokens(g); got != want {
			t.Errorf("team tokens %d, want the settled %d", got, want)
		}
		if n := g.router.InFlightByBackend(); len(n) != 0 {
			t.Errorf("in flight %v, want none", n)
		}
	})

	t.Run("queue timeout answers the last attempt's error", func(t *testing.T) {
		g, _, srv, cancelHolder := hold(t, func(doc string) string {
			return strings.Replace(doc, `"retry": {`, `"retry": { "queue": { "timeout_ms": 30 },`, 1)
		})
		defer cancelHolder()
		a := await(t, send(context.Background(), srv.URL, "r", `{"model":"retry"}`))
		if a.status != 500 {
			t.Errorf("status %d, want the first attempt's 500 relayed", a.status)
		}
		cancelHolder()
		srv.Close()
		if line := logLine(t, g, "r"); !strings.Contains(line, `"kaiak.retry_refused":"queue_timeout"`) {
			t.Errorf("log line: %s", line)
		}
		expectMetricLines(t, scrape(g), `kaiak_queue_rejections_total{model="retry",reason="timeout"} 1`)
	})
}

// capLocal caps backend "local" at one request in flight.
func capLocal(doc string) string {
	return strings.Replace(doc, `"api_key_env": "LOCAL_KEY" }`, `"api_key_env": "LOCAL_KEY", "max_in_flight": 1 }`, 1)
}

func TestDrainCutsARetryWaitingInTheQueue(t *testing.T) {
	g, other := newRetryGateway(t, "local-b", "local", capLocal)
	other.SetReply(fakebackend.Reply{Status: 500})
	g.backend.SetReply(fakebackend.Reply{HangAfter: 1})
	d := newDrainable(t, g)
	_, cancelHolder := holdStream(t, d.url, "open")
	defer cancelHolder()
	g.backend.SetReply(fakebackend.Reply{})
	retried := send(context.Background(), d.url, "r", `{"model":"retry"}`)
	waitQueued(t, g, "retry", 1)

	g.drain.begin(DrainTimes{}, d.logger)
	g.drain.refuse(d.l, d.logger)
	g.drain.finish(d.l, 20*time.Millisecond, nil, d.logger)
	if a := await(t, retried); a.err == nil && a.status == http.StatusOK {
		t.Error("retry served after the cut")
	}
	line := logLine(t, g, "r")
	if !strings.Contains(line, `"error.type":"server_shutting_down"`) || !strings.Contains(line, `"kaiak.attempts":1,`) {
		t.Errorf("log line: %s", line)
	}
	if n := g.router.InFlightByBackend(); len(n) != 0 {
		t.Errorf("in flight %v, want none", n)
	}
}
