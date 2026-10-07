package e2e

// OpenAI Responses end to end (docs/specs/GATEWAY.md, Client API → Client APIs): one
// gateway passes Responses requests through to a vllm, a llama-server, an openai and
// an azure-openai backend — all on one fake backend — each at its module's URL with
// its credential (the keys backendtypes_test.go's variables name), streamed and not, always with store: false; the usage settles from
// the response's report; the stateful parts of the API, hosted tools and limits are
// refused in OpenAI's shape; token counting reaches only the types that have it; and a
// server lacking the endpoint is failed over without opening its circuit.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"kaiak/internal/fakebackend"
)

// responsesBackends are the scenario's backends serving Responses: name, type, the
// public model deployed on it alone, the backend-side model, and what its requests
// carry.
var responsesBackends = []struct {
	name, typ, model, deployed, path, header, credential string
	forcesTier, countsTokens                             bool
}{
	{name: "vl", typ: "vllm", model: "resp-vllm", deployed: "Qwen/Qwen3-8B", path: "/v1/responses"},
	{name: "ls", typ: "llama-server", model: "resp-llama", deployed: "qwen3-8b.gguf", path: "/v1/responses", countsTokens: true},
	{name: "oa", typ: "openai", model: "resp-openai", deployed: "gpt-5.5-mini", path: "/v1/responses",
		header: "Authorization", credential: "Bearer sk-e2e", forcesTier: true, countsTokens: true},
	{name: "az", typ: "azure-openai", model: "resp-azure", deployed: "gpt-5.5-mini-dep", path: "/openai/v1/responses",
		header: "Api-Key", credential: "e2e-azure", forcesTier: true},
}

