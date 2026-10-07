package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"kaiak/internal/config"
	"kaiak/internal/fakebackend"
)

// withResponsesModels swaps in the test config plus a llama-server backend "ls" on the
// fake backend serving model "resp" (as "resp-back", output limit default 64, ceiling
// 128) — llama-server serves Responses and its token counting — and a vllm backend
// "vl" serving model "resp-vllm", which has no token counting.
func withResponsesModels(t *testing.T, g *testGateway) {
	t.Helper()
	g.apply(t, func(doc string) string {
		doc = replaceOnce(t, doc, `"backends": {`, `"backends": {
    "ls": { "type": "llama-server", "base_url": "`+g.backend.URL()+`/v1" },
    "vl": { "type": "vllm", "base_url": "`+g.backend.URL()+`/v1" },`)
		return replaceOnce(t, doc, `"models": { `, `"models": {
    "resp": { "deployments": [{ "backend": "ls", "model": "resp-back" }],
      "metadata": { "context_length": 8192,
        "capabilities": { "streaming": true, "tools": true, "vision": false, "reasoning": true } },
      "output_limit": { "default": 64, "ceiling": 128 } },
    "resp-vllm": { "deployments": [{ "backend": "vl", "model": "resp-vllm-back" }],
      "metadata": { "context_length": 8192,
        "capabilities": { "streaming": true, "tools": true, "vision": false, "reasoning": true } } },
    `)
	})
}

const responsesBody = `{"model":"resp","input":"hello there"}`

// Every answer the gateway gives itself on the Responses endpoints is in OpenAI's
// shape, and the stateful parts of the API are refused before routing
// (docs/specs/GATEWAY.md, Client API → Responses is stateless).
func TestResponsesRefusals(t *testing.T) {
	g := newTestGateway(t)
	withResponsesModels(t, g)
	for _, c := range []struct {
		name   string
		call   call
		status int
		code   string
		param  string
	}{
		{name: "no key", call: call{method: "POST", path: "/v1/responses", body: responsesBody},
			status: 401, code: "missing_api_key"},
		{name: "not served", call: call{method: "POST", path: "/v1/responses", key: workloadKey,
			body: `{"model":"open","input":"hi"}`}, status: 400, code: "endpoint_not_served"},
		{name: "input_tokens not served on vllm", call: call{method: "POST", path: "/v1/responses/input_tokens", key: workloadKey,
			body: `{"model":"resp-vllm","input":"hi"}`}, status: 400, code: "endpoint_not_served"},
		{name: "previous_response_id", call: call{method: "POST", path: "/v1/responses", key: workloadKey,
			body: `{"model":"resp","input":"hi","previous_response_id":"resp_1"}`}, status: 400,
			code: "stateful_responses_unsupported", param: "previous_response_id"},
		{name: "conversation", call: call{method: "POST", path: "/v1/responses", key: workloadKey,
			body: `{"model":"resp","input":"hi","conversation":{"id":"conv_1"}}`}, status: 400,
			code: "stateful_responses_unsupported", param: "conversation"},
		{name: "background", call: call{method: "POST", path: "/v1/responses", key: workloadKey,
			body: `{"model":"resp","input":"hi","background":true}`}, status: 400,
			code: "stateful_responses_unsupported", param: "background"},
		{name: "background not a boolean", call: call{method: "POST", path: "/v1/responses", key: workloadKey,
			body: `{"model":"resp","input":"hi","background":"yes"}`}, status: 400, code: "invalid_type", param: "background"},
		{name: "stateful on input_tokens", call: call{method: "POST", path: "/v1/responses/input_tokens", key: workloadKey,
			body: `{"model":"resp","input":"hi","previous_response_id":"resp_1"}`}, status: 400,
			code: "stateful_responses_unsupported", param: "previous_response_id"},
		{name: "max_output_tokens above the context", call: call{method: "POST", path: "/v1/responses", key: workloadKey,
			body: `{"model":"resp","input":"hi","max_output_tokens":9000}`}, status: 400, code: "invalid_value", param: "max_output_tokens"},
		{name: "max_output_tokens not an integer", call: call{method: "POST", path: "/v1/responses", key: workloadKey,
			body: `{"model":"resp","input":"hi","max_output_tokens":"many"}`}, status: 400, code: "invalid_type", param: "max_output_tokens"},
		{name: "wrong method", call: call{method: "GET", path: "/v1/responses", key: workloadKey},
			status: 405, code: "method_not_allowed"},
		{name: "a stored response", call: call{method: "GET", path: "/v1/responses/resp_123", key: workloadKey},
			status: 404, code: "unknown_url"},
		{name: "cancel", call: call{method: "POST", path: "/v1/responses/resp_123/cancel", key: workloadKey},
			status: 404, code: "unknown_url"},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := do(t, g.h, c.call)
			expectError(t, w, c.status, c.code)
			if c.param != "" && !strings.Contains(w.Body.String(), `"param":"`+c.param+`"`) {
				t.Errorf("the refusal does not name %s: %s", c.param, w.Body.String())
			}
		})
	}
	if n := len(g.backend.Requests()); n != 0 {
		t.Errorf("%d requests reached the backend, want none", n)
	}

	// null counts as absent, and background: false asks for nothing kept.
	w := do(t, g.h, call{method: "POST", path: "/v1/responses", key: workloadKey,
		body: `{"model":"resp","input":"hi","previous_response_id":null,"conversation":null,"background":false}`})
	if w.Code != http.StatusOK {
		t.Fatalf("absent stateful fields: status %d: %s", w.Code, w.Body.String())
	}
}

