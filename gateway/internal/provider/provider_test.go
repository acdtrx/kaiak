package provider

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"kaiak/internal/config"
	"kaiak/internal/fakebackend"
)

func TestURLJoining(t *testing.T) {
	r := NewRegistry(func(string) (string, bool) { return "", false })
	compat := r.For(&config.Backend{ID: "vllm", Type: config.BackendOpenAICompatible, BaseURL: "http://vllm:8000/v1"}).(*openAIFormat)
	azure := r.For(&config.Backend{ID: "azure", Type: config.BackendAzureOpenAI, BaseURL: "https://res.openai.azure.com"}).(*openAIFormat)
	for _, c := range []struct {
		p    *openAIFormat
		e    Endpoint
		want string
	}{
		{compat, ChatCompletions, "http://vllm:8000/v1/chat/completions"},
		{compat, Completions, "http://vllm:8000/v1/completions"},
		{compat, Embeddings, "http://vllm:8000/v1/embeddings"},
		{azure, ChatCompletions, "https://res.openai.azure.com/openai/v1/chat/completions"},
		{azure, Embeddings, "https://res.openai.azure.com/openai/v1/embeddings"},
	} {
		if got := c.p.url(c.e); got != c.want {
			t.Errorf("got %s, want %s", got, c.want)
		}
	}
}

func TestRegistryPoolsPerBackendAndConnectTimeout(t *testing.T) {
	r := NewRegistry(func(string) (string, bool) { return "", false })
	b := &config.Backend{ID: "a", Type: config.BackendOpenAICompatible, ConnectTimeout: time.Second}
	first := r.client(b)
	if r.client(&config.Backend{ID: "a", ConnectTimeout: time.Second}) != first {
		t.Error("same backend and timeout got a new pool")
	}
	if r.client(&config.Backend{ID: "a", ConnectTimeout: 2 * time.Second}) == first {
		t.Error("changed connect timeout kept the old pool")
	}
	if r.client(&config.Backend{ID: "b", ConnectTimeout: time.Second}) == first {
		t.Error("two backends share a pool")
	}
}

func TestRetainDropsRemovedBackendsPools(t *testing.T) {
	r := NewRegistry(func(string) (string, bool) { return "", false })
	kept := &config.Backend{ID: "kept", ConnectTimeout: time.Second}
	first := r.client(kept)
	r.client(&config.Backend{ID: "gone", ConnectTimeout: time.Second})
	r.Retain(map[string]*config.Backend{"kept": kept})
	if len(r.transports) != 1 || r.client(kept) != first {
		t.Errorf("pools %v after Retain(kept), want kept's pool alone, unchanged", r.transports)
	}
}

func TestProbeGetsTheModelsListWithTheBackendCredential(t *testing.T) {
	fb := fakebackend.New()
	defer fb.Close()
	env := map[string]string{"KEY": "probe-secret"}
	r := NewRegistry(func(name string) (string, bool) { v, ok := env[name]; return v, ok })
	compat := &config.Backend{ID: "compat", Type: config.BackendOpenAICompatible, BaseURL: fb.URL() + "/v1",
		APIKeyEnv: "KEY", ConnectTimeout: time.Second}
	azure := &config.Backend{ID: "azure", Type: config.BackendAzureOpenAI, BaseURL: fb.URL(),
		APIKeyEnv: "KEY", ConnectTimeout: time.Second}

	for _, b := range []*config.Backend{compat, azure} {
		if _, err := r.Probe(context.Background(), b); err != nil {
			t.Errorf("%s: %v", b.ID, err)
		}
	}
	got := fb.ModelsRequests()
	if len(got) != 2 {
		t.Fatalf("%d models requests, want 2", len(got))
	}
	if got[0].Method != "GET" || got[0].Path != "/v1/models" || got[0].Header.Get("Authorization") != "Bearer probe-secret" {
		t.Errorf("openai-compatible probe: %s %s, Authorization %q", got[0].Method, got[0].Path, got[0].Header.Get("Authorization"))
	}
	if got[1].Path != "/openai/v1/models" || got[1].Header.Get("Api-Key") != "probe-secret" || got[1].Header.Get("Authorization") != "" {
		t.Errorf("azure probe: %s, api-key %q", got[1].Path, got[1].Header.Get("Api-Key"))
	}
	if len(fb.Requests()) != 0 {
		t.Error("probes recorded as completion requests")
	}

	fb.SetModelsStatus(http.StatusServiceUnavailable)
	_, err := r.Probe(context.Background(), compat)
	if err == nil || !strings.Contains(err.Error(), "503") || strings.Contains(err.Error(), "probe-secret") {
		t.Errorf("probe of a failing models list = %v, want the status and no credential", err)
	}
	down := &config.Backend{ID: "down", Type: config.BackendOpenAICompatible, BaseURL: "http://127.0.0.1:1/v1",
		ConnectTimeout: time.Second}
	if _, err := r.Probe(context.Background(), down); err == nil {
		t.Error("probe of a refused connection = nil")
	}
}

