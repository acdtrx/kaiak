package server

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"kaiak/internal/fakebackend"
	"kaiak/internal/routing"
)

// newCircuitGateway is the test gateway with the circuit breaker opening after
// threshold failures and no retries (one attempt per request: each request is one
// outcome), applied as cmd/kaiak applies a config. Probes run only when a test
// invokes them (no RunProbers).
func newCircuitGateway(t *testing.T, threshold int, edit func(string) string) *testGateway {
	t.Helper()
	g := newTestGateway(t)
	g.apply(t, func(doc string) string {
		doc = replaceOnce(t, doc, `"max_request_body_bytes": 1024 }`, `"max_request_body_bytes": 1024,
    "retries": { "max_attempts": 1 },
    "circuit": { "failure_threshold": `+strconv.Itoa(threshold)+`, "probe_interval_ms": 3600000 } }`)
		if edit != nil {
			doc = edit(doc)
		}
		return doc
	})
	return g
}

func circuitOpen(g *testGateway, backend, model string) bool {
	_, ok := notClosed(g.router)[routing.DeploymentID{Backend: backend, Model: model}]
	return ok
}

// reportFailure counts one failure on model's only deployment, as a routed request
// would.
func reportFailure(t *testing.T, g *testGateway, model string) {
	t.Helper()
	slot, _, err := g.router.Acquire(context.Background(), g.holder.Current().Models[model], routing.Avoid{})
	if err != nil {
		t.Fatal(err)
	}
	slot.Report(routing.Failure, "scripted")
	slot.Release()
}

