package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"kaiak/internal/accounting"
	"kaiak/internal/config"
	"kaiak/internal/provider"
)

// inboundFields are the request parameters the gateway owns (docs/specs/GATEWAY.md,
// Request pipeline). Everything else stays in the raw body, untouched.
type inboundFields struct {
	// Stream is the client's stream; for speech, its mode: true for the raw-audio
	// stream, which takes the stream timers.
	Stream bool
	// OutputLimits are the values of the endpoint's output-limit keys, aligned with
	// endpoint.outputLimitKeys; nil where the request omits the key.
	OutputLimits []*int64
	// ItemOutputLimits are a speech batch's items' own max_new_tokens, by position;
	// nil where an item sets none.
	ItemOutputLimits []*int64
	// ThinkingBudget is a Messages request's thinking.budget_tokens when thinking is
	// enabled; nil otherwise.
	ThinkingBudget *int64
	// IncludeUsage is the client's stream_options.include_usage.
	IncludeUsage bool
	// Sequences is how many output sequences the request asks the backend to
	// generate, each up to the output limit: n (or best_of, when larger) per
	// prompt, times a completion's prompts. At least 1. A speech batch's items are
	// its sequences too, each with its own limit (ItemOutputLimits), which the
	// reservation sums instead.
	Sequences int64
	// Media are the media measures the request counts toward, each with what it
	// reserves before it runs (limits.Need.Media): the images an image request asks
	// for, nothing for the audio a speech request generates, the measured seconds of
	// audio a request uploads. nil for a request that produces no media, which no
	// media limit refuses.
	Media map[config.Measure]int64
	// Speech is what a speech request or a speech batch says about its answer — the
	// sample rate its PCM comes at, a batch item's, whether it is the raw-audio
	// stream — which its meters read.
	Speech accounting.Speech
}

// mediaRequest is what the request says about its answer, which its meters read: a
// speech request's or batch's Speech, the images an image request asks for (its
// image reservation).
func (f *inboundFields) mediaRequest() accounting.MediaRequest {
	return accounting.MediaRequest{Speech: f.Speech, Images: f.Media[config.MeasureImages]}
}

// readInbound parses a body endpoint's request: an upload's under its own rules
// (readUpload); else the raw JSON body, capped by the snapshot's
// max_request_body_bytes and by the whole body budget (a larger body could never be
// held), and the owned fields. The body holds its share of the budget until it is
// dropped (releaseBody, also a finisher).
func readInbound(_ context.Context, rq *request, bodies *BodyBudget, uploads Uploads) *apiError {
	if rq.endpoint.upload != nil {
		return readUpload(rq, uploads)
	}
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
	clearBodyDeadline(rq.w)
	rq.body = body
	return parseOwnedFields(rq)
}

// parseOwnedFields reads the owned fields of the request's format by exact key
// (docs/specs/GATEWAY.md, Client API → owned fields): the ones every format shares —
// model; stream, except on the token-counting endpoints and in the rerank, speech
// batch and images formats, which have none (their stream passes untouched, and the
// request is never a stream) — speech reads it with stream_format for its mode; the
// endpoint's output-limit keys — then the format's own, then the input estimate.
// encoding/json matches struct fields case-insensitively, which would read "Model" as
// model while the backend sees an unknown field; the top-level object is therefore
// decoded as a map. A null value counts as absent, as it does for OpenAI. A top-level
// member named twice is refused (docs/specs/GATEWAY.md, Request pipeline): the gateway
// and the backend could read different occurrences, and the provider edits each owned
// member once.
func parseOwnedFields(rq *request) *apiError {
	top, ok, apiErr := objectMembers(rq.body, "")
	if !ok {
		return errInvalidJSON()
	}
	if apiErr != nil {
		return apiErr
	}
	if apiErr = readModel(rq, top); apiErr != nil {
		return apiErr
	}
	if format := rq.endpoint.api.Format(); !rq.endpoint.counts && format != provider.FormatRerank &&
		format != provider.FormatSpeechBatch && format != provider.FormatImages {
		var stream *bool
		if stream, apiErr = optionalField[bool](top, "stream", "stream", "a boolean"); apiErr != nil {
			return apiErr
		}
		rq.inbound.Stream = stream != nil && *stream
	}
	rq.inbound.OutputLimits = make([]*int64, len(rq.endpoint.outputLimitKeys))
	for i, key := range rq.endpoint.outputLimitKeys {
		if rq.inbound.OutputLimits[i], apiErr = optionalField[int64](top, key, key, "an integer"); apiErr != nil {
			return apiErr
		}
	}
	rq.inbound.Sequences = 1
	switch rq.endpoint.api.Format() {
	case provider.FormatOpenAI:
		apiErr = parseOpenAIFields(rq, top)
	case provider.FormatMessages:
		apiErr = parseMessagesFields(rq, top)
	case provider.FormatResponses:
		apiErr = parseResponsesFields(rq, top)
	case provider.FormatRerank:
		apiErr = parseRerankFields(rq, top)
	case provider.FormatSpeech:
		apiErr = parseSpeechFields(rq, top)
	case provider.FormatSpeechBatch:
		apiErr = parseSpeechBatchFields(rq, top)
	case provider.FormatImages:
		apiErr = parseImagesFields(rq, top)
	default:
		// The endpoint table routes only the formats the inbound stage reads.
		panic("server: no inbound parser for the endpoint's format")
	}
	if apiErr != nil {
		return apiErr
	}
	rq.input = accounting.EstimateInput(rq.endpoint.api, rq.body)
	return nil
}

