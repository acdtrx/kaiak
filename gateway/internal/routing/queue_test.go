package routing

import (
	"context"
	"errors"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"kaiak/internal/config"
)

// waitTimeout bounds every wait on a condition the test expects to happen.
const waitTimeout = 5 * time.Second

func acquire(r *Router, m *config.Model) (config.Deployment, func()) {
	slot, _, err := r.Acquire(context.Background(), m, Avoid{})
	if err != nil {
		panic(err)
	}
	return slot.Deployment, slot.Release
}

func backend(id string, maxInFlight int64) *config.Backend {
	return &config.Backend{ID: id, MaxInFlight: maxInFlight}
}

// queuedModel is a model on backends, one deployment each, with a queue of size.
func queuedModel(name string, size int, timeout time.Duration, backends ...*config.Backend) *config.Model {
	m := &config.Model{Name: name, Queue: config.Queue{Size: size, Timeout: timeout}}
	for _, b := range backends {
		m.Deployments = append(m.Deployments, config.Deployment{Backend: b, Model: name + "@" + b.ID})
	}
	return m
}

// waitQueued waits until n requests wait in model's queue.
func waitQueued(t *testing.T, r *Router, model string, n int) {
	t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for r.QueuedByModel()[model] != n {
		if time.Now().After(deadline) {
			t.Fatalf("queue %s holds %d, want %d", model, r.QueuedByModel()[model], n)
		}
		runtime.Gosched()
	}
}

// result is one Acquire's outcome, from a waiting goroutine.
type result struct {
	name string
	slot Slot
	wait Wait
	err  error
}

// enqueue starts an Acquire for m on its own goroutine, waits until it is queued
// (the queue then holds depth requests) and returns where its result arrives.
func enqueue(t *testing.T, r *Router, ctx context.Context, name string, m *config.Model, depth int) <-chan result {
	t.Helper()
	out := make(chan result, 1)
	go func() {
		slot, wait, err := r.Acquire(ctx, m, Avoid{})
		out <- result{name, slot, wait, err}
	}()
	waitQueued(t, r, m.Name, depth)
	return out
}

func receive(t *testing.T, c <-chan result) result {
	t.Helper()
	select {
	case res := <-c:
		return res
	case <-time.After(waitTimeout):
		t.Fatal("queued request never got an answer")
	}
	return result{}
}

func pending(c <-chan result) bool {
	select {
	case <-c:
		return false
	default:
		return true
	}
}

func TestCapIsRespectedUnderConcurrency(t *testing.T) {
	r := New(Options{})
	m := queuedModel("m", 100, time.Hour, backend("a", 3))
	const requests = 10
	var active, peak atomic.Int32
	admitted := make(chan struct{})
	proceed := make(chan struct{})
	var wg sync.WaitGroup
	for range requests {
		wg.Go(func() {
			slot, _, err := r.Acquire(context.Background(), m, Avoid{})
			if err != nil {
				t.Error(err)
				return
			}
			n := active.Add(1)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
			admitted <- struct{}{}
			<-proceed
			active.Add(-1)
			slot.Release()
		})
	}
	for range 3 {
		<-admitted
	}
	waitQueued(t, r, "m", requests-3)
	if got := r.InFlightByBackend()["a"]; got != 3 {
		t.Errorf("in flight %d with %d waiting, want the cap 3", got, requests-3)
	}
	// Each finished request lets exactly one waiter in.
	for range requests - 3 {
		proceed <- struct{}{}
		<-admitted
	}
	for range 3 {
		proceed <- struct{}{}
	}
	wg.Wait()
	if p := peak.Load(); p != 3 {
		t.Errorf("at most %d requests ran at once, want 3", p)
	}
	if got := r.InFlightByBackend(); len(got) != 0 {
		t.Errorf("in flight %v at the end, want none", got)
	}
}

func TestQueueIsServedInOrder(t *testing.T) {
	r := New(Options{})
	m := queuedModel("m", 10, time.Hour, backend("a", 1))
	_, release := acquire(r, m)
	var waits []<-chan result
	for i, name := range []string{"first", "second", "third"} {
		waits = append(waits, enqueue(t, r, context.Background(), name, m, i+1))
	}
	var order []string
	for _, w := range waits {
		release()
		res := receive(t, w)
		if res.err != nil || !res.wait.Queued || res.wait.Duration <= 0 {
			t.Fatalf("%s: %+v", res.name, res)
		}
		order = append(order, res.name)
		release = res.slot.Release
	}
	release()
	if want := []string{"first", "second", "third"}; !slices.Equal(order, want) {
		t.Errorf("served %v, want %v", order, want)
	}
}