// The probe reads which models the backend lists (H8): a deployment is served only
// when its backend-side model name is among them. Azure's list names models, not the
// deployments requests use, so it is not checked.
func TestProbeReportsTheListedModels(t *testing.T) {
	fb := fakebackend.New()
	defer fb.Close()
	fb.SetModels("Qwen/Qwen3-32B", "other")
	r := NewRegistry(func(string) (string, bool) { return "", false })
	compat := &config.Backend{ID: "compat", Type: config.BackendOpenAICompatible, BaseURL: fb.URL() + "/v1",
		ConnectTimeout: time.Second}
	serves, err := r.Probe(context.Background(), compat)
	if err != nil {
		t.Fatal(err)
	}
	for model, want := range map[string]bool{"Qwen/Qwen3-32B": true, "other": true, "qwen3-32b": false, "llama": false} {
		if got := serves(model); got != want {
			t.Errorf("serves(%q) = %v, want %v", model, got, want)
		}
	}
	azure := &config.Backend{ID: "azure", Type: config.BackendAzureOpenAI, BaseURL: fb.URL(), ConnectTimeout: time.Second}
	serves, err = r.Probe(context.Background(), azure)
	if err != nil || !serves("gpt-4o-deploy") {
		t.Errorf("azure probe = %v, serves(deployment) %v; want every deployment served", err, err == nil && serves("gpt-4o-deploy"))
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "OK")
	}))
	defer srv.Close()
	plain := &config.Backend{ID: "plain", Type: config.BackendOpenAICompatible, BaseURL: srv.URL + "/v1", ConnectTimeout: time.Second}
	if _, err := r.Probe(context.Background(), plain); err == nil || !strings.Contains(err.Error(), "models list") {
		t.Errorf("probe answered with no models list = %v, want an error", err)
	}
}

// A backend 404 saying the deployment's model does not exist is the deployment's
// failure (H8): an error the pipeline retries and counts, not a caller's 4xx relayed.
// Other 404s are relayed as they came.
func TestModelMissingAnswerIsAnError(t *testing.T) {
	fb := fakebackend.New()
	defer fb.Close()
	b := &config.Backend{ID: "b", Type: config.BackendOpenAICompatible, BaseURL: fb.URL() + "/v1",
		ConnectTimeout: time.Second, FirstEventTimeout: time.Minute, ResponseTimeout: time.Minute, StallTimeout: time.Minute}
	send := func(model string) (Response, error) {
		return NewRegistry(func(string) (string, bool) { return "", false }).For(b).Send(context.Background(),
			&Request{Endpoint: ChatCompletions, Deployment: config.Deployment{Backend: b, Model: model},
				Body: []byte(`{"model":"public"}`), RequestID: "r", PublicModel: "public"})
	}
	for _, body := range []string{
		// vLLM
		`{"error":{"message":"The model ` + "`Qwen/Qwen3-32B`" + ` does not exist.","type":"NotFoundError","param":"model","code":404}}`,
		// vLLM, older
		`{"object":"error","message":"The model ` + "`Qwen/Qwen3-32B`" + ` does not exist.","type":"NotFoundError","param":null,"code":404}`,
		// OpenAI
		`{"error":{"message":"The model 'x' does not exist or you do not have access to it.","type":"invalid_request_error","param":null,"code":"model_not_found"}}`,
		// Ollama
		`{"error":"model \"Qwen/Qwen3-32B\" not found, try pulling it first"}`,
	} {
		fb.QueueReplies(fakebackend.Reply{Status: http.StatusNotFound, Body: body})
		resp, err := send("Qwen/Qwen3-32B")
		var perr *Error
		if !errors.As(err, &perr) || perr.Code != CodeModelMissing {
			t.Errorf("404 %s = %v, want upstream_model_missing", body, err)
		}
		if resp != nil {
			resp.Close()
		}
	}
	// A 404 that does not name the model (or names it only inside a longer name) is
	// relayed, body intact.
	for _, body := range []string{
		`{"error":{"message":"LoRA adapter foo not found","type":"NotFoundError","code":404}}`,
		`{"error":{"message":"The model ` + "`Qwen/Qwen3-32B-FP8`" + ` does not exist.","type":"NotFoundError","code":404}}`,
		`not json`,
	} {
		fb.QueueReplies(fakebackend.Reply{Status: http.StatusNotFound, Body: body})
		resp, err := send("Qwen/Qwen3-32B")
		if err != nil {
			t.Errorf("404 %s = %v, want the answer relayed", body, err)
			continue
		}
		var got []byte
		for {
			ev, err := resp.Next()
			if err != nil {
				break
			}
			got = append(got, ev.Data...)
		}
		resp.Close()
		if resp.Status() != http.StatusNotFound || string(got) != body {
			t.Errorf("relayed %d %q, want 404 %q", resp.Status(), got, body)
		}
	}
}

