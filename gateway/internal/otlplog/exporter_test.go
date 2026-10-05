package otlplog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// received is one export request as the fake collector saw it.
type received struct {
	header   http.Header
	request  exportRequest
	messages []string
}

// collector is a fake OTLP/HTTP collector. respond answers request n (from 0);
// nil answers 200 with an empty ExportLogsServiceResponse.
type collector struct {
	srv      *httptest.Server
	got      chan received
	requests atomic.Int64
}

func newCollector(t *testing.T, respond func(w http.ResponseWriter, r *http.Request, n int)) *collector {
	t.Helper()
	c := &collector{got: make(chan received, 1000)}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(c.requests.Add(1) - 1)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("collector: read body: %v", err)
			return
		}
		rec := received{header: r.Header.Clone()}
		if err := json.Unmarshal(body, &rec.request); err != nil {
			t.Errorf("collector: decode %s: %v", body, err)
		}
		for _, rl := range rec.request.ResourceLogs {
			for _, sl := range rl.ScopeLogs {
				for _, lr := range sl.LogRecords {
					rec.messages = append(rec.messages, *lr.Body.StringValue)
				}
			}
		}
		c.got <- rec
		if respond == nil {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, "{}")
			return
		}
		respond(w, r, n)
	}))
	t.Cleanup(c.srv.Close)
	return c
}

// next waits for the collector's next request.
func (c *collector) next(t *testing.T) received {
	t.Helper()
	select {
	case r := <-c.got:
		return r
	case <-time.After(10 * time.Second):
		t.Fatal("no export request reached the collector")
		return received{}
	}
}

// none checks that no request is waiting.
func (c *collector) none(t *testing.T) {
	t.Helper()
	select {
	case r := <-c.got:
		t.Fatalf("unexpected export request with %q", r.messages)
	default:
	}
}

// syncBuffer collects the problem reports, written from the sender goroutine.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

// reports decodes the `log export failing` lines written so far.
func (s *syncBuffer) reports(t *testing.T) []map[string]any {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	var lines []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(s.b.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("report line %q: %v", line, err)
		}
		if m["msg"] != "log export failing" || m["level"] != "WARN" {
			t.Fatalf("report line %q: want a WARN `log export failing`", line)
		}
		lines = append(lines, m)
	}
	return lines
}

// fakeClock is the reports' clock.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// harness is an exporter posting to a fake collector, with a manual tick, retry
// waits that end at once (each recorded), and its reports in a buffer.
type harness struct {
	e       *Exporter
	col     *collector
	tick    chan time.Time
	log     *slog.Logger
	reports *syncBuffer
	clock   *fakeClock

	mu    sync.Mutex
	waits []time.Duration
}

func newHarness(t *testing.T, col *collector, vars map[string]string, opts options) *harness {
	t.Helper()
	if vars == nil {
		vars = map[string]string{}
	}
	vars["OTEL_EXPORTER_OTLP_LOGS_ENDPOINT"] = col.srv.URL + "/v1/logs"
	h := &harness{col: col, tick: make(chan time.Time), reports: &syncBuffer{},
		clock: &fakeClock{now: time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)}}
	opts.tick = h.tick
	opts.now = h.clock.Now
	if opts.after == nil {
		opts.after = func(d time.Duration) <-chan time.Time {
			h.mu.Lock()
			h.waits = append(h.waits, d)
			h.mu.Unlock()
			ch := make(chan time.Time, 1)
			ch <- time.Time{}
			return ch
		}
	}
	h.e = newExporter(mustSettings(t, vars), Resource{ServiceVersion: "1.2.3", InstanceID: "gw-1"},
		slog.New(slog.NewJSONHandler(h.reports, nil)), opts)
	t.Cleanup(h.e.Close)
	h.log = slog.New(h.e.Handler(slog.NewJSONHandler(io.Discard, nil)))
	return h
}

func (h *harness) logN(n int, prefix string) {
	for i := range n {
		h.log.Info(fmt.Sprintf("%s%d", prefix, i))
	}
}

