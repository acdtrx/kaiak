package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"kaiak/internal/config"
	"kaiak/internal/fakebackend"
)

// withMessagesModels swaps in the test config plus a vllm backend "vl" on the fake
// backend serving model "msg" (as "msg-back", output limit default 64, ceiling 128),
// then applies edit (nil = none) to the document.
func withMessagesModels(t *testing.T, g *testGateway, edit func(doc string) string) {
	t.Helper()
	s := testSnapshotWith(t, g.backend.URL(), func(doc string) string {
		doc = strings.Replace(doc, `"backends": {`, `"backends": {
    "vl": { "type": "vllm", "base_url": "`+g.backend.URL()+`/v1" },`, 1)
		doc = strings.Replace(doc, `"models": { `, `"models": {
    "msg": { "deployments": [{ "backend": "vl", "model": "msg-back" }],
      "metadata": { "context_length": 8192,
        "capabilities": { "streaming": true, "tools": true, "vision": false, "reasoning": false } },
      "output_limit": { "default": 64, "ceiling": 128 } },
    `, 1)
		if edit != nil {
			doc = edit(doc)
		}
		return doc
	})
	g.holder.Swap(s)
	g.router.Configure(s)
}

// anthropicError decodes an answer in Anthropic's error shape — failing on any other
// — and returns its error type and kaiak's code.
func anthropicError(t *testing.T, w *httptest.ResponseRecorder) (typ, code string) {
	t.Helper()
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type %q, want application/json", ct)
	}
	var body struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	dec := json.NewDecoder(bytes.NewReader(w.Body.Bytes()))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil || body.Type != "error" || body.Error.Message == "" {
		t.Fatalf("not an Anthropic error body (%v): %s", err, w.Body.String())
	}
	return body.Error.Type, body.Error.Code
}

func expectAnthropicError(t *testing.T, w *httptest.ResponseRecorder, status int, typ, code string) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status %d, want %d: %s", w.Code, status, w.Body.String())
	}
	gotType, gotCode := anthropicError(t, w)
	if gotType != typ || gotCode != code {
		t.Errorf("error %s/%s, want %s/%s", gotType, gotCode, typ, code)
	}
}

const messagesBody = `{"model":"msg","max_tokens":32,"messages":[{"role":"user","content":"hello there"}]}`

