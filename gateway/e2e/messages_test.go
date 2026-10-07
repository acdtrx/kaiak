package e2e

// Anthropic Messages end to end (docs/specs/GATEWAY.md, Client API → Client APIs):
// one gateway passes Messages requests through to a vllm, an anthropic and an
// azure-anthropic backend — all on one fake backend — each at its module's URL with
// its credential, streamed and not; the usage settles from Anthropic's report; the
// gateway's own answers take Anthropic's shape; and a server lacking the endpoint is
// failed over without opening its circuit.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"kaiak/internal/fakebackend"
)

// messagesBackend is a backend of the scenario serving Messages, and whether it is
// sent the standard tier alone (anthropic).
type messagesBackend struct {
	scenarioBackend
	standardOnly bool
}

var messagesBackends = []messagesBackend{
	{scenarioBackend{name: "vl", typ: "vllm", model: "msg-vllm", deployed: "Qwen/Qwen3-8B"}, false},
	{scenarioBackend{name: "claude", typ: "anthropic", model: "msg-claude", deployed: "claude-haiku-4-5"}, true},
	{scenarioBackend{name: "foundry", typ: "azure-anthropic", model: "msg-foundry", deployed: "claude-haiku-4-5-dep"}, false},
}

// chatOnly is the scenario's backend whose type does not serve Messages.
var chatOnly = scenarioBackend{name: "oc", typ: "openai-compatible", model: "chat-only", deployed: "chat-only"}

// messagesPassthrough is a Messages request sent the way Anthropic's SDKs do,
// streamed and not, asking for the auto tier: the usage settles from Anthropic's
// report; no Authorization header reaches the backend; Anthropic's own types get the
// anthropic-version header, vllm none; anthropic is sent the standard tier alone, the
// others the client's.
var messagesPassthrough = passthrough[messagesBackend]{
	path:        "/v1/messages",
	backendPath: "/messages",
	send:        (*gateway).postMessages,
	body: func(model string, c passthroughCase) map[string]any {
		return map[string]any{"model": model, "max_tokens": 32, "stream": c.stream, "service_tier": c.tier,
			"messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	},
	cases:     streamCases,
	streamEnd: "event: message_stop",
	usage: map[string]any{"gen_ai.usage.input_tokens": 30.0, "gen_ai.usage.cache_read.input_tokens": 20.0,
		"gen_ai.usage.output_tokens": 4.0, "kaiak.usage.estimated": false, "kaiak.usage.partial": false,
		"gen_ai.operation.name": "chat"},
	check: func(t *testing.T, b messagesBackend, c passthroughCase, sent map[string]any, got *fakebackend.Request) {
		if got.Header.Get("Authorization") != "" {
			t.Error("an Authorization header reached the backend")
		}
		if (b.typ == "vllm") != (got.Header.Get("Anthropic-Version") == "") {
			t.Errorf("anthropic-version %q on a %s backend", got.Header.Get("Anthropic-Version"), b.typ)
		}
		want := c.tier
		if b.standardOnly {
			want = "standard_only"
		}
		if sent["service_tier"] != want {
			t.Errorf("service_tier sent %v, want %v", sent["service_tier"], want)
		}
	},
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
	writeJSON(t, configFile, apiScenarioConfig(backend.URL(), old.URL(), hash,
		append(scenarioBackends(messagesBackends), chatOnly)))
	g := startGatewayEnv(t, append(gatewayEnv(configFile), backendKeysEnv()...))

	messagesPassthrough.run(t, g, backend, key, messagesBackends)

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