func (h *harness) flush(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := h.e.Flush(ctx); err != nil {
		t.Fatalf("Flush: %v", err)
	}
}

func (h *harness) recordedWaits() []time.Duration {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]time.Duration(nil), h.waits...)
}

func wantCounts(t *testing.T, e *Exporter, want Counts) {
	t.Helper()
	if got := e.Counts(); got != want {
		t.Fatalf("counts %+v, want %+v", got, want)
	}
}

func messages(prefix string, from, to int) []string {
	var m []string
	for i := from; i < to; i++ {
		m = append(m, fmt.Sprintf("%s%d", prefix, i))
	}
	return m
}

func TestExportRequestShape(t *testing.T) {
	col := newCollector(t, nil)
	h := newHarness(t, col, map[string]string{
		"OTEL_EXPORTER_OTLP_HEADERS": "authorization=Bearer%20abc,x-scope-orgid=tenant-1",
	}, options{})
	h.log.Info("one", "kaiak.key.id", "k1")
	h.tick <- time.Now()
	r := col.next(t)
	for name, want := range map[string]string{
		"Content-Type":  "application/json",
		"User-Agent":    "kaiak/1.2.3",
		"Authorization": "Bearer abc",
		"X-Scope-Orgid": "tenant-1",
	} {
		if got := r.header.Get(name); got != want {
			t.Errorf("header %s = %q, want %q", name, got, want)
		}
	}
	rl := r.request.ResourceLogs[0]
	var res []string
	for _, kv := range rl.Resource.Attributes {
		res = append(res, kv.Key+"="+*kv.Value.StringValue)
	}
	if want := []string{"service.name=kaiak", "service.version=1.2.3", "service.instance.id=gw-1"}; !reflect.DeepEqual(res, want) {
		t.Errorf("resource %q, want %q", res, want)
	}
	if rl.ScopeLogs[0].Scope.Name != "kaiak" {
		t.Errorf("scope %q, want kaiak", rl.ScopeLogs[0].Scope.Name)
	}
	if !reflect.DeepEqual(r.messages, []string{"one"}) {
		t.Errorf("messages %q, want [one]", r.messages)
	}
}

func TestBatchBySize(t *testing.T) {
	col := newCollector(t, nil)
	done := make(chan struct{}, 10)
	h := newHarness(t, col, nil, options{batchDone: func() { done <- struct{}{} }})
	h.logN(batchSize+1, "m")

	first := col.next(t)
	if !reflect.DeepEqual(first.messages, messages("m", 0, batchSize)) {
		t.Fatalf("first batch: %d records, want the oldest %d", len(first.messages), batchSize)
	}
	<-done
	col.none(t)
	if n := len(queued(h.e)); n != 1 {
		t.Fatalf("%d records queued after the full batch, want 1 waiting for the interval", n)
	}
	h.tick <- time.Now()
	if second := col.next(t); !reflect.DeepEqual(second.messages, []string{fmt.Sprintf("m%d", batchSize)}) {
		t.Fatalf("second batch %q, want the one left", second.messages)
	}
	<-done
	wantCounts(t, h.e, Counts{Exported: batchSize + 1})
}

func TestBatchByInterval(t *testing.T) {
	col := newCollector(t, nil)
	h := newHarness(t, col, nil, options{})
	h.logN(3, "m")
	h.tick <- time.Now()
	if r := col.next(t); !reflect.DeepEqual(r.messages, messages("m", 0, 3)) {
		t.Fatalf("batch %q, want m0..m2", r.messages)
	}
	h.flush(t)
	wantCounts(t, h.e, Counts{Exported: 3})
}