// parseOpenAIFields reads an OpenAI-format request's own owned fields: the sequence
// count, an embeddings request's inputs and stream_options.include_usage.
func parseOpenAIFields(rq *request, top map[string]json.RawMessage) *apiError {
	var apiErr *apiError
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
		options, ok, apiErr := objectMembers(raw, "stream_options")
		if !ok {
			return errInvalidType("stream_options", "an object")
		}
		if apiErr != nil {
			return apiErr
		}
		includeUsage, apiErr := optionalField[bool](options, "include_usage", "stream_options.include_usage", "a boolean")
		if apiErr != nil {
			return apiErr
		}
		rq.inbound.IncludeUsage = includeUsage != nil && *includeUsage
	}
	return nil
}

// parseRerankFields reads a rerank request's own owned fields (docs/specs/GATEWAY.md,
// Client API → owned fields): texts, TEI's rerank format, refused whatever its value,
// null included — llama-server reads a request naming it as TEI's, takes the documents
// from it when documents is absent or not a list of strings, and answers without
// usage; and how many documents the request holds, refused above
// max_rerank_documents. Its query is read by the input estimate alone; everything
// else — top_n, return_documents, instruction, stream, … — is the backend's and passes
// untouched.
func parseRerankFields(rq *request, top map[string]json.RawMessage) *apiError {
	if _, present := top["texts"]; present {
		return errRerankTEIFormat()
	}
	if documents := rerankDocuments(top["documents"]); documents > rq.snapshot.MaxRerankDocuments {
		return errTooManyDocuments(documents, rq.snapshot.MaxRerankDocuments)
	}
	return nil
}

// parseImagesFields reads an image generation request's own owned fields
// (docs/specs/GATEWAY.md, Client API → owned fields: images), past model
// (parseOwnedFields): n, the images it asks for — absent 1, vLLM-Omni's default; below
// 1 counted as 1, for the backend to refuse — refused above max_images_per_request
// and reserved against the image limits; response_format, b64_json only (absent means
// it): the images are counted from the answer's data, which no other format carries
// countably; lora, refused whatever its value — vLLM-Omni loads the adapter from the
// server path it names; and extra_params, refused whatever its value, null included —
// vLLM-Omni passes it into the diffusion pipeline's extra arguments, and which keys a
// pipeline fetches or reads the gateway cannot know. A null value is otherwise absent.
// stream is not read: an image answer is never a stream. Everything else — prompt,
// negative_prompt, size, layers, seed, … — is the backend's and passes untouched.
func parseImagesFields(rq *request, top map[string]json.RawMessage) *apiError {
	n, apiErr := optionalField[int64](top, "n", "n", "an integer")
	if apiErr != nil {
		return apiErr
	}
	images := int64(1)
	if n != nil && *n > 1 {
		images = *n
	}
	if images > rq.snapshot.MaxImagesPerRequest {
		return errTooManyImages(images, rq.snapshot.MaxImagesPerRequest)
	}
	format, apiErr := optionalField[string](top, "response_format", "response_format", "a string")
	if apiErr != nil {
		return apiErr
	}
	if format != nil && *format != "b64_json" {
		return errResponseFormatUnsupported("response_format", "b64_json")
	}
	if raw, present := top["lora"]; present && string(raw) != "null" {
		return errExternalReference("lora")
	}
	if _, present := top["extra_params"]; present {
		return errExtraParams()
	}
	rq.inbound.Media = map[config.Measure]int64{config.MeasureImages: images}
	return nil
}

// rerankDocuments is how many documents a rerank request's documents holds: one per
// element of a list (listElements); any other value — a string, a multimodal object —
// and an absent or null one count as one, and the backend judges its shape. A
// document list holds no token IDs, unlike an embeddings input (promptCount).
func rerankDocuments(raw json.RawMessage) int64 {
	if !startsWith(raw, '[') {
		return 1
	}
	return listElements(raw)
}

