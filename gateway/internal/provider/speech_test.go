package provider

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"kaiak/internal/config"
	"kaiak/internal/fakebackend"
)

// omniBackend is a vllm-omni backend at baseURL, with api_key_env "KEY".
func omniBackend(baseURL string) *config.Backend {
	return &config.Backend{ID: "omni", Type: config.BackendVLLMOmni, BaseURL: baseURL, APIKeyEnv: "KEY",
		ConnectTimeout: time.Second, FirstEventTimeout: time.Minute, ResponseTimeout: time.Minute, StallTimeout: time.Minute}
}

// sendSpeech sends body to a vllm-omni backend's speech endpoint for backend-side
// model "backend-model", in the mode stream says, with params.
func sendSpeech(b *config.Backend, body string, stream bool, params ...Param) (Response, error) {
	return moduleRegistry().For(b).Send(context.Background(), &Request{Endpoint: AudioSpeech,
		Deployment: config.Deployment{Backend: b, Model: "backend-model"}, Body: []byte(body), Stream: stream,
		RequestID: "r", PublicModel: "pub", Params: params})
}

// A speech request reaches a vllm-omni backend at /v1/audio/speech with its owned
// edits alone — the model, and the output limit the pipeline sets under
// max_new_tokens — every other byte as the client sent it, stream_format included; its
// answer is audio, relayed byte for byte with its content type, never read as a stream
// or rewritten, complete once its body ended (docs/specs/GATEWAY.md, Providers:
// passthrough edits; media answers are relayed as the backend sends them).
func TestSpeechPassesThroughAsAudio(t *testing.T) {
	fb := fakebackend.New()
	defer fb.Close()
	b := omniBackend(fb.URL() + "/v1")
	for _, c := range []struct {
		name, body, contentType string
		stream                  bool
		size                    int
	}{
		{"whole clip, wav", `{"model":"pub", "input":"hello","voice":"vivian","extra_params":{"model":"keep"}}`,
			"audio/wav", false, 44 + 120000},
		{"whole clip, pcm", `{"model":"pub","input":"hello","response_format":"pcm","sample_rate":8000}`,
			"audio/pcm", false, 40000},
		{"raw-audio stream", `{"model":"pub","input":"hello","stream_format":"audio","task_type":"CustomVoice"}`,
			"audio/wav", true, 44 + 120000},
	} {
		t.Run(c.name, func(t *testing.T) {
			before := len(fb.Requests())
			resp, err := sendSpeech(b, c.body, c.stream, Param{Key: "max_new_tokens", Value: []byte("300")})
			if err != nil {
				t.Fatal(err)
			}
			if resp.Stream() || resp.Header().Get("Content-Type") != c.contentType {
				t.Errorf("stream %v, Content-Type %q; want an audio body, %s", resp.Stream(), resp.Header().Get("Content-Type"), c.contentType)
			}
			var got []byte
			var end error
			for end == nil {
				var ev Event
				ev, end = resp.Next()
				got = append(got, ev.Data...)
			}
			resp.Close()
			if !errors.Is(end, io.EOF) || len(got) != c.size {
				t.Errorf("relayed %d bytes, ended %v; want %d, complete", len(got), end, c.size)
			}
			if c.contentType == "audio/wav" && (!bytes.HasPrefix(got, []byte("RIFF")) || bytes.Contains(got, []byte("pub"))) {
				t.Errorf("audio not relayed as sent: % x…", got[:min(len(got), 48)])
			}
			sent := fb.Requests()[before:]
			want := strings.Replace(c.body, `"model":"pub"`, `"model":"backend-model"`, 1)
			want = want[:len(want)-1] + `,"max_new_tokens":300}`
			if len(sent) != 1 || sent[0].Path != "/v1/audio/speech" || string(sent[0].Body) != want {
				t.Fatalf("sent %+v, want one request to /v1/audio/speech with\n %s", sent, want)
			}
		})
	}
}

