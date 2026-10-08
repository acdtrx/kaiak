// Command kaiak is the gateway binary: it builds the dependency graph and runs it
// until SIGINT or SIGTERM, which drain it.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"runtime/debug"
	"sync"
	"syscall"
	"time"

	"kaiak/internal/accounting"
	"kaiak/internal/config"
	"kaiak/internal/limits"
	"kaiak/internal/metrics"
	"kaiak/internal/provider"
	"kaiak/internal/routing"
	"kaiak/internal/server"
	"kaiak/internal/telemetry/metric"
	"kaiak/internal/telemetry/otlp"
	"kaiak/internal/telemetry/otlplog"
	"kaiak/internal/telemetry/otlpmetric"
)

func main() {
	logger, err := newLogger(os.Getenv("KAIAK_LOG_FORMAT"), os.Stderr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "kaiak:", err)
		os.Exit(1)
	}

	// SIGINT drains like SIGTERM: Ctrl-C and docker stop users expect the same clean
	// stop. Registered for the process's whole life, so a second signal reaches run
	// (it hurries the drain) instead of killing the process.
	stop := make(chan os.Signal, 2)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	reload := make(chan os.Signal, 1)
	signal.Notify(reload, syscall.SIGHUP)

	// run logs the error itself, as its last line: with OTLP log export on, the line
	// must go out before the export's final flush.
	if err := run(context.Background(), logger, os.LookupEnv, reload, stop); err != nil {
		os.Exit(1)
	}
}

// newLogger builds the process logger: JSON lines (production, the default) or
// human-readable text (development).
func newLogger(format string, w io.Writer) (*slog.Logger, error) {
	switch format {
	case "", "json":
		return slog.New(slog.NewJSONHandler(w, nil)), nil
	case "text":
		return slog.New(slog.NewTextHandler(w, nil)), nil
	}
	return nil, fmt.Errorf("KAIAK_LOG_FORMAT=%q: want json or text", format)
}

