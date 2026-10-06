package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// What a request can carry that the gateway refuses before routing, past the
// top-level members: references to objects stored at the backend, tools that run
// there brought in by any path, members named twice where the gateway enforces a
// rule (docs/specs/GATEWAY.md, Client API → Responses is stateless, Hosted tools are
// refused; the pre-merge review's H3, M5, L3, [B] H3 and [B] L1). Nothing reaches the
// backend.
func TestInboundRefusalsPastTheTopLevel(t *testing.T) {
	for _, c := range []struct {
		name, path, model, body, code, param string
	}{
		{"responses stored prompt", "/v1/responses", "resp",
			`{"model":"resp","input":"hi","prompt":{"id":"pmpt_1"}}`, "stateful_responses_unsupported", "prompt"},
		{"responses stored file in content", "/v1/responses", "resp",
			`{"model":"resp","input":[{"role":"user","content":[{"type":"input_file","file_id":"file_1"}]}]}`,
			"stateful_responses_unsupported", "input[0].content[0].file_id"},
		{"responses stored file in a tool output", "/v1/responses/input_tokens", "resp",
			`{"model":"resp","input":[{"type":"function_call_output","call_id":"c","output":[{"type":"input_image","file_id":"file_1"}]}]}`,
			"stateful_responses_unsupported", "input[0].output[0].file_id"},
		{"responses tool without a type", "/v1/responses", "resp",
			`{"model":"resp","input":"hi","tools":[{"name":"f"}]}`, "invalid_type", "tools[0].type"},
		{"responses tool_search_output hosted tool", "/v1/responses", "resp",
			`{"model":"resp","input":[{"type":"tool_search_output","tools":[{"type":"file_search"}]}]}`,
			"hosted_tool_unsupported", "input[0].tools[0].type"},
		{"responses shell environment named twice", "/v1/responses", "resp",
			`{"model":"resp","input":"hi","tools":[{"type":"shell","environment":{"type":"local","type":"container_auto"}}]}`,
			"duplicate_member", "tools[0].environment.type"},
		{"responses input item member named twice", "/v1/responses", "resp",
			`{"model":"resp","input":[{"type":"additional_tools","tools":[{"type":"web_search"}],"tools":[]}]}`,
			"duplicate_member", "input[0].tools"},
		{"messages stored file source", "/v1/messages", "msg",
			`{"model":"msg","max_tokens":8,"messages":[{"role":"user","content":[{"type":"document","source":{"type":"file","file_id":"file_1"}}]}]}`,
			"stored_object_unsupported", "messages[0].content[0].source"},
		{"messages stored file in a tool result", "/v1/messages/count_tokens", "msg",
			`{"model":"msg","messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":[{"type":"image","source":{"type":"file","file_id":"file_1"}}]}]}]}`,
			"stored_object_unsupported", "messages[0].content[0].content[0].source"},
		{"messages container upload", "/v1/messages", "msg",
			`{"model":"msg","max_tokens":8,"messages":[{"role":"user","content":[{"type":"container_upload","file_id":"file_1"}]}]}`,
			"stored_object_unsupported", "messages[0].content[0].file_id"},
		{"messages block member named twice", "/v1/messages", "msg",
			`{"model":"msg","max_tokens":8,"messages":[{"role":"user","content":[{"type":"text","text":"a","text":"b"}]}]}`,
			"duplicate_member", "messages[0].content[0].text"},
		{"messages tool member named twice", "/v1/messages", "msg",
			`{"model":"msg","max_tokens":8,"messages":[],"tools":[{"name":"f","input_schema":{},"name":"g"}]}`,
			"duplicate_member", "tools[0].name"},
	} {
		t.Run(c.name, func(t *testing.T) {
			g := newTestGateway(t)
			withMessagesModels(t, g, nil)
			withResponsesModels(t, g)
			w := do(t, g.h, call{method: "POST", path: c.path, key: workloadKey, body: c.body})
			// Anthropic's error shape carries no param: its message names it.
			code, param := errorFields(t, w)
			named := param == c.param || (param == "" && strings.Contains(w.Body.String(), "'"+c.param+"'"))
			if w.Code != http.StatusBadRequest || code != c.code || !named {
				t.Errorf("%d %s, want 400 %s naming %s", w.Code, w.Body.String(), c.code, c.param)
			}
			if n := len(g.backend.Requests()); n != 0 {
				t.Errorf("%d requests reached the backend, want none", n)
			}
		})
	}
}

