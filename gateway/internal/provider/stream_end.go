package provider

import (
	"bytes"
	"encoding/json"
)

// A stream's end, read per client API format (docs/specs/GATEWAY.md, Providers:
// complete responses): HTTP framing ending cleanly does not make an answer whole, so
// each format's events say when the stream is complete, and which event is the
// backend giving up mid-stream.

// streamEnd reads what a successful stream's data events say about its end.
type streamEnd interface {
	// observe reads one data event's payload, as the backend sent it, and reports
	// whether it is an error event: the backend abandoning the stream.
	observe(payload []byte) (errorEvent bool)
	// complete reports whether the events read so far make the stream whole.
	complete() bool
	// nestedModel names the top-level member of an event whose object carries the
	// model name one level down ("" when events carry it at the top level only).
	nestedModel() string
}

// newStreamEnd is the end reader of a stream in format f.
func newStreamEnd(f Format) streamEnd {
	if f == FormatMessages {
		return &messagesStreamEnd{}
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

func (e *openAIStreamEnd) observe(payload []byte) bool {
	if bytes.Equal(payload, []byte("[DONE]")) {
		e.done = true
		return false
	}
	var chunk struct {
		Choices []struct {
			Index        int64           `json:"index"`
			FinishReason json.RawMessage `json:"finish_reason"`
		} `json:"choices"`
	}
	// A chunk that does not decode says nothing about the end.
	if json.Unmarshal(payload, &chunk) != nil {
		return false
	}
	for _, c := range chunk.Choices {
		if e.choices == nil {
			e.choices = make(map[int64]bool)
		}
		finished := len(c.FinishReason) > 0 && !bytes.Equal(c.FinishReason, []byte("null"))
		e.choices[c.Index] = e.choices[c.Index] || finished
	}
	return false
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

func (e *messagesStreamEnd) observe(payload []byte) bool {
	var event struct {
		Type string `json:"type"`
	}
	// An event that does not decode says nothing about the end.
	if json.Unmarshal(payload, &event) != nil {
		return false
	}
	switch event.Type {
	case "message_stop":
		e.stopped = true
	case "error":
		return true
	}
	return false
}

func (e *messagesStreamEnd) complete() bool { return e.stopped }

// The model name is message_start's message.model.
func (e *messagesStreamEnd) nestedModel() string { return "message" }
