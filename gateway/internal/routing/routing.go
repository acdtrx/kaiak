// Package routing chooses the deployment a request to a public model goes to, counts
// the requests in flight on every deployment, caps them per backend, holds the
// requests that find no free slot in their model's queue, and keeps each
// deployment's circuit breaker: deployments failing on real traffic are taken out of
// rotation and probed back in.
package routing

import (
	"container/list"
	"context"
	"errors"
	"log/slog"
	"math"
	"slices"
	"sync"
	"time"

	"kaiak/internal/config"
)

// Queue refusals (docs/specs/GATEWAY.md, Routing and reliability).
var (
	// ErrQueueFull: no eligible deployment had a free slot and the model's queue was
	// full, or its size is 0 (never queue).
	ErrQueueFull = errors.New("routing: queue full")
	// ErrQueueTimeout: the request waited its model's queue timeout without a slot.
	ErrQueueTimeout = errors.New("routing: queue timeout")
	// ErrNoHealthyDeployment: every deployment of the model has its circuit open or,
	// for a retry, is one Avoid refuses.
	ErrNoHealthyDeployment = errors.New("routing: no healthy deployment")
)

// Router balances requests across a model's deployments: among the deployments whose
// backend has a free slot, the one with the fewest requests in flight wins;
// deployments tied on that count take turns (docs/specs/GATEWAY.md, Routing and
// reliability). When none has a free slot the request waits in its model's queue.
//
// Counts belong to the process, not to a config snapshot: a deployment is identified
// by its backend ID and the model name on that backend, a backend by its ID, so a
// config reload that keeps a deployment or backend keeps its counts, and requests
// still running under an older snapshot are counted against the new one.
//
// Slots are handed out by one dispatcher, under the router's mutex: every time a slot
// may have become free (a release, a config with other caps) it gives each free slot
// to the longest-waiting queued request that can use it, by sending that request its
// deployment. So a waiting request never polls, no two waiters race for one slot,
// and no free slot stays unused while a waiter could take it.
//
// Circuits are per deployment, with the same identity and the same life as the
// counts: they outlive reloads that keep the deployment and are dropped with it. An
// open circuit makes its deployment ineligible for new requests and for queued ones;
// one prober per backend with open circuits probes it until it answers, which makes
// the circuit half-open: the deployment takes one trial request at a time, whose
// response starting closes the circuit and whose failure before that opens it
// again. The prober keeps probing while circuits are half-open; a failed probe opens
// them again.
//
// Cooldowns are per deployment too, kept like circuits: a deployment that answered
// 429 cools down for the time its answer asked (Slot.Throttled). While it does it
// takes no request as long as another deployment of the model it may use is
// eligible; with none, it is used — the client gets the backend's 429 honestly. A
// cooldown's end hands the slots it frees to waiting requests at once.
type Router struct {
	probe    ProbeFunc
	observer Observer
	logger   *slog.Logger

	mu sync.Mutex
	// inFlight holds only deployments with requests running; a count that drops to
	// zero is removed, so deployments dropped from the config leave nothing behind.
	inFlight map[DeploymentID]int
	// backendLoad is inFlight summed per backend ID, with the same removal rule.
	backendLoad map[string]int
	// caps are the backend caps of the config last passed to Configure (0 = no cap).
	// A backend it does not name keeps the cap of the snapshot a request runs under.
	caps map[string]int64
	// live is the live-gateway count a cap is split among (SetLiveGateways): each
	// gateway enforces ceil(cap ÷ live). 1 in file mode.
	live int
	// next is, per public model, the deployment index a tie is resolved from.
	next map[string]int
	// queues holds, per public model name, the requests waiting for a slot, oldest
	// first; an empty queue is removed.
	queues map[string]*list.List
	// arrivals numbers queued requests in arrival order, across every model.
	arrivals uint64
	// onServingChange is called, outside the mutex, when a model's queue becomes
	// non-empty or empty, or a circuit opens or closes.
	onServingChange func()

	// deployments are the deployments of the config last passed to Configure; nil
	// before the first, when every deployment counts as configured.
	deployments map[DeploymentID]bool
	// backends are the backends of the config last passed to Configure.
	backends map[string]*config.Backend
	// circuit is the circuit-breaker setting in force.
	circuit config.Circuit
	// circuits holds only deployments with failures or response timeouts counted or
	// an open (or half-open) circuit; a closed circuit with neither is removed.
	circuits map[DeploymentID]*circuit
	// trials numbers half-open trials, so a slot ends only its own.
	trials uint64
	// cooldowns holds only deployments cooling down; its timer removes an entry
	// when the cooldown ends.
	cooldowns map[DeploymentID]*cooldown
	// probeFailures counts, per backend, the failed probes since its circuits
	// opened; removed when none is open.
	probeFailures map[string]int
	// probeCtx is RunProbers' context while it runs, nil otherwise; probers are the
	// running probers' cancel functions by backend ID, all waited for by probersWG.
	probeCtx  context.Context
	probers   map[string]context.CancelFunc
	probersWG sync.WaitGroup
}

