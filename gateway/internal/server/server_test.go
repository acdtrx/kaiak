package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"kaiak/internal/accounting"
	"kaiak/internal/config"
	"kaiak/internal/fakebackend"
	"kaiak/internal/limits"
	"kaiak/internal/metrics"
	"kaiak/internal/provider"
	"kaiak/internal/routing"
)

const (
	workloadKey = "kaiak-test-workload-key"
	userKey     = "kaiak-test-user-key"
	disabledKey = "kaiak-test-disabled-key"
	expiredKey  = "kaiak-test-expired-key"
	futureKey   = "kaiak-test-future-key"
	bodyCap     = 1024
)

func hashOf(key string) string {
	sum := sha256.Sum256([]byte(key))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Backend credentials the test gateway reads from its (fake) environment.
const (
	localBackendKey = "backend-secret-local"
	azureBackendKey = "backend-secret-azure"
)

// testSnapshot is the test config. backendURL is the root URL of the fake backend
// every reachable backend points at. Models:
//   - open, secret, Org/open-7b: backend "local" (openai-compatible), same name there
//   - renamed: backend "local", served as "vendor/renamed-7b-instruct"
//   - on-azure: backend "azure" (azure-openai), deployment "gpt-4o-deploy"
//   - slow: backend "slow", first-event, response and stall timeouts 150 ms
//   - down: backend "down", nothing listening
//   - pair: two deployments, "pair-a" on "local" and "pair-b" on "local-b" (the same
//     fake backend under another ID); declared defaults, an output limit, prices
//     (1 and 2 USD per million input and output tokens now, no tokens_cached price)
func testSnapshot(t *testing.T, backendURL string) *config.Snapshot {
	t.Helper()
	return testSnapshotWith(t, backendURL, nil)
}

// testSnapshotWith is testSnapshot with edit applied to the document first (nil = none).
func testSnapshotWith(t *testing.T, backendURL string, edit func(doc string) string) *config.Snapshot {
	t.Helper()
	return parseTestDoc(t, testDocWith(backendURL, edit))
}

func parseTestDoc(t *testing.T, doc string) *config.Snapshot {
	t.Helper()
	s, err := config.Parse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// testDocWith is the test config document, with edit applied (nil = none).
func testDocWith(backendURL string, edit func(doc string) string) string {
	model := func(name, backend, backendModel string) string {
		return `"` + name + `": {
      "deployments": [{ "backend": "` + backend + `", "model": "` + backendModel + `" }],
      "metadata": { "context_length": 8192,
        "capabilities": { "streaming": true, "tools": false, "vision": false, "reasoning": false } } }`
	}
	doc := `{
  "format_version": 3,
  "global": { "max_request_body_bytes": 1024 },
  "backends": {
    "local": { "type": "openai-compatible", "base_url": "` + backendURL + `/v1", "api_key_env": "LOCAL_KEY" },
    "azure": { "type": "azure-openai", "base_url": "` + backendURL + `", "api_key_env": "AZURE_KEY" },
    "slow": { "type": "openai-compatible", "base_url": "` + backendURL + `/v1", "first_event_timeout_ms": 150, "response_timeout_ms": 150, "stall_timeout_ms": 150 },
    "down": { "type": "openai-compatible", "base_url": "http://127.0.0.1:1/v1" },
    "local-b": { "type": "openai-compatible", "base_url": "` + backendURL + `/v1" }
  },
  "models": { ` + model("open", "local", "open") + `, ` + model("secret", "local", "secret") + `,
    ` + model("Org/open-7b", "local", "Org/open-7b") + `, ` + model("renamed", "local", "vendor/renamed-7b-instruct") + `,
    ` + model("on-azure", "azure", "gpt-4o-deploy") + `, ` + model("slow", "slow", "slow") + `,
    ` + model("down", "down", "down") + `,
    "pair": {
      "deployments": [{ "backend": "local", "model": "pair-a" }, { "backend": "local-b", "model": "pair-b" }],
      "metadata": { "context_length": 32768, "reasoning_efforts": ["low", "high"],
        "capabilities": { "streaming": true, "tools": true, "vision": false, "reasoning": true } },
      "defaults": { "top_k": [1, 2], "temperature": 0.2, "chat_template_kwargs": { "enable_thinking": false } },
      "output_limit": { "default": 256, "ceiling": 1024 },
      "prices": [
        { "effective_from": "2020-01-01",
          "tiers": [{ "above_input_tokens": 0, "usd_per_million": { "tokens_in": 1, "tokens_out": 2 } }] },
        { "effective_from": "2999-01-01",
          "tiers": [{ "above_input_tokens": 0, "usd_per_million": { "tokens_in": 100, "tokens_out": 200 } }] } ] } },
  "groups": {
    "research": {},
    "eval": { "parent": "research", "allowed_models": ["*"] },
    "users": { "child_defaults": { "allowed_models": ["open", "Org/open-7b"] } },
    "ann": { "parent": "users" }
  },
  "keys": {
    "k-eval": { "hash": "` + hashOf(workloadKey) + `", "group": "eval" },
    "k-ann": { "hash": "` + hashOf(userKey) + `", "group": "ann" },
    "k-off": { "hash": "` + hashOf(disabledKey) + `", "group": "ann", "disabled": true },
    "k-old": { "hash": "` + hashOf(expiredKey) + `", "group": "ann", "expires_at": "2020-01-01T00:00:00Z" },
    "k-new": { "hash": "` + hashOf(futureKey) + `", "group": "ann", "expires_at": "2999-01-01T00:00:00Z" }
  }
}`
	if edit != nil {
		doc = edit(doc)
	}
	return doc
}

// testGateway is the API handler over the test snapshot, its log, and the fake
// backend behind it.
type testGateway struct {
	h       http.Handler
	logs    *bytes.Buffer
	log     *lockedWriter
	backend *fakebackend.Backend
	holder  *config.Holder
	router  *routing.Router
	limiter *limits.Limiter
	usage   *usageSink
	metrics *metrics.Registry
	drain   *Drain
	bodies  *BodyBudget
}

// usageSink keeps every usage record the gateway settles. Handlers served by a real
// test server settle on their own goroutines, after the client may have seen the end
// of the response: such tests wait for the record on settled.
type usageSink struct {
	mu      sync.Mutex
	records []accounting.UsageRecord
	settled chan accounting.UsageRecord
}

func (s *usageSink) Record(r accounting.UsageRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records = append(s.records, r)
	select {
	case s.settled <- r:
	default:
	}
}

func (s *usageSink) all() []accounting.UsageRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]accounting.UsageRecord(nil), s.records...)
}

