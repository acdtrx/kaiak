// Package capture makes request captures (docs/specs/GATEWAY.md, Request capture):
// for a request whose key's group captures, a copy of what the client sent and of
// what it received, exported as one OpenTelemetry span.
//
// On the request's path a capture only copies. The server's capture stage starts it
// (Captures.Start: the body copied, the session header read); the client writer
// hands it each piece written to the client (Capture.Append); the stage's finisher
// hands it, with how the request ended, to a background builder (Capture.Hand) and
// does nothing more — finishers run before the response's end reaches the client,
// so building there would delay it. A body larger than the exporter can send in a
// span is not copied at all: the span carries the request's content nearly whole.
// Every byte a capture holds is taken from one budget first; a take that does not
// fit drops the capture and gives back what it held, and the request goes on
// untouched.
//
// The builder makes each capture's span, encodes it, swaps its budget charge from
// the copies to the encoded span and queues it with the trace exporter, whose
// release gives those bytes back once the export ends. A request holding more array
// elements and object members than a build may read is dropped before any of it is
// read: each costs tens of bytes while the span is built. A fault in one build — a
// panic in reading the content, an encoding that fails — drops that capture alone:
// the builder goes on with the next, and serving never sees it. At exit Finish
// builds what was handed before the trace export's final flush.
package capture

import (
	"bytes"
	"context"
	"log/slog"
	"runtime/debug"
	"sync"
	"sync/atomic"

	"kaiak/internal/provider"
	"kaiak/internal/telemetry/otlptrace"
)

// DefaultMemory is the capture budget unless KAIAK_CAPTURE_MEMORY_BYTES sets it.
const DefaultMemory int64 = 128 << 20

// The request line's kaiak.capture (docs/specs/GATEWAY.md, Observability → Logs: the
// request line). The line is written before the span is built: what the builder
// drops is in the counts alone (Drops).
const (
	// OutcomeQueued: the capture was handed to the builder.
	OutcomeQueued = "queued"
	// OutcomeMemory: the budget could not hold the copy of the request or of the
	// answer while the request ran.
	OutcomeMemory = "memory"
	// OutcomeTooLarge: the request's body is larger than a span the exporter can
	// send — nothing is copied.
	OutcomeTooLarge = "too_large"
	// OutcomeExportOff: the key's group captures, but this gateway's trace export is
	// off — nothing is copied.
	OutcomeExportOff = "export_off"
)

// Exporter takes the spans the builder makes: the trace exporter
// (otlptrace.Exporter).
type Exporter interface {
	// MaxSpanBytes is the largest encoded span it can send. It never changes: New
	// reads it once.
	MaxSpanBytes() int
	// Enqueue queues an encoded span without waiting; release gives its bytes back
	// once its export has ended — possibly within Enqueue.
	Enqueue(encoded []byte, release func())
}

// Captures makes the gateway's request captures, every one held within one budget,
// built by one background builder and queued with one exporter. Its methods may be
// called from any goroutine.
type Captures struct {
	budget   budget
	exporter Exporter
	// maxSpan is the exporter's MaxSpanBytes: no span over it is queued, and no
	// request body over it copied.
	maxSpan int
	// logger reports a build that failed (failBuild).
	logger *slog.Logger
	// readContent reads a capture's content attributes: content; tests replace it.
	readContent func(provider.Endpoint, exchange) []otlptrace.Attribute

	mu sync.Mutex
	// backlog is the captures handed and not yet built, oldest first. It has no
	// bound of its own: each capture in it holds its bytes in the budget.
	backlog []job
	// finishing: Finish has begun — the builder stops once the backlog is empty.
	// stopped: nothing more is built; a capture handed now is dropped.
	finishing, stopped bool
	// wake tells the builder a capture was handed, or that Finish began.
	wake chan struct{}
	// built is closed once the builder has returned.
	built chan struct{}

	// The captures dropped before the exporter, by why (Drops).
	droppedMemory, droppedTooLarge, droppedShutdown, droppedFailed atomic.Uint64
}

