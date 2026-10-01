// Command kaiak is the gateway binary: it builds the dependency graph and runs it
// until SIGINT or SIGTERM, which drain it.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"kaiak/internal/accounting"
	"kaiak/internal/config"
	"kaiak/internal/control"
	"kaiak/internal/limits"
	"kaiak/internal/metrics"
	"kaiak/internal/provider"
	"kaiak/internal/routing"
	"kaiak/internal/server"
	"kaiak/internal/state"
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

	if err := run(context.Background(), logger, os.LookupEnv, reload, stop); err != nil {
		logger.Error("kaiak stopped with an error", "error", err)
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

// settings is the process configuration read from the environment
// (docs/specs/GATEWAY.md, Configuration sources).
type settings struct {
	// configFile is set in file mode; control in control-plane mode. Exactly one is.
	configFile string
	control    *controlSettings
	// dataDir is the data directory (KAIAK_DATA_DIR); "" writes nothing anywhere.
	dataDir    string
	instanceID string
	listenAddr string
	adminAddr  string
	drain      server.DrainTimes
	// drainReserve is the end of the drain timeout kept for the usage flush and the
	// final status (control-plane mode): in-flight requests are cut that much before
	// the timeout.
	drainReserve time.Duration
	// client bounds client progress on both listeners.
	client server.ClientTimeouts
	// bodyMemory is the body budget: the request body bytes held at once.
	bodyMemory int64
	// usageMemory bounds the encoded usage records held in memory waiting for
	// delivery (control-plane mode).
	usageMemory int64
	// maxConnections caps the API listener's open connections; 0 is no cap.
	maxConnections int64
	// metricsToken is the bearer token /metrics requires; "" leaves it open.
	metricsToken string
}

// controlSettings are control-plane mode's: where the control plane is, the token
// the gateway presents, and how long boot waits for the snapshot.
type controlSettings struct {
	url      *url.URL
	token    string
	bootWait time.Duration
	// seed is the seed config document (KAIAK_SEED_CONFIG_FILE, seedFile); nil when
	// none is set.
	seed     []byte
	seedFile string
}

// Default listen addresses: the API port and the admin port.
const (
	defaultListenAddr = ":8080"
	defaultAdminAddr  = ":9090"
)

// Default drain waits: the grace period covers load balancers taking the instance out
// of rotation; the timeout lets long streams finish. The flush reserve is the end of
// the timeout kept for the usage flush and the final status; left unset, it is at
// most half the timeout.
const (
	defaultDrainGrace   = 5 * time.Second
	defaultDrainTimeout = 60 * time.Second
	defaultDrainReserve = 10 * time.Second
)

func readSettings(lookupEnv func(string) (string, bool)) (settings, error) {
	s := settings{listenAddr: defaultListenAddr, adminAddr: defaultAdminAddr,
		drain: server.DrainTimes{Grace: defaultDrainGrace, Timeout: defaultDrainTimeout}, client: server.DefaultClientTimeouts,
		bodyMemory: server.DefaultBodyMemory, usageMemory: control.DefaultUsageMemoryBytes}
	var err error
	s.configFile, _ = lookupEnv("KAIAK_CONFIG_FILE")
	if s.control, err = readControlSettings(lookupEnv, s.configFile != ""); err != nil {
		return s, err
	}
	if s.configFile == "" && s.control == nil {
		return s, errors.New("no config source: set KAIAK_CONFIG_FILE (file mode), or KAIAK_CONTROL_URL and KAIAK_CONTROL_TOKEN (control-plane mode)")
	}
	if seed, _ := lookupEnv("KAIAK_SEED_CONFIG_FILE"); seed != "" && s.control == nil {
		return s, errors.New("KAIAK_SEED_CONFIG_FILE is for control-plane mode: in file mode KAIAK_CONFIG_FILE is the config")
	}
	s.dataDir, _ = lookupEnv("KAIAK_DATA_DIR")
	if addr, _ := lookupEnv("KAIAK_LISTEN_ADDR"); addr != "" {
		s.listenAddr = addr
	}
	if addr, _ := lookupEnv("KAIAK_ADMIN_ADDR"); addr != "" {
		s.adminAddr = addr
	}
	if s.drain.Grace, err = durationMS(lookupEnv, "KAIAK_DRAIN_GRACE_MS", s.drain.Grace); err != nil {
		return s, err
	}
	if s.drain.Timeout, err = durationMS(lookupEnv, "KAIAK_DRAIN_TIMEOUT_MS", s.drain.Timeout); err != nil {
		return s, err
	}
	if s.drainReserve, err = durationMS(lookupEnv, "KAIAK_DRAIN_FLUSH_RESERVE_MS", min(defaultDrainReserve, s.drain.Timeout/2)); err != nil {
		return s, err
	}
	if s.drainReserve > s.drain.Timeout {
		return s, fmt.Errorf("KAIAK_DRAIN_FLUSH_RESERVE_MS=%d is above the drain timeout (%d ms): the reserve is the end of the timeout",
			s.drainReserve.Milliseconds(), s.drain.Timeout.Milliseconds())
	}
	for _, t := range []struct {
		name string
		d    *time.Duration
	}{
		{"KAIAK_IDLE_TIMEOUT_MS", &s.client.Idle},
		{"KAIAK_BODY_READ_TIMEOUT_MS", &s.client.BodyRead},
		{"KAIAK_WRITE_TIMEOUT_MS", &s.client.Write},
	} {
		if *t.d, err = durationMS(lookupEnv, t.name, *t.d); err != nil {
			return s, err
		}
		// A client bound of 0 would be no bound at all.
		if *t.d == 0 {
			return s, fmt.Errorf("%s=0: want a whole number of milliseconds above 0", t.name)
		}
	}
	for _, n := range []struct {
		name  string
		v     *int64
		least int64
	}{
		{"KAIAK_BODY_MEMORY_BYTES", &s.bodyMemory, 1},
		{"KAIAK_USAGE_MEMORY_BYTES", &s.usageMemory, 1},
		{"KAIAK_MAX_CONNECTIONS", &s.maxConnections, 0},
	} {
		if *n.v, err = wholeNumber(lookupEnv, n.name, *n.v, n.least); err != nil {
			return s, err
		}
	}
	s.metricsToken, _ = lookupEnv("KAIAK_METRICS_TOKEN")
	s.instanceID, _ = lookupEnv("KAIAK_INSTANCE_ID")
	if s.instanceID == "" {
		host, err := os.Hostname()
		if err != nil {
			return s, fmt.Errorf("KAIAK_INSTANCE_ID is not set and the hostname is unknown: %w", err)
		}
		s.instanceID = host
	}
	// The instance ID travels in the Kaiak-Instance header and in every usage record;
	// a control plane refuses one of another shape.
	if s.control != nil && !control.IsInstanceID(s.instanceID) {
		return s, fmt.Errorf("instance ID %q (KAIAK_INSTANCE_ID, default the hostname) is not valid in control-plane mode: "+
			"want a letter or digit, then up to 252 letters, digits, '.', '_' or '-'", s.instanceID)
	}
	return s, nil
}

// defaultBootWait bounds the boot's snapshot fetches — retried while the control
// plane is unavailable — before the last-known-good or seed config is used; what is
// left of it bounds the wait for the first totals.
const defaultBootWait = control.DefaultBootWait

// readControlSettings reads control-plane mode's variables: nil when none is set. The
// URL and token come together, and never beside a config file (fileMode).
func readControlSettings(lookupEnv func(string) (string, bool), fileMode bool) (*controlSettings, error) {
	rawURL, _ := lookupEnv("KAIAK_CONTROL_URL")
	token, _ := lookupEnv("KAIAK_CONTROL_TOKEN")
	switch {
	case rawURL == "" && token == "":
		return nil, nil
	case fileMode && rawURL != "":
		return nil, errors.New("KAIAK_CONFIG_FILE and KAIAK_CONTROL_URL are both set: choose file mode or control-plane mode")
	case rawURL == "":
		return nil, errors.New("KAIAK_CONTROL_TOKEN is set without KAIAK_CONTROL_URL")
	case token == "":
		return nil, errors.New("KAIAK_CONTROL_URL is set without KAIAK_CONTROL_TOKEN")
	}
	seedFile, _ := lookupEnv("KAIAK_SEED_CONFIG_FILE")
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("KAIAK_CONTROL_URL: want an http:// or https:// base URL with a host and no query")
	}
	bootWait, err := durationMS(lookupEnv, "KAIAK_CONTROL_BOOT_WAIT_MS", defaultBootWait)
	if err != nil {
		return nil, err
	}
	if bootWait == 0 {
		return nil, errors.New("KAIAK_CONTROL_BOOT_WAIT_MS=0: want a whole number of milliseconds above 0")
	}
	cs := &controlSettings{url: u, token: token, bootWait: bootWait}
	if seedFile != "" {
		if cs.seed, err = readSeed(seedFile, lookupEnv); err != nil {
			return nil, err
		}
		cs.seedFile = seedFile
	}
	return cs, nil
}

