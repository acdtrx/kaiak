package provider

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"kaiak/internal/config"
	"kaiak/internal/sse"
)

// Each format's error events are read for their kind by the code or type they name,
// as the matching HTTP status (docs/specs/GATEWAY.md, Providers: error events).
func TestErrorEventKinds(t *testing.T) {
	for _, c := range []struct {
		format  Format
		payload string
		want    ErrorEventKind
	}{
		{FormatMessages, `{"type":"error","error":{"type":"overloaded_error","message":"x"}}`, ErrorEventBusy},
		{FormatMessages, `{"type":"error","error":{"type":"rate_limit_error","message":"x"}}`, ErrorEventBusy},
		{FormatMessages, `{"type":"error","error":{"type":"invalid_request_error","message":"x"}}`, ErrorEventCaller},
		{FormatMessages, `{"type":"error","error":{"type":"api_error","message":"x"}}`, ErrorEventFailure},
		{FormatMessages, `{"type":"error"}`, ErrorEventFailure},
		{FormatResponses, `{"type":"error","code":"rate_limit_exceeded","message":"x"}`, ErrorEventBusy},
		{FormatResponses, `{"type":"error","code":"invalid_prompt","message":"x"}`, ErrorEventCaller},
		{FormatResponses, `{"type":"response.failed","response":{"error":{"code":"failed_to_download_image","message":"x"}}}`, ErrorEventCaller},
		{FormatResponses, `{"type":"response.failed","response":{"error":{"code":"server_error","message":"x"}}}`, ErrorEventFailure},
		{FormatResponses, `{"type":"error","code":null}`, ErrorEventFailure},
	} {
		got := newStreamFormat(c.format).observe([]byte(c.payload)).errorEvent
		if got == nil || got.Kind != c.want {
			t.Errorf("%s: %+v, want kind %d", c.payload, got, c.want)
		}
	}
	for _, payload := range []string{`{"type":"message_stop"}`, `{"type":"response.completed"}`, `not json`} {
		if got := newStreamFormat(FormatResponses).observe([]byte(payload)).errorEvent; got != nil {
			t.Errorf("%s: %+v, want no error event", payload, got)
		}
	}
}

// A relayed error event keeps its type and code but carries the gateway's message,
// never the backend's text (it can name hosts or the backend-side model); its model
// name is rewritten as in any event.
func TestRelayedErrorEventCarriesTheGatewaysMessage(t *testing.T) {
	publicModel, _ := json.Marshal("public")
	for _, c := range []struct {
		format       Format
		event, data  string
		keep, absent []string
	}{
		{FormatMessages, "error", `{"type":"error","error":{"type":"overloaded_error","message":"node gpu-7 for backend-model is down"}}`,
			[]string{`"overloaded_error"`, errorEventMessage}, []string{"gpu-7"}},
		{FormatResponses, "error", `{"type":"error","code":"server_error","message":"host 10.0.0.7 failed","param":null,"sequence_number":4}`,
			[]string{`"server_error"`, `"sequence_number":4`, errorEventMessage}, []string{"10.0.0.7"}},
		{FormatResponses, "response.failed", `{"type":"response.failed","response":{"id":"r","model":"backend-model","error":{"code":"server_error","message":"backend-model crashed"}}}`,
			[]string{`"model":"public"`, `"server_error"`, errorEventMessage}, []string{"backend-model"}},
		{FormatResponses, "error", `{"type":"error","message":"a","message":"b"}`,
			[]string{`"server_error"`, errorEventMessage}, []string{`"a"`, `"b"`}},
	} {
		r := &upstreamResponse{format: newStreamFormat(c.format), publicModel: publicModel}
		r.format.observe([]byte(c.data)) // as readEvent does, before relaying the block
		raw := "event: " + c.event + "\ndata: " + c.data + "\n\n"
		out := string(r.relayedErrorEvent(sse.Block{Raw: []byte(raw), Data: []byte(c.data), HasData: true, Event: c.event}))
		if !strings.HasPrefix(out, "event: "+c.event+"\ndata: ") || !strings.HasSuffix(out, "\n\n") {
			t.Errorf("relayed block %q", out)
		}
		for _, want := range c.keep {
			if !strings.Contains(out, want) {
				t.Errorf("relayed %q lacks %s", out, want)
			}
		}
		for _, gone := range c.absent {
			if strings.Contains(out, gone) {
				t.Errorf("relayed %q still carries %s", out, gone)
			}
		}
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