// Every answer the gateway gives itself on the Messages endpoints is in Anthropic's
// shape, its type following the status and kaiak's code beside it
// (docs/specs/GATEWAY.md, Client API → Errors).
func TestMessagesErrorsTakeAnthropicsShape(t *testing.T) {
	g := newTestGateway(t)
	withMessagesModels(t, g, nil)
	for _, c := range []struct {
		name         string
		call         call
		status       int
		typ, code    string
		wantsBearer  bool
		backendCalls int
	}{
		{name: "no key", call: call{method: "POST", path: "/v1/messages", body: messagesBody},
			status: 401, typ: "authentication_error", code: "missing_api_key", wantsBearer: true},
		{name: "unknown key in x-api-key", call: call{method: "POST", path: "/v1/messages", body: messagesBody,
			header: map[string]string{"X-Api-Key": "kaiak-nope"}},
			status: 401, typ: "authentication_error", code: "invalid_api_key", wantsBearer: true},
		{name: "unknown model", call: call{method: "POST", path: "/v1/messages", key: workloadKey,
			body: `{"model":"nope","max_tokens":1,"messages":[]}`}, status: 404, typ: "not_found_error", code: "model_not_found"},
		{name: "not served", call: call{method: "POST", path: "/v1/messages", key: workloadKey,
			body: `{"model":"open","max_tokens":1,"messages":[]}`}, status: 400, typ: "invalid_request_error", code: "endpoint_not_served"},
		{name: "count_tokens not served", call: call{method: "POST", path: "/v1/messages/count_tokens", key: workloadKey,
			body: `{"model":"open","messages":[]}`}, status: 400, typ: "invalid_request_error", code: "endpoint_not_served"},
		{name: "not json", call: call{method: "POST", path: "/v1/messages", key: workloadKey, body: `[]`},
			status: 400, typ: "invalid_request_error", code: "invalid_json"},
		{name: "max_tokens above the context", call: call{method: "POST", path: "/v1/messages", key: workloadKey,
			body: `{"model":"msg","max_tokens":9000,"messages":[]}`}, status: 400, typ: "invalid_request_error", code: "invalid_value"},
		{name: "wrong method", call: call{method: "GET", path: "/v1/messages", key: workloadKey},
			status: 405, typ: "invalid_request_error", code: "method_not_allowed"},
		{name: "body too large", call: call{method: "POST", path: "/v1/messages", key: workloadKey,
			body: `{"model":"msg","pad":"` + strings.Repeat("x", bodyCap) + `"}`}, status: 413, typ: "request_too_large", code: "request_too_large"},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := do(t, g.h, c.call)
			expectAnthropicError(t, w, c.status, c.typ, c.code)
			if got := w.Header().Get("WWW-Authenticate") != ""; got != c.wantsBearer {
				t.Errorf("WWW-Authenticate set: %v, want %v", got, c.wantsBearer)
			}
		})
	}
	if n := len(g.backend.Requests()); n != 0 {
		t.Errorf("%d requests reached the backend, want none", n)
	}

	// A backend 5xx is answered by the gateway, in the same shape.
	g.backend.SetReply(fakebackend.Reply{Status: http.StatusServiceUnavailable, Header: map[string]string{"Retry-After": "3"}})
	w := do(t, g.h, call{method: "POST", path: "/v1/messages", key: workloadKey, body: messagesBody})
	expectAnthropicError(t, w, http.StatusServiceUnavailable, "overloaded_error", "upstream_error")
	if w.Header().Get("Retry-After") != "3" {
		t.Errorf("Retry-After %q, want the backend's 3", w.Header().Get("Retry-After"))
	}

	// The OpenAI endpoints keep OpenAI's shape.
	expectError(t, do(t, g.h, call{method: "POST", path: "/v1/chat/completions", body: chatBody}),
		http.StatusUnauthorized, "missing_api_key")
}

// A limit refusal on a Messages endpoint keeps its Retry-After and rate-limit headers
// in Anthropic's shape; the token-counting endpoint counts as a request and reserves
// no tokens.
func TestMessagesLimitRefusals(t *testing.T) {
	g := newTestGateway(t)
	withMessagesModels(t, g, func(doc string) string {
		return strings.Replace(doc, `"allowed_models": ["*"] }`,
			`"allowed_models": ["*"], "limits": [{ "type": "requests_per_minute", "value": 2 }, { "type": "tokens_per_minute", "value": 1 }] }`, 1)
	})
	// A token-counting request reserves nothing, so a 1-token budget admits it; it
	// still counts against the requests-per-minute limit.
	for range 2 {
		w := do(t, g.h, call{method: "POST", path: "/v1/messages/count_tokens", key: workloadKey,
			body: `{"model":"msg","messages":[{"role":"user","content":"a long enough prompt"}]}`})
		if w.Code != http.StatusOK {
			t.Fatalf("count_tokens: status %d: %s", w.Code, w.Body.String())
		}
	}
	w := do(t, g.h, call{method: "POST", path: "/v1/messages", key: workloadKey, body: messagesBody})
	expectAnthropicError(t, w, http.StatusTooManyRequests, "rate_limit_error", "rate_limit_exceeded")
	if w.Header().Get("Retry-After") == "" || w.Header().Get("x-ratelimit-limit-requests") != "2" {
		t.Errorf("refusal headers: %v", w.Header())
	}
	if n := len(g.usage.all()); n != 0 {
		t.Errorf("%d usage records, want none: token counting is not billed", n)
	}
}