// logText reads the log safely while a real test server may still be writing it.
func (g *testGateway) logText() string {
	g.log.mu.Lock()
	defer g.log.mu.Unlock()
	return g.logs.String()
}

func newTestGateway(t *testing.T) *testGateway {
	t.Helper()
	return newTestGatewayWith(t, func(h *config.Holder) *limits.Limiter { return limits.New(h, time.Now, nil) })
}

// newTestGatewayWith is newTestGateway with the limiter newLimiter builds.
func newTestGatewayWith(t *testing.T, newLimiter func(*config.Holder) *limits.Limiter) *testGateway {
	t.Helper()
	return buildTestGateway(t, newLimiter, NewBodyBudget(DefaultBodyMemory))
}

// newTestGatewayBodies is newTestGateway holding request bodies within a budget of
// bodyMemory bytes.
func newTestGatewayBodies(t *testing.T, bodyMemory int64) *testGateway {
	t.Helper()
	return buildTestGateway(t, func(h *config.Holder) *limits.Limiter { return limits.New(h, time.Now, nil) },
		NewBodyBudget(bodyMemory))
}

func buildTestGateway(t *testing.T, newLimiter func(*config.Holder) *limits.Limiter, bodies *BodyBudget) *testGateway {
	t.Helper()
	backend := fakebackend.New()
	t.Cleanup(backend.Close)
	holder := &config.Holder{}
	holder.Swap(testSnapshot(t, backend.URL()))
	env := map[string]string{"LOCAL_KEY": localBackendKey, "AZURE_KEY": azureBackendKey}
	lookupEnv := func(name string) (string, bool) { v, ok := env[name]; return v, ok }
	var logs bytes.Buffer
	log := &lockedWriter{w: &logs}
	logger := slog.New(slog.NewJSONHandler(log, nil))
	providers := provider.NewRegistry(lookupEnv)
	usage := &usageSink{settled: make(chan accounting.UsageRecord, 64)}
	reg := metrics.NewRegistry()
	router := routing.New(routing.Options{Probe: providers.Probe, Observer: metrics.NewCircuits(reg), Logger: logger})
	usageMetrics := metrics.NewUsageSink(reg, holder)
	recorder := accounting.NewRecorder(accounting.RecorderOptions{Instance: "gw-test",
		Sink: accounting.Fanout{usage, usageMetrics}, Logger: logger, OutOfRange: usageMetrics.RecordClamped})
	limiter := newLimiter(holder)
	drain := NewDrain()
	h := NewAPI(holder, drain, bodies, providers, limiter, router, recorder, metrics.NewOps(reg, router, holder), logger)
	return &testGateway{h: h, logs: &logs, log: log, backend: backend, holder: holder, router: router,
		limiter: limiter, usage: usage, metrics: reg, drain: drain, bodies: bodies}
}