// DeploymentID identifies a deployment across config snapshots: its backend ID and
// the model name on that backend.
type DeploymentID struct {
	Backend string
	Model   string
}

func keyOf(d config.Deployment) DeploymentID {
	return DeploymentID{Backend: d.Backend.ID, Model: d.Model}
}

// waiter is one queued request.
type waiter struct {
	model   *config.Model
	avoid   Avoid
	arrival uint64
	// granted receives the slot the dispatcher took for the waiter; buffered, so the
	// dispatcher never blocks.
	granted chan grant
	// elem is the waiter's place in its queue; nil once it has left it (granted, or
	// gone).
	elem *list.Element
}

// cooldown is one deployment's 429 cooldown: until is when it ends, timer ends it.
type cooldown struct {
	until time.Time
	timer *time.Timer
}

// grant is a slot taken on a deployment: trial is the half-open trial it carries, 0
// for none.
type grant struct {
	d     config.Deployment
	trial uint64
}

// ProbeFunc checks whether backend b answers (provider.Registry.Probe): a nil error
// when it does, with serves reporting whether it serves a backend-side model name.
// It must return once ctx ends.
type ProbeFunc func(ctx context.Context, b *config.Backend) (serves func(model string) bool, err error)

// CircuitState is a circuit's state, as the observer is told of its transitions.
type CircuitState string

const (
	CircuitOpen     CircuitState = "open"
	CircuitHalfOpen CircuitState = "half_open"
	CircuitClosed   CircuitState = "closed"
)

// Observer is told of circuit transitions and probe results (the metrics). Its
// methods must not block.
type Observer interface {
	CircuitChanged(d DeploymentID, to CircuitState)
	Probed(backend string, ok bool)
}

// Options are a router's collaborators; the zero value is valid (no probes: open
// circuits stay open until a probe function is given).
type Options struct {
	Probe    ProbeFunc
	Observer Observer
	Logger   *slog.Logger
}

// New returns a router with no requests in flight or waiting, every circuit closed,
// and the default circuit-breaker setting until Configure applies a config's.
func New(opts Options) *Router {
	r := &Router{probe: opts.Probe, observer: opts.Observer, logger: opts.Logger,
		inFlight: make(map[DeploymentID]int), backendLoad: make(map[string]int), live: 1,
		next: make(map[string]int), queues: make(map[string]*list.List),
		circuit:  config.Circuit{FailureThreshold: config.DefaultFailureThreshold, ProbeInterval: config.DefaultProbeInterval},
		circuits: make(map[DeploymentID]*circuit), cooldowns: make(map[DeploymentID]*cooldown),
		probeFailures: make(map[string]int),
		probers:       make(map[string]context.CancelFunc)}
	if r.observer == nil {
		r.observer = noObserver{}
	}
	if r.logger == nil {
		r.logger = slog.New(slog.DiscardHandler)
	}
	return r
}

// OnServingChange registers f, called whenever a model's queue goes from empty to
// non-empty or back — not on every change of its depth — and whenever a circuit
// opens or closes. f must not block; it is set before the router serves requests.
func (r *Router) OnServingChange(f func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.onServingChange = f
}

