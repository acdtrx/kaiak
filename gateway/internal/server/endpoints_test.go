package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// withEndpointLists is withSpeechModels with an anthropic backend "claude" on the fake
// backend, painter's endpoints list narrowed to images_generations, and three models
// with narrow lists: "chat-listed" on local (openai-compatible, as "chat-local"),
// listing chat_completions alone; "claude-msg" on claude, listing messages;
// "claude-counts" on claude, listing messages_count_tokens. tts lists every endpoint
// its type serves. Then edits applied.
func withEndpointLists(t *testing.T, g *testGateway, edits ...func(doc string) string) {
	t.Helper()
	withSpeechModels(t, g, func(doc string) string {
		doc = replaceOnce(t, doc, `"backends": {`, `"backends": {
    "claude": { "type": "anthropic", "base_url": "`+g.backend.URL()+`/v1", "api_key_env": "ANTHROPIC_KEY" },`)
		doc = replaceOnce(t, doc, `"model": "Qwen/Qwen-Image" }],
      "endpoints": `+everyEndpoint(config.BackendVLLMOmni)+`,`, `"model": "Qwen/Qwen-Image" }],
      "endpoints": ["images_generations"],`)
		meta := `"metadata": { "context_length": 8192,
        "capabilities": { "streaming": true, "tools": false, "vision": false, "reasoning": false } }`
		doc = replaceOnce(t, doc, `"models": {`, `"models": {
    "chat-listed": { "deployments": [{ "backend": "local", "model": "chat-local" }],
      "endpoints": ["chat_completions"], `+meta+` },
    "claude-msg": { "deployments": [{ "backend": "claude", "model": "claude-x" }], "endpoints": ["messages"], `+meta+` },
    "claude-counts": { "deployments": [{ "backend": "claude", "model": "claude-x" }], "endpoints": ["messages_count_tokens"], `+meta+` },`)
		for _, edit := range edits {
			doc = edit(doc)
		}
		return doc
	})
}

