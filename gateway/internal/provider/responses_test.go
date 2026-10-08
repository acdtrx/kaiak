package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"kaiak/internal/config"
	"kaiak/internal/fakebackend"
)

// relayResponses sends a Responses request through a backend of type typ answering
// with body under contentType, and reads the whole response: what reached the client
// and how it ended.
func relayResponses(t *testing.T, typ config.BackendType, contentType string, body []byte, stream bool) (string, error) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentType)
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	b := &config.Backend{ID: "b", Type: typ, BaseURL: srv.URL + "/v1", ConnectTimeout: time.Second,
		FirstEventTimeout: time.Minute, ResponseTimeout: time.Minute, StallTimeout: time.Minute}
	resp, err := NewRegistry(func(string) (string, bool) { return "", false }).For(b).Send(context.Background(),
		&Request{Endpoint: Responses, Deployment: config.Deployment{Backend: b, Model: "backend-model"},
			Body: []byte(`{"model":"pub","input":"hi"}`), Stream: stream, RequestID: "r", PublicModel: "pub"})
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

// Recorded Responses answers from vLLM and llama-server relay whole: streams end at
// response.completed — llama-server's overlapping output items included, and the
// streams cut by max_output_tokens too — and carry the public model name in every
// response.* event that names one, bodies at the top level; every other byte is the
// backend's.
func TestRecordedResponsesAnswersRelayWhole(t *testing.T) {
	for _, server := range []string{"vllm", "llama-server"} {
		typ := config.BackendType(server)
		for _, name := range []string{"responses-stream.sse", "responses-stream-tool.sse", "responses-stream-incomplete.sse"} {
			t.Run(server+"/"+name, func(t *testing.T) {
				data := fakebackend.Captured(server, name)
				out, err := relayResponses(t, typ, "text/event-stream", data, true)
				if !errors.Is(err, io.EOF) {
					t.Fatalf("ended with %v, want complete", err)
				}
				model := backendModelIn(t, data)
				names := bytes.Count(data, []byte(`"model":"`+model+`"`))
				if strings.Contains(out, model) || strings.Count(out, `"model":"pub"`) != names {
					t.Errorf("the response events do not carry the public name (%d in the recording):\n%.400s", names, out)
				}
				if got, want := len(out), len(data)-names*(len(model)-len(`pub`)); got != want {
					t.Errorf("relayed %d bytes, want %d: only the model name may change", got, want)
				}
				// Cut before response.completed, the same stream is incomplete.
				cut := data[:bytes.LastIndex(data, []byte("event: response.completed"))]
				if _, err := relayResponses(t, typ, "text/event-stream", cut, true); !errors.Is(err, ErrIncomplete) {
					t.Errorf("cut stream ended with %v, want ErrIncomplete", err)
				}
			})
		}
		t.Run(server+"/responses.json", func(t *testing.T) {
			out, err := relayResponses(t, typ, "application/json", fakebackend.Captured(server, "responses.json"), false)
			if !errors.Is(err, io.EOF) || !strings.Contains(out, `"model":"pub"`) || strings.Contains(out, "Qwen3.8") {
				t.Errorf("ended with %v:\n%s", err, out)
			}
		})
	}
}

// OpenAI ends a stream cut by max_output_tokens with response.incomplete: whole too.
func TestResponsesIncompleteEventEndsWhole(t *testing.T) {
	stream := "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"model\":\"m\"}}\n\n" +
		"event: response.incomplete\ndata: {\"type\":\"response.incomplete\",\"response\":{\"model\":\"m\",\"status\":\"incomplete\"}}\n\n"
	out, err := relayResponses(t, config.BackendOpenAI, "text/event-stream", []byte(stream), true)
	if !errors.Is(err, io.EOF) || strings.Count(out, `"model":"pub"`) != 2 {
		t.Errorf("ended with %v:\n%s", err, out)
	}
}

// An error event or response.failed mid-stream reaches the client, then the stream
// ends incomplete; as the first event either is a failure before anything was
// relayed, answered as a 5xx.
func TestResponsesErrorEvents(t *testing.T) {
	created := "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"model\":\"m\"}}\n\n"
	completed := "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"model\":\"m\"}}\n\n"
	for _, failure := range []struct{ name, event string }{
		{"error", "event: error\ndata: {\"type\":\"error\",\"code\":\"server_error\",\"message\":\"boom\"}\n\n"},
		{"response.failed", "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"model\":\"m\",\"status\":\"failed\"}}\n\n"},
	} {
		out, err := relayResponses(t, config.BackendVLLM, "text/event-stream", []byte(created+failure.event+completed), true)
		if !errors.Is(err, ErrIncomplete) || strings.Contains(out, "response.completed") || !strings.Contains(out, "event: "+failure.name+"\n") {
			t.Errorf("%s mid-stream: ended with %v:\n%s", failure.name, err, out)
		}

		_, err = relayResponses(t, config.BackendOpenAI, "text/event-stream", []byte(failure.event), true)
		var perr *Error
		if !errors.As(err, &perr) || perr.Code != CodeErrorEvent {
			t.Errorf("%s first: %v, want %s", failure.name, err, CodeErrorEvent)
		}
	}
}

// Every module sends a Responses request with store: false, the client's true
// replaced; token counting gets no store member.
func TestEveryModuleSendsResponsesWithoutStore(t *testing.T) {
	fb := fakebackend.New()
	defer fb.Close()
	r := moduleRegistry()
	for _, m := range moduleCases {
		b := m.backend(fb, true)
		for _, c := range []struct {
			endpoint Endpoint
			body     string
			want     string
		}{
			{Responses, `{"model":"pub","input":"a"}`, "false"},
			{Responses, `{"model":"pub","input":"a","store":true}`, "false"},
			{ResponsesInputTokens, `{"model":"pub","input":"a"}`, ""},
		} {
			got := sendThrough(t, fb, r, b, c.endpoint, c.body)
			var members map[string]json.RawMessage
			if err := json.Unmarshal(got.Body, &members); err != nil {
				t.Fatalf("backend body %s: %v", got.Body, err)
			}
			if store := members["store"]; string(store) != c.want {
				t.Errorf("%s %s: store %s, want %q: %s", m.typ, c.endpoint.Path(), store, c.want, got.Body)
			}
		}
	}
}
