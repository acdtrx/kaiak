package e2e

// OTLP log export through the built binary (docs/specs/GATEWAY.md, Observability →
// OTLP log export): a fake collector receives exactly the lines stderr shows, with
// the resource; the metric follows; problems stay on stderr; nothing goes out when
// export is off.

import (
	"encoding/json"
	"fmt"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"kaiak/internal/fakebackend"
)

// otlpExport is an ExportLogsServiceRequest as OTLP/HTTP JSON writes it.
type otlpExport struct {
	ResourceLogs []struct {
		Resource struct {
			Attributes []otlpKeyValue `json:"attributes"`
		} `json:"resource"`
		ScopeLogs []struct {
			Scope struct {
				Name string `json:"name"`
			} `json:"scope"`
			LogRecords []otlpRecord `json:"logRecords"`
		} `json:"scopeLogs"`
	} `json:"resourceLogs"`
}

type otlpRecord struct {
	TimeUnixNano         string         `json:"timeUnixNano"`
	ObservedTimeUnixNano string         `json:"observedTimeUnixNano"`
	SeverityNumber       int            `json:"severityNumber"`
	SeverityText         string         `json:"severityText"`
	Body                 otlpValue      `json:"body"`
	Attributes           []otlpKeyValue `json:"attributes"`
}

type otlpKeyValue struct {
	Key   string    `json:"key"`
	Value otlpValue `json:"value"`
}

type otlpValue struct {
	StringValue *string  `json:"stringValue"`
	IntValue    *string  `json:"intValue"`
	DoubleValue *float64 `json:"doubleValue"`
	BoolValue   *bool    `json:"boolValue"`
	ArrayValue  *struct {
		Values []otlpValue `json:"values"`
	} `json:"arrayValue"`
}

// plain is the value as the JSON log line holds it once decoded: numbers are
// float64, arrays []any.
func (v otlpValue) plain(t *testing.T) any {
	t.Helper()
	switch {
	case v.StringValue != nil:
		return *v.StringValue
	case v.IntValue != nil:
		n, err := strconv.ParseInt(*v.IntValue, 10, 64)
		if err != nil {
			t.Errorf("intValue %q: %v", *v.IntValue, err)
		}
		return float64(n)
	case v.DoubleValue != nil:
		return *v.DoubleValue
	case v.BoolValue != nil:
		return *v.BoolValue
	case v.ArrayValue != nil:
		out := []any{}
		for _, e := range v.ArrayValue.Values {
			out = append(out, e.plain(t))
		}
		return out
	}
	t.Errorf("a value of no known kind")
	return nil
}

// severities are the OTLP severity numbers of the levels the gateway writes.
var severities = map[string]int{"DEBUG": 5, "INFO": 9, "WARN": 13, "ERROR": 17}

// line is the record in the shape of a decoded JSON log line, its time in Unix
// nanoseconds.
func (r otlpRecord) line(t *testing.T) map[string]any {
	t.Helper()
	nanos, err := strconv.ParseInt(r.TimeUnixNano, 10, 64)
	if err != nil {
		t.Errorf("timeUnixNano %q: %v", r.TimeUnixNano, err)
	}
	if r.ObservedTimeUnixNano != r.TimeUnixNano {
		t.Errorf("observedTimeUnixNano %s, timeUnixNano %s: want the same", r.ObservedTimeUnixNano, r.TimeUnixNano)
	}
	if severities[r.SeverityText] != r.SeverityNumber {
		t.Errorf("severity %s with number %d", r.SeverityText, r.SeverityNumber)
	}
	out := map[string]any{"time": nanos, "level": r.SeverityText, "msg": r.Body.plain(t)}
	for _, kv := range r.Attributes {
		if _, dup := out[kv.Key]; dup {
			t.Errorf("attribute %s twice", kv.Key)
		}
		out[kv.Key] = kv.Value.plain(t)
	}
	return out
}

// stderrLine is a decoded JSON log line with its time in Unix nanoseconds, to compare
// with a record.
func stderrLine(t *testing.T, entry map[string]any) map[string]any {
	t.Helper()
	out := maps.Clone(entry)
	at, err := time.Parse(time.RFC3339Nano, fmt.Sprint(entry["time"]))
	if err != nil {
		t.Errorf("log line time %v: %v", entry["time"], err)
	}
	out["time"] = at.UnixNano()
	return out
}

