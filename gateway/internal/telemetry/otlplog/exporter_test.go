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
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"kaiak/internal/telemetry/fakeotlp"
	"kaiak/internal/telemetry/otlp"
)

func envOf(vars map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		v, ok := vars[name]
		return v, ok
	}
}

func mustSettings(t *testing.T, vars map[string]string) *otlp.Settings {
	t.Helper()
	s, err := otlp.ReadSettings(otlp.Logs, envOf(vars))
	if err != nil {
		t.Fatalf("ReadSettings: %v", err)
	}
	if s == nil {
		t.Fatal("ReadSettings: export off, want on")
	}
	return s
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

// harness is an exporter posting to a fake collector, with a manual tick and its
// reports in a buffer.
type harness struct {
	e       *Exporter
	col     *fakeotlp.Collector
	tick    chan time.Time
	log     *slog.Logger
	reports *syncBuffer
	clock   *fakeClock
}

func newHarness(t *testing.T, col *fakeotlp.Collector, vars map[string]string, opts options) *harness {
	t.Helper()
	if vars == nil {
		vars = map[string]string{}
	}
	vars["OTEL_EXPORTER_OTLP_LOGS_ENDPOINT"] = col.URL + "/v1/logs"
	h := &harness{col: col, tick: make(chan time.Time), reports: &syncBuffer{},
		clock: &fakeClock{now: time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)}}
	opts.tick = h.tick
	opts.now = h.clock.Now
	h.e = newExporter(mustSettings(t, vars), otlp.Service{Version: "1.2.3", InstanceID: "gw-1"},
		slog.New(slog.NewJSONHandler(h.reports, nil)), opts)
	t.Cleanup(func() { h.e.Shutdown(context.Background()) })
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
	if err := h.e.ForceFlush(ctx); err != nil {
		t.Fatalf("ForceFlush: %v", err)
	}
}

// wantCounts compares an exporter's counts whole.
func wantCounts(t *testing.T, e *Exporter, want Counts) {
	t.Helper()
	if got := e.Counts(); !reflect.DeepEqual(got, want) {
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
	col := fakeotlp.New(t, nil)
	h := newHarness(t, col, map[string]string{
		"OTEL_EXPORTER_OTLP_HEADERS": "authorization=Bearer%20abc,x-scope-orgid=tenant-1",
	}, options{})
	h.log.Info("one", "kaiak.key.id", "k1")
	h.tick <- time.Now()
	r := col.Next(t)
	for name, want := range map[string]string{
		"Content-Type":  "application/json",
		"User-Agent":    "kaiak/1.2.3",
		"Authorization": "Bearer abc",
		"X-Scope-Orgid": "tenant-1",
	} {
		if got := r.Header.Get(name); got != want {
			t.Errorf("header %s = %q, want %q", name, got, want)
		}
	}
	rl := r.Export.ResourceLogs[0]
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
	if !reflect.DeepEqual(r.Messages(), []string{"one"}) {
		t.Errorf("messages %q, want [one]", r.Messages())
	}
}

func TestBatchBySize(t *testing.T) {
	col := fakeotlp.New(t, nil)
	done := make(chan struct{}, 10)
	h := newHarness(t, col, nil, options{batchDone: func() { done <- struct{}{} }})
	h.logN(batchSize+1, "m")

	first := col.Next(t)
	if !reflect.DeepEqual(first.Messages(), messages("m", 0, batchSize)) {
		t.Fatalf("first batch: %d records, want the oldest %d", len(first.Messages()), batchSize)
	}
	<-done
	col.None(t)
	if n := len(queued(h.e)); n != 1 {
		t.Fatalf("%d records queued after the full batch, want 1 waiting for the interval", n)
	}
	h.tick <- time.Now()
	if second := col.Next(t); !reflect.DeepEqual(second.Messages(), []string{fmt.Sprintf("m%d", batchSize)}) {
		t.Fatalf("second batch %q, want the one left", second.Messages())
	}
	<-done
	wantCounts(t, h.e, Counts{Handed: batchSize + 1, Exported: batchSize + 1})
}

func TestBatchByInterval(t *testing.T) {
	col := fakeotlp.New(t, nil)
	h := newHarness(t, col, nil, options{})
	h.logN(3, "m")
	h.tick <- time.Now()
	if r := col.Next(t); !reflect.DeepEqual(r.Messages(), messages("m", 0, 3)) {
		t.Fatalf("batch %q, want m0..m2", r.Messages())
	}
	h.flush(t)
	wantCounts(t, h.e, Counts{Handed: 3, Exported: 3})
	if r := h.reports.reports(t); len(r) != 0 {
		t.Errorf("reports %v, want none: the batch was delivered", r)
	}
}

func TestNoRetryOnOtherStatuses(t *testing.T) {
	for _, status := range []int{400, 401, 404, 413, 500} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			col := fakeotlp.New(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				io.WriteString(w, `{"code":3,"message":"bad data in record 2"}`)
			})
			h := newHarness(t, col, nil, options{})
			h.logN(3, "m")
			h.flush(t)
			if n := col.Requests(); n != 1 {
				t.Fatalf("%d requests, want 1", n)
			}
			wantCounts(t, h.e, Counts{Handed: 3, Failed: map[string]uint64{strconv.Itoa(status): 3}})
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
		name, body string
		exported   uint64
		failed     map[string]uint64
	}{
		{"rejected as a string", `{"partialSuccess":{"rejectedLogRecords":"2","errorMessage":"too large"}}`, 3,
			map[string]uint64{"rejected": 2}},
		{"rejected as a number", `{"partialSuccess":{"rejectedLogRecords":1}}`, 4, map[string]uint64{"rejected": 1}},
		{"more rejected than sent", `{"partialSuccess":{"rejectedLogRecords":"9"}}`, 0, map[string]uint64{"rejected": 5}},
		{"a warning only", `{"partialSuccess":{"errorMessage":"deprecated field"}}`, 5, nil},
		{"no body", ``, 5, nil},
		{"not JSON: unreadable", `ok`, 0, map[string]uint64{"malformed_response": 5}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			col := fakeotlp.New(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
				io.WriteString(w, tc.body)
			})
			h := newHarness(t, col, nil, options{})
			h.logN(5, "m")
			h.flush(t)
			if n := col.Requests(); n != 1 {
				t.Fatalf("%d requests, want 1: a partial success is not retried", n)
			}
			wantCounts(t, h.e, Counts{Handed: 5, Exported: tc.exported, Failed: tc.failed})
		})
	}
}

