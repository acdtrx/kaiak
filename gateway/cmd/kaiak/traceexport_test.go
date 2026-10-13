package main

// OTLP trace export at the process level (docs/specs/GATEWAY.md, Observability →
// OTLP trace export; Request capture): its settings and the capture budget's, the
// endpoint on `kaiak starting`, its counts from the start, the exit order, and the
// warning for a config whose groups capture while the export is off.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"kaiak/internal/capture"
	"kaiak/internal/config"
	"kaiak/internal/provider"
	"kaiak/internal/telemetry/fakeotlp"
	"kaiak/internal/telemetry/otlp"
	"kaiak/internal/telemetry/otlpmetric"
	"kaiak/internal/telemetry/otlptrace"
)

// Trace export needs its explicit switch: the general endpoint alone, which turns
// logs and metrics on, leaves it off. Malformed trace settings fail the start naming
// the variable, never echoing a header's value.
func TestTraceExportSettings(t *testing.T) {
	const secret = "s3cr3t-token"
	env := func(extra map[string]string) func(string) (string, bool) {
		vars := map[string]string{"KAIAK_CONFIG_FILE": "c.json", "KAIAK_INSTANCE_ID": "i"}
		maps.Copy(vars, extra)
		return envOf(vars)
	}
	for _, tc := range []struct {
		name string
		vars map[string]string
		// want is the endpoint's host:port; "" means trace export is off.
		want string
	}{
		{"nothing set", nil, ""},
		{"the general endpoint alone", map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://collector:4318"}, ""},
		{"the traces endpoint", map[string]string{"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "https://langfuse.example.com/api/public/otel/v1/traces"},
			"langfuse.example.com:443"},
		{"otlp with the general endpoint", map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://collector:4318",
			"OTEL_TRACES_EXPORTER": "otlp"}, "collector:4318"},
		{"opted out", map[string]string{"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "http://collector:4318/v1/traces",
			"OTEL_TRACES_EXPORTER": "none"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := readSettings(env(tc.vars))
			switch {
			case err != nil:
				t.Fatalf("readSettings: %v", err)
			case tc.want == "" && s.traceExport != nil:
				t.Errorf("trace export on to %s, want off", s.traceExport.EndpointHost())
			case tc.want != "" && s.traceExport == nil:
				t.Errorf("trace export off, want on to %s", tc.want)
			case s.traceExport != nil && s.traceExport.EndpointHost() != tc.want:
				t.Errorf("trace export to %s, want %s", s.traceExport.EndpointHost(), tc.want)
			}
		})
	}
	for name, value := range map[string]string{
		"OTEL_TRACES_EXPORTER":               "jaeger",
		"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "ftp://collector",
		"OTEL_EXPORTER_OTLP_TRACES_HEADERS":  "authorization=" + secret + "%zz",
		"OTEL_EXPORTER_OTLP_TRACES_TIMEOUT":  "0",
		"OTEL_EXPORTER_OTLP_TRACES_PROTOCOL": "grpc",
	} {
		vars := map[string]string{"OTEL_TRACES_EXPORTER": "otlp"}
		vars[name] = value
		_, err := readSettings(env(vars))
		if err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("%s=%q: got %v, want a startup error naming it", name, value, err)
		} else if strings.Contains(err.Error(), secret) {
			t.Errorf("%s: error %q echoes a header value", name, err)
		}
	}
}