// A reference to an item stored at the backend — an item_reference, or its short
// form with neither type nor role — is refused: Responses is stateless.
func TestResponsesStoredItemReferenceRefused(t *testing.T) {
	for _, item := range []string{`{"type":"item_reference","id":"msg_saved"}`, `{"id":"msg_saved"}`} {
		t.Run(item, func(t *testing.T) {
			g := newTestGateway(t)
			withResponsesModels(t, g)
			body := `{"model":"resp","input":[` + item + `]}`
			w := do(t, g.h, call{method: "POST", path: "/v1/responses", key: workloadKey, body: body})
			if got := len(g.backend.Requests()); got != 0 {
				t.Errorf("stored item reference reached backend (%d request); status=%d", got, w.Code)
			}
			expectError(t, w, http.StatusBadRequest, "stateful_responses_unsupported")
		})
	}
}

// An allowed_tools tool_choice naming its tools twice is refused: one list would
// pass unchecked.
func TestResponsesDuplicateAllowedToolsRefused(t *testing.T) {
	g := newTestGateway(t)
	withResponsesModels(t, g)
	body := `{"model":"resp","input":"hi","tool_choice":{"type":"allowed_tools","tools":[{"type":"web_search"}],"tools":[{"type":"function","name":"f"}]}}`
	w := do(t, g.h, call{method: "POST", path: "/v1/responses", key: workloadKey, body: body})
	if reqs := g.backend.Requests(); len(reqs) != 0 {
		if !strings.Contains(string(reqs[0].Body), `"type":"web_search"`) {
			t.Fatal("reproduction did not forward the unchecked occurrence")
		}
		t.Errorf("duplicate tools member, including unchecked hosted tool, reached backend")
	}
	expectError(t, w, http.StatusBadRequest, "duplicate_member")
}

// The tools the backend would run are refused before routing, in tools and in
// tool_choice, on both Responses endpoints; the tools the client runs pass.
func TestResponsesHostedToolsAreRefused(t *testing.T) {
	g := newTestGateway(t)
	withResponsesModels(t, g)
	for _, c := range []struct{ body, param string }{
		{`"tools":[{"type":"function","name":"f","parameters":{}},{"type":"web_search"}]`, "tools[1].type"},
		{`"tools":[{"type":"mcp","server_label":"x","server_url":"https://example.com"}]`, "tools[0].type"},
		{`"tools":[{"type":"code_interpreter","container":{"type":"auto"}}]`, "tools[0].type"},
		{`"tool_choice":{"type":"web_search_preview"}`, "tool_choice.type"},
		{`"tool_choice":{"type":"allowed_tools","mode":"auto","tools":[{"type":"file_search"}]}`, "tool_choice.tools[0].type"},
	} {
		for _, path := range []string{"/v1/responses", "/v1/responses/input_tokens"} {
			w := do(t, g.h, call{method: "POST", path: path, key: workloadKey,
				body: `{"model":"resp","input":"hi",` + c.body + `}`})
			expectError(t, w, http.StatusBadRequest, "hosted_tool_unsupported")
			if !strings.Contains(w.Body.String(), `"param":"`+c.param+`"`) {
				t.Errorf("%s: the refusal does not name %s: %s", path, c.param, w.Body.String())
			}
		}
	}
	for _, c := range []struct{ name, body, code string }{
		{"type twice", `"tools":[{"type":"function","name":"f","type":"web_search"}]`, "duplicate_member"},
		{"tool_choice type twice", `"tool_choice":{"type":"function","type":"web_search"}`, "duplicate_member"},
		{"tool_choice not a string or object", `"tool_choice":[1]`, "invalid_type"},
		{"tools not a list", `"tools":{"type":"function"}`, "invalid_type"},
	} {
		w := do(t, g.h, call{method: "POST", path: "/v1/responses", key: workloadKey,
			body: `{"model":"resp","input":"hi",` + c.body + `}`})
		expectError(t, w, http.StatusBadRequest, c.code)
	}
	if n := len(g.backend.Requests()); n != 0 {
		t.Fatalf("%d requests reached the backend, want none", n)
	}

	clientTools := `"tools":[{"type":"function","name":"f","parameters":{}},{"type":"custom","name":"g"},
		{"type":"local_shell"},{"type":"shell"},{"type":"apply_patch"}]`
	for _, choice := range []string{`"auto"`, `"required"`, `{"type":"function","name":"f"}`, `{"type":"custom","name":"g"}`,
		`{"type":"allowed_tools","mode":"auto","tools":[{"type":"function","name":"f"}]}`} {
		w := do(t, g.h, call{method: "POST", path: "/v1/responses", key: workloadKey,
			body: `{"model":"resp","input":"hi",` + clientTools + `,"tool_choice":` + choice + `}`})
		if w.Code != http.StatusOK {
			t.Fatalf("client tools, tool_choice %s: status %d: %s", choice, w.Code, w.Body.String())
		}
	}
}

