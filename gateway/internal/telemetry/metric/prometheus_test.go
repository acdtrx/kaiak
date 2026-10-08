package metric

import (
	"bytes"
	"testing"
)

func text(r *Registry) string {
	var buf bytes.Buffer
	WritePrometheus(&buf, r.Collect())
	return buf.String()
}

// The translation of Prometheus's OTLP ingestion by default
// (UnderscoreEscapingWithSuffixes): the compatibility specification's examples, a
// name and unit of each kind and unit the semantic conventions and this gateway use,
// and the edges where Prometheus's code is more particular than the specification's
// words.
func TestPrometheusName(t *testing.T) {
	for _, c := range []struct {
		name, unit string
		kind       Kind
		want       string
	}{
		// The compatibility specification's examples.
		{"foo.bar", "By", KindGauge, "foo_bar_bytes"},
		{"network.io", "By/s", KindGauge, "network_io_bytes_per_second"},
		{"foo.duration", "s", KindHistogram, "foo_duration_seconds"},
		{"foo.duration", "ms", KindHistogram, "foo_duration_milliseconds"},
		{"packets.received", "{packet}", KindCounter, "packets_received_total"},
		{"speed", "m/s", KindGauge, "speed_meters_per_second"},
		{"requests_total", "", KindCounter, "requests_total"},
		// Kinds and units of the gateway's metrics.
		{"http.server.request.duration", "s", KindHistogram, "http_server_request_duration_seconds"},
		{"kaiak.output_token_rate", "{token}/s", KindHistogram, "kaiak_output_token_rate_per_second"},
		{"kaiak.errors", "{request}", KindCounter, "kaiak_errors_total"},
		{"kaiak.request.attempts", "{attempt}", KindHistogram, "kaiak_request_attempts"},
		{"kaiak.backend.active_requests", "{request}", KindUpDownCounter, "kaiak_backend_active_requests"},
		{"kaiak.backend.active_requests_limit", "{request}", KindUpDownCounter, "kaiak_backend_active_requests_limit"},
		{"kaiak.circuit.state", "{deployment}", KindUpDownCounter, "kaiak_circuit_state"},
		{"kaiak.config.last_applied_timestamp", "s", KindGauge, "kaiak_config_last_applied_timestamp_seconds"},
		{"kaiak.config.size", "By", KindGauge, "kaiak_config_size_bytes"},
		{"kaiak.usage.queue.size", "By", KindUpDownCounter, "kaiak_usage_queue_size_bytes"},
		{"kaiak.build.info", "", KindGauge, "kaiak_build_info"},
		{"kaiak.control.connected", "", KindGauge, "kaiak_control_connected"},
		{"kaiak.usage.cost_usd", "{USD}", KindCounter, "kaiak_usage_cost_usd_total"},
		{"gen_ai.client.inference.usage.cache_read.input_tokens", "{token}", KindCounter,
			"gen_ai_client_inference_usage_cache_read_input_tokens_total"},
		{"otel.sdk.exporter.metric_data_point.exported", "{data_point}", KindCounter,
			"otel_sdk_exporter_metric_data_point_exported_total"},
		{"otel.sdk.processor.log.processed", "{log_record}", KindCounter, "otel_sdk_processor_log_processed_total"},
		// A unit word the name already has is not repeated — anywhere in the name, as a
		// word; total and ratio move to the end.
		{"queue.wait_seconds", "s", KindHistogram, "queue_wait_seconds"},
		{"usage.queued_bytes", "By", KindUpDownCounter, "usage_queued_bytes"},
		{"seconds.spent", "s", KindCounter, "seconds_spent_total"},
		{"total.requests", "{request}", KindCounter, "requests_total"},
		{"io.bytes", "By/s", KindGauge, "io_bytes_per_second"},
		// The per word is never found in the name: words are split at '_' too.
		{"tokens_per_second", "{token}/s", KindHistogram, "tokens_per_second_per_second"},
		// Unit 1: _ratio on a gauge only.
		{"cpu.utilization", "1", KindGauge, "cpu_utilization_ratio"},
		{"cpu.utilization_ratio", "1", KindGauge, "cpu_utilization_ratio"},
		{"ready", "1", KindUpDownCounter, "ready"},
		{"events", "1", KindCounter, "events_total"},
		// Characters and runs.
		{"a..b__c-d/e_", "", KindGauge, "a_b_c_d_e"},
		{"ns:sub.metric", "", KindGauge, "ns:sub_metric"},
		// Units outside the map are written as themselves, escaped; a part in braces adds
		// nothing.
		{"price", "USD", KindCounter, "price_USD_total"},
		{"odd", "k.By", KindGauge, "odd_k_By"},
		{"rate", "{req}/min", KindGauge, "rate_per_min"},
		{"rate", "By/{batch}", KindGauge, "rate_bytes"},
		{"rate", "1/s", KindGauge, "rate_per_second"},
		{"disk.io", "KiBy", KindCounter, "disk_io_kibibytes_total"},
		{"uptime", "d", KindGauge, "uptime_days"},
		{"x", "TiBy", KindGauge, "x_tibibytes"},
		{"x", "kBy", KindGauge, "x_kBy"},
		{"x", "By/mo", KindGauge, "x_bytes_per_month"},
		{"x", "{a}/{b}", KindGauge, "x"},
		{"x", "s/", KindGauge, "x_seconds"},
		{"x", "/s", KindGauge, "x_per_second"},
	} {
		if got := PrometheusName(c.name, c.unit, c.kind); got != c.want {
			t.Errorf("%s [%s] %s: %q, want %q", c.name, c.unit, c.kind, got, c.want)
		}
	}
}

