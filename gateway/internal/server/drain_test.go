package server

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"syscall"
	"testing"
	"time"

	"kaiak/internal/config"
	"kaiak/internal/fakebackend"
)

// drainable serves g's API on a real listener, as the binary does. served receives
// Serve's result.
type drainable struct {
	g      *testGateway
	l      *Listener
	url    string
	served chan error
	logger *slog.Logger
}

func newDrainable(t *testing.T, g *testGateway) *drainable {
	t.Helper()
	logger := slog.New(slog.NewJSONHandler(g.log, nil))
	l, err := Listen("api", "127.0.0.1:0", g.h, DefaultClientTimeouts, logger)
	if err != nil {
		t.Fatal(err)
	}
	d := &drainable{g: g, l: l, url: "http://" + l.Addr().String(), served: make(chan error, 1), logger: logger}
	go func() { d.served <- l.Serve() }()
	t.Cleanup(func() { l.cut() }) // a failed test must not leave handlers running
	return d
}

// post sends a chat completion with key through client, tracing whether it reused a
// kept-alive connection.
func (d *drainable) post(t *testing.T, client *http.Client, key, body string) (*http.Response, bool, error) {
	t.Helper()
	reused := false
	trace := &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) { reused = info.Reused }}
	ctx := httptrace.WithClientTrace(context.Background(), trace)
	req, err := http.NewRequestWithContext(ctx, "POST", d.url+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := client.Do(req)
	return resp, reused, err
}

// newClient returns a client with its own connection pool.
func newClient(t *testing.T) *http.Client {
	t.Helper()
	tr := &http.Transport{}
	t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr}
}

func readyz(t *testing.T, admin http.Handler, path string) (int, string) {
	t.Helper()
	w := httptest.NewRecorder()
	admin.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
	return w.Code, strings.TrimSpace(w.Body.String())
}