// KAIAK_CAPTURE_MEMORY_BYTES sets the capture budget, whole bytes above 0, 128 MiB
// unset — read, and a malformed value refused, whether or not trace export is on.
func TestCaptureMemorySetting(t *testing.T) {
	for name, traces := range map[string]map[string]string{
		"trace export off": nil,
		"trace export on":  {"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "http://collector:4318/v1/traces"},
	} {
		t.Run(name, func(t *testing.T) {
			with := func(value string) func(string) (string, bool) {
				vars := map[string]string{"KAIAK_CONFIG_FILE": "c.json", "KAIAK_INSTANCE_ID": "i"}
				maps.Copy(vars, traces)
				if value != "" {
					vars["KAIAK_CAPTURE_MEMORY_BYTES"] = value
				}
				return envOf(vars)
			}
			if s, err := readSettings(with("")); err != nil || s.captureMemory != 134217728 {
				t.Errorf("default %d, %v; want 134217728", s.captureMemory, err)
			}
			if s, err := readSettings(with("1048576")); err != nil || s.captureMemory != 1<<20 {
				t.Errorf("set %d, %v; want 1048576", s.captureMemory, err)
			}
			for _, bad := range []string{"0", "-1", "1.5", "128MiB", "99999999999999999999"} {
				if _, err := readSettings(with(bad)); err == nil || !strings.Contains(err.Error(), "KAIAK_CAPTURE_MEMORY_BYTES") {
					t.Errorf("KAIAK_CAPTURE_MEMORY_BYTES=%q: got %v, want an error naming it", bad, err)
				}
			}
		})
	}
}

// jsonLines decodes a JSON log's lines.
func jsonLines(t *testing.T, text string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for line := range strings.Lines(text) {
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		out = append(out, entry)
	}
	return out
}

// captureWarning is the warning written after an applied config whose groups
// capture while trace export is off.
const captureWarning = "groups capture requests but trace export is off: nothing is captured"

// The gateway starts with trace export on or off: `kaiak starting` names where
// captures go when it is on, and only then — the general endpoint alone, which turns
// logs and metrics on, leaves it off. Off, the startup config — whose one group
// captures — is followed by the warning.
func TestStartingLineNamesTheTraceExportEndpoint(t *testing.T) {
	collector := fakeotlp.New(t, nil)
	host := strings.TrimPrefix(collector.URL, "http://")
	for _, tc := range []struct {
		name string
		vars map[string]string
		want string
	}{
		{"off", nil, ""},
		{"the general endpoint alone", map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": collector.URL}, ""},
		{"on", map[string]string{"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": collector.URL + "/v1/traces"}, host},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vars := map[string]string{"KAIAK_CONFIG_FILE": minimalFixture, "KAIAK_INSTANCE_ID": "test-1",
				"KAIAK_LISTEN_ADDR": "127.0.0.1:0", "KAIAK_ADMIN_ADDR": "127.0.0.1:0"}
			maps.Copy(vars, tc.vars)
			var logs syncBuffer
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if err := run(ctx, slog.New(slog.NewJSONHandler(&logs, nil)), envOf(vars), make(chan os.Signal), make(chan os.Signal)); err != nil {
				t.Fatalf("run: %v", err)
			}
			lines := jsonLines(t, logs.String())
			starting := slices.IndexFunc(lines, func(l map[string]any) bool { return l["msg"] == "kaiak starting" })
			if starting < 0 {
				t.Fatalf("no kaiak starting line:\n%s", logs.String())
			}
			got, present := lines[starting]["kaiak.trace_export.endpoint"]
			switch {
			case tc.want == "" && present:
				t.Errorf("kaiak.trace_export.endpoint %v with trace export off, want absent", got)
			case tc.want != "" && got != tc.want:
				t.Errorf("kaiak.trace_export.endpoint %v, want %s", got, tc.want)
			}
			applied := slices.IndexFunc(lines, func(l map[string]any) bool { return l["msg"] == "config applied" })
			warning := slices.IndexFunc(lines, func(l map[string]any) bool { return l["msg"] == captureWarning })
			switch {
			case tc.want != "" && warning >= 0:
				t.Errorf("warning %q with trace export on:\n%s", captureWarning, logs.String())
			case tc.want == "" && (warning < applied || lines[warning]["level"] != "WARN" ||
				lines[warning]["kaiak.config.capture_groups"] != 1.0):
				t.Errorf("want the warning after config applied, naming 1 capturing group:\n%s", logs.String())
			}
		})
	}
}

