package server

import (
	"sync"
	"time"

	"kaiak/internal/config"
	"kaiak/internal/provider"
	"kaiak/internal/routing"
)

// MissingEndpoints remembers which deployments' servers do not serve an endpoint
// their type serves (upstream_endpoint_missing), for one probe interval each
// (docs/specs/GATEWAY.md, Providers → An endpoint missing from a server): routing
// leaves them out for that endpoint meanwhile, so a deployment whose instant answer
// makes it look least loaded does not draw the endpoint's traffic — and the retries it
// would cost — while it serves its other endpoints as before. Per deployment, not per
// backend: a router-mode llama-server runs each model with its own flags behind one
// base_url. Gateway-local and in memory, like circuits. An entry leaves when a request
// for its endpoint finds it expired, or when an applied config no longer has its
// deployment (Retain).
type MissingEndpoints struct {
	mu    sync.Mutex
	until map[missingEndpoint]time.Time
}

type missingEndpoint struct {
	deployment routing.DeploymentID
	api        provider.Endpoint
}

// NewMissingEndpoints returns an empty memory.
func NewMissingEndpoints() *MissingEndpoints {
	return &MissingEndpoints{until: make(map[missingEndpoint]time.Time)}
}

// Retain forgets every deployment not in models, the applied config's: no request
// visits a removed deployment's entries again, so nothing else would. A request still
// running on an older config may remember a removed deployment after this; the next
// applied config forgets it.
func (m *MissingEndpoints) Retain(models map[string]*config.Model) {
	kept := make(map[routing.DeploymentID]bool)
	for _, model := range models {
		for _, d := range model.Deployments {
			kept[routing.IDOf(d)] = true
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for key := range m.until {
		if !kept[key.deployment] {
			delete(m.until, key)
		}
	}
}

// remember records that d's server does not serve api until now+ttl, unless that is
// already remembered, and reports whether it was not — the one moment worth a
// warning. An entry is never extended: it ends one interval after it was set, so the
// deployment is tried again then and, still lacking the endpoint, remembered afresh
// with its warning — once per interval, however steady the traffic, also while every
// deployment of the model is remembered and they are all tried meanwhile.
func (m *MissingEndpoints) remember(d config.Deployment, api provider.Endpoint, now time.Time, ttl time.Duration) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := missingEndpoint{routing.IDOf(d), api}
	if now.Before(m.until[key]) {
		return false
	}
	m.until[key] = now.Add(ttl)
	return true
}

// exclude is model as routing sees it for api at now: its deployments remembered as
// not serving api left out — model itself when there are none, and when every
// deployment is (they are tried again: a server may have been upgraded, or restarted
// with another model or other flags).
func (m *MissingEndpoints) exclude(model *config.Model, api provider.Endpoint, now time.Time) *config.Model {
	m.mu.Lock()
	defer m.mu.Unlock()
	lacks := func(d config.Deployment) bool {
		key := missingEndpoint{routing.IDOf(d), api}
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