// listElements is how many elements a JSON list holds — raw, a member value
// objectMembers has read whole, which starts with '['. The list is counted, not
// decoded, so the count costs no allocation however long the list: its strings close
// and its brackets balance, and its elements are the commas between them outside
// strings and nested values, plus one. A body at the size cap holding a long list of
// small values costs one pass over the list to refuse against a cap, nothing held.
func listElements(raw json.RawMessage) int64 {
	inside := bytes.TrimLeft(bytes.TrimLeft(raw, " \t\r\n")[1:], " \t\r\n")
	if len(inside) == 0 || inside[0] == ']' {
		return 0
	}
	elements := int64(1)
	depth, inString, escaped := 0, false, false
	for _, c := range inside {
		switch {
		case inString:
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
		case c == '"':
			inString = true
		case c == '[' || c == '{':
			depth++
		case c == ']' || c == '}':
			depth--
		case c == ',' && depth == 0:
			elements++
		}
	}
	return elements
}

// refuseHostedTools refuses a tool list (raw, the request member param) holding a
// tool the backend would run itself (docs/specs/GATEWAY.md, Client API → hosted tools
// are refused): judge looks at every entry — an allowlist, so a server tool released
// later is refused until it is judged — and refuses one the backend would run, nil for
// one the client runs. The list is the gateway's to read: a list or entry of another
// shape is refused (eachObjectStrict).
func refuseHostedTools(raw json.RawMessage, param string, judge objectVisitor) *apiError {
	return eachObjectStrict(raw, param, judge)
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

// objectMembers decodes raw, which must hold exactly one JSON object, into its
// members' raw values; ok is false when it does not. A member named twice is refused
// (apiErr, duplicate_member naming the first repeat under at, the object's path; ""
// for the top level): the gateway and the backend could read different occurrences.
// Names compare after decoding, as every JSON decoder reads them.
func objectMembers(raw json.RawMessage, at string) (members map[string]json.RawMessage, ok bool, apiErr *apiError) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, false, nil
	}
	members = make(map[string]json.RawMessage)
	repeated, hasRepeat := "", false
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, false, nil
		}
		name, _ := tok.(string) // inside an object, More() true means a name comes next
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil, false, nil
		}
		if _, seen := members[name]; seen && !hasRepeat {
			repeated, hasRepeat = name, true
		}
		members[name] = value
	}
	if _, err := dec.Token(); err != nil {
		return nil, false, nil
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, false, nil
	}
	if hasRepeat {
		if at != "" {
			repeated = at + "." + repeated
		}
		return members, true, errDuplicateMember(repeated)
	}
	return members, true, nil
}

// objectVisitor looks at one object of a list: at is its path (param[i]), members its
// members, decoded.
type objectVisitor func(at string, members map[string]json.RawMessage) *apiError

// eachObject calls visit on each object of a JSON list (raw, at path param), stopping
// at the first refusal. An object naming a member twice is refused (objectMembers).
// The list is the backend's to judge: a value that is not a list holds none, and an
// entry that is not an object is skipped.
func eachObject(raw json.RawMessage, param string, visit objectVisitor) *apiError {
	var list []json.RawMessage
	if !startsWith(raw, '[') || json.Unmarshal(raw, &list) != nil {
		return nil
	}
	return visitObjects(list, param, false, visit)
}

// eachObjectStrict is eachObject for a list the gateway reads itself: an absent or
// null list holds none, and a list or entry of another shape is refused
// (invalid_type).
func eachObjectStrict(raw json.RawMessage, param string, visit objectVisitor) *apiError {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var list []json.RawMessage
	if json.Unmarshal(raw, &list) != nil {
		return errInvalidType(param, "an array")
	}
	return visitObjects(list, param, true, visit)
}

func visitObjects(list []json.RawMessage, param string, strict bool, visit objectVisitor) *apiError {
	for i, entry := range list {
		at := fmt.Sprintf("%s[%d]", param, i)
		members, ok, apiErr := objectMembers(entry, at)
		if !ok {
			if strict {
				return errInvalidType(at, "an object")
			}
			continue
		}
		if apiErr != nil {
			return apiErr
		}
		if apiErr := visit(at, members); apiErr != nil {
			return apiErr
		}
	}
	return nil
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
	total := accounting.SaturatingMul(perPrompt, prompts)
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

// inlineData reports whether a reference the backend would resolve — a speech
// request's reference audio, an image edit's input image or mask given as text — is
// inline data: a data: URL, its scheme in any case, as URL schemes are read. An
// allowlist: whatever else a backend learns to fetch or read is refused until it is
// judged (docs/specs/GATEWAY.md, Client API → owned fields: speech; owned parts: image
// edits).
func inlineData(s string) bool {
	const scheme = "data:"
	return len(s) >= len(scheme) && strings.EqualFold(s[:len(scheme)], scheme)
}

// startsWith reports whether a JSON value, past leading whitespace, begins with c.
func startsWith(raw json.RawMessage, c byte) bool {
	raw = bytes.TrimLeft(raw, " \t\r\n")
	return len(raw) > 0 && raw[0] == c
}