// Backend model names in the backend's own naming (E12): llama-server lists a model
// by its file path. The probe matches the path whole, and a 404 naming it is the
// deployment's failure; a longer path containing it is another model.
func TestPathStyleBackendModelNames(t *testing.T) {
	const path = "/models/qwen3-embedding-0.6b-q8_0.gguf"
	fb := fakebackend.New()
	defer fb.Close()
	fb.SetModels(path)
	b := &config.Backend{ID: "llama", Type: config.BackendOpenAICompatible, BaseURL: fb.URL() + "/v1",
		ConnectTimeout: time.Second, FirstEventTimeout: time.Minute, ResponseTimeout: time.Minute, StallTimeout: time.Minute}
	r := NewRegistry(func(string) (string, bool) { return "", false })
	serves, err := r.Probe(context.Background(), b)
	if err != nil {
		t.Fatal(err)
	}
	for model, want := range map[string]bool{path: true, "qwen3-embedding-0.6b-q8_0.gguf": false, path + ".bak": false} {
		if got := serves(model); got != want {
			t.Errorf("serves(%q) = %v, want %v", model, got, want)
		}
	}
	send := func(body string) error {
		fb.QueueReplies(fakebackend.Reply{Status: http.StatusNotFound, Body: body})
		resp, err := r.For(b).Send(context.Background(), &Request{Endpoint: Embeddings,
			Deployment: config.Deployment{Backend: b, Model: path}, Body: []byte(`{"model":"embed","input":"x"}`),
			RequestID: "r", PublicModel: "embed"})
		if resp != nil {
			resp.Close()
		}
		return err
	}
	var perr *Error
	if err := send(`{"error":{"code":404,"message":"model '` + path + `' not found","type":"not_found_error"}}`); !errors.As(err, &perr) ||
		perr.Code != CodeModelMissing {
		t.Errorf("404 naming the path = %v, want upstream_model_missing", err)
	}
	if err := send(`{"error":{"code":404,"message":"model '` + path + `.bak' not found","type":"not_found_error"}}`); err != nil {
		t.Errorf("404 naming a longer path = %v, want the answer relayed", err)
	}
}

