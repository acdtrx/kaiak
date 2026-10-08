package e2e

// OTLP metric export through the built binary (docs/specs/GATEWAY.md, Observability →
// OTLP metric export): a fake collector receives what /metrics shows — every family
// under the Prometheus translation, with the same descriptions, kinds, attributes and,
// at a quiet moment, values — under the logs' resource; the final export goes at exit,
// before `kaiak stopped`; a delta stream carries changes; nothing is sent when export
// is off.

import (
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"net/http"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"kaiak/internal/fakebackend"
	"kaiak/internal/fakecontrol"
	"kaiak/internal/telemetry/fakeotlp"
	"kaiak/internal/telemetry/metric"
)

// exposition is a /metrics page, or a metric export in the shape /metrics would
// write it: the families with samples, by Prometheus name, and the samples, by
// series (name and labels, labels sorted).
type exposition struct {
	families map[string]promFamily
	samples  map[string]float64
}

// promFamily is a family's HELP text and TYPE.
type promFamily struct{ help, typ string }

// seriesKey is a series as exposition keys it: le formatted as the writer does,
// labels sorted.
func seriesKey(name string, labels map[string]string) string {
	if len(labels) == 0 {
		return name
	}
	parts := make([]string, 0, len(labels))
	for _, k := range slices.Sorted(maps.Keys(labels)) {
		parts = append(parts, fmt.Sprintf("%s=%q", k, labels[k]))
	}
	return name + "{" + strings.Join(parts, ",") + "}"
}

// sortedSeries is a series as /metrics writes it, as exposition keys it.
func sortedSeries(t *testing.T, series string) string {
	t.Helper()
	return seriesKey(parseSeries(t, series))
}