// job is a capture handed to the builder: its copies, the bytes they hold in the
// budget, its session and how its request ended.
type job struct {
	api                     provider.Endpoint
	request, response       []byte
	held                    int64
	session, sessionIgnored string
	summary                 Summary
}

// New starts the builder of the captures held within memory bytes and queued with
// exporter; a build that fails is reported to logger. Finish stops it.
func New(memory int64, exporter Exporter, logger *slog.Logger) *Captures {
	cs := &Captures{budget: budget{limit: memory}, exporter: exporter, maxSpan: exporter.MaxSpanBytes(), logger: logger,
		readContent: content, wake: make(chan struct{}, 1), built: make(chan struct{})}
	go cs.runBuilder()
	return cs
}

// Capture is one request's capture while the request runs: the copy of its body and
// of the bytes written to its client, held in the budget, and its session. Its
// methods are called from the request's goroutine alone.
type Capture struct {
	captures *Captures
	api      provider.Endpoint
	// request is the body as the client sent it; response every byte written to the
	// client, in a buffer that grows by doubling.
	request, response []byte
	// held is the bytes taken from the budget: the request's length and the response
	// buffer's capacity.
	held                    int64
	session, sessionIgnored string
	// dropped is why the capture was dropped — OutcomeTooLarge or OutcomeMemory —
	// having given back everything it held; "" while it is kept.
	dropped string
	// handed: Hand passed its bytes to the builder, which owns them now.
	handed bool
}

// Start captures a request to api — chat completions, Messages or Responses — whose
// body is body, as the client sent it, and whose Kaiak-Session-Id headers are
// sessionValues. A body larger than the exporter can send in a span drops the
// capture at once, nothing copied (Hand returns OutcomeTooLarge): the span carries
// the request's content nearly whole, so it could not fit, and copying, holding and
// building it would only spend memory. Else the copy of the body is taken from the
// budget; when it does not fit, the capture is dropped at once (OutcomeMemory).
func (cs *Captures) Start(api provider.Endpoint, body []byte, sessionValues []string) *Capture {
	c := &Capture{captures: cs, api: api}
	c.session, c.sessionIgnored = session(sessionValues)
	if len(body) > cs.maxSpan {
		c.drop(OutcomeTooLarge)
		return c
	}
	size := int64(len(body))
	if !cs.budget.take(size) {
		c.drop(OutcomeMemory)
		return c
	}
	c.request, c.held = bytes.Clone(body), size
	return c
}

// responseStep is the first size of a capture's response buffer.
const responseStep = 4 << 10

// Append adds p, bytes just written to the client, to the copy of the answer. The
// buffer doubles from responseStep as the answer grows, each growth taken from the
// budget before it is made; a growth the budget cannot hold drops the capture, which
// gives back everything it held. The client's write is never affected.
func (c *Capture) Append(p []byte) {
	if c.dropped != "" || c.handed || len(p) == 0 {
		return
	}
	if need := len(c.response) + len(p); need > cap(c.response) {
		size := max(2*cap(c.response), need, responseStep)
		growth := int64(size - cap(c.response))
		if !c.captures.budget.take(growth) {
			c.drop(OutcomeMemory)
			return
		}
		c.held += growth
		grown := make([]byte, len(c.response), size)
		copy(grown, c.response)
		c.response = grown
	}
	c.response = append(c.response, p...)
}

// drop drops the capture for why — OutcomeMemory or OutcomeTooLarge: it gives back
// everything it held, and is counted.
func (c *Capture) drop(why string) {
	c.captures.budget.give(c.held)
	c.request, c.response, c.held, c.dropped = nil, nil, 0, why
	if why == OutcomeTooLarge {
		c.captures.droppedTooLarge.Add(1)
		return
	}
	c.captures.droppedMemory.Add(1)
}

