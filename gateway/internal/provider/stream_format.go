package provider

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"slices"
)

// A client API format's rules on the wire (docs/specs/GATEWAY.md, Providers: complete
// responses, error events): the edits the gateway owns in its requests, and how a
// successful stream's events are read — HTTP framing ending cleanly does not make an
// answer whole, so each format's events say when the stream is complete, and which
// event is the backend giving up mid-stream.

// streamFormat is one client API format's rules: requestEdits for a request's body,
// the rest for the data events of one successful stream (a value per stream).
type streamFormat interface {
	// requestEdits are the edits the format's requests get beside the model name, the
	// module's and the pipeline's; stripUsage marks the usage-only chunk an edit asked
	// for, which the client did not, as hidden.
	requestEdits(req *Request) (edits []memberEdit, stripUsage bool)
	// observe reads one data event's payload, as the backend sent it.
	observe(payload []byte) observation
	// complete reports whether the events read so far make the stream whole.
	complete() bool
	// nestedModel names the top-level member whose object carries the model name one
	// level down in the event last observed — only the format's envelope events carry
	// one: an unknown or extension event's members pass untouched ("" for events
	// that carry the model at the top level only).
	nestedModel() string
	// relayedError is an error event's payload as the client gets it
	// (withGatewayMessage), with the format's own plain error event in place of one
	// whose members cannot be edited.
	relayedError(payload []byte) []byte
}

// observation is what one data event says.
type observation struct {
	// errorEvent: the event is an error event — the backend abandoning the stream —
	// naming its kind and its code (the error's code, or type where the format names
	// none), as sent.
	errorEvent *ErrorEvent
	// usageOnly: the event is the usage-only chunk stream_options.include_usage asks
	// for.
	usageOnly bool
}

// newStreamFormat is format f's rules.
func newStreamFormat(f Format) streamFormat {
	switch f {
	case FormatMessages:
		return &messagesStream{}
	case FormatResponses:
		return &responsesStream{}
	}
	return &openAIStream{}
}

// errorEventMessage replaces the backend's text in a relayed error event: like a 5xx
// body's, it can name hosts, engine internals or the backend-side model
// (docs/specs/GATEWAY.md, Providers: error events). Its type and code stay.
const errorEventMessage = "The model backend ended the response with an error."

// withGatewayMessage is an error event's payload as the client gets it: every message
// it carries — at the top level (a Responses error), in its error object (a Messages
// error), in its response's error (response.failed) — becomes errorEventMessage,
// whichever format's event it is, so a server's event in another shape relays none
// of its text either. A payload whose members cannot be edited (one named twice) is
// replaced by plain, the format's plain error event with %s where the message goes.
func withGatewayMessage(payload []byte, plain string) []byte {
	message, _ := json.Marshal(errorEventMessage) // a string always encodes
	replace := func(current []byte) ([]byte, error) {
		if current == nil {
			return nil, nil
		}
		return message, nil
	}
	inObject := func(edits ...memberEdit) func([]byte) ([]byte, error) {
		return func(current []byte) ([]byte, error) {
			if !startsWith(current, '{') {
				return current, nil
			}
			return editObject(current, edits...)
		}
	}
	errorMessage := memberEdit{key: "error", set: inObject(memberEdit{key: "message", set: replace})}
	edited, err := editObject(payload, memberEdit{key: "message", set: replace}, errorMessage,
		memberEdit{key: "response", set: inObject(errorMessage)})
	if err != nil {
		return fmt.Appendf(nil, plain, message)
	}
	return edited
}

// openAIStream: an OpenAI stream is complete once it carried [DONE], or once every
// choice it carried (by index) has its finish_reason — servers that end without
// [DONE] still end whole. The format has no error event.
type openAIStream struct {
	done bool
	// choices maps each choice index seen to whether its finish_reason arrived.
	choices map[int64]bool
}

// requestEdits: a stream gets stream_options.include_usage, so the backend reports
// usage; when the client did not ask for usage, the usage-only chunk is hidden from
// it.
func (e *openAIStream) requestEdits(req *Request) ([]memberEdit, bool) {
	if req.Stream && !req.IncludeUsage {
		return []memberEdit{{key: "stream_options", set: setIncludeUsage}}, true
	}
	return nil, false
}