func TestPrometheusLabel(t *testing.T) {
	for key, want := range map[string]string{
		"model":                     "model",
		"key_group":                 "key_group",
		"kaiak.backend.id":          "kaiak_backend_id",
		"error.type":                "error_type",
		"http.response.status_code": "http_response_status_code",
		"a.._b":                     "a_b",
		"a__b":                      "a_b",
		"trailing.":                 "trailing_",
	} {
		if got := PrometheusLabel(key); got != want {
			t.Errorf("%s: %q, want %q", key, got, want)
		}
	}
}

func TestPrometheusGolden(t *testing.T) {
	reg := NewRegistry()
	requests := reg.Counter(Definition{Name: "test.requests", Unit: "{request}",
		Description: "Requests.\nSecond line with \\ backslash.", Attributes: []string{"url.path", "code"}})
	requests.Add(3, `/a"b`, "200")
	requests.Inc("line\nbreak\\", "500")
	requests.Add(0, "", "404") // an empty value leaves its attribute out
	reg.ScaledCounter(Definition{Name: "test.cost_usd", Unit: "{USD}", Description: "Cost."}, 1e9).Add(1_500_000_000)
	temperature := reg.Gauge(Definition{Name: "test.temperature", Unit: "Cel", Description: "Temp.", Attributes: []string{"room"}})
	temperature.Set(-1.5, "b")
	temperature.Set(21, "a")
	latency := reg.Histogram(Definition{Name: "test.latency", Unit: "s", Description: "Latency.",
		Attributes: []string{"op"}, Buckets: []float64{0.25, 1}})
	latency.Observe(0.125, "read")
	latency.Observe(0.25, "read") // a bound is inclusive
	latency.Observe(3, "read")
	queued := reg.UpDownCounter(Definition{Name: "test.queue.size", Unit: "By", Description: "Queued."})
	queued.Add(5)
	queued.Add(-2)
	inFlight := reg.ObservableUpDownCounter(Definition{Name: "test.in_flight", Unit: "{request}", Description: "In flight.",
		Attributes: []string{"backend"}})
	sent := reg.ObservableCounter(Definition{Name: "test.sent", Unit: "{record}", Description: "Sent."})
	up := reg.ObservableGauge(Definition{Name: "test.up", Description: "Up."})
	reg.Callback(func(o *Observer) {
		inFlight.Observe(o, 2, "y")
		inFlight.Observe(o, 0, "x")
		sent.Observe(o, 1024)
		up.Observe(o, 1)
	}, inFlight, sent, up)

	want := `# HELP test_cost_usd_total Cost.
# TYPE test_cost_usd_total counter
test_cost_usd_total 1.5
# HELP test_in_flight In flight.
# TYPE test_in_flight gauge
test_in_flight{backend="x"} 0
test_in_flight{backend="y"} 2
# HELP test_latency_seconds Latency.
# TYPE test_latency_seconds histogram
test_latency_seconds_bucket{op="read",le="0.25"} 2
test_latency_seconds_bucket{op="read",le="1"} 2
test_latency_seconds_bucket{op="read",le="+Inf"} 3
test_latency_seconds_sum{op="read"} 3.375
test_latency_seconds_count{op="read"} 3
# HELP test_queue_size_bytes Queued.
# TYPE test_queue_size_bytes gauge
test_queue_size_bytes 3
# HELP test_requests_total Requests.\nSecond line with \\ backslash.
# TYPE test_requests_total counter
test_requests_total{code="404"} 0
test_requests_total{url_path="/a\"b",code="200"} 3
test_requests_total{url_path="line\nbreak\\",code="500"} 1
# HELP test_sent_total Sent.
# TYPE test_sent_total counter
test_sent_total 1024
# HELP test_temperature_celsius Temp.
# TYPE test_temperature_celsius gauge
test_temperature_celsius{room="a"} 21
test_temperature_celsius{room="b"} -1.5
# HELP test_up Up.
# TYPE test_up gauge
test_up 1
`
	if got := text(reg); got != want {
		t.Errorf("exposition:\n%s\nwant:\n%s", got, want)
	}
	if got := text(reg); got != want {
		t.Error("a second write differs: output is not deterministic")
	}
}

// Families are written in the order of their Prometheus names, which is not always
// the order of their OpenTelemetry names.
func TestPrometheusOrderIsByPrometheusName(t *testing.T) {
	reg := NewRegistry()
	reg.Gauge(Definition{Name: "a.b", Unit: "s", Description: "Seconds."}).Set(1)
	reg.Gauge(Definition{Name: "a_a", Description: "Plain."}).Set(2)
	want := "# HELP a_a Plain.\n# TYPE a_a gauge\na_a 2\n# HELP a_b_seconds Seconds.\n# TYPE a_b_seconds gauge\na_b_seconds 1\n"
	if got := text(reg); got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}