// adminAddress waits for run's admin listener to bind and returns its address.
func adminAddress(t *testing.T, logs *syncBuffer) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for line := range strings.Lines(logs.String()) {
			var entry map[string]any
			if json.Unmarshal([]byte(line), &entry) == nil && entry["msg"] == "listening" && entry["kaiak.listener.name"] == "admin" {
				return net.JoinHostPort(fmt.Sprint(entry["server.address"]), fmt.Sprint(entry["server.port"]))
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("the admin listener never bound:\n%s", logs.String())
	return ""
}

// The trace export's own counts as a scrape names them.
const (
	spanProcessed = `otel_sdk_processor_span_processed_total{otel_component_type="capture_span_processor",otel_component_name="capture_span_processor/0"`
	spanExported  = `otel_sdk_exporter_span_exported_total{otel_component_type="otlp_http_json_span_exporter",otel_component_name="otlp_http_json_span_exporter/0"`
)

// exportedPoints are the points of the metric name in an export, by error.type ("" for
// none): their otel.component.type and otel.component.name must be component's.
func exportedPoints(t *testing.T, r fakeotlp.Received, name, component string) map[string]string {
	t.Helper()
	var points map[string]string
	for _, m := range r.Metrics() {
		if m.Name != name || m.Sum == nil {
			continue
		}
		points = map[string]string{}
		for _, p := range m.Sum.DataPoints {
			attrs := map[string]string{}
			for _, kv := range p.Attributes {
				if kv.Value.StringValue != nil {
					attrs[kv.Key] = *kv.Value.StringValue
				}
			}
			if attrs["otel.component.type"] != component || attrs["otel.component.name"] != component+"/0" || p.AsInt == nil {
				t.Errorf("%s point %v, want component %s and an integer", name, attrs, component)
				continue
			}
			points[attrs["error.type"]] = *p.AsInt
		}
	}
	return points
}

// With trace export on, its counts are on /metrics and in the OTLP metric export from
// the start, at 0 — the spans accepted and the processor's five — with no failure
// series; with it off there are none. Either way run returns leaving nothing
// running: the exporter shut down.
func TestTraceExportCountsFromTheStart(t *testing.T) {
	for _, on := range []bool{true, false} {
		t.Run(fmt.Sprintf("trace export on %v", on), func(t *testing.T) {
			collector := fakeotlp.New(t, nil)
			vars := map[string]string{"KAIAK_CONFIG_FILE": minimalFixture, "KAIAK_INSTANCE_ID": "test-1",
				"KAIAK_LISTEN_ADDR": "127.0.0.1:0", "KAIAK_ADMIN_ADDR": "127.0.0.1:0", "KAIAK_DRAIN_GRACE_MS": "0",
				"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT": collector.URL + "/v1/metrics"}
			if on {
				vars["OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"] = collector.URL + "/v1/traces"
			}
			var logs syncBuffer
			stop := make(chan os.Signal, 1)
			done := make(chan error, 1)
			go func() {
				done <- run(context.Background(), slog.New(slog.NewJSONHandler(&logs, nil)), envOf(vars), make(chan os.Signal), stop)
			}()
			resp, err := http.Get("http://" + adminAddress(t, &logs) + "/metrics")
			if err != nil {
				t.Fatal(err)
			}
			scrape, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil || resp.StatusCode != http.StatusOK {
				t.Fatalf("/metrics: %d, %v", resp.StatusCode, err)
			}
			stop <- syscall.SIGTERM
			if err := <-done; err != nil {
				t.Fatalf("run: %v", err)
			}
			if left := ourGoroutines(); len(left) > 0 {
				t.Errorf("goroutines left after run returned:\n%s", strings.Join(left, "\n\n"))
			}

			var series []string
			for line := range strings.Lines(string(scrape)) {
				if strings.HasPrefix(line, spanProcessed) || strings.HasPrefix(line, spanExported) {
					series = append(series, line)
				}
			}
			var want []string
			if on {
				want = []string{spanExported + "} 0\n", spanProcessed + "} 0\n", spanProcessed + `,error_type="_OTHER"} 0` + "\n",
					spanProcessed + `,error_type="memory"} 0` + "\n",
					spanProcessed + `,error_type="shutdown"} 0` + "\n", spanProcessed + `,error_type="too_large"} 0` + "\n"}
			}
			if !slices.Equal(series, want) {
				t.Errorf("/metrics holds\n%q\nwant\n%q", series, want)
			}

			var final []fakeotlp.Received
			for _, r := range collector.Received() {
				switch r.Path {
				case "/v1/metrics":
					final = append(final, r)
				default:
					t.Errorf("an export to %s: nothing is captured", r.Path)
				}
			}
			if len(final) != 1 {
				t.Fatalf("%d metric exports, want the final one alone", len(final))
			}
			processed := exportedPoints(t, final[0], "otel.sdk.processor.span.processed", "capture_span_processor")
			exported := exportedPoints(t, final[0], "otel.sdk.exporter.span.exported", "otlp_http_json_span_exporter")
			wantProcessed, wantExported := map[string]string(nil), map[string]string(nil)
			if on {
				wantProcessed = map[string]string{"": "0", "_OTHER": "0", "memory": "0", "too_large": "0", "shutdown": "0"}
				wantExported = map[string]string{"": "0"}
			}
			if !maps.Equal(processed, wantProcessed) || !maps.Equal(exported, wantExported) {
				t.Errorf("final export: processed %v, exported %v; want %v and %v", processed, exported, wantProcessed, wantExported)
			}
		})
	}
}

// exitExports starts the exports as run does, to collector — logs, metrics, and
// traces switched on — over a graph whose metrics count the spans, with one span
// queued. stderr is the process's stderr; the logger returned is the process logger.
func exitExports(t *testing.T, collector *fakeotlp.Collector, stderr io.Writer) (exports, *slog.Logger, chan struct{}) {
	t.Helper()
	env := envOf(map[string]string{"KAIAK_CONFIG_FILE": minimalFixture, "KAIAK_INSTANCE_ID": "test-1",
		"OTEL_EXPORTER_OTLP_ENDPOINT": collector.URL, "OTEL_TRACES_EXPORTER": "otlp", "OTEL_EXPORTER_OTLP_TIMEOUT": "20000"})
	s, err := readSettings(env)
	if err != nil {
		t.Fatal(err)
	}
	service := otlp.Service{Version: "test", InstanceID: s.instanceID}
	exp, logger := startExports(s, service, slog.New(slog.NewJSONHandler(stderr, nil)))
	g := newGraph(s, "test", env, logger, exp)
	exp.metrics = otlpmetric.New(s.metricExport, service, g.registry, logger)
	now := time.Now()
	encoded, err := otlptrace.Encode(otlptrace.Span{TraceID: otlptrace.NewTraceID(), SpanID: otlptrace.NewSpanID(),
		Name: "chat llama", Kind: otlptrace.KindClient, Start: now.Add(-time.Second), End: now,
		Attributes: []otlptrace.Attribute{otlptrace.String("kaiak.request.id", "exit-1")}})
	if err != nil {
		t.Fatal(err)
	}
	released := make(chan struct{})
	exp.traces.Enqueue(encoded, func() { close(released) })
	return exp, logger, released
}

// At exit the trace export's final flush comes first, within the drain's deadline: a
// span still queued reaches the collector — or is refused by it — before the final
// metric export, which counts it, and before the log export's final flush, which
// carries every line: the trace export's failure report is an ordinary line, written
// to the process logger, so it reaches stderr and the log export alike.
func TestExitFlushesTracesFirst(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		// counted is the error.type of the exporter series that counts the span.
		counted string
	}{
		{"accepted", http.StatusOK, ""},
		{"refused", http.StatusBadRequest, "400"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			collector := fakeotlp.New(t, fakeotlp.AnswerStatus(func(_ int, r *http.Request) int {
				if r.URL.Path == "/v1/traces" {
					return tc.status
				}
				return http.StatusOK
			}))
			var stderr syncBuffer
			exp, logger, released := exitExports(t, collector, &stderr)
			start := time.Now()
			exp.finish(context.Background(), start.Add(10*time.Second), func() { logger.Info("kaiak stopped") })
			if took := time.Since(start); took > 5*time.Second {
				t.Errorf("exit took %s", took)
			}
			select {
			case <-released:
			default:
				t.Error("the span's bytes were not given back")
			}

			exports := collector.Received()
			to := func(path string) func(fakeotlp.Received) bool {
				return func(r fakeotlp.Received) bool { return r.Path == path }
			}
			traces := slices.IndexFunc(exports, to("/v1/traces"))
			metricsAt := slices.IndexFunc(exports, to("/v1/metrics"))
			stopped := slices.IndexFunc(exports, func(r fakeotlp.Received) bool { return slices.Contains(r.Messages(), "kaiak stopped") })
			if traces < 0 || metricsAt < 0 || stopped < 0 || !(traces < metricsAt && metricsAt < stopped) {
				t.Fatalf("exports in the order traces %d, metrics %d, the last line's logs %d; want them in that order", traces,
					metricsAt, stopped)
			}
			if spans := exports[traces].Spans(); len(spans) != 1 || spans[0].Attrs(t)["kaiak.request.id"] != "exit-1" {
				t.Errorf("trace export holds %d spans, want the queued one", len(spans))
			}
			processed := exportedPoints(t, exports[metricsAt], "otel.sdk.processor.span.processed", "capture_span_processor")
			exported := exportedPoints(t, exports[metricsAt], "otel.sdk.exporter.span.exported", "otlp_http_json_span_exporter")
			if processed[""] != "1" || exported[tc.counted] != "1" {
				t.Errorf("final metric export: processed %v, exported %v; want the span handed over and counted under %q",
					processed, exported, tc.counted)
			}

			reported := strings.Contains(stderr.String(), `"msg":"trace export failing"`)
			var exportedLines []string
			for _, r := range exports {
				exportedLines = append(exportedLines, r.Messages()...)
			}
			if want := tc.status != http.StatusOK; reported != want || slices.Contains(exportedLines, "trace export failing") != want {
				t.Errorf("trace export failing on stderr %v, in the log export %v; want both %v:\n%s", reported,
					slices.Contains(exportedLines, "trace export failing"), want, stderr.String())
			}
		})
	}
}