func TestRetryOnEachRetryableStatus(t *testing.T) {
	for _, status := range []int{429, 502, 503, 504} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			col := newCollector(t, func(w http.ResponseWriter, _ *http.Request, n int) {
				if n < 2 {
					w.WriteHeader(status)
					return
				}
				io.WriteString(w, "{}")
			})
			h := newHarness(t, col, nil, options{})
			h.logN(2, "m")
			h.flush(t)
			if n := col.requests.Load(); n != 3 {
				t.Fatalf("%d requests, want 3 (two retries)", n)
			}
			wantCounts(t, h.e, Counts{Exported: 2})
			waits := h.recordedWaits()
			if len(waits) != 2 {
				t.Fatalf("waits %v, want 2", waits)
			}
			// Backoff with jitter over the upper half: 0.5 s, then 1 s.
			for i, base := range []time.Duration{500 * time.Millisecond, time.Second} {
				if waits[i] < base/2 || waits[i] > base {
					t.Errorf("wait %d = %s, want within [%s, %s]", i, waits[i], base/2, base)
				}
			}
			if r := h.reports.reports(t); len(r) != 0 {
				t.Errorf("reports %v, want none: the batch was delivered", r)
			}
		})
	}
}

func TestBackoffDoublesToFiveSeconds(t *testing.T) {
	for attempt, base := range []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second, 4 * time.Second, 5 * time.Second, 5 * time.Second, 5 * time.Second} {
		for range 100 {
			if d := backoff(attempt); d < base/2 || d > base {
				t.Fatalf("backoff(%d) = %s, want within [%s, %s]", attempt, d, base/2, base)
			}
		}
	}
	if d := backoff(1000); d > maxBackoff {
		t.Fatalf("backoff(1000) = %s, want at most %s", d, maxBackoff)
	}
}

func TestRetryOnNetworkError(t *testing.T) {
	col := newCollector(t, func(w http.ResponseWriter, _ *http.Request, n int) {
		if n == 0 {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Errorf("hijack: %v", err)
				return
			}
			conn.Close()
			return
		}
		io.WriteString(w, "{}")
	})
	h := newHarness(t, col, nil, options{})
	h.logN(3, "m")
	h.flush(t)
	if n := col.requests.Load(); n != 2 {
		t.Fatalf("%d requests, want 2 (one retry)", n)
	}
	wantCounts(t, h.e, Counts{Exported: 3})
}

func TestRetryAfterIsHonoured(t *testing.T) {
	for _, tc := range []struct {
		name     string
		header   func() string
		min, max time.Duration
	}{
		{"seconds", func() string { return "3" }, 3 * time.Second, 3 * time.Second},
		{"zero seconds: the backoff", func() string { return "0" }, 250 * time.Millisecond, 500 * time.Millisecond},
		{"HTTP date", func() string { return time.Now().Add(5 * time.Second).UTC().Format(http.TimeFormat) }, 3 * time.Second, 5 * time.Second},
		{"HTTP date in the past: the backoff", func() string { return time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat) }, 250 * time.Millisecond, 500 * time.Millisecond},
		{"unreadable: backoff", func() string { return "soon" }, 250 * time.Millisecond, 500 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			col := newCollector(t, func(w http.ResponseWriter, _ *http.Request, n int) {
				if n == 0 {
					w.Header().Set("Retry-After", tc.header())
					w.WriteHeader(http.StatusTooManyRequests)
					return
				}
				io.WriteString(w, "{}")
			})
			h := newHarness(t, col, nil, options{})
			h.logN(1, "m")
			h.flush(t)
			waits := h.recordedWaits()
			if len(waits) != 1 || waits[0] < tc.min || waits[0] > tc.max {
				t.Fatalf("waits %v, want one within [%s, %s]", waits, tc.min, tc.max)
			}
			wantCounts(t, h.e, Counts{Exported: 1})
		})
	}
}

func TestRetryAfterBeyondTheTimeLeftFailsAtOnce(t *testing.T) {
	col := newCollector(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	h := newHarness(t, col, map[string]string{"OTEL_EXPORTER_OTLP_LOGS_TIMEOUT": "10000"}, options{})
	h.logN(4, "m")
	h.flush(t)
	if n := col.requests.Load(); n != 1 {
		t.Fatalf("%d requests, want 1", n)
	}
	if w := h.recordedWaits(); len(w) != 0 {
		t.Fatalf("waited %v, want no wait", w)
	}
	wantCounts(t, h.e, Counts{Failed: 4})
}

func TestRetriesEndWithTheTimeout(t *testing.T) {
	col := newCollector(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.WriteHeader(http.StatusBadGateway)
	})
	// The first backoff (at least 0.25 s) is beyond a 100 ms timeout.
	h := newHarness(t, col, map[string]string{"OTEL_EXPORTER_OTLP_TIMEOUT": "100"}, options{})
	h.logN(2, "m")
	h.flush(t)
	if n := col.requests.Load(); n != 1 {
		t.Fatalf("%d requests, want 1", n)
	}
	wantCounts(t, h.e, Counts{Failed: 2})
}