// readSeed reads and checks the seed config: a seed that could not be applied must
// fail the start, not the outage it is kept for — so it is checked completely,
// backend credentials included. It may hold no priced model: it serves while the
// control plane, which keeps the budgets, is unavailable.
func readSeed(path string, lookupEnv func(string) (string, bool)) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("KAIAK_SEED_CONFIG_FILE: %w", err)
	}
	snapshot, err := config.Check(data, lookupEnv)
	if err != nil {
		return nil, fmt.Errorf("KAIAK_SEED_CONFIG_FILE %s: %w", path, err)
	}
	for _, name := range snapshot.ModelNames {
		if len(snapshot.Models[name].Prices) > 0 {
			return nil, fmt.Errorf("KAIAK_SEED_CONFIG_FILE %s: model %q is priced: the seed serves only free models, "+
				"since budgets need the control plane", path, name)
		}
	}
	return data, nil
}

// wholeNumber reads a whole number, least or more, from the environment variable
// name; unset or empty keeps def.
func wholeNumber(lookupEnv func(string) (string, bool), name string, def, least int64) (int64, error) {
	v, _ := lookupEnv(name)
	if v == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < least {
		return 0, fmt.Errorf("%s=%q: want a whole number, %d or more", name, v, least)
	}
	return n, nil
}

