package server

import (
	"bufio"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"kaiak/internal/fakebackend"
)

// shortTimeouts are client timeouts small enough for tests to run out.
var shortTimeouts = ClientTimeouts{Idle: 300 * time.Millisecond, BodyRead: 300 * time.Millisecond, Write: 300 * time.Millisecond}

// listen serves g's API on a real listener with timeouts and returns its address.
func listen(t *testing.T, g *testGateway, timeouts ClientTimeouts) string {
	t.Helper()
	l, err := Listen("api", "127.0.0.1:0", g.h, timeouts, slog.New(slog.NewJSONHandler(g.log, nil)))
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = l.Serve() }()
	t.Cleanup(l.cut)
	return l.Addr().String()
}

// dial opens a raw connection to addr, closed when the test ends.
func dial(t *testing.T, addr string) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

// readAnswer reads what the gateway sends on conn until it closes the connection or
// limit passes; closed reports whether it closed.
func readAnswer(t *testing.T, conn net.Conn, limit time.Duration) (answer string, closed bool) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(limit))
	data, err := io.ReadAll(conn)
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		return string(data), false
	}
	return string(data), true
}

// Complete headers declaring a one-byte body that never
// comes, and no key. Refused before the body is read, the answer still waits for
// net/http to dispose of the body — the body-read deadline bounds that wait.
func TestUnauthenticatedStalledBodyIsBounded(t *testing.T) {
	g := newTestGateway(t)
	conn := dial(t, listen(t, g, shortTimeouts))
	start := time.Now()
	if _, err := io.WriteString(conn, "POST /v1/chat/completions HTTP/1.1\r\nHost: kaiak\r\nContent-Type: application/json\r\nContent-Length: 1\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	answer, closed := readAnswer(t, conn, 3*time.Second)
	if !closed {
		t.Fatalf("connection still open %s after the request, answer so far %q", time.Since(start), answer)
	}
	if !strings.HasPrefix(answer, "HTTP/1.1 401") || !strings.Contains(answer, "missing_api_key") {
		t.Errorf("answer %q, want the 401", answer)
	}
}

// An authenticated client that sends part of its body and stalls is answered once the
// body-read deadline passes, before any backend work.
func TestSlowBodyIsBounded(t *testing.T) {
	g := newTestGateway(t)
	conn := dial(t, listen(t, g, shortTimeouts))
	if _, err := io.WriteString(conn, "POST /v1/chat/completions HTTP/1.1\r\nHost: kaiak\r\nAuthorization: Bearer "+userKey+
		"\r\nContent-Type: application/json\r\nContent-Length: 100\r\n\r\n{\"model\":"); err != nil {
		t.Fatal(err)
	}
	answer, closed := readAnswer(t, conn, 3*time.Second)
	if !closed {
		t.Fatalf("connection still open, answer so far %q", answer)
	}
	if !strings.HasPrefix(answer, "HTTP/1.1 400") || !strings.Contains(answer, "invalid_body") {
		t.Errorf("answer %q, want 400 invalid_body", answer)
	}
	if n := len(g.backend.Requests()); n != 0 {
		t.Errorf("backend got %d requests", n)
	}
}

// The body-read deadline is cleared once the body is in: a stream longer than it runs
// to its end.
func TestStreamOutlivesTheBodyReadDeadline(t *testing.T) {
	g := newTestGateway(t)
	g.backend.SetReply(fakebackend.Reply{EventDelay: 100 * time.Millisecond})
	addr := listen(t, g, shortTimeouts)
	req, _ := http.NewRequest("POST", "http://"+addr+"/v1/chat/completions", strings.NewReader(`{"model":"open","stream":true}`))
	req.Header.Set("Authorization", "Bearer "+userKey)
	start := time.Now()
	resp, err := newClient(t).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("stream broke after %s: %v (%q)", time.Since(start), err, data)
	}
	if time.Since(start) < shortTimeouts.BodyRead {
		t.Fatalf("stream took %s, want longer than the body-read deadline", time.Since(start))
	}
	if !strings.HasSuffix(string(data), "data: [DONE]\n\n") {
		t.Errorf("stream %q, want it whole", data)
	}
}