// canonical is a line's JSON, keys sorted: lines compare by it.
func canonical(t *testing.T, line map[string]any) string {
	t.Helper()
	data, err := json.Marshal(line)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// otlpCollector is a fake OpenTelemetry collector: it answers each export with the
// status answer gives (200 when nil) and keeps what it received.
type otlpCollector struct {
	*httptest.Server
	// answer gives the status for the nth export (from 0); it may block, until the
	// request's context ends.
	answer func(n int, r *http.Request) int

	mu       sync.Mutex
	exports  int
	accepted []otlpExport
	refused  []otlpExport
	requests []*http.Request
	arrived  chan struct{} // gets a value on every export, without waiting
}

func newOTLPCollector(t *testing.T, answer func(n int, r *http.Request) int) *otlpCollector {
	t.Helper()
	c := &otlpCollector{answer: answer, arrived: make(chan struct{}, 1)}
	c.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var export otlpExport
		if err := json.NewDecoder(r.Body).Decode(&export); err != nil {
			t.Errorf("collector: export not decoded: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		c.mu.Lock()
		n := c.exports
		c.exports++
		c.requests = append(c.requests, r)
		c.mu.Unlock()
		select {
		case c.arrived <- struct{}{}:
		default:
		}
		status := http.StatusOK
		if c.answer != nil {
			status = c.answer(n, r)
		}
		c.mu.Lock()
		if status == http.StatusOK {
			c.accepted = append(c.accepted, export)
		} else {
			c.refused = append(c.refused, export)
		}
		c.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if status == http.StatusOK {
			_, _ = w.Write([]byte(`{}`))
			return
		}
		_, _ = w.Write([]byte(`{"code": 3, "message": "export refused by the test"}`))
	}))
	t.Cleanup(c.Close)
	return c
}

// records returns the records of the accepted (or refused) exports, in order, and
// checks each export's resource and scope.
func (c *otlpCollector) records(t *testing.T, accepted bool, resource map[string]any) []otlpRecord {
	t.Helper()
	c.mu.Lock()
	exports := slices.Clone(c.refused)
	if accepted {
		exports = slices.Clone(c.accepted)
	}
	c.mu.Unlock()
	var out []otlpRecord
	for _, e := range exports {
		if len(e.ResourceLogs) != 1 || len(e.ResourceLogs[0].ScopeLogs) != 1 {
			t.Fatalf("export with %d resources, want one resource and one scope", len(e.ResourceLogs))
		}
		got := map[string]any{}
		for _, kv := range e.ResourceLogs[0].Resource.Attributes {
			got[kv.Key] = kv.Value.plain(t)
		}
		if !maps.Equal(got, resource) {
			t.Errorf("resource %v, want %v", got, resource)
		}
		if name := e.ResourceLogs[0].ScopeLogs[0].Scope.Name; name != "kaiak" {
			t.Errorf("scope %q, want kaiak", name)
		}
		out = append(out, e.ResourceLogs[0].ScopeLogs[0].LogRecords...)
	}
	return out
}

// count is the number of records accepted so far.
func (c *otlpCollector) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, e := range c.accepted {
		for _, rl := range e.ResourceLogs {
			for _, sl := range rl.ScopeLogs {
				n += len(sl.LogRecords)
			}
		}
	}
	return n
}

// collectorHost is the collector's host:port as kaiak.log_export.endpoint names it.
func collectorHost(t *testing.T, c *otlpCollector) string {
	t.Helper()
	u, err := url.Parse(c.URL)
	if err != nil {
		t.Fatal(err)
	}
	return net.JoinHostPort(u.Hostname(), u.Port())
}

// buildVersion is the version kaiak_build_info reports.
func buildVersion(t *testing.T, g *gateway) string {
	t.Helper()
	_, body := g.get(t, "/metrics", "")
	for line := range strings.SplitSeq(string(body), "\n") {
		if rest, ok := strings.CutPrefix(line, `kaiak_build_info{version="`); ok {
			v, _, _ := strings.Cut(rest, `"`)
			return v
		}
	}
	t.Fatal("kaiak_build_info not exposed")
	return ""
}

// sameLines fails unless want and got hold the same lines, each as many times —
// order aside: two goroutines logging at once may queue and write in other orders.
func sameLines(t *testing.T, what string, want, got []map[string]any) {
	t.Helper()
	counts := map[string]int{}
	for _, l := range want {
		counts[canonical(t, l)]++
	}
	for _, l := range got {
		counts[canonical(t, l)]--
	}
	for line, n := range counts {
		switch {
		case n > 0:
			t.Errorf("%s: on stderr, not exported (%d×): %s", what, n, line)
		case n < 0:
			t.Errorf("%s: exported, not on stderr (%d×): %s", what, -n, line)
		}
	}
}

