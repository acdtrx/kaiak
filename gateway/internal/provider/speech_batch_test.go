package provider

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"kaiak/internal/config"
	"kaiak/internal/fakebackend"
)

// A speech batch reaches a vllm-omni backend at /v1/audio/speech/batch with its owned
// edit alone — the model; the pipeline sets no output limit on speech — every other
// byte as the client sent it, the items' and stream included; its answer is the
// backend's JSON, relayed as sent, never a stream, complete once its value closed
// (docs/specs/GATEWAY.md, Providers: passthrough edits; media answers are relayed as
// the backend sends them).
func TestSpeechBatchPassesThroughAsJSON(t *testing.T) {
	fb := fakebackend.New()
	defer fb.Close()
	b := omniBackend(fb.URL() + "/v1")
	const body = `{"model":"pub", "items":[{"input":"one"} , {"input":"two","max_new_tokens":1000,"voice":"x"},` +
		`{"input":"three","response_format":"pcm","sample_rate":8000}],"stream":"yes","voice":"vivian"}`
	resp, err := moduleRegistry().For(b).Send(context.Background(), &Request{Endpoint: AudioSpeechBatch,
		Deployment: config.Deployment{Backend: b, Model: "backend-model"}, Body: []byte(body), RequestID: "r", PublicModel: "pub"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Stream() || resp.Header().Get("Content-Type") != "application/json" {
		t.Errorf("stream %v, Content-Type %q; want a JSON body", resp.Stream(), resp.Header().Get("Content-Type"))
	}
	var got []byte
	var end error
	for end == nil {
		var ev Event
		ev, end = resp.Next()
		got = append(got, ev.Data...)
	}
	resp.Close()
	if !errors.Is(end, io.EOF) || !strings.HasPrefix(string(got), `{"id":"speech-batch-fake","results":[{"index":0,"status":"success","audio_data":"UklGR`) ||
		!strings.Contains(string(got), `"media_type":"audio/pcm"`) {
		t.Errorf("ended %v, relayed %.200s…; want the backend's answer, complete", end, got)
	}
	sent := fb.Requests()
	want := `{"model":"backend-model", "items":[{"input":"one"} , {"input":"two","max_new_tokens":1000,"voice":"x"},` +
		`{"input":"three","response_format":"pcm","sample_rate":8000}],"stream":"yes","voice":"vivian"}`
	if len(sent) != 1 || sent[0].Path != "/v1/audio/speech/batch" || string(sent[0].Body) != want {
		t.Fatalf("sent %+v, want one request to /v1/audio/speech/batch with\n %s", sent, want)
	}
	if got := sent[0].Header.Get("Accept"); got != "application/json" {
		t.Errorf("Accept %q, want application/json: a batch is never a stream", got)
	}
}

// A speech batch answer is a JSON body, complete once its top-level value closed, its
// top-level model the public name when it carries one and each errored result's text
// the gateway's; an event stream answering a batch is no batch answer and never
// complete, and an error answer is relayed as it came (docs/specs/GATEWAY.md,
// Providers: complete responses, media answers).
func TestSpeechBatchAnswerCompleteness(t *testing.T) {
	const whole = `{"id":"b","results":[{"index":0,"status":"error","error":"x"}],"total":1,"succeeded":0,"failed":1}`
	const edited = `{"id":"b","results":[{"index":0,"status":"error","error":"The model backend could not generate this item."}],` +
		`"total":1,"succeeded":0,"failed":1}`
	const refused = `{"error":{"message":"m","type":"BadRequestError","code":400},"results":[{"error":"x"}]}`
	for _, c := range []struct {
		name, contentType, body, relayed string
		status                           int
		complete                         bool
	}{
		{"whole body", "application/json", whole, edited, http.StatusOK, true},
		{"a model, rewritten", "application/json", `{"model":"backend-model","results":[]}`, `{"model":"pub","results":[]}`,
			http.StatusOK, true},
		{"body cut short", "application/json", whole[:30], whole[:30], http.StatusOK, false},
		{"an event stream", "text/event-stream", "data: " + whole + "\n\n", "data: " + whole + "\n\n", http.StatusOK, false},
		{"an error answer", "application/json", refused, refused, http.StatusBadRequest, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", c.contentType)
				w.WriteHeader(c.status)
				_, _ = io.WriteString(w, c.body)
			}))
			defer srv.Close()
			b := omniBackend(srv.URL + "/v1")
			resp, err := moduleRegistry().For(b).Send(context.Background(), &Request{Endpoint: AudioSpeechBatch,
				Deployment: config.Deployment{Backend: b, Model: "backend-model"},
				Body:       []byte(`{"model":"pub","items":[{"input":"a"}]}`), RequestID: "r", PublicModel: "pub"})
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Close()
			var got []byte
			for err == nil {
				var ev Event
				ev, err = resp.Next()
				got = append(got, ev.Data...)
			}
			if string(got) != c.relayed {
				t.Errorf("relayed %s, want %s", got, c.relayed)
			}
			if complete := errors.Is(err, io.EOF); complete != c.complete || (!c.complete && !errors.Is(err, ErrIncomplete)) {
				t.Errorf("end %v, want complete %v", err, c.complete)
			}
		})
	}
}
