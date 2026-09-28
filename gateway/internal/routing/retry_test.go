package routing

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRetryNeverUsesARefusedDeployment(t *testing.T) {
	r := New(Options{})
	m := queuedModel("m", 10, time.Hour, backend("a", 0), backend("b", 0), backend("c", 0))
	a, b, c := keyOf(m.Deployments[0]), keyOf(m.Deployments[1]), keyOf(m.Deployments[2])
	// Many times over, so turn-taking cannot hide a wrong choice.
	for range 6 {
		slot, _, err := r.Acquire(context.Background(), m, Avoid{Refused: []DeploymentID{a, c}})
		if err != nil {
			t.Fatal(err)
		}
		if got := keyOf(slot.Deployment); got != b {
			t.Fatalf("chose %v, want %v", got, b)
		}
		slot.Release()
	}
	// Every deployment refused (tried): nothing left, at once, without queueing.
	_, wait, err := r.Acquire(context.Background(), m, Avoid{Refused: []DeploymentID{a, b, c}})
	if !errors.Is(err, ErrNoHealthyDeployment) || wait.Queued {
		t.Errorf("err %v queued %v, want ErrNoHealthyDeployment at once", err, wait.Queued)
	}
}

// A retry never goes back to a deployment tried: with the only other one open,
// none is left.
func TestRetryWithTheOthersOpenHasNoDeploymentLeft(t *testing.T) {
	r := New(Options{})
	m := queuedModel("m", 10, time.Hour, backend("a", 0), backend("b", 0))
	r.Configure(circuitSnapshot(1, time.Hour, m))
	a := keyOf(m.Deployments[0])
	fail(r, m.Deployments[1], 1)
	_, wait, err := r.Acquire(context.Background(), m, Avoid{Refused: []DeploymentID{a}})
	if !errors.Is(err, ErrNoHealthyDeployment) || wait.Queued {
		t.Errorf("err %v queued %v, want ErrNoHealthyDeployment at once", err, wait.Queued)
	}
}

func TestQueuedRetryWaitsForItsDeploymentAndLetsOthersPass(t *testing.T) {
	r := New(Options{})
	m := queuedModel("m", 10, time.Hour, backend("a", 1), backend("b", 1))
	a, b := keyOf(m.Deployments[0]), keyOf(m.Deployments[1])
	holdA, releaseA := acquire(r, m)
	holdB, releaseB := acquire(r, m)
	if keyOf(holdA) == keyOf(holdB) {
		t.Fatal("both slots on one deployment")
	}
	if keyOf(holdA) != a {
		releaseA, releaseB = releaseB, releaseA
	}

	// A retry that must not use a (it was tried) waits first; a new request
	// waits behind it.
	retry := make(chan result, 1)
	go func() {
		slot, wait, err := r.Acquire(context.Background(), m, Avoid{Refused: []DeploymentID{a}})
		retry <- result{"retry", slot, wait, err}
	}()
	waitQueued(t, r, "m", 1)
	other := enqueue(t, r, context.Background(), "other", m, 2)

	// a frees: the retry may not use it, so the request behind it gets it.
	releaseA()
	got := receive(t, other)
	if got.err != nil || keyOf(got.slot.Deployment) != a {
		t.Fatalf("request behind the retry: %v on %v, want a's slot", got.err, keyOf(got.slot.Deployment))
	}
	if !pending(retry) {
		t.Fatal("the retry was served a's slot")
	}
	// b frees: the retry gets it.
	releaseB()
	res := receive(t, retry)
	if res.err != nil || keyOf(res.slot.Deployment) != b || !res.wait.Queued {
		t.Errorf("retry: %v on %v queued %v, want b after a wait", res.err, keyOf(res.slot.Deployment), res.wait.Queued)
	}
	res.slot.Release()
	got.slot.Release()
	if n := r.InFlightByBackend(); len(n) != 0 {
		t.Errorf("in flight %v, want none", n)
	}
}