// testAPI returns the API handler over the test snapshot and a buffer of its log.
func testAPI(t *testing.T) (http.Handler, *bytes.Buffer) {
	t.Helper()
	g := newTestGateway(t)
	return g.h, g.logs
}

// lockedWriter serializes writes: handlers served by a real test server log from
// their own goroutines.
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

type call struct {
	method, path, key, body string
	header                  map[string]string
}

func do(t *testing.T, h http.Handler, c call) *httptest.ResponseRecorder {
	t.Helper()
	var body io.Reader
	if c.body != "" {
		body = strings.NewReader(c.body)
	}
	r := httptest.NewRequest(c.method, c.path, body)
	if c.key != "" {
		r.Header.Set("Authorization", "Bearer "+c.key)
	}
	for k, v := range c.header {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// openAIError decodes an OpenAI error body, failing on any other shape.
func openAIError(t *testing.T, w *httptest.ResponseRecorder) (typ, code string, param *string) {
	t.Helper()
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type %q, want application/json", ct)
	}
	var body struct {
		Error struct {
			Message *string `json:"message"`
			Type    string  `json:"type"`
			Param   *string `json:"param"`
			Code    string  `json:"code"`
		} `json:"error"`
	}
	dec := json.NewDecoder(bytes.NewReader(w.Body.Bytes()))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		t.Fatalf("not an OpenAI error body: %v\n%s", err, w.Body.String())
	}
	if body.Error.Message == nil || *body.Error.Message == "" || !bytes.Contains(w.Body.Bytes(), []byte(`"param":`)) {
		t.Errorf("error body misses message or param: %s", w.Body.String())
	}
	return body.Error.Type, body.Error.Code, body.Error.Param
}

func expectError(t *testing.T, w *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status %d, want %d: %s", w.Code, status, w.Body.String())
	}
	if _, got, _ := openAIError(t, w); got != code {
		t.Errorf("code %q, want %q", got, code)
	}
}

const chatBody = `{"model":"open","messages":[{"role":"user","content":"hello there"}]}`