// durationMS reads a whole number of milliseconds (0 or more) from the environment
// variable name; unset or empty keeps def.
func durationMS(lookupEnv func(string) (string, bool), name string, def time.Duration) (time.Duration, error) {
	v, _ := lookupEnv(name)
	if v == "" {
		return def, nil
	}
	ms, err := strconv.ParseInt(v, 10, 64)
	if err != nil || ms < 0 || ms > math.MaxInt64/int64(time.Millisecond) {
		return 0, fmt.Errorf("%s=%q: want a whole number of milliseconds, 0 or more", name, v)
	}
	return time.Duration(ms) * time.Millisecond, nil
}

// run loads the config and serves the API and admin listeners. In file mode it
// reloads the config file on every value received from reload; in control-plane mode
// the control client boots the config — run returns its error when there is none —
// then follows the control plane, sends usage and reports status in the background
// until the drain is over. A value on stop, or a
// listener failing, starts the drain (server.Drain.Run); a further value on stop
// hurries it. Cancelling ctx stops without waiting: the drain runs hurried. Once the
// API has drained, run flushes usage to the control plane (control-plane mode) or
// writes the limits snapshot (file mode, with a data directory), stops the admin
// listener and returns.
func run(ctx context.Context, logger *slog.Logger, lookupEnv func(string) (string, bool),
	reload, stop <-chan os.Signal) error {
	startedAt := time.Now()
	s, err := readSettings(lookupEnv)
	if err != nil {
		return err
	}
	source := []any{"config_file", s.configFile}
	if s.control != nil {
		source = []any{"control_url", s.control.url.Redacted()}
	}
	logger.Info("kaiak starting", append([]any{"pid", os.Getpid(), "instance_id", s.instanceID,
		"data_dir", s.dataDir}, source...)...)

	// With no data directory nothing is written anywhere: usage batches wait in
	// memory, and there is no last-known-good copy, totals cache or limits snapshot.
	var dir *state.Dir
	if s.dataDir != "" {
		if dir, err = state.Open(s.dataDir, logger); err != nil {
			return err
		}
		// The directory's lock is held until run returns (and released by the OS if
		// the process dies first).
		defer func() {
			if err := dir.Close(); err != nil {
				logger.Warn("data directory lock not released", "error", err)
			}
		}()
	}

	// Background work stops when the drain is over, or when run returns early (a
	// listener that cannot bind): the control client runs before the listeners bind.
	bgCtx, stopBackground := context.WithCancel(context.Background())
	var background sync.WaitGroup
	defer func() {
		stopBackground()
		background.Wait()
	}()

	holder := &config.Holder{}
	providers := provider.NewRegistry(lookupEnv)
	registry := metrics.NewRegistry()
	circuits := metrics.NewCircuits(registry)
	router := routing.New(routing.Options{Probe: providers.Probe, Observer: circuits, Logger: logger})
	ops := metrics.NewOps(registry, router, holder)
	modelChecker := routing.NewModelChecker(providers.Probe, logger)
	// Every applied config sets the backend caps routing enforces and the backends
	// whose connection pools are kept, and has its deployments' models checked in
	// the background.
	applier := config.NewApplier(holder, logger, lookupEnv, func(trigger string, applied bool, at time.Time) {
		if applied {
			router.Configure(holder.Current())
			circuits.PrepareSeries(holder.Current())
			providers.Retain(holder.Current().Backends)
			modelChecker.Check(holder.Current())
			if bodyCap := holder.Current().MaxRequestBodyBytes; bodyCap > s.bodyMemory {
				logger.Warn("max_request_body_bytes exceeds the body budget: bodies above the budget are refused as too large",
					"max_request_body_bytes", bodyCap, "body_memory_bytes", s.bodyMemory)
			}
		}
		ops.ConfigLoaded(trigger, applied, at)
	})
	var loader *config.FileLoader
	var client *control.Client
	var limiter *limits.Limiter
	if s.control == nil {
		loader = config.NewFileLoader(s.configFile, applier)
		if err := loader.Load("startup"); err != nil {
			return err
		}
		limiter = limits.New(holder, time.Now, logger)
		restoreLimits(limiter, dir, logger)
	} else {
		// The limiter reads the client's contact (the outage) and the client feeds
		// the limiter totals and usage generations; client is set before any request
		// or scrape can read it.
		limiter = limits.NewShared(holder, time.Now, func() limits.Contact { return controlContact(client) }, logger)
		client = control.New(control.Options{URL: s.control.url, Token: s.control.token, Instance: s.instanceID,
			Applier: applier, Dir: dir, Logger: logger, BootWait: s.control.bootWait, StartedAt: startedAt,
			SeedConfig: s.control.seed, SeedFile: s.control.seedFile,
			Serving: func() control.Serving {
				return servingStatus(router.InFlightByBackend(), router.QueuedByModel(), router.Circuits(), holder.Current())
			},
			Observer: metrics.NewUsageDelivery(registry), UsageMemoryBytes: s.usageMemory,
			OnTotals: func(u control.TotalsUpdate) {
				applied := limiter.TakeTotals(limitsTotals(u.Totals), u.Counted)
				// Backend caps are split among the live gateways the totals count.
				router.SetLiveGateways(limiter.LiveGateways())
				if applied {
					saveShared(limiter, dir, logger, "totals")
				}
			}})
		metrics.RegisterControlState(registry, controlState{client, limiter})
		// A model's queue starting or ending and a circuit opening or closing are
		// told within the status minimum gap; depth changes in between wait for the
		// regular report.
		router.OnServingChange(client.ServingChanged)
		// No config from the control plane, the last-known-good copy or the seed:
		// the process exits, and its supervisor restarts it with backoff.
		bootDeadline := time.Now().Add(s.control.bootWait)
		// A stop signal during the boot wait ends it: nothing is served and no usage
		// exists yet, so run returns at once instead of binding the listeners.
		bootCtx, endBoot := stopOnSignal(ctx, stop)
		err := client.Boot(bootCtx)
		if err == nil {
			restoreShared(limiter, dir, logger, client.RestoredGeneration())
			router.SetLiveGateways(limiter.LiveGateways())
			// The client follows the control plane until the drain is over, so
			// requests admitted during the grace period run on the newest config. It
			// starts before the listeners: the first totals come on its stream.
			background.Go(func() { client.Run(bgCtx) })
			// A seed boot (no control-plane version) serves only free models: no
			// budget waits for totals.
			if _, fromControlPlane := client.AppliedVersion(); fromControlPlane {
				waitFirstTotals(bootCtx, limiter, bootDeadline, logger)
			}
		}
		if sig := endBoot(); sig != nil {
			logger.Info("kaiak stopped", "reason", "signal "+sig.String()+" during boot")
			return nil
		}
		if err != nil {
			return err
		}
	}

	// Usage records go to the control client's batch sender (control-plane mode), which
	// tags each with its batch's generation, then to the usage metrics. Local limits
	// settle from the request's own records in a request finisher, so they are not a
	// sink.
	usageMetrics := metrics.NewUsageSink(registry, holder)
	recorderOpts := accounting.RecorderOptions{Instance: s.instanceID, Sink: usageMetrics, Logger: logger,
		OutOfRange: usageMetrics.RecordClamped}
	if client != nil {
		recorderOpts.Batcher = client
	}
	recorder := accounting.NewRecorder(recorderOpts)
	drain := server.NewDrain()
	apiHandler := server.NewAPI(holder, drain, server.NewBodyBudget(s.bodyMemory), providers, limiter, router, recorder,
		ops, logger)
	api, err := server.Listen("api", s.listenAddr, apiHandler, s.client, logger)
	if err != nil {
		return err
	}
	// The cap counts connections before any request on them is authenticated; the
	// admin listener stays uncapped, so probes and scrapes pass a flood.
	api.LimitConnections(s.maxConnections, ops.ConnectionRefused)
	admin, err := server.Listen("admin", s.adminAddr, server.NewAdmin(holder, drain, registry, s.metricsToken), s.client, logger)
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
	background.Go(func() { router.RunProbers(bgCtx) })
	background.Go(func() { modelChecker.Run(bgCtx) })
	if loader != nil {
		background.Go(func() { reloadOnSignal(bgCtx, reload, loader) })
		if dir != nil {
			background.Go(func() { saveLimitsPeriodically(bgCtx, limiter, dir, logger) })
		}
	} else {
		background.Go(func() { ignoreReloads(bgCtx, reload, logger) })
	}

	var cause error
	var reason string
	select {
	case sig := <-stop:
		reason = "signal " + sig.String()
	case <-ctx.Done():
		reason = "context cancelled"
	case cause = <-failed:
		reason = "listener failed"
	}
	logger.Info("kaiak stopping", "reason", reason)

	// The drain's own waits (grace, then timeout) bound the usage flush too. In
	// control-plane mode in-flight requests are cut the flush reserve before the
	// timeout, so the records they settle still have time to reach the control plane.
	drainDeadline := time.Now().Add(s.drain.Grace + s.drain.Timeout)
	drainTimes := s.drain
	if client != nil {
		drainTimes.Reserve = s.drainReserve
		client.SetDraining()
	}
	// hurry is closed by a second stop signal, or at once when ctx is cancelled.
	hurry := make(chan struct{})
	drained := make(chan struct{})
	if ctx.Err() != nil {
		close(hurry)
	} else {
		background.Go(func() {
			select {
			case sig := <-stop:
				logger.Warn("second stop signal: skipping the remaining drain", "signal", sig.String())
			case <-ctx.Done():
			case <-drained:
				return
			}
			close(hurry)
		})
	}
	drain.Run(api, drainTimes, hurry, logger)
	if client != nil {
		finishWithControlPlane(client, drainDeadline, hurry)
	}
	close(drained)
	stopBackground()
	background.Wait()
	switch {
	case dir == nil:
	case loader != nil:
		saveLimits(limiter, dir, logger, "shutdown")
	default:
		saveShared(limiter, dir, logger, "shutdown")
	}
	admin.Shutdown(adminShutdownTimeout)
	listeners.Wait()
	close(failed)
	if cause == nil {
		cause = <-failed
	}
	if cause != nil {
		return cause
	}
	logger.Info("kaiak stopped", "reason", reason)
	return nil
}