// Sent reports that the request reached the backend in full — accounting bills the
// prompt from then on (D1) — and never fires for a connection refused.
func TestSentReportsTheRequestWrittenInFull(t *testing.T) {
	fb := fakebackend.New()
	defer fb.Close()
	fb.SetReply(fakebackend.Reply{StallBeforeFirstByte: true})
	r := NewRegistry(func(string) (string, bool) { return "", false })
	send := func(ctx context.Context, b *config.Backend, sent func()) error {
		resp, err := r.For(b).Send(ctx, &Request{Endpoint: ChatCompletions, Deployment: config.Deployment{Backend: b, Model: "m"},
			Body: []byte(`{"model":"m","messages":[]}`), RequestID: "req-1", PublicModel: "m", Sent: sent})
		if resp != nil {
			resp.Close()
		}
		return err
	}

	// A backend that takes the request and never answers: Sent fires (and the test
	// then gives up on the answer).
	stalled := &config.Backend{ID: "stall", Type: config.BackendOpenAICompatible, BaseURL: fb.URL() + "/v1",
		ConnectTimeout: time.Second, ResponseTimeout: time.Hour}
	ctx, cancel := context.WithTimeoutCause(context.Background(), 10*time.Second, errors.New("sent never reported"))
	defer cancel()
	if err := send(ctx, stalled, cancel); !errors.Is(err, context.Canceled) {
		t.Errorf("a request never answered: %v, want it cancelled by Sent", err)
	}
	select {
	case <-fb.Arrivals():
	case <-time.After(10 * time.Second):
		t.Error("the backend never got the request")
	}

	var sent atomic.Bool
	down := &config.Backend{ID: "down", Type: config.BackendOpenAICompatible, BaseURL: "http://127.0.0.1:1/v1",
		ConnectTimeout: time.Second, ResponseTimeout: time.Second}
	if err := send(context.Background(), down, func() { sent.Store(true) }); err == nil || sent.Load() {
		t.Errorf("a refused connection: err %v, sent %v; want an error and not sent", err, sent.Load())
	}
}

// A successful stream is complete once it carried [DONE], or once every choice it
// carried has its finish_reason; its end before then is ErrIncomplete. An error
// answer is relayed as it came, complete when it ends.
func TestStreamCompleteness(t *testing.T) {
	chunk := func(choices string) string { return `data: {"id":"c","choices":[` + choices + "]}\n\n" }
	content := func(i int) string {
		return `{"index":` + strconv.Itoa(i) + `,"delta":{"content":"x"},"finish_reason":null}`
	}
	finish := func(i int) string { return `{"index":` + strconv.Itoa(i) + `,"delta":{},"finish_reason":"stop"}` }
	for _, c := range []struct {
		name     string
		status   int
		body     string
		complete bool
	}{
		{"content only", 200, chunk(content(0)), false},
		{"finished", 200, chunk(content(0)) + chunk(finish(0)), true},
		{"finished, then usage only", 200, chunk(content(0)) + chunk(finish(0)) + `data: {"choices":[],"usage":{"prompt_tokens":1}}` + "\n\n", true},
		{"[DONE] without finish_reason", 200, chunk(content(0)) + "data: [DONE]\n\n", true},
		{"one of two choices finished", 200, chunk(content(0)) + chunk(content(1)) + chunk(finish(0)), false},
		{"both choices finished", 200, chunk(content(0)+","+content(1)) + chunk(finish(1)) + chunk(finish(0)), true},
		{"comments only after content", 200, chunk(content(0)) + ": keep-alive\n\n", false},
		{"error answer", 500, chunk(content(0)), true},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(c.status)
				_, _ = io.WriteString(w, c.body)
			}))
			defer srv.Close()
			b := &config.Backend{ID: "b", Type: config.BackendOpenAICompatible, BaseURL: srv.URL + "/v1",
				ConnectTimeout: time.Second, FirstEventTimeout: time.Minute, StallTimeout: time.Minute}
			resp, err := NewRegistry(func(string) (string, bool) { return "", false }).For(b).Send(context.Background(),
				&Request{Endpoint: ChatCompletions, Deployment: config.Deployment{Backend: b, Model: "m"},
					Body: []byte(`{"model":"m","stream":true}`), Stream: true, IncludeUsage: true, RequestID: "r", PublicModel: "m"})
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Close()
			for {
				_, err = resp.Next()
				if err != nil {
					break
				}
			}
			if got := errors.Is(err, io.EOF); got != c.complete {
				t.Errorf("end %v, want complete %v", err, c.complete)
			}
			if !c.complete && !errors.Is(err, ErrIncomplete) {
				t.Errorf("end %v, want ErrIncomplete", err)
			}
		})
	}
}

