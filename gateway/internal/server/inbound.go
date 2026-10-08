package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"

	"kaiak/internal/accounting"
	"kaiak/internal/config"
	"kaiak/internal/provider"
)

// inboundFields are the request parameters the gateway owns (docs/specs/GATEWAY.md,
// Request pipeline). Everything else stays in the raw body, untouched.
type inboundFields struct {
	Stream bool
	// MaxTokens, MaxCompletionTokens and MaxOutputTokens (Responses) are nil when the
	// request omits them.
	MaxTokens           *int64
	MaxCompletionTokens *int64
	MaxOutputTokens     *int64
	// ThinkingBudget is a Messages request's thinking.budget_tokens when thinking is
	// enabled; nil otherwise.
	ThinkingBudget *int64
	// IncludeUsage is the client's stream_options.include_usage.
	IncludeUsage bool
	// Sequences is how many output sequences the request asks the backend to
	// generate, each up to the output limit: n (or best_of, when larger) per
	// prompt, times a completion's prompts. At least 1.
	Sequences int64
}

// readInbound parses a body endpoint's request: the raw body, capped by the snapshot's
// max_request_body_bytes and by the whole body budget (a larger body could never be
// held), and the owned fields. The body holds its share of the budget until it is
// dropped (releaseBody, also a finisher).
func readInbound(_ context.Context, rq *request, bodies *BodyBudget) *apiError {
	limit := min(rq.snapshot.MaxRequestBodyBytes, bodies.Limit())
	if rq.r.ContentLength > limit {
		return errBodyTooLarge(limit)
	}
	rq.finishers = append(rq.finishers, rq.releaseBody)
	body, apiErr := rq.readBody(bodies, limit)
	if apiErr != nil {
		return apiErr
	}
	// The body arrived within the body-read deadline (Listen); the rest of the
	// request — queueing, a long stream — is not bounded by it.
	ClearBodyDeadline(rq.w)
	rq.body = body
	return parseOwnedFields(rq)
}

// parseOwnedFields reads the owned fields of the request's format by exact key
// (docs/specs/GATEWAY.md, Client API → owned fields). encoding/json matches struct
// fields case-insensitively, which would read "Model" as model while the backend sees
// an unknown field; the top-level object is therefore decoded as a map. A null value
// counts as absent, as it does for OpenAI. A top-level member named twice is refused
// (docs/specs/GATEWAY.md, Request pipeline): the gateway and the backend could read
// different occurrences, and the provider edits each owned member once.
func parseOwnedFields(rq *request) *apiError {
	top, repeated, ok := decodeMembers(rq.body)
	if !ok {
		return errInvalidJSON()
	}
	if repeated != "" {
		return errDuplicateMember(repeated)
	}
	switch rq.endpoint.api.Format() {
	case provider.FormatOpenAI:
		return parseOpenAIFields(rq, top)
	case provider.FormatMessages:
		return parseMessagesFields(rq, top)
	case provider.FormatResponses:
		return parseResponsesFields(rq, top)
	}
	// The endpoint table routes only the formats the inbound stage reads.
	panic("server: no inbound parser for the endpoint's format")
}