func TestNoRetryOnOtherStatuses(t *testing.T) {
	for _, status := range []int{400, 401, 404, 413, 500} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			col := newCollector(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				io.WriteString(w, `{"code":3,"message":"bad data in record 2"}`)
			})
			h := newHarness(t, col, nil, options{})
			h.logN(3, "m")
			h.flush(t)
			if n := col.requests.Load(); n != 1 {
				t.Fatalf("%d requests, want 1", n)
			}
			wantCounts(t, h.e, Counts{Failed: 3})
			reports := h.reports.reports(t)
			if len(reports) != 1 {
				t.Fatalf("reports %v, want 1", reports)
			}
			// The collector's message is never reported (Logs: no remote text).
			want := fmt.Sprintf("collector answered %d %s", status, http.StatusText(status))
			if r := reports[0]; r["http.response.status_code"] != float64(status) || r["exception.message"] != want ||
				r["kaiak.log_export.failed"] != float64(3) || r["kaiak.log_export.dropped"] != float64(0) {
				t.Fatalf("report %v, want status %d, message %q, 3 failed, 0 dropped", r, status, want)
			}
		})
	}
}

func TestPartialSuccessCountsRejectedAsFailed(t *testing.T) {
	for _, tc := range []struct {
		name, body       string
		exported, failed uint64
	}{
		{"rejected as a string", `{"partialSuccess":{"rejectedLogRecords":"2","errorMessage":"too large"}}`, 3, 2},
		{"rejected as a number", `{"partialSuccess":{"rejectedLogRecords":1}}`, 4, 1},
		{"more rejected than sent", `{"partialSuccess":{"rejectedLogRecords":"9"}}`, 0, 5},
		{"a warning only", `{"partialSuccess":{"errorMessage":"deprecated field"}}`, 5, 0},
		{"no body", ``, 5, 0},
		{"not JSON: unreadable", `ok`, 0, 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			col := newCollector(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
				io.WriteString(w, tc.body)
			})
			h := newHarness(t, col, nil, options{})
			h.logN(5, "m")
			h.flush(t)
			if n := col.requests.Load(); n != 1 {
				t.Fatalf("%d requests, want 1: a partial success is not retried", n)
			}
			wantCounts(t, h.e, Counts{Exported: tc.exported, Failed: tc.failed})
		})
	}
}

func TestPartialSuccessIsReported(t *testing.T) {
	col := newCollector(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		io.WriteString(w, `{"partialSuccess":{"rejectedLogRecords":"2","errorMessage":"too large"}}`)
	})
	h := newHarness(t, col, nil, options{})
	h.logN(5, "m")
	h.flush(t)
	reports := h.reports.reports(t)
	if len(reports) != 1 || reports[0]["exception.message"] != "collector rejected 2 records" ||
		reports[0]["http.response.status_code"] != float64(200) || reports[0]["kaiak.log_export.failed"] != float64(2) {
		t.Fatalf("reports %v, want one with the rejection", reports)
	}
}

func TestOversizedResponseFailsUnretried(t *testing.T) {
	for _, status := range []int{200, 503} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			col := newCollector(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
				w.WriteHeader(status)
				w.Write(bytes.Repeat([]byte(" "), maxResponseSize+1))
			})
			h := newHarness(t, col, nil, options{})
			h.logN(2, "m")
			h.flush(t)
			if n := col.requests.Load(); n != 1 {
				t.Fatalf("%d requests, want 1", n)
			}
			wantCounts(t, h.e, Counts{Failed: 2})
		})
	}
}

