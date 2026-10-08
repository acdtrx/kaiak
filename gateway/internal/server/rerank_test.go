package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"kaiak/internal/accounting"
	"kaiak/internal/fakebackend"
	"kaiak/internal/provider"
)

// withRerankModels swaps in the test config, with global.max_rerank_documents 3, plus
// rerankers on the fake backend: "reranker" on a vllm backend "vl" (as
// "reranker-back"), "reranker-ls" on a llama-server backend "ls" (as
// "reranker-ls-back"), both priced at $2 per million input tokens, and "reranker-slow"
// on a vllm backend "vl-slow" whose response timeout is 150 ms and first-event
// timeout 5 s.
func withRerankModels(t *testing.T, g *testGateway) {
	t.Helper()
	g.apply(t, func(doc string) string {
		doc = replaceOnce(t, doc, `"global": { `, `"global": { "max_rerank_documents": 3, `)
		doc = replaceOnce(t, doc, `"backends": {`, `"backends": {
    "vl": { "type": "vllm", "base_url": "`+g.backend.URL()+`/v1" },
    "ls": { "type": "llama-server", "base_url": "`+g.backend.URL()+`/v1" },
    "vl-slow": { "type": "vllm", "base_url": "`+g.backend.URL()+`/v1", "response_timeout_ms": 150, "first_event_timeout_ms": 5000 },`)
		model := func(name, backend, backendModel string) string {
			return `"` + name + `": { "deployments": [{ "backend": "` + backend + `", "model": "` + backendModel + `" }],
      "metadata": { "context_length": 8192,
        "capabilities": { "streaming": false, "tools": false, "vision": false, "reasoning": false } },
      "prices": [{ "effective_from": "2020-01-01",
        "tiers": [{ "above_input_tokens": 0, "usd_per_million": { "tokens_in": 2, "tokens_out": 0 } }] }] },
    `
		}
		return replaceOnce(t, doc, `"models": { `, `"models": {
    `+model("reranker", "vl", "reranker-back")+model("reranker-ls", "ls", "reranker-ls-back")+
			model("reranker-slow", "vl-slow", "reranker-slow-back"))
	})
}

