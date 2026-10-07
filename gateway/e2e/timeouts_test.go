package e2e

// Timeouts on real sockets through the built binary: the client-side deadlines set
// from the environment (KAIAK_BODY_READ_TIMEOUT_MS, KAIAK_IDLE_TIMEOUT_MS,
// KAIAK_WRITE_TIMEOUT_MS) and a backend stream's stall timeout. The server tests
// prove each mechanism on a listener; these prove the binary wires them up.

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"kaiak/internal/fakebackend"
)

// clientTimeout is the client-side deadline these tests set: far below the defaults
// (60 s and 120 s, past waitLimit), so a test that passes proves the setting took.
const clientTimeout = 300 * time.Millisecond

// startTimeoutGateway runs a file-mode gateway on the e2e config with every client
// timeout at clientTimeout.
func startTimeoutGateway(t *testing.T, backendURL string) (*gateway, string) {
	t.Helper()
	dir := t.TempDir()
	evalKey, evalHash := newKey()
	_, annHash := newKey()
	configFile := filepath.Join(dir, "config.json")
	writeJSON(t, configFile, testConfig(backendURL, evalHash, annHash, ""))
	ms := strconv.Itoa(int(clientTimeout.Milliseconds()))
	env := append(gatewayEnv(configFile),
		"KAIAK_BODY_READ_TIMEOUT_MS="+ms, "KAIAK_IDLE_TIMEOUT_MS="+ms, "KAIAK_WRITE_TIMEOUT_MS="+ms)
	return startGatewayEnv(t, env), evalKey
}

// dialAPI opens a raw connection to the gateway's API listener, closed at cleanup.
func dialAPI(t *testing.T, g *gateway) net.Conn {
	t.Helper()
	u, err := url.Parse(g.api)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("tcp", u.Host)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

// readUntilClosed reads what the gateway sends until it closes the connection, up to
// waitLimit; it fails when the connection is still open then.
func readUntilClosed(t *testing.T, conn net.Conn) string {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(waitLimit))
	data, err := io.ReadAll(conn)
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatalf("connection still open after %s, answer so far %q", waitLimit, data)
	}
	return string(data)
}

func TestClientTimeoutsOnARealListener(t *testing.T) {
	backend := fakebackend.New()
	defer backend.Close()
	g, key := startTimeoutGateway(t, backend.URL())

	t.Run("a stalled body without a key is answered 401 and closed", func(t *testing.T) {
		// The audit's reproduction: headers declaring a one-byte body that never comes.
		conn := dialAPI(t, g)
		if _, err := io.WriteString(conn, "POST /v1/chat/completions HTTP/1.1\r\nHost: kaiak\r\n"+
			"Content-Type: application/json\r\nContent-Length: 1\r\n\r\n"); err != nil {
			t.Fatal(err)
		}
		answer := readUntilClosed(t, conn)
		if !strings.HasPrefix(answer, "HTTP/1.1 401") || !strings.Contains(answer, "missing_api_key") {
			t.Errorf("answer %q, want the 401", answer)
		}
	})

	t.Run("an authenticated body that stalls is answered 400 at the body-read deadline", func(t *testing.T) {
		conn := dialAPI(t, g)
		start := time.Now()
		if _, err := io.WriteString(conn, "POST /v1/chat/completions HTTP/1.1\r\nHost: kaiak\r\nAuthorization: Bearer "+key+
			"\r\nContent-Type: application/json\r\nContent-Length: 100\r\n\r\n{\"model\":"); err != nil {
			t.Fatal(err)
		}
		answer := readUntilClosed(t, conn)
		if elapsed := time.Since(start); elapsed < clientTimeout {
			t.Errorf("answered after %s, before the %s body-read deadline", elapsed, clientTimeout)
		}
		if !strings.HasPrefix(answer, "HTTP/1.1 400") || !strings.Contains(answer, "invalid_body") {
			t.Errorf("answer %q, want 400 invalid_body", answer)
		}
		if n := len(backend.Requests()); n != 0 {
			t.Errorf("backend got %d requests", n)
		}
	})

	t.Run("an idle keep-alive connection is closed at the idle timeout", func(t *testing.T) {
		conn := dialAPI(t, g)
		if _, err := io.WriteString(conn, "GET /v1/models HTTP/1.1\r\nHost: kaiak\r\nAuthorization: Bearer "+key+"\r\n\r\n"); err != nil {
			t.Fatal(err)
		}
		rd := bufio.NewReader(conn)
		_ = conn.SetReadDeadline(time.Now().Add(waitLimit))
		resp, err := http.ReadResponse(rd, nil)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || resp.Close {
			t.Fatalf("status %d, close %v; want a kept-alive 200", resp.StatusCode, resp.Close)
		}
		idle := time.Now()
		if _, err := rd.ReadByte(); err != io.EOF {
			t.Fatalf("idle connection: %v after %s, want it closed", err, time.Since(idle))
		}
		if elapsed := time.Since(idle); elapsed < clientTimeout/2 {
			t.Errorf("closed after %s idle, want about the %s idle timeout", elapsed, clientTimeout)
		}
	})

	t.Run("a client that stops reading a stream is cut off at the write timeout", func(t *testing.T) {
		// Far more than the socket buffers between the gateway and the client hold.
		chunks := make([]string, 128)
		for i := range chunks {
			chunks[i] = strings.Repeat("x", 256<<10)
		}
		backend.SetReply(fakebackend.Reply{Chunks: chunks})
		defer backend.SetReply(fakebackend.Reply{})
		conn := dialAPI(t, g)
		body := `{"model":"chat","stream":true,"messages":[{"role":"user","content":"Say hello."}]}`
		if _, err := io.WriteString(conn, "POST /v1/chat/completions HTTP/1.1\r\nHost: kaiak\r\nAuthorization: Bearer "+key+
			"\r\nX-Request-Id: never-read\r\nContent-Type: application/json\r\nContent-Length: "+strconv.Itoa(len(body))+"\r\n\r\n"+body); err != nil {
			t.Fatal(err)
		}
		// The client reads nothing: the relay ends as the client gone, its slot freed.
		if line := g.settled(t, "never-read"); line["kaiak.relay_end"] != "client_closed" {
			t.Errorf("log line %v, want kaiak.relay_end client_closed", line)
		}
		g.waitMetric(t, "the slot freed", `kaiak_backend_in_flight_requests{backend="fake"}`, func(v float64) bool { return v == 0 })
	})

	g.stop(t)
}

