package routing

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"kaiak/internal/config"
	"kaiak/internal/logattr"
)

// Outcome is what a request's result says about its deployment's health, as the
// request pipeline classifies it (docs/specs/GATEWAY.md, Routing and reliability).
type Outcome int

const (
	// Neutral says nothing about the deployment (backend 429, caller 4xx, the
	// client gone, the drain's cut): the failure count is left as it is.
	Neutral Outcome = iota
	// Success: the backend answered and the response was relayed to its end. It
	// resets the failure count and the response-timeout run.
	Success
	// Failure: the backend is broken (connect error, a stream's first-event timeout,
	// 5xx, its credential refused, the response broken off upstream, stalled or
	// ended incomplete).
	Failure
	// ResponseTimeout: a non-stream response ran into its response timeout. Alone it
	// is neutral — a long generation is the backend working — but it is a failure
	// for a half-open trial, and from the responseTimeoutsAsFailure-th in a row on
	// (no success between): a backend that answers nothing, run after run, is hung.
	ResponseTimeout
)

// responseTimeoutsAsFailure is the length of a run of response timeouts on one
// deployment, with no success between, from which each counts as a failure: one or
// two are long generations; three in a row, with nothing succeeding, is a hung
// engine behind a live models list (docs/specs/GATEWAY.md, Routing and reliability:
// outcome classes).
const responseTimeoutsAsFailure = 3

// circuit is one deployment's breaker state.
type circuit struct {
	// failures counts consecutive failures while closed; responseTimeouts the run
	// of response timeouts since the last success.
	failures         int
	responseTimeouts int
	// openedAt is when the circuit opened; zero while closed. A half-open circuit
	// keeps it: it counts as open until a trial closes it.
	openedAt time.Time
	// halfOpen: a probe succeeded since the circuit opened; the deployment takes one
	// trial request at a time. trial is the one under way, 0 for none.
	halfOpen bool
	trial    uint64
	// unlisted: a probe found the backend not listing the deployment's model since
	// the circuit opened (logged once).
	unlisted bool
}

// noObserver is the Observer of a router given none.
type noObserver struct{}

func (noObserver) CircuitChanged(DeploymentID, CircuitState) {}
func (noObserver) Probed(string, bool)                       {}

// report counts one request's outcome on d; trial is the half-open trial the
// request carries, 0 for none.
func (r *Router) report(d config.Deployment, trial uint64, o Outcome, reason string) {
	r.mu.Lock()
	ts, changed := r.reportLocked(IDOf(d), trial, o, reason)
	r.mu.Unlock()
	r.emit(ts, changed)
}

// reportLocked counts an outcome on key (report). The threshold-th consecutive
// failure opens key's circuit and starts its backend's prober. Outcomes on an open
// circuit change nothing, except the trial's on a half-open one: its success closes
// the circuit, its failure (a response timeout included) opens it again, a neutral
// outcome lets another request try. A trial decided when its response started
// (ResponseStarted) reports like any request. Deployments the applied config does not
// have (a request still running under an older snapshot) are not tracked. It returns
// what emit reports. Called with r.mu held.
func (r *Router) reportLocked(key DeploymentID, trial uint64, o Outcome, reason string) (ts []transition, changed bool) {
	if !r.deployments[key] {
		return nil, false
	}
	c := r.circuits[key]
	if c != nil && trial != 0 && c.trial == trial {
		return r.endTrial(key, c, o, reason)
	}
	if o == Neutral {
		return nil, false
	}
	if o == Success {
		if c != nil && c.openedAt.IsZero() {
			delete(r.circuits, key)
		}
		return nil, false
	}
	if c == nil {
		c = &circuit{}
		r.circuits[key] = c
	}
	if !c.openedAt.IsZero() {
		return nil, false
	}
	if o == ResponseTimeout {
		c.responseTimeouts++
		if c.responseTimeouts < responseTimeoutsAsFailure {
			return nil, false
		}
	}
	c.failures++
	if c.failures < r.circuit.FailureThreshold {
		return nil, false
	}
	c.openedAt = time.Now()
	r.startProber(key.Backend)
	return []transition{{key: key, to: CircuitOpen,
		attrs: []any{"kaiak.circuit.failures", c.failures, "kaiak.circuit.last_error", reason}}}, false
}

// endTrial applies the outcome o of the half-open circuit c's trial on key and
// returns what emit reports. Called with r.mu held.
func (r *Router) endTrial(key DeploymentID, c *circuit, o Outcome, reason string) (ts []transition, changed bool) {
	c.trial = 0
	switch o {
	case Neutral:
		// Nothing learned: the deployment is eligible for the next trial.
		return nil, r.dispatch()
	case Success:
		openFor := time.Since(c.openedAt)
		delete(r.circuits, key)
		if !r.needsProber(key.Backend) {
			r.stopProber(key.Backend)
		}
		return []transition{{key: key, to: CircuitClosed,
			attrs: []any{"kaiak.trigger", "trial", logattr.Seconds("kaiak.circuit.open_duration", openFor)}}}, r.dispatch()
	default:
		r.reopen(c, key.Backend)
		return []transition{{key: key, to: CircuitOpen,
			attrs: []any{"kaiak.circuit.trial", true, "kaiak.circuit.last_error", reason}}}, false
	}
}