func TestFreedSlotGoesToTheLongestWaitingAcrossModels(t *testing.T) {
	r := New(Options{})
	a, shared := backend("a", 1), backend("shared", 1)
	// x runs on a and on shared; y only on shared.
	x := queuedModel("x", 10, time.Hour, a, shared)
	y := queuedModel("y", 10, time.Hour, shared)
	_, releaseA := acquire(r, x)
	_, releaseShared := acquire(r, x)

	y1 := enqueue(t, r, context.Background(), "y1", y, 1)
	x1 := enqueue(t, r, context.Background(), "x1", x, 1)
	y2 := enqueue(t, r, context.Background(), "y2", y, 2)

	// A slot on shared: y1 waited longest.
	releaseShared()
	res := receive(t, y1)
	if res.slot.Deployment.Backend.ID != "shared" {
		t.Fatalf("y1 got %s", res.slot.Deployment.Backend.ID)
	}
	if !pending(x1) || !pending(y2) {
		t.Fatal("one slot served two waiters")
	}
	// A slot on a: only x can use it, although y2 waits too.
	releaseA()
	if res := receive(t, x1); res.slot.Deployment.Backend.ID != "a" {
		t.Fatalf("x1 got %s", res.slot.Deployment.Backend.ID)
	}
	res.slot.Release()
	if res := receive(t, y2); res.name != "y2" || res.slot.Deployment.Backend.ID != "shared" {
		t.Fatalf("after y1 finished: %+v", res)
	}
}

func TestQueueFullIsRefusedAtOnce(t *testing.T) {
	r := New(Options{})
	never := queuedModel("never", 0, time.Hour, backend("a", 1))
	_, release := acquire(r, never)
	defer release()
	if _, wait, err := r.Acquire(context.Background(), never, Avoid{}); !errors.Is(err, ErrQueueFull) || wait.Queued {
		t.Errorf("size 0: %v, %+v; want ErrQueueFull, not queued", err, wait)
	}

	small := queuedModel("small", 1, time.Hour, backend("b", 1))
	_, releaseB := acquire(r, small)
	ctx, cancel := context.WithCancel(context.Background())
	queued := enqueue(t, r, ctx, "queued", small, 1)
	if _, _, err := r.Acquire(context.Background(), small, Avoid{}); !errors.Is(err, ErrQueueFull) {
		t.Errorf("full queue: %v, want ErrQueueFull", err)
	}
	cancel()
	if res := receive(t, queued); !errors.Is(res.err, context.Canceled) {
		t.Errorf("cancelled waiter: %v", res.err)
	}
	releaseB()
}

func TestQueueTimeout(t *testing.T) {
	r := New(Options{})
	const timeout = 30 * time.Millisecond
	m := queuedModel("m", 10, timeout, backend("a", 1))
	_, release := acquire(r, m)
	slot, wait, err := r.Acquire(context.Background(), m, Avoid{})
	if !errors.Is(err, ErrQueueTimeout) || !wait.Queued || wait.Duration < timeout {
		t.Fatalf("got %v after %+v, want ErrQueueTimeout after at least %s", err, wait, timeout)
	}
	if slot.Deployment.Backend != nil {
		t.Error("a timed-out request got a slot")
	}
	if q := r.QueuedByModel(); len(q) != 0 {
		t.Errorf("queues %v after the timeout, want none", q)
	}
	release()
	if got := r.InFlightByBackend(); len(got) != 0 {
		t.Errorf("in flight %v, want none", got)
	}
}

func TestLeavingTheQueueFreesItsPlace(t *testing.T) {
	r := New(Options{})
	m := queuedModel("m", 10, time.Hour, backend("a", 1))
	_, release := acquire(r, m)
	ctx, cancel := context.WithCancel(context.Background())
	gone := enqueue(t, r, ctx, "gone", m, 1)
	stays := enqueue(t, r, context.Background(), "stays", m, 2)
	cancel()
	if res := receive(t, gone); !errors.Is(res.err, context.Canceled) || !res.wait.Queued {
		t.Fatalf("left: %+v", res)
	}
	waitQueued(t, r, "m", 1)
	// The slot skips the request that left.
	release()
	res := receive(t, stays)
	if res.err != nil {
		t.Fatal(res.err)
	}
	res.slot.Release()
	if got := r.InFlightByBackend(); len(got) != 0 {
		t.Errorf("in flight %v, want none", got)
	}
}

