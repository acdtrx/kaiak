package otlptrace

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
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

// reports decodes the `trace export failing` lines written so far.
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
		if m["msg"] != "trace export failing" || m["level"] != "WARN" {
			t.Fatalf("report line %q: want a WARN `trace export failing`", line)
		}
		lines = append(lines, m)
	}
	return lines
}

// releases counts the release calls of each span, by name.
type releases struct {
	mu sync.Mutex
	n  map[string]int
}

func (r *releases) of(name string) func() {
	return func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.n[name]++
	}
}

// want fails the test unless each of names was released n times, and no other span
// was released.
func (r *releases) want(t *testing.T, names []string, n int) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	want := map[string]int{}
	if n > 0 {
		for _, name := range names {
			want[name] = n
		}
	}
	if !reflect.DeepEqual(r.n, want) {
		t.Fatalf("releases %v, want %v", r.n, want)
	}
}

// harness is an exporter posting to a fake collector, with a manual tick, its
// reports in a buffer and its spans' releases counted.
type harness struct {
	e        *Exporter
	col      *fakeotlp.Collector
	tick     chan time.Time
	reports  *syncBuffer
	released *releases
}

func newHarness(t *testing.T, col *fakeotlp.Collector, vars map[string]string, opts options) *harness {
	t.Helper()
	if vars == nil {
		vars = map[string]string{}
	}
	vars["OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"] = col.URL + "/v1/traces"
	s, err := otlp.ReadSettings(otlp.Traces, envOf(vars))
	if err != nil || s == nil {
		t.Fatalf("ReadSettings = %v, %v; want trace export on", s, err)
	}
	h := &harness{col: col, tick: make(chan time.Time), reports: &syncBuffer{}, released: &releases{n: map[string]int{}}}
	opts.tick = h.tick
	h.e = newExporter(s, otlp.Service{Version: "1.2.3", InstanceID: "gw-1"},
		slog.New(slog.NewJSONHandler(h.reports, nil)), opts)
	t.Cleanup(func() { h.e.Shutdown(context.Background()) })
	return h
}

// encodeSpan is a span named name, its encoding exactly size bytes long — padded
// with an attribute — or as short as it gets when size is 0.
func encodeSpan(t *testing.T, name string, size int) []byte {
	t.Helper()
	span := func(pad int) []byte {
		b, err := Encode(Span{TraceID: NewTraceID(), SpanID: NewSpanID(), Name: name, Kind: KindClient,
			Start: testStart, End: testStart.Add(time.Second),
			Attributes: []Attribute{String("pad", strings.Repeat("x", pad))}})
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	b := span(0)
	if size == 0 {
		return b
	}
	if size < len(b) {
		t.Fatalf("a span of %d bytes: at least %d", size, len(b))
	}
	if b = span(size - len(b)); len(b) != size {
		t.Fatalf("span of %d bytes, want %d", len(b), size)
	}
	return b
}

// enqueue queues a span of size bytes (0: the shortest) named name, its release
// counted.
func (h *harness) enqueue(t *testing.T, name string, size int) {
	t.Helper()
	h.e.Enqueue(encodeSpan(t, name, size), h.released.of(name))
}

func (h *harness) enqueueN(t *testing.T, n int, prefix string) []string {
	t.Helper()
	names := spanNames(prefix, 0, n)
	for _, name := range names {
		h.enqueue(t, name, 0)
	}
	return names
}

func (h *harness) flush(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := h.e.ForceFlush(ctx); err != nil {
		t.Fatalf("ForceFlush: %v", err)
	}
}

func spanNames(prefix string, from, to int) []string {
	var names []string
	for i := from; i < to; i++ {
		names = append(names, fmt.Sprintf("%s%d", prefix, i))
	}
	return names
}

// names are the names of an export's spans, in order.
func names(r fakeotlp.Received) []string {
	var out []string
	for _, s := range r.Spans() {
		out = append(out, s.Name)
	}
	return out
}

// wantCounts compares an exporter's counts whole.
func wantCounts(t *testing.T, e *Exporter, want Counts) {
	t.Helper()
	if got := e.Counts(); !reflect.DeepEqual(got, want) {
		t.Fatalf("counts %+v, want %+v", got, want)
	}
}

// sizes records the body size of each request a collector received.
type sizes struct {
	mu sync.Mutex
	n  []int64
}

// answer is a Respond that records the request's size, then answers 200 with an
// empty export response.
func (s *sizes) answer(w http.ResponseWriter, r *http.Request, _ int) {
	s.mu.Lock()
	s.n = append(s.n, r.ContentLength)
	s.mu.Unlock()
	io.WriteString(w, "{}")
}

func (s *sizes) all() []int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]int64(nil), s.n...)
}