// observe reads the chunk's choices for the end, and whether it is the usage-only
// chunk: a JSON object with an empty "choices" array and a non-null "usage". The
// chunk is read in one pass, its members matched by their exact keys; a member named
// twice counts as its last.
func (e *openAIStream) observe(payload []byte) observation {
	if bytes.Equal(payload, []byte("[DONE]")) {
		e.done = true
		return observation{}
	}
	var (
		choices  []openAIChoice
		usage    json.RawMessage
		hasUsage bool
	)
	// A chunk that does not decode says nothing.
	dec := json.NewDecoder(bytes.NewReader(payload))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return observation{}
	}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return observation{}
		}
		switch key, _ := tok.(string); key { // inside an object, More() true means a key comes next
		case "choices":
			choices = nil
			err = dec.Decode(&choices)
		case "usage":
			usage, hasUsage = nil, true
			err = dec.Decode(&usage)
		default:
			err = dec.Decode(new(json.RawMessage))
		}
		if err != nil {
			return observation{}
		}
	}
	if _, err := dec.Token(); err != nil {
		return observation{}
	}
	if _, err := dec.Token(); err != io.EOF {
		return observation{} // data after the object
	}
	for _, c := range choices {
		if e.choices == nil {
			e.choices = make(map[int64]bool)
		}
		finished := len(c.FinishReason) > 0 && !bytes.Equal(c.FinishReason, []byte("null"))
		e.choices[c.Index] = e.choices[c.Index] || finished
	}
	return observation{usageOnly: hasUsage && string(usage) != "null" && choices != nil && len(choices) == 0}
}

// openAIChoice is what the end is read from in a chunk's choice.
type openAIChoice struct {
	Index        int64           `json:"index"`
	FinishReason json.RawMessage `json:"finish_reason"`
}

func (e *openAIStream) complete() bool {
	if e.done {
		return true
	}
	if len(e.choices) == 0 {
		return false
	}
	for _, finished := range e.choices {
		if !finished {
			return false
		}
	}
	return true
}

func (e *openAIStream) nestedModel() string { return "" }

// relayedError is never reached — the format has no error event; were one relayed,
// its plain form is an OpenAI error.
func (e *openAIStream) relayedError(payload []byte) []byte {
	return withGatewayMessage(payload, `{"error":{"message":%s,"type":"server_error","param":null,"code":null}}`)
}

// messagesStream: a Messages stream is complete once it carried message_stop; an
// error event (Anthropic's overloaded_error mid-stream) is the backend abandoning it.
// Events are read by their data's type, which every Messages event carries; content
// blocks may interleave (llama-server opens a block before closing the previous one),
// so nothing here depends on their order.
type messagesStream struct {
	stopped bool
	// envelope: the event last observed is message_start, whose message carries the
	// model name.
	envelope bool
}

// requestEdits: none — a Messages stream always reports usage.
func (e *messagesStream) requestEdits(*Request) ([]memberEdit, bool) { return nil, false }

func (e *messagesStream) observe(payload []byte) observation {
	var event struct {
		Type  string `json:"type"`
		Error struct {
			Type json.RawMessage `json:"type"`
		} `json:"error"`
	}
	// An event that does not decode says nothing about the end.
	e.envelope = false
	if json.Unmarshal(payload, &event) != nil {
		return observation{}
	}
	e.envelope = event.Type == "message_start"
	switch event.Type {
	case "message_stop":
		e.stopped = true
	case "error":
		var errorType string
		_ = json.Unmarshal(event.Error.Type, &errorType) // a type that is not a string is none
		return observation{errorEvent: &ErrorEvent{Kind: messagesErrorKind(errorType), Code: event.Error.Type}}
	}
	return observation{}
}

// messagesErrorKind classifies an Anthropic error type as its HTTP status would be
// (docs/specs/GATEWAY.md, Providers: error events): overloaded_error (the 529) and
// rate_limit_error (the 429) are the backend busy; invalid_request_error,
// request_too_large and not_found_error are the caller's; anything else — api_error,
// a type unknown or missing — is the backend failing.
func messagesErrorKind(errorType string) ErrorEventKind {
	switch errorType {
	case "overloaded_error", "rate_limit_error":
		return ErrorEventBusy
	case "invalid_request_error", "request_too_large", "not_found_error":
		return ErrorEventCaller
	}
	return ErrorEventFailure
}

func (e *messagesStream) complete() bool { return e.stopped }