// A stream that goes silent after its first event ends at the stall timeout as the
// backend's failure: the client's stream stops without [DONE], and the failure opens
// the deployment's circuit (threshold 1 here); the other deployment serves next.
func TestStalledStreamEndsAndCountsTowardTheCircuit(t *testing.T) {
	const stall = 500 * time.Millisecond
	a, b := fakebackend.New(), fakebackend.New()
	defer a.Close()
	defer b.Close()
	g, key := startReliability(t, a.URL(), b.URL(), func(cfg map[string]any) {
		cfg["global"].(map[string]any)["circuit"] = map[string]any{"failure_threshold": 1, "probe_interval_ms": 60000}
		cfg["backends"].(map[string]any)["a"].(map[string]any)["stall_timeout_ms"] = int(stall.Milliseconds())
	})

	a.SetReply(fakebackend.Reply{Fault: &fakebackend.StreamFault{At: 1, Kind: fakebackend.Hang}})
	stream := openStream(t, g, key, "stalled", chatBody("chat", true, nil))
	firstEvent := time.Now()
	// Bounded: a stall timeout not taking effect leaves the stream open.
	bound := time.AfterFunc(waitLimit, func() { stream.Body.Close() })
	rest, err := io.ReadAll(stream.Body)
	elapsed := time.Since(firstEvent)
	if !bound.Stop() {
		t.Fatalf("stream still open %s after its first event", waitLimit)
	}
	_ = err // the gateway ends the response; how is not the point
	if strings.Contains(string(rest), "[DONE]") {
		t.Errorf("stalled stream ended with [DONE]: %q", rest)
	}
	// The client read its first event before the gateway started waiting for the
	// second, so the gap is at least the stall timeout less the event's way to it.
	if elapsed < stall-100*time.Millisecond {
		t.Errorf("stream ended %s after its first event, before the %s stall timeout", elapsed, stall)
	}
	line := g.settled(t, "stalled")
	if line["kaiak.relay_end"] != "upstream_stalled" || line["kaiak.backend.id"] != "a" || line["kaiak.attempts"] != 1.0 {
		t.Fatalf("log line %v, want one attempt on a ending upstream_stalled", line)
	}
	select {
	case <-a.Requests()[0].Canceled():
	case <-time.After(waitLimit):
		t.Error("the stalled request was not cancelled upstream")
	}
	g.logs.wait(t, "a's circuit opening", msg("circuit opened", "kaiak.backend.id", "a"))
	if got := g.metric(t, circuitSeries("a")); got != 1 {
		t.Errorf("a's circuit = %v, want open", got)
	}
	if got := sumMetric(t, g, "kaiak_upstream_attempts_total", `backend="a"`, `outcome="broke_off"`); got != 1 {
		t.Errorf("a's broken-off attempts = %v, want the stall", got)
	}
	// a's circuit is open (its probe far off): the next request goes to b.
	if line := chatOK(t, g, key, "after", "chat"); line["kaiak.backend.id"] != "b" || line["kaiak.attempts"] != 1.0 {
		t.Errorf("after the stall: log line %v, want one attempt on b", line)
	}
	g.stop(t)
}
