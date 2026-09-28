package server

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"kaiak/internal/fakebackend"
)

// D2: a non-stream request past its backend's response timeout answers 504, is not
// retried (the backend was working on a long answer) and bills the prompt it sent
// (D1). The "slow" backend's response timeout is 150 ms.
func TestResponseTimeoutIsNotRetried(t *testing.T) {
	g := newTestGateway(t)
	g.backend.SetReply(fakebackend.Reply{StallBeforeFirstByte: true})
	body := `{"model":"slow","messages":[{"role":"user","content":"write a long essay"}]}`
	w := do(t, g.h, call{method: "POST", path: "/v1/chat/completions", key: workloadKey, body: body})
	expectError(t, w, http.StatusGatewayTimeout, "upstream_timeout")
	if n := len(g.backend.Requests()); n != 1 {
		t.Errorf("backend got %d requests, want 1: a response timeout is not retried", n)
	}
	expectUnits(t, onlyRecord(t, g), units(int64(len(body)+3)/4, 0, 0, 0), true, true)
	logs := g.logText()
	for _, want := range []string{`"attempts":1`, `"error_code":"upstream_timeout"`, `no response within 150ms`} {
		if !strings.Contains(logs, want) {
			t.Errorf("log misses %s:\n%s", want, logs)
		}
	}
}

// H1: a stream silent past its backend's stall timeout after the first events ends
// as a backend failure: the client connection is cut, the upstream request
// cancelled, usage settled partial, no retry (part of the answer reached the
// client). The "slow" backend's stall timeout is 150 ms.
func TestStalledStreamEndsAsUpstreamFailure(t *testing.T) {
	g := newTestGateway(t)
	g.backend.SetReply(fakebackend.Reply{HangAfter: 2})
	url := serveGateway(t, g)
	body := `{"model":"slow","stream":true}`
	start := time.Now()
	resp := workloadStream(t, url, body)
	data, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err == nil {
		t.Fatalf("stream ended cleanly after the backend stalled: %q", data)
	}
	if time.Since(start) > waitTimeout {
		t.Fatalf("stall noticed after %s", time.Since(start))
	}
	if n := strings.Count(string(data), "data: "); n != 2 {
		t.Errorf("client got %d events, want the 2 sent before the stall", n)
	}
	select {
	case <-onlyRequest(t, g.backend).Canceled():
	case <-time.After(waitTimeout):
		t.Fatal("backend request not cancelled after the stall")
	}
	// Two chunks seen, "Hello" and " from": 10 bytes.
	expectUnits(t, settledRecord(t, g), units(int64(len(body)+3)/4, 0, 3, 0), true, true)
	if logs := g.logText(); !strings.Contains(logs, `"relay_end":"upstream_stalled"`) || !strings.Contains(logs, "silent for 150ms") {
		t.Errorf("log misses the stall:\n%s", logs)
	}
}

// The stall timer bounds each silence, not the stream: events arriving more often
// than the stall timeout keep a stream longer than it alive.
func TestSteadyStreamOutlivesTheStallTimeout(t *testing.T) {
	g := newTestGateway(t)
	g.backend.SetReply(fakebackend.Reply{EventDelay: 60 * time.Millisecond})
	url := serveGateway(t, g)
	start := time.Now()
	resp := workloadStream(t, url, `{"model":"slow","stream":true}`)
	data, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("stream broke: %v (%q)", err, data)
	}
	if time.Since(start) < 150*time.Millisecond {
		t.Fatalf("stream took %s, want longer than the stall timeout", time.Since(start))
	}
	if !strings.HasSuffix(string(data), "data: [DONE]\n\n") {
		t.Errorf("stream %q, want it whole", data)
	}
	if rec := settledRecord(t, g); rec.Partial || rec.Estimated {
		t.Errorf("record %+v, want complete", rec)
	}
}

