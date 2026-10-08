package e2e

// Rerank end to end (docs/specs/GATEWAY.md, Client API → Client APIs; Providers →
// rerank answers are relayed as the backend sends them; Accounting → rerank usage):
// one gateway passes rerank requests through to a vllm and a llama-server backend —
// each a fake backend answering in its server's shape — with the model edit alone;
// the answer comes back in that shape under the public model name; the usage settles
// from the reported prompt tokens, priced on tokens_in, or is estimated and flagged
// when the answer reports none; the documents cap and a model with no
// rerank-serving deployment are refused before any backend; and a chat request to the
// vllm reranker, whose server has no chat route, is answered as the endpoint missing,
// neutral for the circuit.

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"kaiak/internal/accounting"
	"kaiak/internal/fakebackend"
	"kaiak/internal/provider"
)

// rerankBackend is a backend of the scenario serving rerank: its server's answer
// shape, and the members that shape's answer and each of its results carry.
type rerankBackend struct {
	scenarioBackend
	shape                  fakebackend.RerankShape
	answerKeys, resultKeys string
}

var rerankBackends = []rerankBackend{
	{scenarioBackend{name: "vl", typ: "vllm", model: "rerank-vllm", deployed: "Qwen/Qwen3-Reranker-0.6B"},
		fakebackend.VLLMRerank, "id,model,results,usage", "document,index,relevance_score"},
	{scenarioBackend{name: "ls", typ: "llama-server", model: "rerank-llama", deployed: "qwen3-reranker-0.6b-q8_0.gguf"},
		fakebackend.LlamaServerRerank, "model,object,results,usage", "index,relevance_score"},
}

// rerankUnserved is the scenario's backend whose type does not serve rerank, on the
// vllm row's fake backend, with the same reranker deployed.
var rerankUnserved = scenarioBackend{name: "oc", typ: "openai-compatible", model: "rerank-oc", deployed: "Qwen/Qwen3-Reranker-0.6B"}

// vllmRouteMissing is vLLM's answer to a path it has no route for — FastAPI's
// (docs/specs/GATEWAY.md, Providers → Wrong path to a host).
const vllmRouteMissing = `{"detail":"Not Found"}`

// rerankPromptTokens is what the fake backends report; at the rerankers' $2 per
// million input tokens it costs 80 µUSD.
const rerankPromptTokens = 40

// rerankConfig is a config with each row's backend on its fake backend serving its
// reranker alone, priced at $2 per million input tokens, rerankUnserved on the vllm
// row's fake, and global.max_rerank_documents 3 — all open to the key with hash in
// group w.
func rerankConfig(fakes map[string]*fakebackend.Backend, hash string) map[string]any {
	reranker := func(b scenarioBackend) map[string]any {
		return map[string]any{
			"deployments": []any{map[string]any{"backend": b.name, "model": b.deployed}},
			"metadata": map[string]any{"context_length": 8192,
				"capabilities": map[string]any{"streaming": false, "tools": false, "vision": false, "reasoning": false}},
			"prices": []any{map[string]any{"effective_from": "2026-01-01", "tiers": []any{map[string]any{
				"above_input_tokens": 0, "usd_per_million": map[string]any{"tokens_in": 2, "tokens_out": 0}}}}},
		}
	}
	backends := map[string]any{rerankUnserved.name: backendEntry(rerankUnserved.typ, fakes["vl"].URL())}
	models := map[string]any{rerankUnserved.model: reranker(rerankUnserved)}
	for _, b := range rerankBackends {
		backends[b.name] = backendEntry(b.typ, fakes[b.name].URL())
		models[b.model] = reranker(b.scenarioBackend)
	}
	return map[string]any{
		"format_version": 5,
		"global":         map[string]any{"max_rerank_documents": 3},
		"backends":       backends,
		"models":         models,
		"groups":         map[string]any{"w": map[string]any{"allowed_models": []any{"*"}}},
		"keys":           map[string]any{"k": map[string]any{"hash": hash, "group": "w"}},
	}
}

