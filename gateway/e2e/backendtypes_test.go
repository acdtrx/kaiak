package e2e

// Backend types end to end (docs/specs/GATEWAY.md, Providers): one gateway routes to
// an openai, a vllm, a llama-server and an azure-openai backend — four backends on
// one fake backend, which serves both URL layouts — and each request leaves with its
// module's URL, credential and service tier.

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"kaiak/internal/fakebackend"
)

// typedBackend is one backend of the scenario: its type, the public model deployed
// on it alone, the backend-side model name (which tells its requests apart at the
// fake backend), and what its requests carry.
type typedBackend struct {
	name, typ, model, deployed string
	path                       string // the chat completions path the backend receives
	header, credential         string // the credential header and its value ("" = none)
	forcesTier                 bool   // the standard tier is forced (openai, azure-openai)
}

var typedBackends = []typedBackend{
	{name: "openai", typ: "openai", model: "on-openai", deployed: "gpt-4.1-mini",
		path: "/v1/chat/completions", header: "Authorization", credential: "Bearer sk-e2e-openai", forcesTier: true},
	{name: "vllm", typ: "vllm", model: "on-vllm", deployed: "Qwen/Qwen3-8B",
		path: "/v1/chat/completions", header: "Authorization"},
	{name: "llama-server", typ: "llama-server", model: "on-llama-server", deployed: "qwen3-8b-q4",
		path: "/v1/chat/completions", header: "Authorization"},
	{name: "azure", typ: "azure-openai", model: "on-azure", deployed: "gpt-4.1-mini-prod",
		path: "/openai/v1/chat/completions", header: "Api-Key", credential: "e2e-azure", forcesTier: true},
}

// Environment variables holding the openai and azure-openai backends' keys.
const (
	openAIKeyEnv = "E2E_OPENAI_API_KEY"
	azureKeyEnv  = "E2E_AZURE_API_KEY"
)

// typesConfig is a config with one backend of each typedBackends type on the fake
// backend at backendURL, and one model per backend.
func typesConfig(backendURL, hash string) map[string]any {
	backends := map[string]any{}
	models := map[string]any{}
	for _, b := range typedBackends {
		entry := map[string]any{"type": b.typ, "base_url": backendURL + "/v1"}
		switch b.typ {
		case "openai":
			entry["api_key_env"] = openAIKeyEnv
		case "azure-openai":
			entry["base_url"] = backendURL // the resource endpoint: the module adds /openai/v1/
			entry["api_key_env"] = azureKeyEnv
		}
		backends[b.name] = entry
		models[b.model] = map[string]any{
			"deployments": []any{map[string]any{"backend": b.name, "model": b.deployed}},
			"metadata": map[string]any{"context_length": 8192,
				"capabilities": map[string]any{"streaming": true, "tools": false, "vision": false, "reasoning": false}},
			"output_limit": map[string]any{"default": 64, "ceiling": 128},
		}
	}
	return map[string]any{
		"format_version": 4,
		"global":         map[string]any{},
		"backends":       backends,
		"models":         models,
		"groups":         map[string]any{"w": map[string]any{"allowed_models": []any{"*"}}},
		"keys":           map[string]any{"k": map[string]any{"hash": hash, "group": "w"}},
	}
}

// Each type's request succeeds and reaches the backend at its module's path with its
// credential; only openai and azure-openai send "service_tier":"default" — added to a
// chat that names no tier, replacing the one a client asks for — while vllm and
// llama-server pass the client's tier untouched and add none.
func TestBackendTypes(t *testing.T) {
	backend := fakebackend.New()
	defer backend.Close()
	var served []string
	for _, b := range typedBackends {
		served = append(served, b.deployed)
	}
	backend.SetModels(served...)
	dir := t.TempDir()
	key, hash := newKey()
	configFile := filepath.Join(dir, "config.json")
	writeJSON(t, configFile, typesConfig(backend.URL(), hash))
	g := startGatewayEnv(t, append(gatewayEnv(configFile, ""),
		openAIKeyEnv+"=sk-e2e-openai", azureKeyEnv+"=e2e-azure"))

	for _, b := range typedBackends {
		for _, c := range []struct {
			name   string
			client any // the client's service_tier; nil = none sent
		}{{"no-tier", nil}, {"priority", "priority"}} {
			t.Run(b.typ+"/"+c.name, func(t *testing.T) {
				var extra map[string]any
				if c.client != nil {
					extra = map[string]any{"service_tier": c.client}
				}
				id := b.name + "-" + c.name
				before := len(backend.Requests())
				if r := g.post(t, "/v1/chat/completions", key, id, chatBody(b.model, false, extra)); r.StatusCode != http.StatusOK {
					t.Fatalf("%d %s, want 200", r.StatusCode, r.body)
				}
				line := g.settled(t, id)
				if line["kaiak.backend.id"] != b.name || line["gen_ai.usage.output_tokens"] != 4.0 {
					t.Errorf("log line %v, want backend %s and the fake's 4 tokens out", line, b.name)
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
				if v := got.Header.Get(b.header); v != b.credential {
					t.Errorf("%s header %q, want %q", b.header, v, b.credential)
				}
				tier, sentTier := sent["service_tier"]
				switch {
				case b.forcesTier:
					if tier != "default" {
						t.Errorf("service_tier sent = %v, want the forced \"default\"", tier)
					}
				case c.client == nil:
					if sentTier {
						t.Errorf("service_tier sent = %v, want none", tier)
					}
				case tier != c.client:
					t.Errorf("service_tier sent = %v, want the client's %v", tier, c.client)
				}
			})
		}
	}
	g.stop(t)
}

// A backend whose base_url misses the API version path (the 2026-09-30 review's O1):
// its models list answers 404, and the check at config apply warns, naming the
// backend, its base_url and what base_url should hold — the other backends, whose
// lists answer, get no such warning.
func TestWrongBaseURLIsWarnedAtApply(t *testing.T) {
	backend := fakebackend.New()
	defer backend.Close()
	dir := t.TempDir()
	_, hash := newKey()
	cfg := typesConfig(backend.URL(), hash)
	cfg["backends"].(map[string]any)["lost"] = map[string]any{"type": "vllm", "base_url": backend.URL()}
	cfg["models"].(map[string]any)["on-lost"] = map[string]any{
		"deployments": []any{map[string]any{"backend": "lost", "model": "Qwen/Qwen3-8B"}},
		"metadata": map[string]any{"context_length": 8192,
			"capabilities": map[string]any{"streaming": true, "tools": false, "vision": false, "reasoning": false}},
	}
	configFile := filepath.Join(dir, "config.json")
	writeJSON(t, configFile, cfg)
	g := startGatewayEnv(t, append(gatewayEnv(configFile, ""),
		openAIKeyEnv+"=sk-e2e-openai", azureKeyEnv+"=e2e-azure"))

	line := g.logs.wait(t, "the base_url warning", msg("the backend has no models list at its base_url", "kaiak.backend.id", "lost"))
	if line["level"] != "WARN" || line["kaiak.backend.base_url"] != backend.URL() ||
		line["kaiak.backend.base_url_hint"] != "base_url should end in the API version path, e.g. /v1" {
		t.Errorf("warning %v, want WARN naming base_url %s with the version-path hint", line, backend.URL())
	}
	if strings.Count(g.logs.text(), "no models list at its base_url") != 1 {
		t.Errorf("log:\n%s\nwant the one warning, for lost", g.logs.text())
	}
}