func TestDrainFinishesInFlightStreamsAndRefusesNewRequests(t *testing.T) {
	g := newTestGateway(t)
	pace := make(chan struct{})
	g.backend.SetReply(fakebackend.Reply{Pace: pace})
	d := newDrainable(t, g)
	admin := NewAdmin(g.holder, g.drain, g.metrics, "")

	// A kept-alive connection, idle when the drain starts.
	keptAlive := newClient(t)
	resp, _, err := d.post(t, keptAlive, userKey, `{"model":"open"}`)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Close {
		t.Fatalf("before the drain: status %d, Connection: close %v", resp.StatusCode, resp.Close)
	}

	// A stream in flight.
	stream, _, err := d.post(t, newClient(t), userKey, `{"model":"open","stream":true,"stream_options":{"include_usage":true}}`)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Body.Close()
	events := bufio.NewReader(stream.Body)
	readEvent(t, events)

	g.drain.begin(DrainTimes{}, d.logger)
	if code, body := readyz(t, admin, "/readyz"); code != http.StatusServiceUnavailable || body != "draining" {
		t.Errorf("/readyz while draining: %d %q, want 503 draining", code, body)
	}
	if code, _ := readyz(t, admin, "/healthz"); code != http.StatusOK {
		t.Errorf("/healthz while draining: %d, want 200", code)
	}

	// Grace: a new request is served, and its connection closed after it.
	resp, _, err = d.post(t, newClient(t), userKey, `{"model":"open"}`)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !resp.Close {
		t.Errorf("during grace: status %d, Connection: close %v; want 200, true", resp.StatusCode, resp.Close)
	}

	g.drain.refuse(d.l, d.logger)
	if err := <-d.served; err != nil {
		t.Errorf("Serve returned %v after the listener stopped accepting, want nil", err)
	}
	// Refusing: a request on the kept-alive connection is answered 503 and the
	// connection closed; a new connection is refused.
	resp, reused, err := d.post(t, keptAlive, userKey, `{"model":"open"}`)
	if err != nil {
		t.Fatalf("request on the kept-alive connection: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !reused || resp.StatusCode != http.StatusServiceUnavailable || !resp.Close ||
		!strings.Contains(string(body), `"code":"server_shutting_down"`) {
		t.Errorf("after grace on a kept-alive connection: reused %v, status %d, Connection: close %v, body %s",
			reused, resp.StatusCode, resp.Close, body)
	}
	if _, _, err := d.post(t, newClient(t), userKey, `{"model":"open"}`); !errors.Is(err, syscall.ECONNREFUSED) {
		t.Errorf("new connection after grace: %v, want connection refused", err)
	}

	finished := make(chan struct{})
	go func() {
		g.drain.finish(d.l, time.Hour, nil, d.logger)
		close(finished)
	}()
	select {
	case <-finished:
		t.Fatal("drain finished with a stream in flight")
	default:
	}
	// The stream runs to its end: text chunks, finish chunk, usage chunk, [DONE].
	for range len(fakebackend.DefaultChunks) + 2 {
		pace <- struct{}{}
	}
	rest, err := io.ReadAll(events)
	if err != nil || !strings.HasSuffix(string(rest), "data: [DONE]\n\n") {
		t.Errorf("stream did not end cleanly: %v, %q", err, rest)
	}
	select {
	case <-finished:
	case <-time.After(waitTimeout):
		t.Fatal("drain did not finish after the stream ended")
	}

	// Drained means settled. Every record is complete with the reported usage: the
	// request before the drain, the stream, the request during grace (the refused one
	// never reached routing).
	records := g.usage.all()
	if len(records) != 3 {
		t.Errorf("%d records, want 3", len(records))
	}
	for _, r := range records {
		if r.Partial || r.Estimated || r.Units[config.UnitTokensIn] != 7 ||
			r.Units[config.UnitTokensOut] != int64(len(fakebackend.DefaultChunks)) {
			t.Errorf("record %+v, want complete with the reported usage", r)
		}
	}
	logs := g.logText()
	for _, want := range []string{`"msg":"draining"`, `"msg":"draining: refusing new requests","in_flight":1`,
		`"msg":"drained"`, `"error_code":"server_shutting_down"`} {
		if !strings.Contains(logs, want) {
			t.Errorf("log misses %s:\n%s", want, logs)
		}
	}
}

func TestDrainTimeoutCutsOffHungRequests(t *testing.T) {
	for _, c := range []struct {
		name  string
		reply fakebackend.Reply
	}{
		{"mid-stream", fakebackend.Reply{HangAfter: 1}},
		{"before the first byte", fakebackend.Reply{StallBeforeFirstByte: true}},
	} {
		t.Run(c.name, func(t *testing.T) {
			g := newTestGateway(t)
			withLimits(t, g, `[{ "type": "tokens_per_minute", "value": 100000 }]`, "")
			g.backend.SetReply(c.reply)
			d := newDrainable(t, g)

			// "pair" reserves its output limit (256 tokens) on top of the input estimate.
			body := `{"model":"pair","stream":true}`
			type result struct {
				data []byte
				err  error
			}
			got := make(chan result, 1)
			go func() {
				resp, _, err := d.post(t, newClient(t), workloadKey, body)
				if err != nil {
					got <- result{nil, err}
					return
				}
				defer resp.Body.Close()
				data, err := io.ReadAll(resp.Body)
				got <- result{data, err}
			}()
			var arrived *fakebackend.Request
			select {
			case arrived = <-g.backend.Arrivals():
			case <-time.After(waitTimeout):
				t.Fatal("request never reached the backend")
			}

			g.drain.begin(DrainTimes{}, d.logger)
			g.drain.refuse(d.l, d.logger)
			g.drain.finish(d.l, 20*time.Millisecond, nil, d.logger)

			select {
			case <-arrived.Canceled():
			case <-time.After(waitTimeout):
				t.Fatal("upstream request not cancelled by the cut")
			}
			res := <-got
			if res.err == nil {
				t.Errorf("client saw a clean end after the cut: %q", res.data)
			}
			// finish returned after the handler: the request is settled, as partial,
			// and its reservation replaced by what it used.
			records := g.usage.all()
			if len(records) != 1 || !records[0].Partial {
				t.Fatalf("records %+v, want one partial", records)
			}
			r := records[0]
			if c.reply.StallBeforeFirstByte {
				// D1: the backend had the prompt when the drain cut it: the input,
				// estimated from the body, no output.
				expectUnits(t, r, units(int64(len(body)+3)/4, 0, 0, 0), true, true)
			}
			want := r.Units[config.UnitTokensIn] + r.Units[config.UnitTokensCached] + r.Units[config.UnitTokensOut]
			if used := counterUsed(t, g, "research", config.LimitTokensPerMinute); used != want {
				t.Errorf("team tokens %d after the cut, want the settled %d", used, want)
			}
			logs := g.logText()
			for _, want := range []string{`"reason":"timeout","requests":1`, `"cut_off":1`} {
				if !strings.Contains(logs, want) {
					t.Errorf("log misses %s:\n%s", want, logs)
				}
			}
			wantEnd := `"relay_end":"shutdown"`
			if c.reply.StallBeforeFirstByte {
				wantEnd = `"error_code":"server_shutting_down"`
			}
			if !strings.Contains(logs, wantEnd) {
				t.Errorf("log misses %s:\n%s", wantEnd, logs)
			}
			if got := errorsCounted(t, g, "shutting_down"); got != "1" {
				t.Errorf("kaiak_errors_total{class=shutting_down} = %s, want 1", got)
			}
		})
	}
}

func TestHurriedDrainSkipsTheWaitsAndCutsOff(t *testing.T) {
	g := newTestGateway(t)
	g.backend.SetReply(fakebackend.Reply{HangAfter: 1})
	d := newDrainable(t, g)
	resp, _, err := d.post(t, newClient(t), userKey, `{"model":"open","stream":true}`)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	readEvent(t, bufio.NewReader(resp.Body))

	hurry := make(chan struct{})
	close(hurry)
	done := make(chan struct{})
	go func() {
		g.drain.Run(d.l, DrainTimes{Grace: time.Hour, Timeout: time.Hour}, hurry, d.logger)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(waitTimeout):
		t.Fatal("hurried drain still waiting")
	}
	if records := g.usage.all(); len(records) != 1 || !records[0].Partial {
		t.Errorf("records %+v, want one partial", records)
	}
	if logs := g.logText(); !strings.Contains(logs, `"reason":"hurried","requests":1`) {
		t.Errorf("log misses the hurried cut:\n%s", logs)
	}
}

// errorsCounted reads kaiak_errors_total for class from the exposition.
func errorsCounted(t *testing.T, g *testGateway, class string) string {
	t.Helper()
	w := httptest.NewRecorder()
	g.metrics.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	prefix := `kaiak_errors_total{class="` + class + `"} `
	for line := range strings.SplitSeq(w.Body.String(), "\n") {
		if v, ok := strings.CutPrefix(line, prefix); ok {
			return v
		}
	}
	t.Fatalf("no %s in the exposition", prefix)
	return ""
}