func TestExportRequestShape(t *testing.T) {
	col := fakeotlp.New(t, nil)
	h := newHarness(t, col, map[string]string{
		"OTEL_EXPORTER_OTLP_TRACES_HEADERS": "authorization=Bearer%20abc,x-langfuse-ingestion-version=4",
	}, options{})
	traceID, spanID := NewTraceID(), NewSpanID()
	failed, err := Encode(Span{TraceID: traceID, SpanID: spanID, Name: "chat gpt-4o", Kind: KindClient,
		Start: testStart, End: testStart.Add(1500 * time.Millisecond),
		Attributes: []Attribute{String("gen_ai.operation.name", "chat"), Int("gen_ai.usage.input_tokens", 12),
			Float64("kaiak.usage.cost_usd", 0.5), Bool("kaiak.usage.estimated", true),
			Strings("gen_ai.response.finish_reasons", []string{"stop"})},
		Status: Status{Code: StatusError, Message: "upstream_error"}})
	if err != nil {
		t.Fatal(err)
	}
	h.e.Enqueue(failed, h.released.of("failed"))
	h.enqueue(t, "ok", 0)
	h.tick <- time.Now()
	r := col.Next(t)

	if r.Path != "/v1/traces" {
		t.Errorf("path %q, want /v1/traces", r.Path)
	}
	for name, want := range map[string]string{
		"Content-Type":                 "application/json",
		"User-Agent":                   "kaiak/1.2.3",
		"Authorization":                "Bearer abc",
		"X-Langfuse-Ingestion-Version": "4",
	} {
		if got := r.Header.Get(name); got != want {
			t.Errorf("header %s = %q, want %q", name, got, want)
		}
	}
	if len(r.Export.ResourceSpans) != 1 || len(r.Export.ResourceSpans[0].ScopeSpans) != 1 {
		t.Fatalf("export %+v, want one resource with one scope", r.Export)
	}
	rs := r.Export.ResourceSpans[0]
	var res []string
	for _, kv := range rs.Resource.Attributes {
		res = append(res, kv.Key+"="+*kv.Value.StringValue)
	}
	if want := []string{"service.name=kaiak", "service.version=1.2.3", "service.instance.id=gw-1"}; !reflect.DeepEqual(res, want) {
		t.Errorf("resource %q, want %q", res, want)
	}
	if rs.ScopeSpans[0].Scope.Name != "kaiak" {
		t.Errorf("scope %q, want kaiak", rs.ScopeSpans[0].Scope.Name)
	}

	spans := r.Spans()
	if len(spans) != 2 {
		t.Fatalf("%d spans, want 2", len(spans))
	}
	s := spans[0]
	if s.TraceID != fmt.Sprintf("%x", traceID[:]) || s.SpanID != fmt.Sprintf("%x", spanID[:]) || len(s.TraceID) != 32 || len(s.SpanID) != 16 {
		t.Errorf("IDs %s, %s; want %x, %x in lowercase hex", s.TraceID, s.SpanID, traceID, spanID)
	}
	if s.Name != "chat gpt-4o" || s.Kind != 3 || s.StartTimeUnixNano != strconv.FormatInt(testStart.UnixNano(), 10) ||
		s.EndTimeUnixNano != strconv.FormatInt(testStart.Add(1500*time.Millisecond).UnixNano(), 10) {
		t.Errorf("span %+v", s)
	}
	if want := map[string]any{"gen_ai.operation.name": "chat", "gen_ai.usage.input_tokens": int64(12), "kaiak.usage.cost_usd": 0.5,
		"kaiak.usage.estimated": true, "gen_ai.response.finish_reasons": []any{"stop"}}; !reflect.DeepEqual(s.Attrs(t), want) {
		t.Errorf("attributes %v, want %v", s.Attrs(t), want)
	}
	if s.Status == nil || *s.Status != (fakeotlp.Status{Code: 2, Message: "upstream_error"}) {
		t.Errorf("status %+v, want code 2 with the error type", s.Status)
	}
	if spans[1].Status != nil {
		t.Errorf("status %+v on a span with no error, want none", spans[1].Status)
	}
	h.flush(t)
	wantCounts(t, h.e, Counts{Handed: 2, Exported: 2})
	h.released.want(t, []string{"failed", "ok"}, 1)
}

