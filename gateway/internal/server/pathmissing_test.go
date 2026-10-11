package server

import (
	"net/http"
	"strings"
	"testing"

	"kaiak/internal/config"
	"kaiak/internal/fakebackend"
	"kaiak/internal/provider"
)

// twinRequests are a request to model "twin" on an endpoint of each kind the tests
// below send: its route and body.
var twinRequests = map[provider.Endpoint]struct{ path, body string }{
	provider.ChatCompletions:   {"/v1/chat/completions", `{"model":"twin","messages":[{"role":"user","content":"hi"}]}`},
	provider.Embeddings:        {"/v1/embeddings", `{"model":"twin","input":"hi"}`},
	provider.Messages:          {"/v1/messages", `{"model":"twin","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`},
	provider.Rerank:            {"/v1/rerank", `{"model":"twin","query":"q","documents":["a"]}`},
	provider.ImagesGenerations: {"/v1/images/generations", `{"model":"twin","prompt":"a fox"}`},
}

// withTwins edits the test config so that model "twin" has two deployments on
// backends of type typ — twin-a on "p1" (g's fake backend), twin-b on "p2" (other) —
// listing endpoint alone; suffix is what the type's base_url adds to a fake backend's
// address. A circuit opens at its first failure.
func withTwins(t *testing.T, g *testGateway, other *fakebackend.Backend, typ config.BackendType, suffix string,
	endpoint provider.Endpoint) {
	t.Helper()
	g.apply(t, func(doc string) string {
		doc = withGlobal(t, `"circuit": { "failure_threshold": 1, "probe_interval_ms": 3600000 }`)(doc)
		doc = replaceOnce(t, doc, `"backends": {`, `"backends": {
    "p1": { "type": "`+string(typ)+`", "base_url": "`+g.backend.URL()+suffix+`", "api_key_env": "LOCAL_KEY" },
    "p2": { "type": "`+string(typ)+`", "base_url": "`+other.URL()+suffix+`", "api_key_env": "LOCAL_KEY" },`)
		return replaceOnce(t, doc, `"models": { `, `"models": {
    "twin": { "deployments": [{ "backend": "p1", "model": "twin-a" }, { "backend": "p2", "model": "twin-b" }],
      "endpoints": ["`+string(endpoint.Name())+`"],
      "metadata": { "context_length": 8192,
        "capabilities": { "streaming": true, "tools": false, "vision": false, "reasoning": false } } },
    `)
	})
}

// pathMissingMessage is upstream_path_missing's message, naming both causes.
const pathMissingMessage = "The model backend does not have this endpoint: its address is misconfigured, " +
	"or its server does not serve this API for the model."

// One "not found" reading on every type (docs/specs/GATEWAY.md, Providers → A path
// the server does not have): each type's signature of a path its server does not have
// answers the client 502 upstream_path_missing at once, in the endpoint's error shape,
// with the message naming both causes and not the backend's text. It is not retried —
// the model's other deployment, an identical instance, gets nothing — it is neutral
// for the circuit, counted as the attempt outcome path_missing and in the
// upstream_error class, and its record carries no usage.
func TestPathMissingAnswersAtOnce(t *testing.T) {
	for _, c := range []struct {
		name     string
		typ      config.BackendType
		suffix   string
		endpoint provider.Endpoint
		status   int
		answer   string
	}{
		{"route missing", config.BackendVLLM, "/v1", provider.ChatCompletions, http.StatusNotFound, `{"detail":"Not Found"}`},
		{"method not allowed", config.BackendVLLM, "/v1", provider.Rerank, http.StatusMethodNotAllowed,
			`{"detail":"Method Not Allowed"}`},
		{"route missing", config.BackendVLLMOmni, "/v1", provider.ImagesGenerations, http.StatusNotFound, `{"detail":"Not Found"}`},
		{"file not found", config.BackendLlamaServer, "/v1", provider.Embeddings, http.StatusNotFound,
			`{"error":{"message":"File Not Found","type":"not_found_error","code":404}}`},
		{"invalid URL", config.BackendOpenAI, "/v1", provider.ChatCompletions, http.StatusNotFound,
			`{"error":{"message":"Invalid URL (POST /chat/completions)","type":"invalid_request_error","param":null,"code":null}}`},
		{"resource not found", config.BackendAzureOpenAI, "", provider.ChatCompletions, http.StatusNotFound,
			`{"error":{"code":"404","message":"Resource not found"}}`},
		{"not found", config.BackendAnthropic, "/v1", provider.Messages, http.StatusNotFound,
			`{"type":"error","error":{"type":"not_found_error","message":"Not Found"}}`},
		{"resource not found", config.BackendAzureAnthropic, "", provider.Messages, http.StatusNotFound,
			`{"error":{"code":"404","message":"Resource not found"}}`},
		{"page not found", config.BackendOpenAICompatible, "/v1", provider.ChatCompletions, http.StatusNotFound,
			"404 page not found\n"},
	} {
		t.Run(string(c.typ)+"/"+c.name, func(t *testing.T) {
			other := fakebackend.New()
			t.Cleanup(other.Close)
			g := newTestGateway(t)
			withTwins(t, g, other, c.typ, c.suffix, c.endpoint)
			for _, b := range []*fakebackend.Backend{g.backend, other} {
				b.SetReply(fakebackend.Reply{Status: c.status, Body: c.answer})
			}

			r := twinRequests[c.endpoint]
			w := do(t, g.h, call{method: "POST", path: r.path, key: workloadKey, body: r.body,
				header: map[string]string{"X-Request-Id": "r"}})
			if c.endpoint.Format() == provider.FormatMessages {
				expectAnthropicError(t, w, http.StatusBadGateway, "api_error", "upstream_path_missing")
			} else {
				expectError(t, w, http.StatusBadGateway, "upstream_path_missing")
			}
			if msg := errorMessage(t, w); msg != pathMissingMessage {
				t.Errorf("message %q, want %q", msg, pathMissingMessage)
			}
			for _, text := range []string{"Invalid URL", "Not Found", "not found", "Not Allowed"} {
				if strings.Contains(w.Body.String(), text) {
					t.Errorf("the backend's answer reached the client: %s", w.Body.String())
				}
			}

			sentA, sentB := len(g.backend.Requests()), len(other.Requests())
			if sentA+sentB != 1 {
				t.Fatalf("the deployments got %d and %d requests, want one in all: not retried", sentA, sentB)
			}
			backend, model := "p1", "twin-a"
			if sentB == 1 {
				backend, model = "p2", "twin-b"
			}
			if circuitOpen(g, backend, model) {
				t.Errorf("circuit of %s/%s open after a failure threshold of 1, want closed: the outcome is neutral",
					backend, model)
			}
			if line := logLine(t, g, "r"); !strings.Contains(line, `"kaiak.attempts":1`) {
				t.Errorf("log line, want one attempt:\n%s", line)
			}
			records := recordsOf(g, "r")
			if len(records) != 1 {
				t.Fatalf("%d records, want the request's one", len(records))
			}
			for unit, n := range records[0].Units {
				if n != 0 {
					t.Errorf("record %s = %d, want no usage: nothing was processed", unit, n)
				}
			}
			if records[0].Estimated {
				t.Errorf("record estimated, want the backend's answer: %+v", records[0])
			}
			metricsText := scrape(g)
			expectMetricLines(t, metricsText,
				`kaiak_upstream_attempts_total{kaiak_backend_id="`+backend+`",kaiak_deployment_model="`+model+`",kaiak_attempt_outcome="path_missing"} 1`,
				`kaiak_errors_total{kaiak_error_class="upstream_error"} 1`)
			for line := range strings.Lines(metricsText) {
				if strings.HasPrefix(line, "kaiak_retries_total{") && !strings.HasSuffix(line, " 0\n") {
					t.Errorf("a retry counted: %s", line)
				}
			}
		})
	}
}

