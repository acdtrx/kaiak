package server

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"kaiak/internal/config"
	"kaiak/internal/fakebackend"
)

// Queue settings of the capped test config: backends local and slow take one request
// at a time (slow's first-event timeout is 500 ms, the margin a queued request has to
// join the queue behind a stalled one); the global queue holds 2 for a minute; secret times out after
// secretQueueTimeout; Org/open-7b never queues. The research team has a token limit,
// so reservations show.
const secretQueueTimeout = 50 * time.Millisecond

func cappedDoc(t *testing.T, doc string) string {
	doc = replaceOnce(t, doc, `"max_request_body_bytes": 1024 }`,
		`"max_request_body_bytes": 1024, "queue": { "size": 2, "timeout_ms": 60000 } }`)
	doc = replaceOnce(t, doc, `"api_key_env": "LOCAL_KEY" }`, `"api_key_env": "LOCAL_KEY", "max_in_flight": 1 }`)
	doc = replaceOnce(t, doc, `"first_event_timeout_ms": 150, "response_timeout_ms": 150, "stall_timeout_ms": 150 }`,
		`"first_event_timeout_ms": 500, "response_timeout_ms": 500, "stall_timeout_ms": 500, "max_in_flight": 1 }`)
	doc = replaceOnce(t, doc, `"secret": {`, `"secret": { "queue": { "timeout_ms": 50 },`)
	doc = replaceOnce(t, doc, `"Org/open-7b": {`, `"Org/open-7b": { "queue": { "size": 0 },`)
	return replaceOnce(t, doc, `"research": {}`, `"research": { "limits": [{ "type": "tokens_per_minute", "value": 100000 }] }`)
}

// newCappedGateway is the test gateway under the capped config, applied as cmd/kaiak
// applies a config: swapped in, and its caps handed to routing.
func newCappedGateway(t *testing.T) *testGateway {
	t.Helper()
	g := newTestGateway(t)
	g.apply(t, func(doc string) string { return cappedDoc(t, doc) })
	return g
}

// waitQueued waits until n requests wait in model's queue.
func waitQueued(t *testing.T, g *testGateway, model string, n int) {
	t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for g.router.Serving(nil).Models[model].Queued != n {
		if time.Now().After(deadline) {
			t.Fatalf("queue %s holds %d, want %d", model, g.router.Serving(nil).Models[model].Queued, n)
		}
		runtime.Gosched()
	}
}

// answer is a client's view of one response.
type answer struct {
	status int
	header http.Header
	body   string
	err    error
}

// send posts body with the workload key and request ID id to url on its own
// goroutine; ctx ends the client's side.
func send(ctx context.Context, url, id, body string) <-chan answer {
	out := make(chan answer, 1)
	go func() {
		req, err := http.NewRequestWithContext(ctx, "POST", url+"/v1/chat/completions", strings.NewReader(body))
		if err != nil {
			out <- answer{err: err}
			return
		}
		req.Header.Set("Authorization", "Bearer "+workloadKey)
		req.Header.Set("X-Request-Id", id)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			out <- answer{err: err}
			return
		}
		defer resp.Body.Close()
		data, err := io.ReadAll(resp.Body)
		out <- answer{status: resp.StatusCode, header: resp.Header, body: string(data), err: err}
	}()
	return out
}

func await(t *testing.T, c <-chan answer) answer {
	t.Helper()
	select {
	case a := <-c:
		return a
	case <-time.After(waitTimeout):
		t.Fatal("no answer")
	}
	return answer{}
}

