// Package otlplog exports the gateway's log records to an OpenTelemetry collector
// over OTLP/HTTP with JSON encoding (docs/specs/GATEWAY.md, Observability → OTLP
// log export). An slog handler hands every record to the next handler (stderr) and
// queues a copy; a background sender posts the queue in batches. Logging never
// waits on the collector: a full queue drops the newest records, and every record
// ends up counted as exported, failed or dropped.
package otlplog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/rand/v2"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"kaiak/internal/netfail"
)

// Delivery is fixed, not configurable (the batch processor's OTEL_BLRP_* variables
// are not read).
const (
	queueCapacity = 10000
	batchSize     = 512
	batchInterval = time.Second
	// maxResponseSize bounds a collector's answer, the OTLP specification's bound.
	maxResponseSize = 4 << 20
	firstBackoff    = 500 * time.Millisecond
	maxBackoff      = 5 * time.Second
	// reportInterval is the least time between two `log export failing` lines.
	reportInterval = time.Minute
)

// Resource is what the gateway itself puts in the resource; both win over
// OTEL_RESOURCE_ATTRIBUTES.
type Resource struct {
	// ServiceVersion is the build version: service.version, and the User-Agent's.
	ServiceVersion string
	// InstanceID is the gateway's instance ID: service.instance.id.
	InstanceID string
}

// Counts are the records by what became of them, since the exporter started:
// accepted by the collector, in a batch given up, or never sent.
type Counts struct {
	Exported, Failed, Dropped uint64
}

// options are the exporter's fixed sizes and its timing; tests replace them.
type options struct {
	capacity  int
	batchSize int
	// tick fires a send of whatever is queued; nil is a ticker every batchInterval.
	tick <-chan time.Time
	// after waits out a retry's delay.
	after func(time.Duration) <-chan time.Time
	// now is the clock problem reports are rate-limited by.
	now func() time.Time
	// batchDone, when set, runs after each batch's export has ended.
	batchDone func()
}

// Exporter queues log records and sends them to the collector in the background.
// Its handler (Handler) is safe for concurrent use; Flush and Close may be called
// from any goroutine.
type Exporter struct {
	endpoint  string
	headers   []header
	timeout   time.Duration
	userAgent string
	resource  []keyValue
	client    *http.Client
	report    *slog.Logger
	opts      options

	mu     sync.Mutex
	queue  []record
	closed bool

	wake      chan struct{}
	flushes   chan chan error
	stop      context.CancelFunc
	done      chan struct{}
	closeOnce sync.Once

	exported, failed, dropped atomic.Uint64

	// Problem reporting: the sender's alone, and Close's once the sender has
	// stopped.
	lastStatus      int
	lastError       string
	lastReport      time.Time
	reportedFailed  uint64
	reportedDropped uint64
}

// errClosed is Flush's answer once Close has run.
var errClosed = errors.New("otlplog: exporter closed")

// New starts an exporter sending to the collector s names, under a resource made of
// s and res. Export problems are written to report, which must not lead back to
// the exporter's handler: a problem exported would feed the problem. Close stops
// the exporter.
func New(s *Settings, res Resource, report *slog.Logger) *Exporter {
	return newExporter(s, res, report, options{})
}

