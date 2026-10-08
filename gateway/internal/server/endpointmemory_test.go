package server

import (
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"kaiak/internal/config"
	"kaiak/internal/fakebackend"
	"kaiak/internal/provider"
)

// rememberedDeployments lists the deployments m remembers as not serving an endpoint,
// as backend/model, sorted.
func rememberedDeployments(m *MissingEndpoints) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var deployments []string
	for key := range m.until {
		deployments = append(deployments, key.deployment.Backend+"/"+key.deployment.Model)
	}
	slices.Sort(deployments)
	return deployments
}

// notSupported is llama-server's answer to an endpoint whose mode it was not started
// in (tools/server/server-common.cpp, format_error_response; b11513).
func notSupported(endpoint, flag string) fakebackend.Reply {
	return fakebackend.Reply{Status: http.StatusNotImplemented,
		Body: `{"error":{"code":501,"message":"This server does not support ` + endpoint + `. Start it with ` + "`" + flag + "`" +
			`","type":"not_supported_error"}}`}
}

// withRouterModels adds two router-mode llama-server backends — "ls" on g's fake
// backend, "ls2" on other — each model in a process of its own, with the circuit's
// probe interval probeMs:
//   - chat: ls/chat-gguf, a chat model started without --reranking;
//   - reranker: ls/rerank-gguf beside it, and ls2/rerank-gguf;
//   - embed: two variants on ls, embed-a-gguf and embed-b-gguf.
func withRouterModels(t *testing.T, g *testGateway, other *fakebackend.Backend, probeMs int) {
	t.Helper()
	g.apply(t, func(doc string) string {
		doc = withGlobal(t, `"circuit": { "probe_interval_ms": `+strconv.Itoa(probeMs)+` }`)(doc)
		doc = replaceOnce(t, doc, `"backends": {`, `"backends": {
    "ls": { "type": "llama-server", "base_url": "`+g.backend.URL()+`/v1" },
    "ls2": { "type": "llama-server", "base_url": "`+other.URL()+`/v1" },`)
		model := func(name string, deployments ...string) string {
			var list []string
			for _, d := range deployments {
				backend, backendModel, _ := strings.Cut(d, "/")
				list = append(list, `{ "backend": "`+backend+`", "model": "`+backendModel+`" }`)
			}
			return `"` + name + `": { "deployments": [` + strings.Join(list, ", ") + `],
      "metadata": { "context_length": 8192,
        "capabilities": { "streaming": true, "tools": false, "vision": false, "reasoning": false } } },
    `
		}
		return replaceOnce(t, doc, `"models": { `, `"models": {
    `+model("chat", "ls/chat-gguf")+model("reranker", "ls/rerank-gguf", "ls2/rerank-gguf")+
			model("embed", "ls/embed-a-gguf", "ls/embed-b-gguf"))
	})
}

// sendTo sends body to path for model with request ID id, and fails unless the
// answer's status is status.
func sendTo(t *testing.T, g *testGateway, path, id, body string, status int) {
	t.Helper()
	w := do(t, g.h, call{method: "POST", path: path, key: workloadKey, body: body, header: map[string]string{"X-Request-Id": id}})
	if w.Code != status {
		t.Fatalf("%s: status %d, want %d: %s", id, w.Code, status, w.Body.String())
	}
}

const (
	rerankOn = `{"query":"q","documents":["a"],"model":`
	embedOn  = `{"input":"a","model":`
)

// modelsSent lists the model each of reqs was sent for.
func modelsSent(t *testing.T, reqs []*fakebackend.Request) []string {
	t.Helper()
	var models []string
	for _, r := range reqs {
		var body struct{ Model string }
		if err := json.Unmarshal(r.Body, &body); err != nil {
			t.Fatalf("backend body %s: %v", r.Body, err)
		}
		models = append(models, body.Model)
	}
	return models
}

// warnings returns the log's endpoint-missing warnings.
func warnings(g *testGateway) []string {
	var lines []string
	for line := range strings.SplitSeq(g.logText(), "\n") {
		if strings.Contains(line, `"msg":"the deployment's server does not serve an endpoint its type serves"`) {
			lines = append(lines, line)
		}
	}
	return lines
}

