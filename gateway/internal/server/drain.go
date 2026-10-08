package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"kaiak/internal/logattr"
	"kaiak/internal/metrics"
)

// Drain phases (docs/specs/GATEWAY.md, Lifecycle).
const (
	phaseServing  int32 = iota
	phaseDraining       // not ready; new requests still admitted (the grace period)
	phaseRefusing       // new requests refused with 503; in-flight ones finish
)

// errDrainCut is the cancellation cause of requests the drain cuts off: relays and
// upstream calls stopped by it end as "shutdown", not as a client that left.
var errDrainCut = errors.New("request cut off by the drain")

// Drain is the API's shutdown state: which drain phase it is in and how many client
// requests are in flight. A request counts from the moment the API handler takes it
// until the handler returns — after its finishers, so its usage record is settled and
// its limit reservation released. The API handler, the admin readiness check and Run
// share one Drain.
type Drain struct {
	phase atomic.Int32

	mu       sync.Mutex
	inFlight int
	// idle is closed while inFlight is 0; a new one is made when a request arrives.
	idle chan struct{}
}

// NewDrain returns the state of a serving gateway: ready, admitting, nothing in
// flight.
func NewDrain() *Drain {
	idle := make(chan struct{})
	close(idle)
	return &Drain{idle: idle}
}

// Draining reports whether the drain has begun: the gateway is no longer ready.
func (d *Drain) Draining() bool { return d.phase.Load() != phaseServing }

func (d *Drain) refusing() bool { return d.phase.Load() == phaseRefusing }

// enter counts a request in; exit counts it out.
func (d *Drain) enter() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.inFlight == 0 {
		d.idle = make(chan struct{})
	}
	d.inFlight++
}

func (d *Drain) exit() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.inFlight--
	if d.inFlight == 0 {
		close(d.idle)
	}
}

// idleNow returns a channel closed once no request is in flight (already closed when
// none is), and the number in flight now.
func (d *Drain) idleNow() (<-chan struct{}, int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.idle, d.inFlight
}

// DrainTimes are the drain's two waits.
type DrainTimes struct {
	// Grace: how long new requests are still admitted after readiness fails, while
	// load balancers take the instance out of rotation.
	Grace time.Duration
	// Timeout: how long in-flight requests get to finish once new ones are refused.
	Timeout time.Duration
	// Reserve: the end of Timeout kept for what follows the drain (the usage flush,
	// in control-plane mode): requests still running at Timeout − Reserve are cut off.
	Reserve time.Duration
}

// Run drains the API listener l, whose handler serves through d, and returns once
// every request it took is over and l has stopped:
//
//  1. readiness fails (reason "draining"); responses carry Connection: close;
//  2. new requests are still admitted for the grace period;
//  3. l stops accepting connections (new ones are refused by the OS); a request that
//     still arrives on an open keep-alive connection is answered 503
//     server_shutting_down and its connection closed;
//  4. in-flight requests get the drain timeout less the reserve to finish; those
//     still running then are cut off: their upstream calls are cancelled, their
//     connections closed, their usage settled as partial.
//
// hurry skips whatever waiting remains once it is closed (a second signal): the
// requests still in flight are cut off at once. Run is the mechanism; what triggers
// it is the caller's. The result is logged.
func (d *Drain) Run(l *Listener, t DrainTimes, hurry <-chan struct{}, logger *slog.Logger) {
	d.begin(t, logger)
	grace := time.NewTimer(t.Grace)
	select {
	case <-grace.C:
	case <-hurry:
	}
	grace.Stop()
	d.refuse(l, logger)
	d.finish(l, t.Timeout-t.Reserve, hurry, logger)
}

// begin fails readiness; requests are still admitted.
func (d *Drain) begin(t DrainTimes, logger *slog.Logger) {
	d.phase.Store(phaseDraining)
	_, n := d.idleNow()
	logger.Info("draining", logattr.Seconds("kaiak.drain.grace", t.Grace), logattr.Seconds("kaiak.drain.timeout", t.Timeout),
		logattr.Seconds("kaiak.drain.flush_reserve", t.Reserve), logattr.Seconds("kaiak.drain.cut_after", t.Timeout-t.Reserve),
		"kaiak.drain.in_flight", n)
}

// refuse stops l accepting connections and refuses requests still arriving on open
// ones.
func (d *Drain) refuse(l *Listener, logger *slog.Logger) {
	d.phase.Store(phaseRefusing)
	l.stopAccepting()
	_, n := d.idleNow()
	logger.Info("draining: refusing new requests", "kaiak.drain.in_flight", n)
}

// finish waits up to timeout for the requests in flight, cuts off those left (at
// once when hurry is closed) and stops l.
func (d *Drain) finish(l *Listener, timeout time.Duration, hurry <-chan struct{}, logger *slog.Logger) {
	deadline := time.Now().Add(timeout)
	idle, _ := d.idleNow()
	reason := ""
	timer := time.NewTimer(timeout)
	select {
	case <-idle:
	case <-timer.C:
		reason = "timeout"
	case <-hurry:
		reason = "hurried"
	}
	timer.Stop()
	// A hurry that came with nothing in flight has nothing to cut.
	if reason != "" {
		select {
		case <-idle:
			reason = ""
		default:
		}
	}

	if reason == "" {
		// Handlers are done; connections may still be writing their last bytes.
		// Shutdown closes the idle ones and waits for those, up to the deadline.
		ctx, cancel := context.WithDeadline(context.Background(), deadline)
		defer cancel()
		if err := l.server.Shutdown(ctx); errors.Is(err, context.DeadlineExceeded) {
			logger.Warn("drain: connections still writing at the timeout; closing them")
			l.cut()
		}
		logger.Info("drained")
		return
	}
	idle, n := d.idleNow()
	logger.Warn("drain: cutting off in-flight requests", "kaiak.reason", reason, "kaiak.drain.in_flight", n)
	l.cut()
	// Bounded: a cut-off handler's upstream call is cancelled and its client
	// connection closed, so whatever it was blocked on returns.
	<-idle
	logger.Info("drained", "kaiak.drain.cut_off", n)
}

// errShuttingDown answers a request that arrives once the drain refuses new ones.
func errShuttingDown() *apiError {
	return &apiError{status: http.StatusServiceUnavailable, errType: typeServer, code: "server_shutting_down", class: metrics.ErrorShuttingDown,
		message: "The gateway is shutting down; retry the request."}
}

// errConfigNotLoaded answers a request that arrives with no config in force. The
// binary binds its listeners only once a config is in force; the handler still
// refuses rather than serve without one.
func errConfigNotLoaded() *apiError {
	return &apiError{status: http.StatusServiceUnavailable, errType: typeServer, code: "config_not_loaded", class: metrics.ErrorNotReady,
		message: "The gateway has no config yet; retry the request."}
}

// admit is the pipeline's first stage: once the drain refuses new requests, and while
// no config is in force, they end here, before anything is read or reserved.
func admit(d *Drain, rq *request) *apiError {
	if d.refusing() {
		return errShuttingDown()
	}
	if rq.snapshot == nil {
		return errConfigNotLoaded()
	}
	return nil
}

// cutOff reports whether ctx was cancelled by the drain cutting the request off.
func cutOff(ctx context.Context) bool {
	return errors.Is(context.Cause(ctx), errDrainCut)
}