// The shell discriminator alone does not establish client-side execution.
func TestResponsesHostedShellRefused(t *testing.T) {
	for _, environment := range []string{`{"type":"container_auto"}`, `{"type":"container_reference","container_id":"cntr_test"}`} {
		t.Run(environment, func(t *testing.T) {
			g := newTestGateway(t)
			withResponsesModels(t, g)
			body := `{"model":"resp","input":"run a command","tools":[{"type":"shell","environment":` + environment + `}]}`
			w := do(t, g.h, call{method: "POST", path: "/v1/responses", key: workloadKey, body: body})
			if got := len(g.backend.Requests()); got != 0 {
				t.Errorf("hosted shell reached backend (%d request); status=%d", got, w.Code)
			}
			expectError(t, w, http.StatusBadRequest, "hosted_tool_unsupported")
		})
	}
}

// Responses can introduce real tool definitions through an input item too.
func TestResponsesInputItemHostedToolsRefused(t *testing.T) {
	for _, path := range []string{"/v1/responses", "/v1/responses/input_tokens"} {
		t.Run(path, func(t *testing.T) {
			g := newTestGateway(t)
			withResponsesModels(t, g)
			body := `{"model":"resp","input":[{"type":"additional_tools","role":"developer","tools":[{"type":"web_search"}]},{"role":"user","content":"search the web"}]}`
			w := do(t, g.h, call{method: "POST", path: path, key: workloadKey, body: body})
			if got := len(g.backend.Requests()); got != 0 {
				t.Errorf("input-supplied hosted tool reached backend (%d request); status=%d", got, w.Code)
			}
			expectError(t, w, http.StatusBadRequest, "hosted_tool_unsupported")
		})
	}
}

// Responses passes through with the gateway's owned edits only: the deployment's model
// name, store: false and the output limit under max_output_tokens; the answer carries
// the public name and settles into a record of its usage, mapped onto kaiak's units.
func TestResponsesPassThrough(t *testing.T) {
	g := newTestGateway(t)
	withResponsesModels(t, g)
	g.backend.SetReply(fakebackend.Reply{Usage: &fakebackend.Usage{PromptTokens: 30, CompletionTokens: 9,
		CachedTokens: 10, CacheWriteTokens: 5, ReasoningTokens: 4}})

	body := `{"model":"resp","input":[{"role":"user","content":"hi"}],"instructions":"be brief","max_output_tokens":500,` +
		`"store":true,"reasoning":{"effort":"low"},"include":["reasoning.encrypted_content"],"service_tier":"priority"}`
	w := do(t, g.h, call{method: "POST", path: "/v1/responses", key: workloadKey, body: body})
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	reqs := g.backend.Requests()
	if len(reqs) != 1 || reqs[0].Path != "/v1/responses" {
		t.Fatalf("backend requests: %+v", reqs)
	}
	sent := string(reqs[0].Body)
	for _, want := range []string{`"model":"resp-back"`, `"max_output_tokens":128`, `"store":false`, `"instructions":"be brief"`,
		`"reasoning":{"effort":"low"}`, `"include":["reasoning.encrypted_content"]`, `"service_tier":"priority"`} {
		if !strings.Contains(sent, want) {
			t.Errorf("sent body lacks %s: %s", want, sent)
		}
	}
	if strings.Contains(sent, "stream_options") || strings.Contains(sent, `"store":true`) {
		t.Errorf("sent body: %s", sent)
	}
	if !strings.Contains(w.Body.String(), `"model":"resp"`) || strings.Contains(w.Body.String(), "resp-back") {
		t.Errorf("answer does not carry the public model name: %s", w.Body.String())
	}
	rec := settledRecord(t, g)
	u := rec.Units
	if u[config.UnitTokensIn] != 15 || u[config.UnitTokensCached] != 10 || u[config.UnitTokensCacheWrite] != 5 ||
		u[config.UnitTokensOut] != 9 || u[config.UnitTokensReasoning] != 4 || rec.Estimated || rec.Partial {
		t.Errorf("record %+v", rec)
	}

	// No max_output_tokens: the default is set, fitted to the context; store is added.
	do(t, g.h, call{method: "POST", path: "/v1/responses", key: workloadKey, body: responsesBody})
	if sent := string(g.backend.Requests()[1].Body); !strings.Contains(sent, `"max_output_tokens":64`) ||
		!strings.Contains(sent, `"store":false`) {
		t.Errorf("no default output limit or store: %s", sent)
	}
	if !strings.Contains(g.logText(), `"gen_ai.operation.name":"chat"`) {
		t.Errorf("log line:\n%s", g.logText())
	}
}