// One model's 501 on rerank on a router-mode llama-server says nothing of the
// reranker beside it: the memory holds the deployment, not the backend, so the
// reranker on the same backend keeps its share of rerank traffic, and the chat model
// keeps serving chat. The warning names the deployment and the endpoint, at warning
// level (docs/specs/GATEWAY.md, Providers → An endpoint missing from a server).
func TestMissingEndpointIsRememberedPerDeployment(t *testing.T) {
	other := fakebackend.New()
	t.Cleanup(other.Close)
	g := newTestGateway(t)
	withRouterModels(t, g, other, 3600000)
	g.backend.QueueReplies(notSupported("reranking", "--reranking"))

	w := do(t, g.h, call{method: "POST", path: "/v1/rerank", key: workloadKey, body: rerankOn + `"chat"}`,
		header: map[string]string{"X-Request-Id": "to-chat"}})
	expectError(t, w, http.StatusBadGateway, "upstream_endpoint_missing")
	if strings.Contains(w.Body.String(), "support") {
		t.Errorf("the backend's answer reached the client: %s", w.Body.String())
	}
	if got := rememberedDeployments(g.missing); !slices.Equal(got, []string{"ls/chat-gguf"}) {
		t.Fatalf("remembered %v, want ls/chat-gguf alone", got)
	}
	lines := warnings(g)
	if len(lines) != 1 {
		t.Fatalf("%d endpoint-missing warnings, want 1:\n%s", len(lines), g.logText())
	}
	for _, want := range []string{`"level":"WARN"`, `"kaiak.request.id":"to-chat"`, `"kaiak.backend.id":"ls"`,
		`"kaiak.deployment.model":"chat-gguf"`, `"kaiak.endpoint":"rerank"`} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("warning misses %s: %s", want, lines[0])
		}
	}

	before := len(g.backend.Requests())
	for i := range 4 {
		sendTo(t, g, "/v1/rerank", "rr-"+strconv.Itoa(i), rerankOn+`"reranker"}`, http.StatusOK)
	}
	if got := modelsSent(t, g.backend.Requests()[before:]); len(got) == 0 || slices.ContainsFunc(got, func(m string) bool {
		return m != "rerank-gguf"
	}) {
		t.Errorf("ls got %v of 4 rerank requests, want some, all for rerank-gguf: one model's answer keeps no other deployment on its backend out", got)
	}
	sendTo(t, g, "/v1/chat/completions", "chat", `{"model":"chat"}`, http.StatusOK)
	if circuitOpen(g, "ls", "chat-gguf") {
		t.Error("circuit of ls/chat-gguf open, want closed: the outcome is neutral")
	}
}

// A request whose deployment's server answers the endpoint missing is retried on the
// model's other deployments — a sibling on the same backend too — and the deployment
// is left out of the endpoint's routing for the probe interval: the requests after
// it all go to the sibling (docs/specs/GATEWAY.md, Routing and reliability: retries;
// Providers → An endpoint missing from a server).
func TestMissingEndpointRetriesOnASiblingAndIsLeftOut(t *testing.T) {
	other := fakebackend.New()
	t.Cleanup(other.Close)
	g := newTestGateway(t)
	withRouterModels(t, g, other, 3600000)
	g.backend.QueueReplies(notSupported("embeddings", "--embeddings"))

	sendTo(t, g, "/v1/embeddings", "e-0", embedOn+`"embed"}`, http.StatusOK)
	tried := modelsSent(t, g.backend.Requests())
	if len(tried) != 2 || tried[0] == tried[1] {
		t.Fatalf("ls got requests for %v, want one for each variant", tried)
	}
	missing, serving := tried[0], tried[1]
	if want := `"kaiak.tried":"ls/` + missing + `:upstream_endpoint_missing,ls/` + serving + `:200"`; !strings.Contains(logLine(t, g, "e-0"), want) {
		t.Errorf("log line misses %s:\n%s", want, logLine(t, g, "e-0"))
	}
	for i := 1; i <= 3; i++ {
		sendTo(t, g, "/v1/embeddings", "e-"+strconv.Itoa(i), embedOn+`"embed"}`, http.StatusOK)
	}
	if got := modelsSent(t, g.backend.Requests()[2:]); !slices.Equal(got, []string{serving, serving, serving}) {
		t.Errorf("requests after the answer went to %v, want %s alone", got, serving)
	}
	expectMetricLines(t, scrape(g),
		`kaiak_retries_total{gen_ai_request_model="embed",kaiak_backend_id="ls",kaiak_attempt_outcome="endpoint_missing"} 1`)
}

