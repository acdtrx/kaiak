package otlp

import (
	"context"
	"log/slog"
	"maps"
	"sync"
	"time"
)

// reportInterval is the least time between two `… export failing` lines.
const reportInterval = time.Minute

// ProblemLine names an exporter's `… export failing` line and its counts.
type ProblemLine struct {
	// Message is the line's message, such as "log export failing".
	Message string
	// Failed and Dropped are the keys of its counts: the items failed, and dropped,
	// since the previous line.
	Failed, Dropped string
}

// Problems are the export problems of an exporter that queues what it exports
// (docs/specs/GATEWAY.md, Observability → OTLP log export and OTLP trace export:
// export problems): its failed items by error.type, and the warn line that reports
// them with its drops — at the first failure or drop, then at most once a minute
// while they continue, with the items failed and dropped since the previous line,
// the collector's last status when it answered and the last error in the gateway's
// own words. The line at exit is never held back (ReportNow). Problems is safe for
// concurrent use.
type Problems struct {
	report  *slog.Logger
	line    ProblemLine
	dropped func() uint64
	now     func() time.Time

	mu       sync.Mutex
	failed   uint64
	failedBy map[string]uint64
	// What the next line says, and what the previous one did.
	lastStatus      int
	lastError       string
	lastReport      time.Time
	reportedFailed  uint64
	reportedDropped uint64
}

// NewProblems is the tally of an exporter whose line is line, written to report.
// dropped is the number of items the exporter has dropped since it started; now is
// the clock the lines are rate-limited by, nil for the system's.
func NewProblems(report *slog.Logger, line ProblemLine, dropped func() uint64, now func() time.Time) *Problems {
	if now == nil {
		now = time.Now
	}
	return &Problems{report: report, line: line, dropped: dropped, now: now}
}

// Fail counts n items failed, of class errorType (Outcome.ErrorType), and keeps
// status and err for the next line.
func (p *Problems) Fail(n uint64, errorType string, status int, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.failed += n
	if p.failedBy == nil {
		p.failedBy = map[string]uint64{}
	}
	p.failedBy[errorType] += n
	p.lastStatus = status
	p.lastError = err.Error()
}

// Failed are the items failed since the exporter started, by error.type; nil when
// none has.
func (p *Problems) Failed() map[string]uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.failedBy) == 0 {
		return nil
	}
	return maps.Clone(p.failedBy)
}

// Report writes the line when items failed or were dropped since the previous one,
// unless that one was written less than a minute ago.
func (p *Problems) Report() {
	p.write(false)
}

// ReportNow writes the line when items failed or were dropped since the previous
// one, whatever the minute says: the report at exit is never held back, so drops
// at exit are never silent.
func (p *Problems) ReportNow() {
	p.write(true)
}

func (p *Problems) write(always bool) {
	attrs := p.next(always)
	if attrs == nil {
		return
	}
	p.report.LogAttrs(context.Background(), slog.LevelWarn, p.line.Message, attrs...)
}

// next is the line to write, nil when there is none — or, unless always, when the
// previous one is less than a minute old; taking it starts the next line's tally
// and minute.
func (p *Problems) next(always bool) []slog.Attr {
	p.mu.Lock()
	defer p.mu.Unlock()
	at := p.now()
	if !always && !p.lastReport.IsZero() && at.Sub(p.lastReport) < reportInterval {
		return nil
	}
	dropped := p.dropped()
	newFailed, newDropped := p.failed-p.reportedFailed, dropped-p.reportedDropped
	if newFailed == 0 && newDropped == 0 {
		return nil
	}
	attrs := []slog.Attr{
		slog.Uint64(p.line.Failed, newFailed),
		slog.Uint64(p.line.Dropped, newDropped),
	}
	if p.lastStatus != 0 {
		attrs = append(attrs, slog.Int("http.response.status_code", p.lastStatus))
	}
	if p.lastError != "" {
		attrs = append(attrs, slog.String("exception.message", p.lastError))
	}
	p.reportedFailed, p.reportedDropped = p.failed, dropped
	p.lastReport = at
	p.lastStatus, p.lastError = 0, ""
	return attrs
}