// The tools the backend would run are refused before routing, on both Messages
// endpoints; the tools the client runs pass (docs/specs/GATEWAY.md, Client API →
// hosted tools are refused).
func TestMessagesHostedToolsAreRefused(t *testing.T) {
	g := newTestGateway(t)
	withMessagesModels(t, g, nil)
	for _, c := range []struct{ body, param string }{
		{`"tools":[{"name":"f","input_schema":{}},{"type":"web_search_20250305","name":"web_search"}]`, "tools[1].type"},
		{`"tools":[{"type":"code_execution_20250825","name":"code"}]`, "tools[0].type"},
		// A client tool's prefix admits only its dated versions: a toolset sharing the
		// prefix is another tool.
		{`"tools":[{"type":"computer_toolset_20260801","name":"computer"}]`, "tools[0].type"},
		{`"tools":[{"type":"bash_","name":"bash"}]`, "tools[0].type"},
		{`"mcp_servers":[{"type":"url","url":"https://example.com","name":"x"}]`, "mcp_servers"},
		{`"container":"c-1"`, "container"},
	} {
		for _, path := range []string{"/v1/messages", "/v1/messages/count_tokens"} {
			w := do(t, g.h, call{method: "POST", path: path, key: workloadKey,
				body: `{"model":"msg","max_tokens":8,"messages":[],` + c.body + `}`})
			expectAnthropicError(t, w, http.StatusBadRequest, "invalid_request_error", "hosted_tool_unsupported")
			if !strings.Contains(w.Body.String(), c.param) {
				t.Errorf("%s: the refusal does not name %s: %s", path, c.param, w.Body.String())
			}
		}
	}
	for _, c := range []struct{ name, tools, code string }{
		{"type twice", `[{"type":"custom","name":"f","type":"web_search_20250305"}]`, "duplicate_member"},
		{"type not a string", `[{"type":1}]`, "invalid_type"},
		{"tool not an object", `["f"]`, "invalid_type"},
		{"tools not a list", `{"type":"custom"}`, "invalid_type"},
	} {
		w := do(t, g.h, call{method: "POST", path: "/v1/messages", key: workloadKey,
			body: `{"model":"msg","max_tokens":8,"messages":[],"tools":` + c.tools + `}`})
		expectAnthropicError(t, w, http.StatusBadRequest, "invalid_request_error", c.code)
	}
	if n := len(g.backend.Requests()); n != 0 {
		t.Fatalf("%d requests reached the backend, want none", n)
	}

	clientTools := `[{"name":"f","input_schema":{}},{"type":"custom","name":"g","input_schema":{}},
		{"type":"bash_20250124","name":"bash"},{"type":"text_editor_20250728","name":"str_replace_based_edit_tool"},
		{"type":"computer_20250124","name":"computer","display_width_px":1,"display_height_px":1},
		{"type":"memory_20250818","name":"memory"},{"type":null,"name":"h","input_schema":{}}]`
	w := do(t, g.h, call{method: "POST", path: "/v1/messages", key: workloadKey,
		body: `{"model":"msg","max_tokens":8,"messages":[],"tools":` + clientTools + `,"mcp_servers":null}`})
	if w.Code != http.StatusOK {
		t.Fatalf("client tools: status %d: %s", w.Code, w.Body.String())
	}
}