// exitCaptures starts the exports as run does, to collector — logs, metrics and
// traces switched on — with the captures over the trace exporter, wrapped by wrap
// when it is set, and a graph whose metrics count them.
func exitCaptures(t *testing.T, collector *fakeotlp.Collector, stderr io.Writer,
	wrap func(capture.Exporter) capture.Exporter) (exports, *slog.Logger) {
	t.Helper()
	env := envOf(map[string]string{"KAIAK_CONFIG_FILE": minimalFixture, "KAIAK_INSTANCE_ID": "test-1",
		"OTEL_EXPORTER_OTLP_ENDPOINT": collector.URL, "OTEL_TRACES_EXPORTER": "otlp"})
	s, err := readSettings(env)
	if err != nil {
		t.Fatal(err)
	}
	service := otlp.Service{Version: "test", InstanceID: s.instanceID}
	exp, logger := startExports(s, service, slog.New(slog.NewJSONHandler(stderr, nil)))
	if wrap != nil {
		exp.captures.Finish(context.Background()) // nothing handed: it stops at once
		exp.captures = capture.New(s.captureMemory, wrap(exp.traces), logger)
	}
	g := newGraph(s, "test", env, logger, exp)
	exp.metrics = otlpmetric.New(s.metricExport, service, g.registry, logger)
	return exp, logger
}