// otlpLines are the records as decoded log lines.
func otlpLines(t *testing.T, records []otlpRecord) []map[string]any {
	t.Helper()
	out := make([]map[string]any, len(records))
	for i, r := range records {
		out[i] = r.line(t)
	}
	return out
}

// stderrLines are the gateway's JSON log lines, but those with message skip, in the
// shape otlpLines gives.
func stderrLines(t *testing.T, g *gateway, skip string) []map[string]any {
	t.Helper()
	g.logs.mu.Lock()
	lines := slices.Clone(g.logs.lines)
	g.logs.mu.Unlock()
	var out []map[string]any
	for _, l := range lines {
		if l["msg"] != skip {
			out = append(out, stderrLine(t, l))
		}
	}
	return out
}

func logExportSeries(outcome string) string {
	return fmt.Sprintf(`kaiak_log_export_records_total{outcome=%q}`, outcome)
}

// Every line of a gateway's life — boot, a success, a 401, a limit refusal, the
// drain, `kaiak stopped` — reaches the collector with stderr's message, level, time
// and attributes, under the resource; headers go out and are never logged; the
// metric counts the exports.
func TestLogExport(t *testing.T) {
	backend := fakebackend.New()
	defer backend.Close()
	dir := t.TempDir()
	configFile := filepath.Join(dir, "config.json")
	evalKey, evalHash := newKey()
	_, annHash := newKey()
	writeJSON(t, configFile, testConfig(backend.URL(), evalHash, annHash, ""))
	const secret = "s3cret-otlp-header"
	collector := newOTLPCollector(t, func(_ int, r *http.Request) int {
		if r.URL.Path != "/base/v1/logs" || r.Header.Get("Authorization") != "Bearer "+secret ||
			r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("export to %s with Authorization %q, Content-Type %q", r.URL.Path,
				r.Header.Get("Authorization"), r.Header.Get("Content-Type"))
		}
		return http.StatusOK
	})
	g := startGatewayEnv(t, append(gatewayEnv(configFile, ""),
		"OTEL_EXPORTER_OTLP_ENDPOINT="+collector.URL+"/base",
		"OTEL_EXPORTER_OTLP_HEADERS=Authorization=Bearer%20"+secret,
		"OTEL_RESOURCE_ATTRIBUTES=deployment.environment.name=e2e,service.instance.id=not-this-one"))

	starting := g.logs.wait(t, "kaiak starting", msg("kaiak starting"))
	if got, want := starting["kaiak.log_export.endpoint"], collectorHost(t, collector); got != want {
		t.Errorf("kaiak.log_export.endpoint %v, want %s", got, want)
	}
	for _, outcome := range []string{"failed", "dropped"} {
		if v, ok := g.metricValue(t, logExportSeries(outcome)); !ok || v != 0 {
			t.Errorf("%s = %v (exposed %v), want 0", logExportSeries(outcome), v, ok)
		}
	}

	if r := g.post(t, "/v1/chat/completions", evalKey, "otlp-ok", chatBody("chat", false, nil)); r.StatusCode != http.StatusOK {
		t.Fatalf("chat: %d %s", r.StatusCode, r.body)
	}
	if r := g.post(t, "/v1/chat/completions", "", "otlp-401", chatBody("chat", false, nil)); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no key: %d %s", r.StatusCode, r.body)
	}
	for i := range 3 {
		r := g.post(t, "/v1/chat/completions", evalKey, fmt.Sprintf("otlp-rpm-%d", i), chatBody("rpm", false, nil))
		if want := []int{200, 200, 429}[i]; r.StatusCode != want {
			t.Fatalf("rpm request %d: %d %s, want %d", i, r.StatusCode, r.body, want)
		}
	}
	for _, id := range []string{"otlp-ok", "otlp-401", "otlp-rpm-2"} {
		g.settled(t, id)
	}
	// The metric moves with the exports: once the request lines are out, it counts
	// every record the collector accepted.
	exported := g.waitMetric(t, "exports counted", logExportSeries("exported"), func(v float64) bool {
		return v > 0 && v == float64(collector.count())
	})
	version := buildVersion(t, g)
	g.stop(t)

	resource := map[string]any{"service.name": "kaiak", "service.version": version, "service.instance.id": "e2e",
		"deployment.environment.name": "e2e"}
	records := otlpLines(t, collector.records(t, true, resource))
	if n := len(collector.records(t, false, resource)); n != 0 {
		t.Errorf("%d records refused, want none", n)
	}
	if len(records) <= int(exported) {
		t.Errorf("%d records exported in all, %v before the stop: the drain's lines are missing", len(records), exported)
	}
	sameLines(t, "every line", stderrLines(t, g, ""), records)

	find := func(match func(map[string]any) bool) map[string]any {
		for _, r := range records {
			if match(r) {
				return r
			}
		}
		return nil
	}
	for _, c := range []struct {
		what  string
		match func(map[string]any) bool
		want  map[string]any
	}{
		{"kaiak starting", msg("kaiak starting"), map[string]any{"service.instance.id": "e2e"}},
		{"config applied", msg("config applied", "kaiak.trigger", "startup"), nil},
		{"the API listener", msg("listening", "kaiak.listener.name", "api"), nil},
		{"the success", msg("request", "kaiak.request.id", "otlp-ok"), map[string]any{
			"http.response.status_code": 200.0, "kaiak.key.id": "k-eval", "gen_ai.usage.output_tokens": 4.0,
			"kaiak.usage.estimated": false}},
		{"the 401", msg("request", "kaiak.request.id", "otlp-401"), map[string]any{
			"http.response.status_code": 401.0, "kaiak.auth.failure": "missing_key", "error.type": "missing_api_key"}},
		{"the limit refusal", msg("request", "kaiak.request.id", "otlp-rpm-2"), map[string]any{
			"http.response.status_code": 429.0, "error.type": "rate_limit_exceeded", "kaiak.limit.type": "requests_per_minute",
			"kaiak.limit.id": "eval", "kaiak.limit.configured": 2.0}},
		{"kaiak stopped", msg("kaiak stopped"), nil},
	} {
		r := find(c.match)
		if r == nil {
			t.Errorf("%s not exported", c.what)
			continue
		}
		for k, v := range c.want {
			if r[k] != v {
				t.Errorf("%s: %s = %v, want %v", c.what, k, r[k], v)
			}
		}
	}
	if text := g.logs.text(); strings.Contains(text, secret) || strings.Contains(text, collector.URL+"/base") {
		t.Error("the stderr log holds the header value or the endpoint URL")
	}
}