// Configure makes s's backend caps and circuit-breaker setting the ones in force. A
// lowered cap applies to new admissions only (requests in flight above it finish as
// they are); a raised one hands its new slots to waiting requests at once. The
// circuits and cooldowns of deployments s no longer has are dropped, their probers
// stopped, and the turns of models it no longer has forgotten.
func (r *Router) Configure(s *config.Snapshot) {
	r.mu.Lock()
	r.caps = make(map[string]int64, len(s.Backends))
	for id, b := range s.Backends {
		r.caps[id] = b.MaxInFlight
	}
	r.backends = s.Backends
	r.circuit = s.Circuit
	for name := range r.next {
		if _, ok := s.Models[name]; !ok {
			delete(r.next, name)
		}
	}
	r.deployments = make(map[DeploymentID]bool)
	for _, m := range s.Models {
		for _, d := range m.Deployments {
			r.deployments[keyOf(d)] = true
		}
	}
	changed := r.dropRemovedCircuits()
	r.dropRemovedCooldowns()
	changed = r.dispatch() || changed
	r.mu.Unlock()
	r.notify(changed)
}

// Slot is a request's place on a deployment, taken by Acquire.
type Slot struct {
	Deployment config.Deployment
	release    func()
	report     func(Outcome, string)
	started    func()
	throttled  func(time.Duration)
}

// Release frees the slot. It must be called exactly once, when the request has
// finished — whatever the outcome (success, backend error, client disconnect);
// further calls do nothing. The outcome is told to the circuit breaker by Report,
// before Release.
func (s Slot) Release() { s.release() }

// Report tells the deployment's circuit breaker how the request went. reason
// describes a Failure for the log line of a circuit it opens.
func (s Slot) Report(o Outcome, reason string) { s.report(o, reason) }

// ResponseStarted tells the circuit breaker that the request's response started:
// its first data event (a stream; comment blocks do not count) or its first body
// bytes (not a stream) arrived under a status below 400. A half-open trial is decided by it — the circuit closes at once
// and the deployment serves other requests while the trial runs on; the request's
// outcome is still told by Report when it ends, counted like any other request's.
// On a slot that carries no trial it does nothing.
func (s Slot) ResponseStarted() { s.started() }

// Throttled tells the router that the deployment answered 429 and asked to be left
// alone for d: it cools down until then (a later 429 sets a new end). d of 0 or
// less starts none.
func (s Slot) Throttled(d time.Duration) { s.throttled(d) }

// Avoid is what a retry must steer clear of (docs/specs/GATEWAY.md, Routing and
// reliability: retries — failover only). The zero value is a first attempt, which
// avoids nothing.
type Avoid struct {
	// Refused are deployments never to choose: the ones the request's earlier
	// attempts ran on, and every deployment on a backend that refused the gateway's
	// credential.
	Refused []DeploymentID
}

// firstAttempt reports whether a is a first attempt's: it avoids nothing.
func (a Avoid) firstAttempt() bool { return len(a.Refused) == 0 }

// Wait is what Acquire's queue did for a request.
type Wait struct {
	// Queued: the request entered its model's queue.
	Queued bool
	// Duration is how long it waited there; 0 when it did not queue.
	Duration time.Duration
}

// Acquire takes a slot for one attempt of a request to m: at once when one of m's
// deployments the attempt may use has a free slot, else after waiting in m's queue —
// behind the requests already there, a retry included — until the dispatcher hands
// it one. The deployments it may use are the eligible ones avoid does not refuse,
// the ones not cooling down among them when there are any; which ones those are is
// decided anew whenever a slot is handed out, as circuits open and close and
// cooldowns start and end. It fails with
// ErrNoHealthyDeployment when there are none, ErrQueueFull when the queue has no
// room (both at once),
// ErrQueueTimeout after m.Queue.Timeout, or ctx's error when ctx ends first — a
// request whose client left is out of the queue when Acquire returns. A request
// already waiting keeps waiting when m's last circuit opens: a probe may half-open one
// before its timeout. The Wait is reported whatever the result.
func (r *Router) Acquire(ctx context.Context, m *config.Model, avoid Avoid) (Slot, Wait, error) {
	r.mu.Lock()
	if d, ok := r.choose(m, avoid); ok {
		slot := r.slotFor(r.take(d))
		r.mu.Unlock()
		return slot, Wait{}, nil
	}
	if !r.anyUsable(m, avoid) {
		r.mu.Unlock()
		return Slot{}, Wait{}, ErrNoHealthyDeployment
	}
	q := r.queues[m.Name]
	if m.Queue.Size == 0 || (q != nil && q.Len() >= m.Queue.Size) {
		r.mu.Unlock()
		return Slot{}, Wait{}, ErrQueueFull
	}
	if q == nil {
		q = list.New()
		r.queues[m.Name] = q
	}
	r.arrivals++
	w := &waiter{model: m, avoid: avoid, arrival: r.arrivals, granted: make(chan grant, 1)}
	w.elem = q.PushBack(w)
	changed := q.Len() == 1
	r.mu.Unlock()
	r.notify(changed)

	start := time.Now()
	timer := time.NewTimer(m.Queue.Timeout)
	defer timer.Stop()
	var err error
	select {
	case g := <-w.granted:
		return r.slotFor(g), Wait{Queued: true, Duration: time.Since(start)}, nil
	case <-timer.C:
		err = ErrQueueTimeout
	case <-ctx.Done():
		err = ctx.Err()
	}

	r.mu.Lock()
	if w.elem == nil {
		// The dispatcher handed the waiter a slot as it stopped waiting.
		g := <-w.granted
		if err == ErrQueueTimeout {
			r.mu.Unlock()
			return r.slotFor(g), Wait{Queued: true, Duration: time.Since(start)}, nil
		}
		// The client left: the slot (and the trial it carries) goes to the next
		// waiter.
		changed = r.releaseLocked(keyOf(g.d), g.trial)
	} else {
		changed = r.leave(w)
	}
	r.mu.Unlock()
	r.notify(changed)
	return Slot{}, Wait{Queued: true, Duration: time.Since(start)}, err
}