// stopOnSignal returns a context that ends with ctx or at the first value on stop,
// and the function that ends the watch: it returns the signal taken from stop, nil
// when none was — stop is then left for the caller's later reads.
func stopOnSignal(ctx context.Context, stop <-chan os.Signal) (context.Context, func() os.Signal) {
	watched, cancel := context.WithCancel(ctx)
	var sig os.Signal
	done := make(chan struct{})
	go func() {
		defer close(done)
		select {
		case sig = <-stop:
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

// waitFirstTotals holds the listeners back — the gateway is not ready — until the first
// totals arrive, or until deadline, the end of the boot wait (docs/specs/GATEWAY.md,
// Control-plane mode → Readiness waits for the first totals). They follow the config
// on the stream's connect, so the wait is normally milliseconds. Without them the
// gateway still starts: priced USD-limited requests are refused budget_unavailable
// until they arrive (the limiter's no-totals state).
func waitFirstTotals(ctx context.Context, limiter *limits.Limiter, deadline time.Time, logger *slog.Logger) {
	started := time.Now()
	wait := max(time.Until(deadline), 0)
	logger.Info("waiting for the first totals", "wait_ms", wait.Milliseconds())
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
		logger.Info("first totals received", "waited_ms", time.Since(started).Milliseconds())
		return
	}
	logger.Warn("first totals not received within the boot wait: priced USD-limited requests are refused until they arrive",
		"waited_ms", time.Since(started).Milliseconds())
}

// finalStatusTimeout bounds the last status report of a drain.
const finalStatusTimeout = 2 * time.Second

// finishWithControlPlane is the control-plane part of the drain's last step
// (docs/specs/GATEWAY.md, Lifecycle), run once the drained requests' records are
// settled and before the client stops: the usage flush — the filling batch sealed,
// queued batches sent until acknowledged, the drain's deadline or a hurry — then a
// final draining status. What is not delivered stays in the spool for the next start
// (with a data directory) or is lost with the process.
func finishWithControlPlane(client *control.Client, deadline time.Time, hurry <-chan struct{}) {
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	var watch sync.WaitGroup
	watch.Go(func() {
		select {
		case <-hurry:
			cancel()
		case <-ctx.Done():
		}
	})
	client.FlushUsage(ctx, "drain")
	hurried := false
	select {
	case <-hurry:
		hurried = true
	default:
	}
	cancel()
	watch.Wait()
	if hurried {
		return
	}
	statusCtx, cancelStatus := context.WithTimeout(context.Background(), finalStatusTimeout)
	defer cancelStatus()
	_ = client.ReportStatus(statusCtx, "drain") // the result is logged; the usage flush is over either way
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
			_ = loader.Load("sighup")
		}
	}
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

// controlContact is the control client's contact as the limiter decides the outage
// on it.
func controlContact(client *control.Client) limits.Contact {
	connected, last := client.Contact()
	return limits.Contact{Connected: connected, Last: last, UsageWaitingSince: client.UsageWaitingSince()}
}

// controlState is the control-plane connection for the metrics: the client's contact
// and the limiter's outage and config mismatch.
type controlState struct {
	client  *control.Client
	limiter *limits.Limiter
}

func (s controlState) Contact() (bool, time.Time) { return s.client.Contact() }
func (s controlState) Outage() bool               { return s.limiter.Outage() }
func (s controlState) ConfigMismatch() bool       { return s.limiter.ConfigMismatch() }
func (s controlState) TotalsAppliedAt() (time.Time, bool) {
	return s.limiter.TotalsAppliedAt()
}

// servingStatus is the routing state for status reports: every backend, model and
// deployment of the applied config s (none while s is nil), every backend inFlight
// counts requests on and every model queued counts waiting requests for (a reload may
// have dropped them). A deployment's circuit is open or half-open, with its opening
// time, as circuits has it; closed when circuits does not.
func servingStatus(inFlight, queued map[string]int, circuits map[routing.DeploymentID]routing.CircuitReport, s *config.Snapshot) control.Serving {
	out := control.Serving{Backends: map[string]control.BackendStatus{}, Models: map[string]control.ModelStatus{}}
	if s != nil {
		for id, b := range s.Backends {
			out.Backends[id] = control.BackendStatus{MaxInFlight: b.MaxInFlight,
				Deployments: map[string]control.DeploymentStatus{}}
		}
		for name, m := range s.Models {
			out.Models[name] = control.ModelStatus{}
			for _, d := range m.Deployments {
				status := control.DeploymentStatus{Circuit: control.CircuitClosed}
				if c, ok := circuits[routing.DeploymentID{Backend: d.Backend.ID, Model: d.Model}]; ok {
					at := c.OpenedAt.UTC()
					status = control.DeploymentStatus{Circuit: control.CircuitOpen, OpenedAt: &at}
					if c.State == routing.CircuitHalfOpen {
						status.Circuit = control.CircuitHalfOpen
					}
				}
				out.Backends[d.Backend.ID].Deployments[d.Model] = status
			}
		}
	}
	for id, n := range inFlight {
		b, ok := out.Backends[id]
		if !ok {
			b.Deployments = map[string]control.DeploymentStatus{}
		}
		b.InFlight = int64(n)
		out.Backends[id] = b
	}
	for name, n := range queued {
		out.Models[name] = control.ModelStatus{Queued: int64(n)}
	}
	return out
}

// limitsTotals converts the control plane's totals for the limiter; nil stays nil
// (totals older than the ones applied).
func limitsTotals(t *control.Totals) *limits.Totals {
	if t == nil {
		return nil
	}
	out := &limits.Totals{Config: config.Version{Epoch: t.ConfigEpoch, Number: t.ConfigVersion},
		LiveGateways: t.LiveGateways, Windows: make([]limits.PushedWindow, len(t.Windows))}
	for i, w := range t.Windows {
		out.Windows[i] = limits.PushedWindow{Group: w.Group, Type: w.Type, Models: w.Models,
			Start: w.WindowStart, Used: w.Used}
	}
	return out
}

// restoreLimits loads the file-mode usage snapshot before traffic starts, when there
// is a data directory. The
// snapshot is a cache: one that cannot be read is logged and the gateway starts with
// empty windows.
func restoreLimits(limiter *limits.Limiter, dir *state.Dir, logger *slog.Logger) {
	if dir == nil {
		return
	}
	restored, dropped, err := limiter.LoadSnapshot(dir)
	if err != nil {
		logger.Warn("limits snapshot not restored", "file", limits.SnapshotFile, "error", err)
		return
	}
	logger.Info("limits snapshot restored", "file", limits.SnapshotFile, "windows", restored, "dropped", dropped)
}

// saveLimits writes the file-mode usage snapshot; trigger names what asked for it
// (interval, shutdown). The routine interval write logs at debug level.
func saveLimits(limiter *limits.Limiter, dir *state.Dir, logger *slog.Logger, trigger string) {
	n, err := limiter.SaveSnapshot(dir)
	if err != nil {
		logger.Error("limits snapshot not written", "trigger", trigger, "file", limits.SnapshotFile, "error", err)
		return
	}
	level := slog.LevelInfo
	if trigger == "interval" {
		level = slog.LevelDebug
	}
	logger.Log(context.Background(), level, "limits snapshot written", "trigger", trigger,
		"file", limits.SnapshotFile, "windows", n)
}

// restoreShared loads the control-plane-mode limits state before traffic starts, when
// there is a data directory
// (limits.LoadShared). It is a cache: one that cannot be read, or belongs to another
// config than the one booted, is logged and the gateway counts from the next totals.
func restoreShared(limiter *limits.Limiter, dir *state.Dir, logger *slog.Logger, restoredGeneration uint64) {
	if dir == nil {
		return
	}
	r, err := limiter.LoadShared(dir, restoredGeneration)
	switch {
	case err != nil:
		logger.Warn("limits totals not restored", "file", limits.SharedFile, "error", err)
	case !r.Found:
	case r.Discarded != "":
		logger.Info("limits totals discarded", "file", limits.SharedFile, "reason", r.Discarded)
	default:
		logger.Info("limits totals restored", "file", limits.SharedFile, "windows", r.Restored, "dropped", r.Dropped)
	}
}

// saveShared writes the control-plane-mode limits state, when there is a data
// directory; trigger names what asked for it (totals, shutdown). The routine write on applied totals logs at debug level.
func saveShared(limiter *limits.Limiter, dir *state.Dir, logger *slog.Logger, trigger string) {
	if dir == nil {
		return
	}
	n, err := limiter.SaveShared(dir)
	if err != nil {
		logger.Error("limits totals not written", "trigger", trigger, "file", limits.SharedFile, "error", err)
		return
	}
	level := slog.LevelInfo
	if trigger == "totals" {
		level = slog.LevelDebug
	}
	logger.Log(context.Background(), level, "limits totals written", "trigger", trigger,
		"file", limits.SharedFile, "windows", n)
}

// saveLimitsPeriodically is the interval trigger for the usage snapshot, stopped by
// ctx; the shutdown write is run's, once the drain is over.
func saveLimitsPeriodically(ctx context.Context, limiter *limits.Limiter, dir *state.Dir, logger *slog.Logger) {
	ticker := time.NewTicker(limits.SnapshotInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			saveLimits(limiter, dir, logger, "interval")
		}
	}
}