func TestAuthFailuresAre401(t *testing.T) {
	h, _ := testAPI(t)
	cases := []struct {
		name          string
		authorization string
		code          string
	}{
		{"missing", "", "missing_api_key"},
		{"not bearer", "Basic " + userKey, "invalid_api_key"},
		{"empty bearer", "Bearer ", "invalid_api_key"},
		{"unknown", "Bearer kaiak-unknown", "invalid_api_key"},
		{"disabled", "Bearer " + disabledKey, "invalid_api_key"},
		{"expired", "Bearer " + expiredKey, "invalid_api_key"},
	}
	for _, c := range cases {
		for _, target := range []call{
			{method: "POST", path: "/v1/chat/completions", body: chatBody},
			{method: "GET", path: "/v1/models"},
			{method: "GET", path: "/v1/models/open/props"},
		} {
			t.Run(c.name+" "+target.path, func(t *testing.T) {
				target.header = map[string]string{"Authorization": c.authorization}
				w := do(t, h, target)
				expectError(t, w, http.StatusUnauthorized, c.code)
				if c.name == "missing" && !strings.Contains(w.Body.String(), "Bearer <key>") {
					t.Errorf("message HTML-escaped: %s", w.Body.String())
				}
				if w.Header().Get("WWW-Authenticate") != "Bearer" {
					t.Error("401 without WWW-Authenticate: Bearer")
				}
			})
		}
	}
}

func TestValidKeysPassAuth(t *testing.T) {
	h, _ := testAPI(t)
	for _, key := range []string{workloadKey, userKey, futureKey} {
		w := do(t, h, call{method: "POST", path: "/v1/chat/completions", key: key, body: chatBody})
		if w.Code != http.StatusOK {
			t.Errorf("status %d, want 200: %s", w.Code, w.Body.String())
		}
	}
}

