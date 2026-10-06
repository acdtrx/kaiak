package e2e

// Anthropic Messages end to end (docs/specs/GATEWAY.md, Client API → Client APIs):
// one gateway passes Messages requests through to a vllm, an anthropic and an
// azure-anthropic backend — all on one fake backend — each at its module's URL with
// its credential, streamed and not; the usage settles from Anthropic's report; the
// gateway's own answers take Anthropic's shape; and a server lacking the endpoint is
// failed over without opening its circuit.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"kaiak/internal/fakebackend"
)

// Environment variables holding the Anthropic backends' keys.
const (
	anthropicKeyEnv      = "E2E_ANTHROPIC_API_KEY"
	azureAnthropicKeyEnv = "E2E_AZURE_ANTHROPIC_API_KEY"
)

// messagesBackends are the scenario's backends serving Messages: name, type, the
// public model deployed on it alone, the backend-side model, and what its requests
// carry.
var messagesBackends = []struct {
	name, typ, model, deployed, path, header, credential string
	standardOnly                                         bool
}{
	{name: "vl", typ: "vllm", model: "msg-vllm", deployed: "Qwen/Qwen3-8B", path: "/v1/messages"},
	{name: "claude", typ: "anthropic", model: "msg-claude", deployed: "claude-haiku-4-5",
		path: "/v1/messages", header: "X-Api-Key", credential: "sk-ant-e2e", standardOnly: true},
	{name: "foundry", typ: "azure-anthropic", model: "msg-foundry", deployed: "claude-haiku-4-5-dep",
		path: "/anthropic/v1/messages", header: "Api-Key", credential: "e2e-foundry"},
}

// messagesConfig is a config with the messagesBackends, an openai-compatible backend
// "oc" serving "chat-only", a vllm backend "old" whose server lacks the endpoint and
// a model on it and on "vl" ("pair"), and a token limit on the key's group.
func messagesConfig(backendURL, oldURL, hash string) map[string]any {
	backends := map[string]any{
		"oc":  map[string]any{"type": "openai-compatible", "base_url": backendURL + "/v1"},
		"old": map[string]any{"type": "vllm", "base_url": oldURL + "/v1"},
	}
	meta := map[string]any{"context_length": 8192,
		"capabilities": map[string]any{"streaming": true, "tools": true, "vision": false, "reasoning": false}}
	model := func(deployments ...map[string]any) map[string]any {
		ds := []any{}
		for _, d := range deployments {
			ds = append(ds, d)
		}
		return map[string]any{"deployments": ds, "metadata": meta, "output_limit": map[string]any{"default": 64, "ceiling": 128}}
	}
	models := map[string]any{
		"chat-only": model(map[string]any{"backend": "oc", "model": "chat-only"}),
		"pair": model(map[string]any{"backend": "old", "model": "Qwen/Qwen3-8B"},
			map[string]any{"backend": "vl", "model": "Qwen/Qwen3-8B"}),
		"rpm": model(map[string]any{"backend": "vl", "model": "Qwen/Qwen3-8B"}),
	}
	for _, b := range messagesBackends {
		entry := map[string]any{"type": b.typ, "base_url": backendURL + "/v1"}
		switch b.typ {
		case "anthropic":
			entry["api_key_env"] = anthropicKeyEnv
		case "azure-anthropic":
			entry["base_url"] = backendURL // the resource endpoint: the module adds /anthropic/v1/
			entry["api_key_env"] = azureAnthropicKeyEnv
		}
		backends[b.name] = entry
		models[b.model] = model(map[string]any{"backend": b.name, "model": b.deployed})
	}
	return map[string]any{
		"format_version": 5,
		"global":         map[string]any{},
		"backends":       backends,
		"models":         models,
		"groups": map[string]any{
			"w":     map[string]any{"allowed_models": []any{"*"}},
			"tight": map[string]any{"allowed_models": []any{"rpm"}, "limits": []any{map[string]any{"type": "tokens_per_minute", "value": 10}}},
		},
		"keys": map[string]any{
			"k":       map[string]any{"hash": hash, "group": "w"},
			"k-tight": map[string]any{"hash": tightHash, "group": "tight"},
		},
	}
}

// The tight group's key.
var tightKey, tightHash = newKey()