// parseOpenAIFields reads an OpenAI-format request's owned fields: model, stream, the
// output-limit keys, the sequence count, an embeddings request's inputs and
// stream_options.include_usage.
func parseOpenAIFields(rq *request, top map[string]json.RawMessage) *apiError {
	apiErr := readModelAndStream(rq, top)
	if apiErr != nil {
		return apiErr
	}
	if rq.inbound.MaxTokens, apiErr = optionalField[int64](top, "max_tokens", "max_tokens", "an integer"); apiErr != nil {
		return apiErr
	}
	if rq.inbound.MaxCompletionTokens, apiErr = optionalField[int64](top, "max_completion_tokens", "max_completion_tokens", "an integer"); apiErr != nil {
		return apiErr
	}

	if rq.inbound.Sequences, apiErr = sequences(rq.endpoint.api, top, rq.snapshot); apiErr != nil {
		return apiErr
	}
	if rq.endpoint.api == provider.Embeddings {
		if inputs := promptCount(top["input"]); inputs > rq.snapshot.MaxEmbeddingInputs {
			return errTooManyInputs(inputs, rq.snapshot.MaxEmbeddingInputs)
		}
	}

	// stream_options is the one owned object: the provider edits its include_usage, so
	// a repeated member there is refused as at the top level.
	if raw, present := top["stream_options"]; present && string(raw) != "null" {
		options, repeated, ok := decodeMembers(raw)
		if !ok {
			return errInvalidType("stream_options", "an object")
		}
		if repeated != "" {
			return errDuplicateMember("stream_options." + repeated)
		}
		includeUsage, apiErr := optionalField[bool](options, "include_usage", "stream_options.include_usage", "a boolean")
		if apiErr != nil {
			return apiErr
		}
		rq.inbound.IncludeUsage = includeUsage != nil && *includeUsage
	}
	rq.input = accounting.EstimateInput(rq.endpoint.api, rq.body)
	return nil
}

// toolJudge refuses a tool definition (its members, decoded; at, its parameter path)
// the backend would run itself (docs/specs/GATEWAY.md, Client API → hosted tools are
// refused), nil for one the client runs.
type toolJudge func(at string, tool map[string]json.RawMessage) *apiError

// refuseHostedTools refuses a tool list (raw, the request member param) holding a
// tool the backend would run itself: judge looks at every entry — an allowlist, so a
// server tool released later is refused until it is judged. An entry naming a member
// twice is refused: the gateway and the backend could read different ones. An absent
// or null list holds none.
func refuseHostedTools(raw json.RawMessage, param string, judge toolJudge) *apiError {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var tools []json.RawMessage
	if json.Unmarshal(raw, &tools) != nil {
		return errInvalidType(param, "an array")
	}
	for i, tool := range tools {
		at := fmt.Sprintf("%s[%d]", param, i)
		members, repeats, ok := decodeMembersRepeats(tool)
		if !ok {
			return errInvalidType(at, "an object")
		}
		if len(repeats) > 0 {
			return errDuplicateMember(at + "." + repeats[0])
		}
		if apiErr := judge(at, members); apiErr != nil {
			return apiErr
		}
	}
	return nil
}

// readModelAndStream reads the two owned fields every generating format shares:
// model, which must be a non-empty string, and stream.
func readModelAndStream(rq *request, top map[string]json.RawMessage) *apiError {
	if apiErr := readModel(rq, top); apiErr != nil {
		return apiErr
	}
	stream, apiErr := optionalField[bool](top, "stream", "stream", "a boolean")
	if apiErr != nil {
		return apiErr
	}
	rq.inbound.Stream = stream != nil && *stream
	return nil
}

// readModel reads the model the request names, which must be a non-empty string.
func readModel(rq *request, top map[string]json.RawMessage) *apiError {
	model, apiErr := optionalField[string](top, "model", "model", "a string")
	if apiErr != nil {
		return apiErr
	}
	if model == nil || *model == "" {
		return errMissingModel()
	}
	rq.model = *model
	return nil
}

// decodeMembers decodes data, which must hold exactly one JSON object, into its members'
// raw values; ok is false when it does not. repeated names the first member that
// appears twice ("" if none) — names compare after decoding, as every JSON decoder
// reads them.
func decodeMembers(data []byte) (members map[string]json.RawMessage, repeated string, ok bool) {
	members, repeats, ok := decodeMembersRepeats(data)
	if len(repeats) > 0 {
		repeated = repeats[0]
	}
	return members, repeated, ok
}