// A shell the client runs passes — no environment, or a local one — and so do inline
// input items carrying their own IDs.
func TestInboundAdmitsClientShellsAndInlineItems(t *testing.T) {
	for _, body := range []string{
		`{"model":"resp","input":"hi","tools":[{"type":"shell"}]}`,
		`{"model":"resp","input":"hi","tools":[{"type":"shell","environment":{"type":"local"}}]}`,
		`{"model":"resp","input":[{"id":"msg_1","role":"user","content":"hi"},{"type":"function_call","id":"fc_1","call_id":"c","name":"f","arguments":"{}"}]}`,
	} {
		g := newTestGateway(t)
		withResponsesModels(t, g)
		if w := do(t, g.h, call{method: "POST", path: "/v1/responses", key: workloadKey, body: body}); w.Code != http.StatusOK {
			t.Errorf("%s: %d %s", body, w.Code, w.Body.String())
		}
	}
}

// A Messages request thinking with a budget at or above the output limit the gateway
// would set — the model's ceiling, or its default — is refused naming max_tokens and
// that limit; one the client set itself goes to the backend (the pre-merge review's
// M6). The test model's ceiling is 128, its default 64, its context 8192.
func TestThinkingBudgetAboveTheModelsOutputLimit(t *testing.T) {
	g := newTestGateway(t)
	withMessagesModels(t, g, nil)
	for _, c := range []struct {
		body   string
		status int
	}{
		{`{"model":"msg","max_tokens":4000,"thinking":{"type":"enabled","budget_tokens":3999},"messages":[]}`, http.StatusBadRequest},
		{`{"model":"msg","thinking":{"type":"enabled","budget_tokens":100},"messages":[]}`, http.StatusBadRequest},
		{`{"model":"msg","max_tokens":4000,"thinking":{"type":"enabled","budget_tokens":100},"messages":[]}`, http.StatusOK},
		{`{"model":"msg","max_tokens":100,"thinking":{"type":"enabled","budget_tokens":100},"messages":[]}`, http.StatusOK},
		{`{"model":"msg","max_tokens":4000,"thinking":{"type":"adaptive"},"messages":[]}`, http.StatusOK},
	} {
		w := do(t, g.h, call{method: "POST", path: "/v1/messages", key: workloadKey, body: c.body})
		if w.Code != c.status {
			t.Errorf("%s: %d %s, want %d", c.body, w.Code, w.Body.String(), c.status)
			continue
		}
		if c.status == http.StatusBadRequest && (errorCodeOf(t, w) != "invalid_value" || !strings.Contains(w.Body.String(), "max_tokens")) {
			t.Errorf("%s: %s", c.body, w.Body.String())
		}
	}
}

// errorFields are an error answer's code and param, in either shape (both nest them
// under "error"; Anthropic's carries no param).
func errorFields(t *testing.T, w *httptest.ResponseRecorder) (code, param string) {
	t.Helper()
	var body struct {
		Error struct {
			Code  string  `json:"code"`
			Param *string `json:"param"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("error body %s: %v", w.Body.String(), err)
	}
	if body.Error.Param != nil {
		param = *body.Error.Param
	}
	return body.Error.Code, param
}

func errorCodeOf(t *testing.T, w *httptest.ResponseRecorder) string {
	code, _ := errorFields(t, w)
	return code
}
