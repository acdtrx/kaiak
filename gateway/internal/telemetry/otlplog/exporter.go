// Package otlplog exports the gateway's log records to an OpenTelemetry collector
// over OTLP/HTTP with JSON encoding (docs/specs/GATEWAY.md, Observability → OTLP
// log export). An slog handler hands every record to the next handler (stderr) and
// queues a copy; a background sender posts the queue in batches through the
// signal's OTLP connection (otlp), which holds the delivery rules. Logging never
// waits on the collector: a full queue drops the newest records, and every record
// ends up counted: dropped by the queue, or handed to the exporter and then
// exported or failed (Counts).
package otlplog

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"kaiak/internal/telemetry/otlp"
)

// Delivery is fixed, not configurable (the batch processor's OTEL_BLRP_* variables
// are not read).
const (
	queueCapacity = 10000
	batchSize     = 512
	batchInterval = time.Second
	// reportInterval is the least time between two `log export failing` lines.
	reportInterval = time.Minute
)

// options are the exporter's fixed sizes and its timing; tests replace them.
type options struct {
	capacity  int
	batchSize int
	// tick fires a send of whatever is queued; nil is a ticker every batchInterval.
	tick <-chan time.Time
	// now is the clock problem reports are rate-limited by.
	now func() time.Time
	// batchDone, when set, runs after each batch's export has ended.
	batchDone func()
}

// Exporter queues log records and sends them to the collector in the background.
// Its handler (Handler) is safe for concurrent use; ForceFlush and Shutdown may be
// called from any goroutine.
type Exporter struct {
	client *otlp.Client
	report *slog.Logger
	opts   options

	mu     sync.Mutex
	queue  []record
	closed bool

	wake         chan struct{}
	flushes      chan chan error
	stop         context.CancelFunc
	done         chan struct{}
	shutdownOnce sync.Once

	// The counts: records handed from the queue to the export, exported, failed (in
	// all), and dropped by a full queue or at shutdown.
	handed, exported, failed, droppedFull, droppedShutdown atomic.Uint64
	// failedBy is failed by error.type.
	failedMu sync.Mutex
	failedBy map[string]uint64

	// Problem reporting: the sender's alone, and Shutdown's once the sender has
	// stopped.
	lastStatus      int
	lastError       string
	lastReport      time.Time
	reportedFailed  uint64
	reportedDropped uint64
}

// errShutdown is ForceFlush's answer once Shutdown has run.
var errShutdown = errors.New("otlplog: exporter shut down")

// New starts an exporter sending to the collector s names, under a resource made of
// s and svc. Export problems are written to report, which must not lead back to
// the exporter's handler: a problem exported would feed the problem. Shutdown stops
// the exporter.
func New(s *otlp.Settings, svc otlp.Service, report *slog.Logger) *Exporter {
	return newExporter(s, svc, report, options{})
}

func newExporter(s *otlp.Settings, svc otlp.Service, report *slog.Logger, opts options) *Exporter {
	if opts.capacity == 0 {
		opts.capacity = queueCapacity
	}
	if opts.batchSize == 0 {
		opts.batchSize = batchSize
	}
	if opts.now == nil {
		opts.now = time.Now
	}
	ctx, stop := context.WithCancel(context.Background())
	e := &Exporter{
		client:  otlp.NewClient(s, svc),
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

// Counts are the log records by what became of them since the exporter started, as
// the SDK's own metrics count them (docs/specs/GATEWAY.md, Observability →
// Exporters' own counts).
type Counts struct {
	// Handed are the records the queue handed to the export; QueueFull and Shutdown
	// those it dropped: refused by the full queue, and still queued (or logged) once
	// the exporter shut down.
	Handed, QueueFull, Shutdown uint64
	// Exported are the records the collector accepted; Failed those in an export
	// given up or a partial success's rejections, by error.type (otlp.Outcome).
	Exported uint64
	Failed   map[string]uint64
}

// Counts are the records by what became of them, since the exporter started.
func (e *Exporter) Counts() Counts {
	c := Counts{Handed: e.handed.Load(), QueueFull: e.droppedFull.Load(), Shutdown: e.droppedShutdown.Load(),
		Exported: e.exported.Load()}
	e.failedMu.Lock()
	defer e.failedMu.Unlock()
	if len(e.failedBy) > 0 {
		c.Failed = make(map[string]uint64, len(e.failedBy))
		for t, n := range e.failedBy {
			c.Failed[t] = n
		}
	}
	return c
}

// ForceFlush sends what is queued until the queue is empty — records logged
// meanwhile included — and returns nil then, or ctx's error when ctx ends first.
// The sender carries on either way; Shutdown drops what is still queued.
func (e *Exporter) ForceFlush(ctx context.Context) error {
	reply := make(chan error, 1)
	select {
	case e.flushes <- reply:
	case <-ctx.Done():
		return ctx.Err()
	case <-e.done:
		return errShutdown
	}
	select {
	case err := <-reply:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Shutdown stops the sender, cutting short an export in flight (its records count
// as failed); records still queued are dropped and counted, and records logged
// later are dropped. Shutdown waits for the sender and is safe to call more than
// once. It does not consult ctx: cutting the export in flight is what stops the
// sender, so the wait never depends on the collector — the time for sending what
// is queued is ForceFlush's.
func (e *Exporter) Shutdown(ctx context.Context) {
	e.shutdownOnce.Do(func() {
		e.stop()
		<-e.done
		e.mu.Lock()
		left := len(e.queue)
		e.queue, e.closed = nil, true
		e.mu.Unlock()
		e.droppedShutdown.Add(uint64(left))
		e.client.CloseIdleConnections()
		// The report at exit is never held back: drops at exit are never silent.
		e.writeReport()
	})
}

// enqueue adds r to the queue without waiting; a full or closed queue drops it.
func (e *Exporter) enqueue(r record) {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		e.droppedShutdown.Add(1)
		return
	}
	if len(e.queue) >= e.opts.capacity {
		e.mu.Unlock()
		e.droppedFull.Add(1)
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
				reply <- errShutdown
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
	e.handed.Add(uint64(n))
	return batch
}

// export sends one batch through the client, which retries it until the batch's
// timeout, and counts its records.
func (e *Exporter) export(ctx context.Context, batch []record) {
	n := uint64(len(batch))
	body, err := encodeBatch(e.client.Resource(), batch)
	if err != nil {
		e.fail(n, errorTypeOther, 0, fmt.Errorf("encode batch: %w", err))
		return
	}
	o := e.client.Export(ctx, body)
	if o.Err != nil {
		e.fail(n, o.ErrorType, o.Status, o.Err)
		return
	}
	rejected := min(o.Rejected, n)
	e.exported.Add(n - rejected)
	if rejected > 0 {
		e.fail(rejected, otlp.ErrorRejected, o.Status, o.RejectErr)
	}
}

// errorTypeOther is the error.type of a failure outside the export's classes (a
// batch that could not be encoded): the convention's fallback value.
const errorTypeOther = "_OTHER"

// fail counts n records as failed, of class errorType, and keeps the reason for
// the next report.
func (e *Exporter) fail(n uint64, errorType string, status int, err error) {
	e.failed.Add(n)
	e.failedMu.Lock()
	if e.failedBy == nil {
		e.failedBy = map[string]uint64{}
	}
	e.failedBy[errorType] += n
	e.failedMu.Unlock()
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
	failed, dropped := e.failed.Load(), e.droppedFull.Load()+e.droppedShutdown.Load()
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