// choose picks m's deployment for an attempt now: among the deployments it may use
// (usable) whose backend has a free slot, the fewest in flight, ties taking turns
// from the model's next index. ok is false when none has a free slot.
func (r *Router) choose(m *config.Model, avoid Avoid) (d config.Deployment, ok bool) {
	n := len(m.Deployments)
	start := r.next[m.Name] % n
	best := -1
	warm := r.anyWarm(m, avoid)
	for i := range n {
		candidate := (start + i) % n
		cd := m.Deployments[candidate]
		if !r.usable(cd, avoid, warm) || !r.hasFreeSlot(cd.Backend) {
			continue
		}
		if best < 0 || r.inFlight[keyOf(cd)] < r.inFlight[keyOf(m.Deployments[best])] {
			best = candidate
		}
	}
	if best < 0 {
		return config.Deployment{}, false
	}
	r.next[m.Name] = best + 1
	return m.Deployments[best], true
}

// eligible reports whether routing may send requests to d: its circuit is closed,
// or half-open with no trial under way.
func (r *Router) eligible(d config.Deployment) bool {
	c, ok := r.circuits[keyOf(d)]
	return !ok || c.openedAt.IsZero() || (c.halfOpen && c.trial == 0)
}

// usable reports whether an attempt avoiding avoid may use d: d is eligible, not
// refused, and — when warm (an eligible deployment of the model not refused is not
// cooling down) — not cooling down itself.
func (r *Router) usable(d config.Deployment, avoid Avoid, warm bool) bool {
	key := keyOf(d)
	if !r.eligible(d) || slices.Contains(avoid.Refused, key) {
		return false
	}
	return !warm || r.cooldowns[key] == nil
}

// anyWarm reports whether one of m's eligible deployments that avoid does not
// refuse is not cooling down.
func (r *Router) anyWarm(m *config.Model, avoid Avoid) bool {
	for _, d := range m.Deployments {
		key := keyOf(d)
		if r.eligible(d) && !slices.Contains(avoid.Refused, key) && r.cooldowns[key] == nil {
			return true
		}
	}
	return false
}

// anyUsable reports whether an attempt avoiding avoid may use one of m's
// deployments.
func (r *Router) anyUsable(m *config.Model, avoid Avoid) bool {
	warm := r.anyWarm(m, avoid)
	for _, d := range m.Deployments {
		if r.usable(d, avoid, warm) {
			return true
		}
	}
	return false
}

// hasFreeSlot reports whether b may take one more request: its cap (the configured
// one, else the one b carries) is 0 or its share above its requests in flight.
func (r *Router) hasFreeSlot(b *config.Backend) bool {
	limit, ok := r.caps[b.ID]
	if !ok {
		limit = b.MaxInFlight
	}
	return limit == 0 || int64(r.backendLoad[b.ID]) < r.share(limit)
}

