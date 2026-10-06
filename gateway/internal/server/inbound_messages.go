package server

import (
	"encoding/json"
	"strings"

	"kaiak/internal/accounting"
)

// parseMessagesFields reads a Messages request's owned fields (docs/specs/GATEWAY.md,
// Client API → owned fields): model; on /v1/messages also stream and max_tokens (the
// output limit); and the tool fields, refusing a tool the backend would run on both
// endpoints. Everything else — system, messages, thinking, tool_choice — is the
// backend's and passes untouched.
func parseMessagesFields(rq *request, top map[string]json.RawMessage) *apiError {
	if rq.endpoint == endpointMessages {
		if apiErr := readModelAndStream(rq, top); apiErr != nil {
			return apiErr
		}
		var apiErr *apiError
		if rq.inbound.MaxTokens, apiErr = optionalField[int64](top, "max_tokens", "max_tokens", "an integer"); apiErr != nil {
			return apiErr
		}
	} else if apiErr := readModel(rq, top); apiErr != nil {
		return apiErr
	}
	rq.inbound.Sequences = 1
	if apiErr := refuseMessagesHostedTools(top); apiErr != nil {
		return apiErr
	}
	rq.input = accounting.EstimateInput(providerEndpoint(rq.endpoint), rq.body)
	return nil
}

// messagesHostedMembers are the top-level Messages members that hand the backend tools
// to run itself: remote MCP servers, and a code-execution container.
var messagesHostedMembers = []string{"mcp_servers", "container"}

// messagesClientToolPrefixes start the types of Anthropic's tools the client runs
// (bash, the text editor, computer use, memory); a dated version follows each
// (computer_20250124).
var messagesClientToolPrefixes = []string{"bash_", "text_editor_", "computer_", "memory_"}

// refuseMessagesHostedTools refuses a request handing the backend a tool to run itself
// (docs/specs/GATEWAY.md, Client API → hosted tools are refused): mcp_servers or
// container, or a tools entry whose type is not on the allowlist of tools the client
// runs — no type, "custom", or one of Anthropic's client-run tools.
func refuseMessagesHostedTools(top map[string]json.RawMessage) *apiError {
	for _, key := range messagesHostedMembers {
		if raw, ok := top[key]; ok && string(raw) != "null" {
			return errHostedMember(key)
		}
	}
	return refuseHostedToolTypes(top["tools"], "tools", messagesClientTool)
}

// messagesClientTool reports whether a Messages tool of type t is one the client runs:
// "custom", or a client-run tool's prefix followed directly by its version date. A
// name that merely shares a prefix (computer_toolset_20260801) is another tool, to be
// judged before it passes.
func messagesClientTool(t string) bool {
	if t == "custom" {
		return true
	}
	for _, prefix := range messagesClientToolPrefixes {
		if version, ok := strings.CutPrefix(t, prefix); ok && isDigits(version) {
			return true
		}
	}
	return false
}

// isDigits reports whether s is one or more ASCII digits.
func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
