package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"kaiak/internal/telemetry/fakeotlp"
)

// configLoads is the value of kaiak.config.loads for trigger and result in a metric
// export, and whether the series was in it.
func configLoads(t *testing.T, r fakeotlp.Received, trigger, result string) (string, bool) {
	t.Helper()
	for _, m := range r.Metrics() {
		if m.Name != "kaiak.config.loads" || m.Sum == nil {
			continue
		}
		for _, p := range m.Sum.DataPoints {
			attrs := map[string]string{}
			for _, kv := range p.Attributes {
				if kv.Value.StringValue != nil {
					attrs[kv.Key] = *kv.Value.StringValue
				}
			}
			if attrs["kaiak.trigger"] == trigger && attrs["kaiak.config.result"] == result && p.AsInt != nil {
				return *p.AsInt, true
			}
		}
	}
	return "", false
}

// The final metric export runs before run's last line: after a drain, before
// `kaiak stopped`; after a start that fails once the exporter runs, before `kaiak
// stopped with an error` — and it carries what the process ends with.
func TestFinalMetricExportBeforeTheLastLine(t *testing.T) {
	rejected := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(rejected, []byte(`{"format_version": 5}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, configFile, lastLine, result string
		wantErr                            bool
	}{
		{"drained", minimalFixture, `"msg":"kaiak stopped"`, "applied", false},
		{"start failed", rejected, `"msg":"kaiak stopped with an error"`, "rejected", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs syncBuffer
			var mu sync.Mutex
			lastLineWritten := map[int]bool{} // per export: the last line already on stderr
			collector := fakeotlp.New(t, func(w http.ResponseWriter, r *http.Request, n int) {
				mu.Lock()
				lastLineWritten[n] = strings.Contains(logs.String(), tc.lastLine)
				mu.Unlock()
				io.WriteString(w, "{}")
			})
			stop := make(chan os.Signal, 1)
			stop <- syscall.SIGTERM
			env := envOf(map[string]string{
				"KAIAK_CONFIG_FILE": tc.configFile, "KAIAK_INSTANCE_ID": "test-1",
				"KAIAK_LISTEN_ADDR": "127.0.0.1:0", "KAIAK_ADMIN_ADDR": "127.0.0.1:0", "KAIAK_DRAIN_GRACE_MS": "0",
				"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT": collector.URL + "/v1/metrics",
			})
			err := run(context.Background(), slog.New(slog.NewJSONHandler(&logs, nil)), env, make(chan os.Signal), stop)
			if (err != nil) != tc.wantErr {
				t.Fatalf("run: %v, want an error %v", err, tc.wantErr)
			}
			// The interval (60 s by default) never came: the one export is the final one.
			exports := collector.Received()
			if len(exports) != 1 {
				t.Fatalf("collector received %d exports, want the final one alone", len(exports))
			}
			mu.Lock()
			defer mu.Unlock()
			if lastLineWritten[0] {
				t.Errorf("the final export arrived after %s", tc.lastLine)
			}
			if v, ok := configLoads(t, exports[0], "startup", tc.result); !ok || v != "1" {
				t.Errorf("kaiak.config.loads{startup, %s} = %q (present %v), want 1", tc.result, v, ok)
			}
			if !strings.Contains(logs.String(), tc.lastLine) {
				t.Errorf("no %s line:\n%s", tc.lastLine, logs.String())
			}
		})
	}
}

// A second stop signal arriving during the final metric export cuts it to its 1 s:
// with a collector that stalls on it, run returns about a second after the export
// started, not at the drain's deadline (10 s here); the export counts failed and is
// reported.
func TestSecondSignalCutsTheFinalMetricExport(t *testing.T) {
	exporting := make(chan struct{})
	release := make(chan struct{})
	var seen, released sync.Once
	unblock := func() { released.Do(func() { close(release) }) }
	collector := fakeotlp.New(t, func(w http.ResponseWriter, r *http.Request, n int) {
		seen.Do(func() { close(exporting) })
		select {
		case <-release:
		case <-r.Context().Done():
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
		"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT": collector.URL + "/v1/metrics", "OTEL_EXPORTER_OTLP_TIMEOUT": "20000",
		"OTEL_METRIC_EXPORT_TIMEOUT": "20000",
	})
	done := make(chan error, 1)
	go func() {
		done <- run(context.Background(), slog.New(slog.NewJSONHandler(&logs, nil)), env, make(chan os.Signal), stop)
	}()
	select {
	case <-exporting:
	case err := <-done:
		t.Fatalf("run returned before the final export: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("the final export never reached the collector")
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
		t.Fatal("the second signal did not cut the final metric export: run returned only once the collector answered")
	}
	text := logs.String()
	report := strings.Index(text, `"msg":"metric export failing"`)
	stopped := strings.Index(text, `"msg":"kaiak stopped"`)
	if report < 0 || stopped < report || !strings.Contains(text, `"exception.message":"export cut short`) {
		t.Errorf("want the cut export reported, cut short, before kaiak stopped:\n%s", text)
	}
}