// Messages passes through with the gateway's owned edits only: the deployment's model
// name and the output limit under max_tokens; the answer carries the public name and
// settles into a record of its usage, mapped onto kaiak's units.
func TestMessagesPassThrough(t *testing.T) {
	g := newTestGateway(t)
	withMessagesModels(t, g, nil)
	g.backend.SetReply(fakebackend.Reply{Usage: &fakebackend.Usage{PromptTokens: 30, CompletionTokens: 4,
		CachedTokens: 10, CacheWriteTokens: 5}})

	body := `{"model":"msg","max_tokens":500,"system":"be brief","messages":[{"role":"user","content":"hi"}],` +
		`"thinking":{"type":"enabled","budget_tokens":16},"service_tier":"auto","speed":"fast"}`
	w := do(t, g.h, call{method: "POST", path: "/v1/messages", key: workloadKey, body: body})
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	reqs := g.backend.Requests()
	if len(reqs) != 1 || reqs[0].Path != "/v1/messages" {
		t.Fatalf("backend requests: %+v", reqs)
	}
	sent := string(reqs[0].Body)
	for _, want := range []string{`"model":"msg-back"`, `"max_tokens":128`, `"system":"be brief"`,
		`"thinking":{"type":"enabled","budget_tokens":16}`, `"service_tier":"auto"`, `"speed":"fast"`} {
		if !strings.Contains(sent, want) {
			t.Errorf("sent body lacks %s: %s", want, sent)
		}
	}
	if strings.Contains(sent, "stream_options") {
		t.Errorf("a Messages request got an OpenAI usage edit: %s", sent)
	}
	if !strings.Contains(w.Body.String(), `"model":"msg"`) || strings.Contains(w.Body.String(), "msg-back") {
		t.Errorf("answer does not carry the public model name: %s", w.Body.String())
	}
	recs := g.usage.all()
	if len(recs) != 1 {
		t.Fatalf("%d records, want 1", len(recs))
	}
	u := recs[0].Units
	if u[config.UnitTokensIn] != 15 || u[config.UnitTokensCached] != 10 || u[config.UnitTokensCacheWrite] != 5 ||
		u[config.UnitTokensOut] != 4 || u[config.UnitTokensReasoning] != 0 || recs[0].Estimated || recs[0].Partial {
		t.Errorf("record %+v", recs[0])
	}

	// No max_tokens: the default is set, fitted to the context.
	do(t, g.h, call{method: "POST", path: "/v1/messages", key: workloadKey,
		body: `{"model":"msg","messages":[{"role":"user","content":"hi"}]}`})
	if sent := string(g.backend.Requests()[1].Body); !strings.Contains(sent, `"max_tokens":64`) {
		t.Errorf("no default output limit: %s", sent)
	}
}

// A Messages stream is relayed as the backend sends it, with the public model name in
// message_start; it is complete at message_stop, and its usage is read from
// message_start and message_delta.
func TestMessagesStream(t *testing.T) {
	g := newTestGateway(t)
	withMessagesModels(t, g, nil)
	g.backend.SetReply(fakebackend.Reply{Usage: &fakebackend.Usage{PromptTokens: 12, CompletionTokens: 4, CachedTokens: 2}})
	srv := httptest.NewServer(g.h)
	defer srv.Close()

	resp := postStream(t, srv.URL+"/v1/messages", `{"model":"msg","max_tokens":32,"stream":true,"messages":[]}`)
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	text := string(data)
	if resp.StatusCode != http.StatusOK || !strings.Contains(text, "event: message_stop") {
		t.Fatalf("status %d:\n%s", resp.StatusCode, text)
	}
	if !strings.Contains(text, `"message":{"content":[],"id":"msg_fake_1","model":"msg"`) || strings.Contains(text, "msg-back") {
		t.Errorf("message_start does not carry the public model name:\n%s", text)
	}
	rec := settledRecord(t, g)
	u := rec.Units
	if u[config.UnitTokensIn] != 10 || u[config.UnitTokensCached] != 2 || u[config.UnitTokensOut] != 4 || rec.Partial || rec.Estimated {
		t.Errorf("record %+v", rec)
	}
}