// With no endpoint, or with OTEL_LOGS_EXPORTER=none beside one, nothing is exported:
// no export reaches the collector, no metric series, no endpoint on `kaiak starting`.
func TestLogExportOff(t *testing.T) {
	backend := fakebackend.New()
	defer backend.Close()
	dir := t.TempDir()
	configFile := filepath.Join(dir, "config.json")
	evalKey, evalHash := newKey()
	_, annHash := newKey()
	writeJSON(t, configFile, testConfig(backend.URL(), evalHash, annHash, ""))
	collector := newOTLPCollector(t, nil)
	for name, env := range map[string][]string{
		"no endpoint": {"OTEL_SERVICE_NAME=kaiak-e2e", "OTEL_EXPORTER_OTLP_PROTOCOL=grpc"},
		"opted out":   {"OTEL_EXPORTER_OTLP_ENDPOINT=" + collector.URL, "OTEL_LOGS_EXPORTER=none"},
	} {
		t.Run(name, func(t *testing.T) {
			g := startGatewayEnv(t, append(gatewayEnv(configFile, ""), env...))
			if r := g.post(t, "/v1/chat/completions", evalKey, "off-ok", chatBody("chat", false, nil)); r.StatusCode != http.StatusOK {
				t.Fatalf("chat: %d %s", r.StatusCode, r.body)
			}
			g.settled(t, "off-ok")
			if _, ok := g.metricValue(t, logExportSeries("exported")); ok {
				t.Error("kaiak_log_export_records_total exposed with export off")
			}
			if line := g.logs.wait(t, "kaiak starting", msg("kaiak starting")); line["kaiak.log_export.endpoint"] != nil {
				t.Errorf("kaiak starting names an endpoint with export off: %v", line)
			}
			g.stop(t)
			collector.mu.Lock()
			n := collector.exports
			collector.mu.Unlock()
			if n != 0 {
				t.Errorf("collector received %d exports with export off", n)
			}
		})
	}
}