// A keep-alive connection left idle is closed after the idle timeout.
func TestIdleConnectionIsClosed(t *testing.T) {
	g := newTestGateway(t)
	conn := dial(t, listen(t, g, shortTimeouts))
	if _, err := io.WriteString(conn, "GET /v1/models HTTP/1.1\r\nHost: kaiak\r\nAuthorization: Bearer "+userKey+"\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	rd := bufio.NewReader(conn)
	resp, err := http.ReadResponse(rd, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Close {
		t.Fatalf("status %d, close %v; want a kept-alive 200", resp.StatusCode, resp.Close)
	}
	start := time.Now()
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := rd.ReadByte(); err != io.EOF {
		t.Fatalf("idle connection: %v after %s, want it closed", err, time.Since(start))
	}
}

// A client that stops reading a stream is taken as gone once a write makes no
// progress for the write timeout: the relay ends client_closed and the slot is freed.
func TestClientThatStopsReadingIsCutOff(t *testing.T) {
	g := newTestGateway(t)
	// Far more than the socket buffers between the gateway and the client hold.
	chunks := make([]string, 128)
	for i := range chunks {
		chunks[i] = strings.Repeat("x", 256<<10)
	}
	g.backend.SetReply(fakebackend.Reply{Chunks: chunks})
	conn := dial(t, listen(t, g, shortTimeouts))
	body := `{"model":"open","stream":true}`
	if _, err := io.WriteString(conn, "POST /v1/chat/completions HTTP/1.1\r\nHost: kaiak\r\nAuthorization: Bearer "+userKey+
		"\r\nContent-Type: application/json\r\nContent-Length: "+strconv.Itoa(len(body))+"\r\n\r\n"+body); err != nil {
		t.Fatal(err)
	}
	// The client reads nothing.
	deadline := time.Now().Add(waitTimeout)
	for !strings.Contains(g.logText(), `"kaiak.relay_end":"client_closed"`) {
		if time.Now().After(deadline) {
			t.Fatalf("relay still running with a client that stopped reading:\n%s", g.logText())
		}
		time.Sleep(20 * time.Millisecond)
	}
	// The log line is written after the slot was freed.
	if n := inFlight(g); len(n) != 0 {
		t.Errorf("in flight %v after the relay ended, want none", n)
	}
}

// Both listeners (Listen serves API and admin) refuse request headers above
// maxHeaderBytes with 431, instead of net/http's default 1 MiB per connection; a
// request with ordinary headers — tens of KiB still — passes.
func TestOversizeHeadersAreRefused(t *testing.T) {
	g := newTestGateway(t)
	addr := listen(t, g, DefaultClientTimeouts)
	send := func(padding int) *http.Response {
		t.Helper()
		req, _ := http.NewRequest("GET", "http://"+addr+"/v1/models", nil)
		req.Header.Set("Authorization", "Bearer "+userKey)
		req.Header.Set("X-Padding", strings.Repeat("x", padding))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp
	}
	if resp := send(80 << 10); resp.StatusCode != http.StatusRequestHeaderFieldsTooLarge {
		t.Errorf("80 KiB of headers: status %d, want 431", resp.StatusCode)
	}
	if resp := send(32 << 10); resp.StatusCode != http.StatusOK {
		t.Errorf("32 KiB of headers: status %d, want 200", resp.StatusCode)
	}
}

// KAIAK_MAX_CONNECTIONS: with the cap's connections open (idle keep-alive ones
// included) a further connection is closed at accept, before any byte is read, and
// counted; once one closes, a new connection is served again.
func TestConnectionsBeyondTheCapAreClosedAtAccept(t *testing.T) {
	g := newTestGateway(t)
	l, err := Listen("api", "127.0.0.1:0", g.h, DefaultClientTimeouts, slog.New(slog.NewJSONHandler(g.log, nil)))
	if err != nil {
		t.Fatal(err)
	}
	var refused atomic.Int64
	l.LimitConnections(2, func() { refused.Add(1) })
	go func() { _ = l.Serve() }()
	t.Cleanup(l.cut)
	addr := l.Addr().String()

	// served sends one request on conn and reports whether it was answered 200.
	served := func(conn net.Conn) bool {
		t.Helper()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		if _, err := io.WriteString(conn, "GET /v1/models HTTP/1.1\r\nHost: kaiak\r\nAuthorization: Bearer "+userKey+"\r\n\r\n"); err != nil {
			return false
		}
		resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
		if err != nil {
			return false
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}
	first, second := dial(t, addr), dial(t, addr)
	if !served(first) || !served(second) {
		t.Fatal("connections within the cap not served")
	}
	third := dial(t, addr)
	if answer, closed := readAnswer(t, third, 5*time.Second); !closed || answer != "" {
		t.Fatalf("connection beyond the cap: closed %v, answer %q; want closed with nothing sent", closed, answer)
	}
	if n := refused.Load(); n != 1 {
		t.Errorf("%d refusals counted, want 1", n)
	}

	// The server sees first's close on its own time: a new connection is served once
	// it has.
	first.Close()
	deadline := time.Now().Add(5 * time.Second)
	for !served(dial(t, addr)) {
		if time.Now().After(deadline) {
			t.Fatal("no connection served after one within the cap closed")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !served(second) {
		t.Error("the connection kept open was not served")
	}
}

func TestNoCapLeavesTheListenerUncapped(t *testing.T) {
	g := newTestGateway(t)
	l, err := Listen("api", "127.0.0.1:0", g.h, DefaultClientTimeouts, slog.New(slog.NewJSONHandler(g.log, nil)))
	if err != nil {
		t.Fatal(err)
	}
	l.LimitConnections(0, func() { t.Error("a connection was refused with no cap") })
	if _, capped := l.ln.(*cappedListener); capped {
		t.Error("cap 0 wrapped the listener")
	}
	_ = l.Close()
}