// formatBound is a bucket bound as /metrics writes it in le.
func formatBound(v float64) string {
	if math.IsInf(v, 1) {
		return "+Inf"
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// scrapeExposition reads the gateway's /metrics.
func scrapeExposition(t *testing.T, g *gateway) exposition {
	t.Helper()
	status, body := g.get(t, "/metrics", "")
	if status != http.StatusOK {
		t.Fatalf("/metrics = %d", status)
	}
	declared := map[string]promFamily{}
	e := exposition{families: map[string]promFamily{}, samples: map[string]float64{}}
	for line := range strings.SplitSeq(string(body), "\n") {
		switch {
		case line == "":
		case strings.HasPrefix(line, "# HELP "):
			name, help, _ := strings.Cut(strings.TrimPrefix(line, "# HELP "), " ")
			help = strings.NewReplacer(`\\`, `\`, `\n`, "\n").Replace(help)
			declared[name] = promFamily{help: help, typ: declared[name].typ}
		case strings.HasPrefix(line, "# TYPE "):
			name, typ, _ := strings.Cut(strings.TrimPrefix(line, "# TYPE "), " ")
			declared[name] = promFamily{help: declared[name].help, typ: typ}
		default:
			i := strings.LastIndexByte(line, ' ')
			if i < 0 {
				t.Fatalf("/metrics line %q: no value", line)
			}
			series, value := line[:i], line[i+1:]
			v, err := strconv.ParseFloat(value, 64)
			if err != nil {
				t.Fatalf("/metrics line %q: %v", line, err)
			}
			name, labels := parseSeries(t, series)
			if le, ok := labels["le"]; ok {
				bound, err := strconv.ParseFloat(le, 64)
				if err != nil {
					t.Fatalf("/metrics line %q: le: %v", line, err)
				}
				labels["le"] = formatBound(bound)
			}
			e.samples[seriesKey(name, labels)] = v
			family := name
			if _, ok := declared[family]; !ok {
				for _, suffix := range []string{"_bucket", "_sum", "_count"} {
					family = strings.TrimSuffix(family, suffix)
					if _, ok := declared[family]; ok {
						break
					}
				}
			}
			f, ok := declared[family]
			if !ok {
				t.Fatalf("/metrics sample %q of no declared family", line)
			}
			e.families[family] = f
		}
	}
	return e
}

// parseSeries splits a sample's series into its name and labels.
func parseSeries(t *testing.T, series string) (string, map[string]string) {
	t.Helper()
	name, rest, found := strings.Cut(series, "{")
	labels := map[string]string{}
	if !found {
		return name, labels
	}
	rest = strings.TrimSuffix(rest, "}")
	for rest != "" {
		key, after, ok := strings.Cut(rest, `="`)
		if !ok {
			t.Fatalf("series %q: labels unreadable", series)
		}
		var value strings.Builder
		i := 0
		for ; i < len(after) && after[i] != '"'; i++ {
			if after[i] == '\\' && i+1 < len(after) {
				i++
				if after[i] == 'n' {
					value.WriteByte('\n')
					continue
				}
			}
			value.WriteByte(after[i])
		}
		labels[key] = value.String()
		rest = strings.TrimPrefix(after[min(i+1, len(after)):], ",")
	}
	return name, labels
}

// metricExports are the metric exports the collector received, in arrival order.
func metricExports(c *fakeotlp.Collector) []fakeotlp.Received {
	var out []fakeotlp.Received
	for _, r := range c.Received() {
		if len(r.Export.ResourceMetrics) > 0 {
			out = append(out, r)
		}
	}
	return out
}

// exportTime is the collect time every point of an export carries.
func exportTime(t *testing.T, r fakeotlp.Received) time.Time {
	t.Helper()
	var at string
	check := func(name, pointTime string) {
		if at == "" {
			at = pointTime
		} else if pointTime != at {
			t.Errorf("%s: a point at %s in an export at %s", name, pointTime, at)
		}
	}
	for _, m := range r.Metrics() {
		switch {
		case m.Sum != nil:
			for _, p := range m.Sum.DataPoints {
				check(m.Name, p.TimeUnixNano)
			}
		case m.Gauge != nil:
			for _, p := range m.Gauge.DataPoints {
				check(m.Name, p.TimeUnixNano)
			}
		case m.Histogram != nil:
			for _, p := range m.Histogram.DataPoints {
				check(m.Name, p.TimeUnixNano)
			}
		}
	}
	n, err := strconv.ParseInt(at, 10, 64)
	if err != nil {
		t.Fatalf("export time %q: %v", at, err)
	}
	return time.Unix(0, n)
}

// intAttributes are the metric attributes typed int, as the log export types them
// (docs/specs/GATEWAY.md, OTLP metric export: data model); every other is a string.
var intAttributes = map[string]bool{"http.response.status_code": true}

// pushedExposition is a metric export as /metrics would write it: each metric under
// its Prometheus name (the registry's own translation), each point's attributes as
// labels, a histogram as cumulative buckets, sum and count. temporality is each
// metric's aggregationTemporality, by OpenTelemetry name (gauges have none).
func pushedExposition(t *testing.T, r fakeotlp.Received) (e exposition, temporality map[string]int) {
	t.Helper()
	e = exposition{families: map[string]promFamily{}, samples: map[string]float64{}}
	temporality = map[string]int{}
	labelsOf := func(m string, attrs []fakeotlp.KeyValue) map[string]string {
		labels := map[string]string{}
		for _, kv := range attrs {
			switch v := kv.Value; {
			case intAttributes[kv.Key] && v.IntValue != nil && v.StringValue == nil:
				n, err := strconv.ParseInt(*v.IntValue, 10, 64)
				if err != nil {
					t.Errorf("%s: attribute %s: intValue %q: %v", m, kv.Key, *v.IntValue, err)
				}
				labels[metric.PrometheusLabel(kv.Key)] = strconv.FormatInt(n, 10)
			case !intAttributes[kv.Key] && v.StringValue != nil && v.IntValue == nil:
				labels[metric.PrometheusLabel(kv.Key)] = *v.StringValue
			default:
				t.Errorf("%s: attribute %s is %+v, want an int %v", m, kv.Key, v, intAttributes[kv.Key])
			}
		}
		return labels
	}
	number := func(m string, p fakeotlp.NumberDataPoint) float64 {
		switch {
		case p.AsInt != nil:
			n, err := strconv.ParseInt(*p.AsInt, 10, 64)
			if err != nil {
				t.Errorf("%s: asInt %q: %v", m, *p.AsInt, err)
			}
			return float64(n)
		case p.AsDouble != nil:
			return *p.AsDouble
		}
		t.Errorf("%s: a point with no value", m)
		return 0
	}
	add := func(name string, f promFamily) {
		if _, dup := e.families[name]; dup {
			t.Errorf("%s twice in one export", name)
		}
		e.families[name] = f
	}
	for _, m := range r.Metrics() {
		switch {
		case m.Sum != nil:
			kind, typ := metric.KindUpDownCounter, "gauge"
			if m.Sum.IsMonotonic {
				kind, typ = metric.KindCounter, "counter"
			}
			name := metric.PrometheusName(m.Name, m.Unit, kind)
			add(name, promFamily{m.Description, typ})
			temporality[m.Name] = m.Sum.AggregationTemporality
			for _, p := range m.Sum.DataPoints {
				e.samples[seriesKey(name, labelsOf(m.Name, p.Attributes))] = number(m.Name, p)
			}
		case m.Gauge != nil:
			name := metric.PrometheusName(m.Name, m.Unit, metric.KindGauge)
			add(name, promFamily{m.Description, "gauge"})
			for _, p := range m.Gauge.DataPoints {
				e.samples[seriesKey(name, labelsOf(m.Name, p.Attributes))] = number(m.Name, p)
			}
		case m.Histogram != nil:
			name := metric.PrometheusName(m.Name, m.Unit, metric.KindHistogram)
			add(name, promFamily{m.Description, "histogram"})
			temporality[m.Name] = m.Histogram.AggregationTemporality
			for _, p := range m.Histogram.DataPoints {
				labels := labelsOf(m.Name, p.Attributes)
				if len(p.BucketCounts) != len(p.ExplicitBounds)+1 {
					t.Errorf("%s: %d buckets for %d bounds", m.Name, len(p.BucketCounts), len(p.ExplicitBounds))
					continue
				}
				var cumulative uint64
				for i, c := range p.BucketCounts {
					n, err := strconv.ParseUint(c, 10, 64)
					if err != nil {
						t.Errorf("%s: bucket count %q: %v", m.Name, c, err)
					}
					cumulative += n
					bound := math.Inf(1)
					if i < len(p.ExplicitBounds) {
						bound = p.ExplicitBounds[i]
					}
					bucket := maps.Clone(labels)
					bucket["le"] = formatBound(bound)
					e.samples[seriesKey(name+"_bucket", bucket)] = float64(cumulative)
				}
				count, err := strconv.ParseUint(p.Count, 10, 64)
				if err != nil || count != cumulative {
					t.Errorf("%s: count %q, buckets add up to %d", m.Name, p.Count, cumulative)
				}
				e.samples[seriesKey(name+"_count", labels)] = float64(count)
				if p.Sum == nil {
					t.Errorf("%s: a histogram point with no sum", m.Name)
					continue
				}
				e.samples[seriesKey(name+"_sum", labels)] = *p.Sum
			}
		default:
			t.Errorf("%s: neither sum, gauge nor histogram", m.Name)
		}
	}
	return e, temporality
}

// familyOf is the family a series belongs to among families.
func familyOf(series string, families map[string]promFamily) string {
	name, _, _ := strings.Cut(series, "{")
	if _, ok := families[name]; ok {
		return name
	}
	for _, suffix := range []string{"_bucket", "_sum", "_count"} {
		if base, ok := strings.CutSuffix(name, suffix); ok {
			if _, ok := families[base]; ok {
				return base
			}
		}
	}
	return name
}

// sameExposition fails unless got holds want's families (HELP and TYPE) and
// series; and their values, but for the families in moving — each with the reason
// its values move on their own.
func sameExposition(t *testing.T, what string, want, got exposition, moving map[string]string) {
	t.Helper()
	for _, name := range slices.Sorted(maps.Keys(want.families)) {
		g, ok := got.families[name]
		switch {
		case !ok:
			t.Errorf("%s: family %s missing", what, name)
		case g != want.families[name]:
			t.Errorf("%s: family %s is %+v, want %+v", what, name, g, want.families[name])
		}
	}
	for name := range got.families {
		if _, ok := want.families[name]; !ok {
			t.Errorf("%s: family %s not expected", what, name)
		}
	}
	for _, series := range slices.Sorted(maps.Keys(want.samples)) {
		v, ok := got.samples[series]
		_, moves := moving[familyOf(series, want.families)]
		switch {
		case !ok:
			t.Errorf("%s: series %s missing", what, series)
		case !moves && v != want.samples[series]:
			t.Errorf("%s: %s = %v, want %v", what, series, v, want.samples[series])
		}
	}
	for series := range got.samples {
		if _, ok := want.samples[series]; !ok {
			t.Errorf("%s: series %s not expected", what, series)
		}
	}
}

// exportAfter waits for a metric export collected after since and returns it.
func exportAfter(t *testing.T, c *fakeotlp.Collector, since time.Time) fakeotlp.Received {
	t.Helper()
	var found fakeotlp.Received
	pollUntil(t, "a metric export after "+since.Format(time.RFC3339Nano), waitLimit, func() bool {
		for _, r := range metricExports(c) {
			if exportTime(t, r).After(since) {
				found = r
				return true
			}
		}
		return false
	})
	return found
}

// quietExport takes a scrape, the next export collected after it and a scrape after
// that, with no traffic in between: the export equals the first scrape, which equals
// the second — but for the families in moving, whose values move on their own.
func quietExport(t *testing.T, g *gateway, c *fakeotlp.Collector, moving map[string]string) fakeotlp.Received {
	t.Helper()
	before := scrapeExposition(t, g)
	scraped := time.Now()
	export := exportAfter(t, c, scraped)
	after := scrapeExposition(t, g)
	sameExposition(t, "scrape after the export", before, after, moving)
	pushed, _ := pushedExposition(t, export)
	sameExposition(t, "the export", before, pushed, moving)
	return export
}

// resourceOf is a resource's attributes.
func resourceOf(t *testing.T, r fakeotlp.Resource) map[string]any {
	t.Helper()
	out := map[string]any{}
	for _, kv := range r.Attributes {
		out[kv.Key] = plainValue(t, kv.Value)
	}
	return out
}

// metricExportSeries is the metric exporter's otel.sdk.exporter.metric_data_point.exported
// series of errorType: the data points accepted for "".
func metricExportSeries(errorType string) string {
	return logExportSeries("otel_sdk_exporter_metric_data_point_exported_total", "otlp_http_json_metric_exporter", errorType)
}

// The movers of a gateway exporting both signals: the exporters' own counts.
var (
	metricExporterMoves = map[string]string{
		"otel_sdk_exporter_metric_data_point_exported_total": "each export counts its points once it ends, after its own collect",
	}
	logExporterMoves = map[string]string{
		"otel_sdk_exporter_log_exported_total":   "the log export sends lines on its own schedule",
		"otel_sdk_processor_log_processed_total": "the log export's queue hands lines over on its own schedule",
	}
)

// A gateway in file mode serves a success, a 401, a limit refusal and an upstream
// error; at a quiet moment the export and /metrics agree family by family. One
// endpoint variable feeds logs and metrics, under one resource; the final export
// goes at exit, collected after the drain and sent before `kaiak stopped`.
func TestMetricExport(t *testing.T) {
	backend := fakebackend.New()
	defer backend.Close()
	dir := t.TempDir()
	configFile := filepath.Join(dir, "config.json")
	evalKey, evalHash := newKey()
	_, annHash := newKey()
	writeJSON(t, configFile, testConfig(backend.URL(), evalHash, annHash, ""))
	const secret = "s3cret-otlp-metrics-header"
	var collector *fakeotlp.Collector
	collector = fakeotlp.New(t, fakeotlp.AnswerStatus(func(n int, r *http.Request) int {
		want := "/base/v1/logs"
		if len(collector.Received()[n].Export.ResourceMetrics) > 0 {
			want = "/base/v1/metrics"
		}
		if r.URL.Path != want || r.Header.Get("Authorization") != "Bearer "+secret {
			t.Errorf("export to %s with Authorization %q, want %s", r.URL.Path, r.Header.Get("Authorization"), want)
		}
		return http.StatusOK
	}))
	g := startGatewayEnv(t, append(gatewayEnv(configFile),
		"OTEL_EXPORTER_OTLP_ENDPOINT="+collector.URL+"/base",
		"OTEL_EXPORTER_OTLP_HEADERS=Authorization=Bearer%20"+secret,
		"OTEL_RESOURCE_ATTRIBUTES=deployment.environment.name=e2e",
		"OTEL_METRIC_EXPORT_INTERVAL=200"))

	starting := g.logs.wait(t, "kaiak starting", msg("kaiak starting"))
	if got, want := starting["kaiak.metric_export.endpoint"], collectorHost(t, collector); got != want {
		t.Errorf("kaiak.metric_export.endpoint %v, want %s", got, want)
	}
	if r := g.post(t, "/v1/chat/completions", evalKey, "metrics-ok", chatBody("chat", false, nil)); r.StatusCode != http.StatusOK {
		t.Fatalf("chat: %d %s", r.StatusCode, r.body)
	}
	if r := g.post(t, "/v1/chat/completions", "", "metrics-401", chatBody("chat", false, nil)); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no key: %d %s", r.StatusCode, r.body)
	}
	for i := range 3 {
		r := g.post(t, "/v1/chat/completions", rpmKey, fmt.Sprintf("metrics-rpm-%d", i), chatBody("rpm", false, nil))
		if want := []int{200, 200, 429}[i]; r.StatusCode != want {
			t.Fatalf("rpm request %d: %d %s, want %d", i, r.StatusCode, r.body, want)
		}
	}
	backend.QueueReplies(fakebackend.Reply{Status: http.StatusInternalServerError})
	if r := g.post(t, "/v1/chat/completions", evalKey, "metrics-upstream", chatBody("chat", false, nil)); r.StatusCode < 500 {
		t.Fatalf("upstream error: %d %s, want a 5xx", r.StatusCode, r.body)
	}
	for _, id := range []string{"metrics-ok", "metrics-401", "metrics-rpm-2", "metrics-upstream"} {
		g.settled(t, id)
	}

	moving := maps.Clone(metricExporterMoves)
	maps.Copy(moving, logExporterMoves)
	quiet := quietExport(t, g, collector, moving)
	pushed, temporality := pushedExposition(t, quiet)
	for name, v := range temporality {
		if v != 2 {
			t.Errorf("%s: aggregationTemporality %d, want 2 (cumulative, the default)", name, v)
		}
	}
	// The traffic is in the export: one sample per outcome.
	duration := func(labels string) string {
		return `http_server_request_duration_seconds_count{` + labels +
			`http_request_method="POST",http_response_status_code=`
	}
	for _, series := range []string{
		duration(`gen_ai_request_model="chat",`) + `"200",http_route="/v1/chat/completions",url_scheme="http"}`,
		duration(`error_type="missing_api_key",`) + `"401",http_route="/v1/chat/completions",url_scheme="http"}`,
		duration(`error_type="rate_limit_exceeded",gen_ai_request_model="rpm",`) + `"429",http_route="/v1/chat/completions",url_scheme="http"}`,
		duration(`error_type="upstream_error",gen_ai_request_model="chat",`) + `"500",http_route="/v1/chat/completions",url_scheme="http"}`,
		`kaiak_limit_rejections_total{kaiak_limit_scope="group",kaiak_limit_type="requests_per_minute"}`,
		`kaiak_upstream_attempts_total{kaiak_attempt_outcome="server_error",kaiak_backend_id="fake",kaiak_deployment_model="` + backendChatModel + `"}`,
	} {
		if v, ok := pushed.samples[series]; !ok || v != 1 {
			t.Errorf("%s = %v (present %v), want 1", series, v, ok)
		}
	}
	lastScrape := scrapeExposition(t, g)

	version := buildVersion(t, g)
	stopAt := time.Now()
	g.stop(t)

	// The final export: collected after the stop and before `kaiak stopped` was
	// logged, equal to the last scrape (no traffic since); then the log export's final
	// flush.
	exports := metricExports(collector)
	final := exports[len(exports)-1]
	stopped := stderrLine(t, g.logs.wait(t, "kaiak stopped", msg("kaiak stopped")))["time"].(int64)
	if at := exportTime(t, final); !at.After(stopAt) || at.UnixNano() > stopped {
		t.Errorf("the last export was collected at %s, want after the stop at %s and before kaiak stopped at %s",
			at, stopAt, time.Unix(0, stopped))
	}
	finalPushed, _ := pushedExposition(t, final)
	sameExposition(t, "the final export", lastScrape, finalPushed, moving)
	if v := finalPushed.samples[sortedSeries(t, metricExportSeries(""))]; v == 0 {
		t.Error("the final export counts no data point exported before it")
	}
	received := collector.Received()
	finalAt, stoppedAt := -1, -1
	for i, r := range received {
		if len(r.Export.ResourceMetrics) > 0 {
			finalAt = i
		}
		if slices.Contains(r.Messages(), "kaiak stopped") {
			stoppedAt = i
		}
	}
	if stoppedAt < 0 || finalAt > stoppedAt {
		t.Errorf("the final metric export is export %d, `kaiak stopped` in export %d: want the metrics first", finalAt, stoppedAt)
	}

	// One resource for both signals, one scope.
	wantResource := map[string]any{"service.name": "kaiak", "service.version": version, "service.instance.id": "e2e",
		"deployment.environment.name": "e2e"}
	var logs, metricsSeen int
	for _, r := range received {
		for _, rl := range r.Export.ResourceLogs {
			logs++
			if got := resourceOf(t, rl.Resource); !maps.Equal(got, wantResource) {
				t.Errorf("log resource %v, want %v", got, wantResource)
			}
		}
		for _, rm := range r.Export.ResourceMetrics {
			metricsSeen++
			if got := resourceOf(t, rm.Resource); !maps.Equal(got, wantResource) {
				t.Errorf("metric resource %v, want %v", got, wantResource)
			}
			if len(rm.ScopeMetrics) != 1 || rm.ScopeMetrics[0].Scope.Name != "kaiak" {
				t.Errorf("metric export with scopes %+v, want one named kaiak", rm.ScopeMetrics)
			}
		}
	}
	if logs == 0 || metricsSeen == 0 {
		t.Errorf("%d log and %d metric exports through the one endpoint variable, want both", logs, metricsSeen)
	}
	if strings.Contains(g.logs.text(), secret) {
		t.Error("the stderr log holds the header value")
	}
}

// In control-plane mode the export carries the control families and usage delivery
// too, and agrees with /metrics at a quiet moment.
func TestMetricExportControlPlane(t *testing.T) {
	backend := fakebackend.New()
	defer backend.Close()
	const token = "e2e-control-token"
	cp := fakecontrol.New(token)
	defer cp.Close()
	evalKey, evalHash := newKey()
	_, annHash := newKey()
	data, err := json.Marshal(testConfig(backend.URL(), evalHash, annHash, ""))
	if err != nil {
		t.Fatal(err)
	}
	cp.Publish(data)
	collector := fakeotlp.New(t, nil)
	g := startGatewayEnv(t, append(controlEnv(cp.URL(), token),
		"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT="+collector.URL+"/v1/metrics",
		"OTEL_METRIC_EXPORT_INTERVAL=200"))
	if r := g.post(t, "/v1/chat/completions", evalKey, "cp-metrics-ok", chatBody("chat", false, nil)); r.StatusCode != http.StatusOK {
		t.Fatalf("chat: %d %s", r.StatusCode, r.body)
	}
	g.settled(t, "cp-metrics-ok")
	// Sealed on the 5 s interval, then acknowledged: nothing left to deliver.
	g.waitMetric(t, "the usage acknowledged", `kaiak_usage_batch_sends_total{kaiak_usage_batch_result="acked"}`,
		func(v float64) bool { return v >= 1 })
	g.waitMetric(t, "the usage queue empty", "kaiak_usage_queue_records", func(v float64) bool { return v == 0 })

	quiet := quietExport(t, g, collector, metricExporterMoves)
	names := map[string]bool{}
	for _, m := range quiet.Metrics() {
		names[m.Name] = true
	}
	for _, name := range []string{"kaiak.usage.batch.sends", "kaiak.usage.queue.batches", "kaiak.usage.queue.records",
		"kaiak.usage.queue.size", "kaiak.usage.last_ack_timestamp", "kaiak.usage.dropped_records",
		"kaiak.control.connected", "kaiak.control.last_contact_timestamp", "kaiak.control.totals_applied_timestamp",
		"kaiak.control.outage", "kaiak.control.config_rejected", "kaiak.usage.records"} {
		if !names[name] {
			t.Errorf("%s not exported", name)
		}
	}
	g.stop(t)
}

// With no endpoint, or with OTEL_METRICS_EXPORTER=none beside one, no metric export
// is sent — not even at exit — the exporter's series are absent and `kaiak starting`
// names no metric endpoint.
func TestMetricExportOff(t *testing.T) {
	backend := fakebackend.New()
	defer backend.Close()
	dir := t.TempDir()
	configFile := filepath.Join(dir, "config.json")
	evalKey, evalHash := newKey()
	_, annHash := newKey()
	writeJSON(t, configFile, testConfig(backend.URL(), evalHash, annHash, ""))
	collector := fakeotlp.New(t, nil)
	for name, env := range map[string][]string{
		"no endpoint": {"OTEL_METRIC_EXPORT_INTERVAL=100"},
		"opted out":   {"OTEL_EXPORTER_OTLP_ENDPOINT=" + collector.URL, "OTEL_METRICS_EXPORTER=none"},
	} {
		t.Run(name, func(t *testing.T) {
			g := startGatewayEnv(t, append(gatewayEnv(configFile), env...))
			if r := g.post(t, "/v1/chat/completions", evalKey, "metrics-off-ok", chatBody("chat", false, nil)); r.StatusCode != http.StatusOK {
				t.Fatalf("chat: %d %s", r.StatusCode, r.body)
			}
			g.settled(t, "metrics-off-ok")
			if _, ok := g.metricValue(t, metricExportSeries("")); ok {
				t.Error("otel_sdk_exporter_metric_data_point_exported_total exposed with export off")
			}
			if line := g.logs.wait(t, "kaiak starting", msg("kaiak starting")); line["kaiak.metric_export.endpoint"] != nil {
				t.Errorf("kaiak starting names a metric endpoint with export off: %v", line)
			}
			g.stop(t)
			if n := len(metricExports(collector)); n != 0 {
				t.Errorf("collector received %d metric exports with export off", n)
			}
		})
	}
}

// With the delta preference, counters and histograms are delta end to end: every
// series in every export, an unchanged one at 0, and the deltas add up to what
// /metrics shows; up-down counters stay cumulative.
func TestMetricExportDelta(t *testing.T) {
	backend := fakebackend.New()
	defer backend.Close()
	dir := t.TempDir()
	configFile := filepath.Join(dir, "config.json")
	evalKey, evalHash := newKey()
	_, annHash := newKey()
	writeJSON(t, configFile, testConfig(backend.URL(), evalHash, annHash, ""))
	collector := fakeotlp.New(t, nil)
	g := startGatewayEnv(t, append(gatewayEnv(configFile),
		"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT="+collector.URL+"/v1/metrics",
		"OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE=delta",
		"OTEL_METRIC_EXPORT_INTERVAL=200"))
	if r := g.post(t, "/v1/chat/completions", evalKey, "delta-ok", chatBody("chat", false, nil)); r.StatusCode != http.StatusOK {
		t.Fatalf("chat: %d %s", r.StatusCode, r.body)
	}
	g.settled(t, "delta-ok")
	carried := exportAfter(t, collector, time.Now())
	unchanged := exportAfter(t, collector, exportTime(t, carried))
	scraped := scrapeExposition(t, g)
	g.stop(t)

	// The request's usage record: the one series of its family.
	var records string
	for series := range scraped.samples {
		if strings.HasPrefix(series, "kaiak_usage_records_total{") {
			if records != "" {
				t.Fatalf("usage records in %s and %s, want one series", records, series)
			}
			records = series
		}
	}
	if e, _ := pushedExposition(t, unchanged); e.samples[records] != 0 {
		t.Errorf("%s = %v in an export with no traffic since the last, want 0", records, e.samples[records])
	}

	// Summed over every export, the final one included, each counter and histogram
	// series is the cumulative value /metrics showed; an up-down counter's or a
	// gauge's last value is its value.
	exports := metricExports(collector)
	kinds := map[string]string{}
	for _, r := range exports {
		for _, m := range r.Metrics() {
			switch {
			case m.Sum != nil && m.Sum.IsMonotonic, m.Histogram != nil:
				kinds[m.Name] = "delta"
			default:
				kinds[m.Name] = "cumulative"
			}
		}
	}
	summed := map[string]float64{}
	var last exposition
	for i, r := range exports {
		e, temporality := pushedExposition(t, r)
		for name, v := range temporality {
			if want := map[string]int{"delta": 1, "cumulative": 2}[kinds[name]]; v != want {
				t.Errorf("export %d: %s aggregationTemporality %d, want %d", i, name, v, want)
			}
		}
		for series := range last.samples {
			if _, ok := e.samples[series]; !ok {
				t.Errorf("export %d: %s left out, sent before: every series goes every time", i, series)
			}
		}
		for series, v := range e.samples {
			summed[series] += v
		}
		last = e
	}
	for series, want := range scraped.samples {
		family := familyOf(series, scraped.families)
		if _, moves := metricExporterMoves[family]; moves {
			continue
		}
		got, ok := summed[series]
		cumulative := last.samples[series]
		switch {
		case !ok:
			t.Errorf("%s never exported", series)
		case scraped.families[family].typ == "counter" || scraped.families[family].typ == "histogram":
			if math.Abs(got-want) > 1e-9*math.Max(1, math.Abs(want)) {
				t.Errorf("%s: deltas add up to %v, /metrics shows %v", series, got, want)
			}
		case cumulative != want:
			t.Errorf("%s: last export %v, /metrics shows %v", series, cumulative, want)
		}
	}
	if summed[records] != 1 {
		t.Errorf("%s: deltas add up to %v, want 1", records, summed[records])
	}
}