func newExporter(s *Settings, res Resource, report *slog.Logger, opts options) *Exporter {
	if opts.capacity == 0 {
		opts.capacity = queueCapacity
	}
	if opts.batchSize == 0 {
		opts.batchSize = batchSize
	}
	if opts.after == nil {
		opts.after = time.After
	}
	if opts.now == nil {
		opts.now = time.Now
	}
	ctx, stop := context.WithCancel(context.Background())
	e := &Exporter{
		endpoint:  s.endpoint.String(),
		headers:   s.headers,
		timeout:   s.timeout,
		userAgent: "kaiak/" + res.ServiceVersion,
		resource:  resourceAttributes(s, res),
		client: &http.Client{
			Transport: http.DefaultTransport.(*http.Transport).Clone(),
			// A redirect is not followed: Go would resend the batch and every
			// configured header to its target (docs/specs/GATEWAY.md, OTLP log
			// export → Delivery: no redirects). The 3xx fails the batch.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		report:  report,
		opts:    opts,
		wake:    make(chan struct{}, 1),
		flushes: make(chan chan error),
		stop:    stop,
		done:    make(chan struct{}),
	}
	tick := opts.tick
	var ticker *time.Ticker
	if tick == nil {
		ticker = time.NewTicker(batchInterval)
		tick = ticker.C
	}
	go func() {
		defer close(e.done)
		if ticker != nil {
			defer ticker.Stop()
		}
		e.run(ctx, tick)
	}()
	return e
}

// Handler returns the exporting handler: next gets every record unchanged, and the
// exporter a copy.
func (e *Exporter) Handler(next slog.Handler) slog.Handler {
	return &handler{next: next, exp: e}
}

// Counts reads the counts.
func (e *Exporter) Counts() Counts {
	return Counts{Exported: e.exported.Load(), Failed: e.failed.Load(), Dropped: e.dropped.Load()}
}

// Flush sends what is queued until the queue is empty — records logged meanwhile
// included — and returns nil then, or ctx's error when ctx ends first. The sender
// carries on either way; Close drops what is still queued.
func (e *Exporter) Flush(ctx context.Context) error {
	reply := make(chan error, 1)
	select {
	case e.flushes <- reply:
	case <-ctx.Done():
		return ctx.Err()
	case <-e.done:
		return errClosed
	}
	select {
	case err := <-reply:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close stops the sender, cutting short an export in flight (its records count as
// failed); records still queued are dropped and counted, and records logged later
// are dropped. Close waits for the sender and is safe to call more than once.
func (e *Exporter) Close() {
	e.closeOnce.Do(func() {
		e.stop()
		<-e.done
		e.mu.Lock()
		left := len(e.queue)
		e.queue, e.closed = nil, true
		e.mu.Unlock()
		e.dropped.Add(uint64(left))
		e.client.CloseIdleConnections()
		// The report at exit is never held back: drops at exit are never silent.
		e.writeReport()
	})
}

// enqueue adds r to the queue without waiting; a full or closed queue drops it.
func (e *Exporter) enqueue(r record) {
	e.mu.Lock()
	if e.closed || len(e.queue) >= e.opts.capacity {
		e.mu.Unlock()
		e.dropped.Add(1)
		return
	}
	e.queue = append(e.queue, r)
	full := len(e.queue) >= e.opts.batchSize
	e.mu.Unlock()
	if full {
		select {
		case e.wake <- struct{}{}:
		default:
		}
	}
}

// run is the sender: a full batch goes at once, a partial one at the next tick or
// flush; one export is in flight at a time.
func (e *Exporter) run(ctx context.Context, tick <-chan time.Time) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-e.wake:
			e.sendBatches(ctx, e.opts.batchSize)
		case <-tick:
			e.sendBatches(ctx, 1)
		case reply := <-e.flushes:
			e.sendBatches(ctx, 1)
			if ctx.Err() != nil {
				reply <- errClosed
			} else {
				reply <- nil
			}
		}
	}
}

// sendBatches exports batches while at least least records are queued.
func (e *Exporter) sendBatches(ctx context.Context, least int) {
	for ctx.Err() == nil {
		batch := e.take(least)
		if batch == nil {
			return
		}
		e.export(ctx, batch)
		if e.opts.batchDone != nil {
			e.opts.batchDone()
		}
		e.reportProblems()
	}
}

// take removes the oldest batch from the queue when at least least records are
// queued; nil otherwise.
func (e *Exporter) take(least int) []record {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := len(e.queue)
	if n == 0 || n < least {
		return nil
	}
	n = min(n, e.opts.batchSize)
	batch := e.queue[:n:n]
	e.queue = e.queue[n:]
	return batch
}

// export sends one batch, retrying as the OTLP specification has it until the
// batch's timeout, and counts its records.
func (e *Exporter) export(ctx context.Context, batch []record) {
	n := uint64(len(batch))
	body, err := encodeBatch(e.resource, batch)
	if err != nil {
		e.fail(n, 0, fmt.Errorf("encode batch: %w", err))
		return
	}
	ctx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()
	for attempt := 0; ; attempt++ {
		o := e.post(ctx, body)
		if o.err == nil {
			rejected := min(o.rejected, n)
			e.exported.Add(n - rejected)
			if rejected > 0 {
				e.fail(rejected, o.status, o.rejectErr)
			}
			return
		}
		if !o.retry {
			e.fail(n, o.status, o.err)
			return
		}
		// The collector's Retry-After when it is longer than the backoff: one of 0
		// (or a date already past) would retry back to back.
		wait := max(o.retryAfter, backoff(attempt))
		if deadline, _ := ctx.Deadline(); wait >= time.Until(deadline) {
			e.fail(n, o.status, o.err)
			return
		}
		select {
		case <-e.opts.after(wait):
		case <-ctx.Done():
			e.fail(n, o.status, o.err)
			return
		}
	}
}

// outcome is one attempt's result. Its errors are in the gateway's own words,
// never text the collector sent (docs/specs/GATEWAY.md, Observability → Logs: no
// remote text): an auth proxy may echo the credential in its answer.
type outcome struct {
	// status is the collector's answer; 0 when it gave none.
	status int
	// err is nil when the batch was delivered.
	err   error
	retry bool
	// retryAfter is the collector's Retry-After; below 0 when it sent none.
	retryAfter time.Duration
	// rejected and rejectErr are a delivered batch's partial success.
	rejected  uint64
	rejectErr error
}

func (e *Exporter) post(ctx context.Context, body []byte) outcome {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.endpoint, bytes.NewReader(body))
	if err != nil {
		return outcome{err: err}
	}
	for _, h := range e.headers {
		req.Header.Add(h.name, h.value)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", e.userAgent)
	resp, err := e.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return outcome{err: fmt.Errorf("export cut short: %w", ctx.Err())}
		}
		return outcome{err: errors.New("no answer from the collector: " + netfail.Class(err)), retry: true, retryAfter: -1}
	}
	defer resp.Body.Close()
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize+1))
	o := outcome{status: resp.StatusCode, retryAfter: -1}
	answered := "collector answered " + strconv.Itoa(resp.StatusCode)
	if text := http.StatusText(resp.StatusCode); text != "" {
		answered += " " + text
	}
	if len(data) > maxResponseSize {
		o.err = errors.New(answered + " with a body above 4 MiB")
		return o
	}
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		// What counts as delivered: an empty body or an ExportLogsServiceResponse.
		// Anything else fails the batch unretried — the collector may have taken
		// some of it, and a retry would duplicate those records.
		if readErr != nil {
			o.err = errors.New(answered + ", its answer unreadable: " + netfail.Class(readErr))
			return o
		}
		rejected, ok := readExportResponse(data)
		if !ok {
			o.err = errors.New(answered + " with a body that is not an ExportLogsServiceResponse")
			return o
		}
		if rejected > 0 {
			o.rejected, o.rejectErr = rejected, fmt.Errorf("collector rejected %d records", rejected)
		}
		return o
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		o.err = errors.New(answered + ": redirects are not followed")
		return o
	}
	// A failure's body, a Status, is not decoded: its message is the collector's
	// text.
	o.err = errors.New(answered)
	switch resp.StatusCode {
	case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		o.retry = true
		o.retryAfter = retryAfter(resp.Header.Get("Retry-After"), time.Now())
	}
	return o
}