func TestConfiguredCaps(t *testing.T) {
	r := New(Options{})
	a := backend("a", 3)
	m := queuedModel("m", 10, time.Hour, a)
	var releases []func()
	for range 3 {
		_, release := acquire(r, m)
		releases = append(releases, release)
	}
	// A reload lowers the cap: running requests stay, new ones wait for it.
	r.Configure(&config.Snapshot{Backends: map[string]*config.Backend{"a": backend("a", 1)}})
	waiting := enqueue(t, r, context.Background(), "waiting", m, 1)
	releases[0]()
	releases[1]()
	if !pending(waiting) {
		t.Fatal("admitted with 1 in flight under a cap of 1")
	}
	releases[2]()
	res := receive(t, waiting)

	// A raised cap lets a waiter in at once.
	second := enqueue(t, r, context.Background(), "second", m, 1)
	r.Configure(&config.Snapshot{Backends: map[string]*config.Backend{"a": backend("a", 2)}})
	res2 := receive(t, second)
	if got := r.InFlightByBackend()["a"]; got != 2 {
		t.Errorf("in flight %d, want 2", got)
	}
	res.slot.Release()
	res2.slot.Release()
}

func TestQueueChangeIsToldOnEmptyAndNonEmpty(t *testing.T) {
	r := New(Options{})
	changes := make(chan struct{}, 10)
	r.OnServingChange(func() { changes <- struct{}{} })
	m := queuedModel("m", 10, time.Hour, backend("a", 1))
	_, release := acquire(r, m)
	first := enqueue(t, r, context.Background(), "first", m, 1)
	select {
	case <-changes:
	case <-time.After(waitTimeout):
		t.Fatal("no change told when the queue became non-empty")
	}
	second := enqueue(t, r, context.Background(), "second", m, 2)
	release()
	res := receive(t, first)
	res.slot.Release() // hands the slot to second: the queue is empty now
	res = receive(t, second)
	res.slot.Release()
	// Releases notify on the releasing goroutine, before they return.
	if n := len(changes); n != 1 {
		t.Errorf("%d more changes told, want 1 (the queue emptied); depth changes in between tell none", n)
	}
}

func TestCapIsSplitAmongLiveGateways(t *testing.T) {
	r := New(Options{})
	m := queuedModel("m", 10, time.Hour, backend("a", 4))
	r.Configure(circuitSnapshot(5, time.Hour, m, queuedModel("odd", 10, time.Hour, backend("b", 5)),
		queuedModel("small", 10, time.Hour, backend("c", 1))))
	r.SetLiveGateways(2)
	if got := r.MaxInFlightByBackend(); got["a"] != 2 || got["b"] != 3 || got["c"] != 1 {
		t.Fatalf("caps %v with 2 live gateways, want a 2 (4÷2), b 3 (5÷2 rounded up), c 1 (never 0)", got)
	}
	_, release1 := acquire(r, m)
	defer release1()
	_, release2 := acquire(r, m)
	defer release2()
	waiter := enqueue(t, r, context.Background(), "third", m, 1)
	// Fewer live gateways: the share rises and the waiter gets the slot at once.
	r.SetLiveGateways(0)
	res := receive(t, waiter)
	if res.err != nil {
		t.Fatalf("waiter = %v after the share rose", res.err)
	}
	res.slot.Release()
	if got := r.MaxInFlightByBackend()["a"]; got != 4 {
		t.Errorf("cap %d with 0 live gateways (counted as 1), want 4", got)
	}
}

// N-C3: after a reload, one model's queue holds waiters of two snapshots. A slot the
// new snapshot's deployments can use goes to its waiter, even behind an older
// waiter that cannot use it.
func TestFreedSlotReachesTheNewSnapshotsWaiter(t *testing.T) {
	r := New(Options{})
	a, b := backend("a", 1), backend("b", 1)
	old := queuedModel("m", 10, time.Hour, a)
	current := queuedModel("m", 10, time.Hour, a, b)
	r.Configure(circuitSnapshot(5, time.Hour, current))
	_, releaseA := acquire(r, current)
	defer releaseA()
	slotB, _, err := r.Acquire(context.Background(), current, Avoid{})
	if err != nil || slotB.Deployment.Backend.ID != "b" {
		t.Fatalf("second slot %+v, %v; want b", slotB.Deployment, err)
	}
	oldWaiter := enqueue(t, r, context.Background(), "old", old, 1)
	newWaiter := enqueue(t, r, context.Background(), "new", current, 2)
	slotB.Release()
	res := receive(t, newWaiter)
	if res.err != nil || res.slot.Deployment.Backend.ID != "b" {
		t.Fatalf("new waiter: %+v", res)
	}
	res.slot.Release()
	if !pending(oldWaiter) {
		t.Fatal("the old waiter got a slot on a deployment its model does not have")
	}
	releaseA()
	res = receive(t, oldWaiter)
	if res.err != nil || res.slot.Deployment.Backend.ID != "a" {
		t.Fatalf("old waiter: %+v", res)
	}
	res.slot.Release()
}