// The OpenAI types run every Responses request on the standard tier, the client's
// tier replaced and an absent one added; token counting keeps the client's.
func TestResponsesServiceTierOnAzure(t *testing.T) {
	g := newTestGateway(t)
	for _, c := range []struct{ path, body, want, absent string }{
		{"/v1/responses", `{"model":"on-azure","input":"hi"}`, `"service_tier":"default"`, ""},
		{"/v1/responses", `{"model":"on-azure","input":"hi","service_tier":"priority"}`, `"service_tier":"default"`, "priority"},
	} {
		w := do(t, g.h, call{method: "POST", path: c.path, key: workloadKey, body: c.body})
		if w.Code != http.StatusOK {
			t.Fatalf("status %d: %s", w.Code, w.Body.String())
		}
		reqs := g.backend.Requests()
		sent := reqs[len(reqs)-1]
		if sent.Path != "/openai/v1/responses" || !strings.Contains(string(sent.Body), c.want) ||
			(c.absent != "" && strings.Contains(string(sent.Body), c.absent)) {
			t.Errorf("%s: sent %s %s", c.body, sent.Path, sent.Body)
		}
	}
}

// A Responses stream is relayed as the backend sends it, with the public model name in
// the response.* events; it is complete at response.completed, and its usage is read
// from the response that event carries.
func TestResponsesStream(t *testing.T) {
	g := newTestGateway(t)
	withResponsesModels(t, g)
	g.backend.SetReply(fakebackend.Reply{Usage: &fakebackend.Usage{PromptTokens: 12, CompletionTokens: 4, CachedTokens: 2}})
	srv := httptest.NewServer(g.h)
	defer srv.Close()

	resp := postStream(t, srv.URL+"/v1/responses", `{"model":"resp","stream":true,"input":"hi"}`)
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	text := string(data)
	if resp.StatusCode != http.StatusOK || !strings.Contains(text, "event: response.completed") {
		t.Fatalf("status %d:\n%s", resp.StatusCode, text)
	}
	if strings.Count(text, `"model":"resp"`) != 3 || strings.Contains(text, "resp-back") {
		t.Errorf("the response events do not carry the public model name:\n%s", text)
	}
	rec := settledRecord(t, g)
	u := rec.Units
	if u[config.UnitTokensIn] != 10 || u[config.UnitTokensCached] != 2 || u[config.UnitTokensOut] != 4 || rec.Partial || rec.Estimated {
		t.Errorf("record %+v", rec)
	}

	// Cut by the output limit, the stream ends with response.incomplete: whole.
	g.backend.SetReply(fakebackend.Reply{HonorMaxTokens: true})
	resp = postStream(t, srv.URL+"/v1/responses", `{"model":"resp","stream":true,"input":"hi","max_output_tokens":2}`)
	data, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || !strings.Contains(string(data), "event: response.incomplete") {
		t.Fatalf("cut stream (%v):\n%s", err, data)
	}
	if rec := settledRecord(t, g); rec.Partial || rec.Units[config.UnitTokensOut] != 2 {
		t.Errorf("record %+v", rec)
	}
}