// TestOutcomeClassification drives one request of each kind through a gateway whose
// circuits open at the second consecutive failure, after one failure was counted:
// a failure opens the deployment's circuit; a success resets the count (one more
// failure leaves it closed); a neutral outcome leaves the count (one more failure
// opens it).
func TestOutcomeClassification(t *testing.T) {
	const (
		failure = "failure"
		neutral = "neutral"
		success = "success"
	)
	for _, c := range []struct {
		name  string
		model string
		reply fakebackend.Reply
		// stream: the request asks for a stream; leave: the client disconnects
		// after reading events events (0: before any answer).
		stream bool
		leave  bool
		events int
		class  string
	}{
		{name: "success", model: "open", class: success},
		{name: "stream success", model: "open", stream: true, class: success},
		{name: "backend 500", model: "open", reply: fakebackend.Reply{Status: 500}, class: failure},
		{name: "backend 503", model: "open", reply: fakebackend.Reply{Status: 503}, class: failure},
		{name: "backend 429", model: "open", reply: fakebackend.Reply{Status: 429}, class: neutral},
		{name: "backend 400", model: "open", reply: fakebackend.Reply{Status: 400}, class: neutral},
		{name: "backend 404", model: "open", reply: fakebackend.Reply{Status: 404}, class: neutral},
		// The host serves another model: the deployment is broken.
		{name: "backend 404 naming the deployment's model", model: "open", reply: fakebackend.Reply{Status: 404,
			Body: `{"error":{"message":"The model ` + "`open`" + ` does not exist.","type":"NotFoundError","param":"model","code":404}}`},
			class: failure},
		// The backend's base_url leads to no endpoint: the deployment is broken.
		{name: "backend 404 at no endpoint", model: "open", reply: fakebackend.Reply{Status: 404, Body: "404 page not found\n"},
			class: failure},
		{name: "credential refused (401)", model: "open",
			reply: fakebackend.Reply{RequireHeader: "X-Never-Sent", RequireValue: "x"}, class: failure},
		{name: "credential refused (403)", model: "open", reply: fakebackend.Reply{Status: 403}, class: failure},
		{name: "connect refused", model: "down", class: failure},
		{name: "first-event timeout", model: "slow", stream: true, reply: fakebackend.Reply{Before: fakebackend.StallFirstByte},
			class: failure},
		// A long non-stream generation: the backend was working.
		{name: "response timeout", model: "slow", reply: fakebackend.Reply{Before: fakebackend.StallFirstByte}, class: neutral},
		{name: "cut mid-stream", model: "open", stream: true, reply: fakebackend.Reply{Fault: &fakebackend.StreamFault{At: 2, Kind: fakebackend.Cut}}, class: failure},
		{name: "stalled mid-stream", model: "slow", stream: true, reply: fakebackend.Reply{Fault: &fakebackend.StreamFault{At: 2, Kind: fakebackend.Hang}}, class: failure},
		{name: "stream ended incomplete", model: "open", stream: true, reply: fakebackend.Reply{Fault: &fakebackend.StreamFault{At: 2, Kind: fakebackend.End}}, class: failure},
		{name: "JSON body ended incomplete", model: "open", reply: fakebackend.Reply{Body: `{"id":"x","choices":[`},
			class: failure},
		{name: "client gone before the answer", model: "open", reply: fakebackend.Reply{Before: fakebackend.StallFirstByte},
			leave: true, class: neutral},
		// The backend answered and was serving: it proved itself.
		{name: "client gone mid-stream", model: "open", stream: true, reply: fakebackend.Reply{Fault: &fakebackend.StreamFault{At: 2, Kind: fakebackend.Hang}},
			leave: true, events: 2, class: success},
	} {
		t.Run(c.name, func(t *testing.T) {
			g := newCircuitGateway(t, 2, nil)
			backend := g.snapshotBackend(c.model)
			reportFailure(t, g, c.model)
			g.backend.SetReply(c.reply)
			srv := httptest.NewServer(g.h)
			t.Cleanup(srv.Close)
			body := `{"model":"` + c.model + `","stream":` + strconv.FormatBool(c.stream) + `}`
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if c.leave {
				req, _ := http.NewRequestWithContext(ctx, "POST", srv.URL+"/v1/chat/completions", strings.NewReader(body))
				req.Header.Set("Authorization", "Bearer "+workloadKey)
				done := make(chan struct{})
				go func() {
					defer close(done)
					resp, err := http.DefaultClient.Do(req)
					if err != nil {
						return
					}
					defer resp.Body.Close()
					rd := bufio.NewReader(resp.Body)
					for range c.events {
						readEvent(t, rd)
					}
					cancel()
				}()
				if c.events == 0 {
					<-g.backend.Arrivals()
					cancel()
				}
				<-done
			} else {
				a := await(t, send(ctx, srv.URL, "r", body))
				if a.err != nil && c.class != failure {
					t.Fatal(a.err)
				}
			}
			srv.Close() // waits for the handler: its finishers have run
			open := circuitOpen(g, backend, c.model)
			if c.class == failure {
				if !open {
					t.Error("circuit closed, want open: the outcome is a failure")
				}
				return
			}
			if open {
				t.Fatalf("circuit open after a %s outcome", c.class)
			}
			reportFailure(t, g, c.model)
			if open := circuitOpen(g, backend, c.model); open != (c.class == neutral) {
				t.Errorf("after one more failure the circuit is open %v, want %v (a %s outcome)", open, c.class == neutral, c.class)
			}
		})
	}
}

// snapshotBackend is the backend ID of model's only deployment.
func (g *testGateway) snapshotBackend(model string) string {
	return g.holder.Current().Models[model].Deployments[0].Backend.ID
}

