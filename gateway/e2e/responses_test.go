package e2e

// OpenAI Responses end to end (docs/specs/GATEWAY.md, Client API → Client APIs): one
// gateway passes Responses requests through to a vllm, a llama-server, an openai and
// an azure-openai backend — all on one fake backend — each at its module's URL with
// its credential, streamed and not, always with store: false; the usage settles from
// the response's report; the stateful parts of the API, hosted tools and limits are
// refused in OpenAI's shape; token counting reaches only the types that have it; and a
// server lacking the endpoint is failed over without opening its circuit.

import (
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"kaiak/internal/fakebackend"
)

// responsesBackend is a backend of the scenario serving Responses, whether the
// standard tier is forced on it (openai, azure-openai), and whether its type counts
// input tokens.
type responsesBackend struct {
	scenarioBackend
	forcesTier, countsTokens bool
}

var responsesBackends = []responsesBackend{
	{scenarioBackend{name: "vl", typ: "vllm", model: "resp-vllm", deployed: "Qwen/Qwen3-8B"}, false, false},
	{scenarioBackend{name: "ls", typ: "llama-server", model: "resp-llama", deployed: "qwen3-8b.gguf"}, false, true},
	{scenarioBackend{name: "oa", typ: "openai", model: "resp-openai", deployed: "gpt-5.5-mini"}, true, true},
	{scenarioBackend{name: "az", typ: "azure-openai", model: "resp-azure", deployed: "gpt-5.5-mini-dep"}, true, false},
}

// claudeOnly is the scenario's backend whose type does not serve Responses.
var claudeOnly = scenarioBackend{name: "claude", typ: "anthropic", model: "claude-only", deployed: "claude-haiku-4-5"}

// responsesPassthrough is a Responses request, streamed and not, asking to be stored
// and for the auto tier: the usage settles from the response's report; the backend
// is always sent store: false, and the tier openai and azure-openai force or else
// the client's.
var responsesPassthrough = passthrough[responsesBackend]{
	path:        "/v1/responses",
	backendPath: "/responses",
	send:        (*gateway).post,
	body: func(model string, c passthroughCase) map[string]any {
		return map[string]any{"model": model, "stream": c.stream, "store": true, "service_tier": c.tier, "input": "hi"}
	},
	cases:     streamCases,
	streamEnd: "event: response.completed",
	usage: map[string]any{"gen_ai.usage.input_tokens": 30.0, "gen_ai.usage.cache_read.input_tokens": 20.0,
		"gen_ai.usage.output_tokens": 9.0, "kaiak.usage.estimated": false, "kaiak.usage.partial": false,
		"gen_ai.operation.name": "chat"},
	check: func(t *testing.T, b responsesBackend, c passthroughCase, sent map[string]any, _ *fakebackend.Request) {
		if sent["store"] != false {
			t.Errorf("store sent %v, want false", sent["store"])
		}
		want := c.tier
		if b.forcesTier {
			want = "default"
		}
		if sent["service_tier"] != want {
			t.Errorf("service_tier sent %v, want %v", sent["service_tier"], want)
		}
	},
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
	writeJSON(t, configFile, apiScenarioConfig(backend.URL(), old.URL(), hash,
		append(scenarioBackends(responsesBackends), claudeOnly)))
	g := startGatewayEnv(t, append(gatewayEnv(configFile), backendKeysEnv()...))

	responsesPassthrough.run(t, g, backend, key, responsesBackends)

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