func TestFullQueueDropsTheNewest(t *testing.T) {
	col := newCollector(t, nil)
	h := newHarness(t, col, nil, options{capacity: 4})
	h.logN(10, "m")
	wantCounts(t, h.e, Counts{Dropped: 6})
	h.flush(t)
	if r := col.next(t); !reflect.DeepEqual(r.messages, messages("m", 0, 4)) {
		t.Fatalf("sent %q, want the oldest four", r.messages)
	}
	wantCounts(t, h.e, Counts{Exported: 4, Dropped: 6})
	reports := h.reports.reports(t)
	if len(reports) != 1 || reports[0]["kaiak.log_export.dropped"] != float64(6) || reports[0]["kaiak.log_export.failed"] != float64(0) {
		t.Fatalf("reports %v, want one with 6 dropped", reports)
	}
	if _, ok := reports[0]["http.response.status_code"]; ok {
		t.Fatalf("report %v carries a status, want none: nothing failed", reports[0])
	}
}

// stalledCollector answers no request until released or the request is cut.
func stalledCollector(t *testing.T) (*collector, func()) {
	release := make(chan struct{})
	var once sync.Once
	col := newCollector(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	releaseAll := func() { once.Do(func() { close(release) }) }
	t.Cleanup(releaseAll)
	return col, releaseAll
}

func TestStalledCollectorNeverBlocksHandle(t *testing.T) {
	col, _ := stalledCollector(t)
	h := newHarness(t, col, nil, options{capacity: 4})
	h.logN(4, "a")
	h.tick <- time.Now()
	col.next(t) // the sender is now stuck in the export

	logged := make(chan struct{})
	go func() {
		defer close(logged)
		for range 1000 {
			h.log.Info("b")
		}
	}()
	select {
	case <-logged:
	case <-time.After(10 * time.Second):
		t.Fatal("logging blocked behind a stalled collector")
	}
	wantCounts(t, h.e, Counts{Dropped: 996})
}

func TestFlushDeliversWhatIsQueued(t *testing.T) {
	col := newCollector(t, nil)
	h := newHarness(t, col, nil, options{})
	h.logN(600, "m")
	h.flush(t)
	wantCounts(t, h.e, Counts{Exported: 600})
	var got []string
	for len(got) < 600 {
		got = append(got, col.next(t).messages...)
	}
	if !reflect.DeepEqual(got, messages("m", 0, 600)) {
		t.Fatal("records arrived out of order or not all")
	}
}

func TestFlushEndsWithItsDeadlineAndCloseDrops(t *testing.T) {
	col, _ := stalledCollector(t)
	h := newHarness(t, col, nil, options{})
	h.logN(3, "a")
	h.tick <- time.Now()
	col.next(t) // three records in an export that never ends
	h.logN(5, "b")

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := h.e.Flush(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Flush = %v, want the deadline", err)
	}
	h.e.Close()
	wantCounts(t, h.e, Counts{Failed: 3, Dropped: 5})
	// The report at exit is never held back: the drops at Close get their own
	// line within the minute of the cut export's.
	reports := h.reports.reports(t)
	if len(reports) != 2 || reports[0]["kaiak.log_export.failed"] != float64(3) || reports[0]["kaiak.log_export.dropped"] != float64(0) ||
		reports[1]["kaiak.log_export.failed"] != float64(0) || reports[1]["kaiak.log_export.dropped"] != float64(5) {
		t.Fatalf("reports %v, want two: the cut export's 3 failed, then Close's 5 dropped", reports)
	}
}

func TestAfterClose(t *testing.T) {
	col := newCollector(t, nil)
	var next bytes.Buffer
	h := newHarness(t, col, nil, options{})
	l := slog.New(h.e.Handler(slog.NewJSONHandler(&next, nil)))
	h.e.Close()
	h.e.Close()
	l.Info("after close")
	if !strings.Contains(next.String(), "after close") {
		t.Fatal("the next handler missed a record logged after Close")
	}
	wantCounts(t, h.e, Counts{Dropped: 1})
	if err := h.e.Flush(context.Background()); !errors.Is(err, errClosed) {
		t.Fatalf("Flush after Close = %v, want errClosed", err)
	}
	col.none(t)
}

func TestProblemReportsAreRateLimited(t *testing.T) {
	col := newCollector(t, func(w http.ResponseWriter, _ *http.Request, n int) {
		if n == 3 {
			w.Header().Set("Retry-After", "60")
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusBadRequest)
	})
	h := newHarness(t, col, nil, options{})

	h.logN(1, "a")
	h.flush(t)
	if r := h.reports.reports(t); len(r) != 1 || r[0]["kaiak.log_export.failed"] != float64(1) {
		t.Fatalf("reports %v, want one at the first failure", r)
	}

	h.clock.Advance(30 * time.Second)
	h.logN(2, "b")
	h.flush(t)
	h.clock.Advance(29 * time.Second)
	h.logN(3, "c")
	h.flush(t)
	if r := h.reports.reports(t); len(r) != 1 {
		t.Fatalf("reports %v, want still one within the minute", r)
	}

	h.clock.Advance(time.Second)
	h.logN(4, "d")
	h.flush(t)
	r := h.reports.reports(t)
	if len(r) != 2 {
		t.Fatalf("reports %v, want a second one a minute after the first", r)
	}
	if r[1]["kaiak.log_export.failed"] != float64(9) || r[1]["http.response.status_code"] != float64(503) ||
		!strings.HasPrefix(r[1]["exception.message"].(string), "collector answered 503") {
		t.Fatalf("second report %v, want the 9 failed since the first and the last status and error", r[1])
	}

	h.clock.Advance(2 * time.Minute)
	col2 := col.requests.Load()
	h.flush(t)
	if r := h.reports.reports(t); len(r) != 2 || col.requests.Load() != col2 {
		t.Fatalf("reports %v, want no line without new problems", r)
	}
}

// A redirect is not followed: neither the batch nor a configured header reaches
// the target — here another host name, which Go's redirect rule would send a
// custom credential header to — and the batch fails at once, reported by status.
func TestRedirectIsNotFollowed(t *testing.T) {
	reached := make(chan string, 10)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached <- r.Header.Get("X-Api-Key")
		io.WriteString(w, "{}")
	}))
	defer target.Close()
	destination := strings.Replace(target.URL, "127.0.0.1", "localhost", 1)
	for _, status := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			col := newCollector(t, func(w http.ResponseWriter, r *http.Request, _ int) {
				http.Redirect(w, r, destination, status)
			})
			h := newHarness(t, col, map[string]string{"OTEL_EXPORTER_OTLP_HEADERS": "x-api-key=test-secret"}, options{})
			h.logN(2, "m")
			h.flush(t)
			select {
			case key := <-reached:
				t.Fatalf("the redirect's target was reached (x-api-key %q)", key)
			default:
			}
			if n := col.requests.Load(); n != 1 {
				t.Fatalf("%d requests, want 1: a redirect is not retried", n)
			}
			wantCounts(t, h.e, Counts{Failed: 2})
			reports := h.reports.reports(t)
			if len(reports) != 1 || reports[0]["http.response.status_code"] != float64(status) {
				t.Fatalf("reports %v, want one with status %d", reports, status)
			}
		})
	}
}