// A refused batch is counted failed and reported on stderr only — the report is never
// exported — and the rest still goes: refused and accepted records together are
// exactly stderr's other lines.
func TestLogExportRefusedBatch(t *testing.T) {
	backend := fakebackend.New()
	defer backend.Close()
	dir := t.TempDir()
	configFile := filepath.Join(dir, "config.json")
	evalKey, evalHash := newKey()
	_, annHash := newKey()
	writeJSON(t, configFile, testConfig(backend.URL(), evalHash, annHash, ""))
	collector := newOTLPCollector(t, func(n int, _ *http.Request) int {
		if n == 0 {
			return http.StatusBadRequest
		}
		return http.StatusOK
	})
	g := startGatewayEnv(t, append(gatewayEnv(configFile, ""), "OTEL_EXPORTER_OTLP_LOGS_ENDPOINT="+collector.URL+"/v1/logs"))

	report := g.logs.wait(t, "the failure report", msg("log export failing"))
	if report["http.response.status_code"] != 400.0 || report["kaiak.log_export.dropped"] != 0.0 ||
		!strings.Contains(fmt.Sprint(report["exception.message"]), "export refused by the test") {
		t.Errorf("failure report %v", report)
	}
	failed := report["kaiak.log_export.failed"].(float64)
	if got := g.metric(t, logExportSeries("failed")); got != failed || failed == 0 {
		t.Errorf("failed records: metric %v, report %v", got, failed)
	}
	if r := g.post(t, "/v1/chat/completions", evalKey, "refused-ok", chatBody("chat", false, nil)); r.StatusCode != http.StatusOK {
		t.Fatalf("chat: %d %s", r.StatusCode, r.body)
	}
	g.settled(t, "refused-ok")
	g.waitMetric(t, "exports counted", logExportSeries("exported"), func(v float64) bool {
		return v > 0 && v == float64(collector.count())
	})
	g.stop(t)

	resource := map[string]any{"service.name": "kaiak", "service.version": buildVersionOf(t, collector),
		"service.instance.id": "e2e"}
	refused := collector.records(t, false, resource)
	if len(refused) != int(failed) {
		t.Errorf("collector refused %d records, the report says %v", len(refused), failed)
	}
	all := append(otlpLines(t, refused), otlpLines(t, collector.records(t, true, resource))...)
	sameLines(t, "refused and accepted", stderrLines(t, g, "log export failing"), all)
}

// buildVersionOf is the service.version of the collector's first export.
func buildVersionOf(t *testing.T, c *otlpCollector) string {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	exports := append(slices.Clone(c.refused), c.accepted...)
	if len(exports) == 0 || len(exports[0].ResourceLogs) == 0 {
		t.Fatal("no export")
	}
	for _, kv := range exports[0].ResourceLogs[0].Resource.Attributes {
		if kv.Key == "service.version" && kv.Value.StringValue != nil {
			return *kv.Value.StringValue
		}
	}
	t.Fatal("no service.version in the resource")
	return ""
}

// A collector that never answers holds one export; at exit the final flush gives up
// at its bound — the drain's deadline, at least 1 s — and the export in flight counts
// as failed, which stderr reports. What is still queued is dropped and counted; its
// report falls within the once-a-minute limit of the one just written.
func TestLogExportStalledCollectorAtExit(t *testing.T) {
	backend := fakebackend.New()
	defer backend.Close()
	dir := t.TempDir()
	configFile := filepath.Join(dir, "config.json")
	evalKey, evalHash := newKey()
	_, annHash := newKey()
	writeJSON(t, configFile, testConfig(backend.URL(), evalHash, annHash, ""))
	collector := newOTLPCollector(t, func(_ int, r *http.Request) int {
		<-r.Context().Done()
		return http.StatusServiceUnavailable
	})
	g := startGatewayEnv(t, append(gatewayEnv(configFile, ""),
		"OTEL_EXPORTER_OTLP_ENDPOINT="+collector.URL,
		"OTEL_EXPORTER_OTLP_TIMEOUT=600000",
		"KAIAK_DRAIN_TIMEOUT_MS=500"))
	select { // the first batch is held from here on
	case <-collector.arrived:
	case <-time.After(waitLimit):
		t.Fatalf("no export within %s", waitLimit)
	}
	if r := g.post(t, "/v1/chat/completions", evalKey, "stalled-ok", chatBody("chat", false, nil)); r.StatusCode != http.StatusOK {
		t.Fatalf("chat: %d %s", r.StatusCode, r.body)
	}
	g.settled(t, "stalled-ok")
	if v := g.metric(t, logExportSeries("exported")); v != 0 {
		t.Errorf("exported %v with a collector that never answers", v)
	}

	stopped := time.Now()
	g.signal(t, syscall.SIGTERM)
	g.waitExit(t)
	if took := time.Since(stopped); took > 5*time.Second {
		t.Errorf("exit took %s with a stalled collector, want about 1 s", took)
	}
	report := g.logs.wait(t, "the failure report", msg("log export failing"))
	if report["kaiak.log_export.failed"].(float64) == 0 || !strings.Contains(fmt.Sprint(report["exception.message"]), "cut short") {
		t.Errorf("failure report %v: want the held batch failed, cut short", report)
	}
	if n := collector.count(); n != 0 {
		t.Errorf("collector accepted %d records", n)
	}
}