// workloadStream posts a chat completion with the workload key, which may use the
// "slow" model.
func workloadStream(t *testing.T, url, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("POST", url+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+workloadKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// M13 (the audit's probe): a stream that ends cleanly after a content event, with no
// finish_reason and no [DONE], is incomplete — a backend failure: the client
// connection is cut, usage settled partial, not retried.
func TestStreamEndingBeforeItsTerminalChunkIsIncomplete(t *testing.T) {
	g := newTestGateway(t)
	g.backend.SetReply(fakebackend.Reply{EndAfter: 1})
	url := serveGateway(t, g)
	body := `{"model":"open","stream":true}`
	resp := streamRequest(t, context.Background(), url, body)
	data, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err == nil {
		t.Fatalf("incomplete stream ended cleanly: %q", data)
	}
	if n := strings.Count(string(data), "data: "); n != 1 {
		t.Errorf("client got %d events, want 1", n)
	}
	// One chunk seen, "Hello": 5 bytes.
	expectUnits(t, settledRecord(t, g), units(int64(len(body)+3)/4, 0, 2, 0), true, true)
	if n := len(g.backend.Requests()); n != 1 {
		t.Errorf("backend got %d requests, want 1", n)
	}
	if logs := g.logText(); !strings.Contains(logs, `"relay_end":"upstream_incomplete"`) {
		t.Errorf("log misses relay_end:\n%s", logs)
	}
}

// A successful JSON body that ends before its top-level value closes is incomplete
// too; one cut short of its Content-Length breaks off at the connection.
func TestJSONBodyEndingEarlyIsAnUpstreamFailure(t *testing.T) {
	for _, c := range []struct {
		name  string
		reply fakebackend.Reply
		end   string
	}{
		{"ended cleanly, unclosed", fakebackend.Reply{Body: `{"id":"x","choices":[{"message":{"content":"Hel`}, "upstream_incomplete"},
		{"short of its length", fakebackend.Reply{Body: `{"id":"x"}`, Header: map[string]string{"Content-Length": "40"}}, "upstream_failed"},
	} {
		t.Run(c.name, func(t *testing.T) {
			g := newTestGateway(t)
			g.backend.SetReply(c.reply)
			url := serveGateway(t, g)
			req, _ := http.NewRequest("POST", url+"/v1/chat/completions", strings.NewReader(`{"model":"open"}`))
			req.Header.Set("Authorization", "Bearer "+userKey)
			resp, err := http.DefaultClient.Do(req)
			if err == nil {
				data, readErr := io.ReadAll(resp.Body)
				resp.Body.Close()
				if readErr == nil {
					t.Fatalf("truncated body relayed as complete: %q", data)
				}
			}
			if rec := settledRecord(t, g); !rec.Partial {
				t.Errorf("record %+v, want partial", rec)
			}
			if logs := g.logText(); !strings.Contains(logs, `"relay_end":"`+c.end+`"`) {
				t.Errorf("log misses relay_end %s:\n%s", c.end, logs)
			}
		})
	}
}

// E8: only data events are progress. A backend that sends keep-alive comments and
// no data past the stall timeout has stalled: the relay ends upstream_stalled. The
// comments still reach the client. The "slow" backend's stall timeout is 150 ms.
func TestKeepAliveCommentsDoNotHoldOffTheStallTimer(t *testing.T) {
	g := newTestGateway(t)
	g.backend.SetReply(fakebackend.Reply{HangAfter: 2, PingEvery: 40 * time.Millisecond})
	url := serveGateway(t, g)
	// Bounded: a stall timer the pings keep resetting would never end the stream.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", url+"/v1/chat/completions", strings.NewReader(`{"model":"slow","stream":true}`))
	req.Header.Set("Authorization", "Bearer "+workloadKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if ctx.Err() != nil {
		t.Fatalf("stream still open after 2 s of pings, stall timeout 150 ms: %q", data)
	}
	if err == nil {
		t.Fatalf("stream ended cleanly while the backend only pinged: %q", data)
	}
	if n := strings.Count(string(data), "data: "); n != 2 {
		t.Errorf("client got %d data events, want the 2 sent before the pings", n)
	}
	if !strings.Contains(string(data), ": ping\n\n") {
		t.Errorf("client got %q, want the keep-alive comments relayed", data)
	}
	settledRecord(t, g)
	if logs := g.logText(); !strings.Contains(logs, `"relay_end":"upstream_stalled"`) {
		t.Errorf("log misses the stall:\n%s", logs)
	}
}