func TestPartialSuccessIsReported(t *testing.T) {
	col := fakeotlp.New(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
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

func TestFullQueueDropsTheNewest(t *testing.T) {
	col := fakeotlp.New(t, nil)
	h := newHarness(t, col, nil, options{capacity: 4})
	h.logN(10, "m")
	wantCounts(t, h.e, Counts{QueueFull: 6})
	h.flush(t)
	if r := col.Next(t); !reflect.DeepEqual(r.Messages(), messages("m", 0, 4)) {
		t.Fatalf("sent %q, want the oldest four", r.Messages())
	}
	wantCounts(t, h.e, Counts{Handed: 4, Exported: 4, QueueFull: 6})
	reports := h.reports.reports(t)
	if len(reports) != 1 || reports[0]["kaiak.log_export.dropped"] != float64(6) || reports[0]["kaiak.log_export.failed"] != float64(0) {
		t.Fatalf("reports %v, want one with 6 dropped", reports)
	}
	if _, ok := reports[0]["http.response.status_code"]; ok {
		t.Fatalf("report %v carries a status, want none: nothing failed", reports[0])
	}
}

// stalledCollector answers no request until released or the request is cut.
func stalledCollector(t *testing.T) (*fakeotlp.Collector, func()) {
	release := make(chan struct{})
	var once sync.Once
	col := fakeotlp.New(t, func(w http.ResponseWriter, r *http.Request, _ int) {
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
	col.Next(t) // the sender is now stuck in the export

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
	wantCounts(t, h.e, Counts{Handed: 4, QueueFull: 996})
}

func TestForceFlushDeliversWhatIsQueued(t *testing.T) {
	col := fakeotlp.New(t, nil)
	h := newHarness(t, col, nil, options{})
	h.logN(600, "m")
	h.flush(t)
	wantCounts(t, h.e, Counts{Handed: 600, Exported: 600})
	var got []string
	for len(got) < 600 {
		got = append(got, col.Next(t).Messages()...)
	}
	if !reflect.DeepEqual(got, messages("m", 0, 600)) {
		t.Fatal("records arrived out of order or not all")
	}
}

func TestForceFlushEndsWithItsDeadlineAndShutdownDrops(t *testing.T) {
	col, _ := stalledCollector(t)
	h := newHarness(t, col, nil, options{})
	h.logN(3, "a")
	h.tick <- time.Now()
	col.Next(t) // three records in an export that never ends
	h.logN(5, "b")

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := h.e.ForceFlush(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ForceFlush = %v, want the deadline", err)
	}
	h.e.Shutdown(context.Background())
	// The export Shutdown cut ran out of time at exit: timeout.
	wantCounts(t, h.e, Counts{Handed: 3, Failed: map[string]uint64{"timeout": 3}, Shutdown: 5})
	// The report at exit is never held back: the drops at Shutdown get their own
	// line within the minute of the cut export's.
	reports := h.reports.reports(t)
	if len(reports) != 2 || reports[0]["kaiak.log_export.failed"] != float64(3) || reports[0]["kaiak.log_export.dropped"] != float64(0) ||
		reports[1]["kaiak.log_export.failed"] != float64(0) || reports[1]["kaiak.log_export.dropped"] != float64(5) {
		t.Fatalf("reports %v, want two: the cut export's 3 failed, then Shutdown's 5 dropped", reports)
	}
}

func TestAfterShutdown(t *testing.T) {
	col := fakeotlp.New(t, nil)
	var next bytes.Buffer
	h := newHarness(t, col, nil, options{})
	l := slog.New(h.e.Handler(slog.NewJSONHandler(&next, nil)))
	h.e.Shutdown(context.Background())
	h.e.Shutdown(context.Background())
	l.Info("after shutdown")
	if !strings.Contains(next.String(), "after shutdown") {
		t.Fatal("the next handler missed a record logged after Shutdown")
	}
	wantCounts(t, h.e, Counts{Shutdown: 1})
	if err := h.e.ForceFlush(context.Background()); !errors.Is(err, errShutdown) {
		t.Fatalf("ForceFlush after Shutdown = %v, want errShutdown", err)
	}
	col.None(t)
}

func TestProblemReportsAreRateLimited(t *testing.T) {
	col := fakeotlp.New(t, func(w http.ResponseWriter, _ *http.Request, n int) {
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
	col2 := col.Requests()
	h.flush(t)
	if r := h.reports.reports(t); len(r) != 2 || col.Requests() != col2 {
		t.Fatalf("reports %v, want no line without new problems", r)
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
			col := fakeotlp.New(t, func(w http.ResponseWriter, r *http.Request, _ int) {
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
	col := fakeotlp.New(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
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
	wantCounts(t, h.e, Counts{Handed: 1, Failed: map[string]uint64{"malformed_response": 1}})
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
