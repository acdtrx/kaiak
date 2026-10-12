package provider

import (
	"bytes"
	"context"
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

// transcriptionForm is a transcription as the inbound stage hands it over: a held body
// and its parts, slices of it — the model, the audio file, a response format and a
// list part repeated.
func transcriptionForm() ([]byte, []FormPart) {
	pieces := []string{"pub", "RIFF\x00\x01\r\n--\xffWAVE", "json", "word", "segment"}
	body := []byte(strings.Join(pieces, ""))
	at := 0
	slice := func(i int) []byte {
		s := body[at : at+len(pieces[i]) : at+len(pieces[i])]
		at += len(pieces[i])
		return s
	}
	return body, []FormPart{
		{Name: "model", Body: slice(0)},
		{Name: "file", File: true, Filename: "memo.wav", ContentType: []byte("audio/wav"), Body: slice(1)},
		{Name: "response_format", Body: slice(2)},
		{Name: "timestamp_granularities[]", Body: slice(3)},
		{Name: "timestamp_granularities[]", Body: slice(4)},
	}
}

// sendTranscription sends the transcription form to b's module for backend-side model
// "backend-model".
func sendTranscription(b *config.Backend) (Response, error) {
	client, parts := transcriptionForm()
	return moduleRegistry().For(b).Send(context.Background(), &Request{Endpoint: AudioTranscriptions,
		Deployment: config.Deployment{Backend: b, Model: "backend-model"}, Body: client, Form: parts, RequestID: "r",
		PublicModel: "pub"})
}

// A transcription reaches a vllm or a llama-server backend at base_url plus
// audio/transcriptions, its form rebuilt with the deployment's model — every other
// part as the client sent it, the list part repeated — sent with its length, never a
// stream; its answer is the server's JSON, relayed as sent with its content type,
// complete once its value closed (docs/specs/GATEWAY.md, Providers → Base URLs,
// passthrough edits, media answers are relayed as the backend sends them).
func TestTranscriptionsReachVLLMAndLlamaServer(t *testing.T) {
	for _, c := range []struct {
		typ    config.BackendType
		shape  fakebackend.TranscriptionShape
		answer string
	}{
		{config.BackendVLLM, fakebackend.VLLMTranscription, `{"text":"Hello from the fake","usage":{"seconds":3,"type":"duration"}}`},
		{config.BackendLlamaServer, fakebackend.LlamaServerTranscription, `{"text":"Hello from the fake","type":"transcript.text.done",` +
			`"usage":{"input_tokens":7,"input_tokens_details":{"cached_tokens":0},"output_tokens":4,"total_tokens":11,` +
			`"type":"tokens"}}`},
	} {
		t.Run(string(c.typ), func(t *testing.T) {
			fb := fakebackend.New()
			defer fb.Close()
			fb.SetModels("backend-model")
			fb.SetTranscriptionShape(c.shape)
			b := &config.Backend{ID: "b", Type: c.typ, BaseURL: fb.URL() + "/v1", ConnectTimeout: time.Second,
				FirstEventTimeout: time.Minute, ResponseTimeout: time.Minute, StallTimeout: time.Minute}
			resp, err := sendTranscription(b)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Close()
			if resp.Stream() || resp.Status() != http.StatusOK || resp.Header().Get("Content-Type") != "application/json" {
				t.Errorf("stream %v, %d, Content-Type %q; want a JSON body", resp.Stream(), resp.Status(),
					resp.Header().Get("Content-Type"))
			}
			got, end := readEvents(resp)
			if !errors.Is(end, io.EOF) || string(got) != c.answer {
				t.Errorf("ended %v, relayed %s; want the backend's %s, complete", end, got, c.answer)
			}
			sent := fb.Requests()
			if len(sent) != 1 || sent[0].Path != "/v1/audio/transcriptions" {
				t.Fatalf("sent %+v, want one request to /v1/audio/transcriptions", sent)
			}
			_, parts := transcriptionForm()
			if want := writtenForm(t, boundaryOf(t, sent[0].Header.Get("Content-Type")), parts, "backend-model"); !bytes.Equal(sent[0].Body, want) {
				t.Errorf("the backend got\n%q\nwant\n%q", sent[0].Body, want)
			}
			if got := sent[0].Header.Get("Accept"); got != "application/json" {
				t.Errorf("Accept %q, want application/json: a transcription is never a stream", got)
			}
		})
	}
}

// vLLM's model check on a transcription answers a model it does not serve with its
// 404 naming it: the deployment's model is upstream_model_missing; a 404 naming
// another model is relayed whole (docs/specs/GATEWAY.md, Providers → Wrong model on a
// host).
func TestVLLMTranscriptionModelMissing(t *testing.T) {
	const missing = `{"error":{"message":"The model ` + "`backend-model`" + ` does not exist.","type":"NotFoundError","param":"model","code":404}}`
	const other = `{"error":{"message":"The model ` + "`backend-model-v2`" + ` does not exist.","type":"NotFoundError","param":"model","code":404}}`
	s := newWireServer(t)
	b := wireBackend(s, config.BackendVLLM)

	s.set(http.StatusNotFound, missing)
	_, err := sendTranscription(b)
	if perr, ok := errors.AsType[*Error](err); !ok || perr.Code != CodeModelMissing {
		t.Errorf("404 naming the deployment's model: %v, want upstream_model_missing", err)
	}

	s.set(http.StatusNotFound, other)
	resp, err := sendTranscription(b)
	if err != nil {
		t.Fatalf("404 naming another model: %v, want it relayed", err)
	}
	if got := readAll(resp); resp.Status() != http.StatusNotFound || got != other {
		t.Errorf("relayed %d %s, want 404 %s", resp.Status(), got, other)
	}
}

// A transcription's answer is relayed as the backend sent it, its content type kept —
// JSON with its top-level model rewritten should it carry one, another type
// untouched — complete once it ends whole; an event stream answering a transcription
// is no answer and never complete (docs/specs/GATEWAY.md, Providers: media answers,
// complete responses).
func TestTranscriptionAnswerCompleteness(t *testing.T) {
	const whole = `{"text":"Hello","usage":{"type":"duration","seconds":3}}`
	for _, c := range []struct {
		name, contentType, body, relayed string
		complete                         bool
	}{
		{"whole body", "application/json", whole, whole, true},
		{"a model, rewritten", "application/json", `{"model":"backend-model","text":"Hello"}`, `{"model":"pub","text":"Hello"}`, true},
		{"plain text", "text/plain; charset=utf-8", "Hello", "Hello", true},
		{"body cut short", "application/json", whole[:30], whole[:30], false},
		{"an event stream", "text/event-stream", "data: " + whole + "\n\n", "data: " + whole + "\n\n", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", c.contentType)
				_, _ = io.WriteString(w, c.body)
			}))
			defer srv.Close()
			b := &config.Backend{ID: "b", Type: config.BackendLlamaServer, BaseURL: srv.URL + "/v1", ConnectTimeout: time.Second,
				FirstEventTimeout: time.Minute, ResponseTimeout: time.Minute, StallTimeout: time.Minute}
			resp, err := sendTranscription(b)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Close()
			if got := resp.Header().Get("Content-Type"); got != c.contentType {
				t.Errorf("Content-Type %q, want the backend's %q", got, c.contentType)
			}
			got, end := readEvents(resp)
			if string(got) != c.relayed {
				t.Errorf("relayed %s, want %s", got, c.relayed)
			}
			if complete := errors.Is(end, io.EOF); complete != c.complete || (!c.complete && !errors.Is(end, ErrIncomplete)) {
				t.Errorf("end %v, want complete %v", end, c.complete)
			}
		})
	}
}
