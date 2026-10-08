package main

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"kaiak/internal/accounting"
	"kaiak/internal/auth"
	"kaiak/internal/config"
	"kaiak/internal/fakebackend"
	"kaiak/internal/limits"
	"kaiak/internal/server"
)

// An applied config that drops a deployment makes the gateway forget that the
// deployment's server lacks an endpoint: newGraph has every applied config keep only
// its own deployments in the missing-endpoint memory (docs/specs/GATEWAY.md,
// Providers → An endpoint missing from a server). Put back within the probe interval,
// the deployment is remembered afresh at its next missing answer, its warning logged
// again; had the memory kept it, it would still be inside its interval, the warning
// silent.
func TestAppliedConfigForgetsADroppedDeploymentsMissingEndpoint(t *testing.T) {
	backend := fakebackend.New()
	defer backend.Close()
	// A vLLM server serving a reranker has no chat route.
	backend.SetNoRoute(`{"detail":"Not Found"}`, "chat/completions")
	var logs syncBuffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	g := newGraph(settings{bodyMemory: server.DefaultBodyMemory}, "test", envOf(nil), logger, nil)
	api := server.NewAPI(g.holder, server.NewDrain(), server.NewBodyBudget(server.DefaultBodyMemory), g.providers,
		limits.New(g.holder, time.Now, logger), g.router, g.missingEndpoints,
		accounting.NewRecorder(accounting.RecorderOptions{Instance: "test", Metrics: g.usage, Logger: logger}), g.ops, logger)

	const key = "kaiak-test-key"
	apply := func(trigger config.Trigger, withReranker bool) {
		t.Helper()
		model := func(name, backendModel string) string {
			return `"` + name + `": { "deployments": [{ "backend": "vl", "model": "` + backendModel + `" }],
			  "metadata": { "context_length": 8192,
			    "capabilities": { "streaming": true, "tools": false, "vision": false, "reasoning": false } } }`
		}
		models := model("embedder", "embed-back")
		if withReranker {
			models += ", " + model("reranker", "rerank-back")
		}
		doc := `{ "format_version": 5, "global": { "circuit": { "probe_interval_ms": 3600000 } },
		  "backends": { "vl": { "type": "vllm", "base_url": "` + backend.URL() + `/v1" } },
		  "models": { ` + models + ` },
		  "groups": { "g": { "allowed_models": ["*"] } },
		  "keys": { "k": { "hash": "` + auth.KeyHash(key) + `", "group": "g" } } }`
		if _, err := g.applier.Apply(trigger, []byte(doc)); err != nil {
			t.Fatal(err)
		}
	}
	chatToReranker := func() {
		t.Helper()
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
			strings.NewReader(`{"model":"reranker","messages":[{"role":"user","content":"hi"}]}`))
		r.Header.Set("Authorization", "Bearer "+key)
		w := httptest.NewRecorder()
		api.ServeHTTP(w, r)
		if w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), `"upstream_endpoint_missing"`) {
			t.Fatalf("chat to the reranker: %d %s, want 502 upstream_endpoint_missing", w.Code, w.Body.String())
		}
	}
	warnings := func() int {
		return strings.Count(logs.String(), `"msg":"the deployment's server does not serve an endpoint its type serves"`)
	}

	apply(config.TriggerStartup, true)
	chatToReranker()
	chatToReranker()
	if n := warnings(); n != 1 {
		t.Fatalf("%d endpoint-missing warnings, want 1: the second answer is inside the interval\n%s", n, logs.String())
	}
	apply(config.TriggerSIGHUP, false)
	apply(config.TriggerSIGHUP, true)
	chatToReranker()
	if n := warnings(); n != 2 {
		t.Errorf("%d endpoint-missing warnings, want 2: the config without the deployment forgets it\n%s", n, logs.String())
	}
}