// reopen opens the half-open circuit c of a deployment on backend again, its trial
// (if one runs) no longer its trial, and makes sure the backend's prober runs.
// Called with r.mu held.
func (r *Router) reopen(c *circuit, backend string) {
	c.halfOpen, c.unlisted, c.trial = false, false, 0
	c.openedAt = time.Now()
	r.startProber(backend)
}

// responseStarted decides the half-open trial a request carries once its response
// started: a success, so the circuit closes and the deployment serves other requests
// while the trial runs on. A request that is not the trial is left to report when it
// ends.
func (r *Router) responseStarted(d config.Deployment, trial uint64) {
	if trial == 0 {
		return
	}
	key := IDOf(d)
	var ts []transition
	var changed bool
	r.mu.Lock()
	if c := r.circuits[key]; c != nil && c.trial == trial {
		ts, changed = r.endTrial(key, c, Success, "")
	}
	r.mu.Unlock()
	r.emit(ts, changed)
}

// transition is one circuit's change of state, collected under r.mu and reported by
// emit once it is released; attrs are its log line's attributes after the
// deployment's.
type transition struct {
	key   DeploymentID
	to    CircuitState
	attrs []any
}

// emit reports circuit transitions, called without r.mu: each one's log line —
// `circuit opened` (warn), `circuit half-open` or `circuit closed` (info) — and the
// observer, then the serving change: changed (a queue became empty), or a circuit
// that opened or closed (OnServingChange); half-opening alone is not one.
func (r *Router) emit(ts []transition, changed bool) {
	for _, t := range ts {
		level, msg := slog.LevelInfo, "circuit half-open"
		switch t.to {
		case CircuitOpen:
			level, msg = slog.LevelWarn, "circuit opened"
		case CircuitClosed:
			msg = "circuit closed"
		}
		r.logger.Log(context.Background(), level, msg,
			append([]any{"kaiak.backend.id", t.key.Backend, "kaiak.deployment.model", t.key.Model}, t.attrs...)...)
		r.observer.CircuitChanged(t.key, t.to)
		changed = changed || t.to != CircuitHalfOpen
	}
	r.notify(changed)
}

// CircuitReport is a circuit that is not closed: open or half-open (State), and
// when it opened. A half-open circuit keeps its opening time: it opened then and has
// not closed since.
type CircuitReport struct {
	State    CircuitState
	OpenedAt time.Time
}

// Circuits returns the circuits that are not closed, by deployment: open ones, and
// half-open ones (a probe succeeded; the deployment takes one trial at a time).
// Deployments with a closed circuit are absent.
func (r *Router) Circuits() map[DeploymentID]CircuitReport {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[DeploymentID]CircuitReport)
	for key, c := range r.circuits {
		switch {
		case c.openedAt.IsZero():
		case c.halfOpen:
			out[key] = CircuitReport{State: CircuitHalfOpen, OpenedAt: c.openedAt}
		default:
			out[key] = CircuitReport{State: CircuitOpen, OpenedAt: c.openedAt}
		}
	}
	return out
}

// dropRemovedCircuits forgets the circuits of deployments the applied config no
// longer has and stops the probers of backends left without an open or half-open
// circuit. changed reports a dropped open circuit.
func (r *Router) dropRemovedCircuits() (changed bool) {
	for key, c := range r.circuits {
		if r.deployments[key] {
			continue
		}
		delete(r.circuits, key)
		if !c.openedAt.IsZero() {
			changed = true
		}
	}
	for id := range r.probers {
		if !r.needsProber(id) {
			r.stopProber(id)
		}
	}
	return changed
}

// hasOpenCircuit reports whether one of backend's deployments has its circuit open
// and not half-open: one a probe is still needed to half-open.
func (r *Router) hasOpenCircuit(backend string) bool {
	for key, c := range r.circuits {
		if key.Backend == backend && c.probing() {
			return true
		}
	}
	return false
}

// needsProber reports whether one of backend's deployments has its circuit open or
// half-open: the backend's prober runs.
func (r *Router) needsProber(backend string) bool {
	for key, c := range r.circuits {
		if key.Backend == backend && !c.openedAt.IsZero() {
			return true
		}
	}
	return false
}

// probing reports whether c waits for a probe: open, not half-open.
func (c *circuit) probing() bool { return !c.openedAt.IsZero() && !c.halfOpen }

// RunProbers runs the probe timer until ctx ends: one prober per backend with open or
// half-open circuits, started when its first circuit opens (or at once, for circuits
// already open), stopped when none is left open or half-open (trials closed them, a
// reload dropped them) and when ctx ends. RunProbers returns once every prober has
// stopped.
func (r *Router) RunProbers(ctx context.Context) {
	r.mu.Lock()
	r.probeCtx = ctx
	for key, c := range r.circuits {
		if !c.openedAt.IsZero() {
			r.startProber(key.Backend)
		}
	}
	r.mu.Unlock()
	<-ctx.Done()
	r.mu.Lock()
	r.probeCtx = nil
	for id := range r.probers {
		r.stopProber(id)
	}
	r.mu.Unlock()
	r.probersWG.Wait()
}