// A server's other answers to an endpoint it lacks are its own statuses, not read
// (docs/specs/GATEWAY.md, Providers → A path the server does not have): llama-server's
// 501 for embeddings or rerank when it was not started for them, and vLLM-Omni's 503
// for an image request to a model with no diffusion stage, are backend 5xx as any
// server error — retried on the model's other deployment as server_error, each a
// circuit failure, the client answered upstream_error under the backend's status.
func TestServersOwnAnswersAreServerErrors(t *testing.T) {
	for _, c := range []struct {
		name     string
		typ      config.BackendType
		endpoint provider.Endpoint
		status   int
		script   func(b *fakebackend.Backend)
	}{
		{"embeddings not supported", config.BackendLlamaServer, provider.Embeddings, http.StatusNotImplemented,
			func(b *fakebackend.Backend) { b.SetNotSupported("embeddings") }},
		{"rerank not supported", config.BackendLlamaServer, provider.Rerank, http.StatusNotImplemented,
			func(b *fakebackend.Backend) { b.SetNotSupported("rerank") }},
		{"no diffusion stage", config.BackendVLLMOmni, provider.ImagesGenerations, http.StatusServiceUnavailable,
			func(b *fakebackend.Backend) { b.SetNonImageModels("twin-a", "twin-b") }},
	} {
		t.Run(string(c.typ)+"/"+c.name, func(t *testing.T) {
			other := fakebackend.New()
			t.Cleanup(other.Close)
			g := newTestGateway(t)
			withTwins(t, g, other, c.typ, "/v1", c.endpoint)
			c.script(g.backend)
			c.script(other)

			r := twinRequests[c.endpoint]
			w := do(t, g.h, call{method: "POST", path: r.path, key: workloadKey, body: r.body,
				header: map[string]string{"X-Request-Id": "r"}})
			expectError(t, w, c.status, "upstream_error")
			for _, text := range []string{"support", "diffusion"} {
				if strings.Contains(w.Body.String(), text) {
					t.Errorf("the backend's answer reached the client: %s", w.Body.String())
				}
			}
			if a, b := len(g.backend.Requests()), len(other.Requests()); a != 1 || b != 1 {
				t.Errorf("the deployments got %d and %d requests, want one each: retried", a, b)
			}
			if !circuitOpen(g, "p1", "twin-a") || !circuitOpen(g, "p2", "twin-b") {
				t.Error("a circuit closed after its 5xx at a failure threshold of 1, want both open: a failure")
			}
			if line := logLine(t, g, "r"); !strings.Contains(line, `"kaiak.attempts":2`) {
				t.Errorf("log line, want two attempts:\n%s", line)
			}
			metricsText := scrape(g)
			for _, d := range []struct{ backend, model string }{{"p1", "twin-a"}, {"p2", "twin-b"}} {
				expectMetricLines(t, metricsText,
					`kaiak_upstream_attempts_total{kaiak_backend_id="`+d.backend+`",kaiak_deployment_model="`+d.model+`",kaiak_attempt_outcome="server_error"} 1`)
			}
		})
	}
}
