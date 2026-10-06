package provider

import (
	"bytes"
	"encoding/json"
	"slices"
)

// A stream's end, read per client API format (docs/specs/GATEWAY.md, Providers:
// complete responses): HTTP framing ending cleanly does not make an answer whole, so
// each format's events say when the stream is complete, and which event is the
// backend giving up mid-stream.

// streamEnd reads what a successful stream's data events say about its end.
type streamEnd interface {
	// observe reads one data event's payload, as the backend sent it, and reports
	// whether it is an error event — the backend abandoning the stream — and what it
	// names: its kind and its code (the error's code, or type where the format names
	// none), as sent.
	observe(payload []byte) (errorEvent *ErrorEvent)
	// complete reports whether the events read so far make the stream whole.
	complete() bool
	// nestedModel names the top-level member of an event whose object carries the
	// model name one level down ("" when events carry it at the top level only).
	nestedModel() string
}

// newStreamEnd is the end reader of a stream in format f.
func newStreamEnd(f Format) streamEnd {
	switch f {
	case FormatMessages:
		return &messagesStreamEnd{}
	case FormatResponses:
		return &responsesStreamEnd{}
	}
	return &openAIStreamEnd{}
}

// openAIStreamEnd: an OpenAI stream is complete once it carried [DONE], or once every
// choice it carried (by index) has its finish_reason — servers that end without
// [DONE] still end whole. The format has no error event.
type openAIStreamEnd struct {
	done bool
	// choices maps each choice index seen to whether its finish_reason arrived.
	choices map[int64]bool
}

func (e *openAIStreamEnd) observe(payload []byte) *ErrorEvent {
	if bytes.Equal(payload, []byte("[DONE]")) {
		e.done = true
		return nil
	}
	var chunk struct {
		Choices []struct {
			Index        int64           `json:"index"`
			FinishReason json.RawMessage `json:"finish_reason"`
		} `json:"choices"`
	}
	// A chunk that does not decode says nothing about the end.
	if json.Unmarshal(payload, &chunk) != nil {
		return nil
	}
	for _, c := range chunk.Choices {
		if e.choices == nil {
			e.choices = make(map[int64]bool)
		}
		finished := len(c.FinishReason) > 0 && !bytes.Equal(c.FinishReason, []byte("null"))
		e.choices[c.Index] = e.choices[c.Index] || finished
	}
	return nil
}

func (e *openAIStreamEnd) complete() bool {
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

func (e *openAIStreamEnd) nestedModel() string { return "" }

// messagesStreamEnd: a Messages stream is complete once it carried message_stop; an
// error event (Anthropic's overloaded_error mid-stream) is the backend abandoning it.
// Events are read by their data's type, which every Messages event carries; content
// blocks may interleave (llama-server opens a block before closing the previous one),
// so nothing here depends on their order.
type messagesStreamEnd struct {
	stopped bool
}

func (e *messagesStreamEnd) observe(payload []byte) *ErrorEvent {
	var event struct {
		Type  string `json:"type"`
		Error struct {
			Type json.RawMessage `json:"type"`
		} `json:"error"`
	}
	// An event that does not decode says nothing about the end.
	if json.Unmarshal(payload, &event) != nil {
		return nil
	}
	switch event.Type {
	case "message_stop":
		e.stopped = true
	case "error":
		var errorType string
		_ = json.Unmarshal(event.Error.Type, &errorType) // a type that is not a string is none
		return &ErrorEvent{Kind: messagesErrorKind(errorType), Code: event.Error.Type}
	}
	return nil
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

func (e *messagesStreamEnd) complete() bool { return e.stopped }

// The model name is message_start's message.model.
func (e *messagesStreamEnd) nestedModel() string { return "message" }

// responsesStreamEnd: a Responses stream is complete once it carried
// response.completed or response.incomplete — vLLM ends a stream cut by
// max_output_tokens with response.completed whose status is incomplete, OpenAI with
// response.incomplete, and both are whole. An error event or response.failed is the
// backend abandoning it. Events are read by their data's type, which every Responses
// event carries; output items may overlap (llama-server adds a function call before
// its reasoning item is done), so nothing here depends on their order.
type responsesStreamEnd struct {
	ended bool
}

func (e *responsesStreamEnd) observe(payload []byte) *ErrorEvent {
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
	if json.Unmarshal(payload, &event) != nil {
		return nil
	}
	code := event.Code
	switch event.Type {
	case "response.completed", "response.incomplete":
		e.ended = true
		return nil
	case "response.failed":
		code = event.Response.Error.Code
	case "error":
	default:
		return nil
	}
	var name string
	_ = json.Unmarshal(code, &name) // a code that is not a string is none
	return &ErrorEvent{Kind: responsesErrorKind(name), Code: code}
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

func (e *responsesStreamEnd) complete() bool { return e.ended }

// The model name is the response's model, in every response.* event that carries the
// response (llama-server's created and in_progress events carry none).
func (e *responsesStreamEnd) nestedModel() string { return "response" }