// A rerank request passes the whole pipeline on a vllm and a llama-server deployment:
// sent with the model edit alone (stream, top_n and the rest untouched), answered as
// the backend answered with the public model name, settled from the reported
// prompt_tokens and priced on tokens_in, with operation rerank on the log line and
// the usage metrics and /v1/rerank as the route (docs/specs/GATEWAY.md, Client API →
// owned fields; Accounting → rerank usage; Observability).
func TestRerankThroughThePipeline(t *testing.T) {
	for _, c := range []struct {
		model, backendType, backendModel string
		shape                            fakebackend.RerankShape
	}{
		{"reranker", "vllm", "reranker-back", fakebackend.VLLMRerank},
		{"reranker-ls", "llama-server", "reranker-ls-back", fakebackend.LlamaServerRerank},
	} {
		t.Run(c.backendType, func(t *testing.T) {
			g := newTestGateway(t)
			withRerankModels(t, g)
			g.backend.SetRerankShape(c.shape)
			g.backend.SetReply(fakebackend.Reply{Usage: &fakebackend.Usage{PromptTokens: 40}})
			body := `{"model":"` + c.model + `", "query":"what is a panda","documents":["the sky is blue","a panda is a bear"],` +
				`"top_n":1,"stream":"yes","return_documents":true}`
			w := do(t, g.h, call{method: "POST", path: "/v1/rerank", key: workloadKey, body: body,
				header: map[string]string{"X-Request-Id": "rr-1"}})
			if w.Code != http.StatusOK {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			}
			var answer struct {
				Model   string
				Results []struct {
					Index          int
					RelevanceScore float64 `json:"relevance_score"`
				}
			}
			if err := json.Unmarshal(w.Body.Bytes(), &answer); err != nil || w.Header().Get("Content-Type") != "application/json" {
				t.Fatalf("answer %s (%v), Content-Type %q", w.Body.String(), err, w.Header().Get("Content-Type"))
			}
			if answer.Model != c.model || strings.Contains(w.Body.String(), c.backendModel) ||
				len(answer.Results) != 1 || answer.Results[0].Index != 1 {
				t.Errorf("answer %s: want the public model and the panda document alone", w.Body.String())
			}

			reqs := g.backend.Requests()
			if len(reqs) != 1 || reqs[0].Path != "/v1/rerank" {
				t.Fatalf("backend requests %+v, want one to /v1/rerank", reqs)
			}
			if want := strings.Replace(body, `"model":"`+c.model+`"`, `"model":"`+c.backendModel+`"`, 1); string(reqs[0].Body) != want {
				t.Errorf("sent\n %s\nwant\n %s", reqs[0].Body, want)
			}

			rec := onlyRecord(t, g)
			expectUnits(t, rec, units(40, 0, 0, 0, 0), false, false)
			if rec.Operation != "rerank" || rec.CostNanoUSD != 80_000 {
				t.Errorf("record operation %q, cost %d nano-USD; want rerank and 80000 (40 × $2/M)", rec.Operation, rec.CostNanoUSD)
			}
			line := logLine(t, g, "rr-1")
			for _, want := range []string{`"gen_ai.operation.name":"rerank"`, `"gen_ai.request.stream":false`,
				`"kaiak.backend.type":"` + c.backendType + `"`, `"gen_ai.usage.input_tokens":40`} {
				if !strings.Contains(line, want) {
					t.Errorf("log line lacks %s: %s", want, line)
				}
			}
			usage := `{kaiak_key_group="eval",kaiak_key_root_group="research",kaiak_key_id="k-eval",gen_ai_request_model="` + c.model +
				`",gen_ai_operation_name="rerank",kaiak_usage_status="complete"`
			expectMetricLines(t, g.metricsText(),
				`http_server_request_duration_seconds_count{http_request_method="POST",url_scheme="http",http_route="/v1/rerank",`+
					`http_response_status_code="200",gen_ai_request_model="`+c.model+`"} 1`,
				`kaiak_usage_records_total`+usage+`} 1`,
				`gen_ai_client_inference_usage_input_tokens_total`+usage+`,gen_ai_token_modality="unknown"} 40`,
				`gen_ai_client_inference_usage_output_tokens_total`+usage+`,gen_ai_token_modality="unknown"} 0`)
		})
	}
}

