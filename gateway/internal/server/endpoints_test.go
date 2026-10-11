package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"kaiak/internal/config"
	"kaiak/internal/metrics"
	"kaiak/internal/provider"
	"kaiak/internal/routing"
)

// errorMessage is an OpenAI-shaped error answer's message.
func errorMessage(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("error body %s: %v", w.Body.String(), err)
	}
	return body.Error.Message
}

// withAnthropicModels swaps in the test config plus an anthropic backend "claude" on
// the fake backend, a model served only there ("claude-only") and one with a
// deployment there and one on "local" ("mixed").
func withAnthropicModels(t *testing.T, g *testGateway, edits ...func(doc string) string) {
	t.Helper()
	g.apply(t, func(doc string) string {
		for _, edit := range edits {
			doc = edit(doc)
		}
		doc = replaceOnce(t, doc, `"backends": {`, `"backends": {
    "claude": { "type": "anthropic", "base_url": "`+g.backend.URL()+`/v1", "api_key_env": "ANTHROPIC_KEY" },`)
		meta := `"metadata": { "context_length": 8192,
        "capabilities": { "streaming": true, "tools": false, "vision": false, "reasoning": false } }`
		return replaceOnce(t, doc, `"models": { `, `"models": {
    "claude-only": { "deployments": [{ "backend": "claude", "model": "claude-x" }], `+meta+` },
    "mixed": { "deployments": [{ "backend": "claude", "model": "claude-x" }, { "backend": "local", "model": "mixed-local" }], `+meta+` },
    `)
	})
}

// A model none of whose deployments serves the endpoint is refused before routing,
// 400 endpoint_not_served, nothing sent and nothing settled; a model with some that
// do is routed to those only (docs/specs/GATEWAY.md, Providers → Endpoint support).
func TestRoutingFollowsEndpointSupport(t *testing.T) {
	g := newTestGateway(t)
	withAnthropicModels(t, g)

	w := do(t, g.h, call{method: "POST", path: "/v1/chat/completions", key: workloadKey,
		body: `{"model":"claude-only","messages":[]}`})
	expectError(t, w, http.StatusBadRequest, "endpoint_not_served")
	if msg, want := errorMessage(t, w), "The model is not served on /v1/chat/completions: none of its backends serves this API."; msg != want {
		t.Errorf("message %q, want %q", msg, want)
	}
	if n := len(g.backend.Requests()); n != 0 {
		t.Errorf("%d requests reached the backend, want none", n)
	}
	if n := len(g.usage.all()); n != 0 {
		t.Errorf("%d usage records, want none: nothing was routed", n)
	}
	if !strings.Contains(g.logText(), `"error.type":"endpoint_not_served"`) {
		t.Errorf("log line lacks the error:\n%s", g.logText())
	}

	for range 4 {
		w := do(t, g.h, call{method: "POST", path: "/v1/chat/completions", key: workloadKey,
			body: `{"model":"mixed","messages":[]}`})
		if w.Code != http.StatusOK {
			t.Fatalf("mixed: status %d: %s", w.Code, w.Body.String())
		}
	}
	for _, r := range g.backend.Requests() {
		if !strings.Contains(string(r.Body), `"model":"mixed-local"`) {
			t.Errorf("a chat request reached the anthropic deployment: %s", r.Body)
		}
	}
}

// withOmniModel swaps in the test config plus a vllm-omni backend "omni" on the fake
// backend and a model served only there ("omni-only").
func withOmniModel(t *testing.T, g *testGateway) {
	t.Helper()
	g.apply(t, func(doc string) string {
		doc = replaceOnce(t, doc, `"backends": {`, `"backends": {
    "omni": { "type": "vllm-omni", "base_url": "`+g.backend.URL()+`/v1" },`)
		return replaceOnce(t, doc, `"models": { `, `"models": {
    "omni-only": { "deployments": [{ "backend": "omni", "model": "Qwen/Qwen3-TTS" }],
      "metadata": { "context_length": 8192,
        "capabilities": { "streaming": true, "tools": false, "vision": false, "reasoning": false } } },
    `)
	})
}