func TestCircuitOpensAfterTheThresholdAndAllOpenIs503(t *testing.T) {
	g := newCircuitGateway(t, 3, nil)
	for i := range 3 {
		w := do(t, g.h, call{method: "POST", path: "/v1/chat/completions", key: workloadKey, body: `{"model":"down"}`})
		expectError(t, w, http.StatusBadGateway, "upstream_unavailable")
		if open := circuitOpen(g, "down", "down"); open != (i == 2) {
			t.Fatalf("after failure %d: circuit open %v", i+1, open)
		}
	}
	w := do(t, g.h, call{method: "POST", path: "/v1/chat/completions", key: workloadKey, body: `{"model":"down"}`,
		header: map[string]string{"X-Request-Id": "refused"}})
	expectError(t, w, http.StatusServiceUnavailable, "no_healthy_deployment")
	if typ, _, _ := openAIError(t, w); typ != "server_error" {
		t.Errorf("type %q, want server_error", typ)
	}
	if strings.Contains(w.Body.String(), "127.0.0.1") {
		t.Errorf("the answer names the backend: %s", w.Body.String())
	}
	logs := g.logText()
	if !strings.Contains(logs, `"msg":"circuit opened","kaiak.backend.id":"down","kaiak.deployment.model":"down","kaiak.circuit.failures":3,"kaiak.circuit.last_error":"upstream_unavailable: backend down:`) {
		t.Errorf("no circuit-opened line with the last failure:\n%s", logs)
	}
	line := logLine(t, g, "refused")
	if strings.Contains(line, `"kaiak.backend.id"`) || !strings.Contains(line, `"error.type":"no_healthy_deployment"`) {
		t.Errorf("log line of the refused request: %s", line)
	}
	// Refused before routing: no usage record for it.
	if n := len(g.usage.all()); n != 3 {
		t.Errorf("%d usage records, want 3 (the refused request has none)", n)
	}
	metrics := scrape(g)
	for _, want := range []string{
		`kaiak_circuit_open{backend="down",deployment_model="down"} 1`,
		`kaiak_circuit_open{backend="local",deployment_model="open"} 0`,
		`kaiak_circuit_transitions_total{backend="down",deployment_model="down",to="open"} 1`,
		`kaiak_errors_total{class="no_healthy_deployment"} 1`,
	} {
		if !strings.Contains(metrics, want+"\n") {
			t.Errorf("metrics miss %s", want)
		}
	}
}

// Retries are failover only: with the default max_attempts, a request to a
// single-deployment model is one attempt, so a client (an SDK) repeating a failed
// call counts one failure per call — the threshold is reached by the calls, not
// multiplied by the attempts within each.
func TestClientRepeatsCountOneFailureEach(t *testing.T) {
	g, _ := newRetryGateway(t, "local", "local-b",
		withGlobal(t, `"circuit": { "failure_threshold": 3, "probe_interval_ms": 3600000 }`))
	for i := range 3 {
		expectError(t, post(t, g, "r"+strconv.Itoa(i), `{"model":"down"}`), http.StatusBadGateway, "upstream_unavailable")
		if open := circuitOpen(g, "down", "down"); open != (i == 2) {
			t.Fatalf("after call %d: circuit open %v, want open at the third", i+1, open)
		}
	}
}

