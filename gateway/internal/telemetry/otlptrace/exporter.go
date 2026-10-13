// Package otlptrace exports spans to an OpenTelemetry collector over OTLP/HTTP with
// JSON encoding (docs/specs/GATEWAY.md, Observability → OTLP trace export). The
// caller builds a span at its operation's end (Span) and encodes it (Encode), which
// tells it the span's size — what its own memory budget holds until the span's
// export ends — then queues the bytes with a callback that gives them back
// (Exporter.Enqueue). A background sender (otlp.Sender) posts the queue through the
// signal's OTLP connection (otlp), which holds the delivery rules, in requests of at
// most the export limit. The queue has no bound of its own: the caller's budget
// bounds it, so the exporter drops nothing for a full queue. Every span queued ends
// up counted and released: exported, failed, or dropped at shutdown (Counts).
package otlptrace

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"kaiak/internal/telemetry/otlp"
)

// maxRequestSize is the export limit: an export request's body, its envelope
// included, is at most 16 MiB — under the OpenTelemetry Collector's OTLP/HTTP
// receiver default of 20 MiB (docs/specs/GATEWAY.md, Request capture → The export
// limit). Fixed, not configurable, as the rest of delivery: the batch span
// processor's OTEL_BSP_* variables are not read, and a partial request goes every
// second (otlp.Sender).
const maxRequestSize = 16 << 20

// options are the exporter's timing and its request bound; tests replace them.
type options struct {
	// tick fires a send of whatever is queued; nil is a ticker every second.
	tick <-chan time.Time
	// now is the clock problem reports are rate-limited by.
	now func() time.Time
	// maxRequestSize bounds one request's body; 0 is maxRequestSize.
	maxRequestSize int
	// requestDone, when set, runs after each request's export has ended.
	requestDone func()
}

// Exporter queues encoded spans and sends them to the collector in the background.
// Its methods may be called from any goroutine.
type Exporter struct {
	client *otlp.Client
	opts   options
	sender *otlp.Sender
	// problems holds the failed spans by error.type and writes `trace export
	// failing`.
	problems *otlp.Problems
	// open and closing are every request's envelope: its spans go between them.
	open, closing []byte

	mu    sync.Mutex
	queue []queued
	// queuedBytes is the queued spans' bytes, the commas a request puts between them
	// left out.
	queuedBytes  int
	closed       bool
	shutdownOnce sync.Once

	// The counts: spans handed from the queue to the export, exported, and dropped
	// at shutdown.
	handed, exported, droppedShutdown atomic.Uint64
}

// queued is one span waiting for its export, with what gives its bytes back.
type queued struct {
	encoded []byte
	release func()
}

// New starts an exporter sending to the collector s names, under a resource made of
// s and svc. Export problems are written to report. Shutdown stops the exporter.
func New(s *otlp.Settings, svc otlp.Service, report *slog.Logger) *Exporter {
	return newExporter(s, svc, report, options{})
}

func newExporter(s *otlp.Settings, svc otlp.Service, report *slog.Logger, opts options) *Exporter {
	if opts.maxRequestSize == 0 {
		opts.maxRequestSize = maxRequestSize
	}
	client := otlp.NewClient(s, svc)
	open, closing := envelope(client.Resource())
	e := &Exporter{client: client, opts: opts, open: open, closing: closing}
	e.problems = otlp.NewProblems(report, otlp.ProblemLine{
		Message: "trace export failing",
		Failed:  "kaiak.trace_export.failed",
		Dropped: "kaiak.trace_export.dropped",
	}, e.droppedShutdown.Load, opts.now)
	e.sender = otlp.StartSender(opts.tick, e.sendRequests)
	return e
}

// MaxSpanBytes is the largest encoded span an export request can carry: the export
// limit less the request's envelope, whose resource is the operator's to set. A
// larger span can never be sent: the caller drops it instead of queueing it.
func (e *Exporter) MaxSpanBytes() int {
	return e.opts.maxRequestSize - len(e.open) - len(e.closing)
}

// Enqueue queues an encoded span (Encode) for export without waiting. release is
// called exactly once: when the span's export has ended — delivered or failed —
// or when the span is dropped at shutdown, which for a span queued once the
// exporter has shut down is within Enqueue, so the caller must not hold anything
// release takes. Until then the caller's budget holds the span's bytes: the queue
// has no bound of its own. encoded must not change once queued, and must be at most
// MaxSpanBytes long: a longer span would go alone, in a request over the export
// limit.
func (e *Exporter) Enqueue(encoded []byte, release func()) {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		e.droppedShutdown.Add(1)
		release()
		return
	}
	e.queue = append(e.queue, queued{encoded: encoded, release: release})
	e.queuedBytes += len(encoded)
	full := e.full()
	e.mu.Unlock()
	if full {
		e.sender.Wake()
	}
}

