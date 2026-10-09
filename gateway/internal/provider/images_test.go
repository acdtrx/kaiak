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

// sendImages sends body to a vllm-omni backend's image generation endpoint for
// backend-side model "backend-model".
func sendImages(b *config.Backend, body string) (Response, error) {
	return moduleRegistry().For(b).Send(context.Background(), &Request{Endpoint: ImagesGenerations,
		Deployment: config.Deployment{Backend: b, Model: "backend-model"}, Body: []byte(body), RequestID: "r", PublicModel: "pub"})
}

// readEvents reads a response to its end: the bytes relayed and the error that ended it.
func readEvents(resp Response) ([]byte, error) {
	var got []byte
	for {
		ev, err := resp.Next()
		if err != nil {
			return got, err
		}
		got = append(got, ev.Data...)
	}
}

// An image request reaches a vllm-omni backend at /v1/images/generations with the
// model edit alone — every other byte as the client sent it, stream and whitespace
// included, nothing added; its answer is the backend's JSON, relayed as sent, never a
// stream, complete once its value closed (docs/specs/GATEWAY.md, Providers:
// passthrough edits; media answers are relayed as the backend sends them).
func TestImagesPassThroughAsJSON(t *testing.T) {
	fb := fakebackend.New()
	defer fb.Close()
	fb.SetReply(fakebackend.Reply{ImageBytes: 3000})
	b := omniBackend(fb.URL() + "/v1")
	const body = `{"model":"pub", "prompt":"a red fox","negative_prompt":"blur","n":2,"size":"512x512",` +
		`"stream":true,"seed":7,"extra":{"model":"keep"}}`
	resp, err := sendImages(b, body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Close()
	if resp.Stream() || resp.Header().Get("Content-Type") != "application/json" {
		t.Errorf("stream %v, Content-Type %q; want a JSON body", resp.Stream(), resp.Header().Get("Content-Type"))
	}
	got, end := readEvents(resp)
	if !errors.Is(end, io.EOF) || !strings.HasPrefix(string(got), `{"created":1700000000,"data":[{"b64_json":"iVBORw0KGgo`) ||
		strings.Count(string(got), `"b64_json"`) != 2 || !strings.HasSuffix(string(got), `"size":"512x512","cot_output":null,"metrics":{}}`) {
		t.Errorf("ended %v, relayed %.120s… (%d bytes); want the backend's answer of two images, complete", end, got, len(got))
	}
	sent := fb.Requests()
	want := strings.Replace(body, `"pub"`, `"backend-model"`, 1)
	if len(sent) != 1 || sent[0].Path != "/v1/images/generations" || string(sent[0].Body) != want {
		t.Fatalf("sent %+v, want one request to /v1/images/generations with\n %s", sent, want)
	}
	if got := sent[0].Header.Get("Accept"); got != "application/json" {
		t.Errorf("Accept %q, want application/json: an image request is never a stream", got)
	}
}

// An image answer is a JSON body, complete once its top-level value closed, its
// top-level model the public name should it carry one; an event stream answering an
// image request is no image answer and never complete (docs/specs/GATEWAY.md,
// Providers: complete responses, media answers).
func TestImagesAnswerCompleteness(t *testing.T) {
	const whole = `{"created":1,"data":[{"b64_json":"iVBORw0KGgo=","url":null,"revised_prompt":null}],"output_format":"png"}`
	for _, c := range []struct {
		name, contentType, body, relayed string
		complete                         bool
	}{
		{"whole body", "application/json", whole, whole, true},
		{"a model, rewritten", "application/json", `{"model":"backend-model","data":[]}`, `{"model":"pub","data":[]}`, true},
		{"body cut short", "application/json", whole[:40], whole[:40], false},
		{"an event stream", "text/event-stream", "data: " + whole + "\n\n", "data: " + whole + "\n\n", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", c.contentType)
				_, _ = io.WriteString(w, c.body)
			}))
			defer srv.Close()
			resp, err := sendImages(omniBackend(srv.URL+"/v1"), `{"model":"pub","prompt":"a fox"}`)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Close()
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