// startProber starts backend's prober unless it runs, or RunProbers does not.
func (r *Router) startProber(backend string) {
	if r.probeCtx == nil || r.probe == nil {
		return
	}
	if _, ok := r.probers[backend]; ok {
		return
	}
	ctx, cancel := context.WithCancel(r.probeCtx)
	r.probers[backend] = cancel
	r.probersWG.Go(func() { r.runProber(ctx, backend) })
}

// stopProber stops backend's prober; its goroutine returns once its probe in
// progress, if any, has seen the cancellation.
func (r *Router) stopProber(backend string) {
	if cancel, ok := r.probers[backend]; ok {
		cancel()
		delete(r.probers, backend)
	}
}

// runProber is the timer trigger of the probe: every probe interval (the setting in
// force when the wait starts) it probes backend, until ctx ends.
func (r *Router) runProber(ctx context.Context, backend string) {
	for {
		r.mu.Lock()
		interval := r.circuit.ProbeInterval
		r.mu.Unlock()
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		_ = r.ProbeNow(ctx, backend, "interval") // logged and counted by ProbeNow
	}
}

// errUnknownBackend: ProbeNow was asked for a backend the router does not know.
var errUnknownBackend = errors.New("routing: unknown backend")

// ProbeNow probes backend once — the probe mechanism; trigger names what invoked it
// ("interval" for the prober's timer) for the log. A success makes every circuit of
// the backend that was open when the probe started half-open — when the backend
// lists the deployment's model or cannot tell; one it does not list stays open — and
// hands their trial slots to waiting requests; half-open circuits stay half-open. A
// failure keeps open circuits open and opens the half-open ones again: a backend
// whose models list fails is not serving, whatever a trial would say. It returns the
// probe's error; a probe cut short by ctx is neither counted nor logged.
func (r *Router) ProbeNow(ctx context.Context, backend, trigger string) error {
	r.mu.Lock()
	b := r.backends[backend]
	r.mu.Unlock()
	if b == nil {
		return errUnknownBackend
	}
	if r.probe == nil {
		return errors.New("routing: no probe function")
	}
	start := time.Now()
	serves, err := r.probe(ctx, b)
	if err != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	r.observer.Probed(backend, err == nil)
	duration := time.Since(start)

	r.mu.Lock()
	if err != nil {
		var reopened []transition
		for key, c := range r.circuits {
			if key.Backend == backend && c.halfOpen {
				r.reopen(c, backend)
				reopened = append(reopened, transition{key: key, to: CircuitOpen,
					attrs: []any{"kaiak.trigger", trigger, "kaiak.circuit.last_error", "probe failed: " + err.Error()}})
			}
		}
		r.probeFailures[backend]++
		first := r.probeFailures[backend] == 1
		if !r.hasOpenCircuit(backend) {
			delete(r.probeFailures, backend)
		}
		r.mu.Unlock()
		// The first failure after the circuits opened is worth reading; the repeats
		// while the backend stays down are not.
		level := slog.LevelDebug
		if first {
			level = slog.LevelInfo
		}
		r.logger.Log(context.Background(), level, "probe failed", "kaiak.backend.id", backend, "kaiak.trigger", trigger,
			logattr.Seconds("kaiak.duration", duration), "exception.message", err.Error())
		r.emit(reopened, false)
		return err
	}
	var halfOpened []transition
	var unlisted []DeploymentID
	for key, c := range r.circuits {
		if key.Backend != backend || !c.probing() || c.openedAt.After(start) {
			continue
		}
		if serves != nil && !serves(key.Model) {
			// The first probe that finds the model missing says so; the repeats
			// while the backend keeps serving another model do not.
			if !c.unlisted {
				c.unlisted = true
				unlisted = append(unlisted, key)
			}
			continue
		}
		halfOpened = append(halfOpened, transition{key: key, to: CircuitHalfOpen,
			attrs: []any{"kaiak.trigger", trigger, logattr.Seconds("kaiak.circuit.open_duration", time.Since(c.openedAt))}})
		c.halfOpen = true
	}
	if !r.hasOpenCircuit(backend) {
		delete(r.probeFailures, backend)
	}
	changed := r.dispatch()
	r.mu.Unlock()

	// A probe that half-opened circuits is worth reading; the repeats that find the
	// backend still up while its circuits wait for a trial are not.
	level := slog.LevelDebug
	if len(halfOpened) > 0 {
		level = slog.LevelInfo
	}
	r.logger.Log(context.Background(), level, "probe succeeded", "kaiak.backend.id", backend, "kaiak.trigger", trigger,
		logattr.Seconds("kaiak.duration", duration), "kaiak.circuit.half_opened", len(halfOpened))
	for _, key := range unlisted {
		r.logger.Warn("circuit kept open: the backend does not list the deployment's model", "kaiak.backend.id", key.Backend,
			"kaiak.deployment.model", key.Model, "kaiak.trigger", trigger)
	}
	r.emit(halfOpened, changed)
	return nil
}
