package server

import (
	"encoding/json"
	"slices"

	"kaiak/internal/accounting"
)

// parseResponsesFields reads a Responses request's owned fields (docs/specs/GATEWAY.md,
// Client API → owned fields): model; on /v1/responses also stream and
// max_output_tokens (the output limit); the stateful fields, refused on both endpoints
// (Responses is stateless); and the tool fields, refusing a tool the backend would run.
// store is the provider's to set (false, on every request) and is not read. Everything
// else — input, instructions, reasoning, include — is the backend's and passes
// untouched.
func parseResponsesFields(rq *request, top map[string]json.RawMessage) *apiError {
	if rq.endpoint == endpointResponses {
		if apiErr := readModelAndStream(rq, top); apiErr != nil {
			return apiErr
		}
		var apiErr *apiError
		if rq.inbound.MaxOutputTokens, apiErr = optionalField[int64](top, "max_output_tokens", "max_output_tokens", "an integer"); apiErr != nil {
			return apiErr
		}
	} else if apiErr := readModel(rq, top); apiErr != nil {
		return apiErr
	}
	rq.inbound.Sequences = 1
	if apiErr := refuseStatefulResponses(top); apiErr != nil {
		return apiErr
	}
	if apiErr := refuseResponsesHostedTools(top); apiErr != nil {
		return apiErr
	}
	rq.input = accounting.EstimateInput(providerEndpoint(rq.endpoint), rq.body)
	return nil
}

// statefulResponsesFields name a conversation kept on the backend: a stored response
// to continue, or a stored conversation.
var statefulResponsesFields = []string{"previous_response_id", "conversation"}

// refuseStatefulResponses refuses a request that relies on state kept between
// requests (docs/specs/GATEWAY.md, Client API → Responses is stateless): continuing a
// stored response or conversation, or running in the background to be fetched later.
// A null value is absent, and background: false asks for nothing kept.
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
// custom tools, and the shell and patch tools whose calls come back to the client.
var responsesClientTools = []string{"function", "custom", "local_shell", "shell", "apply_patch"}

func responsesClientTool(t string) bool { return slices.Contains(responsesClientTools, t) }

// refuseResponsesHostedTools refuses a request handing the backend a tool to run
// itself (docs/specs/GATEWAY.md, Client API → hosted tools are refused): a tools entry
// whose type is not one the client runs, or a tool_choice naming such a type. A
// tool_choice of type allowed_tools names no tool of its own — it narrows the tools
// list — and its own list is held to the same allowlist.
func refuseResponsesHostedTools(top map[string]json.RawMessage) *apiError {
	if apiErr := refuseHostedToolTypes(top["tools"], "tools", responsesClientTool); apiErr != nil {
		return apiErr
	}
	raw := top["tool_choice"]
	// A string tool_choice (none, auto, required) names no tool.
	if len(raw) == 0 || string(raw) == "null" || raw[0] == '"' {
		return nil
	}
	members, repeats, ok := decodeMembersRepeats(raw)
	if !ok {
		return errInvalidType("tool_choice", "a string or an object")
	}
	if slices.Contains(repeats, "type") {
		return errDuplicateMember("tool_choice.type")
	}
	typ, apiErr := optionalField[string](members, "type", "tool_choice.type", "a string")
	if apiErr != nil || typ == nil {
		return apiErr
	}
	if *typ == "allowed_tools" {
		return refuseHostedToolTypes(members["tools"], "tool_choice.tools", responsesClientTool)
	}
	if !responsesClientTool(*typ) {
		return errHostedTool("tool_choice.type", *typ)
	}
	return nil
}
