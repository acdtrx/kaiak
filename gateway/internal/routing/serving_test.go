package routing

import (
	"context"
	"testing"
	"time"

	"kaiak/internal/config"
)

// inFlight is what r counts in flight per backend: only backends with requests
// running.
func inFlight(r *Router) map[string]int {
	counts := make(map[string]int)
	for id, b := range r.Serving(nil).Backends {
		counts[id] = b.InFlight
	}
	return counts
}

// queuedIn is the depth of each model's queue: only models with requests waiting.
func queuedIn(r *Router) map[string]int {
	counts := make(map[string]int)
	for name, m := range r.Serving(nil).Models {
		counts[name] = m.Queued
	}
	return counts
}

// coolingDown returns, per deployment r keeps a cooldown for, when it ends.
func coolingDown(r *Router) map[DeploymentID]time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[DeploymentID]time.Time, len(r.cooldowns))
	for key, c := range r.cooldowns {
		out[key] = c.until
	}
	return out
}

// Serving covers the snapshot it is taken against — every backend, model and
// deployment, idle ones at zero and closed — plus the backends and models a reload
// dropped that still have requests in flight or waiting; a backend's cap next to this
// gateway's share; each deployment's circuit, with its opening time, and cooldown.
func TestServingCoversTheSnapshotAndWhatADroppedOneLeftRunning(t *testing.T) {
	probe := func(context.Context, *config.Backend) (func(string) bool, error) { return nil, nil }
	r := New(Options{Probe: probe})
	a, b, c, gone := backend("a", 4), backend("b", 0), backend("c", 3), backend("gone", 1)
	chat := queuedModel("chat", 5, time.Hour, a, b)
	alias := &config.Model{Name: "alias", Queue: chat.Queue, Deployments: chat.Deployments[:1]}
	embed := queuedModel("embed", 5, time.Hour, c)
	idle := queuedModel("idle", 5, time.Hour, c)
	old := queuedModel("old", 5, time.Hour, gone)
	r.Configure(circuitSnapshot(1, time.Hour, chat, alias, embed, idle, old))
	_, releaseRetired := acquire(r, old)
	defer releaseRetired()
	ctx, leave := context.WithCancel(context.Background())
	waiting := enqueue(t, r, ctx, "waiting", old, 1)
	defer func() { leave(); receive(t, waiting) }()

	s := circuitSnapshot(1, time.Hour, chat, alias, embed, idle)
	r.Configure(s)
	r.SetLiveGateways(2)
	before := time.Now()
	fail(r, chat.Deployments[1], 1) // b: open
	fail(r, embed.Deployments[0], 1)
	if err := r.ProbeNow(context.Background(), "c", "test"); err != nil { // c: half-open
		t.Fatal(err)
	}
	r.throttle(IDOf(chat.Deployments[0]), time.Hour) // a: cooling down
	_, release := acquire(r, alias)
	defer release()

	got := r.Serving(s)
	wantBackends := map[string]BackendServing{"a": {MaxInFlight: 4, Share: 2, InFlight: 1}, "b": {}, "c": {MaxInFlight: 3, Share: 2},
		"gone": {InFlight: 1}}
	if len(got.Backends) != len(wantBackends) {
		t.Errorf("backends %v, want %v", got.Backends, wantBackends)
	}
	for id, want := range wantBackends {
		if got.Backends[id] != want {
			t.Errorf("backend %s = %+v, want %+v", id, got.Backends[id], want)
		}
	}
	wantModels := map[string]ModelServing{"chat": {}, "alias": {}, "embed": {}, "idle": {}, "old": {Queued: 1}}
	if len(got.Models) != len(wantModels) {
		t.Errorf("models %v, want %v", got.Models, wantModels)
	}
	for name, want := range wantModels {
		if got.Models[name] != want {
			t.Errorf("model %s = %+v, want %+v", name, got.Models[name], want)
		}
	}
	if len(got.Deployments) != 4 {
		t.Errorf("deployments %v, want chat's two, embed's and idle's: alias shares chat's on a", got.Deployments)
	}
	if d := got.Deployments[IDOf(chat.Deployments[0])]; d.Circuit != CircuitClosed || !d.OpenedAt.IsZero() ||
		!d.CoolingDown() || time.Until(d.CoolingUntil) < 59*time.Minute {
		t.Errorf("a = %+v, want closed and cooling down for an hour", d)
	}
	if d := got.Deployments[IDOf(chat.Deployments[1])]; d.Circuit != CircuitOpen || d.OpenedAt.Before(before) || d.CoolingDown() {
		t.Errorf("b = %+v, want open since the failure", d)
	}
	if d := got.Deployments[IDOf(embed.Deployments[0])]; d.Circuit != CircuitHalfOpen || d.OpenedAt.Before(before) {
		t.Errorf("c/embed = %+v, want half-open, opened at the failure", d)
	}
	if d := got.Deployments[IDOf(idle.Deployments[0])]; d != (DeploymentServing{Circuit: CircuitClosed}) {
		t.Errorf("c/idle = %+v, want closed and warm", d)
	}

	// Without a snapshot: only what runs or waits.
	bare := r.Serving(nil)
	if len(bare.Backends) != 2 || bare.Backends["a"] != (BackendServing{InFlight: 1}) || bare.Backends["gone"].InFlight != 1 ||
		len(bare.Models) != 1 || bare.Models["old"].Queued != 1 || len(bare.Deployments) != 0 {
		t.Errorf("without a snapshot: %+v, want a's and gone's in-flight counts and old's queue only", bare)
	}
}
