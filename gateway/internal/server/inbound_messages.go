package server

import (
	"encoding/json"
	"fmt"
	"slices"
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
// (bash, the text editor, computer use, memory), whatever their version suffix.
var messagesClientToolPrefixes = []string{"bash_", "text_editor_", "computer_", "memory_"}

// refuseMessagesHostedTools refuses a request handing the backend a tool to run itself
// (docs/specs/GATEWAY.md, Client API → hosted tools are refused): mcp_servers or
// container, or a tools entry whose type is not on the allowlist of tools the client
// runs — no type, "custom", or one of Anthropic's client-run tools. An allowlist, so a
// server tool released later is refused until it is judged. A tool naming its type
// twice is refused: the gateway and the backend could read different ones.
func refuseMessagesHostedTools(top map[string]json.RawMessage) *apiError {
	for _, key := range messagesHostedMembers {
		if raw, ok := top[key]; ok && string(raw) != "null" {
			return errHostedMember(key)
		}
	}
	raw, ok := top["tools"]
	if !ok || string(raw) == "null" {
		return nil
	}
	var tools []json.RawMessage
	if json.Unmarshal(raw, &tools) != nil {
		return errInvalidType("tools", "an array")
	}
	for i, tool := range tools {
		param := fmt.Sprintf("tools[%d]", i)
		members, repeats, ok := decodeMembersRepeats(tool)
		if !ok {
			return errInvalidType(param, "an object")
		}
		if slices.Contains(repeats, "type") {
			return errDuplicateMember(param + ".type")
		}
		typ, apiErr := optionalField[string](members, "type", param+".type", "a string")
		if apiErr != nil {
			return apiErr
		}
		if typ != nil && !messagesClientTool(*typ) {
			return errHostedTool(param+".type", *typ)
		}
	}
	return nil
}

// messagesClientTool reports whether a Messages tool of type t is one the client runs.
func messagesClientTool(t string) bool {
	if t == "custom" {
		return true
	}
	return slices.ContainsFunc(messagesClientToolPrefixes, func(prefix string) bool { return strings.HasPrefix(t, prefix) })
}
