package server

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"kaiak/internal/fakebackend"
)

// paddedChat is a chat body for model of exactly size bytes.
func paddedChat(t *testing.T, model string, stream bool, size int) string {
	t.Helper()
	head := `{"model":"` + model + `","stream":` + map[bool]string{true: "true", false: "false"}[stream] +
		`,"messages":[{"role":"user","content":"`
	tail := `"}]}`
	pad := size - len(head) - len(tail)
	if pad < 0 {
		t.Fatalf("a %d-byte body cannot hold its fields", size)
	}
	return head + strings.Repeat("x", pad) + tail
}

// waitIdle waits until the gateway has no request in flight: every finisher has run.
func waitIdle(t *testing.T, g *testGateway) {
	t.Helper()
	idle, _ := g.drain.idleNow()
	select {
	case <-idle:
	case <-time.After(waitTimeout):
		t.Fatal("requests still in flight")
	}
}

// M2: request bodies share one gateway-wide budget. A request whose body does not fit
// in what the requests in flight leave is refused at once — 503 server_busy with
// Retry-After: 1, its body never read — and the budget comes back when they end.
func TestBodyBudgetRefusesARequestWhenSpent(t *testing.T) {
	g := buildTestGateway(t, testOptions{bodyMemory: 1000})
	g.backend.SetReply(fakebackend.Reply{Before: fakebackend.StallFirstByte})
	url := serveGateway(t, g)
	body := paddedChat(t, "open", true, 600)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		req, _ := http.NewRequestWithContext(ctx, "POST", url+"/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+workloadKey)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
		}
	}()
	select {
	case <-g.backend.Arrivals():
	case <-time.After(waitTimeout):
		t.Fatal("the first request never reached the backend")
	}
	if got := g.bodies.InUse(); got != 600 {
		t.Fatalf("budget in use %d while the first request waits upstream, want its 600 bytes", got)
	}

	w := do(t, g.h, call{method: "POST", path: "/v1/chat/completions", key: workloadKey, body: body})
	expectError(t, w, http.StatusServiceUnavailable, "server_busy")
	if got := w.Header().Get("Retry-After"); got != "1" {
		t.Errorf("Retry-After %q, want 1", got)
	}
	if n := len(g.backend.Requests()); n != 1 {
		t.Errorf("backend got %d requests, want only the first", n)
	}
	// Platform-side, and its own remedy (memory, not backend capacity): its own class.
	expectMetricLines(t, g.metricsText(), `kaiak_errors_total{class="server_busy"} 1`)

	cancel()
	waitIdle(t, g)
	if got := g.bodies.InUse(); got != 0 {
		t.Fatalf("budget in use %d once every request ended, want 0", got)
	}
	g.backend.SetReply(fakebackend.Reply{})
	w = do(t, g.h, call{method: "POST", path: "/v1/chat/completions", key: workloadKey, body: body})
	if w.Code != http.StatusOK {
		t.Fatalf("status %d once the budget is back, want 200: %s", w.Code, w.Body.String())
	}
	if got := g.bodies.InUse(); got != 0 {
		t.Errorf("budget in use %d after the request, want 0", got)
	}
}

// M2: a body is dropped, and its share of the budget given back, as soon as the
// response starts relaying — no retry can use it any more — not when a long stream
// ends.
func TestBodyIsReleasedOnceTheResponseRelays(t *testing.T) {
	g := buildTestGateway(t, testOptions{bodyMemory: 1000})
	pace := make(chan struct{})
	g.backend.SetReply(fakebackend.Reply{Pace: pace})
	url := serveGateway(t, g)

	resp := workloadStream(t, url, paddedChat(t, "open", true, 600))
	defer resp.Body.Close()
	events := bufio.NewReader(resp.Body)
	for {
		line, err := events.ReadString('\n')
		if err != nil {
			t.Fatalf("stream ended before its first event: %v", err)
		}
		if line == "\n" {
			break
		}
	}
	if got := g.bodies.InUse(); got != 0 {
		t.Errorf("budget in use %d while the stream relays, want 0: the body is still held", got)
	}
	close(pace)
	if _, err := io.Copy(io.Discard, events); err != nil {
		t.Fatal(err)
	}
}

// M2: a body larger than the whole budget could never be held: it is refused as too
// large, with the budget as the limit, even under a larger max_request_body_bytes.
func TestBodyOverTheWholeBudgetIsTooLarge(t *testing.T) {
	g := buildTestGateway(t, testOptions{bodyMemory: 500})
	w := do(t, g.h, call{method: "POST", path: "/v1/chat/completions", key: workloadKey,
		body: paddedChat(t, "open", false, 800)})
	expectError(t, w, http.StatusRequestEntityTooLarge, "request_too_large")
	if !strings.Contains(w.Body.String(), "limit of 500 bytes") {
		t.Errorf("message does not name the budget as the limit: %s", w.Body.String())
	}
	if got := g.bodies.InUse(); got != 0 {
		t.Errorf("budget in use %d, want 0", got)
	}
}

