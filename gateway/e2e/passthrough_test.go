package e2e

// The passthrough scenarios' shared parts (docs/specs/GATEWAY.md, Providers): what
// each backend type's requests carry and where its base_url points, the scenario
// config, and the per-backend check every client API's passthrough runs — the API's
// own expectations come in its spec.

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"kaiak/internal/fakebackend"
)

// backendType is what a backend of a type carries in the scenarios: its credential
// and where its requests go.
type backendType struct {
	header     string // the credential header
	credential string // its value ("": none sent — a type configured without a key)
	keyEnv     string // the environment variable holding the key ("": none)
	key        string // the key keyEnv holds
	// resourceRoot: base_url is the resource endpoint and the module adds apiPrefix;
	// otherwise base_url ends in apiPrefix (/v1).
	resourceRoot bool
	apiPrefix    string // the path prefix the backend receives requests under
}

// backendTypes are the facts the scenarios expect of each type. The self-hosted
// types run without a key here, so their requests carry no Authorization.
var backendTypes = map[string]backendType{
	"openai-compatible": {header: "Authorization", apiPrefix: "/v1"},
	"vllm":              {header: "Authorization", apiPrefix: "/v1"},
	"llama-server":      {header: "Authorization", apiPrefix: "/v1"},
	"openai": {header: "Authorization", credential: "Bearer sk-e2e-openai",
		keyEnv: "E2E_OPENAI_API_KEY", key: "sk-e2e-openai", apiPrefix: "/v1"},
	"azure-openai": {header: "Api-Key", credential: "e2e-azure",
		keyEnv: "E2E_AZURE_API_KEY", key: "e2e-azure", resourceRoot: true, apiPrefix: "/openai/v1"},
	"anthropic": {header: "X-Api-Key", credential: "sk-ant-e2e",
		keyEnv: "E2E_ANTHROPIC_API_KEY", key: "sk-ant-e2e", apiPrefix: "/v1"},
	"azure-anthropic": {header: "Api-Key", credential: "e2e-foundry",
		keyEnv: "E2E_AZURE_ANTHROPIC_API_KEY", key: "e2e-foundry", resourceRoot: true, apiPrefix: "/anthropic/v1"},
}

// typeOf is typ's row of backendTypes; an unknown type is a mistake in the test.
func typeOf(typ string) backendType {
	bt, ok := backendTypes[typ]
	if !ok {
		panic("e2e: no backendTypes row for type " + typ)
	}
	return bt
}

// backendEntry is the config entry of a typ backend on the fake backend at fakeURL.
func backendEntry(typ, fakeURL string) map[string]any {
	bt := typeOf(typ)
	entry := map[string]any{"type": typ, "base_url": fakeURL + bt.apiPrefix}
	if bt.resourceRoot {
		entry["base_url"] = fakeURL
	}
	if bt.keyEnv != "" {
		entry["api_key_env"] = bt.keyEnv
	}
	return entry
}

// backendKeysEnv sets every keyed type's key variable.
func backendKeysEnv() []string {
	var env []string
	for _, bt := range backendTypes {
		if bt.keyEnv != "" {
			env = append(env, bt.keyEnv+"="+bt.key)
		}
	}
	slices.Sort(env)
	return env
}

// scenarioBackend is one backend of a scenario: its name, type, the public model
// deployed on it alone, and the backend-side model (which tells its requests apart at
// the fake backend).
type scenarioBackend struct {
	name, typ, model, deployed string
}

func (b scenarioBackend) scenario() scenarioBackend { return b }

// apiRow is a row of an API's backend list: a scenario backend, and what the API
// expects of it.
type apiRow interface{ scenario() scenarioBackend }

// scenarioBackends are the rows' scenario backends.
func scenarioBackends[R apiRow](rows []R) []scenarioBackend {
	out := make([]scenarioBackend, len(rows))
	for i, r := range rows {
		out[i] = r.scenario()
	}
	return out
}

// scenarioModel is a public model of the scenarios on deployments.
func scenarioModel(deployments ...map[string]any) map[string]any {
	ds := []any{}
	for _, d := range deployments {
		ds = append(ds, d)
	}
	return map[string]any{
		"deployments": ds,
		"metadata": map[string]any{"context_length": 8192,
			"capabilities": map[string]any{"streaming": true, "tools": true, "vision": false, "reasoning": true}},
		"output_limit": map[string]any{"default": 64, "ceiling": 128},
	}
}

// scenarioConfig is a config with backends on the fake backend at fakeURL, each
// serving its public model alone, all open to the key with hash in group w.
func scenarioConfig(fakeURL, hash string, backends []scenarioBackend) map[string]any {
	entries := map[string]any{}
	models := map[string]any{}
	for _, b := range backends {
		entries[b.name] = backendEntry(b.typ, fakeURL)
		models[b.model] = scenarioModel(map[string]any{"backend": b.name, "model": b.deployed})
	}
	return map[string]any{
		"format_version": 5,
		"global":         map[string]any{},
		"backends":       entries,
		"models":         models,
		"groups":         map[string]any{"w": map[string]any{"allowed_models": []any{"*"}}},
		"keys":           map[string]any{"k": map[string]any{"hash": hash, "group": "w"}},
	}
}

