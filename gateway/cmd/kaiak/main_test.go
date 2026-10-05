package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"kaiak/internal/config"
	"kaiak/internal/control"
	"kaiak/internal/fakecontrol"
	"kaiak/internal/limits"
	"kaiak/internal/metrics"
	"kaiak/internal/routing"
	"kaiak/internal/server"
)

const minimalFixture = "../../../protocol/fixtures/config/valid/minimal.json"

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
	dataDir := filepath.Join(t.TempDir(), "data")
	env := envOf(map[string]string{
		"KAIAK_CONFIG_FILE": minimalFixture,
		"KAIAK_DATA_DIR":    dataDir,
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
		"kaiak.listener.name=api", "kaiak.listener.name=admin", "limits snapshot restored", `msg="limits snapshot written" kaiak.trigger=shutdown`,
		"kaiak stopped"} {
		if !strings.Contains(out, want) {
			t.Errorf("log misses %q:\n%s", want, out)
		}
	}
	if info, err := os.Stat(dataDir); err != nil || !info.IsDir() {
		t.Errorf("data directory not created: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, limits.SnapshotFile)); err != nil {
		t.Errorf("no limits snapshot written on shutdown: %v", err)
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
func drainEnv(t *testing.T, dataDir, graceMS string) func(string) (string, bool) {
	t.Helper()
	return envOf(map[string]string{
		"KAIAK_CONFIG_FILE":      minimalFixture,
		"KAIAK_DATA_DIR":         dataDir,
		"KAIAK_INSTANCE_ID":      "test-1",
		"KAIAK_LISTEN_ADDR":      "127.0.0.1:0",
		"KAIAK_ADMIN_ADDR":       "127.0.0.1:0",
		"KAIAK_DRAIN_GRACE_MS":   graceMS,
		"KAIAK_DRAIN_TIMEOUT_MS": "3600000",
	})
}

func TestStopSignalDrainsThenWritesTheSnapshot(t *testing.T) {
	var logs syncBuffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	dataDir := t.TempDir()
	stop := make(chan os.Signal, 1)
	stop <- syscall.SIGTERM // taken once run serves

	// run is called on the test goroutine: once it returns, nothing it started may
	// still run.
	if err := run(context.Background(), logger, drainEnv(t, dataDir, "0"), make(chan os.Signal), stop); err != nil {
		t.Fatalf("run returned %v, want nil", err)
	}
	if left := ourGoroutines(); len(left) > 0 {
		t.Errorf("goroutines left after run returned:\n%s", strings.Join(left, "\n\n"))
	}
	out := logs.String()
	last := -1
	for _, want := range []string{`msg="kaiak stopping" kaiak.reason="signal terminated"`, `msg=draining kaiak.drain.grace=0 kaiak.drain.timeout=3600`,
		`msg="draining: refusing new requests"`, `msg=drained`, `msg="limits snapshot written" kaiak.trigger=shutdown`,
		`msg="kaiak stopped"`} {
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
	if _, err := os.Stat(filepath.Join(dataDir, limits.SnapshotFile)); err != nil {
		t.Errorf("no limits snapshot written on shutdown: %v", err)
	}
}

func TestSecondStopSignalSkipsTheRemainingDrain(t *testing.T) {
	var logs syncBuffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	dataDir := t.TempDir()
	stop := make(chan os.Signal, 2)
	stop <- syscall.SIGTERM
	stop <- os.Interrupt

	// A one-hour grace period: run returns only because the second signal hurried it.
	if err := run(context.Background(), logger, drainEnv(t, dataDir, "3600000"), make(chan os.Signal), stop); err != nil {
		t.Fatalf("run returned %v, want nil", err)
	}
	if left := ourGoroutines(); len(left) > 0 {
		t.Errorf("goroutines left after run returned:\n%s", strings.Join(left, "\n\n"))
	}
	out := logs.String()
	for _, want := range []string{`msg="second stop signal: skipping the remaining drain" kaiak.signal=interrupt`,
		`msg=drained`, `msg="limits snapshot written" kaiak.trigger=shutdown`, `msg="kaiak stopped"`} {
		if !strings.Contains(out, want) {
			t.Errorf("log misses %q:\n%s", want, out)
		}
	}
	if _, err := os.Stat(filepath.Join(dataDir, limits.SnapshotFile)); err != nil {
		t.Errorf("no limits snapshot written on shutdown: %v", err)
	}
}

func TestDrainTimeSettings(t *testing.T) {
	base := map[string]string{"KAIAK_CONFIG_FILE": "c.json", "KAIAK_INSTANCE_ID": "i"}
	with := func(extra map[string]string) map[string]string {
		m := map[string]string{}
		for k, v := range base {
			m[k] = v
		}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	s, err := readSettings(envOf(base))
	if err != nil {
		t.Fatal(err)
	}
	if s.drain.Grace != 5*time.Second || s.drain.Timeout != 60*time.Second {
		t.Errorf("defaults = %v, %v; want 5s, 1m0s", s.drain.Grace, s.drain.Timeout)
	}
	s, err = readSettings(envOf(with(map[string]string{"KAIAK_DRAIN_GRACE_MS": "0", "KAIAK_DRAIN_TIMEOUT_MS": "1500"})))
	if err != nil {
		t.Fatal(err)
	}
	if s.drain.Grace != 0 || s.drain.Timeout != 1500*time.Millisecond {
		t.Errorf("set = %v, %v; want 0s, 1.5s", s.drain.Grace, s.drain.Timeout)
	}
	for _, bad := range []string{"-1", "1.5", "5s", "abc", "99999999999999999999"} {
		for _, name := range []string{"KAIAK_DRAIN_GRACE_MS", "KAIAK_DRAIN_TIMEOUT_MS"} {
			if _, err := readSettings(envOf(with(map[string]string{name: bad}))); err == nil || !strings.Contains(err.Error(), name) {
				t.Errorf("%s=%q: got %v, want an error naming it", name, bad, err)
			}
		}
	}
}

func TestClientTimeoutSettings(t *testing.T) {
	base := map[string]string{"KAIAK_CONFIG_FILE": "c.json", "KAIAK_INSTANCE_ID": "i"}
	with := func(name, value string) map[string]string {
		m := map[string]string{name: value}
		for k, v := range base {
			m[k] = v
		}
		return m
	}
	s, err := readSettings(envOf(base))
	if err != nil {
		t.Fatal(err)
	}
	if s.client != server.DefaultClientTimeouts {
		t.Errorf("defaults = %+v, want %+v", s.client, server.DefaultClientTimeouts)
	}
	for name, got := range map[string]func(settings) time.Duration{
		"KAIAK_IDLE_TIMEOUT_MS":      func(s settings) time.Duration { return s.client.Idle },
		"KAIAK_BODY_READ_TIMEOUT_MS": func(s settings) time.Duration { return s.client.BodyRead },
		"KAIAK_WRITE_TIMEOUT_MS":     func(s settings) time.Duration { return s.client.Write },
	} {
		s, err := readSettings(envOf(with(name, "1500")))
		if err != nil || got(s) != 1500*time.Millisecond {
			t.Errorf("%s=1500: %v, %v; want 1.5s", name, got(s), err)
		}
		for _, bad := range []string{"0", "-1", "5s"} {
			if _, err := readSettings(envOf(with(name, bad))); err == nil || !strings.Contains(err.Error(), name) {
				t.Errorf("%s=%q: got %v, want an error naming it", name, bad, err)
			}
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
		"KAIAK_DATA_DIR":    t.TempDir(),
		"KAIAK_INSTANCE_ID": "test-1",
		"KAIAK_LISTEN_ADDR": "127.0.0.1:0",
		"KAIAK_ADMIN_ADDR":  taken.Addr().String(),
	})
	err = run(context.Background(), slog.New(slog.DiscardHandler), env, make(chan os.Signal), make(chan os.Signal))
	if err == nil || !strings.Contains(err.Error(), "admin listener") {
		t.Fatalf("want an admin listener bind error, got %v", err)
	}
}

func TestListenAddressDefaults(t *testing.T) {
	s, err := readSettings(envOf(map[string]string{"KAIAK_CONFIG_FILE": "c.json", "KAIAK_INSTANCE_ID": "i"}))
	if err != nil {
		t.Fatal(err)
	}
	if s.listenAddr != ":8080" || s.adminAddr != ":9090" {
		t.Errorf("defaults = %q, %q; want :8080, :9090", s.listenAddr, s.adminAddr)
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
	if err := os.WriteFile(path, []byte(`{"format_version": 4}`), 0o600); err != nil {
		t.Fatal(err)
	}
	env := envOf(map[string]string{"KAIAK_CONFIG_FILE": path, "KAIAK_DATA_DIR": t.TempDir()})
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
	registry := metrics.NewRegistry()
	ops := metrics.NewOps(registry, routing.New(routing.Options{}), holder)
	loader := config.NewFileLoader(path, config.NewApplier(holder, logger, envOf(nil), ops.ConfigLoaded))
	if err := loader.Load("startup"); err != nil {
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
	registry.WriteText(&exposition)
	for _, want := range []string{
		`kaiak_config_loads_total{trigger="startup",result="applied"} 1`,
		`kaiak_config_loads_total{trigger="sighup",result="rejected"} 2`,
	} {
		if !strings.Contains(exposition.String(), want+"\n") {
			t.Errorf("metrics miss %q:\n%s", want, exposition.String())
		}
	}
	if strings.Contains(exposition.String(), "kaiak_config_last_applied_timestamp_seconds 0\n") {
		t.Error("config applied timestamp not set")
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

func TestControlModeSettings(t *testing.T) {
	controlEnv := map[string]string{"KAIAK_CONTROL_URL": "https://cp.example:8443/base", "KAIAK_CONTROL_TOKEN": "t0ken",
		"KAIAK_INSTANCE_ID": "gw-1"}
	with := func(extra map[string]string) map[string]string {
		m := map[string]string{}
		for k, v := range controlEnv {
			m[k] = v
		}
		for k, v := range extra {
			if v == "" {
				delete(m, k)
				continue
			}
			m[k] = v
		}
		return m
	}
	s, err := readSettings(envOf(controlEnv))
	if err != nil {
		t.Fatal(err)
	}
	if s.configFile != "" || s.control == nil || s.control.url.String() != "https://cp.example:8443/base" ||
		s.control.token != "t0ken" || s.control.bootWait != 60*time.Second {
		t.Errorf("settings %+v %+v", s, s.control)
	}
	s, err = readSettings(envOf(with(map[string]string{"KAIAK_CONTROL_BOOT_WAIT_MS": "250"})))
	if err != nil || s.control.bootWait != 250*time.Millisecond {
		t.Errorf("boot wait %v, %v", s.control, err)
	}

	for name, c := range map[string]struct {
		env  map[string]string
		want string
	}{
		"both modes":          {with(map[string]string{"KAIAK_CONFIG_FILE": "c.json"}), "both set"},
		"token only":          {with(map[string]string{"KAIAK_CONTROL_URL": ""}), "KAIAK_CONTROL_TOKEN is set without KAIAK_CONTROL_URL"},
		"token beside a file": {with(map[string]string{"KAIAK_CONTROL_URL": "", "KAIAK_CONFIG_FILE": "c.json"}), "KAIAK_CONTROL_TOKEN is set without"},
		"url only":            {with(map[string]string{"KAIAK_CONTROL_TOKEN": ""}), "KAIAK_CONTROL_URL is set without KAIAK_CONTROL_TOKEN"},
		"not a url":           {with(map[string]string{"KAIAK_CONTROL_URL": "cp.example:8443"}), "KAIAK_CONTROL_URL: want"},
		"query":               {with(map[string]string{"KAIAK_CONTROL_URL": "http://cp/?token=x"}), "KAIAK_CONTROL_URL: want"},
		"bad instance":        {with(map[string]string{"KAIAK_INSTANCE_ID": "gw 1"}), `instance ID "gw 1"`},
		"zero boot wait":      {with(map[string]string{"KAIAK_CONTROL_BOOT_WAIT_MS": "0"}), "KAIAK_CONTROL_BOOT_WAIT_MS"},
		"bad boot wait":       {with(map[string]string{"KAIAK_CONTROL_BOOT_WAIT_MS": "5s"}), "KAIAK_CONTROL_BOOT_WAIT_MS"},
	} {
		if _, err := readSettings(envOf(c.env)); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want an error containing %q", name, err, c.want)
		}
	}
	// File mode does not send the instance ID anywhere that checks its shape.
	if _, err := readSettings(envOf(map[string]string{"KAIAK_CONFIG_FILE": "c.json", "KAIAK_INSTANCE_ID": "gw 1"})); err != nil {
		t.Errorf("file mode refused an instance ID: %v", err)
	}
}

func TestRunInControlModeBootsFromTheControlPlane(t *testing.T) {
	cp := fakecontrol.New("cp-token")
	defer cp.Close()
	data, err := os.ReadFile(minimalFixture)
	if err != nil {
		t.Fatal(err)
	}
	cp.Publish(data)
	var logs syncBuffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	dataDir := t.TempDir()
	env := envOf(map[string]string{
		"KAIAK_CONTROL_URL":    cp.URL(),
		"KAIAK_CONTROL_TOKEN":  "cp-token",
		"KAIAK_DATA_DIR":       dataDir,
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
	for _, want := range []string{"kaiak.control.url=" + cp.URL(), `msg="config applied" kaiak.trigger=control kaiak.config.version=1`,
		`msg="kaiak stopped"`} {
		if !strings.Contains(out, want) {
			t.Errorf("log misses %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "cp-token") {
		t.Error("the control token reached the log")
	}
	if _, err := os.Stat(filepath.Join(dataDir, control.LastKnownGoodFile)); err != nil {
		t.Errorf("no last-known-good config written: %v", err)
	}
	statuses := cp.Statuses()
	if len(statuses) == 0 || !strings.Contains(string(statuses[len(statuses)-1]), `"state":"draining"`) {
		t.Errorf("statuses %s, want the last one draining", statuses)
	}
	if !strings.Contains(out, `msg="usage flushed" kaiak.control.url=`+cp.URL()+` kaiak.trigger=drain`) {
		t.Errorf("no usage flush in the drain:\n%s", out)
	}
	// The control plane owns hour and month totals: no file-mode snapshot.
	if strings.Contains(out, "limits snapshot") {
		t.Errorf("limits snapshot used in control-plane mode:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(dataDir, limits.SnapshotFile)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("limits snapshot written in control-plane mode: %v", err)
	}
}

func TestServingStatusCoversTheAppliedConfig(t *testing.T) {
	data, err := os.ReadFile("../../../protocol/fixtures/config/valid/full.json")
	if err != nil {
		t.Fatal(err)
	}
	s, err := config.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	openedAt := time.Date(2026, 9, 24, 12, 0, 0, 0, time.FixedZone("CEST", 2*60*60))
	got := servingStatus(map[string]int{"vllm-a": 2, "retired": 1}, map[string]int{"qwen3-32b": 3, "retired-model": 1},
		map[routing.DeploymentID]routing.CircuitReport{
			{Backend: "vllm-b", Model: "BAAI/bge-m3"}:    {State: routing.CircuitOpen, OpenedAt: openedAt},
			{Backend: "vllm-b", Model: "Qwen/Qwen3-32B"}: {State: routing.CircuitHalfOpen, OpenedAt: openedAt},
		}, s)

	if len(got.Backends) != 4 || len(got.Models) != 5 {
		t.Fatalf("backends %v, models %v: want the config's 3 backends plus the retired one, and its 4 models plus the retired one", got.Backends, got.Models)
	}
	a := got.Backends["vllm-a"]
	if a.InFlight != 2 || a.MaxInFlight != 8 || len(a.Deployments) != 1 || a.Deployments["Qwen/Qwen3-32B"].Circuit != control.CircuitClosed {
		t.Errorf("vllm-a = %+v", a)
	}
	b := got.Backends["vllm-b"]
	if b.InFlight != 0 || b.MaxInFlight != 0 || len(b.Deployments) != 2 {
		t.Errorf("vllm-b = %+v, want idle, no cap, two deployments", b)
	}
	if d := b.Deployments["BAAI/bge-m3"]; d.Circuit != control.CircuitOpen || d.OpenedAt == nil ||
		!d.OpenedAt.Equal(openedAt) || d.OpenedAt.Location() != time.UTC {
		t.Errorf("vllm-b/BAAI/bge-m3 = %+v, want open since %s, in UTC", d, openedAt)
	}
	if d := b.Deployments["Qwen/Qwen3-32B"]; d.Circuit != control.CircuitHalfOpen || d.OpenedAt == nil || !d.OpenedAt.Equal(openedAt) {
		t.Errorf("vllm-b/Qwen/Qwen3-32B = %+v, want half-open since %s", d, openedAt)
	}
	if r := got.Backends["retired"]; r.InFlight != 1 || r.Deployments == nil || len(r.Deployments) != 0 {
		t.Errorf("retired = %+v, want its in-flight count and no deployments", r)
	}
	if m, ok := got.Models["qwen3-32b"]; !ok || m.Queued != 3 {
		t.Errorf("qwen3-32b = %+v, %v; want 3 queued", m, ok)
	}
	if m := got.Models["retired-model"]; m.Queued != 1 {
		t.Errorf("retired-model = %+v, want its 1 queued", m)
	}
	for name, m := range got.Models {
		if name != "qwen3-32b" && name != "retired-model" && m.Queued != 0 {
			t.Errorf("%s = %+v, want 0 queued", name, m)
		}
	}

	none := servingStatus(map[string]int{}, map[string]int{}, nil, nil)
	if none.Backends == nil || none.Models == nil || len(none.Backends)+len(none.Models) != 0 {
		t.Errorf("before a config: %+v, want empty collections", none)
	}
}

// The limiter applies totals only to the config they were computed under, so the
// conversion must carry that identity: epoch and version.
func TestLimitsTotalsCarryTheirConfig(t *testing.T) {
	got := limitsTotals(&control.Totals{ConfigEpoch: "c0ffee00c0ffee00c0ffee00c0ffee00", ConfigVersion: 7, LiveGateways: 3})
	if want := (config.Version{Epoch: "c0ffee00c0ffee00c0ffee00c0ffee00", Number: 7}); got.Config != want || got.LiveGateways != 3 {
		t.Errorf("limitsTotals = %+v, want config %+v and 3 live gateways", got, want)
	}
	if limitsTotals(nil) != nil {
		t.Error("nil totals must stay nil")
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
			"config snapshot not fetched at startup"},
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
				"KAIAK_DATA_DIR":             t.TempDir(),
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
