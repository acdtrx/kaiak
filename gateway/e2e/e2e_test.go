// Package e2e drives the built kaiak binary as a real process against the fake
// backend: the end-to-end list in docs/plans/gateway-file-mode/OVERVIEW.md.
package e2e

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"kaiak/internal/fakebackend"
)

// Public and backend model names in the test config.
const (
	backendChatModel  = "Qwen/Qwen3-8B"
	backendEmbedModel = "BAAI/bge-m3"
)

// testConfig is the config document the gateway runs with; extraModel, when set,
// adds one more chat model (the reload scenario).
func testConfig(backendURL, evalHash, annHash, extraModel string) map[string]any {
	chatMeta := map[string]any{
		"context_length": 8192,
		"capabilities":   map[string]any{"streaming": true, "tools": false, "vision": false, "reasoning": false},
	}
	chatModel := func(prices []any) map[string]any {
		m := map[string]any{
			"deployments":  []any{map[string]any{"backend": "fake", "model": backendChatModel}},
			"metadata":     chatMeta,
			"output_limit": map[string]any{"default": 64, "ceiling": 128},
		}
		if prices != nil {
			m["prices"] = prices
		}
		return m
	}
	price := func(in, out float64) []any {
		return []any{map[string]any{"effective_from": "2026-01-01", "tiers": []any{map[string]any{
			"above_input_tokens": 0, "usd_per_million": map[string]any{"tokens_in": in, "tokens_out": out}}}}}
	}
	models := map[string]any{
		"chat": chatModel(price(1, 2)),
		"rpm":  chatModel(nil),
		// One answer (7 tokens in, 4 out) costs 0.00015 USD: over the budget below.
		"priced": chatModel(price(10, 20)),
		"embed": map[string]any{
			"deployments": []any{map[string]any{"backend": "fake", "model": backendEmbedModel}},
			"metadata": map[string]any{"context_length": 512,
				"capabilities": map[string]any{"streaming": false, "tools": false, "vision": false, "reasoning": false}},
		},
	}
	if extraModel != "" {
		models[extraModel] = chatModel(nil)
	}
	return map[string]any{
		"format_version": 3,
		"global": map[string]any{
			"limits": []any{map[string]any{"type": "usd_per_month", "value": 0.0001, "models": []any{"priced"}}},
		},
		"backends": map[string]any{"fake": map[string]any{"type": "openai-compatible", "base_url": backendURL + "/v1"}},
		"models":   models,
		"groups": map[string]any{
			"research": map[string]any{},
			"eval": map[string]any{
				"parent": "research", "allowed_models": []any{"*"},
				"limits": []any{map[string]any{"type": "requests_per_minute", "value": 2, "models": []any{"rpm"}}},
			},
			"users": map[string]any{"child_defaults": map[string]any{"allowed_models": []any{"chat", "embed"}}},
			"ann":   map[string]any{"parent": "users"},
		},
		"keys": map[string]any{
			"k-eval": map[string]any{"hash": evalHash, "group": "eval"},
			"k-ann":  map[string]any{"hash": annHash, "group": "ann"},
		},
	}
}

func chatBody(model string, stream bool, extra map[string]any) map[string]any {
	body := map[string]any{"model": model, "messages": []any{map[string]any{"role": "user", "content": "Say hello."}}}
	if stream {
		body["stream"] = true
	}
	for k, v := range extra {
		body[k] = v
	}
	return body
}

