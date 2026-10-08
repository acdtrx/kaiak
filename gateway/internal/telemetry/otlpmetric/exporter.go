// Package otlpmetric exports a metric registry to an OpenTelemetry collector over
// OTLP/HTTP with JSON encoding, at a fixed interval (docs/specs/GATEWAY.md,
// Observability → OTLP metric export). It is a periodic reader of the registry:
// each export collects once, applies the temporality preference with its own state
// — a scrape of the same registry never sees it — encodes the collect, split into
// requests of at most 4 MiB, and posts them through the signal's OTLP connection
// (otlp), which holds the delivery rules. Every data point it sends is counted:
// exported, or failed by error.type (Counts).
package otlpmetric

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"kaiak/internal/telemetry/metric"
	"kaiak/internal/telemetry/otlp"
)

// reportInterval is the least time between two `metric export failing` lines.
const reportInterval = time.Minute

// options are the exporter's timing and its request bound; tests replace them.
type options struct {
	// tick fires an export; nil is a ticker at the settings' interval. A tick is the
	// time it was due: one due while an export ran is skipped.
	tick <-chan time.Time
	// now is the clock problem reports are rate-limited by.
	now func() time.Time
	// maxRequestSize bounds one request's body; 0 is maxRequestSize.
	maxRequestSize int
}

// Exporter collects a registry on an interval and sends each collect to the
// collector in the background. ForceFlush and Shutdown may be called from any
// goroutine.
type Exporter struct {
	client        *otlp.Client
	registry      *metric.Registry
	reader        *reader
	exportTimeout time.Duration
	report        *slog.Logger
	opts          options

	flushes      chan flush
	stop         context.CancelFunc
	done         chan struct{}
	shutdownOnce sync.Once

	// The counts: data points exported and failed (in all), and failed by
	// error.type.
	exported, failed atomic.Uint64
	failedMu         sync.Mutex
	failedBy         map[string]uint64

	// Problem reporting: the sender's alone, and Shutdown's once the sender has
	// stopped.
	lastStatus     int
	lastError      string
	lastReport     time.Time
	reportedFailed uint64
}

// flush is a ForceFlush's request: export now, within ctx.
type flush struct {
	ctx   context.Context
	reply chan error
}

// errShutdown is ForceFlush's answer once Shutdown has run.
var errShutdown = errors.New("otlpmetric: exporter shut down")

// New starts an exporter sending reg's metrics to the collector s names, under a
// resource made of s and svc, one interval after it starts and every interval
// after. Export problems are written to report. Shutdown stops the exporter.
func New(s *otlp.Settings, svc otlp.Service, reg *metric.Registry, report *slog.Logger) *Exporter {
	return newExporter(s, svc, reg, report, options{})
}