// What the gateway refuses on /v1/rerank it refuses before routing, in OpenAI's error
// shape: a model none of whose deployments' types serves rerank, the owned fields'
// mistakes, more documents than max_rerank_documents (counts named, never a
// document), another method than POST. What is not owned passes: documents of any
// shape, a stream member of any type (docs/specs/GATEWAY.md, Client API → owned
// fields; Limits → batch caps).
func TestRerankRefusals(t *testing.T) {
	g := newTestGateway(t)
	withRerankModels(t, g)
	for _, c := range []struct {
		name   string
		call   call
		status int
		code   string
		param  string
	}{
		{name: "no key", call: call{method: "POST", path: "/v1/rerank", body: `{"model":"reranker","query":"q","documents":["a"]}`},
			status: 401, code: "missing_api_key"},
		{name: "not served on openai-compatible", call: call{method: "POST", path: "/v1/rerank", key: workloadKey,
			body: `{"model":"open","query":"q","documents":["a"]}`}, status: 400, code: "endpoint_not_served"},
		{name: "not served on azure-openai", call: call{method: "POST", path: "/v1/rerank", key: workloadKey,
			body: `{"model":"on-azure","query":"q","documents":["a"]}`}, status: 400, code: "endpoint_not_served"},
		{name: "model missing", call: call{method: "POST", path: "/v1/rerank", key: workloadKey,
			body: `{"query":"q","documents":["a"]}`}, status: 400, code: "missing_required_parameter", param: "model"},
		{name: "model not a string", call: call{method: "POST", path: "/v1/rerank", key: workloadKey,
			body: `{"model":7,"query":"q","documents":["a"]}`}, status: 400, code: "invalid_type", param: "model"},
		{name: "not JSON", call: call{method: "POST", path: "/v1/rerank", key: workloadKey, body: `["reranker"]`},
			status: 400, code: "invalid_json"},
		{name: "documents over the cap", call: call{method: "POST", path: "/v1/rerank", key: workloadKey,
			body: `{"model":"reranker","query":"q","documents":["secret-a","b","c","d"]}`}, status: 400, code: "invalid_value", param: "documents"},
		{name: "wrong method", call: call{method: "GET", path: "/v1/rerank", key: workloadKey},
			status: 405, code: "method_not_allowed"},
		{name: "body too large", call: call{method: "POST", path: "/v1/rerank", key: workloadKey,
			body: `{"model":"reranker","documents":["` + strings.Repeat("x", bodyCap) + `"]}`}, status: 413, code: "request_too_large"},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := do(t, g.h, c.call)
			expectError(t, w, c.status, c.code)
			if c.param != "" && !strings.Contains(w.Body.String(), `"param":"`+c.param+`"`) {
				t.Errorf("the refusal does not name %s: %s", c.param, w.Body.String())
			}
			if c.status == http.StatusMethodNotAllowed && w.Header().Get("Allow") != http.MethodPost {
				t.Errorf("Allow %q, want POST", w.Header().Get("Allow"))
			}
			if c.code == "endpoint_not_served" && !strings.Contains(errorMessage(t, w), "/v1/rerank") {
				t.Errorf("message %q does not name the endpoint", errorMessage(t, w))
			}
			if c.code == "invalid_value" {
				if msg := errorMessage(t, w); !strings.Contains(msg, "4 documents") || !strings.Contains(msg, "maximum is 3") ||
					strings.Contains(msg, "secret-a") {
					t.Errorf("message %q: want the counts, never a document", msg)
				}
			}
		})
	}
	if n := len(g.backend.Requests()); n != 0 {
		t.Errorf("%d requests reached the backend, want none", n)
	}

	for _, body := range []string{
		`{"model":"reranker","query":"q","documents":["a","b","c"]}`,
		`{"model":"reranker","query":"q","documents":"one document"}`,
		`{"model":"reranker","query":"q","documents":["a"],"stream":true}`,
		`{"model":"reranker","query":"q","documents":["a"],"stream":{"not":"a boolean"}}`,
	} {
		w := do(t, g.h, call{method: "POST", path: "/v1/rerank", key: workloadKey, body: body})
		if w.Code != http.StatusOK {
			t.Errorf("%s: status %d: %s", body, w.Code, w.Body.String())
		}
		reqs := g.backend.Requests()
		if sent := string(reqs[len(reqs)-1].Body); sent != strings.Replace(body, `"reranker"`, `"reranker-back"`, 1) {
			t.Errorf("sent %s for %s", sent, body)
		}
	}

	// A backend 5xx is answered by the gateway, in the same shape.
	g.backend.SetReply(fakebackend.Reply{Status: http.StatusInternalServerError})
	expectError(t, do(t, g.h, call{method: "POST", path: "/v1/rerank", key: workloadKey,
		body: `{"model":"reranker","query":"q","documents":["a"]}`}), http.StatusInternalServerError, "upstream_error")
}

// A rerank request carrying texts, TEI's rerank format, is refused before routing on
// every backend type, whatever texts holds — null too, and beside documents:
// llama-server reads a request naming texts as TEI's, takes the documents from it
// when documents is absent or not a list of strings, past the documents cap, and
// answers without usage. The refusal names the member, never its value
// (docs/specs/GATEWAY.md, Client API → owned fields: rerank).
func TestRerankRefusesTEIFormat(t *testing.T) {
	g := newTestGateway(t)
	withRerankModels(t, g)
	g.backend.SetRerankShape(fakebackend.LlamaServerRerank)
	for _, body := range []string{
		`{"model":"reranker-ls","query":"q","texts":["secret-a","b","c","d","e","f","g","h"]}`,
		`{"model":"reranker-ls","query":"q","documents":["a"],"texts":[]}`,
		`{"model":"reranker-ls","query":"q","texts":null,"documents":["a"]}`,
		`{"model":"reranker-ls","query":"q","documents":"a","texts":"secret-a"}`,
		`{"model":"reranker","query":"q","documents":["a"],"texts":["secret-a"]}`,
	} {
		w := do(t, g.h, call{method: "POST", path: "/v1/rerank", key: workloadKey, body: body})
		expectError(t, w, http.StatusBadRequest, "tei_format_unsupported")
		if typ, _, param := openAIError(t, w); typ != "invalid_request_error" || param == nil || *param != "texts" {
			t.Errorf("%s: type %q, param %v, want invalid_request_error on texts", body, typ, param)
		}
		if msg := errorMessage(t, w); !strings.Contains(msg, "'texts'") || strings.Contains(msg, "secret-a") {
			t.Errorf("%s: message %q, want texts named and its value not", body, msg)
		}
	}
	if n := len(g.backend.Requests()); n != 0 {
		t.Errorf("%d requests reached the backend, want none", n)
	}
}

