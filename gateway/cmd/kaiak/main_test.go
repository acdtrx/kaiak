package main

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"kaiak/internal/config"
	"kaiak/internal/fakecontrol"
	"kaiak/internal/fixturetest"
	"kaiak/internal/metrics"
	"kaiak/internal/routing"
	"kaiak/internal/telemetry/metric"
)

var minimalFixture = fixturetest.Dir("config", "valid", "minimal.json")

// envOf returns a lookupEnv over a fixed map.
func envOf(vars map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		v, ok := vars[name]
		return v, ok
	}
}

// syncBuffer lets the test read logs written by run's goroutines.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestRunLoadsConfigAndReturnsWhenContextIsCancelled(t *testing.T) {
	var logs syncBuffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	env := envOf(map[string]string{
		"KAIAK_CONFIG_FILE": minimalFixture,
		"KAIAK_INSTANCE_ID": "test-1",
		"KAIAK_LISTEN_ADDR": "127.0.0.1:0",
		"KAIAK_ADMIN_ADDR":  "127.0.0.1:0",
	})
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- run(ctx, logger, env, make(chan os.Signal), make(chan os.Signal)) }()
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run returned %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return after the context was cancelled")
	}

	out := logs.String()
	for _, want := range []string{"kaiak starting", "service.instance.id=test-1", "config applied", "kaiak.trigger=startup",
		"kaiak.listener.name=api", "kaiak.listener.name=admin", "kaiak stopped"} {
		if !strings.Contains(out, want) {
			t.Errorf("log misses %q:\n%s", want, out)
		}
	}
}

// ourGoroutines lists the stacks of goroutines running this module's code, other
// than test goroutines.
func ourGoroutines() []string {
	buf := make([]byte, 1<<20)
	buf = buf[:runtime.Stack(buf, true)]
	var ours []string
	for g := range strings.SplitSeq(string(buf), "\n\n") {
		if strings.Contains(g, "kaiak/") && !strings.Contains(g, "testing.tRunner") {
			ours = append(ours, g)
		}
	}
	return ours
}

// stopWhenServing sends SIGTERM on stop once run's API listener is bound — in
// control-plane mode a signal before then ends the boot instead
// (TestStopSignalDuringTheBootWaitExits). The returned function waits for the
// sender to be done.
func stopWhenServing(logs *syncBuffer, stop chan<- os.Signal) (wait func()) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		for !strings.Contains(logs.String(), "kaiak.listener.name=api") {
			time.Sleep(5 * time.Millisecond)
		}
		stop <- syscall.SIGTERM
	}()
	return func() { <-done }
}

// drainEnv is a test environment with the given grace period.
func drainEnv(t *testing.T, graceMS string) func(string) (string, bool) {
	t.Helper()
	return envOf(map[string]string{
		"KAIAK_CONFIG_FILE":      minimalFixture,
		"KAIAK_INSTANCE_ID":      "test-1",
		"KAIAK_LISTEN_ADDR":      "127.0.0.1:0",
		"KAIAK_ADMIN_ADDR":       "127.0.0.1:0",
		"KAIAK_DRAIN_GRACE_MS":   graceMS,
		"KAIAK_DRAIN_TIMEOUT_MS": "3600000",
	})
}

func TestStopSignalDrains(t *testing.T) {
	var logs syncBuffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	stop := make(chan os.Signal, 1)
	stop <- syscall.SIGTERM // taken once run serves

	// run is called on the test goroutine: once it returns, nothing it started may
	// still run.
	if err := run(context.Background(), logger, drainEnv(t, "0"), make(chan os.Signal), stop); err != nil {
		t.Fatalf("run returned %v, want nil", err)
	}
	if left := ourGoroutines(); len(left) > 0 {
		t.Errorf("goroutines left after run returned:\n%s", strings.Join(left, "\n\n"))
	}
	out := logs.String()
	last := -1
	for _, want := range []string{`msg="kaiak stopping" kaiak.reason="signal terminated"`, `msg=draining kaiak.drain.grace=0 kaiak.drain.timeout=3600`,
		`msg="draining: refusing new requests"`, `msg=drained`, `msg="kaiak stopped"`} {
		i := strings.Index(out, want)
		if i < 0 {
			t.Errorf("log misses %q:\n%s", want, out)
			continue
		}
		if i < last {
			t.Errorf("%q logged out of order:\n%s", want, out)
		}
		last = i
	}
}

