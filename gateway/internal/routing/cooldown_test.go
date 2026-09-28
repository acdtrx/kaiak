package routing

import (
	"context"
	"testing"
	"time"
)

// A deployment that answered 429 takes no first attempt while another deployment
// of the model is eligible — even one with more requests in flight.
func TestCoolingDeploymentIsSkipped(t *testing.T) {
	r := New(Options{})
	m := queuedModel("m", 10, time.Hour, backend("a", 0), backend("b", 0))
	a, b := keyOf(m.Deployments[0]), keyOf(m.Deployments[1])
	slot, _, err := r.Acquire(context.Background(), m, Avoid{Refused: []DeploymentID{b}})
	if err != nil {
		t.Fatal(err)
	}
	slot.Throttled(time.Hour)
	slot.Release()
	if until, ok := r.CoolingDown()[a]; !ok || time.Until(until) < 59*time.Minute {
		t.Fatalf("cooling down %v, want a for an hour", r.CoolingDown())
	}
	_, releaseHeld := acquire(r, m)
	defer releaseHeld()
	for range 4 {
		d, release := acquire(r, m)
		if keyOf(d) != b {
			t.Fatalf("chose %v, want %v: a cools down", keyOf(d), b)
		}
		release()
	}
}

// Every deployment of the model cooling down, or the other one open: the cooling
// deployment is used — the client gets its 429 honestly.
func TestCoolingDeploymentIsUsedWhenNoOtherIsEligible(t *testing.T) {
	t.Run("all cooling", func(t *testing.T) {
		r := New(Options{})
		m := queuedModel("m", 10, time.Hour, backend("a", 0), backend("b", 0))
		r.throttle(keyOf(m.Deployments[0]), time.Hour)
		r.throttle(keyOf(m.Deployments[1]), time.Hour)
		seen := map[DeploymentID]bool{}
		for range 4 {
			d, release := acquire(r, m)
			seen[keyOf(d)] = true
			release()
		}
		if len(seen) != 2 {
			t.Errorf("used %v, want both in turn", seen)
		}
	})
	t.Run("the other open", func(t *testing.T) {
		r := New(Options{})
		m := queuedModel("m", 10, time.Hour, backend("a", 0), backend("b", 0))
		r.Configure(circuitSnapshot(1, time.Hour, m))
		r.throttle(keyOf(m.Deployments[0]), time.Hour)
		fail(r, m.Deployments[1], 1)
		if d, release := acquire(r, m); keyOf(d) != keyOf(m.Deployments[0]) {
			t.Errorf("chose %v, want the cooling a", keyOf(d))
		} else {
			release()
		}
	})
	t.Run("a single deployment", func(t *testing.T) {
		r := New(Options{})
		m := queuedModel("m", 10, time.Hour, backend("a", 0))
		r.throttle(keyOf(m.Deployments[0]), time.Hour)
		_, release := acquire(r, m)
		release()
	})
}

// A request waits for the eligible deployment's slot rather than take the cooling
// one; when the cooldown ends, the dispatcher hands it the no longer cooling one.
func TestQueuedRequestGetsTheDeploymentWhoseCooldownEnds(t *testing.T) {
	r := New(Options{})
	m := queuedModel("m", 10, time.Hour, backend("a", 0), backend("b", 1))
	a := keyOf(m.Deployments[0])
	held, _, err := r.Acquire(context.Background(), m, Avoid{Refused: []DeploymentID{a}})
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	r.throttle(a, 50*time.Millisecond)
	queued := enqueue(t, r, context.Background(), "queued", m, 1)
	got := receive(t, queued)
	if got.err != nil || keyOf(got.slot.Deployment) != a || !got.wait.Queued {
		t.Fatalf("queued request: %v on %v queued %v, want a once it cooled down", got.err, keyOf(got.slot.Deployment), got.wait.Queued)
	}
	got.slot.Release()
	if len(r.CoolingDown()) != 0 {
		t.Errorf("cooling down %v, want none", r.CoolingDown())
	}
}

// The last eligible deployment starting to cool down makes the cooling ones usable
// again: requests waiting for it get a free slot on another at once.
func TestWaitersGetACoolingDeploymentWhenTheLastOtherCoolsDown(t *testing.T) {
	r := New(Options{})
	m := queuedModel("m", 10, time.Hour, backend("a", 0), backend("b", 1))
	a := keyOf(m.Deployments[0])
	held, _, err := r.Acquire(context.Background(), m, Avoid{Refused: []DeploymentID{a}})
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	r.throttle(a, time.Hour)
	queued := enqueue(t, r, context.Background(), "queued", m, 1)
	held.Throttled(time.Hour)
	got := receive(t, queued)
	if got.err != nil || keyOf(got.slot.Deployment) != a {
		t.Fatalf("queued request: %v on %v, want a", got.err, keyOf(got.slot.Deployment))
	}
	got.slot.Release()
}

// Cooldowns belong to deployments the applied config has: a 429 on one it does not
// have is not tracked, and a reload that drops one drops its cooldown.
func TestCooldownsFollowTheConfig(t *testing.T) {
	r := New(Options{})
	m := queuedModel("m", 10, time.Hour, backend("a", 0), backend("b", 0))
	gone := queuedModel("gone", 10, time.Hour, backend("c", 0))
	r.Configure(circuitSnapshot(5, time.Hour, m))
	r.throttle(keyOf(gone.Deployments[0]), time.Hour)
	r.throttle(keyOf(m.Deployments[0]), time.Hour)
	if n := len(r.CoolingDown()); n != 1 {
		t.Fatalf("cooling down %v, want a only", r.CoolingDown())
	}
	r.Configure(circuitSnapshot(5, time.Hour, queuedModel("m", 10, time.Hour, backend("b", 0))))
	if n := len(r.CoolingDown()); n != 0 {
		t.Errorf("cooling down %v after a reload without a, want none", r.CoolingDown())
	}
}

// A later 429 on a cooling deployment moves the cooldown's end to the later of the
// two: a short Retry-After after a long one never ends the long cooldown early.
func TestLaterThrottleNeverShortensTheCooldown(t *testing.T) {
	r := New(Options{})
	m := queuedModel("m", 10, time.Hour, backend("a", 0), backend("b", 0))
	a := keyOf(m.Deployments[0])
	r.throttle(a, time.Hour)
	long := r.CoolingDown()[a]
	r.throttle(a, time.Millisecond)
	if until, ok := r.CoolingDown()[a]; !ok || !until.Equal(long) {
		t.Fatalf("cooling until %v (%v) after a shorter 429, want the hour's end %v", until, ok, long)
	}
	r.throttle(a, 2*time.Hour)
	if until := r.CoolingDown()[a]; !until.After(long) {
		t.Errorf("cooling until %v after a longer 429, want past %v", until, long)
	}
}