// Counts are the spans the exporter was given, by what became of them since it
// started, as the SDK's own metrics count them (docs/specs/GATEWAY.md,
// Observability → Exporters' own counts). Spans the caller dropped instead of
// queueing them are the caller's to count.
type Counts struct {
	// Handed are the spans the queue handed to the export; Shutdown those it
	// dropped: still queued, or queued later, once the exporter shut down.
	Handed, Shutdown uint64
	// Exported are the spans the collector accepted; Failed those in an export
	// given up or a partial success's rejections, by error.type (otlp.Outcome).
	Exported uint64
	Failed   map[string]uint64
}

// Counts are the spans by what became of them, since the exporter started. An
// export's spans are counted once it ends.
func (e *Exporter) Counts() Counts {
	return Counts{Handed: e.handed.Load(), Shutdown: e.droppedShutdown.Load(),
		Exported: e.exported.Load(), Failed: e.problems.Failed()}
}

// ForceFlush sends what is queued until the queue is empty — spans queued meanwhile
// included — and returns nil then, or ctx's error when ctx ends first
// (otlp.ErrShutdown once Shutdown has run). The sender carries on either way;
// Shutdown drops what is still queued.
func (e *Exporter) ForceFlush(ctx context.Context) error {
	return e.sender.ForceFlush(ctx)
}

// Shutdown stops the sender, cutting short an export in flight (its spans count as
// failed); spans still queued are dropped, counted and released, and spans queued
// later are dropped as they come. The last `trace export failing` line is never
// held back. Shutdown waits for the sender and is safe to call more than once. It
// does not consult ctx: cutting the export in flight is what stops the sender, so
// the wait never depends on the collector — the time for sending what is queued is
// ForceFlush's.
func (e *Exporter) Shutdown(ctx context.Context) {
	e.shutdownOnce.Do(func() {
		e.sender.Stop()
		e.mu.Lock()
		left := e.queue
		e.queue, e.queuedBytes, e.closed = nil, 0, true
		e.mu.Unlock()
		e.droppedShutdown.Add(uint64(len(left)))
		for _, q := range left {
			q.release()
		}
		e.client.CloseIdleConnections()
		e.problems.ReportNow()
	})
}

// sendRequests is the sender's send (otlp.Sender): it exports full requests while
// one is queued, or with all every span queued. A full request goes at once, a
// partial one at the next tick or flush.
func (e *Exporter) sendRequests(ctx context.Context, all bool) {
	for ctx.Err() == nil {
		batch := e.take(all)
		if batch == nil {
			return
		}
		e.export(ctx, batch)
		if e.opts.requestDone != nil {
			e.opts.requestDone()
		}
		e.problems.Report()
	}
}

// full reports whether the queue holds a full request: its spans in one envelope
// reach the export limit. It is called with mu held.
func (e *Exporter) full() bool {
	n := len(e.queue)
	return n > 0 && len(e.open)+e.queuedBytes+n-1+len(e.closing) >= e.opts.maxRequestSize
}

// take removes from the queue the oldest spans that fit in one request — at least
// one — when the queue holds a full request, or with all any span; nil otherwise.
func (e *Exporter) take(all bool) []queued {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.queue) == 0 || !all && !e.full() {
		return nil
	}
	room := e.MaxSpanBytes()
	n, size := 1, len(e.queue[0].encoded)
	for n < len(e.queue) && size+1+len(e.queue[n].encoded) <= room {
		size += 1 + len(e.queue[n].encoded)
		n++
	}
	batch := e.queue[:n:n]
	e.queue = e.queue[n:]
	e.queuedBytes -= size - (n - 1)
	e.handed.Add(uint64(n))
	return batch
}

// export sends one request through the client, which retries it until the
// request's timeout, counts its spans, then releases them: their export has ended.
func (e *Exporter) export(ctx context.Context, batch []queued) {
	size := len(e.open) + len(batch) - 1 + len(e.closing)
	for _, q := range batch {
		size += len(q.encoded)
	}
	body := make([]byte, 0, size)
	body = append(body, e.open...)
	for i := range batch {
		if i > 0 {
			body = append(body, ',')
		}
		body = append(body, batch[i].encoded...)
		// The body holds the span's bytes from here, and its release gives back what
		// the body holds: the queue lets go of its copy.
		batch[i].encoded = nil
	}
	body = append(body, e.closing...)

	n := uint64(len(batch))
	if o := e.client.Export(ctx, body); o.Err != nil {
		e.problems.Fail(n, o.ErrorType, o.Status, o.Err)
	} else {
		rejected := min(o.Rejected, n)
		e.exported.Add(n - rejected)
		if rejected > 0 {
			e.problems.Fail(rejected, otlp.ErrorRejected, o.Status, o.RejectErr)
		}
	}
	for _, q := range batch {
		q.release()
	}
	// batch is the front of the queue's array: clearing it drops the callbacks too.
	clear(batch)
}