func TestSecondStopSignalSkipsTheRemainingDrain(t *testing.T) {
	var logs syncBuffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	stop := make(chan os.Signal, 2)
	stop <- syscall.SIGTERM
	stop <- os.Interrupt

	// A one-hour grace period: run returns only because the second signal hurried it.
	if err := run(context.Background(), logger, drainEnv(t, "3600000"), make(chan os.Signal), stop); err != nil {
		t.Fatalf("run returned %v, want nil", err)
	}
	if left := ourGoroutines(); len(left) > 0 {
		t.Errorf("goroutines left after run returned:\n%s", strings.Join(left, "\n\n"))
	}
	out := logs.String()
	for _, want := range []string{`msg="second stop signal: skipping the remaining drain" kaiak.signal=interrupt`,
		`msg=drained`, `msg="kaiak stopped"`} {
		if !strings.Contains(out, want) {
			t.Errorf("log misses %q:\n%s", want, out)
		}
	}
}

func TestRunFailsWhenAListenAddressIsTaken(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	env := envOf(map[string]string{
		"KAIAK_CONFIG_FILE": minimalFixture,
		"KAIAK_INSTANCE_ID": "test-1",
		"KAIAK_LISTEN_ADDR": "127.0.0.1:0",
		"KAIAK_ADMIN_ADDR":  taken.Addr().String(),
	})
	err = run(context.Background(), slog.New(slog.DiscardHandler), env, make(chan os.Signal), make(chan os.Signal))
	if err == nil || !strings.Contains(err.Error(), "admin listener") {
		t.Fatalf("want an admin listener bind error, got %v", err)
	}
}

func TestRunFailsWithoutAConfigFile(t *testing.T) {
	logger := slog.New(slog.DiscardHandler)
	err := run(context.Background(), logger, envOf(nil), make(chan os.Signal), make(chan os.Signal))
	if err == nil || !strings.Contains(err.Error(), "no config source") {
		t.Fatalf("want a missing-config error, got %v", err)
	}
}

func TestRunFailsOnAnInvalidConfigFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"format_version": 5}`), 0o600); err != nil {
		t.Fatal(err)
	}
	env := envOf(map[string]string{"KAIAK_CONFIG_FILE": path})
	err := run(context.Background(), slog.New(slog.DiscardHandler), env, make(chan os.Signal), make(chan os.Signal))
	if err == nil || !strings.Contains(err.Error(), "config rejected") {
		t.Fatalf("want a config rejection, got %v", err)
	}
}

func TestSignalTriggersReloadAndStopsWithContext(t *testing.T) {
	var logs syncBuffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	path := filepath.Join(t.TempDir(), "config.json")
	data, err := os.ReadFile(minimalFixture)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	holder := &config.Holder{}
	registry := metric.NewRegistry()
	ops := metrics.NewOps(registry, routing.New(routing.Options{}), metrics.NewCircuits(registry), holder)
	loader := config.NewFileLoader(path, config.NewApplier(holder, logger, envOf(nil), ops.ConfigLoaded))
	if err := loader.Load(config.TriggerStartup); err != nil {
		t.Fatal(err)
	}
	running := holder.Current()

	ctx, cancel := context.WithCancel(context.Background())
	reload := make(chan os.Signal) // unbuffered: a send returns once the loop took it
	var wg sync.WaitGroup
	wg.Go(func() { reloadOnSignal(ctx, reload, loader) })

	if err := os.WriteFile(path, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	reload <- syscall.SIGHUP
	// The loop handles one signal at a time, so a second send returns only after the
	// first reload finished.
	reload <- syscall.SIGHUP
	cancel()
	wg.Wait() // the loop stops on cancel

	if holder.Current() != running {
		t.Error("a rejected reload replaced the running config")
	}
	out := logs.String()
	if got := strings.Count(out, `msg="config rejected"`); got != 2 {
		t.Errorf("%d rejected reloads logged, want 2:\n%s", got, out)
	}
	if !strings.Contains(out, "kaiak.trigger=sighup") || !strings.Contains(out, "kaiak.config.running=kept") {
		t.Errorf("reload not logged as a kept config:\n%s", out)
	}
	var exposition bytes.Buffer
	metric.WritePrometheus(&exposition, registry.Collect())
	for _, want := range []string{
		`kaiak_config_loads_total{kaiak_trigger="startup",kaiak_config_result="applied"} 1`,
		`kaiak_config_loads_total{kaiak_trigger="sighup",kaiak_config_result="rejected"} 2`,
	} {
		if !strings.Contains(exposition.String(), want+"\n") {
			t.Errorf("metrics miss %q:\n%s", want, exposition.String())
		}
	}
	if strings.Contains(exposition.String(), "kaiak_config_last_applied_timestamp_seconds 0\n") {
		t.Error("config applied timestamp not set")
	}
}

func TestBuildVersion(t *testing.T) {
	stamped := &debug.BuildInfo{Main: debug.Module{Version: "v0.0.0-20260925-abcdef"}}
	for _, c := range []struct {
		name, stamped string
		info          *debug.BuildInfo
		ok            bool
		want          string
	}{
		{"link-time version wins", "0.6.0-3-gabc1234", stamped, true, "0.6.0-3-gabc1234"},
		{"module version without a link-time one", "", stamped, true, "v0.0.0-20260925-abcdef"},
		{"go run", "", &debug.BuildInfo{}, true, "(devel)"},
		{"no build info", "", nil, false, "(devel)"},
	} {
		if got := buildVersion(c.stamped, c.info, c.ok); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestNewLoggerFormats(t *testing.T) {
	var buf bytes.Buffer
	for _, format := range []string{"", "json"} {
		buf.Reset()
		logger, err := newLogger(format, &buf)
		if err != nil {
			t.Fatal(err)
		}
		logger.Info("hello")
		if !strings.HasPrefix(buf.String(), "{") {
			t.Errorf("format %q wrote %q, want JSON", format, buf.String())
		}
	}
	buf.Reset()
	logger, err := newLogger("text", &buf)
	if err != nil {
		t.Fatal(err)
	}
	logger.Info("hello")
	if !strings.Contains(buf.String(), "msg=hello") {
		t.Errorf("text format wrote %q", buf.String())
	}
	if _, err := newLogger("yaml", &buf); err == nil {
		t.Error("unknown format accepted")
	}
}

func TestRunInControlModeBootsFromTheControlPlane(t *testing.T) {
	cp := fakecontrol.New("cp-token")
	defer cp.Close()
	data, err := os.ReadFile(minimalFixture)
	if err != nil {
		t.Fatal(err)
	}
	hash := cp.Publish(data)
	var logs syncBuffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	env := envOf(map[string]string{
		"KAIAK_CONTROL_URL":    cp.URL(),
		"KAIAK_CONTROL_TOKEN":  "cp-token",
		"KAIAK_INSTANCE_ID":    "test-1",
		"KAIAK_LISTEN_ADDR":    "127.0.0.1:0",
		"KAIAK_ADMIN_ADDR":     "127.0.0.1:0",
		"KAIAK_DRAIN_GRACE_MS": "0",
	})
	stop := make(chan os.Signal, 1)
	stopped := stopWhenServing(&logs, stop)

	if err := run(context.Background(), logger, env, make(chan os.Signal), stop); err != nil {
		t.Fatalf("run returned %v, want nil", err)
	}
	stopped()
	cp.Close() // ends the server's side of any stream before counting goroutines
	if left := ourGoroutines(); len(left) > 0 {
		t.Errorf("goroutines left after run returned:\n%s", strings.Join(left, "\n\n"))
	}
	out := logs.String()
	for _, want := range []string{"kaiak.control.url=" + cp.URL(), `msg="config applied" kaiak.trigger=control kaiak.config.hash=` + hash,
		`msg="kaiak stopped"`} {
		if !strings.Contains(out, want) {
			t.Errorf("log misses %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "cp-token") {
		t.Error("the control token reached the log")
	}
	statuses := cp.Statuses()
	if len(statuses) == 0 || !strings.Contains(string(statuses[len(statuses)-1]), `"state":"draining"`) {
		t.Errorf("statuses %s, want the last one draining", statuses)
	}
	if !strings.Contains(out, `msg="usage flushed" kaiak.control.url=`+cp.URL()+` kaiak.trigger=drain`) {
		t.Errorf("no usage flush in the drain:\n%s", out)
	}
}

// A stop signal during the boot wait — the control plane not up yet, or its first
// totals not come — ends the boot at once: run returns without binding a listener,
// long before the 60 s boot wait would end.
func TestStopSignalDuringTheBootWaitExits(t *testing.T) {
	data, err := os.ReadFile(minimalFixture)
	if err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		setup   func(cp *fakecontrol.Server)
		waiting string // the log line that shows the boot waiting
	}{
		"control plane down": {func(cp *fakecontrol.Server) { cp.SetDown(true) },
			"config not received at startup"},
		"first totals withheld": {func(cp *fakecontrol.Server) { cp.HoldTotalsOnConnect(true) },
			"waiting for the first totals"},
	} {
		t.Run(name, func(t *testing.T) {
			cp := fakecontrol.New("cp-token")
			defer cp.Close()
			cp.Publish(data)
			c.setup(cp)
			var logs syncBuffer
			logger := slog.New(slog.NewTextHandler(&logs, nil))
			env := envOf(map[string]string{
				"KAIAK_CONTROL_URL":          cp.URL(),
				"KAIAK_CONTROL_TOKEN":        "cp-token",
				"KAIAK_INSTANCE_ID":          "test-1",
				"KAIAK_LISTEN_ADDR":          "127.0.0.1:0",
				"KAIAK_ADMIN_ADDR":           "127.0.0.1:0",
				"KAIAK_CONTROL_BOOT_WAIT_MS": "60000",
			})
			stop := make(chan os.Signal, 1)
			done := make(chan error, 1)
			go func() { done <- run(context.Background(), logger, env, make(chan os.Signal), stop) }()
			deadline := time.Now().Add(5 * time.Second)
			for !strings.Contains(logs.String(), c.waiting) {
				if time.Now().After(deadline) {
					t.Fatalf("boot never waited (%q):\n%s", c.waiting, logs.String())
				}
				time.Sleep(10 * time.Millisecond)
			}
			stop <- syscall.SIGTERM
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("run returned %v, want nil", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("run still booting 5 s after the stop signal:\n%s", logs.String())
			}
			cp.Close()
			if left := ourGoroutines(); len(left) > 0 {
				t.Errorf("goroutines left after run returned:\n%s", strings.Join(left, "\n\n"))
			}
			out := logs.String()
			if !strings.Contains(out, `msg="kaiak stopped" kaiak.reason="signal terminated during boot"`) {
				t.Errorf("no stop logged:\n%s", out)
			}
			if strings.Contains(out, "kaiak.listener.name=api") {
				t.Errorf("a listener bound after the stop signal:\n%s", out)
			}
		})
	}
}