func TestEndpointsRunThePipeline(t *testing.T) {
	h, _ := testAPI(t)
	for _, c := range []call{
		{method: "POST", path: "/v1/chat/completions", body: chatBody},
		{method: "POST", path: "/v1/completions", body: `{"model":"open","prompt":"hi"}`},
		{method: "POST", path: "/v1/embeddings", body: `{"model":"open","input":"hi"}`},
		{method: "GET", path: "/v1/models"},
		{method: "GET", path: "/v1/models/open"},
		{method: "GET", path: "/v1/models/open/props"},
		{method: "GET", path: "/v1/models/Org/open-7b"},
		{method: "GET", path: "/v1/models/Org/open-7b/props"},
	} {
		t.Run(c.method+" "+c.path, func(t *testing.T) {
			c.key = userKey
			w := do(t, h, c)
			if w.Code != http.StatusOK {
				t.Errorf("status %d, want 200: %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestUnknownAndNotAllowedModelsLookTheSame(t *testing.T) {
	h, _ := testAPI(t)
	// "secret" exists but ann may not use it; "ghost" does not exist. Same length names,
	// so the bodies must be byte-identical once the name is swapped.
	bodies := map[string][]byte{}
	for _, name := range []string{"secret", "ghost1"} {
		w := do(t, h, call{method: "POST", path: "/v1/chat/completions", key: userKey,
			body: `{"model":"` + name + `","messages":[]}`})
		expectError(t, w, http.StatusNotFound, "model_not_found")
		bodies[name] = bytes.ReplaceAll(w.Body.Bytes(), []byte(name), []byte("MODEL"))

		for _, path := range []string{"/v1/models/" + name, "/v1/models/" + name + "/props"} {
			w := do(t, h, call{method: "GET", path: path, key: userKey})
			expectError(t, w, http.StatusNotFound, "model_not_found")
			if got := bytes.ReplaceAll(w.Body.Bytes(), []byte(name), []byte("MODEL")); !bytes.Equal(got, bodies[name]) {
				t.Errorf("%s answered differently from the POST:\n%s\n%s", path, got, bodies[name])
			}
		}
	}
	if !bytes.Equal(bodies["secret"], bodies["ghost1"]) {
		t.Errorf("not allowed and unknown differ:\n%s\n%s", bodies["secret"], bodies["ghost1"])
	}
	// The workload may use every model, including secret.
	w := do(t, h, call{method: "POST", path: "/v1/chat/completions", key: workloadKey,
		body: `{"model":"secret","messages":[]}`})
	if w.Code != http.StatusOK {
		t.Errorf("workload on secret: status %d, want 200", w.Code)
	}
}

func TestBodyCap(t *testing.T) {
	h, _ := testAPI(t)
	big := `{"model":"open","prompt":"` + strings.Repeat("x", bodyCap) + `"}`

	w := do(t, h, call{method: "POST", path: "/v1/completions", key: userKey, body: big})
	expectError(t, w, http.StatusRequestEntityTooLarge, "request_too_large")

	// Without a Content-Length the cap still holds while reading.
	r := httptest.NewRequest("POST", "/v1/completions", io.MultiReader(strings.NewReader(big)))
	r.ContentLength = -1
	r.Header.Set("Authorization", "Bearer "+userKey)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	expectError(t, rec, http.StatusRequestEntityTooLarge, "request_too_large")

	exact := `{"model":"open","prompt":"` + strings.Repeat("x", bodyCap-len(`{"model":"open","prompt":""}`)) + `"}`
	w = do(t, h, call{method: "POST", path: "/v1/completions", key: userKey, body: exact})
	if w.Code != http.StatusOK {
		t.Errorf("body at the cap: status %d, want 200", w.Code)
	}
}

func TestOversizeBodyFromUnauthenticatedClientIsRefusedByAuth(t *testing.T) {
	h, _ := testAPI(t)
	big := `{"model":"open","prompt":"` + strings.Repeat("x", bodyCap) + `"}`
	w := do(t, h, call{method: "POST", path: "/v1/completions", body: big})
	expectError(t, w, http.StatusUnauthorized, "missing_api_key")
}

func TestInboundValidation(t *testing.T) {
	h, _ := testAPI(t)
	cases := []struct {
		name, body, code, param string
	}{
		{"not json", `{"model":`, "invalid_json", ""},
		{"array", `[1,2]`, "invalid_json", ""},
		{"null", `null`, "invalid_json", ""},
		{"no model", `{"messages":[]}`, "missing_required_parameter", "model"},
		{"empty model", `{"model":""}`, "missing_required_parameter", "model"},
		{"model case differs", `{"Model":"open"}`, "missing_required_parameter", "model"},
		{"model not string", `{"model":7}`, "invalid_type", "model"},
		{"stream not bool", `{"model":"open","stream":"yes"}`, "invalid_type", "stream"},
		{"n not an integer", `{"model":"open","n":1.5}`, "invalid_type", "n"},
		{"max_tokens fractional", `{"model":"open","max_tokens":1.5}`, "invalid_type", "max_tokens"},
		{"max_completion_tokens string", `{"model":"open","max_completion_tokens":"9"}`, "invalid_type", "max_completion_tokens"},
		{"stream_options not object", `{"model":"open","stream_options":true}`, "invalid_type", "stream_options"},
		{"include_usage not bool", `{"model":"open","stream_options":{"include_usage":1}}`, "invalid_type", "stream_options.include_usage"},
		{"model twice", `{"model":"open","messages":[],"model":"open"}`, "duplicate_member", "model"},
		{"unowned member twice", `{"model":"open","messages":[],"messages":[]}`, "duplicate_member", "messages"},
		{"include_usage twice", `{"model":"open","stream":true,"stream_options":{"include_usage":false,"include_usage":true}}`, "duplicate_member", "stream_options.include_usage"},
		{"escaped spelling of the same name", `{"model":"open","mod\u0065l":"open"}`, "duplicate_member", "model"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := do(t, h, call{method: "POST", path: "/v1/chat/completions", key: userKey, body: c.body})
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status %d, want 400: %s", w.Code, w.Body.String())
			}
			_, code, param := openAIError(t, w)
			if code != c.code {
				t.Errorf("code %q, want %q", code, c.code)
			}
			if (param == nil && c.param != "") || (param != nil && *param != c.param) {
				t.Errorf("param %v, want %q", param, c.param)
			}
		})
	}
}

func TestParseOwnedFieldsKeepsTheRawBody(t *testing.T) {
	body := `{"model":"open","stream":true,"max_tokens":null,"max_completion_tokens":64,` +
		`"stream_options":{"include_usage":true},"vendor_x":{"keep":[1,2]}}`
	rq := &request{body: []byte(body), snapshot: &config.Snapshot{MaxN: config.DefaultMaxN}}
	if err := parseOwnedFields(rq); err != nil {
		t.Fatal(err)
	}
	if rq.model != "open" || !rq.inbound.Stream || rq.inbound.MaxTokens != nil ||
		rq.inbound.MaxCompletionTokens == nil || *rq.inbound.MaxCompletionTokens != 64 || !rq.inbound.IncludeUsage {
		t.Errorf("parsed %q %+v", rq.model, rq.inbound)
	}
	if string(rq.body) != body {
		t.Error("raw body changed")
	}
}

func TestRequestID(t *testing.T) {
	h, _ := testAPI(t)

	w := do(t, h, call{method: "GET", path: "/v1/models", key: userKey,
		header: map[string]string{"X-Request-Id": "client-id_1.2:3"}})
	if got := w.Header().Get("X-Request-Id"); got != "client-id_1.2:3" {
		t.Errorf("valid client ID not echoed: %q", got)
	}

	seen := map[string]bool{}
	for _, bad := range []string{"", "has space", "semi;colon", strings.Repeat("a", 129), "ünïcode"} {
		w := do(t, h, call{method: "GET", path: "/v1/models", key: userKey,
			header: map[string]string{"X-Request-Id": bad}})
		got := w.Header().Get("X-Request-Id")
		if got == bad || !validRequestID(got) || len(got) != 32 || seen[got] {
			t.Errorf("for client ID %q got %q, want a fresh generated ID", bad, got)
		}
		seen[got] = true
	}
	if got := do(t, h, call{method: "GET", path: "/nope"}).Header().Get("X-Request-Id"); got == "" {
		t.Error("refused request has no request ID")
	}
	if !validRequestID(strings.Repeat("a", 128)) {
		t.Error("128-character ID rejected")
	}
}

func TestLogLineCarriesKeyIDNeverKeyOrContent(t *testing.T) {
	h, logs := testAPI(t)
	secret := "top-secret-prompt-words"
	do(t, h, call{method: "POST", path: "/v1/chat/completions", key: userKey,
		header: map[string]string{"X-Request-Id": "req-1"},
		body:   `{"model":"open","messages":[{"role":"user","content":"` + secret + `"}]}`})
	do(t, h, call{method: "POST", path: "/v1/chat/completions", key: expiredKey, body: chatBody})
	do(t, h, call{method: "POST", path: "/v1/chat/completions", key: "kaiak-unknown-key", body: chatBody})

	out := logs.String()
	for _, forbidden := range []string{userKey, expiredKey, "kaiak-unknown-key", secret, "Bearer", "hello there"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("log contains %q:\n%s", forbidden, out)
		}
	}

	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 {
		t.Fatalf("%d log lines, want one per request:\n%s", len(lines), out)
	}
	var first, expired, unknown map[string]any
	for i, into := range []*map[string]any{&first, &expired, &unknown} {
		if err := json.Unmarshal([]byte(lines[i]), into); err != nil {
			t.Fatal(err)
		}
	}
	want := map[string]any{"msg": "request", "request_id": "req-1", "key_id": "k-ann", "model": "open",
		"status": float64(200), "method": "POST", "path": "/v1/chat/completions",
		"backend": "local", "deployment_model": "open"}
	for k, v := range want {
		if first[k] != v {
			t.Errorf("log field %s = %v, want %v", k, first[k], v)
		}
	}
	if _, ok := first["latency_ms"].(float64); !ok {
		t.Errorf("log misses latency_ms: %v", first)
	}
	if expired["key_id"] != "k-old" || expired["auth_failure"] != "expired_key" || expired["status"] != float64(401) {
		t.Errorf("expired key logged as %v", expired)
	}
	if _, ok := unknown["key_id"]; ok || unknown["auth_failure"] != "unknown_key" {
		t.Errorf("unknown key logged as %v", unknown)
	}
}

func TestUnknownPathsAndMethods(t *testing.T) {
	h, logs := testAPI(t)
	for _, path := range []string{"/", "/v1/nope", "/v2/chat/completions", "/v1/models/", "/healthz", "/readyz", "/metrics"} {
		w := do(t, h, call{method: "GET", path: path, key: userKey})
		expectError(t, w, http.StatusNotFound, "unknown_url")
	}
	for _, c := range []struct{ method, path, allow string }{
		{"GET", "/v1/chat/completions", "POST"},
		{"PUT", "/v1/embeddings", "POST"},
		{"POST", "/v1/models", "GET, HEAD"},
		{"DELETE", "/v1/models/open", "GET, HEAD"},
	} {
		w := do(t, h, call{method: c.method, path: c.path, key: userKey})
		expectError(t, w, http.StatusMethodNotAllowed, "method_not_allowed")
		if got := w.Header().Get("Allow"); got != c.allow {
			t.Errorf("%s %s: Allow %q, want %q", c.method, c.path, got, c.allow)
		}
	}
	if n := strings.Count(logs.String(), `"msg":"request"`); n != 11 {
		t.Errorf("%d request log lines, want 11", n)
	}
}

func TestAdmin(t *testing.T) {
	holder := &config.Holder{}
	h := NewAdmin(holder, NewDrain(), metrics.NewRegistry(), "")
	get := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		return w
	}

	if w := get("/healthz"); w.Code != http.StatusOK {
		t.Errorf("/healthz %d, want 200", w.Code)
	}
	if w := get("/readyz"); w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "config not loaded") {
		t.Errorf("/readyz before load: %d %q", w.Code, w.Body.String())
	}
	holder.Swap(testSnapshot(t, "http://127.0.0.1:1"))
	if w := get("/readyz"); w.Code != http.StatusOK {
		t.Errorf("/readyz after load: %d, want 200", w.Code)
	}
	for _, path := range []string{"/v1/models", "/v1/chat/completions"} {
		if w := get(path); w.Code != http.StatusNotFound {
			t.Errorf("admin serves %s: %d", path, w.Code)
		}
	}
}