// handCapture hands a capture of a finished chat request, id, to the builder, as
// the capture stage's finisher does; it returns the bytes the capture holds.
func handCapture(t *testing.T, captures *capture.Captures, id string) int64 {
	t.Helper()
	body := []byte(`{"model":"llama","messages":[]}`)
	c := captures.Start(provider.ChatCompletions, body, nil)
	now := time.Now()
	if got := c.Hand(capture.Summary{Start: now.Add(-time.Second), End: now, RequestID: id, KeyID: "k-1", Group: "me",
		RootGroup: "me", Model: "llama", Status: http.StatusOK}); got != capture.OutcomeQueued {
		t.Fatalf("capture %s: %s, want queued", id, got)
	}
	return int64(len(body))
}

// At exit the captures the drained requests handed to the builder are built before
// the trace export's final flush, which sends them — before the final metric export,
// which counts them handed and exported — and every byte they held goes back.
func TestExitBuildsCapturesBeforeTheTraceFlush(t *testing.T) {
	collector := fakeotlp.New(t, nil)
	var stderr syncBuffer
	exp, logger := exitCaptures(t, collector, &stderr, nil)
	for _, id := range []string{"exit-1", "exit-2"} {
		handCapture(t, exp.captures, id)
	}
	start := time.Now()
	exp.finish(context.Background(), start.Add(10*time.Second), func() { logger.Info("kaiak stopped") })
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("exit took %s", took)
	}
	exports := collector.Received()
	var ids []any
	traces, metricsAt := -1, -1
	for i, r := range exports {
		switch r.Path {
		case "/v1/traces":
			traces = i
			for _, span := range r.Spans() {
				ids = append(ids, span.Attrs(t)["kaiak.request.id"])
			}
		case "/v1/metrics":
			metricsAt = i
		}
	}
	if !slices.Equal(ids, []any{"exit-1", "exit-2"}) || traces < 0 || traces > metricsAt {
		t.Fatalf("spans %v exported at %d, the metrics at %d; want both captures before the final metric export", ids,
			traces, metricsAt)
	}
	processed := exportedPoints(t, exports[metricsAt], "otel.sdk.processor.span.processed", "capture_span_processor")
	exported := exportedPoints(t, exports[metricsAt], "otel.sdk.exporter.span.exported", "otlp_http_json_span_exporter")
	if processed[""] != "2" || processed["shutdown"] != "0" || exported[""] != "2" {
		t.Errorf("final metric export: processed %v, exported %v; want both handed and exported", processed, exported)
	}
	if held := exp.captures.InUse(); held != 0 {
		t.Errorf("captures hold %d bytes, want 0", held)
	}
}

