package server

import (
	"bufio"
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

	"kaiak/internal/fakebackend"
)

// waitTimeout bounds every wait on something that must happen promptly.
const waitTimeout = 5 * time.Second

// onlyRequest returns the one request the backend received.
func onlyRequest(t *testing.T, b *fakebackend.Backend) *fakebackend.Request {
	t.Helper()
	reqs := b.Requests()
	if len(reqs) != 1 {
		t.Fatalf("backend received %d requests, want 1", len(reqs))
	}
	return reqs[0]
}

// directStream asks the fake backend for a stream with body, bypassing the gateway:
// the bytes the backend sends for it.
func directStream(t *testing.T, b *fakebackend.Backend, body []byte) string {
	t.Helper()
	return directPost(t, b, "/v1/chat/completions", body)
}

// directPost sends body to the fake backend's path, bypassing the gateway: the bytes
// the backend answers with.
func directPost(t *testing.T, b *fakebackend.Backend, path string, body []byte) string {
	t.Helper()
	resp, err := http.Post(b.URL()+path, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// sseEvents splits an LF-framed event stream into its events (blank line included).
func sseEvents(stream string) []string {
	var events []string
	for rest := stream; rest != ""; {
		event, after, found := strings.Cut(rest, "\n\n")
		if !found {
			return append(events, rest)
		}
		events = append(events, event+"\n\n")
		rest = after
	}
	return events
}

func TestNonStreamRelayIsFaithfulExceptTheModel(t *testing.T) {
	g := newTestGateway(t)
	g.backend.SetReply(fakebackend.Reply{Header: map[string]string{
		"X-Request-Id": "backend-id", "Set-Cookie": "session=1", "Openai-Processing-Ms": "12", "Server": "fake",
	}})
	body := `{"model":"renamed","messages":[{"role":"user","content":"hi"}]}`
	w := do(t, g.h, call{method: "POST", path: "/v1/chat/completions", key: workloadKey, body: body,
		header: map[string]string{"X-Request-Id": "rid-42", "Cookie": "c=1", "X-Custom": "v"}})
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}

	got := onlyRequest(t, g.backend)
	if got.Path != "/v1/chat/completions" {
		t.Errorf("backend path %q", got.Path)
	}
	// The model and the standard service tier are the gateway's edits (a chat request
	// always carries it); every other byte is the client's.
	wantBody := strings.TrimSuffix(strings.Replace(body, `"renamed"`, `"vendor/renamed-7b-instruct"`, 1), "}") +
		`,"service_tier":"default"}`
	if string(got.Body) != wantBody {
		t.Errorf("backend body\n%s\nwant\n%s", got.Body, wantBody)
	}
	if got.Header.Get("X-Request-Id") != "rid-42" {
		t.Errorf("x-request-id to backend %q", got.Header.Get("X-Request-Id"))
	}
	if got.Header.Get("Authorization") != "Bearer "+localBackendKey {
		t.Errorf("backend Authorization %q, want the gateway's backend credential", got.Header.Get("Authorization"))
	}
	for _, name := range []string{"Cookie", "X-Custom", "Api-Key"} {
		if v := got.Header.Get(name); v != "" {
			t.Errorf("client header %s forwarded to the backend: %q", name, v)
		}
	}

	// The client gets the backend's body byte for byte, and only allowlisted headers.
	var answer struct {
		Model   string `json:"model"`
		Choices []struct {
			Message struct{ Content string } `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &answer); err != nil || answer.Choices[0].Message.Content != "Hello from the fake" {
		t.Errorf("client body %s (%v)", w.Body.String(), err)
	}
	// Byte for byte the backend's answer, except the model carries the public name.
	direct := directPost(t, g.backend, "/v1/chat/completions", got.Body)
	if want := strings.Replace(direct, `"model":"vendor/renamed-7b-instruct"`, `"model":"renamed"`, 1); w.Body.String() != want || want == direct {
		t.Errorf("client body\n%s\nwant\n%s", w.Body.String(), want)
	}
	if answer.Model != "renamed" {
		t.Errorf("response model %q, want the public name", answer.Model)
	}
	if v := w.Header().Get("X-Accel-Buffering"); v != "" {
		t.Errorf("non-stream answer has X-Accel-Buffering %q", v)
	}
	if w.Header().Get("X-Request-Id") != "rid-42" || w.Header().Get("Content-Type") != "application/json" {
		t.Errorf("client headers %v", w.Header())
	}
	for _, name := range []string{"Set-Cookie", "Openai-Processing-Ms", "Server"} {
		if v := w.Header().Get(name); v != "" {
			t.Errorf("backend header %s relayed: %q", name, v)
		}
	}
}

func TestUnknownFieldsReachTheBackendUntouched(t *testing.T) {
	g := newTestGateway(t)
	body := "{ \"vendor_opts\": {\"nested\": {\"deep\": [1, 2.50, {\"x\": null}]}, \"flag\": true},\n" +
		"  \"model\" : \"renamed\",\n" +
		"  \"big\": 123456789012345678901234567890, \"precise\": 0.10000000000000000000000000001,\n" +
		"  \"exp\": 1E+400, \"text\": \"caf\\u00e9 \\\"quoted\\\"\",\n" +
		"  \"messages\": [ {\"role\": \"user\", \"content\": \"hi\"} ], \"empty\": {}, \"list\": []\n}"
	w := do(t, g.h, call{method: "POST", path: "/v1/chat/completions", key: workloadKey, body: body})
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	// The standard service tier is added after the last member, before the closing
	// whitespace and brace.
	want := strings.TrimSuffix(strings.Replace(body, `"renamed"`, `"vendor/renamed-7b-instruct"`, 1), "\n}") +
		`,"service_tier":"default"` + "\n}"
	if got := string(onlyRequest(t, g.backend).Body); got != want {
		t.Errorf("backend body\n%s\nwant\n%s", got, want)
	}
}

func TestStreamWithoutUsageRequestHidesTheUsageChunk(t *testing.T) {
	g := newTestGateway(t)
	body := `{"model":"open","stream":true,"messages":[]}`
	w := do(t, g.h, call{method: "POST", path: "/v1/chat/completions", key: userKey, body: body})
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("status %d, content type %q", w.Code, w.Header().Get("Content-Type"))
	}

	sent := onlyRequest(t, g.backend).Body
	if want := `{"model":"open","stream":true,"messages":[],"service_tier":"default","stream_options":{"include_usage":true}}`; string(sent) != want {
		t.Errorf("backend body %s, want %s", sent, want)
	}

	// What the backend streamed, minus the usage chunk, is exactly what the client got.
	var want strings.Builder
	usageChunks := 0
	for _, event := range sseEvents(directStream(t, g.backend, sent)) {
		if strings.Contains(event, `"choices":[]`) {
			usageChunks++
			continue
		}
		want.WriteString(event)
	}
	if usageChunks != 1 {
		t.Fatalf("backend sent %d usage chunks, want 1", usageChunks)
	}
	if w.Body.String() != want.String() {
		t.Errorf("client stream\n%s\nwant\n%s", w.Body.String(), want.String())
	}
	if !strings.HasSuffix(w.Body.String(), "data: [DONE]\n\n") {
		t.Error("stream does not end with [DONE]")
	}
}

func TestStreamWithUsageRequestKeepsTheUsageChunk(t *testing.T) {
	g := newTestGateway(t)
	body := `{"model":"open","stream":true,"stream_options":{"include_usage":true},"messages":[]}`
	w := do(t, g.h, call{method: "POST", path: "/v1/chat/completions", key: userKey, body: body})
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	sent := onlyRequest(t, g.backend).Body
	if want := strings.TrimSuffix(body, "}") + `,"service_tier":"default"}`; string(sent) != want {
		t.Errorf("backend body %s, want the client's with the standard service tier added: %s", sent, want)
	}
	if want := directStream(t, g.backend, sent); w.Body.String() != want {
		t.Errorf("client stream\n%s\nwant\n%s", w.Body.String(), want)
	}
	if !strings.Contains(w.Body.String(), `"choices":[]`) {
		t.Error("usage chunk missing")
	}
}

func TestStreamOptionsEditKeepsOtherOptions(t *testing.T) {
	g := newTestGateway(t)
	body := `{"model":"open","stream":true,"stream_options":{"include_usage":false,"continuous_usage_stats":true}}`
	do(t, g.h, call{method: "POST", path: "/v1/completions", key: userKey, body: body})
	want := `{"model":"open","stream":true,"stream_options":{"include_usage":true,"continuous_usage_stats":true}}`
	if got := string(onlyRequest(t, g.backend).Body); got != want {
		t.Errorf("backend body %s, want %s", got, want)
	}
}

func TestAzureUsesTheV1PathAndAPIKeyHeader(t *testing.T) {
	g := newTestGateway(t)
	for _, c := range []struct{ path, backendPath string }{
		{"/v1/chat/completions", "/openai/v1/chat/completions"},
		{"/v1/embeddings", "/openai/v1/embeddings"},
	} {
		w := do(t, g.h, call{method: "POST", path: c.path, key: workloadKey, body: `{"model":"on-azure","input":"x","messages":[]}`})
		if w.Code != http.StatusOK {
			t.Fatalf("%s: status %d: %s", c.path, w.Code, w.Body.String())
		}
	}
	reqs := g.backend.Requests()
	for i, want := range []string{"/openai/v1/chat/completions", "/openai/v1/embeddings"} {
		r := reqs[i]
		if r.Path != want {
			t.Errorf("azure path %q, want %q", r.Path, want)
		}
		if r.Header.Get("Api-Key") != azureBackendKey {
			t.Errorf("api-key %q", r.Header.Get("Api-Key"))
		}
		if v := r.Header.Get("Authorization"); v != "" {
			t.Errorf("azure request carries Authorization %q", v)
		}
		if !strings.Contains(string(r.Body), `"model":"gpt-4o-deploy"`) {
			t.Errorf("azure body %s", r.Body)
		}
	}
}

func TestBackendWithoutCredentialGetsNoAuthorization(t *testing.T) {
	g := newTestGateway(t)
	do(t, g.h, call{method: "POST", path: "/v1/embeddings", key: workloadKey, body: `{"model":"slow","input":"x"}`})
	if v := onlyRequest(t, g.backend).Header.Get("Authorization"); v != "" {
		t.Errorf("Authorization %q sent to a backend with no api_key_env; the client's key must never be forwarded", v)
	}
}

func TestUpstreamFailuresBeforeTheFirstByte(t *testing.T) {
	g := newTestGateway(t)

	w := do(t, g.h, call{method: "POST", path: "/v1/chat/completions", key: workloadKey, body: `{"model":"down"}`})
	expectError(t, w, http.StatusBadGateway, "upstream_unavailable")

	// The only deployment times out: retries are failover only, so its one attempt
	// answers.
	g.backend.SetReply(fakebackend.Reply{StallBeforeFirstByte: true})
	start := time.Now()
	w = do(t, g.h, call{method: "POST", path: "/v1/chat/completions", key: workloadKey, body: `{"model":"slow","stream":true}`})
	expectError(t, w, http.StatusGatewayTimeout, "upstream_timeout")
	if elapsed := time.Since(start); elapsed < 150*time.Millisecond || elapsed > waitTimeout {
		t.Errorf("timed out after %s, want about the 150 ms first-event timeout", elapsed)
	}
	reqs := g.backend.Requests()
	if len(reqs) != 1 {
		t.Fatalf("backend received %d requests, want 1 attempt", len(reqs))
	}
	for _, r := range reqs {
		select {
		case <-r.Canceled():
		case <-time.After(waitTimeout):
			t.Error("backend never saw a timed-out attempt cancelled")
		}
	}
	if body := w.Body.String(); strings.Contains(body, "127.0.0.1") || strings.Contains(body, g.backend.URL()) {
		t.Errorf("error names the backend: %s", body)
	}

	logs := g.logs.String()
	for _, want := range []string{`"error_code":"upstream_unavailable"`, `"error_code":"upstream_timeout"`, `"upstream_error":`, `"backend":"down"`} {
		if !strings.Contains(logs, want) {
			t.Errorf("log misses %s:\n%s", want, logs)
		}
	}
}

func TestBackendErrorsAreRelayedExceptCredentialRefusals(t *testing.T) {
	g := newTestGateway(t)
	errBody := `{"error":{"message":"too long","type":"invalid_request_error","param":"messages","code":"context_length_exceeded"}}`
	g.backend.SetReply(fakebackend.Reply{Status: 400, Body: errBody})
	w := do(t, g.h, call{method: "POST", path: "/v1/chat/completions", key: userKey, body: `{"model":"open","stream":true}`})
	if w.Code != 400 || w.Body.String() != errBody {
		t.Errorf("400 relayed as %d %s", w.Code, w.Body.String())
	}

	g.backend.SetReply(fakebackend.Reply{Status: 429, Header: map[string]string{"Retry-After": "7"}})
	w = do(t, g.h, call{method: "POST", path: "/v1/chat/completions", key: userKey, body: `{"model":"open"}`})
	if w.Code != 429 || w.Header().Get("Retry-After") != "7" {
		t.Errorf("429 relayed as %d, Retry-After %q", w.Code, w.Header().Get("Retry-After"))
	}

	g.backend.SetReply(fakebackend.Reply{Status: 503})
	w = do(t, g.h, call{method: "POST", path: "/v1/embeddings", key: userKey, body: `{"model":"open"}`})
	if w.Code != 503 {
		t.Errorf("503 relayed as %d", w.Code)
	}

	for _, status := range []int{401, 403} {
		g.backend.SetReply(fakebackend.Reply{Status: status})
		w = do(t, g.h, call{method: "POST", path: "/v1/chat/completions", key: userKey, body: `{"model":"open"}`})
		expectError(t, w, http.StatusBadGateway, "upstream_auth_failed")
	}
}

// serveGateway runs the gateway on a real listener: streaming, flushing and
// disconnects need a real connection.
func serveGateway(t *testing.T, g *testGateway) string {
	t.Helper()
	srv := httptest.NewServer(g.h)
	t.Cleanup(srv.Close)
	return srv.URL
}

func streamRequest(t *testing.T, ctx context.Context, url, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, "POST", url+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+userKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// readEvent reads one LF-framed event.
func readEvent(t *testing.T, r *bufio.Reader) string {
	t.Helper()
	event, err := nextEvent(r)
	if err != nil {
		t.Fatalf("reading event: %v (got %q)", err, event)
	}
	return event
}

func nextEvent(r *bufio.Reader) (string, error) {
	var event strings.Builder
	for {
		line, err := r.ReadString('\n')
		event.WriteString(line)
		if err != nil || line == "\n" {
			return event.String(), err
		}
	}
}

func TestStreamEventsAreFlushedAsTheyArrive(t *testing.T) {
	g := newTestGateway(t)
	pace := make(chan struct{})
	g.backend.SetReply(fakebackend.Reply{Pace: pace})
	url := serveGateway(t, g)

	resp := streamRequest(t, context.Background(), url,
		`{"model":"open","stream":true,"stream_options":{"include_usage":true}}`)
	defer resp.Body.Close()
	r := bufio.NewReader(resp.Body)
	// Each event must reach the client while the backend holds the next one back.
	events := len(fakebackend.DefaultChunks) + 3 // text chunks, finish chunk, usage chunk, [DONE]
	for i := 0; i < events; i++ {
		type result struct {
			event string
			err   error
		}
		got := make(chan result, 1)
		go func() {
			event, err := nextEvent(r)
			got <- result{event, err}
		}()
		select {
		case res := <-got:
			if res.err != nil {
				t.Fatalf("event %d: %v", i, res.err)
			}
			if i == 0 && !strings.Contains(res.event, `"Hello"`) {
				t.Errorf("first event %q", res.event)
			}
		case <-time.After(waitTimeout):
			t.Fatalf("event %d not delivered before the backend sent the next one", i)
		}
		if i < events-1 {
			pace <- struct{}{}
		}
	}
}

func TestClientDisconnectCancelsTheUpstreamRequest(t *testing.T) {
	for _, c := range []struct {
		name  string
		reply fakebackend.Reply
	}{
		{"mid-stream", fakebackend.Reply{HangAfter: 2}},
		{"before the first byte", fakebackend.Reply{StallBeforeFirstByte: true}},
	} {
		t.Run(c.name, func(t *testing.T) {
			g := newTestGateway(t)
			g.backend.SetReply(c.reply)
			url := serveGateway(t, g)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var resp *http.Response
			if c.reply.StallBeforeFirstByte {
				go func() {
					select {
					case <-g.backend.Arrivals():
						cancel()
					case <-time.After(waitTimeout):
					}
				}()
				req, _ := http.NewRequestWithContext(ctx, "POST", url+"/v1/chat/completions",
					strings.NewReader(`{"model":"open","stream":true}`))
				req.Header.Set("Authorization", "Bearer "+userKey)
				if _, err := http.DefaultClient.Do(req); err == nil {
					t.Fatal("request succeeded, want it cancelled")
				}
			} else {
				resp = streamRequest(t, ctx, url, `{"model":"open","stream":true}`)
				r := bufio.NewReader(resp.Body)
				readEvent(t, r)
				readEvent(t, r)
				cancel()
				resp.Body.Close()
			}

			select {
			case <-onlyRequest(t, g.backend).Canceled():
			case <-time.After(waitTimeout):
				t.Fatal("backend request not cancelled after the client left")
			}
		})
	}
}

func TestBackendCutMidStreamCutsTheClient(t *testing.T) {
	g := newTestGateway(t)
	g.backend.SetReply(fakebackend.Reply{CutAfter: 2})
	url := serveGateway(t, g)

	resp := streamRequest(t, context.Background(), url, `{"model":"open","stream":true}`)
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err == nil {
		t.Fatalf("stream ended cleanly after a backend cut: %q", data)
	}
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Logf("client read error: %v", err)
	}
	if n := strings.Count(string(data), "data: "); n != 2 {
		t.Errorf("client got %d events before the cut, want 2", n)
	}
	// The log line is written before the connection is cut.
	if logs := g.logText(); !strings.Contains(logs, `"relay_end":"upstream_failed"`) {
		t.Errorf("log misses relay_end:\n%s", logs)
	}
}

func TestEmbeddingsAndCompletionsPassThrough(t *testing.T) {
	g := newTestGateway(t)
	w := do(t, g.h, call{method: "POST", path: "/v1/embeddings", key: userKey, body: `{"model":"open","input":["a","b"],"encoding_format":"float"}`})
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"embedding":[0.25,-0.5,0.125]`) {
		t.Errorf("embeddings: %d %s", w.Code, w.Body.String())
	}
	w = do(t, g.h, call{method: "POST", path: "/v1/completions", key: userKey, body: `{"model":"open","prompt":"x","stream":true}`})
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"object":"text_completion"`) || strings.Contains(w.Body.String(), `"choices":[]`) {
		t.Errorf("completions stream: %d %s", w.Code, w.Body.String())
	}
	paths := []string{}
	for _, r := range g.backend.Requests() {
		paths = append(paths, r.Path)
	}
	if strings.Join(paths, " ") != "/v1/embeddings /v1/completions" {
		t.Errorf("backend paths %v", paths)
	}
}

func TestLogNeverCarriesBackendCredentials(t *testing.T) {
	g := newTestGateway(t)
	do(t, g.h, call{method: "POST", path: "/v1/chat/completions", key: userKey, body: `{"model":"open"}`})
	do(t, g.h, call{method: "POST", path: "/v1/chat/completions", key: workloadKey, body: `{"model":"on-azure"}`})
	do(t, g.h, call{method: "POST", path: "/v1/chat/completions", key: workloadKey, body: `{"model":"down"}`})
	for _, secret := range []string{localBackendKey, azureBackendKey, userKey, workloadKey, "Hello"} {
		if strings.Contains(g.logs.String(), secret) {
			t.Errorf("log contains %q", secret)
		}
	}
}