func TestListenerServesAndStops(t *testing.T) {
	l, err := Listen("admin", "127.0.0.1:0", NewAdmin(&config.Holder{}, NewDrain(), metrics.NewRegistry(), ""), DefaultClientTimeouts, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- l.Serve() }()

	resp, err := http.Get("http://" + l.Addr().String() + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("/healthz %d", resp.StatusCode)
	}

	l.Shutdown(time.Second)
	if err := <-done; err != nil {
		t.Errorf("Serve returned %v, want nil", err)
	}
	if _, err := http.Get("http://" + l.Addr().String() + "/healthz"); err == nil {
		t.Error("listener still accepts after shutdown")
	}
}

func TestRequestsWithNoConfigAnswer503(t *testing.T) {
	g := newTestGateway(t)
	g.holder.Swap(nil) // control-plane mode before any config is in force
	for _, c := range []call{
		{method: http.MethodPost, path: "/v1/chat/completions", key: "any", body: `{"model":"x"}`},
		{method: http.MethodGet, path: "/v1/models", key: "any"},
	} {
		w := do(t, g.h, c)
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s %s: status %d, want 503", c.method, c.path, w.Code)
		}
		if typ, code, _ := openAIError(t, w); typ != "server_error" || code != "config_not_loaded" {
			t.Errorf("%s %s: type %q code %q", c.method, c.path, typ, code)
		}
	}
	if len(g.backend.Requests()) != 0 {
		t.Error("a request reached the backend with no config")
	}
}

