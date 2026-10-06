package server

import (
	"encoding/json"
	"fmt"
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
	if apiErr := refuseStoredFiles(top["messages"]); apiErr != nil {
		return apiErr
	}
	if rq.endpoint == endpointMessages {
		budget, apiErr := thinkingBudget(top["thinking"])
		if apiErr != nil {
			return apiErr
		}
		rq.inbound.ThinkingBudget = budget
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
	return refuseHostedTools(top["tools"], "tools", judgeMessagesTool)
}

// judgeMessagesTool admits a Messages tool the client runs: one with no type (a
// custom tool), or a type messagesClientTool admits.
func judgeMessagesTool(at string, tool map[string]json.RawMessage) *apiError {
	typ, apiErr := optionalField[string](tool, "type", at+".type", "a string")
	if apiErr != nil || typ == nil {
		return apiErr
	}
	if !messagesClientTool(*typ) {
		return errHostedTool(at+".type", *typ)
	}
	return nil
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

// thinkingBudget reads thinking.budget_tokens when thinking is enabled (Anthropic's
// {"type": "enabled", "budget_tokens": n}): the gateway does not edit it, but an
// output limit it sets must stay above it (applyModelParams). Exact keys; nil when
// thinking is absent, disabled or adaptive.
func thinkingBudget(raw json.RawMessage) (*int64, *apiError) {
	if !startsWith(raw, '{') {
		return nil, nil
	}
	thinking, repeats, ok := decodeMembersRepeats(raw)
	if !ok {
		return nil, errInvalidType("thinking", "an object")
	}
	if len(repeats) > 0 {
		return nil, errDuplicateMember("thinking." + repeats[0])
	}
	typ, apiErr := optionalField[string](thinking, "type", "thinking.type", "a string")
	if apiErr != nil || typ == nil || *typ != "enabled" {
		return nil, apiErr
	}
	return optionalField[int64](thinking, "budget_tokens", "thinking.budget_tokens", "an integer")
}

// maxBlockNesting bounds how deep refuseStoredFiles follows block lists: a message's
// content, a tool result's content inside it, a content source's content inside
// that. Anthropic nests no deeper; a body nesting further is the backend's to refuse,
// and the bound keeps the walk linear.
const maxBlockNesting = 4

// refuseStoredFiles refuses a Messages request referring to a file stored at the
// backend (docs/specs/GATEWAY.md, Client API → stored objects): a block whose source
// is of type file, or a block naming a file_id (a container upload), in any message's
// content, a tool result's content or a content source's content. A block or source
// naming a member twice is refused. A list of another shape is the backend's to judge.
func refuseStoredFiles(messagesRaw json.RawMessage) *apiError {
	var messages []json.RawMessage
	if !startsWith(messagesRaw, '[') || json.Unmarshal(messagesRaw, &messages) != nil {
		return nil
	}
	for i, message := range messages {
		members, _, ok := decodeMembersRepeats(message)
		if !ok {
			continue
		}
		if apiErr := refuseStoredFileBlocks(members["content"], fmt.Sprintf("messages[%d].content", i), 1); apiErr != nil {
			return apiErr
		}
	}
	return nil
}

func refuseStoredFileBlocks(raw json.RawMessage, param string, nesting int) *apiError {
	var blocks []json.RawMessage
	if nesting > maxBlockNesting || !startsWith(raw, '[') || json.Unmarshal(raw, &blocks) != nil {
		return nil
	}
	for j, block := range blocks {
		at := fmt.Sprintf("%s[%d]", param, j)
		members, repeats, ok := decodeMembersRepeats(block)
		if !ok {
			continue
		}
		if len(repeats) > 0 {
			return errDuplicateMember(at + "." + repeats[0])
		}
		if id, ok := members["file_id"]; ok && string(id) != "null" {
			return errStoredObject(at + ".file_id")
		}
		if source, repeats, ok := decodeMembersRepeats(members["source"]); ok {
			if len(repeats) > 0 {
				return errDuplicateMember(at + ".source." + repeats[0])
			}
			var typ string
			if json.Unmarshal(source["type"], &typ) == nil && typ == "file" {
				return errStoredObject(at + ".source")
			}
			if apiErr := refuseStoredFileBlocks(source["content"], at+".source.content", nesting+1); apiErr != nil {
				return apiErr
			}
		}
		if apiErr := refuseStoredFileBlocks(members["content"], at+".content", nesting+1); apiErr != nil {
			return apiErr
		}
	}
	return nil
}