// The tight group's key.
var tightKey, tightHash = newKey()

// apiScenarioConfig is scenarioConfig plus what an API's scenario refuses and fails
// over: a vllm backend "old" at oldURL whose server lacks the API's endpoint, a model
// "pair" on it and on the backend "vl", a model "rpm" on "vl", and the group tight —
// allowed rpm alone, 10 tokens a minute — with the key tightKey.
func apiScenarioConfig(fakeURL, oldURL, hash string, backends []scenarioBackend) map[string]any {
	cfg := scenarioConfig(fakeURL, hash, backends)
	cfg["backends"].(map[string]any)["old"] = backendEntry("vllm", oldURL)
	models := cfg["models"].(map[string]any)
	models["pair"] = scenarioModel(map[string]any{"backend": "old", "model": "Qwen/Qwen3-8B"},
		map[string]any{"backend": "vl", "model": "Qwen/Qwen3-8B"})
	models["rpm"] = scenarioModel(map[string]any{"backend": "vl", "model": "Qwen/Qwen3-8B"})
	cfg["groups"].(map[string]any)["tight"] = map[string]any{"allowed_models": []any{"rpm"},
		"limits": []any{map[string]any{"type": "tokens_per_minute", "value": 10}}}
	cfg["keys"].(map[string]any)["k-tight"] = map[string]any{"hash": tightHash, "group": "tight"}
	return cfg
}

// passthroughCase is one request each backend of a passthrough gets.
type passthroughCase struct {
	name   string // the subtest's name, after the backend type
	stream bool
	tier   any // the service_tier the client asks for; nil = none
}

// passthrough is a client API's passthrough scenario: how its request is sent, and
// what the API expects beyond what every passthrough checks.
type passthrough[R apiRow] struct {
	path        string // the client's path
	backendPath string // the path the backend receives, after its type's API prefix
	send        func(g *gateway, t *testing.T, path, key, requestID string, body any) *response
	body        func(model string, c passthroughCase) map[string]any
	cases       []passthroughCase
	streamEnd   string         // what a streamed answer holds once complete
	usage       map[string]any // the settled log line's usage attributes
	// check makes the API's own assertions on what the backend of row received.
	check func(t *testing.T, row R, c passthroughCase, sent map[string]any, got *fakebackend.Request)
}

// run sends every case to every row's model on g with key, each in a subtest named
// after the backend's type and the case. Every request succeeds with the public model
// name in its answer, settles on its backend with the API's usage, and reaches the
// fake backend once — at the type's path, for the deployed model, with the type's
// credential and never the client's key — and then meets the API's own check.
func (p passthrough[R]) run(t *testing.T, g *gateway, fake *fakebackend.Backend, key string, rows []R) {
	for _, row := range rows {
		b := row.scenario()
		bt := typeOf(b.typ)
		for _, c := range p.cases {
			t.Run(b.typ+"/"+c.name, func(t *testing.T) {
				id := b.name + "-" + strings.ReplaceAll(c.name, "=", "-") // a request ID holds no '='
				before := len(fake.Requests())
				r := p.send(g, t, p.path, key, id, p.body(b.model, c))
				if r.StatusCode != http.StatusOK {
					t.Fatalf("%d %s, want 200", r.StatusCode, r.body)
				}
				if !strings.Contains(string(r.body), `"model":"`+b.model+`"`) || strings.Contains(string(r.body), b.deployed) {
					t.Errorf("answer does not carry the public model name:\n%s", r.body)
				}
				if c.stream && !strings.Contains(string(r.body), p.streamEnd) {
					t.Errorf("stream:\n%s", r.body)
				}
				line := g.settled(t, id)
				if line["kaiak.backend.id"] != b.name {
					t.Errorf("log line %v, want backend %s", line, b.name)
				}
				for attr, want := range p.usage {
					if line[attr] != want {
						t.Errorf("log line %v: %s = %v, want %v", line, attr, line[attr], want)
					}
				}
				reqs := fake.Requests()
				if len(reqs) != before+1 {
					t.Fatalf("backend got %d requests, want 1", len(reqs)-before)
				}
				got := reqs[len(reqs)-1]
				var sent map[string]any
				if err := json.Unmarshal(got.Body, &sent); err != nil {
					t.Fatal(err)
				}
				if path := bt.apiPrefix + p.backendPath; got.Path != path || sent["model"] != b.deployed {
					t.Errorf("backend got %s for model %v, want %s for %s", got.Path, sent["model"], path, b.deployed)
				}
				if v := got.Header.Get(bt.header); v != bt.credential {
					t.Errorf("%s header %q, want %q", bt.header, v, bt.credential)
				}
				for name, values := range got.Header {
					if slices.ContainsFunc(values, func(v string) bool { return strings.Contains(v, key) }) {
						t.Errorf("the client's key reached the backend in %s", name)
					}
				}
				p.check(t, row, c, sent, got)
			})
		}
	}
}

// streamCases are a request streamed and not, asking for the auto tier.
var streamCases = []passthroughCase{{name: "stream=false", tier: "auto"}, {name: "stream=true", stream: true, tier: "auto"}}
