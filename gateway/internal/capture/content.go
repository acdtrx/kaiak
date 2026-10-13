package capture

import (
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
// its API's mapping (docs/specs/GATEWAY.md, Request capture → Parameters, Content).
func content(api provider.Endpoint, x exchange) []otlptrace.Attribute {
	switch api {
	case provider.ChatCompletions:
		return chatCompletionsContent(x)
	case provider.Messages:
		return messagesContent(x)
	case provider.Responses:
		return responsesContent(x)
	}
	return nil
}

// chatCompletionsContent is a Chat Completions exchange's content attributes
// (docs/specs/GATEWAY.md, Request capture → Chat Completions).
func chatCompletionsContent(exchange) []otlptrace.Attribute { return nil }

// messagesContent is a Messages exchange's content attributes (docs/specs/GATEWAY.md,
// Request capture → Messages).
func messagesContent(exchange) []otlptrace.Attribute { return nil }

// responsesContent is a Responses exchange's content attributes
// (docs/specs/GATEWAY.md, Request capture → Responses).
func responsesContent(exchange) []otlptrace.Attribute { return nil }