func newExporter(s *otlp.Settings, svc otlp.Service, reg *metric.Registry, report *slog.Logger, opts options) *Exporter {
	if opts.now == nil {
		opts.now = time.Now
	}
	if opts.maxRequestSize == 0 {
		opts.maxRequestSize = maxRequestSize
	}
	ctx, stop := context.WithCancel(context.Background())
	e := &Exporter{
		client:        otlp.NewClient(s, svc),
		registry:      reg,
		reader:        newReader(s.Temporality()),
		exportTimeout: s.ExportTimeout(),
		report:        report,
		opts:          opts,
		flushes:       make(chan flush),
		stop:          stop,
		done:          make(chan struct{}),
	}
	tick := opts.tick
	var ticker *time.Ticker
	if tick == nil {
		ticker = time.NewTicker(s.Interval())
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

// Counts are the data points the exporter sent, by what became of them, as the
// SDK's own metrics count them (docs/specs/GATEWAY.md, Observability → Exporters'
// own counts).
type Counts struct {
	// Exported are the data points the collector accepted; Failed those in a
	// request that failed or a partial success's rejections, by error.type
	// (otlp.Outcome).
	Exported uint64
	Failed   map[string]uint64
}

// Counts are the data points by what became of them, since the exporter started.
// An export's points are counted once it ends.
func (e *Exporter) Counts() Counts {
	c := Counts{Exported: e.exported.Load()}
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

// ForceFlush collects and exports now, after the export in flight if there is one,
// and returns nil once that export has ended — delivered or failed, as the counts
// and reports tell — or ctx's error when ctx ends first. The export itself ends
// with ctx: its points not delivered by then count as failed.
func (e *Exporter) ForceFlush(ctx context.Context) error {
	f := flush{ctx: ctx, reply: make(chan error, 1)}
	select {
	case e.flushes <- f:
	case <-ctx.Done():
		return ctx.Err()
	case <-e.done:
		return errShutdown
	}
	select {
	case err := <-f.reply:
		if err != nil {
			return err
		}
		return ctx.Err()
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Shutdown stops the exporter, cutting short an export in flight (its points not
// delivered count as failed), and writes the last problem report. It exports
// nothing itself: the final export is ForceFlush's. Shutdown waits for the sender
// and is safe to call more than once. It does not consult ctx: cutting the export
// in flight is what stops the sender, so the wait never depends on the collector.
func (e *Exporter) Shutdown(ctx context.Context) {
	e.shutdownOnce.Do(func() {
		e.stop()
		<-e.done
		e.client.CloseIdleConnections()
		// The report at exit is never held back.
		e.writeReport()
	})
}

// run is the sender: one export at a time, at each tick that was not due while an
// export ran, and at each flush. Once stopped it starts no export: a tick or flush
// that came with the stop would only count its points failed.
func (e *Exporter) run(ctx context.Context, tick <-chan time.Time) {
	var lastEnd time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case due := <-tick:
			if ctx.Err() != nil {
				return
			}
			if due.Before(lastEnd) {
				continue
			}
			e.export(ctx)
			lastEnd = time.Now()
		case f := <-e.flushes:
			if ctx.Err() != nil {
				f.reply <- errShutdown
				return
			}
			fctx, cancel := context.WithCancel(ctx)
			stop := context.AfterFunc(f.ctx, cancel)
			e.export(fctx)
			stop()
			cancel()
			lastEnd = time.Now()
			f.reply <- nil
		}
	}
}

// export collects the registry and sends the collect, its requests one after the
// other within the export's timeout; each request's points count by its own
// outcome.
func (e *Exporter) export(ctx context.Context) {
	snap := e.registry.Collect()
	streams := e.reader.streams(snap)
	requests, err := encodeRequests(e.client.Resource(), snap.Time, streams, e.opts.maxRequestSize)
	if err != nil {
		n := 0
		for _, s := range streams {
			n += len(s.points)
		}
		e.fail(uint64(n), errorTypeOther, 0, fmt.Errorf("encode export: %w", err))
		e.reportProblems()
		return
	}
	ctx, cancel := context.WithTimeout(ctx, e.exportTimeout)
	defer cancel()
	for _, r := range requests {
		n := uint64(r.points)
		o := e.client.Export(ctx, r.body)
		if o.Err != nil {
			e.fail(n, o.ErrorType, o.Status, o.Err)
			continue
		}
		rejected := min(o.Rejected, n)
		e.exported.Add(n - rejected)
		if rejected > 0 {
			e.fail(rejected, otlp.ErrorRejected, o.Status, o.RejectErr)
		}
	}
	e.reportProblems()
}

// errorTypeOther is the error.type of a failure outside the export's classes (a
// collect that could not be encoded): the convention's fallback value.
const errorTypeOther = "_OTHER"

// fail counts n data points as failed, of class errorType, and keeps the reason
// for the next report.
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

// reportProblems writes `metric export failing` when data points failed since the
// last such line, at most once per reportInterval.
func (e *Exporter) reportProblems() {
	if !e.lastReport.IsZero() && e.opts.now().Sub(e.lastReport) < reportInterval {
		return
	}
	e.writeReport()
}

// writeReport writes `metric export failing` when data points failed since the
// last such line: how many, the collector's last status when it answered, and the
// last error in the gateway's own words.
func (e *Exporter) writeReport() {
	failed := e.failed.Load()
	newFailed := failed - e.reportedFailed
	if newFailed == 0 {
		return
	}
	attrs := []slog.Attr{slog.Uint64("kaiak.metric_export.failed", newFailed)}
	if e.lastStatus != 0 {
		attrs = append(attrs, slog.Int("http.response.status_code", e.lastStatus))
	}
	if e.lastError != "" {
		attrs = append(attrs, slog.String("exception.message", e.lastError))
	}
	e.report.LogAttrs(context.Background(), slog.LevelWarn, "metric export failing", attrs...)
	e.reportedFailed = failed
	e.lastReport = e.opts.now()
	e.lastStatus, e.lastError = 0, ""
}