func TestGatewayEndToEnd(t *testing.T) {
	backend := fakebackend.New()
	defer backend.Close()
	dir := t.TempDir()
	configFile := filepath.Join(dir, "config.json")
	dataDir := filepath.Join(dir, "data")
	evalKey, evalHash := newKey()
	annKey, annHash := newKey()
	writeJSON(t, configFile, testConfig(backend.URL(), evalHash, annHash, ""))
	// The backend lists the chat model only: the check at config apply warns about
	// the embedding deployment, and requests still go out.
	backend.SetModels(backendChatModel)

	g := startGateway(t, configFile, dataDir)

	t.Run("a deployment whose model the backend does not list is warned about", func(t *testing.T) {
		line := g.logs.wait(t, "the model check", msg("the backend does not list the deployment's model", "backend", "fake"))
		if line["deployment_model"] != backendEmbedModel {
			t.Errorf("model check line %v, want %s", line, backendEmbedModel)
		}
	})

	t.Run("auth rejects missing and unknown keys", func(t *testing.T) {
		r := g.post(t, "/v1/chat/completions", "", "", chatBody("chat", false, nil))
		if r.StatusCode != http.StatusUnauthorized || r.errorCode(t) != "missing_api_key" {
			t.Fatalf("no key: %d %s", r.StatusCode, r.body)
		}
		r = g.post(t, "/v1/chat/completions", "kaiak-not-a-key", "", chatBody("chat", false, nil))
		if r.StatusCode != http.StatusUnauthorized || r.errorCode(t) != "invalid_api_key" {
			t.Fatalf("unknown key: %d %s", r.StatusCode, r.body)
		}
	})

	t.Run("models are filtered per key", func(t *testing.T) {
		for _, c := range []struct {
			key  string
			want []string
		}{{evalKey, []string{"chat", "embed", "priced", "rpm"}}, {annKey, []string{"chat", "embed"}}} {
			status, body := g.get(t, "/v1/models", c.key)
			var list struct {
				Data []struct{ ID string }
			}
			if err := json.Unmarshal(body, &list); status != http.StatusOK || err != nil {
				t.Fatalf("/v1/models = %d %s", status, body)
			}
			var ids []string
			for _, m := range list.Data {
				ids = append(ids, m.ID)
			}
			if !slices.Equal(ids, c.want) {
				t.Errorf("/v1/models lists %v, want %v", ids, c.want)
			}
		}
		if status, body := g.get(t, "/v1/models/rpm", annKey); status != http.StatusNotFound {
			t.Errorf("/v1/models/rpm for a key without access = %d %s, want 404", status, body)
		}
	})

	t.Run("non-streamed chat", func(t *testing.T) {
		r := g.post(t, "/v1/chat/completions", evalKey, "e2e-chat", chatBody("chat", false, nil))
		if r.StatusCode != http.StatusOK {
			t.Fatalf("status %d %s", r.StatusCode, r.body)
		}
		var answer struct {
			Model   string
			Choices []struct{ Message struct{ Content string } }
		}
		if err := json.Unmarshal(r.body, &answer); err != nil || len(answer.Choices) != 1 {
			t.Fatalf("answer %s", r.body)
		}
		if answer.Model != "chat" || answer.Choices[0].Message.Content != "Hello from the fake" {
			t.Errorf("answer %s", r.body)
		}
		if got := r.Header.Get("X-Request-Id"); got != "e2e-chat" {
			t.Errorf("X-Request-Id %q", got)
		}
		upstream := lastRequest(t, backend)
		if upstream["model"] != backendChatModel || fmt.Sprint(upstream["max_completion_tokens"]) != "64" {
			t.Errorf("backend got model %v, max_completion_tokens %v", upstream["model"], upstream["max_completion_tokens"])
		}
		line := g.settled(t, "e2e-chat")
		if line["tokens_in"] != 7.0 || line["tokens_out"] != 4.0 || line["estimated"] != false || line["cost_usd"] != 0.000015 {
			t.Errorf("log line %v", line)
		}
	})

	t.Run("streamed chat withholds the usage chunk the client did not ask for", func(t *testing.T) {
		r := g.post(t, "/v1/chat/completions", evalKey, "e2e-stream", chatBody("chat", true, nil))
		if r.StatusCode != http.StatusOK || !strings.HasPrefix(r.Header.Get("Content-Type"), "text/event-stream") {
			t.Fatalf("status %d, type %q", r.StatusCode, r.Header.Get("Content-Type"))
		}
		evs := events(r.body)
		if len(evs) < 2 || evs[len(evs)-1] != "[DONE]" {
			t.Fatalf("stream does not end with [DONE]: %q", evs)
		}
		var text strings.Builder
		for _, ev := range evs[:len(evs)-1] {
			var chunk struct {
				Model   string
				Choices []struct{ Delta struct{ Content string } }
				Usage   any
			}
			if err := json.Unmarshal([]byte(ev), &chunk); err != nil {
				t.Fatalf("chunk %s: %v", ev, err)
			}
			if chunk.Model != "chat" || len(chunk.Choices) == 0 || chunk.Usage != nil {
				t.Errorf("chunk %s", ev)
			}
			for _, c := range chunk.Choices {
				text.WriteString(c.Delta.Content)
			}
		}
		if text.String() != "Hello from the fake" {
			t.Errorf("streamed text %q", text.String())
		}
		upstream := lastRequest(t, backend)
		if opts, _ := upstream["stream_options"].(map[string]any); opts["include_usage"] != true {
			t.Errorf("backend got stream_options %v", upstream["stream_options"])
		}
		if line := g.settled(t, "e2e-stream"); line["estimated"] != false || line["tokens_out"] != 4.0 {
			t.Errorf("log line %v", line)
		}
	})

	t.Run("streamed chat relays the usage chunk the client asked for", func(t *testing.T) {
		r := g.post(t, "/v1/chat/completions", evalKey, "e2e-stream-usage",
			chatBody("chat", true, map[string]any{"stream_options": map[string]any{"include_usage": true}}))
		evs := events(r.body)
		if r.StatusCode != http.StatusOK || len(evs) < 2 || evs[len(evs)-1] != "[DONE]" {
			t.Fatalf("status %d, events %q", r.StatusCode, evs)
		}
		var last struct {
			Choices []any
			Usage   struct {
				CompletionTokens int `json:"completion_tokens"`
			}
		}
		if err := json.Unmarshal([]byte(evs[len(evs)-2]), &last); err != nil || len(last.Choices) != 0 || last.Usage.CompletionTokens != 4 {
			t.Errorf("usage chunk %s", evs[len(evs)-2])
		}
		g.settled(t, "e2e-stream-usage")
	})

	t.Run("embeddings", func(t *testing.T) {
		r := g.post(t, "/v1/embeddings", annKey, "e2e-embed", map[string]any{"model": "embed", "input": "hello"})
		if r.StatusCode != http.StatusOK {
			t.Fatalf("status %d %s", r.StatusCode, r.body)
		}
		answer := r.json(t)
		if data, _ := answer["data"].([]any); answer["model"] != "embed" || len(data) != 1 {
			t.Errorf("answer %s", r.body)
		}
		if upstream := lastRequest(t, backend); upstream["model"] != backendEmbedModel {
			t.Errorf("backend got model %v", upstream["model"])
		}
		if line := g.settled(t, "e2e-embed"); line["tokens_in"] != 7.0 || line["key_id"] != "k-ann" {
			t.Errorf("log line %v", line)
		}
	})

	t.Run("requests per minute limit answers 429 with headers", func(t *testing.T) {
		for i, remaining := range []string{"1", "0"} {
			r := g.post(t, "/v1/chat/completions", evalKey, "", chatBody("rpm", false, nil))
			if r.StatusCode != http.StatusOK || r.Header.Get("X-Ratelimit-Remaining-Requests") != remaining {
				t.Fatalf("request %d: %d, remaining %q", i+1, r.StatusCode, r.Header.Get("X-Ratelimit-Remaining-Requests"))
			}
		}
		r := g.post(t, "/v1/chat/completions", evalKey, "", chatBody("rpm", false, nil))
		if r.StatusCode != http.StatusTooManyRequests || r.errorCode(t) != "rate_limit_exceeded" {
			t.Fatalf("third request: %d %s", r.StatusCode, r.body)
		}
		for name, want := range map[string]string{"Retry-After": "", "X-Ratelimit-Limit-Requests": "2",
			"X-Ratelimit-Remaining-Requests": "0", "X-Ratelimit-Reset-Requests": ""} {
			got := r.Header.Get(name)
			if got == "" || (want != "" && got != want) {
				t.Errorf("%s = %q, want %q", name, got, want)
			}
		}
	})

	t.Run("usd per month limit trips", func(t *testing.T) {
		r := g.post(t, "/v1/chat/completions", evalKey, "e2e-priced", chatBody("priced", false, nil))
		if r.StatusCode != http.StatusOK {
			t.Fatalf("first request: %d %s", r.StatusCode, r.body)
		}
		g.settled(t, "e2e-priced") // its cost is in the month's window now
		r = g.post(t, "/v1/chat/completions", evalKey, "", chatBody("priced", false, nil))
		if r.StatusCode != http.StatusTooManyRequests || r.errorCode(t) != "budget_exceeded" || r.Header.Get("Retry-After") == "" {
			t.Fatalf("second request: %d %s", r.StatusCode, r.body)
		}
	})

	t.Run("SIGHUP reload keeps a bad config out and applies a good one", func(t *testing.T) {
		if err := os.WriteFile(configFile, []byte(`{"format_version": 3,`), 0o600); err != nil {
			t.Fatal(err)
		}
		g.signal(t, syscall.SIGHUP)
		g.logs.wait(t, "the rejected reload", msg("config rejected", "trigger", "sighup", "running_config", "kept"))
		if r := g.post(t, "/v1/chat/completions", evalKey, "", chatBody("chat", false, nil)); r.StatusCode != http.StatusOK {
			t.Fatalf("chat after a rejected reload: %d %s", r.StatusCode, r.body)
		}

		writeJSON(t, configFile, testConfig(backend.URL(), evalHash, annHash, "chat-2"))
		g.signal(t, syscall.SIGHUP)
		g.logs.wait(t, "the applied reload", msg("config applied", "trigger", "sighup"))
		if status, body := g.get(t, "/v1/models/chat-2", evalKey); status != http.StatusOK {
			t.Fatalf("/v1/models/chat-2 after reload = %d %s", status, body)
		}
	})

	t.Run("metrics reflect the traffic", func(t *testing.T) {
		usage := `{key_group="eval",root_group="research",key_id="k-eval",model="chat",status="complete"}`
		for series, want := range map[string]float64{
			`kaiak_errors_total{class="auth"}`:                                                  2,
			`kaiak_errors_total{class="rate_limited"}`:                                          1,
			`kaiak_errors_total{class="budget_exceeded"}`:                                       1,
			`kaiak_config_loads_total{trigger="sighup",result="rejected"}`:                      1,
			`kaiak_config_loads_total{trigger="sighup",result="applied"}`:                       1,
			`kaiak_usage_records_total` + usage:                                                 4,
			`kaiak_usage_tokens_total` + strings.TrimSuffix(usage, "}") + `,unit="tokens_out"}`: 16,
			`kaiak_backend_in_flight_requests{backend="fake"}`:                                  0,
		} {
			if got := g.metric(t, series); got != want {
				t.Errorf("%s = %v, want %v", series, got, want)
			}
		}
		if got := g.metric(t, `kaiak_usage_cost_usd_total`+usage); got <= 0 {
			t.Errorf("chat cost %v, want > 0", got)
		}
	})

	g.stop(t)
	g.logs.wait(t, "the shutdown snapshot", msg("limits snapshot written", "trigger", "shutdown"))
	if _, err := os.Stat(filepath.Join(dataDir, "limits.json")); err != nil {
		t.Fatalf("snapshot: %v", err)
	}

	// A second process on the same data directory.
	g = startGateway(t, configFile, dataDir)

	t.Run("usage snapshot survives a restart", func(t *testing.T) {
		g.logs.wait(t, "the restored snapshot", msg("limits snapshot restored"))
		r := g.post(t, "/v1/chat/completions", evalKey, "", chatBody("priced", false, nil))
		if r.StatusCode != http.StatusTooManyRequests || r.errorCode(t) != "budget_exceeded" {
			t.Fatalf("priced after restart: %d %s", r.StatusCode, r.body)
		}
		// Per-minute windows are not kept.
		if r := g.post(t, "/v1/chat/completions", evalKey, "", chatBody("rpm", false, nil)); r.StatusCode != http.StatusOK {
			t.Fatalf("rpm after restart: %d %s", r.StatusCode, r.body)
		}
	})

	t.Run("SIGTERM drain finishes an in-flight stream", func(t *testing.T) {
		pace := make(chan struct{})
		backend.SetReply(fakebackend.Reply{Pace: pace})
		body, err := json.Marshal(chatBody("chat", true, nil))
		if err != nil {
			t.Fatal(err)
		}
		req, err := http.NewRequest(http.MethodPost, g.api+"/v1/chat/completions", strings.NewReader(string(body)))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+evalKey)
		req.Header.Set("X-Request-Id", "e2e-drain")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		stream := bufio.NewReader(resp.Body)
		if first, err := stream.ReadString('\n'); err != nil || !strings.HasPrefix(first, "data: ") {
			t.Fatalf("first event: %q %v", first, err)
		}

		g.signal(t, syscall.SIGTERM)
		g.logs.wait(t, "the drain refusing new requests", msg("draining: refusing new requests"))
		if status, _ := g.get(t, "/readyz", ""); status != http.StatusServiceUnavailable {
			t.Errorf("/readyz while draining = %d, want 503", status)
		}
		fresh := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
		if _, err := fresh.Get(g.api + "/v1/models"); !isConnRefused(err) {
			t.Errorf("new connection while draining: %v, want connection refused", err)
		}

		close(pace) // let the backend finish the stream
		rest, err := readAll(stream)
		if err != nil {
			t.Fatalf("stream broke off: %v", err)
		}
		if evs := events([]byte(rest)); len(evs) == 0 || evs[len(evs)-1] != "[DONE]" {
			t.Fatalf("stream did not end with [DONE]: %q", rest)
		}
		g.waitExit(t)
		if line := g.settled(t, "e2e-drain"); line["partial"] != false || line["relay_end"] != nil {
			t.Errorf("drained stream's log line %v", line)
		}
		g.logs.wait(t, "the drain end", func(e map[string]any) bool { return e["msg"] == "drained" && e["cut_off"] == nil })
	})
}

// lastRequest is the body of the last request the backend received.
func lastRequest(t *testing.T, b *fakebackend.Backend) map[string]any {
	t.Helper()
	reqs := b.Requests()
	if len(reqs) == 0 {
		t.Fatal("the backend received no request")
	}
	var body map[string]any
	if err := json.Unmarshal(reqs[len(reqs)-1].Body, &body); err != nil {
		t.Fatal(err)
	}
	return body
}

func readAll(r *bufio.Reader) (string, error) {
	var sb strings.Builder
	_, err := r.WriteTo(&sb)
	return sb.String(), err
}