// Once the probe interval passes, a deployment remembered as not serving an endpoint
// is tried again: a server that gained it — restarted with the flag, upgraded — is
// found by its next request (docs/specs/GATEWAY.md, Providers → An endpoint missing
// from a server).
func TestMissingEndpointIsFoundAgainAfterTheInterval(t *testing.T) {
	other := fakebackend.New()
	t.Cleanup(other.Close)
	g := newTestGateway(t)
	const interval = 100 * time.Millisecond
	withRouterModels(t, g, other, int(interval/time.Millisecond))
	g.backend.QueueReplies(notSupported("embeddings", "--embeddings"))

	sendTo(t, g, "/v1/embeddings", "e-0", embedOn+`"embed"}`, http.StatusOK)
	missing := modelsSent(t, g.backend.Requests())[0]
	if got := rememberedDeployments(g.missing); !slices.Equal(got, []string{"ls/" + missing}) {
		t.Fatalf("remembered %v, want ls/%s", got, missing)
	}
	time.Sleep(interval + 50*time.Millisecond)

	before := len(g.backend.Requests())
	for i := 1; i <= 4; i++ {
		sendTo(t, g, "/v1/embeddings", "e-"+strconv.Itoa(i), embedOn+`"embed"}`, http.StatusOK)
	}
	if got := modelsSent(t, g.backend.Requests()[before:]); !slices.Contains(got, missing) {
		t.Errorf("requests after the interval went to %v, want %s among them", got, missing)
	}
	if got := rememberedDeployments(g.missing); len(got) != 0 {
		t.Errorf("remembered %v after the interval, want nothing", got)
	}
}

// When every deployment of the model is remembered as not serving the endpoint, they
// are all tried again rather than none: a server may have been upgraded or restarted
// with another model. The warning comes once per interval and deployment, not once
// per request (docs/specs/GATEWAY.md, Providers → An endpoint missing from a server;
// Observability).
func TestEveryDeploymentRememberedIsTriedAgain(t *testing.T) {
	other := fakebackend.New()
	t.Cleanup(other.Close)
	g := newTestGateway(t)
	withRouterModels(t, g, other, 3600000)
	g.backend.SetReply(notSupported("embeddings", "--embeddings"))

	for i := range 3 {
		w := do(t, g.h, call{method: "POST", path: "/v1/embeddings", key: workloadKey, body: embedOn + `"embed"}`})
		expectError(t, w, http.StatusBadGateway, "upstream_endpoint_missing")
		if n := len(g.backend.Requests()); n != 2*(i+1) {
			t.Fatalf("after request %d ls got %d requests, want both variants tried each time", i, n)
		}
	}
	if got := rememberedDeployments(g.missing); !slices.Equal(got, []string{"ls/embed-a-gguf", "ls/embed-b-gguf"}) {
		t.Errorf("remembered %v, want both variants", got)
	}
	if lines := warnings(g); len(lines) != 2 {
		t.Errorf("%d endpoint-missing warnings, want one per deployment:\n%s", len(lines), strings.Join(lines, "\n"))
	}
	for _, model := range []string{"embed-a-gguf", "embed-b-gguf"} {
		if circuitOpen(g, "ls", model) {
			t.Errorf("circuit of ls/%s open, want closed: the outcome is neutral", model)
		}
	}
}

// Steady traffic to a model whose only deployment lacks the endpoint warns once per
// probe interval: the deployment is tried on every request — the model has no other —
// and an attempt inside the interval finds it remembered without extending the
// entry, so the first attempt after the interval remembers it afresh, with its
// warning (docs/specs/GATEWAY.md, Providers → An endpoint missing from a server;
// Observability).
func TestMissingEndpointWarnsOncePerInterval(t *testing.T) {
	other := fakebackend.New()
	t.Cleanup(other.Close)
	g := newTestGateway(t)
	const interval = 100 * time.Millisecond
	withRouterModels(t, g, other, int(interval/time.Millisecond))
	g.backend.SetReply(notSupported("reranking", "--reranking"))

	start := time.Now()
	requests := 0
	for time.Since(start) < 5*interval {
		w := do(t, g.h, call{method: "POST", path: "/v1/rerank", key: workloadKey, body: rerankOn + `"chat"}`})
		expectError(t, w, http.StatusBadGateway, "upstream_endpoint_missing")
		requests++
		time.Sleep(interval / 10) // steady traffic, about ten requests per interval
	}
	elapsed := time.Since(start)
	if n := len(g.backend.Requests()); n != requests {
		t.Fatalf("ls got %d requests of %d, want every one: the model's only deployment is tried each time", n, requests)
	}
	// Warnings come at least an interval apart, and at most an interval and the
	// gap between two requests apart.
	if n, most := len(warnings(g)), int(elapsed/interval)+1; n < 3 || n > most {
		t.Errorf("%d endpoint-missing warnings over %s of steady traffic, want one per %s interval: 3 to %d",
			n, elapsed.Round(time.Millisecond), interval, most)
	}
}