// What a speech answer's headers carry where: the client gets the content type and
// the word timing (X-Word-Timestamps and its -Omitted), never the token usage
// headers, the backend's request ID or its cookies; the backend's headers as sent are
// all there for accounting, whatever case the backend wrote their names in. Another
// endpoint on vllm-omni relays no word timing (docs/specs/GATEWAY.md, Providers:
// headers).
func TestSpeechAnswerHeaders(t *testing.T) {
	fb := fakebackend.New()
	defer fb.Close()
	fb.SetReply(fakebackend.Reply{Header: map[string]string{"X-Word-Timestamps": `[{"word":"hello","start":0.1}]`,
		"X-Word-Timestamps-Omitted": "2", "X-Request-Id": "backend-request", "Set-Cookie": "s=1", "Server": "uvicorn"}})
	b := omniBackend(fb.URL() + "/v1")
	resp, err := sendSpeech(b, `{"model":"pub","input":"hello","word_timestamps":true}`, false)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Close()
	client := resp.Header()
	if client.Get("X-Word-Timestamps") != `[{"word":"hello","start":0.1}]` || client.Get("X-Word-Timestamps-Omitted") != "2" ||
		client.Get("Content-Type") != "audio/wav" {
		t.Errorf("client headers %v: want the content type and the word timing", client)
	}
	for name := range client {
		if strings.HasPrefix(strings.ToLower(name), "x-vllm-omni") || name == "X-Request-Id" || name == "Set-Cookie" || name == "Server" {
			t.Errorf("client headers carry %s", name)
		}
	}
	backend := resp.BackendHeader()
	if backend.Get("X-Vllm-Omni-Input-Tokens") != "7" || backend.Get("x-vllm-omni-output-tokens") != "4" {
		t.Errorf("backend headers %v: want the token usage, read whatever its case", backend)
	}

	other, err := moduleRegistry().For(b).Send(context.Background(), &Request{Endpoint: ChatCompletions,
		Deployment: config.Deployment{Backend: b, Model: "backend-model"}, Body: []byte(`{"model":"pub"}`), RequestID: "r",
		PublicModel: "pub"})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if got := other.Header().Get("X-Word-Timestamps"); got != "" {
		t.Errorf("chat relays word timing %q", got)
	}
}

// An audio body is complete once its framing ended: its Content-Length reached, or
// the chunked end; one cut short of its Content-Length breaks off at the connection.
// The SSE stream is complete once it carried speech.audio.done — usage or not — and
// incomplete when it ended without it, deltas alone or an unknown event
// (docs/specs/GATEWAY.md, Providers: complete responses).
func TestSpeechAnswerCompleteness(t *testing.T) {
	const delta = "event: speech.audio.delta\ndata: {\"type\":\"speech.audio.delta\",\"audio\":\"AAAA\",\"response_format\":\"pcm\"}\n\n"
	const done = "event: speech.audio.done\ndata: {\"type\":\"speech.audio.done\"}\n\n"
	for _, c := range []struct {
		name, contentType, body string
		length                  int
		complete                bool
	}{
		{"whole clip", "audio/wav", "RIFF....WAVE", 12, true},
		{"chunked", "audio/pcm", "\x00\x01\x02", 0, true},
		{"cut short", "audio/wav", "RIFF....WAVE", 4096, false},
		{"SSE, done", "text/event-stream", delta + delta + done, 0, true},
		{"SSE, done alone", "text/event-stream", done, 0, true},
		{"SSE, done named by its data alone", "text/event-stream", "data: {\"type\":\"speech.audio.done\",\"usage\":null}\n\n", 0, true},
		{"SSE, deltas alone", "text/event-stream", delta + delta, 0, false},
		{"SSE, done named by its event line alone", "text/event-stream", delta + "event: speech.audio.done\ndata: {}\n\n", 0, false},
		{"SSE, an unknown event", "text/event-stream", delta + "data: {\"type\":\"speech.audio.finished\"}\n\n", 0, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", c.contentType)
				if c.length > 0 {
					w.Header().Set("Content-Length", strconv.Itoa(c.length))
				}
				_, _ = io.WriteString(w, c.body)
				if c.length == 0 {
					// Flushed before the handler ends, the body goes chunked.
					_ = http.NewResponseController(w).Flush()
				}
			}))
			defer srv.Close()
			resp, err := sendSpeech(omniBackend(srv.URL+"/v1"), `{"model":"m","input":"hi"}`, false)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Close()
			for err == nil {
				_, err = resp.Next()
			}
			if got := errors.Is(err, io.EOF); got != c.complete {
				t.Errorf("end %v, want complete %v", err, c.complete)
			}
			if c.contentType == "text/event-stream" && !c.complete && !errors.Is(err, ErrIncomplete) {
				t.Errorf("end %v, want ErrIncomplete", err)
			}
		})
	}
}

// speechEvents reads a speech SSE stream's events as relayed: each one's event line
// and data.
func speechEvents(t *testing.T, stream []byte) (names []string, data []string) {
	t.Helper()
	for block := range strings.SplitSeq(strings.TrimSuffix(string(stream), "\n\n"), "\n\n") {
		name, payload, ok := strings.Cut(block, "\ndata: ")
		if !ok || !strings.HasPrefix(name, "event: ") {
			t.Fatalf("block %q is no speech event", block)
		}
		names, data = append(names, strings.TrimPrefix(name, "event: ")), append(data, payload)
	}
	return names, data
}

