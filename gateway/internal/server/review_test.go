// Regression tests from the independent pre-merge review of 2026-10-06
// (docs/reviews/2026-10-06/AUDIT-independent.md, [B]).

package server

import (
	"net/http"
	"strings"
	"testing"
)

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