// M2: a body of unknown length (chunked) takes the budget step by step as it arrives:
// it is read while the budget has room, refused 503 once a step finds it spent, and
// refused as too large past the limit. Every share comes back.
func TestBodyOfUnknownLengthTakesTheBudgetAsItArrives(t *testing.T) {
	g := buildTestGateway(t, testOptions{bodyMemory: 1000})
	send := func(body string) *httptest.ResponseRecorder {
		// A reader of unknown length: the request has no Content-Length.
		r := httptest.NewRequest("POST", "/v1/chat/completions", io.MultiReader(strings.NewReader(body)))
		if r.ContentLength != -1 {
			t.Fatalf("ContentLength %d, want unknown", r.ContentLength)
		}
		r.Header.Set("Authorization", "Bearer "+workloadKey)
		w := httptest.NewRecorder()
		g.h.ServeHTTP(w, r)
		return w
	}

	if w := send(paddedChat(t, "open", false, 600)); w.Code != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", w.Code, w.Body.String())
	}
	if got := g.bodies.InUse(); got != 0 {
		t.Fatalf("budget in use %d after the request, want 0", got)
	}

	// Other requests hold 500 bytes: the first step (the whole 1000-byte limit here)
	// does not fit.
	if !g.bodies.take(500) {
		t.Fatal("take")
	}
	w := send(paddedChat(t, "open", false, 600))
	expectError(t, w, http.StatusServiceUnavailable, "server_busy")
	if got := g.bodies.InUse(); got != 500 {
		t.Errorf("budget in use %d after the refusal, want the others' 500", got)
	}
	g.bodies.give(500)

	w = send(paddedChat(t, "open", false, 1100))
	expectError(t, w, http.StatusRequestEntityTooLarge, "request_too_large")
	if got := g.bodies.InUse(); got != 0 {
		t.Errorf("budget in use %d after the refusal, want 0", got)
	}
}

// N-S1, scaled down: connections of one valid key declaring large bodies and sending
// nothing. Taking the budget for the whole declared length before a byte arrived, two
// would fill it here, and every other client would get 503 server_busy until the
// body-read deadline. The key's concurrency limit refuses them past 16, and those
// admitted hold at most one growth step of the budget each: another key's request is
// served.
func TestIdleDeclaredBodiesDoNotStarveOtherKeys(t *testing.T) {
	const declared, attackers, perKey = 1 << 20, 20, 16
	g := buildTestGateway(t, testOptions{bodyMemory: 2 * declared})
	g.holder.Swap(testSnapshotWith(t, g.backend.URL(), func(doc string) string {
		return replaceOnce(t, doc, `"max_request_body_bytes": 1024 }`, `"max_request_body_bytes": `+strconv.Itoa(declared)+` }`)
	}))
	addr := listen(t, g, ClientTimeouts{Idle: time.Minute, BodyRead: time.Minute, Write: time.Minute})

	var mu sync.Mutex
	var answers []string
	for range attackers {
		conn := dial(t, addr)
		if _, err := io.WriteString(conn, "POST /v1/chat/completions HTTP/1.1\r\nHost: kaiak\r\nAuthorization: Bearer "+workloadKey+
			"\r\nContent-Type: application/json\r\nContent-Length: "+strconv.Itoa(declared)+"\r\n\r\n"); err != nil {
			t.Fatal(err)
		}
		go func() {
			// A refusal closes the connection; a held request's read ends when the
			// test closes it.
			data, _ := io.ReadAll(conn)
			if len(data) > 0 {
				mu.Lock()
				answers = append(answers, string(data))
				mu.Unlock()
			}
		}()
	}
	settled := func() (int64, int) {
		mu.Lock()
		defer mu.Unlock()
		return keysInFlight(g, "k-eval"), len(answers)
	}
	deadline := time.Now().Add(waitTimeout)
	for held, answered := settled(); held+int64(answered) != attackers; held, answered = settled() {
		if time.Now().After(deadline) {
			t.Fatalf("%d held and %d answered, want all %d accounted for", held, answered, attackers)
		}
		time.Sleep(time.Millisecond)
	}

	req, _ := http.NewRequest("POST", "http://"+addr+"/v1/chat/completions", strings.NewReader(chatBody))
	req.Header.Set("Authorization", "Bearer "+userKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	victim, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("another key's request: status %d, want 200: %s", resp.StatusCode, victim)
	}
	if got := g.bodies.InUse(); got > perKey*bodyStep {
		t.Errorf("idle declared bodies hold %d bytes of the budget, want at most %d (one step each)", got, perKey*bodyStep)
	}
	held, _ := settled()
	if held != perKey {
		t.Errorf("%d attacker requests held, want the per-key limit %d", held, perKey)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, a := range answers {
		if !strings.HasPrefix(a, "HTTP/1.1 429") || !strings.Contains(a, "concurrency_limit_exceeded") {
			t.Errorf("attacker answered %q, want 429 concurrency_limit_exceeded", a)
		}
	}
}