// A 302 to a login page answering 200 is not a delivery: nothing reached the
// collector.
func TestRedirectToAPageAnsweringOKIsNotExported(t *testing.T) {
	login := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, "<html><body>Sign in</body></html>")
	}))
	defer login.Close()
	col := newCollector(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		http.Redirect(w, r, login.URL+"/login", http.StatusFound)
	})
	h := newHarness(t, col, nil, options{})
	h.logN(3, "m")
	h.flush(t)
	wantCounts(t, h.e, Counts{Failed: 3})
	reports := h.reports.reports(t)
	if len(reports) != 1 || reports[0]["http.response.status_code"] != float64(http.StatusFound) {
		t.Fatalf("reports %v, want one with status 302", reports)
	}
}

// The collector's own text — a Status message, a partial success's errorMessage —
// never reaches a report: an auth proxy may echo the credential in it.
func TestCollectorTextIsNeverReported(t *testing.T) {
	for _, tc := range []struct {
		name    string
		partial bool
		want    string
	}{
		{"failure status", false, "collector answered 401 Unauthorized"},
		{"partial success", true, "collector rejected 1 records"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			col := newCollector(t, func(w http.ResponseWriter, r *http.Request, _ int) {
				message := "invalid credential: " + r.Header.Get("Authorization")
				if tc.partial {
					json.NewEncoder(w).Encode(map[string]any{"partialSuccess": map[string]any{
						"rejectedLogRecords": "1", "errorMessage": message,
					}})
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				json.NewEncoder(w).Encode(map[string]any{"code": 16, "message": message})
			})
			h := newHarness(t, col, map[string]string{"OTEL_EXPORTER_OTLP_HEADERS": "authorization=Bearer%20test-secret"}, options{})
			h.logN(2, "m")
			h.flush(t)
			reports := h.reports.reports(t)
			if len(reports) != 1 {
				t.Fatalf("reports %v, want 1", reports)
			}
			if got := reports[0]["exception.message"]; got != tc.want {
				t.Fatalf("exception.message %q, want %q: never the collector's text", got, tc.want)
			}
		})
	}
}