// readExportResponse reads a 2xx body as an ExportLogsServiceResponse: empty, or a
// JSON object whose partialSuccess, when present, is an object with
// rejectedLogRecords an integer (or its decimal string) and errorMessage a string;
// members OTLP does not define are ignored. It returns the records rejected, and
// false for a body that is not one.
func readExportResponse(data []byte) (uint64, bool) {
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return 0, true
	}
	if data[0] != '{' {
		return 0, false
	}
	var resp struct {
		PartialSuccess *struct {
			RejectedLogRecords *flexInt `json:"rejectedLogRecords"`
			// ErrorMessage is decoded only to check its type: the collector's text
			// is never logged.
			ErrorMessage *string `json:"errorMessage"`
		} `json:"partialSuccess"`
	}
	if json.Unmarshal(data, &resp) != nil {
		return 0, false
	}
	if resp.PartialSuccess == nil || resp.PartialSuccess.RejectedLogRecords == nil || *resp.PartialSuccess.RejectedLogRecords <= 0 {
		return 0, true
	}
	return uint64(*resp.PartialSuccess.RejectedLogRecords), true
}

// flexInt is a JSON int64 written either way protobuf's JSON mapping accepts: a
// decimal string or a number.
type flexInt int64

func (n *flexInt) UnmarshalJSON(b []byte) error {
	s := string(b)
	if unquoted, err := strconv.Unquote(s); err == nil {
		s = unquoted
	}
	v, err := strconv.ParseInt(s, 10, 64)
	*n = flexInt(v)
	return err
}