// The independent audit's finding 2: 10 000 repeated "model" members were each
// rewritten to the deployment's name, far past the body budget. A repeated top-level
// member is refused before any rewrite; the backend sees nothing.
func TestRepeatedTopLevelMemberIsRefusedBeforeAnyRewrite(t *testing.T) {
	g := newTestGateway(t)
	g.holder.Swap(testSnapshotWith(t, g.backend.URL(), func(doc string) string {
		return strings.Replace(doc, `"max_request_body_bytes": 1024`, `"max_request_body_bytes": 1048576`, 1)
	}))
	body := `{` + strings.Repeat(`"model":"renamed",`, 10000) + `"messages":[]}`
	w := do(t, g.h, call{method: "POST", path: "/v1/chat/completions", key: workloadKey, body: body})
	expectError(t, w, http.StatusBadRequest, "duplicate_member")
	if typ, _, param := openAIError(t, w); typ != "invalid_request_error" || param == nil || *param != "model" {
		t.Errorf("type %q, param %v, want invalid_request_error on model", typ, param)
	}
	if n := len(g.backend.Requests()); n != 0 {
		t.Errorf("backend got %d requests, want none", n)
	}
}

// A duplicate below the top level is the backend's business: passthrough preserves
// what the gateway does not own.
func TestNestedDuplicateMembersPassThrough(t *testing.T) {
	g := newTestGateway(t)
	body := `{"model":"open","messages":[{"role":"user","content":"a","content":"b"}]}`
	w := do(t, g.h, call{method: "POST", path: "/v1/chat/completions", key: workloadKey, body: body})
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	reqs := g.backend.Requests()
	if len(reqs) != 1 || !strings.Contains(string(reqs[0].Body), `"content":"a","content":"b"`) {
		t.Errorf("backend requests %d, body not passed through", len(reqs))
	}
}