// A transport failure is reported in the gateway's own words: Go's error quotes
// the bytes it could not parse — here a header line carrying a credential.
func TestTransportErrorTextIsNeverReported(t *testing.T) {
	col := newCollector(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		defer conn.Close()
		buf.WriteString("HTTP/1.1 200 OK\r\nBearer test-secret\r\n\r\n")
		buf.Flush()
	})
	// The first backoff (at least 0.25 s) is beyond the timeout: one attempt.
	h := newHarness(t, col, map[string]string{"OTEL_EXPORTER_OTLP_LOGS_TIMEOUT": "100"}, options{})
	h.logN(1, "m")
	h.flush(t)
	wantCounts(t, h.e, Counts{Failed: 1})
	reports := h.reports.reports(t)
	if len(reports) != 1 {
		t.Fatalf("reports %v, want 1", reports)
	}
	if msg, _ := reports[0]["exception.message"].(string); strings.Contains(msg, "test-secret") || strings.Contains(msg, "Bearer") {
		t.Fatalf("exception.message %q carries the bytes the collector sent", msg)
	}
	if _, ok := reports[0]["http.response.status_code"]; ok {
		t.Fatalf("report %v carries a status: no answer was read", reports[0])
	}
}

// A 2xx counts as delivered only when its body is empty or an
// ExportLogsServiceResponse; one that cannot be read, or is something else, fails
// the whole batch unretried: the collector may have taken some of it.
func TestUnreadableAnswerFailsUnretried(t *testing.T) {
	for _, tc := range []struct {
		name    string
		respond func(w http.ResponseWriter)
	}{
		{"truncated partial success", func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Content-Length", "1000")
			io.WriteString(w, `{"partialSuccess":{"rejectedLogRecords":"1"`)
		}},
		{"a login page", func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "text/html")
			io.WriteString(w, "<html><body>Sign in</body></html>")
		}},
		{"not JSON", func(w http.ResponseWriter) { io.WriteString(w, "ok") }},
		{"a JSON array", func(w http.ResponseWriter) { io.WriteString(w, "[]") }},
		{"JSON null", func(w http.ResponseWriter) { io.WriteString(w, "null") }},
		{"partialSuccess not an object", func(w http.ResponseWriter) { io.WriteString(w, `{"partialSuccess":"none"}`) }},
		{"rejectedLogRecords not an integer", func(w http.ResponseWriter) {
			io.WriteString(w, `{"partialSuccess":{"rejectedLogRecords":true}}`)
		}},
		{"rejectedLogRecords a fraction", func(w http.ResponseWriter) {
			io.WriteString(w, `{"partialSuccess":{"rejectedLogRecords":"1.5"}}`)
		}},
		{"errorMessage not a string", func(w http.ResponseWriter) {
			io.WriteString(w, `{"partialSuccess":{"rejectedLogRecords":"1","errorMessage":7}}`)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			col := newCollector(t, func(w http.ResponseWriter, _ *http.Request, _ int) { tc.respond(w) })
			h := newHarness(t, col, nil, options{})
			h.logN(3, "m")
			h.flush(t)
			if n := col.requests.Load(); n != 1 {
				t.Fatalf("%d requests, want 1: an unreadable answer is not retried", n)
			}
			wantCounts(t, h.e, Counts{Failed: 3})
			reports := h.reports.reports(t)
			if len(reports) != 1 || reports[0]["http.response.status_code"] != float64(200) {
				t.Fatalf("reports %v, want one with status 200", reports)
			}
		})
	}
}