// share is this gateway's part of a backend cap: ceil(limit ÷ live), at least 1
// (docs/specs/GATEWAY.md, Routing and reliability: concurrency cap).
func (r *Router) share(limit int64) int64 {
	live := int64(r.live)
	return max((limit+live-1)/live, 1)
}

// SetLiveGateways sets the live-gateway count backend caps are split among, from the
// control plane's latest totals; n below 1 counts as 1. A count that lowers the
// share applies to new admissions only; one that raises it hands the new slots to
// waiting requests at once.
func (r *Router) SetLiveGateways(n int64) {
	r.mu.Lock()
	live := int(max(min(n, int64(math.MaxInt32)), 1))
	if live == r.live {
		r.mu.Unlock()
		return
	}
	r.live = live
	changed := r.dispatch()
	r.mu.Unlock()
	r.notify(changed)
}

// MaxInFlightByBackend returns the cap this gateway enforces per backend of the
// applied config — its share of the configured cap. Backends without a cap are
// absent.
func (r *Router) MaxInFlightByBackend() map[string]int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	caps := make(map[string]int64, len(r.caps))
	for id, limit := range r.caps {
		if limit > 0 {
			caps[id] = r.share(limit)
		}
	}
	return caps
}

// take counts one request in flight on d; on a half-open circuit it is the trial.
func (r *Router) take(d config.Deployment) grant {
	key := keyOf(d)
	r.inFlight[key]++
	r.backendLoad[d.Backend.ID]++
	g := grant{d: d}
	if c, ok := r.circuits[key]; ok && c.halfOpen {
		r.trials++
		c.trial, g.trial = r.trials, r.trials
	}
	return g
}

// slotFor wraps a slot already counted.
func (r *Router) slotFor(g grant) Slot {
	key := keyOf(g.d)
	var once sync.Once
	return Slot{Deployment: g.d, release: func() { once.Do(func() { r.release(key, g.trial) }) },
		report:    func(o Outcome, reason string) { r.report(g.d, g.trial, o, reason) },
		started:   func() { r.responseStarted(g.d, g.trial) },
		throttled: func(d time.Duration) { r.throttle(key, d) }}
}

func (r *Router) release(key DeploymentID, trial uint64) {
	r.mu.Lock()
	changed := r.releaseLocked(key, trial)
	r.mu.Unlock()
	r.notify(changed)
}

// releaseLocked frees one slot on key and dispatches it; a trial still under way
// (its request ended without a verdict) ends, so another request may try. changed
// reports a queue that became empty.
func (r *Router) releaseLocked(key DeploymentID, trial uint64) (changed bool) {
	if c, ok := r.circuits[key]; ok && trial != 0 && c.trial == trial {
		c.trial = 0
	}
	if r.inFlight[key] <= 1 {
		delete(r.inFlight, key)
	} else {
		r.inFlight[key]--
	}
	if r.backendLoad[key.Backend] <= 1 {
		delete(r.backendLoad, key.Backend)
	} else {
		r.backendLoad[key.Backend]--
	}
	return r.dispatch()
}

// dispatch hands free slots to waiting requests until none can take one: each round
// the longest-waiting request that may use a deployment with a free slot gets that
// deployment (chosen as for a new request). A model's queue is served in order; a
// retry that may not use the free slot's deployment (it avoids it) lets the requests
// behind it that can use it go first, so no slot a waiter could use stays free.
// changed reports a queue that became empty.
//
// Within a round, first attempts (which avoid nothing) of one model under one config
// snapshot all get the same answer from canServe, so it is asked once per model
// snapshot and round; retries, each with its own avoid set, are asked one by one. With every backend full the round then
// walks each queue without re-checking the model's deployments per waiter.
func (r *Router) dispatch() (changed bool) {
	for {
		var best *waiter
		for _, q := range r.queues {
			// firstServable caches canServe for first attempts of one config
			// snapshot's model this round (servableModel): 0 unknown, 1 yes, 2 no. A
			// queue holds waiters of several snapshots after a reload, whose
			// deployments differ, so a waiter of another snapshot asks anew.
			firstServable := 0
			var servableModel *config.Model
			for e := q.Front(); e != nil; e = e.Next() {
				w := e.Value.(*waiter)
				if best != nil && w.arrival > best.arrival {
					break
				}
				var ok bool
				if w.avoid.firstAttempt() {
					if w.model != servableModel {
						firstServable, servableModel = 0, w.model
					}
					if firstServable == 0 {
						firstServable = 2
						if r.canServe(w.model, w.avoid) {
							firstServable = 1
						}
					}
					ok = firstServable == 1
				} else {
					ok = r.canServe(w.model, w.avoid)
				}
				if ok {
					best = w
					break
				}
			}
		}
		if best == nil {
			return changed
		}
		d, _ := r.choose(best.model, best.avoid)
		g := r.take(d)
		changed = r.leave(best) || changed
		best.granted <- g
	}
}