// retryAfter reads a Retry-After header — seconds or an HTTP date — as the wait
// from now; below 0 when it is absent or unreadable. A wait too long for a
// duration saturates, so that it outlasts every batch's timeout rather than read
// as absent.
func retryAfter(h string, now time.Time) time.Duration {
	if h == "" {
		return -1
	}
	if secs, err := strconv.ParseUint(h, 10, 64); err == nil || errors.Is(err, strconv.ErrRange) {
		if secs > uint64(maxWait/time.Second) {
			return maxWait
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(h); err == nil {
		return max(t.Sub(now), 0)
	}
	return -1
}

// maxWait is the longest duration: a Retry-After saturates there.
const maxWait = time.Duration(math.MaxInt64)

// backoff is the wait before retry attempt+1: from 0.5 s, doubling, at most 5 s,
// with jitter over its upper half.
func backoff(attempt int) time.Duration {
	base := maxBackoff
	if attempt < 4 {
		base = min(firstBackoff<<attempt, maxBackoff)
	}
	return base/2 + rand.N(base/2+1)
}

// fail counts n records as failed and keeps the reason for the next report.
func (e *Exporter) fail(n uint64, status int, err error) {
	e.failed.Add(n)
	e.lastStatus = status
	e.lastError = err.Error()
}

// reportProblems writes `log export failing` when records failed or were dropped
// since the last such line, at most once per reportInterval.
func (e *Exporter) reportProblems() {
	if !e.lastReport.IsZero() && e.opts.now().Sub(e.lastReport) < reportInterval {
		return
	}
	e.writeReport()
}

// writeReport writes `log export failing` when records failed or were dropped since
// the last such line.
func (e *Exporter) writeReport() {
	failed, dropped := e.failed.Load(), e.dropped.Load()
	newFailed, newDropped := failed-e.reportedFailed, dropped-e.reportedDropped
	if newFailed == 0 && newDropped == 0 {
		return
	}
	attrs := []slog.Attr{
		slog.Uint64("kaiak.log_export.failed", newFailed),
		slog.Uint64("kaiak.log_export.dropped", newDropped),
	}
	if e.lastStatus != 0 {
		attrs = append(attrs, slog.Int("http.response.status_code", e.lastStatus))
	}
	if e.lastError != "" {
		attrs = append(attrs, slog.String("exception.message", e.lastError))
	}
	e.report.LogAttrs(context.Background(), slog.LevelWarn, "log export failing", attrs...)
	e.reportedFailed, e.reportedDropped = failed, dropped
	e.lastReport = e.opts.now()
	e.lastStatus, e.lastError = 0, ""
}
