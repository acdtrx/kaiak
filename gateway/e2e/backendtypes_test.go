package e2e

// Backend types end to end (docs/specs/GATEWAY.md, Providers): one gateway routes to
// an openai, a vllm, a llama-server and an azure-openai backend — four backends on
// one fake backend, which serves both URL layouts — and each request leaves with its
// module's URL, credential and service tier.

import (
	"path/filepath"
	"strings"
	"testing"

	"kaiak/internal/fakebackend"
)

// chatBackend is a backend of the scenario, and whether the standard tier is forced
// on it (openai, azure-openai).
type chatBackend struct {
	scenarioBackend
	forcesTier bool
}

var chatBackends = []chatBackend{
	{scenarioBackend{name: "openai", typ: "openai", model: "on-openai", deployed: "gpt-4.1-mini"}, true},
	{scenarioBackend{name: "vllm", typ: "vllm", model: "on-vllm", deployed: "Qwen/Qwen3-8B"}, false},
	{scenarioBackend{name: "llama-server", typ: "llama-server", model: "on-llama-server", deployed: "qwen3-8b-q4"}, false},
	{scenarioBackend{name: "azure", typ: "azure-openai", model: "on-azure", deployed: "gpt-4.1-mini-prod"}, true},
}

// chatPassthrough is a chat completion, once naming no tier and once asking for
// priority; only openai and azure-openai send "service_tier":"default" — added to a
// chat that names no tier, replacing the one a client asks for — while vllm and
// llama-server pass the client's tier untouched and add none.
var chatPassthrough = passthrough[chatBackend]{
	path:        "/v1/chat/completions",
	backendPath: "/chat/completions",
	send:        (*gateway).post,
	body: func(model string, c passthroughCase) map[string]any {
		var extra map[string]any
		if c.tier != nil {
			extra = map[string]any{"service_tier": c.tier}
		}
		return chatBody(model, c.stream, extra)
	},
	cases:     []passthroughCase{{name: "no-tier"}, {name: "priority", tier: "priority"}},
	streamEnd: "data: [DONE]",
	usage:     map[string]any{"gen_ai.usage.output_tokens": 4.0}, // the fake's 4 tokens out
	check: func(t *testing.T, b chatBackend, c passthroughCase, sent map[string]any, _ *fakebackend.Request) {
		tier, sentTier := sent["service_tier"]
		switch {
		case b.forcesTier:
			if tier != "default" {
				t.Errorf("service_tier sent = %v, want the forced \"default\"", tier)
			}
		case c.tier == nil:
			if sentTier {
				t.Errorf("service_tier sent = %v, want none", tier)
			}
		case tier != c.tier:
			t.Errorf("service_tier sent = %v, want the client's %v", tier, c.tier)
		}
	},
}

// Each type's request succeeds and reaches the backend at its module's path with its
// credential and the service tier chatPassthrough expects.
func TestBackendTypes(t *testing.T) {
	backend := fakebackend.New()
	defer backend.Close()
	var served []string
	for _, b := range chatBackends {
		served = append(served, b.deployed)
	}
	backend.SetModels(served...)
	dir := t.TempDir()
	key, hash := newKey()
	configFile := filepath.Join(dir, "config.json")
	writeJSON(t, configFile, scenarioConfig(backend.URL(), hash, scenarioBackends(chatBackends)))
	g := startGatewayEnv(t, append(gatewayEnv(configFile), backendKeysEnv()...))

	chatPassthrough.run(t, g, backend, key, chatBackends)
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
	cfg := scenarioConfig(backend.URL(), hash, scenarioBackends(chatBackends))
	cfg["backends"].(map[string]any)["lost"] = map[string]any{"type": "vllm", "base_url": backend.URL()}
	cfg["models"].(map[string]any)["on-lost"] = map[string]any{
		"deployments": []any{map[string]any{"backend": "lost", "model": "Qwen/Qwen3-8B"}},
		"metadata": map[string]any{"context_length": 8192,
			"capabilities": map[string]any{"streaming": true, "tools": false, "vision": false, "reasoning": false}},
	}
	configFile := filepath.Join(dir, "config.json")
	writeJSON(t, configFile, cfg)
	g := startGatewayEnv(t, append(gatewayEnv(configFile), backendKeysEnv()...))

	line := g.logs.wait(t, "the base_url warning", msg("the backend has no models list at its base_url", "kaiak.backend.id", "lost"))
	if line["level"] != "WARN" || line["kaiak.backend.base_url"] != backend.URL() ||
		line["kaiak.backend.base_url_hint"] != "base_url should end in the API version path, e.g. /v1" {
		t.Errorf("warning %v, want WARN naming base_url %s with the version-path hint", line, backend.URL())
	}
	if strings.Count(g.logs.text(), "no models list at its base_url") != 1 {
		t.Errorf("log:\n%s\nwant the one warning, for lost", g.logs.text())
	}
}
