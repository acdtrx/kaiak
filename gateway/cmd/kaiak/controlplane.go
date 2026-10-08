package main

import (
	"context"
	"log/slog"
	"os"
	"time"

	"kaiak/internal/config"
	"kaiak/internal/control"
	"kaiak/internal/limits"
	"kaiak/internal/logattr"
	"kaiak/internal/metrics"
	"kaiak/internal/routing"
	"kaiak/internal/telemetry/metric"
)

// controlPlane is control-plane mode's half of the process (docs/specs/GATEWAY.md,
// Control-plane mode): the control client, and the shared limiter it feeds.
type controlPlane struct {
	client   *control.Client
	limiter  *limits.Limiter
	bootWait time.Duration
	logger   *slog.Logger
}

// controlPlaneDeps are what newControlPlane wires the control-plane half to.
type controlPlaneDeps struct {
	// opts are the client's options from the settings, with the process's own set
	// (instance, applier, logger, start time, usage memory); newControlPlane sets
	// the rest.
	opts     control.Options
	holder   *config.Holder
	router   *routing.Router
	registry *metric.Registry
	ops      *metrics.Ops
}

// bootStopped is a stop signal that ended the boot wait.
type bootStopped struct{ sig os.Signal }

func (e bootStopped) Error() string { return "signal " + e.sig.String() + " during boot" }

// newControlPlane builds the shared limiter and the control client, and registers
// their metrics on the registry, so every family exists before the boot.
func newControlPlane(deps controlPlaneDeps) *controlPlane {
	cp := &controlPlane{bootWait: deps.opts.BootWait, logger: deps.opts.Logger}
	// The limiter reads the client's contact (the outage) and the client feeds the
	// limiter totals and usage generations; cp.client is set before any request or
	// scrape can read it.
	cp.limiter = limits.NewShared(deps.holder, time.Now, func() limits.Contact { return cp.client.LimitsContact() }, cp.logger)
	cp.limiter.ObserveSyncs(deps.ops.ObserveLimitsSync)
	opts := deps.opts
	opts.Serving = func() control.Serving {
		return servingStatus(deps.router.Serving(deps.holder.Current()))
	}
	opts.Observer = metrics.NewUsageDelivery(deps.registry)
	opts.OnTotals = func(totals limits.Totals, counted uint64) {
		cp.limiter.TakeTotals(totals, counted)
		// Backend caps are split among the live gateways the totals count.
		deps.router.SetLiveGateways(totals.LiveGateways)
	}
	cp.client = control.New(opts)
	metrics.RegisterControlState(deps.registry, controlState{cp.client, cp.limiter})
	// A model's queue starting or ending and a circuit opening or closing are told
	// within the status minimum gap; depth changes in between wait for the regular
	// report.
	deps.router.OnServingChange(cp.client.ServingChanged)
	return cp
}

// boot gets the first config, from the control plane or else the seed; for a
// control-plane config, it waits for the first totals (waitFirstTotals). No config
// from either is an error: the process exits, and its supervisor restarts it with
// backoff. A stop signal during the boot ends it with bootStopped: nothing is served
// and no usage exists yet, so run returns at once instead of binding the listeners.
// Once booted, the client follows the control plane in goBackground's work (run).
func (cp *controlPlane) boot(ctx context.Context, stop <-chan os.Signal, goBackground func(work func(context.Context))) error {
	bootDeadline := time.Now().Add(cp.bootWait)
	bootCtx, endBoot := stopOnSignal(ctx, stop, nil)
	err := cp.client.Boot(bootCtx)
	if err == nil {
		// The client follows the control plane until the drain is over, so requests
		// admitted during the grace period run on the newest config. It starts before
		// the listeners: the first totals come on its stream.
		goBackground(cp.run)
		// A seed boot (no config hash) serves only free models: no budget waits for
		// totals.
		if _, fromControlPlane := cp.client.AppliedConfigHash(); fromControlPlane {
			waitFirstTotals(bootCtx, cp.limiter, bootDeadline, cp.logger)
		}
	}
	if sig := endBoot(); sig != nil {
		return bootStopped{sig}
	}
	return err
}

// run is the client's work with the control plane — the config stream, usage
// delivery, status reports — until ctx ends.
func (cp *controlPlane) run(ctx context.Context) { cp.client.Run(ctx) }