// The model name is message_start's message.model.
func (e *messagesStream) nestedModel() string {
	if e.envelope {
		return "message"
	}
	return ""
}

// relayedError: the plain form is an api_error.
func (e *messagesStream) relayedError(payload []byte) []byte {
	return withGatewayMessage(payload, `{"type":"error","error":{"type":"api_error","message":%s}}`)
}

// responsesStream: a Responses stream is complete once it carried
// response.completed or response.incomplete — vLLM ends a stream cut by
// max_output_tokens with response.completed whose status is incomplete, OpenAI with
// response.incomplete, and both are whole. An error event or response.failed is the
// backend abandoning it. Events are read by their data's type, which every Responses
// event carries; output items may overlap (llama-server adds a function call before
// its reasoning item is done), so nothing here depends on their order.
type responsesStream struct {
	ended bool
	// envelope: the event last observed is a response lifecycle event, whose response
	// carries the model name.
	envelope bool
}

// responsesLifecycleEvents carry the whole response, model name included.
var responsesLifecycleEvents = []string{"response.created", "response.queued", "response.in_progress",
	"response.completed", "response.incomplete", "response.failed"}

// requestEdits: a Responses request gets store: false — the gateway serves Responses
// stateless and lets no backend keep a conversation (docs/specs/GATEWAY.md, Client
// API → Responses is stateless); its token counting gets none. A Responses stream
// always reports usage.
func (e *responsesStream) requestEdits(req *Request) ([]memberEdit, bool) {
	if req.Endpoint == Responses {
		return []memberEdit{setValue("store", []byte("false"))}, false
	}
	return nil, false
}

func (e *responsesStream) observe(payload []byte) observation {
	var event struct {
		Type     string          `json:"type"`
		Code     json.RawMessage `json:"code"`
		Response struct {
			Error struct {
				Code json.RawMessage `json:"code"`
			} `json:"error"`
		} `json:"response"`
	}
	// An event that does not decode says nothing about the end.
	e.envelope = false
	if json.Unmarshal(payload, &event) != nil {
		return observation{}
	}
	e.envelope = slices.Contains(responsesLifecycleEvents, event.Type)
	code := event.Code
	switch event.Type {
	case "response.completed", "response.incomplete":
		e.ended = true
		return observation{}
	case "response.failed":
		code = event.Response.Error.Code
	case "error":
	default:
		return observation{}
	}
	var name string
	_ = json.Unmarshal(code, &name) // a code that is not a string is none
	return observation{errorEvent: &ErrorEvent{Kind: responsesErrorKind(name), Code: code}}
}

// responsesCallerCodes are the Responses error codes that name the request's own
// fault — its prompt, an image it carries, a policy its content broke — from
// OpenAI's ResponseError (openai-python, src/openai/types/responses/response_error.py,
// main on 2026-10-06).
var responsesCallerCodes = []string{
	"invalid_prompt", "bio_policy", "misalignment_policy_violation", "invalid_image", "invalid_image_format",
	"invalid_base64_image", "invalid_image_url", "image_too_large", "image_too_small", "image_parse_error",
	"image_content_policy_violation", "invalid_image_mode", "image_file_too_large", "unsupported_image_media_type",
	"empty_image_file", "failed_to_download_image", "image_file_not_found",
}

// responsesErrorKind classifies a Responses error code (docs/specs/GATEWAY.md,
// Providers: error events): rate_limit_exceeded is the backend busy; the caller codes
// are the caller's; anything else — server_error, vector_store_timeout,
// data_residency_mismatch, a code unknown or missing — is the backend failing.
func responsesErrorKind(code string) ErrorEventKind {
	switch {
	case code == "rate_limit_exceeded":
		return ErrorEventBusy
	case slices.Contains(responsesCallerCodes, code):
		return ErrorEventCaller
	}
	return ErrorEventFailure
}

func (e *responsesStream) complete() bool { return e.ended }

// The model name is the response's model, in the lifecycle events that carry the
// response (llama-server's created and in_progress events carry none).
func (e *responsesStream) nestedModel() string {
	if e.envelope {
		return "response"
	}
	return ""
}

// relayedError: the plain form is a server_error.
func (e *responsesStream) relayedError(payload []byte) []byte {
	return withGatewayMessage(payload, `{"type":"error","code":"server_error","message":%s,"param":null}`)
}
