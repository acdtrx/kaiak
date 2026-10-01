package routing

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"sync"

	"kaiak/internal/config"
)

// modelCheckParallel bounds the backends a model check probes at once.
const modelCheckParallel = 8

// ModelChecker checks, for each config applied, that every deployment's backend
// lists the deployment's model (docs/specs/GATEWAY.md, Providers: wrong model on a
// host, wrong path to a host): one probe per backend, in the background, a warning
// per deployment whose model is missing, per backend whose models list is not where
// its base_url says and per backend that does not answer. It never delays or refuses
// a config; a newer config replaces one not yet checked.
type ModelChecker struct {
	probe  ProbeFunc
	logger *slog.Logger

	mu      sync.Mutex
	pending *config.Snapshot
	wake    chan struct{}
}

// NewModelChecker returns a checker probing with probe and warning on logger.
func NewModelChecker(probe ProbeFunc, logger *slog.Logger) *ModelChecker {
	return &ModelChecker{probe: probe, logger: logger, wake: make(chan struct{}, 1)}
}

// Check asks for s to be checked; it returns at once.
func (c *ModelChecker) Check(s *config.Snapshot) {
	c.mu.Lock()
	c.pending = s
	c.mu.Unlock()
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// Run checks the configs Check is given, the latest first come, until ctx ends
// (which also cuts a check under way short).
func (c *ModelChecker) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.wake:
		}
		c.mu.Lock()
		s := c.pending
		c.pending = nil
		c.mu.Unlock()
		if s != nil {
			c.check(ctx, s)
		}
	}
}

// pathMissing is a probe error saying the backend answered 404 for its models list
// (provider.PathMissingError): the server is up, and the backend's base_url is most
// likely wrong. BaseURLHint says what base_url should hold for the backend's type.
type pathMissing interface {
	error
	BaseURLHint() string
}

// check probes every backend of s with deployments, at most modelCheckParallel at
// once, and warns for each deployment whose model its backend does not list.
func (c *ModelChecker) check(ctx context.Context, s *config.Snapshot) {
	models := make(map[string][]string) // backend ID → backend-side model names
	for _, m := range s.Models {
		for _, d := range m.Deployments {
			if !slices.Contains(models[d.Backend.ID], d.Model) {
				models[d.Backend.ID] = append(models[d.Backend.ID], d.Model)
			}
		}
	}
	slots := make(chan struct{}, modelCheckParallel)
	var wg sync.WaitGroup
	for id, names := range models {
		b := s.Backends[id]
		wg.Go(func() {
			select {
			case slots <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-slots }()
			serves, err := c.probe(ctx, b)
			if ctx.Err() != nil {
				return
			}
			if pathErr, ok := errors.AsType[pathMissing](err); ok {
				c.logger.Warn("the backend has no models list at its base_url", "backend", id, "base_url", b.BaseURL,
					"hint", pathErr.BaseURLHint())
				return
			}
			if err != nil {
				// A warning: config apply is infrequent, and a backend out of reach
				// then (often a mistyped host) is what the operator needs to see.
				c.logger.Warn("model check skipped: the backend did not answer", "backend", id, "error", err.Error())
				return
			}
			slices.Sort(names)
			for _, name := range names {
				if !serves(name) {
					c.logger.Warn("the backend does not list the deployment's model", "backend", id, "deployment_model", name)
				}
			}
		})
	}
	wg.Wait()
}