// A Messages stream broken off by an error event reaches the client up to and with
// the event — its type kept, its message the gateway's — then the connection is cut:
// the stream ended incomplete. An error event as the first event is answered as the
// status its type matches, in Anthropic's shape: overloaded_error a 503
// upstream_overloaded, api_error a 502 upstream_error.
func TestMessagesErrorEvents(t *testing.T) {
	g := newTestGateway(t)
	withMessagesModels(t, g, nil)
	srv := httptest.NewServer(g.h)
	defer srv.Close()

	g.backend.SetReply(fakebackend.Reply{ErrorEvent: true, ErrorEventAfter: 2})
	resp := postStream(t, srv.URL+"/v1/messages", `{"model":"msg","max_tokens":32,"stream":true,"messages":[]}`)
	data, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err == nil {
		t.Errorf("the stream ended cleanly; want the connection cut:\n%s", data)
	}
	if !strings.Contains(string(data), "overloaded_error") || strings.Contains(string(data), "message_stop") ||
		strings.Contains(string(data), `"Overloaded"`) || !strings.Contains(string(data), "The model backend ended the response with an error.") {
		t.Errorf("relayed:\n%s", data)
	}
	if rec := settledRecord(t, g); !rec.Partial {
		t.Errorf("record not partial: %+v", rec)
	}
	waitLog(t, g, `"kaiak.relay_end":"upstream_incomplete"`)

	g.backend.SetReply(fakebackend.Reply{ErrorEvent: true})
	w := do(t, g.h, call{method: "POST", path: "/v1/messages", key: workloadKey,
		body: `{"model":"msg","max_tokens":32,"stream":true,"messages":[]}`})
	expectAnthropicError(t, w, http.StatusServiceUnavailable, "overloaded_error", "upstream_overloaded")

	g.backend.SetReply(fakebackend.Reply{ErrorEvent: true, ErrorEventCode: "api_error"})
	w = do(t, g.h, call{method: "POST", path: "/v1/messages", key: workloadKey,
		body: `{"model":"msg","max_tokens":32,"stream":true,"messages":[]}`})
	expectAnthropicError(t, w, http.StatusBadGateway, "api_error", "upstream_error")
}