// SessionIgnored is why the request's Kaiak-Session-Id was ignored — "empty",
// "too_long" or "invalid_characters" — or "" when it was valid or not sent.
func (c *Capture) SessionIgnored() string { return c.sessionIgnored }

// Hand ends the capture's part on the request's path: it passes the copies — their
// bytes still held in the budget — with s, how the request ended, to the builder,
// and returns the request line's kaiak.capture: OutcomeQueued, or why the capture
// was dropped — OutcomeTooLarge for a body too large to send, OutcomeMemory for a
// capture the budget could not hold. It does nothing else: it runs in a finisher,
// before the response's end reaches the client.
func (c *Capture) Hand(s Summary) string {
	if c.dropped != "" {
		return c.dropped
	}
	j := job{api: c.api, request: c.request, response: c.response, held: c.held, session: c.session,
		sessionIgnored: c.sessionIgnored, summary: s}
	c.request, c.response, c.held, c.handed = nil, nil, 0, true
	c.captures.mu.Lock()
	if c.captures.stopped {
		c.captures.mu.Unlock()
		c.captures.dropAtShutdown(j)
		return OutcomeQueued
	}
	c.captures.backlog = append(c.captures.backlog, j)
	c.captures.mu.Unlock()
	c.captures.signal()
	return OutcomeQueued
}

// signal wakes the builder, if it is not already due to wake.
func (cs *Captures) signal() {
	select {
	case cs.wake <- struct{}{}:
	default:
	}
}

// runBuilder builds the handed captures one at a time, oldest first, until Finish
// stops it.
func (cs *Captures) runBuilder() {
	defer close(cs.built)
	for {
		j, ok := cs.next()
		if !ok {
			return
		}
		cs.build(j)
	}
}

// next waits for the oldest capture not yet built; false once Finish has begun and
// none is left, or Finish's bound has ended.
func (cs *Captures) next() (job, bool) {
	for {
		cs.mu.Lock()
		if len(cs.backlog) > 0 {
			j := cs.backlog[0]
			cs.backlog[0] = job{}
			cs.backlog = cs.backlog[1:]
			if len(cs.backlog) == 0 {
				cs.backlog = nil
			}
			cs.mu.Unlock()
			return j, true
		}
		if cs.finishing {
			cs.stopped = true
			cs.mu.Unlock()
			return job{}, false
		}
		cs.mu.Unlock()
		<-cs.wake
	}
}

// maxRequestValues is the most array elements and object members a captured
// request's body may hold for its span to be built. Reading the request costs tens
// of bytes of Go values per element or member, however small it is in the body, so
// without a bound a body of many tiny values would hold many times its size while
// it is built (docs/specs/GATEWAY.md, Request capture → The request's values). A
// body over it is dropped as too_large, nothing read.
const maxRequestValues = 250_000

// countValues is an upper bound on body's array elements and object members: every
// ',', '[' and '{' outside strings — each element or member but its container's
// first follows a comma, and the first follows its container's opening. One pass
// counts them, allocating nothing, and stops as soon as the count passes limit,
// returning limit+1.
func countValues(body []byte, limit int) int {
	n := 0
	for i := 0; i < len(body); i++ {
		switch body[i] {
		case '"':
			// To the string's closing quote, an escaped character skipped with its
			// backslash.
			for i++; i < len(body) && body[i] != '"'; i++ {
				if body[i] == '\\' {
					i++
				}
			}
		case ',', '[', '{':
			if n++; n > limit {
				return n
			}
		}
	}
	return n
}