func TestOpenDeploymentIsSkippedAndProbedBackIn(t *testing.T) {
	// pair-b gets its own backend, so failures can be scripted on one deployment.
	other := fakebackend.New()
	t.Cleanup(other.Close)
	g := newCircuitGateway(t, 2, func(doc string) string {
		return replaceMatch(t, doc, regexp.MustCompile(`"local-b": \{[^}]*\}`),
			`"local-b": { "type": "openai-compatible", "base_url": "`+other.URL()+`/v1" }`)
	})
	other.SetReply(fakebackend.Reply{Status: http.StatusInternalServerError})

	// Ties take turns: a, b, a, b — b fails twice and opens.
	for range 4 {
		do(t, g.h, call{method: "POST", path: "/v1/chat/completions", key: workloadKey, body: `{"model":"pair"}`})
	}
	if !circuitOpen(g, "local-b", "pair-b") || circuitOpen(g, "local", "pair-a") {
		t.Fatalf("open circuits %v, want only local-b/pair-b", notClosed(g.router))
	}
	served := len(g.backend.Requests())
	for range 4 {
		w := do(t, g.h, call{method: "POST", path: "/v1/chat/completions", key: workloadKey, body: `{"model":"pair"}`})
		if w.Code != http.StatusOK {
			t.Fatalf("status %d with one healthy deployment: %s", w.Code, w.Body.String())
		}
	}
	if n := len(g.backend.Requests()) - served; n != 4 || len(other.Requests()) != 2 {
		t.Errorf("healthy backend got %d of 4, open one %d in total; want all 4 on the healthy one", n, len(other.Requests()))
	}

	// A failing probe keeps it open; a successful one makes it half-open, and the
	// trial's success closes it.
	other.SetModelsStatus(http.StatusServiceUnavailable)
	if err := g.router.ProbeNow(context.Background(), "local-b", "test"); err == nil {
		t.Fatal("probe of a failing models list succeeded")
	}
	if !circuitOpen(g, "local-b", "pair-b") {
		t.Fatal("closed by a failed probe")
	}
	// The models list answers without pair-b (the host serves another model): still
	// open.
	other.SetModelsStatus(http.StatusOK)
	if err := g.router.ProbeNow(context.Background(), "local-b", "test"); err != nil {
		t.Fatal(err)
	}
	if !circuitOpen(g, "local-b", "pair-b") || len(other.Requests()) != 2 {
		t.Fatal("a probe not listing the deployment's model let requests in")
	}
	do(t, g.h, call{method: "POST", path: "/v1/chat/completions", key: workloadKey, body: `{"model":"pair"}`})
	if len(other.Requests()) != 2 {
		t.Fatal("a request reached the deployment its backend does not list")
	}
	other.SetModels("pair-b")
	other.SetReply(fakebackend.Reply{})
	if err := g.router.ProbeNow(context.Background(), "local-b", "test"); err != nil {
		t.Fatal(err)
	}
	if !circuitOpen(g, "local-b", "pair-b") {
		t.Fatalf("half-open circuit reported closed: %v", notClosed(g.router))
	}
	// Half-open is not open to the metrics: an idle recovered deployment must not keep
	// an open-circuit alert firing.
	expectMetricLines(t, scrape(g),
		`kaiak_circuit_open{backend="local-b",deployment_model="pair-b"} 0`,
		`kaiak_circuit_half_open{backend="local-b",deployment_model="pair-b"} 1`)
	do(t, g.h, call{method: "POST", path: "/v1/chat/completions", key: workloadKey, body: `{"model":"pair"}`})
	do(t, g.h, call{method: "POST", path: "/v1/chat/completions", key: workloadKey, body: `{"model":"pair"}`})
	if len(other.Requests()) != 3 {
		t.Errorf("the half-open deployment got %d requests in total, want 3 (its trial)", len(other.Requests()))
	}
	if len(notClosed(g.router)) != 0 {
		t.Fatalf("open circuits %v after the trial succeeded", notClosed(g.router))
	}

	logs := g.logText()
	for _, want := range []string{`"msg":"probe failed","kaiak.backend.id":"local-b","kaiak.trigger":"test"`,
		`"msg":"probe succeeded","kaiak.backend.id":"local-b","kaiak.trigger":"test"`,
		`"msg":"circuit kept open: the backend does not list the deployment's model","kaiak.backend.id":"local-b","kaiak.deployment.model":"pair-b","kaiak.trigger":"test"`,
		`"msg":"circuit half-open","kaiak.backend.id":"local-b","kaiak.deployment.model":"pair-b","kaiak.trigger":"test"`,
		`"msg":"circuit closed","kaiak.backend.id":"local-b","kaiak.deployment.model":"pair-b","kaiak.trigger":"trial"`} {
		if !strings.Contains(logs, want) {
			t.Errorf("log misses %s:\n%s", want, logs)
		}
	}
	metrics := scrape(g)
	for _, want := range []string{
		`kaiak_circuit_open{backend="local-b",deployment_model="pair-b"} 0`,
		`kaiak_circuit_half_open{backend="local-b",deployment_model="pair-b"} 0`,
		`kaiak_circuit_transitions_total{backend="local-b",deployment_model="pair-b",to="open"} 1`,
		`kaiak_circuit_transitions_total{backend="local-b",deployment_model="pair-b",to="half_open"} 1`,
		`kaiak_circuit_transitions_total{backend="local-b",deployment_model="pair-b",to="closed"} 1`,
		`kaiak_probes_total{backend="local-b",result="failure"} 1`,
		`kaiak_probes_total{backend="local-b",result="success"} 2`,
	} {
		if !strings.Contains(metrics, want+"\n") {
			t.Errorf("metrics miss %s", want)
		}
	}
	// The probe carried the backend's credential rules: local-b has none.
	if probes := other.ModelsRequests(); len(probes) != 3 || probes[0].Header.Get("Authorization") != "" {
		t.Errorf("probes %d, Authorization %q", len(probes), probes[0].Header.Get("Authorization"))
	}
}