// The SSE stream — stream true or stream_format "sse" — is sent with the owned edits
// alone and relayed as the backend sent it, event by event: a WAV stream's header its
// own delta, then the audio's deltas, then speech.audio.done with the usage. Its
// first event is the first delta (docs/specs/GATEWAY.md, Providers: media answers).
func TestSpeechEventStreamPassesThrough(t *testing.T) {
	fb := fakebackend.New()
	defer fb.Close()
	b := omniBackend(fb.URL() + "/v1")
	for _, c := range []struct {
		name, body string
		deltas     int
		audio      int
	}{
		{"wav", `{"model":"pub","input":"hello","stream":true}`, 5, 44 + 120000},
		{"pcm", `{"model":"pub","input":"hello","stream_format":"sse","response_format":"pcm","sample_rate":8000}`, 4, 40000},
	} {
		t.Run(c.name, func(t *testing.T) {
			before := len(fb.Requests())
			resp, err := sendSpeech(b, c.body, true, Param{Key: "max_new_tokens", Value: []byte("300")})
			if err != nil {
				t.Fatal(err)
			}
			if !resp.Stream() || resp.Header().Get("Content-Type") != "text/event-stream" {
				t.Errorf("stream %v, Content-Type %q; want an event stream", resp.Stream(), resp.Header().Get("Content-Type"))
			}
			var relayed []byte
			var end error
			for end == nil {
				var ev Event
				ev, end = resp.Next()
				relayed = append(relayed, ev.Data...)
			}
			resp.Close()
			if !errors.Is(end, io.EOF) {
				t.Errorf("ended %v, want complete", end)
			}
			names, data := speechEvents(t, relayed)
			var audio int
			for i, name := range names {
				var ev struct {
					Type, Audio string
				}
				if json.Unmarshal([]byte(data[i]), &ev) != nil || ev.Type != name {
					t.Fatalf("event %s carries %s", name, data[i])
				}
				decoded, _ := base64.StdEncoding.DecodeString(ev.Audio)
				audio += len(decoded)
				if i == 0 && c.name == "wav" && len(decoded) != 44 {
					t.Errorf("first delta holds %d bytes, want the 44-byte WAV header alone", len(decoded))
				}
			}
			if len(names) != c.deltas+1 || names[len(names)-1] != "speech.audio.done" || audio != c.audio ||
				!strings.Contains(data[len(data)-1], `"input_tokens":7`) || !strings.Contains(data[len(data)-1], `"output_tokens":4`) {
				t.Errorf("relayed %v, %d bytes of audio; want %d deltas of %d bytes, then done with usage", names, audio, c.deltas, c.audio)
			}
			sent := fb.Requests()[before:]
			want := strings.Replace(c.body, `"model":"pub"`, `"model":"backend-model"`, 1)
			want = want[:len(want)-1] + `,"max_new_tokens":300}`
			if len(sent) != 1 || string(sent[0].Body) != want || sent[0].Header.Get("Accept") != "text/event-stream" {
				t.Fatalf("sent %+v, want one request with\n %s\nand Accept: text/event-stream", sent, want)
			}
		})
	}
}

// speech.audio.error as the first event is the backend failing before anything
// reached the client: an error event the attempt loop answers. After the first event
// it is relayed — its type and code kept, the backend's message replaced by the
// gateway's — and the stream ends there, incomplete, nothing after it read
// (docs/specs/GATEWAY.md, Providers: error events).
func TestSpeechErrorEvents(t *testing.T) {
	fb := fakebackend.New()
	defer fb.Close()
	b := omniBackend(fb.URL() + "/v1")
	body := `{"model":"pub","input":"hello","stream":true}`

	fb.SetReply(fakebackend.Reply{Fault: &fakebackend.StreamFault{At: 0, Kind: fakebackend.ErrorEvent}})
	_, err := sendSpeech(b, body, true)
	var perr *Error
	if !errors.As(err, &perr) || perr.Code != CodeErrorEvent || perr.Event.Kind != ErrorEventFailure || string(perr.Event.Code) != "500" {
		t.Fatalf("first event an error: %v, want a failure error event, code 500", err)
	}

	fb.SetReply(fakebackend.Reply{Fault: &fakebackend.StreamFault{At: 2, Kind: fakebackend.ErrorEvent}})
	resp, err := sendSpeech(b, body, true)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Close()
	var relayed []byte
	for err == nil {
		var ev Event
		ev, err = resp.Next()
		relayed = append(relayed, ev.Data...)
	}
	var end *ErrorEventEnd
	if !errors.As(err, &end) || !errors.Is(err, ErrIncomplete) || end.Event.Kind != ErrorEventFailure {
		t.Fatalf("ended %v, want an error event end, incomplete, a failure", err)
	}
	names, data := speechEvents(t, relayed)
	last := data[len(data)-1]
	if len(names) != 4 || names[3] != "speech.audio.error" || !strings.Contains(last, `"code":500`) ||
		!strings.Contains(last, `"partial_audio":true`) || !strings.Contains(last, errorEventMessage) || strings.Contains(last, "CUDA") {
		t.Errorf("relayed %v, last %s: want the header, two deltas, then the error with the gateway's message", names, last)
	}
}