// build makes j's span and queues it with the exporter, or drops it: a request of
// more values than a build may read (maxRequestValues), before any of it is read;
// too large for the exporter to send; too large — once its charge moves from the
// copies to the encoded span — for what the budget has left; or failed, a fault in
// building it (failBuild): a panic anywhere in the build — the content's reading
// above all, which parses what clients sent — is recovered here, so that one
// capture's fault never stops the builder, let alone the gateway. Whatever the
// capture holds in the budget goes back on every drop.
func (cs *Captures) build(j job) {
	if countValues(j.request, maxRequestValues) > maxRequestValues {
		cs.budget.give(j.held)
		cs.droppedTooLarge.Add(1)
		return
	}
	// charged is what the capture holds in the budget now: its copies, then its
	// encoded span, then nothing — from Enqueue on, the exporter's release gives the
	// span back, and a fault there must not give it twice.
	charged := j.held
	defer func() {
		if recover() != nil {
			cs.failBuild(j.summary.RequestID, charged)
		}
	}()
	encoded, err := otlptrace.Encode(j.span(cs.readContent))
	if err != nil {
		cs.failBuild(j.summary.RequestID, charged)
		return
	}
	if len(encoded) > cs.maxSpan {
		cs.budget.give(charged)
		charged = 0
		cs.droppedTooLarge.Add(1)
		return
	}
	size := int64(len(encoded))
	if !cs.budget.swap(charged, size) {
		charged = 0
		cs.droppedMemory.Add(1)
		return
	}
	charged = 0
	// Enqueue may release at once (an exporter already shut down): no lock is held
	// here that release takes.
	cs.exporter.Enqueue(encoded, func() { cs.budget.give(size) })
}

// failBuild drops a capture whose build failed — a fault in the gateway — giving
// back the charged bytes it held, and reports it: one error line with the request's
// ID and the builder's stack, never the fault's value or an error's text, which a
// parser can fill with what the client sent.
func (cs *Captures) failBuild(requestID string, charged int64) {
	cs.budget.give(charged)
	cs.droppedFailed.Add(1)
	cs.logger.Error("capture build failed", "kaiak.request.id", requestID, "exception.stacktrace", string(debug.Stack()))
}

// dropAtShutdown drops a capture handed and never built: its bytes go back to the
// budget, and it is counted.
func (cs *Captures) dropAtShutdown(j job) {
	cs.budget.give(j.held)
	cs.droppedShutdown.Add(1)
}

// Finish builds the captures handed so far, then stops the builder, and returns
// once it has stopped. When ctx ends first, the captures not yet built are dropped
// — counted as shutdown, their bytes given back — and a build in progress
// completes. A capture handed later is dropped the same way. Finish may be called
// more than once.
func (cs *Captures) Finish(ctx context.Context) {
	cs.mu.Lock()
	cs.finishing = true
	cs.mu.Unlock()
	cs.signal()
	select {
	case <-cs.built:
		return
	case <-ctx.Done():
	}
	cs.mu.Lock()
	cs.stopped = true
	left := cs.backlog
	cs.backlog = nil
	cs.mu.Unlock()
	for _, j := range left {
		cs.dropAtShutdown(j)
	}
	<-cs.built
}

// Drops are the captures dropped before the exporter since New, by why
// (docs/specs/GATEWAY.md, Observability → Exporters' own counts): the exporter
// counts the rest.
type Drops struct {
	// Memory: the budget could not hold one — its copies while its request ran, or
	// its encoded span. TooLarge: its request's body, or its encoded span, exceeds
	// what the exporter can send, or its request holds more values than a build may
	// read (maxRequestValues). Shutdown: handed and not built when Finish's bound
	// ended. Failed: its build failed, a fault in the gateway.
	Memory, TooLarge, Shutdown, Failed uint64
}

// Drops are the captures dropped before the exporter, by why.
func (cs *Captures) Drops() Drops {
	return Drops{Memory: cs.droppedMemory.Load(), TooLarge: cs.droppedTooLarge.Load(),
		Shutdown: cs.droppedShutdown.Load(), Failed: cs.droppedFailed.Load()}
}

// InUse is the bytes the captures hold now: copies of requests and answers, and
// encoded spans until their export ends.
func (cs *Captures) InUse() int64 { return cs.budget.inUse() }