func TestProbeUsesTheBackendCredential(t *testing.T) {
	g := newCircuitGateway(t, 1, nil)
	for _, backend := range []string{"local", "azure"} {
		if err := g.router.ProbeNow(context.Background(), backend, "test"); err != nil {
			t.Fatal(err)
		}
	}
	probes := g.backend.ModelsRequests()
	if len(probes) != 2 {
		t.Fatalf("%d probes, want 2", len(probes))
	}
	if probes[0].Path != "/v1/models" || probes[0].Header.Get("Authorization") != "Bearer "+localBackendKey {
		t.Errorf("local probe: %s, Authorization %q", probes[0].Path, probes[0].Header.Get("Authorization"))
	}
	if probes[1].Path != "/openai/v1/models" || probes[1].Header.Get("Api-Key") != azureBackendKey {
		t.Errorf("azure probe: %s, api-key %q", probes[1].Path, probes[1].Header.Get("Api-Key"))
	}
	if strings.Contains(g.logText(), localBackendKey) || strings.Contains(g.logText(), azureBackendKey) {
		t.Error("a backend credential reached the log")
	}
}

// A half-open trial is decided at its first event. A long stream as the trial does
// not keep its deployment out of rotation: the next request is served while the
// trial still streams.
func TestLongStreamingTrialDoesNotBlockItsDeployment(t *testing.T) {
	g := newCircuitGateway(t, 1, nil)
	url := serveGateway(t, g)
	reportFailure(t, g, "open")
	g.backend.SetModels("open")
	if err := g.router.ProbeNow(context.Background(), "local", "test"); err != nil {
		t.Fatal(err)
	}
	pace := make(chan struct{})
	g.backend.SetReply(fakebackend.Reply{Pace: pace})
	resp := streamRequest(t, context.Background(), url, `{"model":"open","stream":true}`)
	defer resp.Body.Close()
	body := bufio.NewReader(resp.Body)
	if line, err := body.ReadString('\n'); err != nil || !strings.HasPrefix(line, "data: ") {
		t.Fatalf("trial's first line %q, %v", line, err)
	}
	if circuitOpen(g, "local", "open") {
		t.Fatal("circuit still open after the trial's first event")
	}
	w := do(t, g.h, call{method: "POST", path: "/v1/chat/completions", key: userKey, body: `{"model":"open"}`})
	if w.Code != http.StatusOK {
		t.Fatalf("request during the trial = %d %s, want 200", w.Code, w.Body.String())
	}
	close(pace)
	rest, err := io.ReadAll(body)
	if err != nil || !strings.HasSuffix(string(rest), "data: [DONE]\n\n") {
		t.Fatalf("trial's stream ended %q, %v", rest, err)
	}
	expectMetricLines(t, scrape(g),
		`kaiak_circuit_transitions_total{backend="local",deployment_model="open",to="closed"} 1`)
}

