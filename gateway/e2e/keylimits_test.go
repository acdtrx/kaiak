package e2e

// The request guards the follow-up audit added, through the built binary
// (docs/specs/GATEWAY.md, Limits): the per-key concurrency limit, an output limit
// above the model's context, and the short retry of a token refusal blocked only by
// running requests.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"testing"

	"kaiak/internal/fakebackend"
)

// heldStream is a streaming chat request whose answer the backend holds open until
// the stream is cancelled.
type heldStream struct {
	cancel context.CancelFunc
	done   chan struct{} // closed once the request ended, however it ended
}

// holdStream sends a streaming chat request with key and request ID off the test
// goroutine, reading its answer until cancel.
func holdStream(t *testing.T, g *gateway, key, requestID string) *heldStream {
	t.Helper()
	body, err := json.Marshal(chatBody("chat", true, nil))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.api+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("X-Request-Id", requestID)
	s := &heldStream{cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(s.done)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}()
	t.Cleanup(func() {
		cancel()
		<-s.done
	})
	return s
}

// E10: a key holds at most max_concurrent_requests_per_key (default 16) requests on
// the gateway; the next is refused at once with 429 concurrency_limit_exceeded and
// Retry-After 1, another key is still served, and a finished request gives its slot
// back.
func TestPerKeyConcurrencyLimit(t *testing.T) {
	backend := fakebackend.New()
	defer backend.Close()
	dir := t.TempDir()
	configFile := filepath.Join(dir, "config.json")
	evalKey, evalHash := newKey()
	annKey, annHash := newKey()
	writeJSON(t, configFile, testConfig(backend.URL(), evalHash, annHash, ""))
	g := startGateway(t, configFile, "")

	const limit = 16
	held := make([]*heldStream, limit)
	for i := range held {
		backend.QueueReplies(fakebackend.Reply{HangAfter: 1})
		held[i] = holdStream(t, g, evalKey, fmt.Sprintf("e2e-held-%d", i))
		<-backend.Arrivals()
	}

	r := g.post(t, "/v1/chat/completions", evalKey, "e2e-held-over", chatBody("chat", false, nil))
	if r.StatusCode != http.StatusTooManyRequests || r.errorCode(t) != "concurrency_limit_exceeded" {
		t.Fatalf("request %d from one key: %d %s", limit+1, r.StatusCode, r.body)
	}
	if got := r.Header.Get("Retry-After"); got != "1" {
		t.Errorf("Retry-After %q, want 1", got)
	}
	if n := len(backend.Requests()); n != limit {
		t.Errorf("backend received %d requests, want the %d held", n, limit)
	}
	if r := g.post(t, "/v1/chat/completions", annKey, "", chatBody("chat", false, nil)); r.StatusCode != http.StatusOK {
		t.Fatalf("another key while the first is at its limit: %d %s", r.StatusCode, r.body)
	}

	held[0].cancel()
	<-held[0].done
	g.settled(t, "e2e-held-0")
	if r := g.post(t, "/v1/chat/completions", evalKey, "", chatBody("chat", false, nil)); r.StatusCode != http.StatusOK {
		t.Fatalf("the key after one of its requests ended: %d %s", r.StatusCode, r.body)
	}
	for _, s := range held[1:] {
		s.cancel()
		<-s.done
	}
	g.stop(t)
}