// A rerank answer without usage settles the request's input estimate, flagged
// estimated, and no output: nothing is generated (docs/specs/GATEWAY.md, Accounting).
func TestRerankUsageMissingIsEstimated(t *testing.T) {
	g := newTestGateway(t)
	withRerankModels(t, g)
	g.backend.SetReply(fakebackend.Reply{OmitUsage: true})
	body := `{"model":"reranker","query":"what is a panda","documents":["the sky is blue","a panda is a bear","bamboo"]}`
	if w := do(t, g.h, call{method: "POST", path: "/v1/rerank", key: workloadKey, body: body}); w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	input := accounting.EstimateInput(provider.Rerank, []byte(body)).Total
	if body := accounting.EstimateTokens(int64(len(body))); input <= body {
		t.Fatalf("estimate %d, want the body's %d and the query twice more", input, body)
	}
	expectUnits(t, onlyRecord(t, g), units(input, 0, 0, 0, 0), true, false)
}

// A rerank request is never a stream, whatever its stream member says: the backend's
// response timeout bounds it, not the first-event timeout, and it is not retried
// (docs/specs/GATEWAY.md, Client API → owned fields; Routing and reliability →
// Timeouts).
func TestRerankTakesTheNonStreamTimers(t *testing.T) {
	g := newTestGateway(t)
	withRerankModels(t, g)
	g.backend.SetReply(fakebackend.Reply{Before: fakebackend.StallFirstByte})
	w := do(t, g.h, call{method: "POST", path: "/v1/rerank", key: workloadKey,
		body: `{"model":"reranker-slow","query":"q","documents":["a"],"stream":true}`})
	expectError(t, w, http.StatusGatewayTimeout, "upstream_timeout")
	if n := len(g.backend.Requests()); n != 1 {
		t.Errorf("backend got %d requests, want 1: a response timeout is not retried", n)
	}
	logs := g.logText()
	for _, want := range []string{`no response within 150ms`, `"gen_ai.request.stream":false`} {
		if !strings.Contains(logs, want) {
			t.Errorf("log misses %s:\n%s", want, logs)
		}
	}
}

// /v1/models lists rerank for a model with a vllm or llama-server deployment, and not
// for one whose deployments' types have no rerank API (docs/specs/GATEWAY.md, Client
// API → /v1/models).
func TestModelEntriesListRerank(t *testing.T) {
	g := newTestGateway(t)
	withRerankModels(t, g)
	for model, want := range map[string]string{
		"reranker":    `"endpoints":["chat_completions","completions","embeddings","messages","messages_count_tokens","rerank","responses"]`,
		"reranker-ls": `"endpoints":["chat_completions","completions","embeddings","messages","messages_count_tokens","rerank","responses","responses_input_tokens"]`,
		"open":        `"endpoints":["chat_completions","completions","embeddings"]`,
		"on-azure":    `"endpoints":["chat_completions","completions","embeddings","responses"]`,
	} {
		w := do(t, g.h, call{method: "GET", path: "/v1/models/" + model, key: workloadKey})
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), want) {
			t.Errorf("%s: %d %s, want %s", model, w.Code, w.Body.String(), want)
		}
	}
}