// A half-open trial is decided at its first data event, not at a comment block: a
// backend that pings before its first token has not shown it can generate. The ping
// reaches the client ahead of that first data event: until it, the response is still
// in its first-event window (Providers: complete responses).
func TestTrialIsDecidedAtItsFirstDataEvent(t *testing.T) {
	g := newCircuitGateway(t, 1, nil)
	url := serveGateway(t, g)
	reportFailure(t, g, "open")
	g.backend.SetModels("open")
	if err := g.router.ProbeNow(context.Background(), "local", "test"); err != nil {
		t.Fatal(err)
	}
	pace := make(chan struct{})
	g.backend.SetReply(fakebackend.Reply{PingFirst: true, Pace: pace})
	type streamed struct {
		resp *http.Response
	}
	started := make(chan streamed, 1)
	go func() {
		started <- streamed{streamRequest(t, context.Background(), url, `{"model":"open","stream":true}`)}
	}()
	// The backend has the request and has sent its ping, holding its first token.
	waitFor(t, func() bool { return len(g.backend.Requests()) == 1 })
	if !circuitOpen(g, "local", "open") {
		t.Fatal("circuit closed by the trial's comment block")
	}
	pace <- struct{}{}
	resp := (<-started).resp
	defer resp.Body.Close()
	body := bufio.NewReader(resp.Body)
	if line, err := body.ReadString('\n'); err != nil || line != ": ping\n" {
		t.Fatalf("trial's first line %q, %v; want the ping", line, err)
	}
	for {
		line, err := body.ReadString('\n')
		if err != nil {
			t.Fatalf("trial's stream ended before a data event: %v", err)
		}
		if strings.HasPrefix(line, "data: ") {
			break
		}
	}
	if circuitOpen(g, "local", "open") {
		t.Fatal("circuit still open after the trial's first data event")
	}
	close(pace)
	if _, err := io.ReadAll(body); err != nil {
		t.Fatal(err)
	}
}

// A hung backend behind a live models list. The half-open trial running into
// its response timeout opens the circuit again; with the circuit closed, the third
// response timeout in a row counts as a failure. The "slow" backend's response
// timeout is 150 ms.
func TestResponseTimeoutsOfAHungBackendOpenItsCircuit(t *testing.T) {
	g := newCircuitGateway(t, 1, nil)
	g.backend.SetReply(fakebackend.Reply{Before: fakebackend.StallFirstByte})
	slow := func() {
		t.Helper()
		w := do(t, g.h, call{method: "POST", path: "/v1/chat/completions", key: workloadKey, body: `{"model":"slow"}`})
		expectError(t, w, http.StatusGatewayTimeout, "upstream_timeout")
	}
	slow()
	slow()
	if circuitOpen(g, "slow", "slow") {
		t.Fatal("opened by two response timeouts")
	}
	slow()
	if !circuitOpen(g, "slow", "slow") {
		t.Fatal("not open after three response timeouts in a row (threshold 1)")
	}
	g.backend.SetModels("slow")
	if err := g.router.ProbeNow(context.Background(), "slow", "test"); err != nil {
		t.Fatal(err)
	}
	slow() // the trial
	w := do(t, g.h, call{method: "POST", path: "/v1/chat/completions", key: workloadKey, body: `{"model":"slow"}`})
	expectError(t, w, http.StatusServiceUnavailable, "no_healthy_deployment")
	expectMetricLines(t, scrape(g),
		`kaiak_circuit_transitions_total{backend="slow",deployment_model="slow",to="open"} 2`,
		`kaiak_upstream_attempts_total{backend="slow",deployment_model="slow",outcome="response_timeout"} 4`)
}

// notClosed returns when each circuit of r that is not closed (open or half-open)
// opened.
func notClosed(r *routing.Router) map[routing.DeploymentID]time.Time {
	out := make(map[routing.DeploymentID]time.Time)
	for key, c := range r.Circuits() {
		out[key] = c.OpenedAt
	}
	return out
}