// holdStream starts a stream to model at url and waits for its first event: it then
// holds its backend's slot. cancel disconnects the client.
func holdStream(t *testing.T, url, model string) (events *bufio.Reader, cancel func()) {
	t.Helper()
	ctx, stop := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, "POST", url+"/v1/chat/completions",
		strings.NewReader(`{"model":"`+model+`","stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+workloadKey)
	req.Header.Set("X-Request-Id", "holder")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	events = bufio.NewReader(resp.Body)
	readEvent(t, events)
	return events, func() { stop(); resp.Body.Close() }
}

// logLine returns the request log line of request ID id.
func logLine(t *testing.T, g *testGateway, id string) string {
	t.Helper()
	for line := range strings.SplitSeq(g.logText(), "\n") {
		if strings.Contains(line, `"msg":"request"`) && strings.Contains(line, `"kaiak.request.id":"`+id+`"`) {
			return line
		}
	}
	t.Fatalf("no log line for %s:\n%s", id, g.logText())
	return ""
}

// scrape returns the metrics exposition.
func scrape(g *testGateway) string {
	w := httptest.NewRecorder()
	scrapeHandler(g.metrics).ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	return w.Body.String()
}

// reservedTokens is the research team's per-minute tokens in use: settled plus
// still reserved.
func reservedTokens(t *testing.T, g *testGateway) int64 {
	t.Helper()
	return counterUsed(t, g, "research", config.LimitTokensPerMinute)
}

// settledTokens sums the tokens of every usage record so far.
func settledTokens(g *testGateway) int64 {
	var n int64
	for _, r := range g.usage.all() {
		n += r.Units[config.UnitTokensIn] + r.Units[config.UnitTokensCached] + r.Units[config.UnitTokensCacheWrite] +
			r.Units[config.UnitTokensOut]
	}
	return n
}

func TestCappedBackendQueuesInOrderAndRefusesWhenFull(t *testing.T) {
	g := newCappedGateway(t)
	g.backend.SetReply(fakebackend.Reply{Fault: &fakebackend.StreamFault{At: 1, Kind: fakebackend.Hang}})
	srv := httptest.NewServer(g.h)
	t.Cleanup(srv.Close)

	_, cancelHolder := holdStream(t, srv.URL, "open")
	g.backend.SetReply(fakebackend.Reply{})
	first := send(context.Background(), srv.URL, "queued-1", `{"model":"open"}`)
	waitQueued(t, g, "open", 1)
	second := send(context.Background(), srv.URL, "queued-2", `{"model":"open"}`)
	waitQueued(t, g, "open", 2)
	if n := len(g.backend.Requests()); n != 1 {
		t.Fatalf("backend received %d requests with a cap of 1, want 1", n)
	}

	// The queue (size 2) is full: refused at once, without reaching routing's slot.
	w := do(t, g.h, call{method: "POST", path: "/v1/chat/completions", key: workloadKey, body: `{"model":"open"}`,
		header: map[string]string{"X-Request-Id": "refused"}})
	expectError(t, w, http.StatusTooManyRequests, "queue_full")
	if typ, _, _ := openAIError(t, w); typ != "server_error" {
		t.Errorf("queue_full type %q, want server_error", typ)
	}
	if got := w.Header().Get("Retry-After"); got != "1" {
		t.Errorf("queue_full Retry-After %q, want 1", got)
	}

	// The holder's client leaves mid-stream: its slot goes to the first in line, then
	// the second.
	cancelHolder()
	for _, c := range []<-chan answer{first, second} {
		if a := await(t, c); a.err != nil || a.status != http.StatusOK {
			t.Fatalf("queued request: %d %v %s", a.status, a.err, a.body)
		}
	}
	srv.Close() // waits for every handler: records settled, lines logged
	reqs := g.backend.Requests()
	if len(reqs) != 3 || reqs[1].Header.Get("X-Request-Id") != "queued-1" || reqs[2].Header.Get("X-Request-Id") != "queued-2" {
		t.Errorf("backend order %d requests, want holder, queued-1, queued-2", len(reqs))
	}

	for _, id := range []string{"queued-1", "queued-2"} {
		if line := logLine(t, g, id); !strings.Contains(line, `"kaiak.queue.wait_duration":`) {
			t.Errorf("%s log line has no queue wait: %s", id, line)
		}
	}
	if line := logLine(t, g, "refused"); strings.Contains(line, "kaiak.queue.wait_duration") || !strings.Contains(line, `"error.type":"queue_full"`) {
		t.Errorf("refused log line: %s", line)
	}
	if line := logLine(t, g, "holder"); strings.Contains(line, "kaiak.queue.wait_duration") {
		t.Errorf("a request that never queued logs a wait: %s", line)
	}
	// The refused request never routed: no record, its reservation released.
	if n := len(g.usage.all()); n != 3 {
		t.Errorf("%d usage records, want 3 (holder, queued-1, queued-2)", n)
	}
	if got, want := reservedTokens(t, g), settledTokens(g); got != want {
		t.Errorf("team tokens %d, want the settled %d", got, want)
	}
	out := scrape(g)
	for _, want := range []string{
		`kaiak_queue_rejections_total{gen_ai_request_model="open",error_type="queue_full"} 1`,
		`kaiak_queue_wait_duration_seconds_count{gen_ai_request_model="open"} 2`,
		`kaiak_errors_total{kaiak_error_class="queue_rejected"} 1`,
		`kaiak_queue_size{gen_ai_request_model="open"} 0`,
		`kaiak_backend_active_requests_limit{kaiak_backend_id="local"} 1`,
	} {
		if !strings.Contains(out, want+"\n") {
			t.Errorf("metrics miss %s", want)
		}
	}
}

func TestQueueTimeoutAndNoQueue(t *testing.T) {
	g := newCappedGateway(t)
	g.backend.SetReply(fakebackend.Reply{Fault: &fakebackend.StreamFault{At: 1, Kind: fakebackend.Hang}})
	srv := httptest.NewServer(g.h)
	t.Cleanup(srv.Close)
	_, cancelHolder := holdStream(t, srv.URL, "open") // local's one slot

	start := time.Now()
	w := do(t, g.h, call{method: "POST", path: "/v1/chat/completions", key: workloadKey, body: `{"model":"secret"}`,
		header: map[string]string{"X-Request-Id": "timed-out"}})
	waited := time.Since(start)
	expectError(t, w, http.StatusTooManyRequests, "queue_timeout")
	if waited < secretQueueTimeout {
		t.Errorf("answered after %s, want at least the model's timeout %s", waited, secretQueueTimeout)
	}
	if typ, _, _ := openAIError(t, w); typ != "server_error" || !strings.Contains(w.Body.String(), "timeout of 50ms") {
		t.Errorf("queue_timeout type %q, body %s", typ, w.Body.String())
	}
	if got := w.Header().Get("Retry-After"); got != "" {
		t.Errorf("queue_timeout Retry-After %q, want none", got)
	}

	// Size 0: refused at once, never queued.
	w = do(t, g.h, call{method: "POST", path: "/v1/chat/completions", key: workloadKey, body: `{"model":"Org/open-7b"}`,
		header: map[string]string{"X-Request-Id": "no-queue"}})
	expectError(t, w, http.StatusTooManyRequests, "queue_full")

	cancelHolder()
	srv.Close()
	if line := logLine(t, g, "timed-out"); !strings.Contains(line, `"kaiak.queue.wait_duration":`) || !strings.Contains(line, `"http.response.status_code":429`) {
		t.Errorf("timed-out log line: %s", line)
	}
	if line := logLine(t, g, "no-queue"); strings.Contains(line, "kaiak.queue.wait_duration") {
		t.Errorf("no-queue log line: %s", line)
	}
	if n := len(g.usage.all()); n != 1 {
		t.Errorf("%d usage records, want the holder's only", n)
	}
	if got, want := reservedTokens(t, g), settledTokens(g); got != want {
		t.Errorf("team tokens %d, want the settled %d", got, want)
	}
	out := scrape(g)
	for _, want := range []string{
		`kaiak_queue_rejections_total{gen_ai_request_model="secret",error_type="queue_timeout"} 1`,
		`kaiak_queue_rejections_total{gen_ai_request_model="Org/open-7b",error_type="queue_full"} 1`,
		`kaiak_errors_total{kaiak_error_class="queue_rejected"} 2`,
	} {
		if !strings.Contains(out, want+"\n") {
			t.Errorf("metrics miss %s", want)
		}
	}
}

func TestLeavingTheQueueReleasesTheReservation(t *testing.T) {
	g := newCappedGateway(t)
	g.backend.SetReply(fakebackend.Reply{Fault: &fakebackend.StreamFault{At: 1, Kind: fakebackend.Hang}})
	srv := httptest.NewServer(g.h)
	t.Cleanup(srv.Close)
	_, cancelHolder := holdStream(t, srv.URL, "open")

	ctx, leave := context.WithCancel(context.Background())
	gone := send(ctx, srv.URL, "gone", `{"model":"open"}`)
	waitQueued(t, g, "open", 1)
	leave()
	if a := await(t, gone); a.err == nil {
		t.Fatalf("client that left got an answer: %d", a.status)
	}
	waitQueued(t, g, "open", 0)
	if n := len(g.backend.Requests()); n != 1 {
		t.Errorf("backend received %d requests, want the holder's only", n)
	}
	cancelHolder()
	srv.Close()
	line := logLine(t, g, "gone")
	if strings.Contains(line, `"http.response.status_code"`) || !strings.Contains(line, `"error.type":"client_closed"`) ||
		!strings.Contains(line, `"kaiak.queue.wait_duration":`) {
		t.Errorf("gone log line: %s", line)
	}
	// No status was sent: the request duration has none either.
	expectMetricLines(t, g.metricsText(),
		`http_server_request_duration_seconds_count{http_request_method="POST",url_scheme="http",http_route="/v1/chat/completions",error_type="client_closed",gen_ai_request_model="open"} 1`)
	if n := len(g.usage.all()); n != 1 {
		t.Errorf("%d usage records, want the holder's only", n)
	}
	if got, want := reservedTokens(t, g), settledTokens(g); got != want {
		t.Errorf("team tokens %d, want the settled %d", got, want)
	}
	if n := inFlight(g); len(n) != 0 {
		t.Errorf("in flight %v, want none", n)
	}
}

func TestSlotIsHandedOnEveryRelayEnd(t *testing.T) {
	pace := make(chan struct{})
	cutPace := make(chan struct{}, 1)
	for _, c := range []struct {
		name  string
		model string
		hold  fakebackend.Reply
		// end ends the holder's request; nil lets it end by itself.
		end func(cancel func())
	}{
		{"success", "open", fakebackend.Reply{Pace: pace}, func(func()) { close(pace) }},
		{"client disconnect", "open", fakebackend.Reply{Fault: &fakebackend.StreamFault{At: 1, Kind: fakebackend.Hang}}, func(cancel func()) { cancel() }},
		// One more event, then the backend drops the connection.
		{"upstream cut mid-stream", "open", fakebackend.Reply{Pace: cutPace, Fault: &fakebackend.StreamFault{At: 2, Kind: fakebackend.Cut}},
			func(func()) { cutPace <- struct{}{} }},
		// The slow backend's first-event timeout ends the holder's only attempt (its
		// model has one deployment: no retry); the waiting request gets its slot.
		{"backend error", "slow", fakebackend.Reply{Before: fakebackend.StallFirstByte}, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			g := newCappedGateway(t)
			g.backend.SetReply(c.hold)
			srv := httptest.NewServer(g.h)
			t.Cleanup(srv.Close)
			var holder <-chan answer
			var cancel func()
			if c.hold.Before == fakebackend.StallFirstByte {
				holder = send(context.Background(), srv.URL, "holder", `{"model":"`+c.model+`","stream":true}`)
				<-g.backend.Arrivals()
			} else {
				_, cancel = holdStream(t, srv.URL, c.model)
			}
			g.backend.SetReply(fakebackend.Reply{})
			queued := send(context.Background(), srv.URL, "queued", `{"model":"`+c.model+`"}`)
			waitQueued(t, g, c.model, 1)

			if c.end != nil {
				c.end(cancel)
			}
			if a := await(t, queued); a.err != nil || a.status != http.StatusOK {
				t.Fatalf("queued request: %d %v %s", a.status, a.err, a.body)
			}
			if holder != nil {
				if a := await(t, holder); a.status != http.StatusGatewayTimeout {
					t.Errorf("holder answered %d, want 504 from its timed-out attempt", a.status)
				}
				var order []string
				for _, r := range g.backend.Requests() {
					order = append(order, r.Header.Get("X-Request-Id"))
				}
				if strings.Join(order, ",") != "holder,queued" {
					t.Errorf("backend served %v, want the holder, then the waiting request", order)
				}
			}
			if cancel != nil {
				cancel()
			}
			srv.Close()
			if n := inFlight(g); len(n) != 0 {
				t.Errorf("in flight %v, want none", n)
			}
		})
	}
}

func TestDrainServesTheQueueOrCutsIt(t *testing.T) {
	t.Run("served within the timeout", func(t *testing.T) {
		g := newCappedGateway(t)
		g.backend.SetReply(fakebackend.Reply{Fault: &fakebackend.StreamFault{At: 1, Kind: fakebackend.Hang}})
		d := newDrainable(t, g)
		_, cancelHolder := holdStream(t, d.url, "open")
		g.backend.SetReply(fakebackend.Reply{})
		queued := send(context.Background(), d.url, "queued", `{"model":"open"}`)
		waitQueued(t, g, "open", 1)

		g.drain.begin(DrainTimes{}, d.logger)
		g.drain.refuse(d.l, d.logger)
		finished := make(chan struct{})
		go func() {
			g.drain.finish(d.l, time.Hour, nil, d.logger)
			close(finished)
		}()
		cancelHolder()
		if a := await(t, queued); a.status != http.StatusOK {
			t.Errorf("queued request during the drain: %d %s", a.status, a.body)
		}
		select {
		case <-finished:
		case <-time.After(waitTimeout):
			t.Fatal("drain did not finish once the queue was served")
		}
	})

	t.Run("cut at the timeout", func(t *testing.T) {
		g := newCappedGateway(t)
		g.backend.SetReply(fakebackend.Reply{Fault: &fakebackend.StreamFault{At: 1, Kind: fakebackend.Hang}})
		d := newDrainable(t, g)
		_, cancelHolder := holdStream(t, d.url, "open")
		defer cancelHolder()
		queued := send(context.Background(), d.url, "queued", `{"model":"open"}`)
		waitQueued(t, g, "open", 1)

		g.drain.begin(DrainTimes{}, d.logger)
		g.drain.refuse(d.l, d.logger)
		// finish returns only once every handler is over: no queued request hangs.
		g.drain.finish(d.l, 20*time.Millisecond, nil, d.logger)
		if a := await(t, queued); a.err == nil && a.status == http.StatusOK {
			t.Errorf("queued request served after the cut")
		}
		line := logLine(t, g, "queued")
		if !strings.Contains(line, `"error.type":"server_shutting_down"`) || !strings.Contains(line, `"kaiak.queue.wait_duration":`) {
			t.Errorf("queued log line: %s", line)
		}
		if records := g.usage.all(); len(records) != 1 || !records[0].Partial {
			t.Errorf("records %+v, want the holder's partial one only", records)
		}
		if got, want := reservedTokens(t, g), settledTokens(g); got != want {
			t.Errorf("team tokens %d, want the settled %d", got, want)
		}
		if logs := g.logText(); !strings.Contains(logs, `"kaiak.reason":"timeout","kaiak.drain.in_flight":2`) {
			t.Errorf("log misses the cut of both requests:\n%s", logs)
		}
	})
}