// responsesConfig is a config with the responsesBackends, an anthropic backend
// "claude" serving "claude-only", a vllm backend "old" whose server lacks the endpoint
// and a model on it and on "vl" ("pair"), and a token limit on the tight key's group.
func responsesConfig(backendURL, oldURL, hash string) map[string]any {
	backends := map[string]any{
		"claude": map[string]any{"type": "anthropic", "base_url": backendURL + "/v1", "api_key_env": anthropicKeyEnv},
		"old":    map[string]any{"type": "vllm", "base_url": oldURL + "/v1"},
	}
	meta := map[string]any{"context_length": 8192,
		"capabilities": map[string]any{"streaming": true, "tools": true, "vision": false, "reasoning": true}}
	model := func(deployments ...map[string]any) map[string]any {
		ds := []any{}
		for _, d := range deployments {
			ds = append(ds, d)
		}
		return map[string]any{"deployments": ds, "metadata": meta, "output_limit": map[string]any{"default": 64, "ceiling": 128}}
	}
	models := map[string]any{
		"claude-only": model(map[string]any{"backend": "claude", "model": "claude-haiku-4-5"}),
		"pair": model(map[string]any{"backend": "old", "model": "Qwen/Qwen3-8B"},
			map[string]any{"backend": "vl", "model": "Qwen/Qwen3-8B"}),
		"rpm": model(map[string]any{"backend": "vl", "model": "Qwen/Qwen3-8B"}),
	}
	for _, b := range responsesBackends {
		entry := map[string]any{"type": b.typ, "base_url": backendURL + "/v1"}
		switch b.typ {
		case "openai":
			entry["api_key_env"] = openAIKeyEnv
		case "azure-openai":
			entry["base_url"] = backendURL // the resource endpoint: the module adds /openai/v1/
			entry["api_key_env"] = azureKeyEnv
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

// openAIErrorCode is an OpenAI-shaped error body's code.
func openAIErrorCode(t *testing.T, r *response) string {
	t.Helper()
	e, _ := r.json(t)["error"].(map[string]any)
	if e == nil {
		t.Fatalf("not an OpenAI error: %s", r.body)
	}
	return fmt.Sprint(e["code"])
}

func TestResponses(t *testing.T) {
	backend := fakebackend.New()
	defer backend.Close()
	old := fakebackend.New()
	defer old.Close()
	old.SetReply(fakebackend.Reply{Status: http.StatusNotFound, Body: `{"detail":"Not Found"}`})
	backend.SetReply(fakebackend.Reply{Usage: &fakebackend.Usage{PromptTokens: 30, CompletionTokens: 9, CachedTokens: 20, ReasoningTokens: 5}})
	dir := t.TempDir()
	key, hash := newKey()
	configFile := filepath.Join(dir, "config.json")
	writeJSON(t, configFile, responsesConfig(backend.URL(), old.URL(), hash))
	g := startGatewayEnv(t, append(gatewayEnv(configFile),
		openAIKeyEnv+"=sk-e2e", azureKeyEnv+"=e2e-azure", anthropicKeyEnv+"=sk-ant-e2e"))

	for _, b := range responsesBackends {
		for _, stream := range []bool{false, true} {
			name := fmt.Sprintf("%s/stream=%v", b.typ, stream)
			t.Run(name, func(t *testing.T) {
				id := fmt.Sprintf("%s-%v", b.name, stream)
				before := len(backend.Requests())
				r := g.post(t, "/v1/responses", key, id, map[string]any{"model": b.model, "stream": stream,
					"store": true, "service_tier": "auto", "input": "hi"})
				if r.StatusCode != http.StatusOK {
					t.Fatalf("%d %s", r.StatusCode, r.body)
				}
				if !strings.Contains(string(r.body), `"model":"`+b.model+`"`) || strings.Contains(string(r.body), b.deployed) {
					t.Errorf("answer does not carry the public model name:\n%s", r.body)
				}
				if stream && !strings.Contains(string(r.body), "event: response.completed") {
					t.Errorf("stream:\n%s", r.body)
				}
				line := g.settled(t, id)
				if line["gen_ai.usage.input_tokens"] != 30.0 || line["gen_ai.usage.cache_read.input_tokens"] != 20.0 ||
					line["gen_ai.usage.output_tokens"] != 9.0 || line["kaiak.usage.estimated"] != false ||
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
				if got.Path != b.path || sent["model"] != b.deployed || sent["store"] != false {
					t.Errorf("backend got %s for model %v, store %v; want %s for %s, store false",
						got.Path, sent["model"], sent["store"], b.path, b.deployed)
				}
				if b.header != "" && got.Header.Get(b.header) != b.credential {
					t.Errorf("%s header %q, want %q", b.header, got.Header.Get(b.header), b.credential)
				}
				if strings.Contains(got.Header.Get("Authorization"), key) {
					t.Error("the client's key reached the backend")
				}
				if want := map[bool]any{true: "default", false: "auto"}[b.forcesTier]; sent["service_tier"] != want {
					t.Errorf("service_tier sent %v, want %v", sent["service_tier"], want)
				}
			})
		}
	}

	t.Run("input_tokens", func(t *testing.T) {
		for _, b := range responsesBackends {
			id := "count-" + b.name
			r := g.post(t, "/v1/responses/input_tokens", key, id, map[string]any{"model": b.model, "input": "hi"})
			if !b.countsTokens {
				if r.StatusCode != http.StatusBadRequest || openAIErrorCode(t, r) != "endpoint_not_served" {
					t.Errorf("%s: %d %s, want endpoint_not_served", b.typ, r.StatusCode, r.body)
				}
				continue
			}
			if r.StatusCode != http.StatusOK || !strings.Contains(string(r.body), `"input_tokens":30`) {
				t.Fatalf("%s: %d %s", b.typ, r.StatusCode, r.body)
			}
			if line := g.settled(t, id); line["gen_ai.usage.input_tokens"] != nil {
				t.Errorf("a token-counting request settled usage: %v", line)
			}
		}
	})

	t.Run("refusals in OpenAI's shape", func(t *testing.T) {
		for _, c := range []struct {
			name, key, code string
			status          int
			body            map[string]any
		}{
			{"not served", key, "endpoint_not_served", http.StatusBadRequest,
				map[string]any{"model": "claude-only", "input": "hi"}},
			{"previous_response_id", key, "stateful_responses_unsupported", http.StatusBadRequest,
				map[string]any{"model": "resp-openai", "input": "hi", "previous_response_id": "resp_1"}},
			{"background", key, "stateful_responses_unsupported", http.StatusBadRequest,
				map[string]any{"model": "resp-openai", "input": "hi", "background": true}},
			{"hosted tool", key, "hosted_tool_unsupported", http.StatusBadRequest,
				map[string]any{"model": "resp-openai", "input": "hi", "tools": []any{map[string]any{"type": "web_search"}}}},
			{"token limit", tightKey, "rate_limit_exceeded", http.StatusTooManyRequests,
				map[string]any{"model": "rpm", "input": "hi", "max_output_tokens": 64}},
		} {
			t.Run(c.name, func(t *testing.T) {
				r := g.post(t, "/v1/responses", c.key, "refused-"+c.name, c.body)
				if r.StatusCode != c.status || openAIErrorCode(t, r) != c.code {
					t.Fatalf("%d %s, want %d %s", r.StatusCode, r.body, c.status, c.code)
				}
				if c.status == http.StatusTooManyRequests && r.Header.Get("Retry-After") == "" {
					t.Error("no Retry-After")
				}
			})
		}
	})

	// The old server answers its framework's 404 on /v1/responses: the request fails
	// over to vl, and old's circuit stays closed whatever the number of such answers.
	t.Run("endpoint missing", func(t *testing.T) {
		for i := range 8 {
			id := fmt.Sprintf("old-%d", i)
			r := g.post(t, "/v1/responses", key, id, map[string]any{"model": "pair", "input": "hi"})
			if r.StatusCode != http.StatusOK {
				t.Fatalf("%d %s", r.StatusCode, r.body)
			}
			g.settled(t, id)
		}
		if n := len(old.Requests()); n == 0 {
			t.Fatal("the old server got no request: the scenario did not run")
		}
		if v := g.metric(t, `kaiak_circuit_open{backend="old",deployment_model="Qwen/Qwen3-8B"}`); v != 0 {
			t.Errorf("old's circuit open = %v, want closed", v)
		}
	})
	g.stop(t)
}