// A full request — its spans reach the export limit — goes at once; a partial one
// waits for the tick.
func TestFullRequestGoesAtOnce(t *testing.T) {
	sz := &sizes{}
	col := fakeotlp.New(t, sz.answer)
	done := make(chan struct{}, 10)
	const limit = 4096
	h := newHarness(t, col, nil, options{maxRequestSize: limit, requestDone: func() { done <- struct{}{} }})
	// Two spans fill a request exactly: their sizes and a comma are the room.
	room := h.e.MaxSpanBytes()
	small, large := (room-1)/2, room-1-(room-1)/2

	h.enqueue(t, "a", small)
	col.None(t)
	h.enqueue(t, "b", large)
	if r := col.Next(t); !reflect.DeepEqual(names(r), []string{"a", "b"}) {
		t.Fatalf("first request %q, want [a b]", names(r))
	}
	<-done
	h.enqueue(t, "c", small)
	h.enqueue(t, "d", large)
	h.enqueue(t, "e", small)
	if r := col.Next(t); !reflect.DeepEqual(names(r), []string{"c", "d"}) {
		t.Fatalf("second request %q, want [c d]", names(r))
	}
	<-done
	col.None(t)
	h.tick <- time.Now()
	if r := col.Next(t); !reflect.DeepEqual(names(r), []string{"e"}) {
		t.Fatalf("third request %q, want [e], at the tick", names(r))
	}
	<-done
	if got := sz.all(); got[0] != limit || got[1] != limit || got[2] > limit {
		t.Fatalf("request sizes %v, want %d, %d, then less", got, limit, limit)
	}
	wantCounts(t, h.e, Counts{Handed: 5, Exported: 5})
	h.released.want(t, []string{"a", "b", "c", "d", "e"}, 1)
}

// Whatever the spans' sizes, no request passes the export limit, its envelope
// included, and the spans go in the order they were queued.
func TestRequestsNeverPassTheLimit(t *testing.T) {
	sz := &sizes{}
	col := fakeotlp.New(t, sz.answer)
	const limit = 4096
	h := newHarness(t, col, nil, options{maxRequestSize: limit})
	shortest, room := len(encodeSpan(t, "s00", 0)), h.e.MaxSpanBytes()
	rng := rand.New(rand.NewPCG(1, 2))
	const n = 60
	queued := spanNames("s", 10, 10+n) // names of one length: their spans' sizes are what the test sets
	for _, name := range queued {
		h.enqueue(t, name, shortest+rng.IntN(room-shortest+1))
	}
	h.flush(t)

	var sent []string
	for _, r := range col.Received() {
		sent = append(sent, names(r)...)
	}
	if !reflect.DeepEqual(sent, queued) {
		t.Fatalf("spans sent %q, want %q, in order", sent, queued)
	}
	for i, size := range sz.all() {
		if size > limit {
			t.Fatalf("request %d of %d bytes, over the limit of %d", i, size, limit)
		}
	}
	wantCounts(t, h.e, Counts{Handed: n, Exported: n})
	h.released.want(t, queued, 1)
}

