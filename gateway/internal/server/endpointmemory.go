package server

import (
	"sync"
	"time"

	"kaiak/internal/config"
)

// MissingEndpoints remembers which backends' servers lack an endpoint their type
// serves (an older version answering upstream_endpoint_missing), for one probe
// interval each (docs/specs/GATEWAY.md, Providers → An endpoint missing from a
// server): routing leaves their deployments out for that endpoint meanwhile, so a
// server whose instant 404 makes it look least loaded does not draw the endpoint's
// traffic — and the retries it would cost — while it serves its other endpoints as
// before. Gateway-local and in memory, like circuits. An entry leaves when a request
// for its endpoint finds it expired, or when an applied config no longer has its
// backend (Retain).
type MissingEndpoints struct {
	mu    sync.Mutex
	until map[missingEndpoint]time.Time
}

type missingEndpoint struct {
	backend  string
	endpoint endpoint
}

// NewMissingEndpoints returns an empty memory.
func NewMissingEndpoints() *MissingEndpoints {
	return &MissingEndpoints{until: make(map[missingEndpoint]time.Time)}
}

// Retain forgets every backend not in backends, the applied config's: no request
// visits a removed backend's entries again, so nothing else would. A request still
// running on an older config may remember a removed backend after this; the next
// applied config forgets it.
func (m *MissingEndpoints) Retain(backends map[string]*config.Backend) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for key := range m.until {
		if _, ok := backends[key.backend]; !ok {
			delete(m.until, key)
		}
	}
}

// remember records that backend's server lacks ep until now+ttl, and reports whether
// it was not already remembered — the one moment worth a warning per interval.
func (m *MissingEndpoints) remember(backend string, ep endpoint, now time.Time, ttl time.Duration) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := missingEndpoint{backend, ep}
	fresh := !now.Before(m.until[key])
	m.until[key] = now.Add(ttl)
	return fresh
}

// exclude is model as routing sees it for ep at now: its deployments on backends
// remembered as lacking ep left out — model itself when there are none, and when
// every deployment is on one (they are tried again: a server may have been upgraded).
func (m *MissingEndpoints) exclude(model *config.Model, ep endpoint, now time.Time) *config.Model {
	m.mu.Lock()
	defer m.mu.Unlock()
	lacks := func(d config.Deployment) bool {
		key := missingEndpoint{d.Backend.ID, ep}
		until, ok := m.until[key]
		if ok && !now.Before(until) {
			delete(m.until, key)
			return false
		}
		return ok
	}
	kept := make([]config.Deployment, 0, len(model.Deployments))
	for _, d := range model.Deployments {
		if !lacks(d) {
			kept = append(kept, d)
		}
	}
	if len(kept) == len(model.Deployments) || len(kept) == 0 {
		return model
	}
	view := *model
	view.Deployments = kept
	return &view
}
