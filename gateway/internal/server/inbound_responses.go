package server

import (
	"encoding/json"
	"slices"
)

// parseResponsesFields reads a Responses request's own owned fields
// (docs/specs/GATEWAY.md, Client API → owned fields), past those every format shares
// (parseOwnedFields): the stateful fields, refused on both endpoints (Responses is
// stateless); the tool fields, refusing a tool the backend would run; and what the
// input items bring in. store is the provider's to set (false, on every request) and
// is not read. Everything else — the rest of input, instructions, reasoning, include —
// is the backend's and passes untouched.
func parseResponsesFields(rq *request, top map[string]json.RawMessage) *apiError {
	if apiErr := refuseStatefulResponses(top); apiErr != nil {
		return apiErr
	}
	if apiErr := refuseResponsesHostedTools(top); apiErr != nil {
		return apiErr
	}
	return refuseResponsesInputItems(top["input"])
}

// statefulResponsesFields name state kept at the backend: a stored response to
// continue, a stored conversation, a stored prompt template (which can carry tools of
// its own).
var statefulResponsesFields = []string{"previous_response_id", "conversation", "prompt"}

// refuseStatefulResponses refuses a request that relies on state kept between
// requests (docs/specs/GATEWAY.md, Client API → Responses is stateless): continuing a
// stored response or conversation, a stored prompt, or running in the background to
// be fetched later. A null value is absent, and background: false asks for nothing
// kept.
func refuseStatefulResponses(top map[string]json.RawMessage) *apiError {
	for _, key := range statefulResponsesFields {
		if raw, ok := top[key]; ok && string(raw) != "null" {
			return errStatefulResponses(key)
		}
	}
	background, apiErr := optionalField[bool](top, "background", "background", "a boolean")
	if apiErr != nil {
		return apiErr
	}
	if background != nil && *background {
		return errStatefulResponses("background")
	}
	return nil
}

// responsesClientTools are the Responses tool types the client runs: its functions and
// custom tools, and the shell (on the client's machine: judgeResponsesTool) and patch
// tools whose calls come back to the client.
var responsesClientTools = []string{"function", "custom", "local_shell", "shell", "apply_patch"}

func responsesClientTool(t string) bool { return slices.Contains(responsesClientTools, t) }

// judgeResponsesTool admits a Responses tool the client runs: its type — which every
// Responses tool names — on the allowlist, and a shell only when it runs on the
// client: no environment, or a local one. A shell with a container environment
// (container_auto, container_reference) runs in the backend's container.
func judgeResponsesTool(at string, tool map[string]json.RawMessage) *apiError {
	typ, apiErr := optionalField[string](tool, "type", at+".type", "a string")
	if apiErr != nil {
		return apiErr
	}
	if typ == nil {
		return errInvalidType(at+".type", "a string")
	}
	if !responsesClientTool(*typ) {
		return errHostedTool(at+".type", *typ)
	}
	if *typ != "shell" {
		return nil
	}
	raw := tool["environment"]
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	environment, ok, apiErr := objectMembers(raw, at+".environment")
	if !ok {
		return errInvalidType(at+".environment", "an object")
	}
	if apiErr != nil {
		return apiErr
	}
	kind, apiErr := optionalField[string](environment, "type", at+".environment.type", "a string")
	if apiErr != nil {
		return apiErr
	}
	if kind == nil || *kind != "local" {
		name := ""
		if kind != nil {
			name = *kind
		}
		return errHostedTool(at+".environment.type", name)
	}
	return nil
}

// refuseResponsesHostedTools refuses a request handing the backend a tool to run
// itself (docs/specs/GATEWAY.md, Client API → hosted tools are refused): a tools entry
// judgeResponsesTool refuses, or a tool_choice naming such a type. A tool_choice of
// type allowed_tools names no tool of its own — it narrows the tools list — and its
// own list is held to the same rule. A tool_choice naming a member twice is refused.
func refuseResponsesHostedTools(top map[string]json.RawMessage) *apiError {
	if apiErr := refuseHostedTools(top["tools"], "tools", judgeResponsesTool); apiErr != nil {
		return apiErr
	}
	raw := top["tool_choice"]
	// A string tool_choice (none, auto, required) names no tool.
	if len(raw) == 0 || string(raw) == "null" || raw[0] == '"' {
		return nil
	}
	members, ok, apiErr := objectMembers(raw, "tool_choice")
	if !ok {
		return errInvalidType("tool_choice", "a string or an object")
	}
	if apiErr != nil {
		return apiErr
	}
	typ, apiErr := optionalField[string](members, "type", "tool_choice.type", "a string")
	if apiErr != nil || typ == nil {
		return apiErr
	}
	if *typ == "allowed_tools" {
		return refuseHostedTools(members["tools"], "tool_choice.tools", judgeResponsesTool)
	}
	if !responsesClientTool(*typ) {
		return errHostedTool("tool_choice.type", *typ)
	}
	return nil
}

// refuseResponsesInputItems refuses input items that bring in what the request's own
// members would be refused for (docs/specs/GATEWAY.md, Client API → Responses is
// stateless; hosted tools are refused): tools added by an additional_tools or a
// tool_search_output item, judged as the tools list is; a reference to an item stored
// at the backend (an item_reference, or an item with neither type nor role — the
// reference's short form); a content or output part naming a file stored at the
// backend (file_id). An item naming a member twice is refused. A string input holds
// none; an input of another shape is the backend's to judge.
func refuseResponsesInputItems(raw json.RawMessage) *apiError {
	return eachObject(raw, "input", func(at string, item map[string]json.RawMessage) *apiError {
		typ, apiErr := optionalField[string](item, "type", at+".type", "a string")
		if apiErr != nil {
			return apiErr
		}
		switch {
		case typ != nil && (*typ == "additional_tools" || *typ == "tool_search_output"):
			if apiErr := refuseHostedTools(item["tools"], at+".tools", judgeResponsesTool); apiErr != nil {
				return apiErr
			}
		case typ != nil && *typ == "item_reference":
			return errStoredObjectResponses(at)
		case typ == nil && item["role"] == nil:
			return errStoredObjectResponses(at)
		}
		for _, list := range []string{"content", "output"} {
			if apiErr := refuseStoredFileParts(item[list], at+"."+list); apiErr != nil {
				return apiErr
			}
		}
		return nil
	})
}

// refuseStoredFileParts refuses a Responses part list (raw, at param) holding a part
// that names a file stored at the backend (a non-null file_id). A part naming a member
// twice is refused; a list of another shape is the backend's to judge.
func refuseStoredFileParts(raw json.RawMessage, param string) *apiError {
	return eachObject(raw, param, func(at string, part map[string]json.RawMessage) *apiError {
		if id, ok := part["file_id"]; ok && string(id) != "null" {
			return errStoredObjectResponses(at + ".file_id")
		}
		return nil
	})
}