// canServe reports whether one of m's deployments an attempt avoiding avoid may use
// has a free slot.
func (r *Router) canServe(m *config.Model, avoid Avoid) bool {
	warm := r.anyWarm(m, avoid)
	for _, d := range m.Deployments {
		if r.usable(d, avoid, warm) && r.hasFreeSlot(d.Backend) {
			return true
		}
	}
	return false
}

// leave takes w out of its queue. changed reports that the queue became empty.
func (r *Router) leave(w *waiter) (changed bool) {
	q := r.queues[w.model.Name]
	q.Remove(w.elem)
	w.elem = nil
	if q.Len() > 0 {
		return false
	}
	delete(r.queues, w.model.Name)
	return true
}

func (r *Router) notify(changed bool) {
	if !changed {
		return
	}
	r.mu.Lock()
	f := r.onServingChange
	r.mu.Unlock()
	if f != nil {
		f()
	}
}

// InFlightByBackend returns the number of requests in flight per backend ID. Backends
// with none are absent.
func (r *Router) InFlightByBackend() map[string]int {
	r.mu.Lock()
	defer r.mu.Unlock()
	counts := make(map[string]int, len(r.backendLoad))
	for id, n := range r.backendLoad {
		counts[id] = n
	}
	return counts
}

// QueuedByModel returns the number of requests waiting per public model name. Models
// with none are absent.
func (r *Router) QueuedByModel() map[string]int {
	r.mu.Lock()
	defer r.mu.Unlock()
	counts := make(map[string]int, len(r.queues))
	for name, q := range r.queues {
		counts[name] = q.Len()
	}
	return counts
}

// throttle starts key's cooldown, ending d from now; one under way ends at the later
// of the two — a later 429 with a shorter Retry-After never shortens the backend's
// earlier word (a quota's reset does not move closer because one answer said less).
// A cooldown can make the deployments already cooling usable (the last one not
// cooling started to), so waiting requests are dispatched. Deployments the applied
// config does not have (a request still running under an older snapshot) are not
// tracked.
func (r *Router) throttle(key DeploymentID, d time.Duration) {
	if d <= 0 {
		return
	}
	r.mu.Lock()
	if r.deployments != nil && !r.deployments[key] {
		r.mu.Unlock()
		return
	}
	until := time.Now().Add(d)
	if old := r.cooldowns[key]; old != nil {
		if !until.After(old.until) {
			r.mu.Unlock()
			return
		}
		old.timer.Stop()
	}
	c := &cooldown{until: until}
	c.timer = time.AfterFunc(d, func() { r.endCooldown(key, c) })
	r.cooldowns[key] = c
	changed := r.dispatch()
	r.mu.Unlock()
	r.notify(changed)
}

// endCooldown ends c, key's cooldown, unless another replaced it, and hands the
// slots the deployment offers again to waiting requests.
func (r *Router) endCooldown(key DeploymentID, c *cooldown) {
	r.mu.Lock()
	if r.cooldowns[key] != c {
		r.mu.Unlock()
		return
	}
	delete(r.cooldowns, key)
	changed := r.dispatch()
	r.mu.Unlock()
	r.notify(changed)
}

// dropRemovedCooldowns forgets the cooldowns of deployments the applied config no
// longer has.
func (r *Router) dropRemovedCooldowns() {
	for key, c := range r.cooldowns {
		if !r.deployments[key] {
			c.timer.Stop()
			delete(r.cooldowns, key)
		}
	}
}

// CoolingDown returns, per deployment cooling down after a 429, when its cooldown
// ends. Deployments not cooling down are absent.
func (r *Router) CoolingDown() map[DeploymentID]time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[DeploymentID]time.Time, len(r.cooldowns))
	for key, c := range r.cooldowns {
		out[key] = c.until
	}
	return out
}
