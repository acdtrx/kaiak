package routing

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"kaiak/internal/config"
)

// events records what the router tells its observer.
type events struct {
	mu          sync.Mutex
	transitions []string
	probes      []string
}

func (e *events) CircuitChanged(d DeploymentID, to CircuitState) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.transitions = append(e.transitions, d.Backend+"/"+d.Model+" "+string(to))
}

func (e *events) Probed(backend string, ok bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	result := "failure"
	if ok {
		result = "success"
	}
	e.probes = append(e.probes, backend+" "+result)
}

func (e *events) get() (transitions, probes []string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.transitions), slices.Clone(e.probes)
}

// scriptedProbe is a probe whose answers the test gives, one per call, on answers;
// calls reports each call's backend and context as it starts. A successful probe
// lists every model but those in unlisted (set before the answer is sent).
type scriptedProbe struct {
	calls    chan probeCall
	answers  chan error
	unlisted map[string]bool
}

type probeCall struct {
	backend string
	ctx     context.Context
}

func newScriptedProbe() *scriptedProbe {
	return &scriptedProbe{calls: make(chan probeCall, 16), answers: make(chan error, 16)}
}

func (p *scriptedProbe) probe(ctx context.Context, b *config.Backend) (func(string) bool, error) {
	p.calls <- probeCall{b.ID, ctx}
	select {
	case err := <-p.answers:
		if err != nil {
			return nil, err
		}
		unlisted := p.unlisted
		return func(model string) bool { return !unlisted[model] }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (p *scriptedProbe) nextCall(t *testing.T) probeCall {
	t.Helper()
	select {
	case c := <-p.calls:
		return c
	case <-time.After(waitTimeout):
		t.Fatal("no probe")
	}
	return probeCall{}
}

// circuitSnapshot is a config of models over backends, with the circuit setting.
func circuitSnapshot(threshold int, interval time.Duration, models ...*config.Model) *config.Snapshot {
	s := &config.Snapshot{Circuit: config.Circuit{FailureThreshold: threshold, ProbeInterval: interval},
		Backends: map[string]*config.Backend{}, Models: map[string]*config.Model{}}
	for _, m := range models {
		s.Models[m.Name] = m
		for _, d := range m.Deployments {
			s.Backends[d.Backend.ID] = d.Backend
		}
	}
	return s
}

// fail reports n failures on a request routed to d.
func fail(r *Router, d config.Deployment, n int) {
	for range n {
		r.report(d, 0, Failure, "scripted")
	}
}

func isOpen(r *Router, d config.Deployment) bool {
	_, ok := notClosed(r)[keyOf(d)]
	return ok
}

func TestCircuitOpensAtTheThresholdNotBefore(t *testing.T) {
	obs := &events{}
	r := New(Options{Observer: obs})
	changes := make(chan struct{}, 10)
	r.OnServingChange(func() { changes <- struct{}{} })
	m := queuedModel("m", 10, time.Hour, backend("a", 0), backend("b", 0))
	r.Configure(circuitSnapshot(3, time.Hour, m))
	a := m.Deployments[0]

	fail(r, a, 2)
	if isOpen(r, a) || len(changes) != 0 {
		t.Fatal("open after 2 failures with threshold 3")
	}
	before := time.Now()
	fail(r, a, 1)
	opened, ok := notClosed(r)[keyOf(a)]
	if !ok || opened.Before(before) {
		t.Fatalf("circuits %v after the 3rd failure, want a open since the failure", notClosed(r))
	}
	if transitions, _ := obs.get(); !slices.Equal(transitions, []string{"a/m@a open"}) {
		t.Errorf("observer told %v", transitions)
	}
	if len(changes) != 1 {
		t.Errorf("%d serving changes told, want 1", len(changes))
	}
	// Outcomes while open change nothing: only a probe closes it.
	r.report(a, 0, Success, "")
	fail(r, a, 5)
	if !isOpen(r, a) {
		t.Error("an outcome on the open circuit closed it")
	}
	if transitions, _ := obs.get(); len(transitions) != 1 {
		t.Errorf("observer told %v, want the one opening", transitions)
	}
}

func TestSuccessResetsTheFailureCount(t *testing.T) {
	r := New(Options{})
	m := queuedModel("m", 10, time.Hour, backend("a", 0))
	r.Configure(circuitSnapshot(3, time.Hour, m))
	d := m.Deployments[0]
	fail(r, d, 2)
	r.report(d, 0, Success, "")
	fail(r, d, 2)
	if isOpen(r, d) {
		t.Fatal("open after 2 failures, a success, 2 failures")
	}
	fail(r, d, 1)
	if !isOpen(r, d) {
		t.Error("closed after 3 consecutive failures")
	}
}

func TestNeutralOutcomesDoNotCount(t *testing.T) {
	r := New(Options{})
	m := queuedModel("m", 10, time.Hour, backend("a", 0))
	r.Configure(circuitSnapshot(2, time.Hour, m))
	d := m.Deployments[0]
	fail(r, d, 1)
	for range 10 {
		r.report(d, 0, Neutral, "")
	}
	if isOpen(r, d) {
		t.Fatal("neutral outcomes opened the circuit")
	}
	// Neutral outcomes do not reset either: the next failure is the second in a row.
	fail(r, d, 1)
	if !isOpen(r, d) {
		t.Error("closed after 2 failures with only neutral outcomes between")
	}
}

func TestIntermittentFailuresBelowTheThresholdNeverOpen(t *testing.T) {
	r := New(Options{})
	m := queuedModel("m", 10, time.Hour, backend("a", 0))
	r.Configure(circuitSnapshot(3, time.Hour, m))
	d := m.Deployments[0]
	for range 1000 {
		fail(r, d, 2)
		r.report(d, 0, Success, "")
	}
	if isOpen(r, d) {
		t.Error("2 failures out of every 3 requests opened the circuit")
	}
}

func TestRoutingSkipsOpenDeployments(t *testing.T) {
	r := New(Options{})
	m := queuedModel("m", 10, time.Hour, backend("a", 0), backend("b", 0))
	r.Configure(circuitSnapshot(1, time.Hour, m))
	fail(r, m.Deployments[0], 1)
	for range 4 {
		if d, _ := acquire(r, m); d.Backend.ID != "b" {
			t.Fatalf("routed to %s with a's circuit open, want b", d.Backend.ID)
		}
	}
}

func TestAllOpenIsRefusedAtOnce(t *testing.T) {
	r := New(Options{})
	m := queuedModel("m", 10, time.Hour, backend("a", 0), backend("b", 0))
	r.Configure(circuitSnapshot(1, time.Hour, m))
	fail(r, m.Deployments[0], 1)
	fail(r, m.Deployments[1], 1)
	_, wait, err := r.Acquire(context.Background(), m, Avoid{})
	if !errors.Is(err, ErrNoHealthyDeployment) || wait.Queued {
		t.Errorf("Acquire = %v, queued %v; want ErrNoHealthyDeployment without queueing", err, wait.Queued)
	}
}

func TestProbeSuccessMakesEveryOpenCircuitOfTheBackendHalfOpen(t *testing.T) {
	obs := &events{}
	probe := newScriptedProbe()
	r := New(Options{Probe: probe.probe, Observer: obs})
	changes := make(chan struct{}, 10)
	r.OnServingChange(func() { changes <- struct{}{} })
	x, y := backend("x", 0), backend("y", 0)
	m1 := queuedModel("m1", 10, time.Hour, x, y)
	m2 := queuedModel("m2", 10, time.Hour, x)
	r.Configure(circuitSnapshot(1, time.Hour, m1, m2))
	fail(r, m1.Deployments[0], 1)
	fail(r, m2.Deployments[0], 1)
	fail(r, m1.Deployments[1], 1)
	<-changes
	<-changes
	<-changes

	probe.answers <- errors.New("models list answered 503")
	if err := r.ProbeNow(context.Background(), "x", "manual"); err == nil {
		t.Fatal("ProbeNow = nil for a failing probe")
	}
	if _, _, err := r.Acquire(context.Background(), m2, Avoid{}); !errors.Is(err, ErrNoHealthyDeployment) {
		t.Fatalf("m2 after a failed probe = %v, want ErrNoHealthyDeployment", err)
	}
	probe.answers <- nil
	if err := r.ProbeNow(context.Background(), "x", "manual"); err != nil {
		t.Fatal(err)
	}
	// Half-open counts as open until a trial closes it.
	if len(notClosed(r)) != 3 {
		t.Errorf("circuits %v after x's probe succeeded, want all 3 still reported open", notClosed(r))
	}
	transitions, probes := obs.get()
	if !slices.Equal(probes, []string{"x failure", "x success"}) {
		t.Errorf("probes told %v", probes)
	}
	slices.Sort(transitions)
	if want := []string{"x/m1@x half_open", "x/m1@x open", "x/m2@x half_open", "x/m2@x open", "y/m1@y open"}; !slices.Equal(transitions, want) {
		t.Errorf("observer told %v, want %v", transitions, want)
	}
	if len(changes) != 0 {
		t.Errorf("%d serving changes told for the half-opening, want none (status still says open)", len(changes))
	}
	// x's deployments take a trial each; y's stays out.
	slot, _, err := r.Acquire(context.Background(), m2, Avoid{})
	if err != nil || slot.Deployment.Backend.ID != "x" {
		t.Fatalf("m2 = %v on %v, want x's trial", err, slot.Deployment)
	}
	slot.Report(Success, "")
	slot.Release()
	if len(changes) != 1 {
		t.Errorf("%d serving changes told for the closing, want 1", len(changes))
	}
	if d, _ := acquire(r, m1); d.Backend.ID != "x" {
		t.Errorf("m1 routed to %s with y open, want x's trial", d.Backend.ID)
	}
}

func TestQueuedRequestsWaitForAProbeToCloseTheCircuit(t *testing.T) {
	probe := newScriptedProbe()
	r := New(Options{Probe: probe.probe})
	m := queuedModel("m", 10, time.Hour, backend("a", 1))
	r.Configure(circuitSnapshot(1, time.Hour, m))
	slot, _, err := r.Acquire(context.Background(), m, Avoid{})
	if err != nil {
		t.Fatal(err)
	}
	waiter := enqueue(t, r, context.Background(), "waiter", m, 1)
	slot.Report(Failure, "scripted")
	slot.Release()
	if !pending(waiter) {
		t.Fatal("the waiter got the slot of a deployment whose circuit is open")
	}
	probe.answers <- nil
	if err := r.ProbeNow(context.Background(), "a", "manual"); err != nil {
		t.Fatal(err)
	}
	res := receive(t, waiter)
	if res.err != nil || res.slot.Deployment.Backend.ID != "a" {
		t.Fatalf("waiter = %v on %v, want a's slot once the probe closed the circuit", res.err, res.slot.Deployment)
	}
	res.slot.Release()
}

func TestCircuitStateSurvivesAReloadThatKeepsTheDeployment(t *testing.T) {
	r := New(Options{})
	m := queuedModel("m", 10, time.Hour, backend("a", 0), backend("b", 0))
	r.Configure(circuitSnapshot(2, time.Hour, m))
	fail(r, m.Deployments[0], 2)
	fail(r, m.Deployments[1], 1)

	// A new snapshot: new pointers, same deployments.
	m2 := queuedModel("m", 10, time.Hour, backend("a", 0), backend("b", 0))
	r.Configure(circuitSnapshot(2, time.Hour, m2))
	if !isOpen(r, m2.Deployments[0]) {
		t.Error("a's open circuit did not survive the reload")
	}
	fail(r, m2.Deployments[1], 1)
	if !isOpen(r, m2.Deployments[1]) {
		t.Error("b's failure count did not survive the reload")
	}
}

func TestFailuresOnDeploymentsNotConfiguredAreNotTracked(t *testing.T) {
	r := New(Options{})
	m := queuedModel("m", 10, time.Hour, backend("a", 0))
	old := queuedModel("old", 10, time.Hour, backend("gone", 0))
	r.Configure(circuitSnapshot(1, time.Hour, m))
	fail(r, old.Deployments[0], 3)
	if len(notClosed(r)) != 0 || len(r.circuits) != 0 {
		t.Errorf("circuits %v for a deployment the config does not have", notClosed(r))
	}
}

// probersRunning returns the backends with a prober.
func probersRunning(r *Router) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var ids []string
	for id := range r.probers {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

func awaitDone(t *testing.T, ctx context.Context, what string) {
	t.Helper()
	select {
	case <-ctx.Done():
	case <-time.After(waitTimeout):
		t.Fatalf("%s: prober not stopped", what)
	}
}

func TestProberLifecycle(t *testing.T) {
	probe := newScriptedProbe()
	r := New(Options{Probe: probe.probe})
	x, y := backend("x", 0), backend("y", 0)
	m := queuedModel("m", 10, time.Hour, x, y)
	// 1 ms interval: the timer fires at once, so each wait below is for the probe
	// the test answers.
	r.Configure(circuitSnapshot(1, time.Millisecond, m))
	fail(r, m.Deployments[0], 1) // opened before RunProbers: its prober starts with it

	ctx, stop := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		r.RunProbers(ctx)
		close(stopped)
	}()

	// Timer-triggered probes: failures keep the circuit open and the prober running.
	call := probe.nextCall(t)
	if call.backend != "x" {
		t.Fatalf("probed %s, want x", call.backend)
	}
	probe.answers <- errors.New("down")
	call = probe.nextCall(t)
	probe.answers <- errors.New("down")
	if !isOpen(r, m.Deployments[0]) {
		t.Fatal("closed by failed probes")
	}
	// A success makes the circuit half-open; the prober keeps probing it until the
	// trial's success closes it.
	probe.nextCall(t)
	probe.answers <- nil
	call = probe.nextCall(t)
	trial, _, err := r.Acquire(context.Background(), queuedModel("m", 10, time.Hour, x), Avoid{})
	if err != nil {
		t.Fatal(err)
	}
	trial.Report(Success, "")
	trial.Release()
	if isOpen(r, m.Deployments[0]) {
		t.Fatalf("open %v after the trial succeeded", notClosed(r))
	}
	awaitDone(t, call.ctx, "after the trial closed the circuit")
	if len(probersRunning(r)) != 0 {
		t.Fatalf("probers %v after the trial closed the circuit", probersRunning(r))
	}

	// A reload dropping the deployment stops its backend's prober.
	fail(r, m.Deployments[1], 1)
	call = probe.nextCall(t)
	if call.backend != "y" {
		t.Fatalf("probed %s, want y", call.backend)
	}
	r.Configure(circuitSnapshot(1, time.Millisecond, queuedModel("m", 10, time.Hour, backend("x", 0))))
	awaitDone(t, call.ctx, "after the reload dropped the deployment")
	if len(notClosed(r)) != 0 || len(probersRunning(r)) != 0 {
		t.Fatalf("open %v, probers %v after the reload", notClosed(r), probersRunning(r))
	}

	// Shutdown stops a prober in the middle of its probe, and RunProbers waits for it.
	m3 := queuedModel("m", 10, time.Hour, backend("x", 0))
	r.Configure(circuitSnapshot(1, time.Millisecond, m3))
	fail(r, m3.Deployments[0], 1)
	call = probe.nextCall(t)
	stop()
	select {
	case <-stopped:
	case <-time.After(waitTimeout):
		t.Fatal("RunProbers did not return")
	}
	awaitDone(t, call.ctx, "after shutdown")
	if len(probersRunning(r)) != 0 {
		t.Errorf("probers %v after RunProbers returned", probersRunning(r))
	}
	// An opening after shutdown starts nothing.
	fail(r, m3.Deployments[0], 1)
	if len(probersRunning(r)) != 0 {
		t.Error("a prober started after RunProbers returned")
	}
}

func TestProbeCutShortIsNotCounted(t *testing.T) {
	obs := &events{}
	probe := newScriptedProbe()
	r := New(Options{Probe: probe.probe, Observer: obs})
	m := queuedModel("m", 10, time.Hour, backend("a", 0))
	r.Configure(circuitSnapshot(1, time.Hour, m))
	fail(r, m.Deployments[0], 1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := r.ProbeNow(ctx, "a", "manual"); !errors.Is(err, context.Canceled) {
		t.Errorf("ProbeNow = %v, want the context's error", err)
	}
	if _, probes := obs.get(); len(probes) != 0 {
		t.Errorf("probes told %v for a probe cut short", probes)
	}
	if err := r.ProbeNow(context.Background(), "nobody", "manual"); !errors.Is(err, errUnknownBackend) {
		t.Errorf("ProbeNow(unknown) = %v", err)
	}
}

// halfOpen opens d's circuit (threshold 1) and has a successful probe of its backend
// move it to half-open.
func halfOpen(t *testing.T, r *Router, probe *scriptedProbe, d config.Deployment) {
	t.Helper()
	fail(r, d, 1)
	probe.answers <- nil
	if err := r.ProbeNow(context.Background(), d.Backend.ID, "manual"); err != nil {
		t.Fatal(err)
	}
}

func TestHalfOpenAdmitsOneTrialWhoseSuccessCloses(t *testing.T) {
	obs := &events{}
	probe := newScriptedProbe()
	r := New(Options{Probe: probe.probe, Observer: obs})
	m := queuedModel("m", 10, time.Hour, backend("a", 0))
	r.Configure(circuitSnapshot(1, time.Hour, m))
	a := m.Deployments[0]
	halfOpen(t, r, probe, a)
	if !isOpen(r, a) {
		t.Fatal("half-open is not reported open")
	}
	trial, _, err := r.Acquire(context.Background(), m, Avoid{})
	if err != nil {
		t.Fatalf("trial = %v, want a's slot", err)
	}
	// One trial at a time: the next request finds no deployment it may use.
	if _, _, err := r.Acquire(context.Background(), m, Avoid{}); !errors.Is(err, ErrNoHealthyDeployment) {
		t.Fatalf("second request during the trial = %v, want ErrNoHealthyDeployment", err)
	}
	trial.Report(Success, "")
	trial.Release()
	if isOpen(r, a) {
		t.Fatal("still open after the trial succeeded")
	}
	if transitions, _ := obs.get(); !slices.Equal(transitions, []string{"a/m@a open", "a/m@a half_open", "a/m@a closed"}) {
		t.Errorf("observer told %v", transitions)
	}
	for range 3 {
		_, release := acquire(r, m)
		release()
	}
}

func TestHalfOpenTrialFailureReopens(t *testing.T) {
	obs := &events{}
	probe := newScriptedProbe()
	r := New(Options{Probe: probe.probe, Observer: obs})
	m := queuedModel("m", 10, time.Hour, backend("a", 0))
	r.Configure(circuitSnapshot(3, time.Hour, m))
	a := m.Deployments[0]
	fail(r, a, 2) // threshold 3: open on the next
	halfOpen(t, r, probe, a)
	before := time.Now()
	trial, _, err := r.Acquire(context.Background(), m, Avoid{})
	if err != nil {
		t.Fatal(err)
	}
	trial.Report(Failure, "scripted")
	trial.Release()
	// One failure re-opens, whatever the threshold; no trial until a probe succeeds.
	if opened, ok := notClosed(r)[keyOf(a)]; !ok || opened.Before(before) {
		t.Fatalf("circuits %v after the trial failed, want a open since the failure", notClosed(r))
	}
	if _, _, err := r.Acquire(context.Background(), m, Avoid{}); !errors.Is(err, ErrNoHealthyDeployment) {
		t.Fatalf("request after the failed trial = %v, want ErrNoHealthyDeployment", err)
	}
	if transitions, _ := obs.get(); !slices.Equal(transitions, []string{"a/m@a open", "a/m@a half_open", "a/m@a open"}) {
		t.Errorf("observer told %v", transitions)
	}
	probe.answers <- nil
	if err := r.ProbeNow(context.Background(), "a", "manual"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.Acquire(context.Background(), m, Avoid{}); err != nil {
		t.Errorf("trial after the next probe = %v", err)
	}
}

func TestHalfOpenTrialWithoutAVerdictLetsAnotherTry(t *testing.T) {
	probe := newScriptedProbe()
	r := New(Options{Probe: probe.probe})
	m := queuedModel("m", 10, time.Hour, backend("a", 0))
	r.Configure(circuitSnapshot(1, time.Hour, m))
	a := m.Deployments[0]
	halfOpen(t, r, probe, a)
	// A neutral outcome (the backend busy, the client gone) says nothing: still
	// half-open, the next request is the trial.
	trial, _, err := r.Acquire(context.Background(), m, Avoid{})
	if err != nil {
		t.Fatal(err)
	}
	trial.Report(Neutral, "")
	trial.Release()
	// A trial released without any report frees the trial too.
	trial, _, err = r.Acquire(context.Background(), m, Avoid{})
	if err != nil {
		t.Fatalf("after a neutral trial: %v", err)
	}
	trial.Release()
	trial, _, err = r.Acquire(context.Background(), m, Avoid{})
	if err != nil {
		t.Fatalf("after a trial released without a report: %v", err)
	}
	if !isOpen(r, a) {
		t.Error("closed without a successful trial")
	}
	trial.Report(Success, "")
	trial.Release()
	if isOpen(r, a) {
		t.Error("open after the trial succeeded")
	}
}

func TestQueuedRequestGetsTheTrialSlot(t *testing.T) {
	probe := newScriptedProbe()
	r := New(Options{Probe: probe.probe})
	m := queuedModel("m", 10, time.Hour, backend("a", 0))
	r.Configure(circuitSnapshot(1, time.Hour, m))
	slot, _, err := r.Acquire(context.Background(), m, Avoid{})
	if err != nil {
		t.Fatal(err)
	}
	slot.Report(Failure, "scripted")
	slot.Release()
	// Queued behind an open circuit: two waiters, one trial.
	first := enqueueWhileOpen(t, r, m, 1)
	second := enqueueWhileOpen(t, r, m, 2)
	probe.answers <- nil
	if err := r.ProbeNow(context.Background(), "a", "manual"); err != nil {
		t.Fatal(err)
	}
	res := receive(t, first)
	if res.err != nil {
		t.Fatalf("first waiter = %v, want the trial", res.err)
	}
	if !pending(second) {
		t.Fatal("the second waiter got a slot during the trial")
	}
	res.slot.Report(Success, "")
	res.slot.Release()
	res = receive(t, second)
	if res.err != nil {
		t.Fatalf("second waiter = %v once the trial closed the circuit", res.err)
	}
	res.slot.Release()
}

// enqueueWhileOpen adds a request for m to its queue as Acquire does for one that
// finds no free slot (Acquire refuses a new request while every circuit is open, so
// the test places it), and returns where its result arrives.
func enqueueWhileOpen(t *testing.T, r *Router, m *config.Model, depth int) <-chan result {
	t.Helper()
	out := make(chan result, 1)
	r.mu.Lock()
	q := r.queues[m.Name]
	if q == nil {
		q = list.New()
		r.queues[m.Name] = q
	}
	r.arrivals++
	w := &waiter{model: m, arrival: r.arrivals, granted: make(chan grant, 1)}
	w.elem = q.PushBack(w)
	r.mu.Unlock()
	go func() {
		g := <-w.granted
		out <- result{name: "queued", slot: r.slotFor(g)}
	}()
	waitQueued(t, r, m.Name, depth)
	return out
}

// A probe half-opens only the circuits of deployments whose model the backend lists;
// the others stay open and the prober keeps probing (H8).
func TestProbeKeepsUnlistedDeploymentsOpen(t *testing.T) {
	probe := newScriptedProbe()
	r := New(Options{Probe: probe.probe})
	x := backend("x", 0)
	listed, missing := queuedModel("listed", 10, time.Hour, x), queuedModel("missing", 10, time.Hour, x)
	r.Configure(circuitSnapshot(1, time.Millisecond, listed, missing))
	fail(r, listed.Deployments[0], 1)
	fail(r, missing.Deployments[0], 1)
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	stopped := make(chan struct{})
	go func() {
		r.RunProbers(ctx)
		close(stopped)
	}()
	defer func() { stop(); <-stopped }()

	probe.nextCall(t)
	probe.unlisted = map[string]bool{"missing@x": true}
	probe.answers <- nil
	// The prober keeps going for the unlisted deployment.
	call := probe.nextCall(t)
	if _, _, err := r.Acquire(context.Background(), missing, Avoid{}); !errors.Is(err, ErrNoHealthyDeployment) {
		t.Errorf("missing after the probe = %v, want its circuit still open", err)
	}
	trial, _, err := r.Acquire(context.Background(), listed, Avoid{})
	if err != nil {
		t.Fatalf("listed after the probe = %v, want its trial", err)
	}
	trial.Report(Success, "")
	trial.Release()
	// The model is back on the backend: the next probe half-opens it too.
	probe.answers <- errors.New("down")
	probe.nextCall(t)
	probe.unlisted = nil
	probe.answers <- nil
	call = probe.nextCall(t)
	trial, _, err = r.Acquire(context.Background(), missing, Avoid{})
	if err != nil {
		t.Fatalf("missing after its model came back = %v, want its trial", err)
	}
	trial.Report(Success, "")
	trial.Release()
	awaitDone(t, call.ctx, "after the last circuit closed")
}

type logBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	return len(p), nil
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}

// At config apply every backend is probed once, in the background, and each
// deployment whose model it does not list is warned about (H8).
// hintedError is a probe error for a models list answering 404 (provider's
// PathMissingError): it says what base_url should hold.
type hintedError struct{ hint string }

func (e hintedError) Error() string       { return "models list answered 404" }
func (e hintedError) BaseURLHint() string { return e.hint }

func TestModelCheckWarnsPerMissingModel(t *testing.T) {
	var logs logBuffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	var mu sync.Mutex
	probed := map[string]int{}
	done := make(chan struct{}, 4)
	c := NewModelChecker(func(_ context.Context, b *config.Backend) (func(string) bool, error) {
		defer func() { done <- struct{}{} }()
		mu.Lock()
		probed[b.ID]++
		mu.Unlock()
		switch b.ID {
		case "down":
			return nil, errors.New("connection refused")
		case "nopath":
			return nil, fmt.Errorf("probe: %w", hintedError{"base_url should end in /v1"})
		}
		return func(model string) bool { return model != "wrong@x" }, nil
	}, logger)
	x, y, down := backend("x", 0), backend("y", 0), backend("down", 0)
	nopath := &config.Backend{ID: "nopath", BaseURL: "http://vllm:8000"}
	ctx, stop := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		c.Run(ctx)
		close(stopped)
	}()
	c.Check(circuitSnapshot(1, time.Hour, queuedModel("right", 1, time.Hour, x, y), queuedModel("wrong", 1, time.Hour, x),
		queuedModel("other", 1, time.Hour, down), queuedModel("lost", 1, time.Hour, nopath)))
	for range 4 {
		select {
		case <-done:
		case <-time.After(waitTimeout):
			t.Fatal("model check did not probe every backend")
		}
	}
	stop()
	<-stopped
	out := logs.String()
	if !strings.Contains(out, `msg="the backend does not list the deployment's model" backend=x deployment_model=wrong@x`) ||
		strings.Count(out, "does not list") != 1 {
		t.Errorf("log:\n%s\nwant one warning, for wrong@x", out)
	}
	if !strings.Contains(out, `level=WARN msg="model check skipped: the backend did not answer" backend=down error="connection refused"`) {
		t.Errorf("log:\n%s\nwant the unreachable backend named", out)
	}
	// A models list answering 404: the backend is up and its base_url likely wrong.
	if !strings.Contains(out, `level=WARN msg="the backend has no models list at its base_url" backend=nopath base_url=http://vllm:8000 hint="base_url should end in /v1"`) ||
		strings.Contains(out, "skipped: the backend did not answer\" backend=nopath") {
		t.Errorf("log:\n%s\nwant a warning naming nopath's base_url, with the hint", out)
	}
	if probed["x"] != 1 || probed["y"] != 1 || probed["down"] != 1 || probed["nopath"] != 1 {
		t.Errorf("probes %v, want one per backend", probed)
	}
}

// E4: a half-open trial is decided when its response starts: the circuit closes at
// once and the deployment takes other requests while the trial still runs; a later
// failure of that request counts as an ordinary failure against the closed circuit.
func TestHalfOpenTrialIsDecidedWhenItsResponseStarts(t *testing.T) {
	obs := &events{}
	probe := newScriptedProbe()
	r := New(Options{Probe: probe.probe, Observer: obs})
	m := queuedModel("m", 10, time.Hour, backend("a", 0))
	r.Configure(circuitSnapshot(2, time.Hour, m))
	a := m.Deployments[0]
	fail(r, a, 1) // threshold 2: halfOpen's failure opens it
	halfOpen(t, r, probe, a)
	trial, _, err := r.Acquire(context.Background(), m, Avoid{})
	if err != nil {
		t.Fatal(err)
	}
	trial.ResponseStarted()
	if isOpen(r, a) {
		t.Fatal("still open after the trial's response started")
	}
	other, _, err := r.Acquire(context.Background(), m, Avoid{})
	if err != nil {
		t.Fatalf("request during the started trial = %v, want a's slot", err)
	}
	other.Release()
	if transitions, _ := obs.get(); !slices.Equal(transitions, []string{"a/m@a open", "a/m@a half_open", "a/m@a closed"}) {
		t.Errorf("observer told %v", transitions)
	}
	// The trial breaks off later: one ordinary failure (threshold 2), not a re-open.
	trial.Report(Failure, "broke off")
	trial.Release()
	if isOpen(r, a) {
		t.Fatal("the started trial's later failure re-opened the circuit")
	}
	fail(r, a, 1)
	if !isOpen(r, a) {
		t.Fatal("the started trial's failure was not counted")
	}
}

// A response starting decides nothing on a deployment whose circuit is closed: the
// request's outcome counts when it ends, as ever.
func TestResponseStartedOutsideATrialChangesNothing(t *testing.T) {
	r := New(Options{})
	m := queuedModel("m", 10, time.Hour, backend("a", 0))
	r.Configure(circuitSnapshot(3, time.Hour, m))
	a := m.Deployments[0]
	fail(r, a, 1)
	slot, _, err := r.Acquire(context.Background(), m, Avoid{})
	if err != nil {
		t.Fatal(err)
	}
	slot.ResponseStarted()
	slot.Report(Failure, "broke off")
	slot.Release()
	fail(r, a, 1)
	if !isOpen(r, a) {
		t.Fatal("the third failure did not open: a started response reset the count")
	}
}

// N-C2: response timeouts are neutral until responseTimeoutsAsFailure arrive in a row
// with no success between; from then on each counts as a failure.
func TestResponseTimeoutsInARowCountAsFailures(t *testing.T) {
	r := New(Options{})
	m := queuedModel("m", 10, time.Hour, backend("a", 0))
	r.Configure(circuitSnapshot(2, time.Hour, m))
	a := m.Deployments[0]
	timeout := func(n int) {
		for range n {
			r.report(a, 0, ResponseTimeout, "no response")
		}
	}
	timeout(responseTimeoutsAsFailure - 1)
	r.report(a, 0, Success, "")
	timeout(responseTimeoutsAsFailure - 1)
	if isOpen(r, a) || r.circuits[keyOf(a)].failures != 0 {
		t.Fatal("response timeouts below the run counted")
	}
	// Other outcomes between them do not break the run.
	r.report(a, 0, Neutral, "")
	timeout(1) // the run's third: one failure (threshold 2)
	if isOpen(r, a) {
		t.Fatal("opened at the first counted response timeout, threshold 2")
	}
	timeout(1)
	if !isOpen(r, a) {
		t.Fatal("not open after two counted response timeouts")
	}
}

// N-C2: a half-open trial that runs into its response timeout failed: the circuit
// opens again.
func TestHalfOpenTrialResponseTimeoutReopens(t *testing.T) {
	obs := &events{}
	probe := newScriptedProbe()
	r := New(Options{Probe: probe.probe, Observer: obs})
	m := queuedModel("m", 10, time.Hour, backend("a", 0))
	r.Configure(circuitSnapshot(1, time.Hour, m))
	a := m.Deployments[0]
	halfOpen(t, r, probe, a)
	trial, _, err := r.Acquire(context.Background(), m, Avoid{})
	if err != nil {
		t.Fatal(err)
	}
	trial.Report(ResponseTimeout, "no response")
	trial.Release()
	if _, _, err := r.Acquire(context.Background(), m, Avoid{}); !errors.Is(err, ErrNoHealthyDeployment) {
		t.Fatalf("request after the trial timed out = %v, want ErrNoHealthyDeployment", err)
	}
	if transitions, _ := obs.get(); !slices.Equal(transitions, []string{"a/m@a open", "a/m@a half_open", "a/m@a open"}) {
		t.Errorf("observer told %v", transitions)
	}
}

// N-C2: the prober keeps probing a backend while its circuits are half-open: a
// success leaves them half-open, a failure opens them again; the prober stops once
// a trial closed the last one.
func TestProberKeepsProbingHalfOpenCircuits(t *testing.T) {
	obs := &events{}
	probe := newScriptedProbe()
	r := New(Options{Probe: probe.probe, Observer: obs})
	m := queuedModel("m", 10, time.Hour, backend("a", 0))
	r.Configure(circuitSnapshot(1, time.Millisecond, m))
	a := m.Deployments[0]
	fail(r, a, 1)
	ctx, stop := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		r.RunProbers(ctx)
		close(stopped)
	}()
	defer func() { stop(); <-stopped }()

	probe.nextCall(t)
	probe.answers <- nil // half-open
	probe.nextCall(t)
	probe.answers <- nil // still half-open, still probed
	probe.nextCall(t)
	probe.answers <- errors.New("down")
	call := probe.nextCall(t)
	if _, _, err := r.Acquire(context.Background(), m, Avoid{}); !errors.Is(err, ErrNoHealthyDeployment) {
		t.Fatalf("request after the failed probe = %v, want the circuit open again", err)
	}
	probe.answers <- nil
	call = probe.nextCall(t)
	trial, _, err := r.Acquire(context.Background(), m, Avoid{})
	if err != nil {
		t.Fatalf("request after the probe = %v, want the trial", err)
	}
	trial.Report(Success, "")
	trial.Release()
	awaitDone(t, call.ctx, "after the trial closed the last circuit")
	if len(probersRunning(r)) != 0 {
		t.Errorf("probers %v with every circuit closed", probersRunning(r))
	}
	if transitions, _ := obs.get(); !slices.Equal(transitions, []string{"a/m@a open", "a/m@a half_open", "a/m@a open",
		"a/m@a half_open", "a/m@a closed"}) {
		t.Errorf("observer told %v", transitions)
	}
}

// notClosed returns when each circuit that is not closed (open or half-open) opened.
func notClosed(r *Router) map[DeploymentID]time.Time {
	out := make(map[DeploymentID]time.Time)
	for key, c := range r.Circuits() {
		out[key] = c.OpenedAt
	}
	return out
}
