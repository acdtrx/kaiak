package provider

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kaiak/internal/config"
)

// captured reads a recorded backend answer (fakebackend/captures).
func captured(t *testing.T, server, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "fakebackend", "captures", server, name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// relayMessages sends a Messages request through a backend of type typ answering
// with body under contentType, and reads the whole response: what reached the client
// and how it ended.
func relayMessages(t *testing.T, typ config.BackendType, contentType string, body []byte, stream bool) (string, error) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentType)
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	b := &config.Backend{ID: "b", Type: typ, BaseURL: srv.URL + "/v1", ConnectTimeout: time.Second,
		FirstEventTimeout: time.Minute, ResponseTimeout: time.Minute, StallTimeout: time.Minute}
	resp, err := NewRegistry(func(string) (string, bool) { return "", false }).For(b).Send(context.Background(),
		&Request{Endpoint: Messages, Deployment: config.Deployment{Backend: b, Model: "backend-model"},
			Body: []byte(`{"model":"pub","max_tokens":10,"messages":[]}`), Stream: stream, RequestID: "r", PublicModel: "pub"})
	if err != nil {
		return "", err
	}
	defer resp.Close()
	var out bytes.Buffer
	for {
		ev, err := resp.Next()
		if err != nil {
			return out.String(), err
		}
		out.Write(ev.Data)
	}
}

// Recorded Messages answers from vLLM and llama-server relay whole: streams end at
// message_stop — llama-server's interleaved content blocks included — and carry the
// public model name in message_start, bodies at the top level; every other byte is the
// backend's.
func TestRecordedMessagesAnswersRelayWhole(t *testing.T) {
	for _, server := range []string{"vllm", "llama-server"} {
		typ := config.BackendType(server)
		for _, name := range []string{"messages-stream.sse", "messages-stream-tool.sse", "messages-stream-max-tokens.sse"} {
			t.Run(server+"/"+name, func(t *testing.T) {
				data := captured(t, server, name)
				out, err := relayMessages(t, typ, "text/event-stream", data, true)
				if !errors.Is(err, io.EOF) {
					t.Fatalf("ended with %v, want complete", err)
				}
				if !strings.Contains(out, `"model":"pub"`) || strings.Contains(out, "Qwen3.8") {
					t.Errorf("message_start does not carry the public name:\n%.400s", out)
				}
				if got, want := len(out), len(data)-len(backendModelIn(t, data))+len(`pub`); got != want {
					t.Errorf("relayed %d bytes, want %d: only the model name may change", got, want)
				}
				// Cut before message_stop, the same stream is incomplete.
				cut := data[:bytes.LastIndex(data, []byte("event: message_stop"))]
				if _, err := relayMessages(t, typ, "text/event-stream", cut, true); !errors.Is(err, ErrIncomplete) {
					t.Errorf("cut stream ended with %v, want ErrIncomplete", err)
				}
			})
		}
		t.Run(server+"/messages.json", func(t *testing.T) {
			out, err := relayMessages(t, typ, "application/json", captured(t, server, "messages.json"), false)
			if !errors.Is(err, io.EOF) || !strings.Contains(out, `"model":"pub"`) || strings.Contains(out, "Qwen3.8") {
				t.Errorf("ended with %v:\n%s", err, out)
			}
		})
	}
}

// backendModelIn is the backend's model name as a recorded stream's message_start
// carries it.
func backendModelIn(t *testing.T, data []byte) string {
	t.Helper()
	_, rest, ok := bytes.Cut(data, []byte(`"model":"`))
	name, _, found := bytes.Cut(rest, []byte(`"`))
	if !ok || !found {
		t.Fatal("no model in the recording")
	}
	return string(name)
}

// An error event mid-stream reaches the client, then the stream ends incomplete; as
// the first event it is a failure before anything was relayed, answered as a 5xx.
func TestMessagesErrorEvent(t *testing.T) {
	start := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"m\",\"usage\":{\"input_tokens\":1}}}\n\n"
	errorEvent := "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}\n\n"
	stop := "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"

	out, err := relayMessages(t, config.BackendVLLM, "text/event-stream", []byte(start+errorEvent+stop), true)
	if !errors.Is(err, ErrIncomplete) || !strings.Contains(out, "overloaded_error") || strings.Contains(out, "message_stop") {
		t.Errorf("ended with %v:\n%s", err, out)
	}

	_, err = relayMessages(t, config.BackendAnthropic, "text/event-stream", []byte(errorEvent), true)
	var perr *Error
	if !errors.As(err, &perr) || perr.Code != CodeErrorEvent {
		t.Errorf("first-event error: %v, want %s", err, CodeErrorEvent)
	}
}
