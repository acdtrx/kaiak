// Package otlplog exports the gateway's log records to an OpenTelemetry collector
// over OTLP/HTTP with JSON encoding (docs/specs/GATEWAY.md, Observability → OTLP
// log export). An slog handler hands every record to the next handler (stderr) and
// queues a copy; a background sender (otlp.Sender) posts the queue in batches
// through the signal's OTLP connection (otlp), which holds the delivery rules.
// Logging never waits on the collector: a full queue drops the newest records, and
// every record ends up counted: dropped by the queue, or handed to the exporter and
// then exported or failed (Counts).
package otlplog

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"kaiak/internal/telemetry/otlp"
)

// Delivery is fixed, not configurable (the batch processor's OTEL_BLRP_* variables
// are not read); a partial batch goes every second (otlp.Sender).
const (
	queueCapacity = 10000
	batchSize     = 512
)

// options are the exporter's fixed sizes and its timing; tests replace them.
type options struct {
	capacity  int
	batchSize int
	// tick fires a send of whatever is queued; nil is a ticker every second.
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
	opts   options
	sender *otlp.Sender
	// problems holds the failed records by error.type and writes `log export
	// failing`.
	problems *otlp.Problems

	mu           sync.Mutex
	queue        []record
	closed       bool
	shutdownOnce sync.Once

	// The counts: records handed from the queue to the export, exported, and dropped
	// by a full queue or at shutdown.
	handed, exported, droppedFull, droppedShutdown atomic.Uint64
}

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
	e := &Exporter{client: otlp.NewClient(s, svc), opts: opts}
	e.problems = otlp.NewProblems(report, otlp.ProblemLine{
		Message: "log export failing",
		Failed:  "kaiak.log_export.failed",
		Dropped: "kaiak.log_export.dropped",
	}, e.dropped, opts.now)
	e.sender = otlp.StartSender(opts.tick, e.sendBatches)
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
	return Counts{Handed: e.handed.Load(), QueueFull: e.droppedFull.Load(), Shutdown: e.droppedShutdown.Load(),
		Exported: e.exported.Load(), Failed: e.problems.Failed()}
}

// dropped is the records dropped by a full queue or at shutdown.
func (e *Exporter) dropped() uint64 {
	return e.droppedFull.Load() + e.droppedShutdown.Load()
}

// ForceFlush sends what is queued until the queue is empty — records logged
// meanwhile included — and returns nil then, or ctx's error when ctx ends first
// (otlp.ErrShutdown once Shutdown has run). The sender carries on either way;
// Shutdown drops what is still queued.
func (e *Exporter) ForceFlush(ctx context.Context) error {
	return e.sender.ForceFlush(ctx)
}

// Shutdown stops the sender, cutting short an export in flight (its records count
// as failed); records still queued are dropped and counted, and records logged
// later are dropped. Shutdown waits for the sender and is safe to call more than
// once. It does not consult ctx: cutting the export in flight is what stops the
// sender, so the wait never depends on the collector — the time for sending what
// is queued is ForceFlush's.
func (e *Exporter) Shutdown(ctx context.Context) {
	e.shutdownOnce.Do(func() {
		e.sender.Stop()
		e.mu.Lock()
		left := len(e.queue)
		e.queue, e.closed = nil, true
		e.mu.Unlock()
		e.droppedShutdown.Add(uint64(left))
		e.client.CloseIdleConnections()
		e.problems.ReportNow()
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
		e.sender.Wake()
	}
}

// sendBatches is the sender's send (otlp.Sender): it exports full batches while one
// is queued, or with all every record queued. A full batch goes at once, a partial
// one at the next tick or flush.
func (e *Exporter) sendBatches(ctx context.Context, all bool) {
	least := e.opts.batchSize
	if all {
		least = 1
	}
	for ctx.Err() == nil {
		batch := e.take(least)
		if batch == nil {
			return
		}
		e.export(ctx, batch)
		if e.opts.batchDone != nil {
			e.opts.batchDone()
		}
		e.problems.Report()
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
		e.problems.Fail(n, errorTypeOther, 0, fmt.Errorf("encode batch: %w", err))
		return
	}
	o := e.client.Export(ctx, body)
	if o.Err != nil {
		e.problems.Fail(n, o.ErrorType, o.Status, o.Err)
		return
	}
	rejected := min(o.Rejected, n)
	e.exported.Add(n - rejected)
	if rejected > 0 {
		e.problems.Fail(rejected, otlp.ErrorRejected, o.Status, o.RejectErr)
	}
}

// errorTypeOther is the error.type of a failure outside the export's classes (a
// batch that could not be encoded): the convention's fallback value.
const errorTypeOther = "_OTHER"