// run loads the config and serves the API and admin listeners. In file mode it
// reloads the config file on every value received from reload; in control-plane mode
// the control plane's half boots the config (controlPlane.boot) — run returns its
// error when there is none — then follows the control plane, sends usage and reports
// status in the background until the drain is over. A value on stop, or a listener
// failing, starts the drain (server.Drain.Run); a further value on stop hurries it,
// the final metric export and log flush included. Cancelling ctx stops without
// waiting: the drain runs hurried. Once the API has drained, run flushes usage to the
// control plane (control-plane mode), stops the admin listener and returns. With OTLP
// metric export configured, the metrics are pushed from the config load on, and the
// final export runs before run's last line, `kaiak stopped`, or `kaiak stopped with
// an error` with the error it returns. With OTLP log export configured, every line
// from `kaiak starting` on also goes to the collector, and the export's final flush
// is run's last act.
func run(ctx context.Context, logger *slog.Logger, lookupEnv func(string) (string, bool),
	reload, stop <-chan os.Signal) (err error) {
	startedAt := time.Now()
	// The exporters, and the end of their work at exit: the drain's deadline once a
	// drain started (exitBound). hurryCtx never ends before the drain starts; from then
	// on it ends with a second stop signal or with ctx, and its watch runs through the
	// final metric export and the final log flush, which it cuts to their floor:
	// endHurry ends the watch after the flush. reason is what `kaiak stopped` names.
	var logExport *otlplog.Exporter
	var metricExport *otlpmetric.Exporter
	var exitBy time.Time
	var reason string
	hurryCtx, endHurry := context.Background(), func() os.Signal { return nil }
	defer func() {
		if metricExport != nil {
			finishExport(hurryCtx, metricExport, exitBy)
		}
		if err != nil {
			logger.Error("kaiak stopped with an error", "exception.message", err)
		} else {
			logger.Info("kaiak stopped", "kaiak.reason", reason)
		}
		if logExport != nil {
			finishExport(hurryCtx, logExport, exitBy)
		}
		endHurry()
	}()
	s, err := readSettings(lookupEnv)
	if err != nil {
		return err
	}
	source := s.configSource()
	reported := reportedVersion()
	service := otlp.Service{Version: reported, InstanceID: s.instanceID}
	if s.logExport != nil {
		// Export problems go to the stderr logger alone: one exported would feed the
		// problem it reports.
		logExport = otlplog.New(s.logExport, service, logger)
		logger = slog.New(logExport.Handler(logger.Handler()))
		source = append(source, "kaiak.log_export.endpoint", s.logExport.EndpointHost())
	}
	if s.metricExport != nil {
		source = append(source, "kaiak.metric_export.endpoint", s.metricExport.EndpointHost())
	}
	logger.Info("kaiak starting", append([]any{"process.pid", os.Getpid(), "service.instance.id", s.instanceID},
		source...)...)

	// Background work stops when the drain is over, or when run returns early (a
	// listener that cannot bind): the control client runs before the listeners bind.
	bgCtx, stopBackground := context.WithCancel(context.Background())
	var background sync.WaitGroup
	goBackground := func(work func(context.Context)) { background.Go(func() { work(bgCtx) }) }
	defer func() {
		stopBackground()
		background.Wait()
	}()

	g := newGraph(s, reported, lookupEnv, logger, logExport)
	var cp *controlPlane
	if s.control != nil {
		opts := *s.control
		opts.Instance, opts.Applier, opts.Logger, opts.StartedAt = s.instanceID, g.applier, logger, startedAt
		opts.UsageMemoryBytes = s.usageMemory
		cp = newControlPlane(controlPlaneDeps{opts: opts, holder: g.holder, router: g.router, registry: g.registry,
			ops: g.ops})
	}
	if s.metricExport != nil {
		// Every family but the exporter's own count exists by now, and that one is
		// registered next, an interval before the first collect: the first collect
		// sees them all. Export problems are ordinary lines: the log export, when on,
		// carries them.
		metricExport = otlpmetric.New(s.metricExport, service, g.registry, logger)
		metrics.RegisterMetricExport(g.registry, metricExport.Counts)
	}
	// The mode's setup: the first config, the limiter, where usage records go
	// (control-plane mode: the client's batch sender, which tags each with its batch's
	// generation) and what SIGHUP does.
	var limiter *limits.Limiter
	var batcher accounting.Batcher
	var handleReloads func(context.Context)
	if cp == nil {
		loader := config.NewFileLoader(s.configFile, g.applier)
		if err := loader.Load(config.TriggerStartup); err != nil {
			return err
		}
		limiter = limits.New(g.holder, time.Now, logger)
		limiter.ObserveSyncs(g.ops.ObserveLimitsSync)
		handleReloads = func(ctx context.Context) { reloadOnSignal(ctx, reload, loader) }
	} else {
		err = cp.boot(ctx, stop, goBackground)
		if stopped, ok := errors.AsType[bootStopped](err); ok {
			reason = stopped.Error()
			return nil
		}
		if err != nil {
			return err
		}
		limiter, batcher = cp.limiter, cp.client
		handleReloads = func(ctx context.Context) { ignoreReloads(ctx, reload, logger) }
	}

	// Usage records go to the batcher, then to the usage metrics. Local limits settle
	// from the request's own records in a request finisher.
	recorder := accounting.NewRecorder(accounting.RecorderOptions{Instance: s.instanceID, Batcher: batcher,
		Metrics: g.usage, Logger: logger})
	drain := server.NewDrain()
	apiHandler := server.NewAPI(g.holder, drain, server.NewBodyBudget(s.bodyMemory), g.providers, limiter, g.router,
		g.missingEndpoints, recorder, g.ops, logger)
	api, err := server.Listen("api", s.listenAddr, apiHandler, s.client, logger)
	if err != nil {
		return err
	}
	// The cap counts connections before any request on them is authenticated; the
	// admin listener stays uncapped, so probes and scrapes pass a flood.
	api.LimitConnections(s.maxConnections, g.ops.ConnectionRefused)
	admin, err := server.Listen("admin", s.adminAddr, server.NewAdmin(g.holder, drain, g.registry, s.metricsToken), s.client, logger)
	if err != nil {
		_ = api.Close() // never served; the bind error is what matters
		return err
	}

	failed := make(chan error, 2)
	var listeners sync.WaitGroup
	for _, l := range []*server.Listener{api, admin} {
		listeners.Go(func() {
			if err := l.Serve(); err != nil {
				failed <- err
			}
		})
	}
	// Probers run through the drain: a circuit a probe half-opens still serves the
	// requests waiting in its model's queue.
	goBackground(g.router.RunProbers)
	goBackground(g.modelChecker.Run)
	goBackground(handleReloads)

	var cause error
	select {
	case sig := <-stop:
		reason = "signal " + sig.String()
	case <-ctx.Done():
		reason = "context cancelled"
	case cause = <-failed:
		reason = "listener failed"
	}
	logger.Info("kaiak stopping", "kaiak.reason", reason)

	// The drain's own waits (grace, then timeout) bound the usage flush too. In
	// control-plane mode in-flight requests are cut the flush reserve before the
	// timeout, so the records they settle still have time to reach the control plane.
	drainDeadline := time.Now().Add(s.drain.Grace + s.drain.Timeout)
	exitBy = drainDeadline
	drainTimes := s.drain
	if cp != nil {
		drainTimes.Reserve = s.drainReserve
		cp.beforeDrain()
	}
	// Hurried, the drain skips what is left of its waits, and the usage flush ends.
	hurryCtx, endHurry = stopOnSignal(ctx, stop, func(sig os.Signal) {
		logger.Warn("second stop signal: skipping the remaining drain", "kaiak.signal", sig.String())
	})
	drain.Run(api, drainTimes, hurryCtx.Done(), logger)
	if cp != nil {
		cp.finish(hurryCtx, drainDeadline)
	}
	stopBackground()
	background.Wait()
	admin.Shutdown(adminShutdownTimeout)
	listeners.Wait()
	close(failed)
	if cause == nil {
		cause = <-failed
	}
	return cause
}