// A stream that ends before its first event, or a successful JSON body that is empty,
// never reached the client: it fails like a connection lost before the first event.
func TestSuccessfulAnswerEndingBeforeTheFirstEventIsUnavailable(t *testing.T) {
	for _, contentType := range []string{"text/event-stream", "application/json"} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", contentType)
		}))
		b := &config.Backend{ID: "b", Type: config.BackendOpenAICompatible, BaseURL: srv.URL + "/v1",
			ConnectTimeout: time.Second, FirstEventTimeout: time.Minute, ResponseTimeout: time.Minute, StallTimeout: time.Minute}
		_, err := NewRegistry(func(string) (string, bool) { return "", false }).For(b).Send(context.Background(),
			&Request{Endpoint: ChatCompletions, Deployment: config.Deployment{Backend: b, Model: "m"},
				Body: []byte(`{"model":"m"}`), Stream: contentType == "text/event-stream", RequestID: "r", PublicModel: "m"})
		srv.Close()
		var perr *Error
		if !errors.As(err, &perr) || perr.Code != CodeUnavailable || !errors.Is(err, ErrIncomplete) {
			t.Errorf("%s: %v, want upstream_unavailable (incomplete)", contentType, err)
		}
	}
}

// Defense in depth for the independent audit's finding 1: a backend naming one of the
// gateway's own KAIAK_ variables — refused by the config schema, so reaching here
// only past a validation bug — gets no credential: the variable's value never leaves.
func TestReservedVariablesAreNeverSentAsACredential(t *testing.T) {
	fb := fakebackend.New()
	defer fb.Close()
	r := NewRegistry(func(name string) (string, bool) { return "gateway-token", true })
	b := &config.Backend{ID: "sneaky", Type: config.BackendOpenAICompatible, BaseURL: fb.URL() + "/v1",
		APIKeyEnv: "KAIAK_CONTROL_TOKEN", ConnectTimeout: time.Second}
	if _, err := r.Probe(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	got := fb.ModelsRequests()
	if len(got) != 1 || got[0].Header.Get("Authorization") != "" {
		t.Errorf("probe sent Authorization %q", got[0].Header.Get("Authorization"))
	}
	if c := r.credential(b); c != "" {
		t.Errorf("credential %q, want none", c)
	}
}

// An error answer whose body breaks before its first byte is still that answer (the
// independent audit's finding 3): the status came with the headers, so Send returns
// the response — status and headers intact, the break its first Next — and the
// pipeline accounts and retries by the status, not as a request left unanswered. A
// 404 too (its model check reads the body first), and a first-read timeout after
// error headers. A successful status is no answer without its body: still unavailable.
func TestErrorAnswerBrokenBeforeItsBodyKeepsItsStatus(t *testing.T) {
	for _, c := range []struct {
		name   string
		status int
		stall  bool
		stream bool
	}{
		{name: "400 cut", status: 400},
		{name: "404 cut", status: 404},
		{name: "429 cut", status: 429},
		{name: "500 cut", status: 500},
		{name: "429 cut, stream", status: 429, stream: true},
		{name: "429 response timeout", status: 429, stall: true},
		{name: "503 first-event timeout", status: 503, stall: true, stream: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			release := make(chan struct{})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Retry-After", "7")
				if !c.stall {
					w.Header().Set("Content-Length", "100") // promised, never sent: the connection breaks
				}
				w.WriteHeader(c.status)
				w.(http.Flusher).Flush()
				if c.stall {
					select {
					case <-r.Context().Done():
					case <-release:
					}
				}
			}))
			defer srv.Close()
			defer close(release)
			b := &config.Backend{ID: "b", Type: config.BackendOpenAICompatible, BaseURL: srv.URL + "/v1",
				ConnectTimeout: time.Second, FirstEventTimeout: 100 * time.Millisecond,
				ResponseTimeout: 100 * time.Millisecond, StallTimeout: time.Minute}
			var sent atomic.Bool
			resp, err := NewRegistry(func(string) (string, bool) { return "", false }).For(b).Send(context.Background(),
				&Request{Endpoint: ChatCompletions, Deployment: config.Deployment{Backend: b, Model: "m"},
					Body: []byte(`{"model":"m","messages":[]}`), Stream: c.stream, RequestID: "r", PublicModel: "m",
					Sent: func() { sent.Store(true) }})
			if err != nil {
				t.Fatalf("Send: %v, want the %d answer", err, c.status)
			}
			defer resp.Close()
			if resp.Status() != c.status || resp.Header().Get("Retry-After") != "7" || !sent.Load() {
				t.Errorf("status %d, Retry-After %q, sent %v; want %d, 7, sent", resp.Status(),
					resp.Header().Get("Retry-After"), sent.Load(), c.status)
			}
			if _, err := resp.Next(); err == nil || errors.Is(err, io.EOF) {
				t.Errorf("first Next: %v, want the break", err)
			}
		})
	}
}