// rerankBody is a rerank request for model scoring documents against a question
// about pandas, encoded as the client sends it.
func rerankBody(t *testing.T, model string, documents []string, extra map[string]any) json.RawMessage {
	t.Helper()
	body := map[string]any{"model": model, "query": "what does a panda eat", "documents": documents}
	maps.Copy(body, extra)
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// rerankDocuments are three documents of which the second answers the question.
var rerankDocuments = []string{"the sky is blue", "a panda eats bamboo", "the bear sleeps"}

func TestRerank(t *testing.T) {
	fakes := map[string]*fakebackend.Backend{}
	for _, b := range rerankBackends {
		fake := fakebackend.New()
		defer fake.Close()
		fake.SetRerankShape(b.shape)
		fake.SetModels(b.deployed)
		fake.SetReply(fakebackend.Reply{Usage: &fakebackend.Usage{PromptTokens: rerankPromptTokens}})
		fakes[b.name] = fake
	}
	// vl's server is a vLLM reranker: vLLM creates its routes from the loaded model's
	// tasks, so it has none for the generating and embedding endpoints.
	fakes["vl"].SetNoRoute(vllmRouteMissing, "chat/completions", "completions", "embeddings", "messages",
		"messages/count_tokens", "responses")
	received := func() int {
		n := 0
		for _, fake := range fakes {
			n += len(fake.Requests())
		}
		return n
	}
	dir := t.TempDir()
	key, hash := newKey()
	configFile := filepath.Join(dir, "config.json")
	writeJSON(t, configFile, rerankConfig(fakes, hash))
	g := startGateway(t, configFile)

	// Each backend gets the request with the model edit alone and answers in its
	// server's shape: the relevant document first, cut to top_n, under the public
	// model name. The record settles from the reported prompt tokens, priced on
	// tokens_in, with operation rerank — never a stream.
	for _, b := range rerankBackends {
		t.Run(b.typ, func(t *testing.T) {
			fake := fakes[b.name]
			id := "rerank-" + b.name
			body := rerankBody(t, b.model, rerankDocuments, map[string]any{"top_n": 2, "return_documents": true})
			before := len(fake.Requests())
			r := g.post(t, "/v1/rerank", key, id, body)
			if r.StatusCode != http.StatusOK || r.Header.Get("Content-Type") != "application/json" {
				t.Fatalf("%d (%s) %s, want 200 with a JSON body", r.StatusCode, r.Header.Get("Content-Type"), r.body)
			}
			var answer struct {
				Model   string           `json:"model"`
				Results []map[string]any `json:"results"`
				Usage   map[string]any   `json:"usage"`
			}
			if err := json.Unmarshal(r.body, &answer); err != nil {
				t.Fatalf("answer %s: %v", r.body, err)
			}
			if answer.Model != b.model || strings.Contains(string(r.body), b.deployed) {
				t.Errorf("answer %s: want the public model name %s alone", r.body, b.model)
			}
			if len(answer.Results) != 2 || answer.Results[0]["index"] != 1.0 ||
				answer.Results[0]["relevance_score"].(float64) <= answer.Results[1]["relevance_score"].(float64) {
				t.Errorf("results %v: want 2, the panda document first", answer.Results)
			}
			if got := strings.Join(slices.Sorted(maps.Keys(r.json(t))), ","); got != b.answerKeys {
				t.Errorf("answer members %s, want %s's: %s", got, b.typ, b.answerKeys)
			}
			for _, result := range answer.Results {
				if got := strings.Join(slices.Sorted(maps.Keys(result)), ","); got != b.resultKeys {
					t.Errorf("result members %s, want %s's: %s", got, b.typ, b.resultKeys)
				}
			}
			if answer.Usage["prompt_tokens"] != float64(rerankPromptTokens) {
				t.Errorf("usage %v, want the backend's relayed", answer.Usage)
			}

			reqs := fake.Requests()
			if len(reqs) != before+1 {
				t.Fatalf("backend got %d requests, want 1", len(reqs)-before)
			}
			got := reqs[len(reqs)-1]
			want := strings.Replace(string(body), `"model":"`+b.model+`"`, `"model":"`+b.deployed+`"`, 1)
			if got.Path != "/v1/rerank" || string(got.Body) != want {
				t.Errorf("backend got %s\n %s\nwant /v1/rerank\n %s", got.Path, got.Body, want)
			}

			line := g.settled(t, id)
			for attr, want := range map[string]any{"kaiak.backend.id": b.name, "kaiak.backend.type": b.typ,
				"gen_ai.operation.name": "rerank", "gen_ai.request.stream": false,
				"gen_ai.usage.input_tokens": float64(rerankPromptTokens), "gen_ai.usage.output_tokens": 0.0,
				"kaiak.usage.cost_usd": 0.00008, "kaiak.usage.estimated": false, "kaiak.usage.partial": false} {
				if line[attr] != want {
					t.Errorf("log line %v: %s = %v, want %v", line, attr, line[attr], want)
				}
			}
			usage := `{kaiak_key_group="w",kaiak_key_root_group="w",kaiak_key_id="k",gen_ai_request_model="` + b.model +
				`",gen_ai_operation_name="rerank",kaiak_usage_status="complete"`
			for series, want := range map[string]float64{
				`gen_ai_client_inference_usage_input_tokens_total` + usage + `,gen_ai_token_modality="unknown"}`: rerankPromptTokens,
				`kaiak_usage_cost_usd_total` + usage + `}`:                                                       0.00008,
			} {
				if got := g.metric(t, series); got != want {
					t.Errorf("%s = %v, want %v", series, got, want)
				}
			}
		})
	}

	// A chat request to the vllm reranker answers vLLM's route-missing 404: 502
	// upstream_endpoint_missing without the backend's text, neutral for the circuit
	// however many come — more than the failure threshold — with the endpoint-missing
	// warning; the deployment serves rerank right after (docs/specs/GATEWAY.md,
	// Providers → An endpoint missing from a server).
	t.Run("vllm/chat to the reranker", func(t *testing.T) {
		b := rerankBackends[0]
		for i := range 6 {
			r := g.post(t, "/v1/chat/completions", key, fmt.Sprintf("chat-to-reranker-%d", i), chatBody(b.model, false, nil))
			if r.StatusCode != http.StatusBadGateway || openAIErrorCode(t, r) != "upstream_endpoint_missing" ||
				strings.Contains(string(r.body), "Not Found") {
				t.Fatalf("%d %s, want 502 upstream_endpoint_missing", r.StatusCode, r.body)
			}
		}
		deployment := `kaiak_backend_id="vl",kaiak_deployment_model="` + b.deployed + `"`
		if v := g.metric(t, `kaiak_circuit_state{`+deployment+`,kaiak_circuit_state="open"}`); v != 0 {
			t.Errorf("vl's circuit open = %v, want closed", v)
		}
		if v := g.metric(t, `kaiak_upstream_attempts_total{`+deployment+`,kaiak_attempt_outcome="endpoint_missing"}`); v != 6 {
			t.Errorf("endpoint_missing attempts = %v, want 6", v)
		}
		line := g.logs.wait(t, "the endpoint-missing warning",
			msg("the deployment's server does not serve an endpoint its type serves", "kaiak.backend.id", "vl"))
		if line["level"] != "WARN" || line["kaiak.deployment.model"] != b.deployed || line["kaiak.endpoint"] != "chat_completions" {
			t.Errorf("warning %v, want WARN naming %s and chat_completions", line, b.deployed)
		}
		r := g.post(t, "/v1/rerank", key, "rerank-after-chat", rerankBody(t, b.model, rerankDocuments, nil))
		if r.StatusCode != http.StatusOK || !strings.Contains(string(r.body), `"results"`) {
			t.Errorf("rerank after the chat requests: %d %s, want 200 with results", r.StatusCode, r.body)
		}
	})

	// An answer without usage settles the request's input estimate — the body, plus
	// the query once more for each document beyond the first — flagged estimated,
	// priced on tokens_in.
	for _, b := range rerankBackends {
		t.Run(b.typ+"/usage missing", func(t *testing.T) {
			fake := fakes[b.name]
			fake.SetReply(fakebackend.Reply{OmitUsage: true})
			defer fake.SetReply(fakebackend.Reply{Usage: &fakebackend.Usage{PromptTokens: rerankPromptTokens}})
			id := "rerank-no-usage-" + b.name
			body := rerankBody(t, b.model, rerankDocuments, nil)
			r := g.post(t, "/v1/rerank", key, id, body)
			if r.StatusCode != http.StatusOK || strings.Contains(string(r.body), `"usage"`) {
				t.Fatalf("%d %s, want 200 and no usage", r.StatusCode, r.body)
			}
			estimate := accounting.EstimateInput(provider.Rerank, body).Total
			if bodyOnly := accounting.EstimateTokens(int64(len(body))); estimate <= bodyOnly {
				t.Fatalf("estimate %d, want the body's %d and the query twice more", estimate, bodyOnly)
			}
			line := g.settled(t, id)
			for attr, want := range map[string]any{"kaiak.backend.id": b.name, "gen_ai.operation.name": "rerank",
				"gen_ai.usage.input_tokens": float64(estimate), "gen_ai.usage.output_tokens": 0.0,
				"kaiak.usage.cost_usd": float64(estimate*2000) / 1e9, "kaiak.usage.estimated": true, "kaiak.usage.partial": false} {
				if line[attr] != want {
					t.Errorf("log line %v: %s = %v, want %v", line, attr, line[attr], want)
				}
			}
		})
	}

	// More documents than max_rerank_documents, and a model none of whose
	// deployments' types serves rerank, are refused in OpenAI's shape before any
	// backend is asked.
	t.Run("refused before any backend", func(t *testing.T) {
		before := received()
		r := g.post(t, "/v1/rerank", key, "rerank-over-cap",
			rerankBody(t, "rerank-vllm", append(slices.Clone(rerankDocuments), "the secret document"), nil))
		e, _ := r.json(t)["error"].(map[string]any)
		if r.StatusCode != http.StatusBadRequest || e["code"] != "invalid_value" || e["param"] != "documents" ||
			strings.Contains(string(r.body), "secret") {
			t.Errorf("4 documents: %d %s, want 400 invalid_value on documents, naming no document", r.StatusCode, r.body)
		}
		r = g.post(t, "/v1/rerank", key, "rerank-unserved", rerankBody(t, rerankUnserved.model, rerankDocuments, nil))
		if r.StatusCode != http.StatusBadRequest || openAIErrorCode(t, r) != "endpoint_not_served" ||
			!strings.Contains(string(r.body), "/v1/rerank") {
			t.Errorf("%s: %d %s, want 400 endpoint_not_served naming /v1/rerank", rerankUnserved.typ, r.StatusCode, r.body)
		}
		if n := received() - before; n != 0 {
			t.Errorf("the backends got %d requests for refused ones", n)
		}
	})
	g.stop(t)
}