// The token-counting endpoint passes through to the backend's and leaves no record.
func TestMessagesCountTokens(t *testing.T) {
	g := newTestGateway(t)
	withMessagesModels(t, g, nil)
	g.backend.SetReply(fakebackend.Reply{Usage: &fakebackend.Usage{PromptTokens: 21}})
	w := do(t, g.h, call{method: "POST", path: "/v1/messages/count_tokens", key: workloadKey,
		body: `{"model":"msg","messages":[{"role":"user","content":"hi"}],"max_tokens":9000}`})
	if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != `{"input_tokens":21}` {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	reqs := g.backend.Requests()
	if len(reqs) != 1 || reqs[0].Path != "/v1/messages/count_tokens" || !strings.Contains(string(reqs[0].Body), `"max_tokens":9000`) {
		t.Fatalf("backend requests: %+v", reqs)
	}
	if n := len(g.usage.all()); n != 0 {
		t.Errorf("%d usage records, want none", n)
	}
	if !strings.Contains(g.logText(), `"url.path":"/v1/messages/count_tokens"`) || strings.Contains(g.logText(), `"gen_ai.operation.name"`) {
		t.Errorf("log line:\n%s", g.logText())
	}
}

// With anthropic-version, the model list and entry answer in Anthropic's shape, over
// the models some deployment serves through Messages; without it, as before.
func TestAnthropicShapedModelList(t *testing.T) {
	g := newTestGateway(t)
	withMessagesModels(t, g, nil)
	anthropic := map[string]string{"Anthropic-Version": "2023-06-01"}

	w := do(t, g.h, call{method: "GET", path: "/v1/models", key: workloadKey, header: anthropic})
	var list struct {
		Data []struct {
			Type, ID, DisplayName, CreatedAt string
			Endpoints                        []string
		}
		HasMore bool    `json:"has_more"`
		FirstID *string `json:"first_id"`
		LastID  *string `json:"last_id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || w.Code != http.StatusOK {
		t.Fatalf("status %d (%v): %s", w.Code, err, w.Body.String())
	}
	if len(list.Data) != 1 || list.Data[0].ID != "msg" || list.Data[0].Type != "model" || list.HasMore ||
		list.FirstID == nil || *list.FirstID != "msg" || list.LastID == nil || *list.LastID != "msg" {
		t.Errorf("list %s", w.Body.String())
	}
	for _, want := range []string{`"display_name":"msg"`, `"created_at":"1970-01-01T00:00:00Z"`, `"context_length":8192`,
		`"endpoints":["chat_completions","completions","embeddings","messages","messages_count_tokens","responses"]`} {
		if !strings.Contains(w.Body.String(), want) {
			t.Errorf("list lacks %s: %s", want, w.Body.String())
		}
	}

	w = do(t, g.h, call{method: "GET", path: "/v1/models/msg", key: workloadKey, header: anthropic})
	if w.Code != http.StatusOK || !strings.HasPrefix(w.Body.String(), `{"type":"model","id":"msg"`) {
		t.Errorf("entry: %d %s", w.Code, w.Body.String())
	}
	// A model without Messages is as unknown here as a missing one.
	expectAnthropicError(t, do(t, g.h, call{method: "GET", path: "/v1/models/open", key: workloadKey, header: anthropic}),
		http.StatusNotFound, "not_found_error", "model_not_found")
	expectAnthropicError(t, do(t, g.h, call{method: "GET", path: "/v1/models/nope", key: workloadKey, header: anthropic}),
		http.StatusNotFound, "not_found_error", "model_not_found")
	expectAnthropicError(t, do(t, g.h, call{method: "GET", path: "/v1/models", header: anthropic}),
		http.StatusUnauthorized, "authentication_error", "missing_api_key")

	// A key allowed no Messages model gets an empty list.
	w = do(t, g.h, call{method: "GET", path: "/v1/models", key: userKey, header: anthropic})
	if strings.TrimSpace(w.Body.String()) != `{"data":[],"has_more":false,"first_id":null,"last_id":null}` {
		t.Errorf("empty list: %s", w.Body.String())
	}
	// Without the header: the OpenAI list.
	w = do(t, g.h, call{method: "GET", path: "/v1/models", key: workloadKey})
	if !strings.HasPrefix(w.Body.String(), `{"object":"list"`) {
		t.Errorf("OpenAI list: %s", w.Body.String())
	}
}

// postStream posts body with the workload key to url and returns the response.
func postStream(t *testing.T, url, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("X-Api-Key", workloadKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// waitLog waits for the gateway's log to contain want.
func waitLog(t *testing.T, g *testGateway, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(g.logText(), want) {
		if time.Now().After(deadline) {
			t.Fatalf("log lacks %s:\n%s", want, g.logText())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A token-counting request generates and costs nothing: only requests-per-minute
// limits apply to it, so a spent token window refuses the next Messages request but
// not a count (the pre-merge review's L1).
func TestCountTokensMeetsOnlyRequestLimits(t *testing.T) {
	g := newTestGateway(t)
	withMessagesModels(t, g, func(doc string) string {
		return strings.Replace(doc, `"allowed_models": ["*"] }`,
			`"allowed_models": ["*"], "limits": [{ "type": "tokens_per_minute", "value": 100 }] }`, 1)
	})
	g.backend.SetReply(fakebackend.Reply{Usage: &fakebackend.Usage{PromptTokens: 100, CompletionTokens: 20}})
	body := `{"model":"msg","max_tokens":32,"messages":[{"role":"user","content":"hi"}]}`
	if w := do(t, g.h, call{method: "POST", path: "/v1/messages", key: workloadKey, body: body}); w.Code != http.StatusOK {
		t.Fatalf("first request: %d %s", w.Code, w.Body.String())
	}
	w := do(t, g.h, call{method: "POST", path: "/v1/messages/count_tokens", key: workloadKey,
		body: `{"model":"msg","messages":[{"role":"user","content":"hi"}]}`})
	if w.Code != http.StatusOK {
		t.Errorf("count_tokens under a spent token window: %d %s", w.Code, w.Body.String())
	}
	w = do(t, g.h, call{method: "POST", path: "/v1/messages", key: workloadKey, body: body})
	if w.Code != http.StatusTooManyRequests {
		t.Errorf("messages under a spent token window: %d, want 429", w.Code)
	}
}
