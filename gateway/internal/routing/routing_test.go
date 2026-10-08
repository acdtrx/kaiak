package routing

import (
	"slices"
	"sync"
	"testing"
	"time"

	"kaiak/internal/config"
)

func model(name string, deployments ...string) *config.Model {
	m := &config.Model{Name: name}
	for _, backend := range deployments {
		m.Deployments = append(m.Deployments, config.Deployment{Backend: &config.Backend{ID: backend}, Model: name})
	}
	return m
}

func TestTiesTakeTurns(t *testing.T) {
	r := New(Options{})
	m := model("m", "a", "b", "c")
	var got []string
	for range 6 {
		d, release := acquire(r, m)
		got = append(got, d.Backend.ID)
		release()
	}
	if want := []string{"a", "b", "c", "a", "b", "c"}; !slices.Equal(got, want) {
		t.Errorf("idle deployments chosen %v, want %v", got, want)
	}
}

func TestFewestInFlightWins(t *testing.T) {
	r := New(Options{})
	m := model("m", "a", "b")
	_, releaseA := acquire(r, m) // a
	_, releaseB := acquire(r, m) // b
	d, releaseA2 := acquire(r, m)
	if d.Backend.ID != "a" {
		t.Fatalf("tie resolved to %s, want a", d.Backend.ID)
	}
	// a: 2, b: 1 — b wins until they are level again.
	d, releaseB2 := acquire(r, m)
	if d.Backend.ID != "b" {
		t.Errorf("chose %s with a busier, want b", d.Backend.ID)
	}
	releaseB()
	releaseB2()
	// a: 2, b: 0.
	for range 2 {
		if d, _ := acquire(r, m); d.Backend.ID != "b" {
			t.Errorf("chose %s, want the idle b", d.Backend.ID)
		}
	}
	releaseA()
	releaseA2()
	if got := inFlight(r); got["a"] != 0 || got["b"] != 2 {
		t.Errorf("in flight %v, want b: 2", got)
	}
}

func TestReleaseIsCountedOnce(t *testing.T) {
	r := New(Options{})
	m := model("m", "a")
	_, first := acquire(r, m)
	_, second := acquire(r, m)
	first()
	first()
	if got := inFlight(r)["a"]; got != 1 {
		t.Errorf("in flight %d after one request released twice, want 1", got)
	}
	second()
	if got := inFlight(r); len(got) != 0 {
		t.Errorf("in flight %v after every release, want none", got)
	}
}

func TestCountsSurviveAConfigSwap(t *testing.T) {
	r := New(Options{})
	old := model("m", "a", "b")
	_, releaseA := acquire(r, old)
	// A reload builds new Backend values; identity is backend ID + backend model.
	reloaded := model("m", "a", "b")
	if d, _ := acquire(r, reloaded); d.Backend.ID != "b" {
		t.Errorf("after reload chose %s, want b (a still busy from the old snapshot)", d.Backend.ID)
	}
	releaseA()
	if got := inFlight(r); got["a"] != 0 || got["b"] != 1 {
		t.Errorf("in flight %v, want b: 1", got)
	}
}

func TestConcurrentAcquireAndRelease(t *testing.T) {
	r := New(Options{})
	m := model("m", "a", "b")
	var wg sync.WaitGroup
	for range 100 {
		wg.Go(func() {
			_, release := acquire(r, m)
			release()
		})
	}
	wg.Wait()
	if got := inFlight(r); len(got) != 0 {
		t.Errorf("in flight %v, want none", got)
	}
}

func TestConfigureForgetsRemovedModelsTurns(t *testing.T) {
	r := New(Options{})
	old, kept := queuedModel("old", 1, time.Hour, backend("a", 0)), queuedModel("kept", 1, time.Hour, backend("a", 0))
	r.Configure(circuitSnapshot(1, time.Hour, old, kept))
	for _, m := range []*config.Model{old, kept} {
		_, release := acquire(r, m)
		release()
	}
	r.Configure(circuitSnapshot(1, time.Hour, kept))
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.next["old"]; ok || len(r.next) != 1 {
		t.Errorf("turns %v after the reload dropped old, want kept's alone", r.next)
	}
}