// An entry is never extended: it lasts one interval from when it was set, however
// often the deployment is found lacking the endpoint meanwhile, and the first time
// after that remembers it afresh — the moment worth a warning.
func TestMissingEndpointEntryLastsOneInterval(t *testing.T) {
	m := NewMissingEndpoints()
	d := config.Deployment{Backend: &config.Backend{ID: "vl"}, Model: "reranker-back"}
	t0 := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	const interval = 10 * time.Second
	for _, c := range []struct {
		at    time.Duration
		fresh bool
	}{
		{0, true},
		{interval / 2, false},
		{interval - time.Nanosecond, false},
		{interval, true},
		{interval + interval/2, false},
		{2*interval - time.Nanosecond, false},
		{2 * interval, true},
	} {
		if got := m.remember(d, provider.Rerank, t0.Add(c.at), interval); got != c.fresh {
			t.Errorf("remembered at +%s: fresh %v, want %v", c.at, got, c.fresh)
		}
	}
	if got := m.remember(d, provider.Embeddings, t0.Add(interval/2), interval); !got {
		t.Error("another endpoint of the same deployment: not fresh, want its own entry")
	}
}

// Only the endpoint missing is remembered: a missing model, a refused credential and
// a wrong path are failures of the deployment or its backend — counted by the
// circuit — not a server without the endpoint, and leave the memory and its warning
// alone (docs/specs/GATEWAY.md, Providers → An endpoint missing from a server).
func TestOnlyAMissingEndpointIsRemembered(t *testing.T) {
	other := fakebackend.New()
	t.Cleanup(other.Close)
	g := newTestGateway(t)
	withRouterModels(t, g, other, 3600000)
	for _, c := range []struct {
		name, path, body string
		reply            fakebackend.Reply
		code             string
	}{
		{"missing model", "/v1/rerank", rerankOn + `"chat"}`, fakebackend.Reply{Status: http.StatusNotFound,
			Body: `{"error":{"message":"The model ` + "`chat-gguf`" + ` does not exist.","type":"not_found_error","code":404}}`},
			"upstream_model_missing"},
		{"refused credential", "/v1/rerank", rerankOn + `"chat"}`, fakebackend.Reply{Status: http.StatusUnauthorized,
			Body: `{"error":{"message":"Invalid API Key","type":"authentication_error","code":401}}`}, "upstream_auth_failed"},
		{"wrong path", "/v1/chat/completions", `{"model":"chat"}`, fakebackend.Reply{Status: http.StatusNotFound,
			Body: `{"error":{"message":"File Not Found","type":"not_found_error","code":404}}`}, "upstream_path_missing"},
	} {
		g.backend.SetReply(c.reply)
		w := do(t, g.h, call{method: "POST", path: c.path, key: workloadKey, body: c.body})
		expectError(t, w, http.StatusBadGateway, c.code)
	}
	if got := rememberedDeployments(g.missing); len(got) != 0 {
		t.Errorf("remembered %v, want nothing", got)
	}
	if lines := warnings(g); len(lines) != 0 {
		t.Errorf("%d endpoint-missing warnings, want none:\n%s", len(lines), strings.Join(lines, "\n"))
	}
}

// An applied config that no longer has a deployment forgets what the
// missing-endpoint memory holds for it — its backend removed, or the deployment
// alone: no request visits its entry again, so its expiry would never remove it. A
// kept deployment's entry stays.
func TestAppliedConfigForgetsARemovedDeploymentsMissingEndpoints(t *testing.T) {
	g := newTestGateway(t)
	withResponsesModels(t, g)
	g.backend.SetReply(fakebackend.Reply{Status: http.StatusNotFound,
		Body: `{"error":{"message":"File Not Found","type":"not_found_error","code":404}}`})
	w := do(t, g.h, call{method: "POST", path: "/v1/responses", key: workloadKey, body: responsesBody})
	expectError(t, w, http.StatusBadGateway, "upstream_endpoint_missing")
	open := g.holder.Current().Models["open"].Deployments[0]
	g.missing.remember(open, provider.Responses, time.Now(), time.Hour)
	g.missing.remember(config.Deployment{Backend: open.Backend, Model: "gone"}, provider.Responses, time.Now(), time.Hour)
	if got := rememberedDeployments(g.missing); !slices.Equal(got, []string{"local/gone", "local/open", "ls/resp-back"}) {
		t.Fatalf("remembered %v, want local/gone, local/open and ls/resp-back", got)
	}

	g.apply(t, nil) // the test config: no backend ls, no deployment local/gone
	if got := rememberedDeployments(g.missing); !slices.Equal(got, []string{"local/open"}) {
		t.Errorf("remembered %v after the reload, want local/open alone", got)
	}
}