// A model serves exactly what its endpoints list names (docs/specs/GATEWAY.md, Model
// metadata → Endpoints per model): an endpoint the list leaves out is refused 400
// endpoint_not_served before routing — though the model's type serves it — in the
// endpoint's error shape, with the one message naming the route, nothing sent and
// nothing settled; a listed endpoint routes to the model's deployments.
func TestEndpointsListRefusesTheOthers(t *testing.T) {
	g := newTestGateway(t)
	withEndpointLists(t, g)

	for _, c := range []struct{ id, path, body string }{
		{"speech-to-painter", "/v1/audio/speech", `{"model":"painter","input":"Hello there"}`},
		{"batch-to-painter", "/v1/audio/speech/batch", `{"model":"painter","items":[{"input":"Hello there"}]}`},
		{"embeddings-to-chat", "/v1/embeddings", `{"model":"chat-listed","input":"a"}`},
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
	// Messages to an anthropic model whose list leaves Messages out: refused in
	// Anthropic's shape, with the same message.
	w := do(t, g.h, call{method: "POST", path: "/v1/messages", key: workloadKey,
		body: `{"model":"claude-counts","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`})
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

	// The listed endpoints route: images to painter, chat to chat-listed.
	if w := do(t, g.h, imagesCall(`{"model":"painter","prompt":"a fox"}`, "images-listed")); w.Code != http.StatusOK {
		t.Fatalf("images to painter: %d %s", w.Code, w.Body.String())
	}
	if w := do(t, g.h, call{method: "POST", path: "/v1/chat/completions", key: workloadKey,
		body: `{"model":"chat-listed","messages":[{"role":"user","content":"hi"}]}`}); w.Code != http.StatusOK {
		t.Fatalf("chat to chat-listed: %d %s", w.Code, w.Body.String())
	}
	reqs := g.backend.Requests()
	if len(reqs) != 2 || reqs[0].Path != "/v1/images/generations" || reqs[1].Path != "/v1/chat/completions" ||
		!strings.Contains(string(reqs[1].Body), `"model":"chat-local"`) {
		t.Fatalf("backend requests %+v, want the image request and the chat request", reqs)
	}
}

// notListed is endpoint_not_served's message for a model whose endpoints list leaves
// the endpoint at path out.
func notListed(path string) string {
	return "The model is not served on " + path + ": its configured endpoints do not include this API."
}

// A model shows the endpoints it lists — in its entry, in /v1/models, and in the
// Anthropic-shaped list, which holds the models whose endpoints include messages
// (docs/specs/GATEWAY.md, Client API → /v1/models; Anthropic-shaped model list).
func TestEndpointsListShowsInTheModelLists(t *testing.T) {
	g := newTestGateway(t)
	withEndpointLists(t, g)
	for model, want := range map[string]string{
		"painter":       `"endpoints":["images_generations"]`,
		"chat-listed":   `"endpoints":["chat_completions"]`,
		"claude-msg":    `"endpoints":["messages"]`,
		"claude-counts": `"endpoints":["messages_count_tokens"]`,
		"tts":           `"endpoints":["audio_speech","audio_speech_batch","images_edits","images_generations"]`,
		"open":          `"endpoints":["chat_completions","completions","embeddings"]`,
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
	for _, model := range []string{"chat-listed", "claude-counts"} {
		expectAnthropicError(t, do(t, g.h, call{method: "GET", path: "/v1/models/" + model, key: workloadKey, header: anthropic}),
			http.StatusNotFound, "not_found_error", "model_not_found")
	}
	if w := do(t, g.h, call{method: "GET", path: "/v1/models/claude-msg", key: workloadKey, header: anthropic}); w.Code != http.StatusOK {
		t.Errorf("Anthropic entry of claude-msg: %d %s", w.Code, w.Body.String())
	}
}

// withListedOn edits the test config so that open and secret are each served on a
// backend "typed" of type typ, on the fake backend, listing listed as their endpoints.
func withListedOn(t *testing.T, doc string, typ config.BackendType, listed []string) string {
	t.Helper()
	var root map[string]any
	if err := json.Unmarshal([]byte(doc), &root); err != nil {
		t.Fatal(err)
	}
	backends := root["backends"].(map[string]any)
	backends["typed"] = map[string]any{"type": string(typ), "base_url": backends["local"].(map[string]any)["base_url"]}
	for _, name := range []string{"open", "secret"} {
		m := root["models"].(map[string]any)[name].(map[string]any)
		m["deployments"] = []any{map[string]any{"backend": "typed", "model": name}}
		m["endpoints"] = listed
	}
	b, err := json.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// listedRequests are a request to secret for an endpoint the test lists beside the one
// it refuses: its type serves both, so a refusal comes from the list alone.
var listedRequests = map[provider.Endpoint]struct{ path, body string }{
	provider.ChatCompletions:   {"/v1/chat/completions", `{"model":"secret","messages":[]}`},
	provider.Embeddings:        {"/v1/embeddings", `{"model":"secret","input":"hi"}`},
	provider.AudioSpeech:       {"/v1/audio/speech", `{"model":"secret","input":"hi"}`},
	provider.ImagesGenerations: {"/v1/images/generations", `{"model":"secret","prompt":"a fox"}`},
}

// The endpoints list is judged on every body route, streaming or not, after the
// model-access check and before limits (docs/specs/GATEWAY.md, Request pipeline,
// stage 2): each route's model is on a type that serves it — llama-server, or
// vllm-omni for the media endpoints — and lists another endpoint of that type alone,
// so the refusal comes only from the list. A caller without access to the model gets
// model_not_found, nothing about its endpoints; one with access gets
// endpoint_not_served in the route's error shape with the one message, nothing sent,
// nothing settled, and the group's one request a minute still free for the listed
// endpoint. An upload carries a form — one every upload format reads cleanly, an image
// edit's input image and a transcription's audio — and is never a stream: its stream
// part asking for one is refused as the body is read, before the model is known.
func TestEndpointsListIsJudgedOnEveryRoute(t *testing.T) {
	for _, ep := range bodyEndpoints {
		typ := config.BackendLlamaServer
		listed, other := provider.ChatCompletions, provider.Embeddings
		if !typ.Serves(ep.api.Name()) {
			typ = config.BackendVLLMOmni
			listed, other = provider.ImagesGenerations, provider.AudioSpeech
		}
		if ep.api == listed {
			listed = other
		}
		if !typ.Serves(ep.api.Name()) || !typ.Serves(listed.Name()) {
			t.Fatalf("%s: no type in the test serves it", ep.route())
		}
		for _, stream := range []bool{false, true} {
			if stream && ep.upload != nil {
				continue
			}
			t.Run(fmt.Sprintf("%s/stream=%t", ep.api.Name(), stream), func(t *testing.T) {
				g := newTestGateway(t)
				g.apply(t, func(doc string) string {
					doc = replaceOnce(t, doc, `"eval": { "parent": "research", "allowed_models": ["*"] }`,
						`"eval": { "parent": "research", "allowed_models": ["*"], "limits": [{ "type": "requests_per_minute", "value": 1 }] }`)
					return withListedOn(t, doc, typ, []string{string(listed.Name())})
				})
				// One body every route reads cleanly: each takes the members it owns.
				body := fmt.Sprintf(`{"model":"secret","stream":%t,"messages":[],"max_tokens":1,"input":"hi","prompt":"hi",`+
					`"query":"q","documents":["a"],"items":[{"input":"hi"}]}`, stream)
				if ep.api.Format() == provider.FormatResponses {
					body = fmt.Sprintf(`{"model":"secret","stream":%t,"input":"hi"}`, stream)
				}
				var header map[string]string
				if ep.upload != nil {
					body = formOf(textPart("model", "secret"), textPart("prompt", "hi"), filePart("image", "a.png", "image/png", "\x89PNG"),
						filePart("file", "a.wav", "audio/wav", "RIFF"))
					header = map[string]string{"Content-Type": formContentType}
				}
				anthropic := ep.api.Format() == provider.FormatMessages

				c := call{method: "POST", path: ep.route(), key: userKey, body: body, header: header}
				w := do(t, g.h, c)
				if anthropic {
					expectAnthropicError(t, w, http.StatusNotFound, "not_found_error", "model_not_found")
				} else {
					expectError(t, w, http.StatusNotFound, "model_not_found")
				}
				if strings.Contains(w.Body.String(), "endpoints") {
					t.Errorf("the model-access refusal tells of the endpoints list: %s", w.Body.String())
				}

				c.key = workloadKey
				w = do(t, g.h, c)
				if anthropic {
					expectAnthropicError(t, w, http.StatusBadRequest, "invalid_request_error", "endpoint_not_served")
				} else {
					expectError(t, w, http.StatusBadRequest, "endpoint_not_served")
				}
				if !strings.Contains(w.Body.String(), `"message":"`+notListed(ep.route())+`"`) {
					t.Errorf("refusal %s, want the message %q", w.Body.String(), notListed(ep.route()))
				}
				if n, u := len(g.backend.Requests()), len(g.usage.all()); n != 0 || u != 0 {
					t.Fatalf("%d backend requests and %d usage records, want none", n, u)
				}

				r := listedRequests[listed]
				listedCall := call{method: "POST", path: r.path, key: workloadKey, body: r.body}
				if w := do(t, g.h, listedCall); w.Code != http.StatusOK {
					t.Fatalf("the listed endpoint after the refusal: %d %s, want the request slot free", w.Code, w.Body.String())
				}
				expectError(t, do(t, g.h, listedCall), http.StatusTooManyRequests, "rate_limit_exceeded")
			})
		}
	}
}

// A model's endpoints list follows the config applied: the refusal, /v1/models and
// the Anthropic-shaped list, which shows the model only while its endpoints include
// messages, change with each snapshot.
func TestEndpointsListFollowsTheAppliedConfig(t *testing.T) {
	g := newTestGateway(t)
	every := endpointsOf(config.BackendLlamaServer)
	for _, listed := range [][]string{every, {"embeddings"}, {"messages", "messages_count_tokens"}, every} {
		g.apply(t, func(doc string) string { return withListedOn(t, doc, config.BackendLlamaServer, listed) })
		want := slices.Sorted(slices.Values(listed))

		for _, anthropic := range []bool{false, true} {
			c := call{method: "GET", path: "/v1/models", key: workloadKey}
			if anthropic {
				c.header = map[string]string{"Anthropic-Version": "2023-06-01"}
			}
			w := do(t, g.h, c)
			var list struct {
				Data []struct {
					ID        string
					Endpoints []string
				}
			}
			if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || w.Code != http.StatusOK {
				t.Fatalf("list %v (Anthropic %t): %d (%v) %s", listed, anthropic, w.Code, err, w.Body.String())
			}
			i := slices.IndexFunc(list.Data, func(m struct {
				ID        string
				Endpoints []string
			}) bool {
				return m.ID == "open"
			})
			if shown := !anthropic || slices.Contains(want, "messages"); (i >= 0) != shown {
				t.Fatalf("list %v (Anthropic %t): open shown %t, want %t", listed, anthropic, i >= 0, shown)
			}
			if i >= 0 && !slices.Equal(list.Data[i].Endpoints, want) {
				t.Errorf("list %v (Anthropic %t): open lists %v, want %v", listed, anthropic, list.Data[i].Endpoints, want)
			}
		}

		w := do(t, g.h, call{method: "POST", path: "/v1/chat/completions", key: workloadKey,
			body: `{"model":"open","messages":[]}`})
		if slices.Contains(want, "chat_completions") {
			if w.Code != http.StatusOK {
				t.Errorf("list %v: chat %d %s, want it served", listed, w.Code, w.Body.String())
			}
		} else {
			expectError(t, w, http.StatusBadRequest, "endpoint_not_served")
			if msg := errorMessage(t, w); msg != notListed("/v1/chat/completions") {
				t.Errorf("list %v: chat refused with %q, want the list's refusal", listed, msg)
			}
		}
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
// code and parameter, never retried, neutral for the circuit; the server's answer to a
// path it does not have is 502 upstream_path_missing with the message naming both
// causes, never retried — the model's other deployments are identical instances —
// and neutral too (docs/specs/GATEWAY.md, Routing and reliability: retries, outcome
// classes; Providers → A path the server does not have).
func TestRefusalAndPathMissingClassification(t *testing.T) {
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

	missing := &provider.Error{Code: provider.CodePathMissing, Err: errors.New("no endpoint at http://x/v1/messages")}
	at = &attempt{}
	failure = upstreamFailure(context.Background(), at, missing)
	const message = "The model backend does not have this endpoint: its address is misconfigured, or its server does not serve this API for the model."
	if failure.status != http.StatusBadGateway || failure.code != "upstream_path_missing" || failure.message != message ||
		failure.class != metrics.ErrorUpstreamError {
		t.Errorf("path missing answered %+v", failure)
	}
	if reason := retriedFor(at); reason != "" {
		t.Errorf("path missing retried for %q", reason)
	}
	if outcome, class, _ := classifyAttempt(at, ""); outcome != metrics.AttemptPathMissing || class != routing.Neutral {
		t.Errorf("path missing classified %s, %v", outcome, class)
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
	withEndpointLists(t, g, func(doc string) string {
		return replaceOnce(t, doc, `"allowed_models": ["*"] }`,
			`"allowed_models": ["*"], "limits": [{ "type": "requests_per_minute", "value": 1 }] }`)
	})
	if w := do(t, g.h, call{method: "POST", path: "/v1/chat/completions", key: workloadKey, body: chatBody}); w.Code != http.StatusOK {
		t.Fatalf("first request: %d %s", w.Code, w.Body.String())
	}
	w := do(t, g.h, call{method: "POST", path: "/v1/chat/completions", key: workloadKey,
		body: `{"model":"claude-msg","messages":[]}`})
	expectError(t, w, http.StatusBadRequest, "endpoint_not_served")
}