// graph is the part of the dependency graph both modes share: what serves requests
// and what an applied config reaches, built before the first config is loaded.
type graph struct {
	holder           *config.Holder
	providers        *provider.Registry
	registry         *metric.Registry
	router           *routing.Router
	ops              *metrics.Ops
	usage            *metrics.UsageMetrics
	modelChecker     *provider.ModelChecker
	missingEndpoints *server.MissingEndpoints
	applier          *config.Applier
}

// newGraph builds the shared graph; reported is the build version the metrics report
// (reportedVersion), and logExport, when not nil, has its counts in the metrics.
func newGraph(s settings, reported string, lookupEnv func(string) (string, bool), logger *slog.Logger,
	logExport *otlplog.Exporter) *graph {
	g := &graph{holder: &config.Holder{}, providers: provider.NewRegistry(lookupEnv), registry: metric.NewRegistry(),
		missingEndpoints: server.NewMissingEndpoints()}
	metrics.RegisterBuildInfo(g.registry, reported)
	if logExport != nil {
		metrics.RegisterLogExport(g.registry, logExport.Counts)
	}
	circuits := metrics.NewCircuits(g.registry)
	g.router = routing.New(routing.Options{Probe: g.providers.Probe, Observer: circuits, Logger: logger})
	g.ops = metrics.NewOps(g.registry, g.router, circuits, g.holder)
	g.usage = metrics.NewUsageMetrics(g.registry, g.holder)
	g.modelChecker = provider.NewModelChecker(g.providers, logger)
	// Every applied config sets the backend caps routing enforces, the backends whose
	// connection pools are kept and the deployments whose missing endpoints are kept,
	// and has its deployments' models checked in the background.
	g.applier = config.NewApplier(g.holder, logger, lookupEnv, func(load config.Load) {
		if applied := load.Snapshot; applied != nil {
			g.router.Configure(applied)
			g.providers.Retain(applied.Backends)
			g.missingEndpoints.Retain(applied.Models)
			g.modelChecker.Check(applied)
			if bodyCap := applied.MaxRequestBodyBytes; bodyCap > s.bodyMemory {
				logger.Warn("max_request_body_bytes exceeds the body budget: bodies above the budget are refused as too large",
					"kaiak.config.max_request_body_size", bodyCap, "kaiak.body_budget.size", s.bodyMemory)
			}
		}
		g.ops.ConfigLoaded(load)
	})
	return g
}