// MaxSpanBytes is the export limit less the envelope, which grows with the
// resource: a span of that size fills a request alone, to the limit. The check is
// the caller's: a span one byte longer would go alone, over the limit.
func TestMaxSpanBytes(t *testing.T) {
	col := fakeotlp.New(t, nil)
	h := newHarness(t, col, nil, options{})
	open, closing := envelope(h.e.client.Resource())
	if got, want := h.e.MaxSpanBytes(), 16<<20-len(open)-len(closing); got != want {
		t.Fatalf("MaxSpanBytes = %d, want 16 MiB less the envelope, %d", got, want)
	}
	attr := "deployment.environment.name=production"
	more := newHarness(t, col, map[string]string{"OTEL_RESOURCE_ATTRIBUTES": attr}, options{})
	if got, want := more.e.MaxSpanBytes(), h.e.MaxSpanBytes()-len(`,{"key":"deployment.environment.name","value":{"stringValue":"production"}}`); got != want {
		t.Fatalf("MaxSpanBytes with %s = %d, want %d: the envelope carries the resource", attr, got, want)
	}

	sz := &sizes{}
	const limit = 4096
	small := newHarness(t, fakeotlp.New(t, sz.answer), nil, options{maxRequestSize: limit})
	largest := small.e.MaxSpanBytes()
	small.e.Enqueue(encodeSpan(t, "fits", largest), small.released.of("fits"))
	small.col.Next(t)
	small.e.Enqueue(encodeSpan(t, "over", largest+1), small.released.of("over"))
	small.col.Next(t)
	if got := sz.all(); !reflect.DeepEqual(got, []int64{limit, limit + 1}) {
		t.Fatalf("request sizes %v, want [%d %d]: each span alone", got, limit, limit+1)
	}
}