// gatedExporter holds each build at the exporter until gate is closed, telling
// entered first: the builder stands still.
type gatedExporter struct {
	capture.Exporter
	gate, entered chan struct{}
}

func (e gatedExporter) MaxSpanBytes() int {
	e.entered <- struct{}{}
	<-e.gate
	return e.Exporter.MaxSpanBytes()
}

// A builder still busy when the exit's bound ends — the drain's deadline, at least
// 1 s — has the captures it did not reach dropped and counted as shutdown, their
// bytes given back; the build in progress completes, and its span is handed to the
// trace export's last work.
func TestExitDropsCapturesNotBuiltAtTheBound(t *testing.T) {
	collector := fakeotlp.New(t, nil)
	var stderr syncBuffer
	gate, entered := make(chan struct{}), make(chan struct{}, 1)
	exp, logger := exitCaptures(t, collector, &stderr, func(e capture.Exporter) capture.Exporter {
		return gatedExporter{Exporter: e, gate: gate, entered: entered}
	})
	building := handCapture(t, exp.captures, "exit-1")
	<-entered
	handCapture(t, exp.captures, "exit-2")
	finished := make(chan struct{})
	start := time.Now()
	go func() {
		exp.finish(context.Background(), start, func() { logger.Info("kaiak stopped") })
		close(finished)
	}()
	deadline := time.After(exitExportFloor + 5*time.Second)
	for exp.captures.Drops().Shutdown != 1 {
		select {
		case <-deadline:
			close(gate)
			t.Fatalf("drops %+v, want exit-2 dropped at the bound", exp.captures.Drops())
		case <-time.After(10 * time.Millisecond):
		}
	}
	if took := time.Since(start); took < exitExportFloor {
		t.Errorf("exit-2 dropped after %s, before the bound's 1 s floor", took)
	}
	if held := exp.captures.InUse(); held != building {
		t.Errorf("captures hold %d bytes at the bound, want exit-1's %d alone", held, building)
	}
	close(gate)
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("exit still waiting once the build ended")
	}
	if held := exp.captures.InUse(); held != 0 {
		t.Errorf("captures hold %d bytes after exit, want 0", held)
	}
	var metricExport fakeotlp.Received
	for _, r := range collector.Received() {
		if r.Path == "/v1/metrics" {
			metricExport = r
		}
	}
	// exit-1's span, built after the bound, is exported, cut or dropped by the trace
	// export's Shutdown: counted either way.
	processed := exportedPoints(t, metricExport, "otel.sdk.processor.span.processed", "capture_span_processor")
	if shutdown, _ := strconv.Atoi(processed["shutdown"]); shutdown < 1 {
		t.Errorf("final metric export: processed %v, want exit-2 counted as shutdown", processed)
	}
}