// An ExportLogsServiceResponse without rejections delivers the batch, whatever
// members OTLP does not define it carries.
func TestAnswersThatDeliver(t *testing.T) {
	for _, body := range []string{``, "\n", `{}`, `{"partialSuccess":null}`, `{"partialSuccess":{}}`,
		`{"partialSuccess":{"rejectedLogRecords":null,"errorMessage":null}}`, `{"extra":[1,{"x":2}]}`,
		`{"partialSuccess":{"rejectedLogRecords":"0","errorMessage":"","extra":true}}`} {
		t.Run(body, func(t *testing.T) {
			col := newCollector(t, func(w http.ResponseWriter, _ *http.Request, _ int) { io.WriteString(w, body) })
			h := newHarness(t, col, nil, options{})
			h.logN(2, "m")
			h.flush(t)
			wantCounts(t, h.e, Counts{Exported: 2})
			if r := h.reports.reports(t); len(r) != 0 {
				t.Fatalf("reports %v, want none", r)
			}
		})
	}
}

// A Retry-After beyond any batch's timeout — two days, or more than a duration
// holds — fails the batch without a retry: it saturates, never reads as absent.
func TestLongRetryAfterFailsTheBatch(t *testing.T) {
	for _, header := range []string{"172800", "99999999999999999999"} {
		t.Run(header, func(t *testing.T) {
			col := newCollector(t, func(w http.ResponseWriter, _ *http.Request, n int) {
				if n == 0 {
					w.Header().Set("Retry-After", header)
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				io.WriteString(w, "{}")
			})
			h := newHarness(t, col, nil, options{})
			h.logN(1, "m")
			h.flush(t)
			if n := col.requests.Load(); n != 1 {
				t.Fatalf("%d requests (waits %v), want 1: the batch fails without a retry", n, h.recordedWaits())
			}
			wantCounts(t, h.e, Counts{Failed: 1})
			if got := retryAfter(header, time.Now()); got < 172800*time.Second {
				t.Fatalf("retryAfter(%q) = %v, want at least two days", header, got)
			}
		})
	}
}

// Retry-After: 0, or a date already past, still waits the backoff: the wait is
// max(Retry-After, backoff).
func TestRetryAfterBelowTheBackoffWaitsTheBackoff(t *testing.T) {
	for name, header := range map[string]func() string{
		"zero":        func() string { return "0" },
		"a past date": func() string { return time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat) },
	} {
		t.Run(name, func(t *testing.T) {
			col := newCollector(t, func(w http.ResponseWriter, _ *http.Request, n int) {
				if n < 3 {
					w.Header().Set("Retry-After", header())
					w.WriteHeader(http.StatusTooManyRequests)
					return
				}
				io.WriteString(w, "{}")
			})
			h := newHarness(t, col, nil, options{})
			h.logN(1, "m")
			h.flush(t)
			waits := h.recordedWaits()
			if len(waits) != 3 {
				t.Fatalf("waits %v, want 3", waits)
			}
			for i, base := range []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second} {
				if waits[i] < base/2 || waits[i] > base {
					t.Errorf("wait %d = %s, want the backoff, within [%s, %s]", i, waits[i], base/2, base)
				}
			}
			wantCounts(t, h.e, Counts{Exported: 1})
		})
	}
}