// Model access intersects down the key's path: research allows open, pair and
// secret, its child eval allows open and Org/open-7b, so eval's key may use open
// only — in the listing and on a request alike. With no restricting level anywhere
// on the path, every model is allowed.
func TestModelAccessIntersectsDownThePath(t *testing.T) {
	g := newTestGateway(t)
	g.holder.Swap(testSnapshotWith(t, g.backend.URL(), func(doc string) string {
		doc = strings.Replace(doc, `"research": {}`, `"research": { "allowed_models": ["open", "pair", "secret"] }`, 1)
		return strings.Replace(doc, `"allowed_models": ["*"] }`, `"allowed_models": ["open", "Org/open-7b"] }`, 1)
	}))
	w := do(t, g.h, call{method: "GET", path: "/v1/models", key: workloadKey})
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || len(list.Data) != 1 || list.Data[0].ID != "open" {
		t.Errorf("listing %s, want open alone", w.Body.String())
	}
	for _, model := range []string{"pair", "Org/open-7b"} {
		expectError(t, do(t, g.h, call{method: "POST", path: "/v1/chat/completions", key: workloadKey,
			body: `{"model":"` + model + `","messages":[]}`}), http.StatusNotFound, "model_not_found")
	}
	if w := do(t, g.h, call{method: "POST", path: "/v1/chat/completions", key: workloadKey, body: chatBody}); w.Code != http.StatusOK {
		t.Errorf("open: status %d", w.Code)
	}

	// Neither research nor eval restricts: every model.
	g.holder.Swap(testSnapshotWith(t, g.backend.URL(), func(doc string) string {
		return strings.Replace(doc, `"eval": { "parent": "research", "allowed_models": ["*"] }`, `"eval": { "parent": "research" }`, 1)
	}))
	if w := do(t, g.h, call{method: "POST", path: "/v1/chat/completions", key: workloadKey,
		body: `{"model":"secret","messages":[]}`}); w.Code != http.StatusOK {
		t.Errorf("secret with no restriction on the path: status %d", w.Code)
	}
}