// postMessages sends a Messages request the way Anthropic's SDKs do: the key in
// x-api-key, an anthropic-version header.
func (g *gateway) postMessages(t *testing.T, path, key, requestID string, body any) *response {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, g.api+path, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Key", key)
	req.Header.Set("Anthropic-Version", "2023-06-01")
	req.Header.Set("X-Request-Id", requestID)
	return do(t, req)
}

// anthropicErrorOf is an Anthropic-shaped error body's type and kaiak's code.
func anthropicErrorOf(t *testing.T, r *response) (typ, code string) {
	t.Helper()
	body := r.json(t)
	e, _ := body["error"].(map[string]any)
	if body["type"] != "error" || e == nil {
		t.Fatalf("not an Anthropic error: %s", r.body)
	}
	return fmt.Sprint(e["type"]), fmt.Sprint(e["code"])
}

func TestMessages(t *testing.T) {
	backend := fakebackend.New()
	defer backend.Close()
	old := fakebackend.New()
	defer old.Close()
	old.SetReply(fakebackend.Reply{Status: http.StatusNotFound, Body: `{"detail":"Not Found"}`})
	backend.SetReply(fakebackend.Reply{Usage: &fakebackend.Usage{PromptTokens: 30, CompletionTokens: 4, CachedTokens: 20}})
	dir := t.TempDir()
	key, hash := newKey()
	configFile := filepath.Join(dir, "config.json")
	writeJSON(t, configFile, messagesConfig(backend.URL(), old.URL(), hash))
	g := startGatewayEnv(t, append(gatewayEnv(configFile, ""),
		anthropicKeyEnv+"=sk-ant-e2e", azureAnthropicKeyEnv+"=e2e-foundry"))

	for _, b := range messagesBackends {
		for _, stream := range []bool{false, true} {
			name := fmt.Sprintf("%s/stream=%v", b.typ, stream)
			t.Run(name, func(t *testing.T) {
				id := fmt.Sprintf("%s-%v", b.name, stream)
				before := len(backend.Requests())
				r := g.postMessages(t, "/v1/messages", key, id, map[string]any{"model": b.model, "max_tokens": 32,
					"stream": stream, "service_tier": "auto", "messages": []any{map[string]any{"role": "user", "content": "hi"}}})
				if r.StatusCode != http.StatusOK {
					t.Fatalf("%d %s", r.StatusCode, r.body)
				}
				if !strings.Contains(string(r.body), `"model":"`+b.model+`"`) || strings.Contains(string(r.body), b.deployed) {
					t.Errorf("answer does not carry the public model name:\n%s", r.body)
				}
				if stream && !strings.Contains(string(r.body), "event: message_stop") {
					t.Errorf("stream:\n%s", r.body)
				}
				line := g.settled(t, id)
				if line["gen_ai.usage.input_tokens"] != 30.0 || line["gen_ai.usage.cache_read.input_tokens"] != 20.0 ||
					line["gen_ai.usage.output_tokens"] != 4.0 || line["kaiak.usage.estimated"] != false ||
					line["kaiak.usage.partial"] != false || line["gen_ai.operation.name"] != "chat" ||
					line["kaiak.backend.id"] != b.name {
					t.Errorf("log line %v", line)
				}
				reqs := backend.Requests()
				if len(reqs) != before+1 {
					t.Fatalf("backend got %d requests, want 1", len(reqs)-before)
				}
				got := reqs[len(reqs)-1]
				var sent map[string]any
				if err := json.Unmarshal(got.Body, &sent); err != nil {
					t.Fatal(err)
				}
				if got.Path != b.path || sent["model"] != b.deployed {
					t.Errorf("backend got %s for model %v, want %s for %s", got.Path, sent["model"], b.path, b.deployed)
				}
				if b.header != "" && got.Header.Get(b.header) != b.credential {
					t.Errorf("%s header %q, want %q", b.header, got.Header.Get(b.header), b.credential)
				}
				if got.Header.Get("X-Api-Key") == key || got.Header.Get("Authorization") != "" {
					t.Error("the client's key reached the backend")
				}
				if (b.typ == "vllm") != (got.Header.Get("Anthropic-Version") == "") {
					t.Errorf("anthropic-version %q on a %s backend", got.Header.Get("Anthropic-Version"), b.typ)
				}
				if want := map[bool]any{true: "standard_only", false: "auto"}[b.standardOnly]; sent["service_tier"] != want {
					t.Errorf("service_tier sent %v, want %v", sent["service_tier"], want)
				}
			})
		}
	}

	t.Run("count_tokens", func(t *testing.T) {
		r := g.postMessages(t, "/v1/messages/count_tokens", key, "count", map[string]any{"model": "msg-claude",
			"messages": []any{map[string]any{"role": "user", "content": "hi"}}})
		if r.StatusCode != http.StatusOK || !strings.Contains(string(r.body), `"input_tokens":30`) {
			t.Fatalf("%d %s", r.StatusCode, r.body)
		}
		if line := g.settled(t, "count"); line["gen_ai.usage.input_tokens"] != nil {
			t.Errorf("a token-counting request settled usage: %v", line)
		}
	})

	t.Run("refusals in Anthropic's shape", func(t *testing.T) {
		for _, c := range []struct {
			name, key, typ, code string
			status               int
			body                 map[string]any
		}{
			{"not served", key, "invalid_request_error", "endpoint_not_served", http.StatusBadRequest,
				map[string]any{"model": "chat-only", "max_tokens": 8, "messages": []any{}}},
			{"hosted tool", key, "invalid_request_error", "hosted_tool_unsupported", http.StatusBadRequest,
				map[string]any{"model": "msg-vllm", "max_tokens": 8, "messages": []any{},
					"tools": []any{map[string]any{"type": "web_search_20250305", "name": "web_search"}}}},
			{"price option", key, "invalid_request_error", "price_option_unsupported", http.StatusBadRequest,
				map[string]any{"model": "msg-claude", "max_tokens": 8, "messages": []any{}, "speed": "fast"}},
			{"token limit", tightKey, "rate_limit_error", "rate_limit_exceeded", http.StatusTooManyRequests,
				map[string]any{"model": "rpm", "max_tokens": 64, "messages": []any{}}},
		} {
			t.Run(c.name, func(t *testing.T) {
				r := g.postMessages(t, "/v1/messages", c.key, "refused-"+c.name, c.body)
				if r.StatusCode != c.status {
					t.Fatalf("%d %s, want %d", r.StatusCode, r.body, c.status)
				}
				if typ, code := anthropicErrorOf(t, r); typ != c.typ || code != c.code {
					t.Errorf("error %s/%s, want %s/%s", typ, code, c.typ, c.code)
				}
				if c.status == http.StatusTooManyRequests && r.Header.Get("Retry-After") == "" {
					t.Error("no Retry-After")
				}
			})
		}
	})

	// The old server answers its framework's 404 on /v1/messages: the request fails
	// over to vl, and old's circuit stays closed whatever the number of such answers.
	t.Run("endpoint missing", func(t *testing.T) {
		for i := range 8 {
			id := fmt.Sprintf("old-%d", i)
			r := g.postMessages(t, "/v1/messages", key, id, map[string]any{"model": "pair", "max_tokens": 8, "messages": []any{}})
			if r.StatusCode != http.StatusOK {
				t.Fatalf("%d %s", r.StatusCode, r.body)
			}
			g.settled(t, id)
		}
		// After its first 404 the old server is left out for the endpoint for a probe
		// interval: one request, not one per client request (the pre-merge review's M3).
		if n := len(old.Requests()); n != 1 {
			t.Fatalf("the old server got %d requests, want 1", n)
		}
		if v := g.metric(t, `kaiak_circuit_open{backend="old",deployment_model="Qwen/Qwen3-8B"}`); v != 0 {
			t.Errorf("old's circuit open = %v, want closed", v)
		}
		g.logs.wait(t, "the endpoint-missing warning", msg("the backend's server lacks an endpoint its type serves: an older version?",
			"kaiak.backend.id", "old"))
	})

	t.Run("Anthropic-shaped model list", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, g.api+"/v1/models", nil)
		req.Header.Set("X-Api-Key", key)
		req.Header.Set("Anthropic-Version", "2023-06-01")
		r := do(t, req)
		var list struct {
			Data []struct {
				Type string `json:"type"`
				ID   string `json:"id"`
			} `json:"data"`
		}
		if err := json.Unmarshal(r.body, &list); err != nil || r.StatusCode != http.StatusOK {
			t.Fatalf("%d (%v) %s", r.StatusCode, err, r.body)
		}
		var ids []string
		for _, m := range list.Data {
			ids = append(ids, m.ID)
		}
		if strings.Join(ids, ",") != "msg-claude,msg-foundry,msg-vllm,pair,rpm" {
			t.Errorf("models %v: want those serving Messages only", ids)
		}
	})
	g.stop(t)
}