// A Responses stream broken off by an error event reaches the client up to and with
// the event, then the connection is cut; ended without response.completed it is
// incomplete too. An error event as the first event is a backend failure answered as a
// 5xx.
func TestResponsesErrorEvents(t *testing.T) {
	g := newTestGateway(t)
	withResponsesModels(t, g)
	srv := httptest.NewServer(g.h)
	defer srv.Close()

	for _, reply := range []fakebackend.Reply{{Fault: &fakebackend.StreamFault{At: 2, Kind: fakebackend.ErrorEvent}}, {Fault: &fakebackend.StreamFault{At: 2, Kind: fakebackend.End}}} {
		g.backend.SetReply(reply)
		resp := postStream(t, srv.URL+"/v1/responses", `{"model":"resp","stream":true,"input":"hi"}`)
		data, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err == nil {
			t.Errorf("the stream ended cleanly; want the connection cut:\n%s", data)
		}
		if strings.Contains(string(data), "response.completed") || (reply.Fault.Kind == fakebackend.ErrorEvent && !strings.Contains(string(data), "server_error")) {
			t.Errorf("relayed:\n%s", data)
		}
		if rec := settledRecord(t, g); !rec.Partial {
			t.Errorf("record not partial: %+v", rec)
		}
	}
	waitLog(t, g, `"kaiak.relay_end":"upstream_incomplete"`)

	g.backend.SetReply(fakebackend.Reply{Fault: &fakebackend.StreamFault{At: 0, Kind: fakebackend.ErrorEvent}})
	w := do(t, g.h, call{method: "POST", path: "/v1/responses", key: workloadKey,
		body: `{"model":"resp","stream":true,"input":"hi"}`})
	expectError(t, w, http.StatusBadGateway, "upstream_error")
}

// The token-counting endpoint passes through to the backend's, without store or an
// output limit, counts against requests-per-minute limits, reserves no tokens and
// leaves no record.
func TestResponsesInputTokens(t *testing.T) {
	g := newTestGateway(t)
	withResponsesModels(t, g)
	g.backend.SetReply(fakebackend.Reply{Usage: &fakebackend.Usage{PromptTokens: 21}})
	w := do(t, g.h, call{method: "POST", path: "/v1/responses/input_tokens", key: workloadKey,
		body: `{"model":"resp","input":"hi","max_output_tokens":9000}`})
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"input_tokens":21`) {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	reqs := g.backend.Requests()
	if len(reqs) != 1 || reqs[0].Path != "/v1/responses/input_tokens" {
		t.Fatalf("backend requests: %+v", reqs)
	}
	if sent := string(reqs[0].Body); !strings.Contains(sent, `"max_output_tokens":9000`) || strings.Contains(sent, "store") {
		t.Errorf("sent body: %s", sent)
	}
	if n := len(g.usage.all()); n != 0 {
		t.Errorf("%d usage records, want none", n)
	}
	if !strings.Contains(g.logText(), `"url.path":"/v1/responses/input_tokens"`) || strings.Contains(g.logText(), `"gen_ai.operation.name"`) {
		t.Errorf("log line:\n%s", g.logText())
	}
}

// A limit refusal on a Responses endpoint is OpenAI's answer, with its headers; the
// token-counting endpoint reserves no tokens.
func TestResponsesLimitRefusals(t *testing.T) {
	g := newTestGateway(t)
	g.apply(t, func(doc string) string {
		doc = replaceOnce(t, doc, `"backends": {`, `"backends": {
    "ls": { "type": "llama-server", "base_url": "`+g.backend.URL()+`/v1" },`)
		doc = replaceOnce(t, doc, `"models": { `, `"models": {
    "resp": { "deployments": [{ "backend": "ls", "model": "resp-back" }],
      "metadata": { "context_length": 8192,
        "capabilities": { "streaming": true, "tools": true, "vision": false, "reasoning": true } } },
    `)
		return replaceOnce(t, doc, `"allowed_models": ["*"] }`,
			`"allowed_models": ["*"], "limits": [{ "type": "requests_per_minute", "value": 2 }, { "type": "tokens_per_minute", "value": 1 }] }`)
	})
	for range 2 {
		w := do(t, g.h, call{method: "POST", path: "/v1/responses/input_tokens", key: workloadKey,
			body: `{"model":"resp","input":"a long enough prompt"}`})
		if w.Code != http.StatusOK {
			t.Fatalf("input_tokens: status %d: %s", w.Code, w.Body.String())
		}
	}
	w := do(t, g.h, call{method: "POST", path: "/v1/responses", key: workloadKey, body: responsesBody})
	expectError(t, w, http.StatusTooManyRequests, "rate_limit_exceeded")
	if w.Header().Get("Retry-After") == "" || w.Header().Get("x-ratelimit-limit-requests") != "2" {
		t.Errorf("refusal headers: %v", w.Header())
	}
}
