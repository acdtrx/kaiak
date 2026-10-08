package routing

import (
	"time"

	"kaiak/internal/config"
)

// Serving is what a router serves against one config snapshot, taken at one instant:
// every backend, model and deployment of the snapshot, and the backends and models a
// reload dropped that still have requests in flight or waiting. The status report and
// the metrics both format it (docs/specs/CONTROL-PROTOCOL.md, Messages → Status;
// docs/specs/GATEWAY.md, Observability).
type Serving struct {
	Backends    map[string]BackendServing
	Models      map[string]ModelServing
	Deployments map[DeploymentID]DeploymentServing
}

// BackendServing is one backend's load.
type BackendServing struct {
	// MaxInFlight is the snapshot's cap on the backend (0: none, or the snapshot does
	// not have the backend); Share is this gateway's part of it, the cap it enforces
	// (0 without a cap).
	MaxInFlight int64
	Share       int64
	// InFlight counts the requests running on the backend.
	InFlight int
}

// ModelServing is one public model's queue.
type ModelServing struct {
	// Queued counts the requests waiting in the model's queue.
	Queued int
}

// DeploymentServing is one deployment's health.
type DeploymentServing struct {
	// Circuit is its circuit's state; OpenedAt is when an open or half-open circuit
	// opened, zero while it is closed.
	Circuit  CircuitState
	OpenedAt time.Time
	// CoolingUntil is when its 429 cooldown ends; zero while it is not cooling down.
	CoolingUntil time.Time
}

// CoolingDown reports whether the deployment cools down after a 429.
func (d DeploymentServing) CoolingDown() bool { return !d.CoolingUntil.IsZero() }

// Serving returns what r serves against s; with s nil, only the backends and models
// with requests in flight or waiting. Its maps are the caller's.
func (r *Router) Serving(s *config.Snapshot) Serving {
	out := Serving{Backends: map[string]BackendServing{}, Models: map[string]ModelServing{},
		Deployments: map[DeploymentID]DeploymentServing{}}
	if s != nil {
		for id, b := range s.Backends {
			out.Backends[id] = BackendServing{MaxInFlight: b.MaxInFlight}
		}
		for name, m := range s.Models {
			out.Models[name] = ModelServing{}
			for _, d := range m.Deployments {
				out.Deployments[IDOf(d)] = DeploymentServing{Circuit: CircuitClosed}
			}
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, b := range out.Backends {
		if b.MaxInFlight > 0 {
			b.Share = r.share(b.MaxInFlight)
			out.Backends[id] = b
		}
	}
	for id, n := range r.backendLoad {
		b := out.Backends[id]
		b.InFlight = n
		out.Backends[id] = b
	}
	for name, q := range r.queues {
		out.Models[name] = ModelServing{Queued: q.Len()}
	}
	// Circuits and cooldowns are kept only for the applied config's deployments; the
	// snapshot's are the ones reported.
	for key, c := range r.circuits {
		d, ok := out.Deployments[key]
		if !ok || c.openedAt.IsZero() {
			continue
		}
		d.Circuit, d.OpenedAt = CircuitOpen, c.openedAt
		if c.halfOpen {
			d.Circuit = CircuitHalfOpen
		}
		out.Deployments[key] = d
	}
	for key, c := range r.cooldowns {
		if d, ok := out.Deployments[key]; ok {
			d.CoolingUntil = c.until
			out.Deployments[key] = d
		}
	}
	return out
}
