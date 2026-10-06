// Regression tests from the independent pre-merge review of 2026-10-06
// (docs/reviews/2026-10-06/AUDIT-independent.md, [B]).

package provider

import (
	"errors"
	"io"
	"strings"
	"testing"

	"kaiak/internal/config"
)

// A ttl or a cache_control named twice is refused, whichever order the 1-hour one
// comes in.
func TestAnthropicTypesRefuseRepeatedCachePolicy(t *testing.T) {
	for _, policy := range []string{
		`"cache_control":{"type":"ephemeral","ttl":"1h","ttl":"5m"}`,
		`"cache_control":{"type":"ephemeral","ttl":"1h"},"cache_control":{"type":"ephemeral","ttl":"5m"}`,
	} {
		body := `{"model":"pub","max_tokens":10,"messages":[{"role":"user","content":[{"type":"text","text":"hi",` + policy + `}]}]}`
		if err := refusePriceOptions([]byte(body)); err == nil {
			t.Errorf("ambiguous owned price field admitted: %s", policy)
		}
	}
}

// The model name nested in a format's envelope events is rewritten only there: an
// unknown event passes untouched.
func TestUnknownEventsKeepTheirNestedModel(t *testing.T) {
	for _, format := range []string{"messages", "responses"} {
		t.Run(format, func(t *testing.T) {
			unknown := "event: vendor.extension\ndata: {\"type\":\"vendor.extension\",\"message\":{\"model\":\"payload-value\"},\"response\":{\"model\":\"payload-value\"}}\n\n"
			var out string
			var err error
			if format == "messages" {
				out, err = relayMessages(t, config.BackendVLLM, "text/event-stream", []byte(unknown+"data: {\"type\":\"message_stop\"}\n\n"), true)
			} else {
				out, err = relayResponses(t, config.BackendVLLM, "text/event-stream", []byte(unknown+"data: {\"type\":\"response.completed\"}\n\n"), true)
			}
			if !errors.Is(err, io.EOF) {
				t.Fatal(err)
			}
			if !strings.HasPrefix(out, unknown) {
				t.Errorf("unknown event's nested payload was rewritten: %s", out)
			}
		})
	}
}

// A comment before the first data event leaves the first-event window open: an error
// event after it is still the stream's first event, retriable.
func TestCommentBeforeAFirstErrorEventIsRetriable(t *testing.T) {
	stream := ": keepalive\n\nevent: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}\n\n"
	out, err := relayMessages(t, config.BackendAnthropic, "text/event-stream", []byte(stream), true)
	var perr *Error
	if !errors.As(err, &perr) || perr.Code != CodeErrorEvent {
		t.Errorf("first data event is error but cannot retry: err=%v, relayed=%q", err, out)
	}
}
