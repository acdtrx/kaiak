package capture

import (
	"net/http"

	"kaiak/internal/provider"
	"kaiak/internal/telemetry/otlptrace"
)

// exchange is what a capture's content is read from: the request as the client sent
// it and the answer as it received it (docs/specs/GATEWAY.md, Request capture → What
// is captured).
type exchange struct {
	// request is the request's body as received, before the gateway's edits.
	request []byte
	// response is every byte written to the client: the answer — a JSON body, or a
	// stream's events — or an error body; what arrived of it when the response
	// stopped early.
	response []byte
	// stream: the request asked for a stream.
	stream bool
	// status is the status the client was answered: an error answer (400 or above)
	// gives no output messages.
	status int
}

// content is the content attributes of a capture of api — the request's parameters
// and messages, the answer's messages, ID, model and finish reasons — read from x by
// its API's mapping (docs/specs/GATEWAY.md, Request capture → Parameters, Content). An
// error answer (400 or above) — the backend's relayed refusal, the gateway's own
// error body — gives no output messages: error.type says what happened.
func content(api provider.Endpoint, x exchange) []otlptrace.Attribute {
	var m mapping
	switch api {
	case provider.ChatCompletions:
		m = chatCompletionsMapping
	case provider.Messages:
		m = messagesMapping
	case provider.Responses:
		m = responsesMapping
	default:
		return nil
	}
	var an answerContent
	switch {
	case x.status >= http.StatusBadRequest:
	case x.stream:
		an = m.stream(x.response)
	default:
		an = m.answer(x.response)
	}
	return contentAttributes(m.request(x.request), an)
}

// mapping is how an API's exchange is read into the conventions' format: its request,
// its answer as a JSON body, and its answer as an event stream — assembled into what
// the same answer unstreamed gives. Each reads what it is given as far as it has its
// API's shape: a malformed or cut-short input gives what can be read of it.
type mapping struct {
	request func(body []byte) requestContent
	answer  func(body []byte) answerContent
	stream  func(stream []byte) answerContent
}