// Every span is released once, when its export has ended, whatever the export's
// outcome — and counted by it.
func TestReleasedOnceOnEveryOutcome(t *testing.T) {
	for _, tc := range []struct {
		name     string
		vars     map[string]string
		respond  fakeotlp.Respond
		requests int
		exported uint64
		failed   map[string]uint64
	}{
		{"exported", nil, nil, 1, 3, nil},
		{"rejected in a partial success", nil, func(w http.ResponseWriter, _ *http.Request, _ int) {
			io.WriteString(w, `{"partialSuccess":{"rejectedSpans":"2","errorMessage":"too large"}}`)
		}, 1, 1, map[string]uint64{"rejected": 2}},
		// Two attempts: the first backoff (0.25–0.5 s) fits in the timeout, the second
		// (0.5–1 s) does not.
		{"failed after retries", map[string]string{"OTEL_EXPORTER_OTLP_TRACES_TIMEOUT": "700"},
			func(w http.ResponseWriter, _ *http.Request, _ int) { w.WriteHeader(http.StatusServiceUnavailable) },
			2, 0, map[string]uint64{"503": 3}},
		{"a 2xx that cannot be read", nil, func(w http.ResponseWriter, _ *http.Request, _ int) {
			io.WriteString(w, "<html>ok</html>")
		}, 1, 0, map[string]uint64{"malformed_response": 3}},
		{"a status not retried", nil, func(w http.ResponseWriter, _ *http.Request, _ int) {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
		}, 1, 0, map[string]uint64{"413": 3}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			col := fakeotlp.New(t, tc.respond)
			h := newHarness(t, col, tc.vars, options{})
			queued := h.enqueueN(t, 3, "s")
			h.flush(t)
			if n := col.Requests(); n != tc.requests {
				t.Fatalf("%d requests, want %d", n, tc.requests)
			}
			wantCounts(t, h.e, Counts{Handed: 3, Exported: tc.exported, Failed: tc.failed})
			h.released.want(t, queued, 1)
		})
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

// A span's bytes stay held while its export is in flight: release comes when the
// export ends.
func TestReleaseWaitsForTheExportToEnd(t *testing.T) {
	col, answer := stalledCollector(t)
	h := newHarness(t, col, nil, options{})
	queued := h.enqueueN(t, 2, "s")
	h.tick <- time.Now()
	col.Next(t)
	h.released.want(t, nil, 0)
	answer()
	h.flush(t)
	h.released.want(t, queued, 1)
	wantCounts(t, h.e, Counts{Handed: 2, Exported: 2})
}

// Enqueue never waits on the collector, and the queue drops nothing: the caller's
// budget is its bound.
func TestEnqueueNeverWaitsAndDropsNothing(t *testing.T) {
	col, _ := stalledCollector(t)
	h := newHarness(t, col, nil, options{})
	h.enqueue(t, "first", 0)
	h.tick <- time.Now()
	col.Next(t) // the sender is now stuck in the export

	queued := spanNames("s", 0, 2000)
	spans := make([][]byte, len(queued))
	for i, name := range queued {
		spans[i] = encodeSpan(t, name, 0)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i, name := range queued {
			h.e.Enqueue(spans[i], h.released.of(name))
		}
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Enqueue waited on a stalled collector")
	}
	wantCounts(t, h.e, Counts{Handed: 1})
	h.e.Shutdown(context.Background())
	wantCounts(t, h.e, Counts{Handed: 1, Failed: map[string]uint64{"timeout": 1}, Shutdown: 2000})
	h.released.want(t, append([]string{"first"}, queued...), 1)
}

// At shutdown the export in flight is cut (failed: timeout), what is still queued is
// dropped (shutdown), and what is queued later is dropped as it comes — each span
// released once, and the drops reported on a line of their own.
func TestShutdownReleasesWhatIsLeft(t *testing.T) {
	col, _ := stalledCollector(t)
	h := newHarness(t, col, nil, options{})
	inFlight := h.enqueueN(t, 3, "a")
	h.tick <- time.Now()
	col.Next(t)
	queued := h.enqueueN(t, 5, "b")

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := h.e.ForceFlush(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ForceFlush = %v, want the deadline", err)
	}
	h.released.want(t, nil, 0)
	h.e.Shutdown(context.Background())
	wantCounts(t, h.e, Counts{Handed: 3, Failed: map[string]uint64{"timeout": 3}, Shutdown: 5})
	h.released.want(t, append(inFlight, queued...), 1)
	reports := h.reports.reports(t)
	if len(reports) != 2 || reports[0]["kaiak.trace_export.failed"] != float64(3) || reports[0]["kaiak.trace_export.dropped"] != float64(0) ||
		reports[1]["kaiak.trace_export.failed"] != float64(0) || reports[1]["kaiak.trace_export.dropped"] != float64(5) {
		t.Fatalf("reports %v, want two: the cut export's 3 failed, then Shutdown's 5 dropped", reports)
	}

	h.enqueue(t, "late", 0)
	h.released.want(t, append(append(inFlight, queued...), "late"), 1)
	wantCounts(t, h.e, Counts{Handed: 3, Failed: map[string]uint64{"timeout": 3}, Shutdown: 6})
	h.e.Shutdown(context.Background())
	if err := h.e.ForceFlush(context.Background()); !errors.Is(err, otlp.ErrShutdown) {
		t.Fatalf("ForceFlush after Shutdown = %v, want otlp.ErrShutdown", err)
	}
	if n := col.Requests(); n != 1 {
		t.Fatalf("%d requests, want only the one cut", n)
	}
}

func TestRetriedUntilDelivered(t *testing.T) {
	col := fakeotlp.New(t, func(w http.ResponseWriter, _ *http.Request, n int) {
		if n == 0 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		io.WriteString(w, "{}")
	})
	h := newHarness(t, col, nil, options{})
	queued := h.enqueueN(t, 2, "s")
	h.flush(t)
	if n := col.Requests(); n != 2 {
		t.Fatalf("%d requests, want 2: one retry", n)
	}
	if first, second := col.Next(t), col.Next(t); !reflect.DeepEqual(names(first), names(second)) {
		t.Fatalf("retry sent %q, want the same spans as %q", names(second), names(first))
	}
	wantCounts(t, h.e, Counts{Handed: 2, Exported: 2})
	h.released.want(t, queued, 1)
	if r := h.reports.reports(t); len(r) != 0 {
		t.Fatalf("reports %v, want none: the spans were delivered", r)
	}
}

// A Retry-After beyond the time left fails the export at once, reported with the
// status and in the gateway's own words.
func TestRetryAfterBeyondTheTimeLeftFailsAtOnce(t *testing.T) {
	col := fakeotlp.New(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	h := newHarness(t, col, nil, options{})
	queued := h.enqueueN(t, 2, "s")
	h.flush(t)
	if n := col.Requests(); n != 1 {
		t.Fatalf("%d requests, want 1", n)
	}
	wantCounts(t, h.e, Counts{Handed: 2, Failed: map[string]uint64{"503": 2}})
	h.released.want(t, queued, 1)
	reports := h.reports.reports(t)
	if len(reports) != 1 || reports[0]["http.response.status_code"] != float64(503) ||
		reports[0]["exception.message"] != "collector answered 503 Service Unavailable" ||
		reports[0]["kaiak.trace_export.failed"] != float64(2) || reports[0]["kaiak.trace_export.dropped"] != float64(0) {
		t.Fatalf("reports %v, want one: 2 failed with the 503", reports)
	}
}

// A redirect is not followed: neither the spans nor a configured header reach the
// target, and the export fails at once with the redirect's status.
func TestRedirectIsNotFollowed(t *testing.T) {
	reached := make(chan string, 10)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached <- r.Header.Get("X-Api-Key")
		io.WriteString(w, "{}")
	}))
	defer target.Close()
	destination := strings.Replace(target.URL, "127.0.0.1", "localhost", 1)
	col := fakeotlp.New(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		http.Redirect(w, r, destination, http.StatusTemporaryRedirect)
	})
	h := newHarness(t, col, map[string]string{"OTEL_EXPORTER_OTLP_TRACES_HEADERS": "x-api-key=test-secret"}, options{})
	queued := h.enqueueN(t, 2, "s")
	h.flush(t)
	select {
	case key := <-reached:
		t.Fatalf("the redirect's target was reached (x-api-key %q)", key)
	default:
	}
	if n := col.Requests(); n != 1 {
		t.Fatalf("%d requests, want 1: a redirect is not retried", n)
	}
	wantCounts(t, h.e, Counts{Handed: 2, Failed: map[string]uint64{"307": 2}})
	h.released.want(t, queued, 1)
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
		{"partial success", true, "collector rejected 1 spans"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			col := fakeotlp.New(t, func(w http.ResponseWriter, r *http.Request, _ int) {
				message := "invalid credential: " + r.Header.Get("Authorization")
				if tc.partial {
					json.NewEncoder(w).Encode(map[string]any{"partialSuccess": map[string]any{
						"rejectedSpans": "1", "errorMessage": message,
					}})
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				json.NewEncoder(w).Encode(map[string]any{"code": 16, "message": message})
			})
			h := newHarness(t, col, map[string]string{"OTEL_EXPORTER_OTLP_HEADERS": "authorization=Bearer%20test-secret"}, options{})
			h.enqueueN(t, 2, "s")
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
