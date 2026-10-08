package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"kaiak/internal/telemetry/fakeotlp"
)

func TestLogExportSettings(t *testing.T) {
	base := map[string]string{"KAIAK_CONFIG_FILE": "c.json", "KAIAK_INSTANCE_ID": "i"}
	with := func(extra map[string]string) map[string]string {
		env := map[string]string{}
		for k, v := range base {
			env[k] = v
		}
		for k, v := range extra {
			env[k] = v
		}
		return env
	}
	if s, err := readSettings(envOf(base)); err != nil || s.logExport != nil {
		t.Errorf("nothing set: export %v, err %v; want off", s.logExport, err)
	}
	s, err := readSettings(envOf(with(map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://collector:4318"})))
	if err != nil || s.logExport == nil || s.logExport.EndpointHost() != "collector:4318" {
		t.Errorf("endpoint set: export %v, err %v; want on, to collector:4318", s.logExport, err)
	}
	for name, value := range map[string]string{
		"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT": "ftp://collector",
		"OTEL_EXPORTER_OTLP_LOGS_PROTOCOL": "grpc",
		"OTEL_EXPORTER_OTLP_LOGS_TIMEOUT":  "soon",
	} {
		env := with(map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://collector:4318", name: value})
		if _, err := readSettings(envOf(env)); err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("%s=%q: got %v, want a startup error naming it", name, value, err)
		}
	}
}

// A start that fails once the exporter runs still sends its lines — the cause
// included — before run returns; the error is logged once.
func TestRunStartFailureReachesTheCollector(t *testing.T) {
	collector := fakeotlp.New(t, nil)
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"format_version": 5}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var logs syncBuffer
	env := envOf(map[string]string{"KAIAK_CONFIG_FILE": path, "KAIAK_INSTANCE_ID": "test-1",
		"OTEL_EXPORTER_OTLP_ENDPOINT": collector.URL})
	err := run(context.Background(), slog.New(slog.NewJSONHandler(&logs, nil)), env, make(chan os.Signal), make(chan os.Signal))
	if err == nil || !strings.Contains(err.Error(), "config rejected") {
		t.Fatalf("want a config rejection, got %v", err)
	}
	want := []string{"kaiak starting", "config rejected", "kaiak stopped with an error"}
	if got := collector.Bodies(); !slices.Equal(got, want) {
		t.Errorf("collector received %q, want %q", got, want)
	}
	if n := strings.Count(logs.String(), `"msg":"kaiak stopped with an error"`); n != 1 {
		t.Errorf("error logged %d times on stderr, want once:\n%s", n, logs.String())
	}
}

// A start that fails before the exporter exists (its settings) reaches stderr.
func TestRunSettingsFailureReachesStderr(t *testing.T) {
	var logs syncBuffer
	env := envOf(map[string]string{"KAIAK_CONFIG_FILE": "c.json", "OTEL_EXPORTER_OTLP_ENDPOINT": "collector:4318"})
	err := run(context.Background(), slog.New(slog.NewJSONHandler(&logs, nil)), env, make(chan os.Signal), make(chan os.Signal))
	if err == nil || !strings.Contains(err.Error(), "OTEL_EXPORTER_OTLP_ENDPOINT") {
		t.Fatalf("want an endpoint error, got %v", err)
	}
	if n := strings.Count(logs.String(), `"msg":"kaiak stopped with an error"`); n != 1 {
		t.Errorf("error logged %d times on stderr, want once:\n%s", n, logs.String())
	}
}

// A second stop signal arriving during the final log flush cuts it to its 1 s:
// with a collector that stalls on the last lines, run returns about a second after
// the flush started, not at the drain's deadline (10 s here).
func TestSecondSignalCutsTheFinalLogFlush(t *testing.T) {
	flushing := make(chan struct{})
	release := make(chan struct{})
	var seen, released sync.Once
	unblock := func() { released.Do(func() { close(release) }) }
	var collector *fakeotlp.Collector
	collector = fakeotlp.New(t, func(w http.ResponseWriter, r *http.Request, n int) {
		if slices.Contains(collector.Received()[n].Messages(), "kaiak stopped") {
			seen.Do(func() { close(flushing) })
			select {
			case <-release:
			case <-r.Context().Done():
			}
		}
		io.WriteString(w, "{}")
	})
	defer unblock()
	var logs syncBuffer
	stop := make(chan os.Signal, 2)
	stop <- syscall.SIGTERM
	env := envOf(map[string]string{
		"KAIAK_CONFIG_FILE": minimalFixture, "KAIAK_INSTANCE_ID": "test-1",
		"KAIAK_LISTEN_ADDR": "127.0.0.1:0", "KAIAK_ADMIN_ADDR": "127.0.0.1:0",
		"KAIAK_DRAIN_GRACE_MS": "0", "KAIAK_DRAIN_TIMEOUT_MS": "10000",
		"OTEL_EXPORTER_OTLP_ENDPOINT": collector.URL, "OTEL_EXPORTER_OTLP_TIMEOUT": "20000",
	})
	done := make(chan error, 1)
	go func() {
		done <- run(context.Background(), slog.New(slog.NewJSONHandler(&logs, nil)), env, make(chan os.Signal), stop)
	}()
	select {
	case <-flushing:
	case err := <-done:
		t.Fatalf("run returned before the final flush: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("the final flush never reached the collector")
	}
	stop <- os.Interrupt
	// The 1 s floor, plus room for scheduling.
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		unblock()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		t.Fatal("the second signal did not cut the final log flush: run returned only once the collector answered")
	}
}