// A collector that never answers the trace export holds the final flush only to its
// bound — the drain's deadline, at least 1 s: the export cut short counts failed and
// is reported, and the final metric export and the log flush still go.
func TestExitTraceFlushIsBounded(t *testing.T) {
	collector := fakeotlp.New(t, fakeotlp.AnswerStatus(func(_ int, r *http.Request) int {
		if r.URL.Path == "/v1/traces" {
			<-r.Context().Done()
			return http.StatusServiceUnavailable
		}
		return http.StatusOK
	}))
	var stderr syncBuffer
	exp, logger, released := exitExports(t, collector, &stderr)
	start := time.Now()
	exp.finish(context.Background(), start, func() { logger.Info("kaiak stopped") })
	// The trace flush's 1 s floor, plus room for the other exports and scheduling.
	if took := time.Since(start); took < exitExportFloor || took > 3*time.Second {
		t.Errorf("exit took %s with a stalled trace collector, want about %s", took, exitExportFloor)
	}
	select {
	case <-released:
	default:
		t.Error("the span's bytes were not given back")
	}
	var metricExport fakeotlp.Received
	var lines []string
	for _, r := range collector.Received() {
		if r.Path == "/v1/metrics" {
			metricExport = r
		}
		lines = append(lines, r.Messages()...)
	}
	if exported := exportedPoints(t, metricExport, "otel.sdk.exporter.span.exported", "otlp_http_json_span_exporter"); exported["timeout"] != "1" {
		t.Errorf("final metric export: exported %v, want the span failed as timeout", exported)
	}
	if !slices.Contains(lines, "trace export failing") || !slices.Contains(lines, "kaiak stopped") {
		t.Errorf("log export carried %q, want the trace failure report and the last line", lines)
	}
	if !strings.Contains(stderr.String(), `"exception.message":"export cut short`) {
		t.Errorf("want the cut export reported, cut short:\n%s", stderr.String())
	}
}

// Each applied config with a capturing group is followed by one warning naming how
// many groups capture, while trace export is off — and only then: not with trace
// export on, not for a config whose groups do not capture, not for a rejected one.
func TestCaptureWithTraceExportOffIsWarned(t *testing.T) {
	minimal, err := os.ReadFile(minimalFixture)
	if err != nil {
		t.Fatal(err)
	}
	// withGroups is the minimal config with its groups' capture flags set, and more
	// top-level groups.
	withGroups := func(capture map[string]bool) []byte {
		var doc map[string]any
		if err := json.Unmarshal(minimal, &doc); err != nil {
			t.Fatal(err)
		}
		groups := doc["groups"].(map[string]any)
		for id, c := range capture {
			groups[id] = map[string]any{"capture": c}
		}
		data, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	for _, tc := range []struct {
		name   string
		traces bool
		config []byte
		// want is kaiak.config.capture_groups on the warning; 0 means no warning.
		want float64
	}{
		{"one capturing group", false, minimal, 1},
		{"three of four", false, withGroups(map[string]bool{"a": true, "b": true, "c": false}), 3},
		{"no capturing group", false, withGroups(map[string]bool{"me": false}), 0},
		{"trace export on", true, minimal, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vars := map[string]string{"KAIAK_CONFIG_FILE": minimalFixture, "KAIAK_INSTANCE_ID": "test-1"}
			if tc.traces {
				vars["OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"] = "http://collector:4318/v1/traces"
			}
			s, err := readSettings(envOf(vars))
			if err != nil {
				t.Fatal(err)
			}
			var logs syncBuffer
			g := newGraph(s, "test", envOf(nil), slog.New(slog.NewJSONHandler(&logs, nil)), exports{})
			for range 2 {
				if _, err := g.applier.Apply(config.TriggerSIGHUP, tc.config); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := g.applier.Apply(config.TriggerSIGHUP, []byte(`{"protocol_version": 8}`)); err == nil {
				t.Fatal("an incomplete config applied")
			}
			// Each applied config's line, then its warning when one is due.
			var got []string
			for _, l := range jsonLines(t, logs.String()) {
				switch l["msg"] {
				case "config applied", "config rejected":
					got = append(got, l["msg"].(string))
				case captureWarning:
					got = append(got, fmt.Sprintf("%s %v %v", l["level"], l["msg"], l["kaiak.config.capture_groups"]))
				}
			}
			want := []string{"config applied", "config applied", "config rejected"}
			if tc.want > 0 {
				warning := fmt.Sprintf("WARN %s %v", captureWarning, tc.want)
				want = []string{"config applied", warning, "config applied", warning, "config rejected"}
			}
			if !slices.Equal(got, want) {
				t.Errorf("lines %q, want %q", got, want)
			}
		})
	}
}