// stopOnSignal returns a context that ends with ctx or at the first value on stop —
// onSignal, when not nil, is called with that value before the context ends — and the
// function that ends the watch: it returns the signal taken from stop, nil when none
// was — stop is then left for the caller's later reads.
func stopOnSignal(ctx context.Context, stop <-chan os.Signal, onSignal func(os.Signal)) (context.Context, func() os.Signal) {
	watched, cancel := context.WithCancel(ctx)
	var sig os.Signal
	done := make(chan struct{})
	go func() {
		defer close(done)
		select {
		case sig = <-stop:
			if onSignal != nil {
				onSignal(sig)
			}
			cancel()
		case <-watched.Done():
		}
	}()
	return watched, func() os.Signal {
		cancel()
		<-done
		return sig
	}
}

// exitExportFloor is the least time the final metric export and the final log flush
// each get: the last metrics and lines still go when the usage flush took the whole
// drain, after a second stop signal, or when the start failed.
const exitExportFloor = time.Second

// exporter is an OTLP exporter as run finishes it at exit.
type exporter interface {
	ForceFlush(ctx context.Context) error
	Shutdown(ctx context.Context)
}

// finishExport is an exporter's last work (docs/specs/GATEWAY.md, Observability →
// OTLP metric export and OTLP log export: at exit): the final export, or the final
// flush of what is queued, within exitBound, then Shutdown, which counts and reports
// what did not go.
func finishExport(hurry context.Context, e exporter, by time.Time) {
	ctx, cancel := exitBound(hurry, by)
	_ = e.ForceFlush(ctx) // what did not go is Shutdown's to count and report
	cancel()
	e.Shutdown(context.Background())
}

// exitBound is the context an export at exit runs within: it ends at by, or
// exitExportFloor from now when that is later. Once hurry ends — a second stop
// signal, before the export or during it — the floor alone bounds it: an export
// already past it ends at once. The cancel function ends it and the watch on hurry.
func exitBound(hurry context.Context, by time.Time) (context.Context, context.CancelFunc) {
	floor, endFloor := context.WithTimeout(context.Background(), exitExportFloor)
	until, endUntil := context.WithDeadline(hurry, by)
	ctx, cancel := context.WithCancel(context.Background())
	stopWatch := context.AfterFunc(floor, func() {
		<-until.Done()
		cancel()
	})
	return ctx, func() {
		endUntil() // a watch already running returns at once
		stopWatch()
		endFloor()
		cancel()
	}
}

// adminShutdownTimeout bounds how long a probe or scrape still open at the end may
// run before its connection is closed.
const adminShutdownTimeout = 5 * time.Second

// reloadOnSignal is the SIGHUP trigger for the file loader. A failed reload is logged
// and counted by the apply path and changes nothing, so its error needs no further
// handling here.
func reloadOnSignal(ctx context.Context, reload <-chan os.Signal, loader *config.FileLoader) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-reload:
			_ = loader.Load(config.TriggerSIGHUP)
		}
	}
}

// version is the release the binary was built as, set only at link time by the image
// build (-ldflags "-X main.version=<git describe>"). It is a build constant: nothing
// assigns it at run time.
var version string

// reportedVersion is the build version the gateway reports: kaiak.build.info, and the
// OTLP resource's service.version and the export's User-Agent.
func reportedVersion() string {
	info, ok := debug.ReadBuildInfo()
	return buildVersion(version, info, ok)
}

// buildVersion picks the version to report: the one stamped at link time, else the
// module version Go recorded (a VCS pseudo-version for `go build` in a checkout),
// else "(devel)" (`go run`, or a build with no version control).
func buildVersion(stamped string, info *debug.BuildInfo, ok bool) string {
	if stamped != "" {
		return stamped
	}
	if ok && info.Main.Version != "" {
		return info.Main.Version
	}
	return "(devel)"
}