// decodeMembersRepeats is decodeMembers naming every member that appears more than
// once, in the order their repeats appear.
func decodeMembersRepeats(data []byte) (members map[string]json.RawMessage, repeats []string, ok bool) {
	dec := json.NewDecoder(bytes.NewReader(data))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, nil, false
	}
	members = make(map[string]json.RawMessage)
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, nil, false
		}
		name, _ := tok.(string) // inside an object, More() true means a name comes next
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil, nil, false
		}
		if _, seen := members[name]; seen && !slices.Contains(repeats, name) {
			repeats = append(repeats, name)
		}
		members[name] = value
	}
	if _, err := dec.Token(); err != nil {
		return nil, nil, false
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, nil, false
	}
	return members, repeats, true
}

// sequences reads the owned fields that multiply generation (docs/specs/GATEWAY.md,
// Limits → Output multiplicity): n and best_of on chat and completions, the prompt
// list on completions. Per prompt the backend generates n sequences, or best_of when
// that is larger (it generates best_of and returns the best n) — best_of is
// completions-only in OpenAI's API, but vLLM versions accept it on chat too, so it is
// owned there as well; a completion's prompt may be a list of prompts, each
// generating as many. n and best_of above max_n are
// refused; a value below 1 counts as 1 (the backend refuses it). The product
// saturates rather than overflows, and above max_sequences_per_request it is refused:
// the backend takes it as one request — one slot — however many sequences it holds.
func sequences(api provider.Endpoint, top map[string]json.RawMessage, s *config.Snapshot) (int64, *apiError) {
	if api != provider.ChatCompletions && api != provider.Completions {
		return 1, nil
	}
	perPrompt, perPromptParam := int64(1), "n"
	for _, param := range []string{"n", "best_of"} {
		v, apiErr := optionalField[int64](top, param, param, "an integer")
		if apiErr != nil {
			return 0, apiErr
		}
		if v == nil {
			continue
		}
		if *v > s.MaxN {
			return 0, errNTooLarge(param, s.MaxN)
		}
		if *v > perPrompt {
			perPrompt, perPromptParam = *v, param
		}
	}
	prompts := int64(1)
	if api == provider.Completions {
		prompts = promptCount(top["prompt"])
	}
	total := saturatingMul(perPrompt, prompts)
	if total > s.MaxSequencesPerRequest {
		// The parameter at fault: the prompt list when there is one, else what
		// multiplies the single prompt.
		param := perPromptParam
		if prompts > 1 {
			param = "prompt"
		}
		return 0, errTooManySequences(param, total, s.MaxSequencesPerRequest)
	}
	return total, nil
}

// promptCount is how many prompts a completion's prompt holds, or how many inputs an
// embeddings request's input holds (the two share a shape): a list of strings or of
// token-ID lists is one per element; a string, a single token-ID list (a list of
// numbers) or anything else counts as one — the backend judges its shape.
func promptCount(raw json.RawMessage) int64 {
	var list []json.RawMessage
	if json.Unmarshal(raw, &list) != nil || len(list) == 0 {
		return 1
	}
	for _, e := range list {
		if len(e) > 0 && (e[0] == '-' || (e[0] >= '0' && e[0] <= '9')) {
			continue
		}
		return int64(len(list))
	}
	return 1
}

// saturatingMul is a × b for non-negative a and b, math.MaxInt64 when that overflows.
func saturatingMul(a, b int64) int64 {
	if a != 0 && b > math.MaxInt64/a {
		return math.MaxInt64
	}
	return a * b
}

// optionalField decodes object[key] into a T; nil when the key is absent or null.
// param is the parameter's full name for the error.
func optionalField[T any](object map[string]json.RawMessage, key, param, want string) (*T, *apiError) {
	raw, ok := object[key]
	if !ok {
		return nil, nil
	}
	var value *T
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, errInvalidType(param, want)
	}
	return value, nil
}

// startsWith reports whether a JSON value, past leading whitespace, begins with c.
func startsWith(raw json.RawMessage, c byte) bool {
	raw = bytes.TrimLeft(raw, " \t\r\n")
	return len(raw) > 0 && raw[0] == c
}