// E9: max_tokens or max_completion_tokens above the model's context_length is
// refused 400 invalid_value naming the key, before anything reaches the backend; the
// context length itself is accepted (and lowered to the ceiling).
func TestOutputLimitAboveTheContextIsRefused(t *testing.T) {
	backend := fakebackend.New()
	defer backend.Close()
	dir := t.TempDir()
	configFile := filepath.Join(dir, "config.json")
	evalKey, evalHash := newKey()
	_, annHash := newKey()
	writeJSON(t, configFile, testConfig(backend.URL(), evalHash, annHash, ""))
	g := startGateway(t, configFile, "")

	for _, param := range []string{"max_tokens", "max_completion_tokens"} {
		r := g.post(t, "/v1/chat/completions", evalKey, "", chatBody("chat", false, map[string]any{param: 8193}))
		e, _ := r.json(t)["error"].(map[string]any)
		if r.StatusCode != http.StatusBadRequest || e["code"] != "invalid_value" || e["param"] != param {
			t.Errorf("%s above the context: %d %s", param, r.StatusCode, r.body)
		}
	}
	if n := len(backend.Requests()); n != 0 {
		t.Errorf("backend received %d requests for refused ones", n)
	}
	if r := g.post(t, "/v1/chat/completions", evalKey, "", chatBody("chat", false, map[string]any{"max_tokens": 8192})); r.StatusCode != http.StatusOK {
		t.Fatalf("max_tokens at the context: %d %s", r.StatusCode, r.body)
	}
	var sent map[string]any
	if err := json.Unmarshal(backend.Requests()[0].Body, &sent); err != nil {
		t.Fatal(err)
	}
	if sent["max_tokens"] != float64(128) {
		t.Errorf("max_tokens sent %v, want the ceiling 128", sent["max_tokens"])
	}
	g.stop(t)
}

// A token limit blocked only by a request still running (the review's M4) answers the
// short fixed retry — Retry-After 2, x-ratelimit-reset-tokens 2s — instead of the
// minute's slot expiry, and the same request is admitted once the running one has
// settled (docs/specs/GATEWAY.md, Limits → Refusal).
func TestTokenRefusalBlockedByARunningRequestRetriesSoon(t *testing.T) {
	backend := fakebackend.New()
	defer backend.Close()
	dir := t.TempDir()
	configFile := filepath.Join(dir, "config.json")
	evalKey, evalHash := newKey()
	_, annHash := newKey()
	cfg := testConfig(backend.URL(), evalHash, annHash, "")
	// A request reserves its input estimate (about 20 tokens) plus the default output
	// limit (64): one reservation fits 120 tokens, two do not; a settled answer (7 in,
	// 4 out) beside one reservation still fits.
	eval := cfg["groups"].(map[string]any)["eval"].(map[string]any)
	eval["limits"] = []any{map[string]any{"type": "tokens_per_minute", "value": 120}}
	writeJSON(t, configFile, cfg)
	g := startGateway(t, configFile, "")

	// The first request is held after its first event until pace closes.
	pace := make(chan struct{})
	backend.QueueReplies(fakebackend.Reply{Pace: pace})
	running := holdStream(t, g, evalKey, "m4-running")
	<-backend.Arrivals()

	r := g.post(t, "/v1/chat/completions", evalKey, "m4-refused", chatBody("chat", true, nil))
	if r.StatusCode != http.StatusTooManyRequests || r.errorCode(t) != "rate_limit_exceeded" {
		t.Fatalf("beside a running request: %d %s, want 429 rate_limit_exceeded", r.StatusCode, r.body)
	}
	for name, want := range map[string]string{"Retry-After": "2", "X-Ratelimit-Reset-Tokens": "2s"} {
		if got := r.Header.Get(name); got != want {
			t.Errorf("refusal %s %q, want %q: only a running request blocks", name, got, want)
		}
	}
	if line := g.settled(t, "m4-refused"); line["kaiak.limit.type"] != "tokens_per_minute" {
		t.Errorf("refusal's log line %v, want the tokens_per_minute limit", line)
	}

	close(pace)
	<-running.done
	g.settled(t, "m4-running")
	r = g.post(t, "/v1/chat/completions", evalKey, "m4-again", chatBody("chat", true, nil))
	if r.StatusCode != http.StatusOK {
		t.Fatalf("the same request once the running one settled: %d %s, want 200", r.StatusCode, r.body)
	}
	if got := r.Header.Get("X-Ratelimit-Reset-Tokens"); got == "2s" {
		t.Errorf("admitted response x-ratelimit-reset-tokens %q, want the window's reset", got)
	}
	if n := len(backend.Requests()); n != 2 {
		t.Errorf("backend received %d requests, want the running one and the admitted retry", n)
	}
	g.stop(t)
}