// beforeDrain marks the gateway draining: the next status report, sent at once, says
// so.
func (cp *controlPlane) beforeDrain() { cp.client.SetDraining() }

// finish is the control-plane part of the drain's last step (control.Client.Finish),
// run once the drained requests' records are settled and before the background work
// stops: the usage flush until deadline, the drain's end, or until hurry ends; then,
// unless hurry has ended by then, the final draining status.
func (cp *controlPlane) finish(hurry context.Context, deadline time.Time) {
	cp.client.Finish(hurry, deadline)
}

// waitFirstTotals holds the listeners back — the gateway is not ready — until the first
// totals arrive, or until deadline, the end of the boot wait (docs/specs/GATEWAY.md,
// Control-plane mode → Readiness waits for the first totals). They follow the config
// on the stream's connect, so the wait is normally milliseconds. Without them the
// gateway still starts: priced USD-limited requests are refused budget_unavailable
// until they arrive (the limiter's no-totals state).
func waitFirstTotals(ctx context.Context, limiter *limits.Limiter, deadline time.Time, logger *slog.Logger) {
	started := time.Now()
	wait := max(time.Until(deadline), 0)
	logger.Info("waiting for the first totals", logattr.Seconds("kaiak.control.totals_wait", wait))
	timer := time.NewTimer(wait)
	defer timer.Stop()
	received := false
	select {
	case <-limiter.FirstTotals():
		received = true
	case <-timer.C:
		select { // totals that came with the deadline still count
		case <-limiter.FirstTotals():
			received = true
		default:
		}
	case <-ctx.Done():
		return
	}
	if received {
		logger.Info("first totals received", logattr.Seconds("kaiak.control.totals_waited", time.Since(started)))
		return
	}
	logger.Warn("first totals not received within the boot wait: priced USD-limited requests are refused until they arrive",
		logattr.Seconds("kaiak.control.totals_waited", time.Since(started)))
}

// ignoreReloads answers SIGHUP in control-plane mode, where there is no file to
// reload: the config comes from the control plane.
func ignoreReloads(ctx context.Context, reload <-chan os.Signal, logger *slog.Logger) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-reload:
			logger.Info("SIGHUP ignored: the config comes from the control plane")
		}
	}
}

// controlState is the control-plane connection for the metrics: the client's contact
// and the limiter's outage.
type controlState struct {
	client  *control.Client
	limiter *limits.Limiter
}

func (s controlState) Contact() (bool, time.Time) { return s.client.Contact() }
func (s controlState) Outage() bool               { return s.limiter.Outage() }
func (s controlState) ConfigRejected() bool {
	_, rejected := s.client.LastRejection()
	return rejected
}
func (s controlState) TotalsAppliedAt() (time.Time, bool) {
	return s.limiter.TotalsAppliedAt()
}

// servingStatus formats what routing serves (routing.Serving) for status reports: a
// backend's configured cap, not this gateway's share of it, and its deployments
// under it, each with its circuit (and the opening time, in UTC, of one open or
// half-open). Its collections are non-nil, as control.Serving requires.
func servingStatus(serving routing.Serving) control.Serving {
	out := control.Serving{Backends: make(map[string]control.BackendStatus, len(serving.Backends)),
		Models: make(map[string]control.ModelStatus, len(serving.Models))}
	for id, b := range serving.Backends {
		out.Backends[id] = control.BackendStatus{MaxInFlight: b.MaxInFlight, InFlight: int64(b.InFlight),
			Deployments: map[string]control.DeploymentStatus{}}
	}
	for name, m := range serving.Models {
		out.Models[name] = control.ModelStatus{Queued: int64(m.Queued)}
	}
	for id, d := range serving.Deployments {
		status := control.DeploymentStatus{Circuit: control.CircuitClosed}
		if d.Circuit != routing.CircuitClosed {
			at := d.OpenedAt.UTC()
			status = control.DeploymentStatus{Circuit: control.CircuitOpen, OpenedAt: &at}
			if d.Circuit == routing.CircuitHalfOpen {
				status.Circuit = control.CircuitHalfOpen
			}
		}
		out.Backends[id.Backend].Deployments[id.Model] = status
	}
	return out
}