// omniRefusedBodies are a valid request body for each body endpoint vllm-omni does not
// serve, naming the model "omni-only".
var omniRefusedBodies = map[provider.Endpoint]string{
	provider.ChatCompletions:      `{"model":"omni-only","messages":[{"role":"user","content":"hi"}]}`,
	provider.Completions:          `{"model":"omni-only","prompt":"hi"}`,
	provider.Embeddings:           `{"model":"omni-only","input":"a"}`,
	provider.Messages:             `{"model":"omni-only","max_tokens":1,"messages":[]}`,
	provider.MessagesCountTokens:  `{"model":"omni-only","messages":[]}`,
	provider.Responses:            `{"model":"omni-only","input":"hi"}`,
	provider.ResponsesInputTokens: `{"model":"omni-only","input":"hi"}`,
	provider.Rerank:               `{"model":"omni-only","query":"q","documents":["a"]}`,
}

// vllm-omni serves the media endpoints alone: chat, completions, embeddings, Messages,
// Responses, rerank and the token-counting endpoints to a model whose deployments are
// all vllm-omni answer 400 endpoint_not_served, in the endpoint's error shape, before
// any backend and with nothing settled; the model's entry lists only what the type
// serves (docs/specs/GATEWAY.md, Providers → Endpoint support).
func TestVLLMOmniServesNoOtherEndpoint(t *testing.T) {
	g := newTestGateway(t)
	withOmniModel(t, g)
	served := []string{}
	for _, ep := range bodyEndpoints {
		if provider.Serves(config.BackendVLLMOmni, ep.api) {
			served = append(served, ep.name)
			continue
		}
		body, ok := omniRefusedBodies[ep.api]
		if !ok {
			t.Fatalf("no request body for %s, which vllm-omni does not serve", ep.name)
		}
		t.Run(ep.name, func(t *testing.T) {
			w := do(t, g.h, call{method: "POST", path: ep.route(), key: workloadKey, body: body,
				header: map[string]string{"X-Request-Id": "omni-" + ep.name}})
			if ep.api.Format() == provider.FormatMessages {
				expectAnthropicError(t, w, http.StatusBadRequest, "invalid_request_error", "endpoint_not_served")
			} else {
				expectError(t, w, http.StatusBadRequest, "endpoint_not_served")
			}
			if msg := errorMessage(t, w); !strings.Contains(msg, ep.route()) {
				t.Errorf("message %q does not name %s", msg, ep.route())
			}
			if line := logLine(t, g, "omni-"+ep.name); !strings.Contains(line, `"error.type":"endpoint_not_served"`) {
				t.Errorf("log line lacks the error: %s", line)
			}
		})
	}
	if n := len(g.backend.Requests()); n != 0 {
		t.Errorf("%d requests reached the backend, want none", n)
	}
	if n := len(g.usage.all()); n != 0 {
		t.Errorf("%d usage records, want none: nothing was routed", n)
	}

	slices.Sort(served)
	want, _ := json.Marshal(served)
	w := do(t, g.h, call{method: "GET", path: "/v1/models/omni-only", key: workloadKey})
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"endpoints":`+string(want)) {
		t.Errorf("omni-only: %d %s, want the endpoints %s", w.Code, w.Body.String(), want)
	}
}

// Model entries list the endpoints their deployments serve.
func TestModelEntriesListServedEndpoints(t *testing.T) {
	g := newTestGateway(t)
	withAnthropicModels(t, g)
	for model, want := range map[string]string{
		"claude-only": `"endpoints":["messages","messages_count_tokens"]`,
		"mixed":       `"endpoints":["chat_completions","completions","embeddings","messages","messages_count_tokens"]`,
	} {
		w := do(t, g.h, call{method: "GET", path: "/v1/models/" + model, key: workloadKey})
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), want) {
			t.Errorf("%s: %d %s, want %s", model, w.Code, w.Body.String(), want)
		}
	}
}

// withEndpointLists is withSpeechModels with an anthropic backend "claude" on the fake
// backend, painter's endpoints list narrowed to images_generations, and three models
// with lists: "mixed-listed" on claude and local, listing chat_completions alone;
// "claude-msg" on claude, listing messages; "claude-counts" on claude, listing
// messages_count_tokens. tts keeps no list.
func withEndpointLists(t *testing.T, g *testGateway) {
	t.Helper()
	withSpeechModels(t, g, func(doc string) string {
		doc = replaceOnce(t, doc, `"backends": {`, `"backends": {
    "claude": { "type": "anthropic", "base_url": "`+g.backend.URL()+`/v1", "api_key_env": "ANTHROPIC_KEY" },`)
		doc = replaceOnce(t, doc, `"model": "Qwen/Qwen-Image" }],`, `"model": "Qwen/Qwen-Image" }], "endpoints": ["images_generations"],`)
		meta := `"metadata": { "context_length": 8192,
        "capabilities": { "streaming": true, "tools": false, "vision": false, "reasoning": false } }`
		return replaceOnce(t, doc, `"models": {`, `"models": {
    "mixed-listed": { "deployments": [{ "backend": "claude", "model": "claude-x" }, { "backend": "local", "model": "mixed-local" }],
      "endpoints": ["chat_completions"], `+meta+` },
    "claude-msg": { "deployments": [{ "backend": "claude", "model": "claude-x" }], "endpoints": ["messages"], `+meta+` },
    "claude-counts": { "deployments": [{ "backend": "claude", "model": "claude-x" }], "endpoints": ["messages_count_tokens"], `+meta+` },`)
	})
}

// A model's endpoints list narrows what its deployments serve (docs/specs/GATEWAY.md,
// Model metadata → Endpoints per model): an endpoint the list leaves out is refused
// 400 endpoint_not_served before routing — though a deployment's type serves it — in
// the endpoint's error shape, its message naming the route and the model's configured
// endpoints, nothing sent and nothing settled; a listed endpoint routes as before, to
// the deployments whose type serves it.
func TestEndpointsListRefusesTheOthers(t *testing.T) {
	g := newTestGateway(t)
	withEndpointLists(t, g)

	for _, c := range []struct{ id, path, body string }{
		{"speech-to-painter", "/v1/audio/speech", `{"model":"painter","input":"Hello there"}`},
		{"batch-to-painter", "/v1/audio/speech/batch", `{"model":"painter","items":[{"input":"Hello there"}]}`},
		{"embeddings-to-mixed", "/v1/embeddings", `{"model":"mixed-listed","input":"a"}`},
	} {
		w := do(t, g.h, call{method: "POST", path: c.path, key: workloadKey, body: c.body,
			header: map[string]string{"X-Request-Id": c.id}})
		expectError(t, w, http.StatusBadRequest, "endpoint_not_served")
		if msg, want := errorMessage(t, w), notListed(c.path); msg != want {
			t.Errorf("%s: message %q, want %q", c.id, msg, want)
		}
		if line := logLine(t, g, c.id); !strings.Contains(line, `"error.type":"endpoint_not_served"`) {
			t.Errorf("%s: log line lacks the error: %s", c.id, line)
		}
	}
	// Messages to a model on an anthropic deployment whose list leaves Messages out:
	// refused in Anthropic's shape, with the same message.
	w := do(t, g.h, call{method: "POST", path: "/v1/messages", key: workloadKey,
		body: `{"model":"mixed-listed","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`})
	expectAnthropicError(t, w, http.StatusBadRequest, "invalid_request_error", "endpoint_not_served")
	if !strings.Contains(w.Body.String(), `"message":"`+notListed("/v1/messages")+`"`) {
		t.Errorf("Messages refusal %s, want the message %q", w.Body.String(), notListed("/v1/messages"))
	}
	if n := len(g.backend.Requests()); n != 0 {
		t.Errorf("%d requests reached the backend, want none", n)
	}
	if n := len(g.usage.all()); n != 0 {
		t.Errorf("%d usage records, want none: nothing was routed", n)
	}

	// The listed endpoints route as before: images to painter; chat to mixed-listed,
	// on its local deployment, whose type serves chat — never on claude.
	if w := do(t, g.h, imagesCall(`{"model":"painter","prompt":"a fox"}`, "images-listed")); w.Code != http.StatusOK {
		t.Fatalf("images to painter: %d %s", w.Code, w.Body.String())
	}
	for range 3 {
		w := do(t, g.h, call{method: "POST", path: "/v1/chat/completions", key: workloadKey,
			body: `{"model":"mixed-listed","messages":[{"role":"user","content":"hi"}]}`})
		if w.Code != http.StatusOK {
			t.Fatalf("chat to mixed-listed: %d %s", w.Code, w.Body.String())
		}
	}
	reqs := g.backend.Requests()
	if len(reqs) != 4 || reqs[0].Path != "/v1/images/generations" {
		t.Fatalf("backend requests %+v, want the image request and three chat requests", reqs)
	}
	for _, r := range reqs[1:] {
		if r.Path != "/v1/chat/completions" || !strings.Contains(string(r.Body), `"model":"mixed-local"`) {
			t.Errorf("chat request reached %s with %s, want the local deployment", r.Path, r.Body)
		}
	}
}

// notListed is endpoint_not_served's message for a model whose endpoints list leaves
// the endpoint at path out.
func notListed(path string) string {
	return "The model is not served on " + path + ": its configured endpoints do not include this API."
}

// A model with an endpoints list shows the endpoints it lists — in its entry, in
// /v1/models, and in the Anthropic-shaped list, which holds the models whose
// endpoints include messages; a model without one shows every endpoint its
// deployments' types serve, as before (docs/specs/GATEWAY.md, Client API → /v1/models;
// Anthropic-shaped model list).
func TestEndpointsListShowsInTheModelLists(t *testing.T) {
	g := newTestGateway(t)
	withEndpointLists(t, g)
	for model, want := range map[string]string{
		"painter":       `"endpoints":["images_generations"]`,
		"mixed-listed":  `"endpoints":["chat_completions"]`,
		"claude-msg":    `"endpoints":["messages"]`,
		"claude-counts": `"endpoints":["messages_count_tokens"]`,
		"tts":           `"endpoints":["audio_speech","audio_speech_batch","images_generations"]`,
	} {
		w := do(t, g.h, call{method: "GET", path: "/v1/models/" + model, key: workloadKey})
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), want) {
			t.Errorf("%s: %d %s, want %s", model, w.Code, w.Body.String(), want)
		}
	}
	w := do(t, g.h, call{method: "GET", path: "/v1/models", key: workloadKey})
	if !strings.Contains(w.Body.String(), `"id":"painter",`) ||
		!strings.Contains(w.Body.String(), `"endpoints":["images_generations"]}`) {
		t.Errorf("/v1/models: %s, want painter listing images_generations alone", w.Body.String())
	}

	anthropic := map[string]string{"Anthropic-Version": "2023-06-01"}
	w = do(t, g.h, call{method: "GET", path: "/v1/models", key: workloadKey, header: anthropic})
	var list struct {
		Data []struct {
			ID        string
			Endpoints []string
		}
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || w.Code != http.StatusOK {
		t.Fatalf("Anthropic list: %d (%v) %s", w.Code, err, w.Body.String())
	}
	if len(list.Data) != 1 || list.Data[0].ID != "claude-msg" || !slices.Equal(list.Data[0].Endpoints, []string{"messages"}) {
		t.Errorf("Anthropic list %s, want claude-msg alone, listing messages", w.Body.String())
	}
	for _, model := range []string{"mixed-listed", "claude-counts"} {
		expectAnthropicError(t, do(t, g.h, call{method: "GET", path: "/v1/models/" + model, key: workloadKey, header: anthropic}),
			http.StatusNotFound, "not_found_error", "model_not_found")
	}
	if w := do(t, g.h, call{method: "GET", path: "/v1/models/claude-msg", key: workloadKey, header: anthropic}); w.Code != http.StatusOK {
		t.Errorf("Anthropic entry of claude-msg: %d %s", w.Code, w.Body.String())
	}
}

// The key may come in x-api-key when there is no Authorization header, as the
// Anthropic SDKs send it; with both, Authorization is the one read.
func TestXAPIKeyAuthenticates(t *testing.T) {
	g := newTestGateway(t)
	w := do(t, g.h, call{method: "GET", path: "/v1/models", header: map[string]string{"X-Api-Key": workloadKey}})
	if w.Code != http.StatusOK {
		t.Fatalf("x-api-key: status %d: %s", w.Code, w.Body.String())
	}
	w = do(t, g.h, call{method: "POST", path: "/v1/chat/completions", body: chatBody,
		header: map[string]string{"X-Api-Key": workloadKey}})
	if w.Code != http.StatusOK {
		t.Fatalf("x-api-key chat: status %d: %s", w.Code, w.Body.String())
	}
	w = do(t, g.h, call{method: "GET", path: "/v1/models", key: "kaiak-not-a-key",
		header: map[string]string{"X-Api-Key": workloadKey}})
	expectError(t, w, http.StatusUnauthorized, "invalid_api_key")
	w = do(t, g.h, call{method: "GET", path: "/v1/models"})
	expectError(t, w, http.StatusUnauthorized, "missing_api_key")
	if msg := errorMessage(t, w); !strings.Contains(msg, "x-api-key") {
		t.Errorf("missing-key message %q does not name x-api-key", msg)
	}
	// The key never reaches the backend, whichever header carried it.
	for _, r := range g.backend.Requests() {
		if strings.Contains(r.Header.Get("X-Api-Key"), workloadKey) || strings.Contains(r.Header.Get("Authorization"), workloadKey) {
			t.Errorf("the client's key reached the backend: %v", r.Header)
		}
	}
}

// A provider's refusal before sending is the caller's 400, named by the provider's
// code and parameter, never retried, neutral for the circuit; an endpoint missing
// from the deployment's server is retried on the model's other deployments — on the
// same backend too: on a router-mode llama-server each model runs with its own flags
// — neutral too (docs/specs/GATEWAY.md, Routing and reliability: retries, outcome
// classes; Providers → An endpoint missing from a server).
func TestRefusalAndEndpointMissingClassification(t *testing.T) {
	refusal := &provider.RefusalError{Code: "price_option_unsupported", Param: "speed", Message: "no fast mode"}
	at := &attempt{}
	failure := upstreamFailure(context.Background(), at, refusal)
	if failure.status != http.StatusBadRequest || failure.code != "price_option_unsupported" || failure.param != "speed" ||
		failure.errType != typeInvalidRequest {
		t.Errorf("refusal answered %+v", failure)
	}
	if reason := retriedFor(at); reason != "" {
		t.Errorf("refusal retried for %q", reason)
	}
	if outcome, class, _ := classifyAttempt(at, ""); outcome != metrics.AttemptClientError || class != routing.Neutral {
		t.Errorf("refusal classified %s, %v", outcome, class)
	}

	missing := &provider.Error{Code: provider.CodeEndpointMissing, Err: errors.New("no messages endpoint")}
	at = &attempt{}
	failure = upstreamFailure(context.Background(), at, missing)
	if failure.status != http.StatusBadGateway || failure.code != "upstream_endpoint_missing" {
		t.Errorf("endpoint missing answered %+v", failure)
	}
	if reason := retriedFor(at); reason != metrics.AttemptEndpointMissing {
		t.Errorf("endpoint missing retry reason %q", reason)
	}
	if outcome, class, _ := classifyAttempt(at, ""); outcome != metrics.AttemptEndpointMissing || class != routing.Neutral {
		t.Errorf("endpoint missing classified %s, %v", outcome, class)
	}
	a := &config.Backend{ID: "a"}
	b := &config.Backend{ID: "b"}
	m := &config.Model{Deployments: []config.Deployment{{Backend: a, Model: "x"}, {Backend: a, Model: "y"}, {Backend: b, Model: "x"}}}
	avoid := avoidAfter(routing.Avoid{}, m, &attempt{deployment: m.Deployments[0], retryReason: metrics.AttemptEndpointMissing})
	for _, d := range m.Deployments {
		refused := false
		for _, r := range avoid.Refused {
			refused = refused || r == (routing.DeploymentID{Backend: d.Backend.ID, Model: d.Model})
		}
		if refused != (d == m.Deployments[0]) {
			t.Errorf("deployment %s/%s refused %v, want the one tried refused alone", d.Backend.ID, d.Model, refused)
		}
	}
}

// An error event is classified by what it names, as the HTTP status it matches would
// be (docs/specs/GATEWAY.md, Providers: error events):
// the backend failing is a 5xx — retried, a circuit failure; the backend busy
// (overloaded, rate-limiting) a 429 — retried as rate-limited, neutral; the caller's
// fault a 4xx — not retried, neutral. Anthropic's 529 is busy too. Before the first
// event the gateway answers as that status; after it the stream ends incomplete and
// only its classification follows the kind.
func TestErrorEventsAndOverloadClassification(t *testing.T) {
	for _, c := range []struct {
		name       string
		kind       provider.ErrorEventKind
		code       string
		status     int
		answerCode string
		reason     metrics.AttemptOutcome
		outcome    metrics.AttemptOutcome
		class      routing.Outcome
		midOutcome metrics.AttemptOutcome
		midClass   routing.Outcome
		midError   metrics.ErrorClass
	}{
		{"failure", provider.ErrorEventFailure, `"server_error"`, http.StatusBadGateway, "upstream_error", metrics.AttemptServerError,
			metrics.AttemptServerError, routing.Failure, metrics.AttemptBrokeOff, routing.Failure, metrics.ErrorUpstreamError},
		{"busy", provider.ErrorEventBusy, `"overloaded_error"`, http.StatusServiceUnavailable, "upstream_overloaded", metrics.AttemptRateLimited,
			metrics.AttemptRateLimited, routing.Neutral, metrics.AttemptRateLimited, routing.Neutral, metrics.ErrorUpstreamRateLimited},
		{"caller", provider.ErrorEventCaller, `"failed_to_download_image"`, http.StatusBadRequest, "upstream_refused", "",
			metrics.AttemptClientError, routing.Neutral, metrics.AttemptClientError, routing.Neutral, metrics.ErrorUpstreamClientError},
	} {
		t.Run(c.name, func(t *testing.T) {
			event := &provider.ErrorEvent{Kind: c.kind, Code: json.RawMessage(c.code)}
			at := &attempt{}
			failure := upstreamFailure(context.Background(), at,
				&provider.Error{Code: provider.CodeErrorEvent, Event: event, Err: errors.New("error event first")})
			if failure.status != c.status || failure.code != c.answerCode {
				t.Errorf("first event answered %d %s, want %d %s", failure.status, failure.code, c.status, c.answerCode)
			}
			if c.kind == provider.ErrorEventCaller && !strings.Contains(failure.message, "failed_to_download_image") {
				t.Errorf("refusal message %q does not name the backend's code", failure.message)
			}
			if reason := retriedFor(at); reason != c.reason {
				t.Errorf("first event retry reason %q, want %q", reason, c.reason)
			}
			if outcome, class, _ := classifyAttempt(at, ""); outcome != c.outcome || class != c.class {
				t.Errorf("first event classified %s, %v; want %s, %v", outcome, class, c.outcome, c.class)
			}

			midAttempt := &attempt{status: http.StatusOK, err: &provider.ErrorEventEnd{Event: event}}
			mid := &request{w: &statusWriter{ResponseWriter: httptest.NewRecorder()}, relayEnd: relayUpstreamIncomplete,
				attempts: []*attempt{midAttempt}}
			if outcome, class, _ := classifyAttempt(midAttempt, mid.relayEnd); outcome != c.midOutcome || class != c.midClass {
				t.Errorf("mid-stream classified %s, %v; want %s, %v", outcome, class, c.midOutcome, c.midClass)
			}
			if class, _ := errorClass(mid); class != c.midError {
				t.Errorf("mid-stream error class %s, want %s", class, c.midError)
			}
		})
	}

	overloaded := &attempt{status: 529}
	if reason := retriedFor(overloaded); reason != metrics.AttemptRateLimited {
		t.Errorf("529 retry reason %q, want %q", reason, metrics.AttemptRateLimited)
	}
	if outcome, class, _ := classifyAttempt(overloaded, ""); outcome != metrics.AttemptRateLimited || class != routing.Neutral {
		t.Errorf("529 classified %s, %v", outcome, class)
	}
}

// endpoint_not_served is decided before limits: a request its model can never answer
// spends no request slot, and gets the answer that says what is wrong, not a 429.
func TestEndpointNotServedComesBeforeLimits(t *testing.T) {
	g := newTestGateway(t)
	withAnthropicModels(t, g, func(doc string) string {
		return replaceOnce(t, doc, `"allowed_models": ["*"] }`,
			`"allowed_models": ["*"], "limits": [{ "type": "requests_per_minute", "value": 1 }] }`)
	})
	if w := do(t, g.h, call{method: "POST", path: "/v1/chat/completions", key: workloadKey, body: chatBody}); w.Code != http.StatusOK {
		t.Fatalf("first request: %d %s", w.Code, w.Body.String())
	}
	w := do(t, g.h, call{method: "POST", path: "/v1/chat/completions", key: workloadKey,
		body: `{"model":"claude-only","messages":[]}`})
	expectError(t, w, http.StatusBadRequest, "endpoint_not_served")
}
