package metrics

import (
	"bytes"
	"context"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"kaiak/internal/accounting"
	"kaiak/internal/config"
	"kaiak/internal/control"
	"kaiak/internal/routing"
	"kaiak/internal/telemetry/metric"
	"kaiak/internal/telemetry/otlplog"
	"kaiak/internal/telemetry/otlpmetric"
)

func text(r *metric.Registry) string {
	var buf bytes.Buffer
	metric.WritePrometheus(&buf, r.Collect())
	return buf.String()
}

func TestUsageMetricsCountsRecords(t *testing.T) {
	backends := map[string]*config.Backend{
		"cloud": {ID: "cloud", Type: config.BackendAnthropic},
		"local": {ID: "local", Type: config.BackendVLLM},
	}
	holder := &config.Holder{}
	holder.Swap(&config.Snapshot{KeyIDLabel: true, GroupLabel: true, Backends: backends})
	reg := metric.NewRegistry()
	usage := NewUsageMetrics(reg, holder)
	units := accounting.Units{config.UnitTokensIn: 60, config.UnitTokensCached: 40,
		config.UnitTokensCacheWrite: 30, config.UnitTokensOut: 10, config.UnitTokensReasoning: 4}
	rec := accounting.UsageRecord{KeyID: "k-eval", Groups: []string{"research", "rag", "rag-prod", "eval"},
		Model: "pair", Operation: "chat", Deployment: accounting.Deployment{Backend: "cloud", Model: "pair-a"},
		Units: units, CostNanoUSD: 120_000}
	usage.Record(rec)
	usage.Record(rec)
	// A key on a top-level group: that group is its own top-level group. A self-hosted
	// backend type has no provider name.
	topLevel := accounting.UsageRecord{KeyID: "k-ann", Groups: []string{"ann"}, Model: "open", Operation: "embeddings",
		Deployment: accounting.Deployment{Backend: "local", Model: "open"}, Units: units, Partial: true}
	usage.Record(topLevel)

	out := text(reg)
	wl := `kaiak_key_group="eval",kaiak_key_root_group="research",kaiak_key_id="k-eval",gen_ai_request_model="pair",` +
		`gen_ai_operation_name="chat",gen_ai_provider_name="anthropic",kaiak_usage_status="complete"`
	tokens := wl + `,gen_ai_token_modality="unknown"`
	ann := `kaiak_key_group="ann",kaiak_key_root_group="ann",kaiak_key_id="k-ann",gen_ai_request_model="open",` +
		`gen_ai_operation_name="embeddings",kaiak_usage_status="partial"`
	for _, want := range []string{
		`kaiak_usage_records_total{` + wl + `} 2`,
		// Input is all input: in + cached + cache write, twice.
		`gen_ai_client_inference_usage_input_tokens_total{` + tokens + `} 260`,
		`gen_ai_client_inference_usage_cache_read_input_tokens_total{` + tokens + `} 80`,
		`gen_ai_client_inference_usage_cache_write_input_tokens_total{` + tokens + `} 60`,
		// Output includes reasoning: the record's tokens_out as it is.
		`gen_ai_client_inference_usage_output_tokens_total{` + tokens + `} 20`,
		`gen_ai_client_inference_usage_reasoning_output_tokens_total{` + tokens + `} 8`,
		`kaiak_usage_cost_usd_total{` + wl + `} 0.00024`,
		`kaiak_usage_records_total{` + ann + `} 1`,
		`kaiak_usage_cost_usd_total{` + ann + `} 0`,
		`gen_ai_client_inference_usage_input_tokens_total{` + ann + `,gen_ai_token_modality="unknown"} 130`,
	} {
		if !strings.Contains(out, want+"\n") {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	// Intermediate levels of the path are not labels.
	if strings.Contains(out, `"rag"`) || strings.Contains(out, `"rag-prod"`) {
		t.Errorf("an intermediate group labels a series:\n%s", out)
	}

	// Switched off: new records carry no kaiak.key.id.
	holder.Swap(&config.Snapshot{KeyIDLabel: false, GroupLabel: true, Backends: backends})
	usage.Record(topLevel)
	out = text(reg)
	if want := `kaiak_usage_records_total{kaiak_key_group="ann",kaiak_key_root_group="ann",gen_ai_request_model="open",` +
		`gen_ai_operation_name="embeddings",kaiak_usage_status="partial"} 1`; !strings.Contains(out, want+"\n") {
		t.Errorf("missing %q in:\n%s", want, out)
	}

	// group_label switched off: new records carry no kaiak.key.group;
	// kaiak.key.root_group stays; series written before stay. The live config no
	// longer has the record's backend: no provider name.
	holder.Swap(&config.Snapshot{KeyIDLabel: true, GroupLabel: false})
	usage.Record(rec)
	out = text(reg)
	for _, want := range []string{
		`kaiak_usage_records_total{kaiak_key_root_group="research",kaiak_key_id="k-eval",gen_ai_request_model="pair",` +
			`gen_ai_operation_name="chat",kaiak_usage_status="complete"} 1`,
		`kaiak_usage_records_total{` + wl + `} 2`,
	} {
		if !strings.Contains(out, want+"\n") {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	// No usage series is labeled by backend: backends multiply every group's series.
	if strings.Contains(out, `kaiak_backend_id=`) {
		t.Errorf("a usage series carries the backend:\n%s", out)
	}
}

func TestOpsMetrics(t *testing.T) {
	holder := &config.Holder{}
	busy := &config.Backend{ID: "busy", MaxInFlight: 1}
	model := &config.Model{Name: "m", Deployments: []config.Deployment{{Backend: busy, Model: "m"}},
		Queue: config.Queue{Size: 5, Timeout: time.Hour}}
	holder.Swap(&config.Snapshot{Backends: map[string]*config.Backend{"idle": {ID: "idle"}, "busy": busy},
		Models: map[string]*config.Model{"m": model, "quiet": {Name: "quiet"}}})
	router := routing.New(routing.Options{})
	router.Configure(holder.Current())
	slot, _, err := router.Acquire(context.Background(), model, routing.Avoid{})
	if err != nil {
		t.Fatal(err)
	}
	defer slot.Release()
	// A second request waits for the busy backend's one slot until the test ends.
	ctx, leave := context.WithCancel(context.Background())
	waited := make(chan error, 1)
	go func() {
		_, _, err := router.Acquire(ctx, model, routing.Avoid{})
		waited <- err
	}()
	defer func() { leave(); <-waited }()
	for router.Serving(nil).Models["m"].Queued != 1 {
		runtime.Gosched()
	}
	reg := metric.NewRegistry()
	ops := NewOps(reg, router, NewCircuits(reg), holder)
	if out := text(reg); !strings.Contains(out, "kaiak_connections_refused_total 0\n") {
		t.Errorf("refused connections not exposed at 0:\n%s", out)
	}
	ops.ConnectionRefused()
	ops.ConnectionRefused()
	ops.ObserveRequest("POST", "/v1/chat/completions", 429, "rate_limit_exceeded", "m", 20*time.Millisecond)
	ops.ObserveRequest("GET", "", 404, "unknown_url", "", time.Millisecond)
	// A client that left before any answer: no status.
	ops.ObserveRequest("POST", "/v1/chat/completions", 0, "client_closed", "m", time.Millisecond)
	ops.CountError(ErrorRateLimited)
	ops.ObserveQueueWait("m", 40*time.Millisecond)
	ops.CountQueueRejection("m", QueueTimeout)
	ops.CountRetry("m", "busy", "server_error")
	ops.ObserveUpstreamAttempt("busy", "m", AttemptSuccess, 30*time.Millisecond)
	ops.ObserveUpstreamAttempt("busy", "m", AttemptServerError, 2*time.Second)
	for _, outcome := range RetryableOutcomes {
		if !slices.Contains(attemptOutcomes, outcome) {
			t.Errorf("retryable outcome %s is no attempt outcome", outcome)
		}
	}
	ops.ObserveAttempts("m", 2)
	ops.ConfigLoaded(config.Load{Trigger: "startup", Snapshot: holder.Current(), At: time.UnixMilli(1_700_000_000_500)})

	out := text(reg)
	for _, want := range []string{
		`kaiak_backend_active_requests{kaiak_backend_id="busy"} 1`,
		`kaiak_backend_active_requests{kaiak_backend_id="idle"} 0`,
		`kaiak_backend_active_requests_limit{kaiak_backend_id="busy"} 1`,
		`kaiak_queue_size{gen_ai_request_model="m"} 1`,
		`kaiak_queue_size{gen_ai_request_model="quiet"} 0`,
		`kaiak_queue_wait_duration_seconds_bucket{gen_ai_request_model="m",le="0.05"} 1`,
		`kaiak_queue_rejections_total{gen_ai_request_model="m",error_type="queue_timeout"} 1`,
		`kaiak_queue_rejections_total{gen_ai_request_model="m",error_type="queue_full"} 0`,
		`kaiak_errors_total{kaiak_error_class="queue_rejected"} 0`,
		`kaiak_retries_total{gen_ai_request_model="m",kaiak_backend_id="busy",kaiak_attempt_outcome="server_error"} 1`,
		`kaiak_upstream_attempts_total{kaiak_backend_id="busy",kaiak_deployment_model="m",kaiak_attempt_outcome="success"} 1`,
		`kaiak_upstream_attempts_total{kaiak_backend_id="busy",kaiak_deployment_model="m",kaiak_attempt_outcome="server_error"} 1`,
		`kaiak_upstream_attempt_duration_seconds_bucket{kaiak_backend_id="busy",le="0.05"} 1`,
		`kaiak_upstream_attempt_duration_seconds_bucket{kaiak_backend_id="busy",le="2.5"} 2`,
		`kaiak_upstream_attempt_duration_seconds_count{kaiak_backend_id="busy"} 2`,
		`kaiak_request_attempts_bucket{gen_ai_request_model="m",le="1"} 0`,
		`kaiak_request_attempts_bucket{gen_ai_request_model="m",le="2"} 1`,
		`http_server_request_duration_seconds_bucket{http_request_method="POST",url_scheme="http",http_route="/v1/chat/completions",http_response_status_code="429",error_type="rate_limit_exceeded",gen_ai_request_model="m",le="0.025"} 1`,
		`http_server_request_duration_seconds_count{http_request_method="GET",url_scheme="http",http_response_status_code="404",error_type="unknown_url"} 1`,
		`http_server_request_duration_seconds_count{http_request_method="POST",url_scheme="http",http_route="/v1/chat/completions",error_type="client_closed",gen_ai_request_model="m"} 1`,
		`kaiak_errors_total{kaiak_error_class="rate_limited"} 1`,
		`kaiak_errors_total{kaiak_error_class="internal"} 0`,
		`kaiak_config_loads_total{kaiak_trigger="startup",kaiak_config_result="applied"} 1`,
		`kaiak_config_last_applied_timestamp_seconds 1.7000000005e+09`,
		`kaiak_connections_refused_total 2`,
	} {
		if !strings.Contains(out, want+"\n") {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, `kaiak_backend_active_requests_limit{kaiak_backend_id="idle"}`) {
		t.Errorf("a backend without a cap has an active-requests limit:\n%s", out)
	}
}

func TestBuildInfo(t *testing.T) {
	reg := metric.NewRegistry()
	RegisterBuildInfo(reg, "0.6.0-3-gabc1234")
	want := `kaiak_build_info{service_version="0.6.0-3-gabc1234",process_runtime_version="` + runtime.Version() + `"} 1`
	if out := text(reg); !strings.Contains(out, want+"\n") {
		t.Errorf("missing %q in:\n%s", want, out)
	}
}

func TestConfigLoadMetrics(t *testing.T) {
	holder := &config.Holder{}
	reg := metric.NewRegistry()
	ops := NewOps(reg, routing.New(routing.Options{}), NewCircuits(reg), holder)
	out := text(reg)
	for _, want := range []string{
		`kaiak_config_apply_duration_seconds_count{kaiak_trigger="startup",kaiak_config_result="applied"} 0`,
		`kaiak_config_apply_duration_seconds_count{kaiak_trigger="seed",kaiak_config_result="rejected"} 0`,
		`kaiak_limits_sync_duration_seconds_count 0`,
	} {
		if !strings.Contains(out, want+"\n") {
			t.Errorf("not at 0 from the start: %q in:\n%s", want, out)
		}
	}
	// No config is in force: no size to give.
	if strings.Contains(out, "\nkaiak_config_size_bytes ") {
		t.Errorf("a config size before any config:\n%s", out)
	}

	holder.Swap(&config.Snapshot{})
	ops.ConfigLoaded(config.Load{Trigger: "startup", Snapshot: holder.Current(), At: time.Now(), Document: true, Bytes: 2048,
		Duration: 700 * time.Microsecond})
	ops.ConfigLoaded(config.Load{Trigger: "sighup", At: time.Now(), Document: true, Bytes: 9999,
		Duration: 40 * time.Millisecond})
	ops.ConfigLoaded(config.Load{Trigger: "sighup", At: time.Now()}) // the file could not be read
	ops.ObserveLimitsSync(3 * time.Second)

	out = text(reg)
	for _, want := range []string{
		// A rejected document leaves the size of the one in force.
		`kaiak_config_size_bytes 2048`,
		`kaiak_config_apply_duration_seconds_bucket{kaiak_trigger="startup",kaiak_config_result="applied",le="0.0005"} 0`,
		`kaiak_config_apply_duration_seconds_bucket{kaiak_trigger="startup",kaiak_config_result="applied",le="0.001"} 1`,
		`kaiak_config_apply_duration_seconds_count{kaiak_trigger="startup",kaiak_config_result="applied"} 1`,
		`kaiak_config_apply_duration_seconds_bucket{kaiak_trigger="sighup",kaiak_config_result="rejected",le="0.025"} 0`,
		`kaiak_config_apply_duration_seconds_bucket{kaiak_trigger="sighup",kaiak_config_result="rejected",le="0.05"} 1`,
		// The unreadable file is a load, counted, but no document was timed.
		`kaiak_config_apply_duration_seconds_count{kaiak_trigger="sighup",kaiak_config_result="rejected"} 1`,
		`kaiak_config_loads_total{kaiak_trigger="sighup",kaiak_config_result="rejected"} 2`,
		`kaiak_config_apply_duration_seconds_count{kaiak_trigger="sighup",kaiak_config_result="applied"} 0`,
		`kaiak_limits_sync_duration_seconds_bucket{le="2.5"} 0`,
		`kaiak_limits_sync_duration_seconds_bucket{le="5"} 1`,
		`kaiak_limits_sync_duration_seconds_count 1`,
	} {
		if !strings.Contains(out, want+"\n") {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestUsageDeliveryMetrics(t *testing.T) {
	reg := metric.NewRegistry()
	d := NewUsageDelivery(reg)
	if out := text(reg); !strings.Contains(out, "kaiak_usage_queue_batches 0\n") ||
		!strings.Contains(out, "kaiak_usage_queue_size_bytes 0\n") ||
		!strings.Contains(out, `kaiak_usage_batch_sends_total{kaiak_usage_batch_result="rejected"} 0`+"\n") ||
		!strings.Contains(out, `kaiak_usage_dropped_records_total{kaiak_usage_drop_reason="memory_bound"} 0`+"\n") ||
		strings.Contains(out, "kaiak_usage_last_ack_timestamp_seconds 0") {
		t.Errorf("initial exposition:\n%s", out)
	}
	// The control client reports with its own names for the results.
	d.UsageBatchSent(control.BatchFailed, time.UnixMilli(1_700_000_000_000))
	d.UsageBatchSent(control.BatchAcked, time.UnixMilli(1_700_000_000_500))
	d.UsageBatchSent(control.BatchRejected, time.UnixMilli(1_700_000_001_000))
	d.UsageQueueDepth(3, 1200, 612_345)
	out := text(reg)
	for _, want := range []string{
		`kaiak_usage_batch_sends_total{kaiak_usage_batch_result="acked"} 1`,
		`kaiak_usage_batch_sends_total{kaiak_usage_batch_result="rejected"} 1`,
		`kaiak_usage_batch_sends_total{kaiak_usage_batch_result="failed"} 1`,
		`kaiak_usage_last_ack_timestamp_seconds 1.7000000005e+09`,
		`kaiak_usage_queue_batches 3`,
		`kaiak_usage_queue_records 1200`,
		`kaiak_usage_queue_size_bytes 612345`,
	} {
		if !strings.Contains(out, want+"\n") {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestLogExportMetrics(t *testing.T) {
	reg := metric.NewRegistry()
	var counts otlplog.Counts
	RegisterLogExport(reg, func() otlplog.Counts { return counts })
	const (
		processor = `otel_component_type="batching_log_processor",otel_component_name="batching_log_processor/0"`
		exporter  = `otel_component_type="otlp_http_json_log_exporter",otel_component_name="otlp_http_json_log_exporter/0"`
	)
	// At startup: the accepted series and the queue's three, at 0; no failure series.
	want := `otel_sdk_exporter_log_exported_total{` + exporter + `} 0
otel_sdk_processor_log_processed_total{` + processor + `} 0
otel_sdk_processor_log_processed_total{` + processor + `,error_type="queue_full"} 0
otel_sdk_processor_log_processed_total{` + processor + `,error_type="shutdown"} 0
`
	if got := samples(text(reg)); got != want {
		t.Errorf("at startup:\n%s\nwant:\n%s", got, want)
	}
	counts = otlplog.Counts{Handed: 1030, QueueFull: 5, Shutdown: 2, Exported: 1024,
		Failed: map[string]uint64{"503": 4, "rejected": 2}}
	want = `otel_sdk_exporter_log_exported_total{` + exporter + `} 1024
otel_sdk_exporter_log_exported_total{` + exporter + `,error_type="503"} 4
otel_sdk_exporter_log_exported_total{` + exporter + `,error_type="rejected"} 2
otel_sdk_processor_log_processed_total{` + processor + `} 1030
otel_sdk_processor_log_processed_total{` + processor + `,error_type="queue_full"} 5
otel_sdk_processor_log_processed_total{` + processor + `,error_type="shutdown"} 2
`
	if got := samples(text(reg)); got != want {
		t.Errorf("counted:\n%s\nwant:\n%s", got, want)
	}
}

func TestMetricExportMetrics(t *testing.T) {
	reg := metric.NewRegistry()
	var counts otlpmetric.Counts
	RegisterMetricExport(reg, func() otlpmetric.Counts { return counts })
	const exporter = `otel_component_type="otlp_http_json_metric_exporter",otel_component_name="otlp_http_json_metric_exporter/0"`
	// At startup: the accepted series at 0; no failure series.
	want := `otel_sdk_exporter_metric_data_point_exported_total{` + exporter + `} 0
`
	if got := samples(text(reg)); got != want {
		t.Errorf("at startup:\n%s\nwant:\n%s", got, want)
	}
	counts = otlpmetric.Counts{Exported: 420, Failed: map[string]uint64{"timeout": 60, "rejected": 2}}
	want = `otel_sdk_exporter_metric_data_point_exported_total{` + exporter + `} 420
otel_sdk_exporter_metric_data_point_exported_total{` + exporter + `,error_type="rejected"} 2
otel_sdk_exporter_metric_data_point_exported_total{` + exporter + `,error_type="timeout"} 60
`
	if got := samples(text(reg)); got != want {
		t.Errorf("counted:\n%s\nwant:\n%s", got, want)
	}
}

// samples are the sample lines of a scrape, without HELP and TYPE.
func samples(scrape string) string {
	var b strings.Builder
	for line := range strings.Lines(scrape) {
		if !strings.HasPrefix(line, "#") {
			b.WriteString(line)
		}
	}
	return b.String()
}

type fakeControlState struct {
	connected, outage, rejected bool
	last, totalsAt              time.Time
}

func (s *fakeControlState) Contact() (bool, time.Time) { return s.connected, s.last }
func (s *fakeControlState) Outage() bool               { return s.outage }
func (s *fakeControlState) ConfigRejected() bool       { return s.rejected }
func (s *fakeControlState) TotalsAppliedAt() (time.Time, bool) {
	return s.totalsAt, !s.totalsAt.IsZero()
}

func TestControlStateMetrics(t *testing.T) {
	reg := metric.NewRegistry()
	s := &fakeControlState{last: time.UnixMilli(1_700_000_000_250)}
	RegisterControlState(reg, s)
	check := func(want ...string) {
		t.Helper()
		out := text(reg)
		for _, line := range want {
			if !strings.Contains(out, line+"\n") {
				t.Errorf("missing %q in:\n%s", line, out)
			}
		}
	}
	check("kaiak_control_connected 0", "kaiak_control_last_contact_timestamp_seconds 1.70000000025e+09",
		"kaiak_control_outage 0", "kaiak_control_config_rejected 0")
	if strings.Contains(text(reg), "\nkaiak_control_totals_applied_timestamp_seconds ") {
		t.Error("totals applied time present before any totals")
	}
	s.connected, s.outage, s.totalsAt = true, true, time.UnixMilli(1_700_000_001_000)
	check("kaiak_control_connected 1", "kaiak_control_outage 1", "kaiak_control_totals_applied_timestamp_seconds 1.700000001e+09")
	s.outage = false
	check("kaiak_control_outage 0")
	s.rejected = true
	check("kaiak_control_config_rejected 1")
}

// Two public models sharing one deployment emit one circuit sample for it, closed or
// open.
func TestCircuitSampleOncePerDeployment(t *testing.T) {
	local := &config.Backend{ID: "local"}
	shared := config.Deployment{Backend: local, Model: "llama"}
	llama := &config.Model{Name: "llama", Deployments: []config.Deployment{shared}}
	alias := &config.Model{Name: "alias", Deployments: []config.Deployment{shared}}
	s := &config.Snapshot{Circuit: config.Circuit{FailureThreshold: 1, ProbeInterval: time.Hour},
		Backends: map[string]*config.Backend{"local": local}, Models: map[string]*config.Model{"llama": llama, "alias": alias}}
	holder := &config.Holder{}
	holder.Swap(s)
	router := routing.New(routing.Options{})
	router.Configure(s)
	reg := metric.NewRegistry()
	NewOps(reg, router, NewCircuits(reg), holder)
	count := func(value string) int {
		return strings.Count(text(reg), `kaiak_circuit_state{kaiak_backend_id="local",kaiak_deployment_model="llama",kaiak_circuit_state="open"} `+value+"\n")
	}
	if n := count("0"); n != 1 {
		t.Errorf("closed: %d samples, want 1", n)
	}
	slot, _, err := router.Acquire(context.Background(), llama, routing.Avoid{})
	if err != nil {
		t.Fatal(err)
	}
	slot.Report(routing.Failure, "scripted")
	slot.Release()
	if n0, n1 := count("0"), count("1"); n0 != 0 || n1 != 1 {
		t.Errorf("open: %d closed and %d open samples, want 0 and 1", n0, n1)
	}
}

// The routing gauges of one scrape, read from routing's picture against the live
// config: every backend, model and deployment of it, the backend and model a reload
// dropped while requests still ran on one and waited for the other, backend shares,
// and closed, open, half-open and cooling deployments — a deployment two models share
// once.
func TestRoutingGauges(t *testing.T) {
	a := &config.Backend{ID: "a", MaxInFlight: 4}
	b := &config.Backend{ID: "b"}
	c := &config.Backend{ID: "c", MaxInFlight: 2}
	d := &config.Backend{ID: "d", MaxInFlight: 1}
	gone := &config.Backend{ID: "gone", MaxInFlight: 1}
	queue := config.Queue{Size: 5, Timeout: time.Hour}
	chat := &config.Model{Name: "chat", Queue: queue, Deployments: []config.Deployment{{Backend: a, Model: "x"}, {Backend: b, Model: "o"}}}
	alias := &config.Model{Name: "alias", Queue: queue, Deployments: []config.Deployment{{Backend: a, Model: "x"}}}
	embed := &config.Model{Name: "embed", Queue: queue, Deployments: []config.Deployment{{Backend: c, Model: "e"}}}
	opened := &config.Model{Name: "opened", Queue: queue, Deployments: []config.Deployment{{Backend: b, Model: "o"}}}
	queued := &config.Model{Name: "queued", Queue: queue, Deployments: []config.Deployment{{Backend: d, Model: "q"}}}
	old := &config.Model{Name: "old", Queue: queue, Deployments: []config.Deployment{{Backend: gone, Model: "g"}}}
	circuit := config.Circuit{FailureThreshold: 1, ProbeInterval: time.Hour}
	first := &config.Snapshot{Circuit: circuit,
		Backends: map[string]*config.Backend{"a": a, "b": b, "c": c, "d": d, "gone": gone},
		Models:   map[string]*config.Model{"chat": chat, "alias": alias, "embed": embed, "opened": opened, "queued": queued, "old": old}}
	second := &config.Snapshot{Circuit: circuit,
		Backends: map[string]*config.Backend{"a": a, "b": b, "c": c, "d": d},
		Models:   map[string]*config.Model{"chat": chat, "alias": alias, "embed": embed, "opened": opened, "queued": queued}}

	reg := metric.NewRegistry()
	circuits := NewCircuits(reg)
	router := routing.New(routing.Options{Observer: circuits,
		Probe: func(context.Context, *config.Backend) (func(string) bool, error) { return nil, nil }})
	holder := &config.Holder{}
	ops := NewOps(reg, router, circuits, holder)
	apply := func(s *config.Snapshot, at int64) {
		holder.Swap(s)
		router.Configure(s)
		ops.ConfigLoaded(config.Load{Trigger: config.TriggerStartup, Snapshot: s, At: time.UnixMilli(at)})
	}
	apply(first, 1_700_000_000_000)

	acquire := func(m *config.Model) routing.Slot {
		t.Helper()
		slot, _, err := router.Acquire(context.Background(), m, routing.Avoid{})
		if err != nil {
			t.Fatal(err)
		}
		return slot
	}
	ctx, leave := context.WithCancel(context.Background())
	waiting := 0
	waited := make(chan struct{}, 4)
	wait := func(m *config.Model) {
		waiting++
		go func() {
			_, _, _ = router.Acquire(ctx, m, routing.Avoid{})
			waited <- struct{}{}
		}()
		for router.Serving(nil).Models[m.Name].Queued != 1 {
			runtime.Gosched()
		}
	}
	defer func() {
		leave()
		for range waiting {
			<-waited
		}
	}()

	// b/o open; c/e half-open (a probe answered); a/x cooling down.
	fail := func(m *config.Model) {
		slot := acquire(m)
		slot.Report(routing.Failure, "scripted")
		slot.Release()
	}
	fail(opened)
	fail(embed)
	if err := router.ProbeNow(context.Background(), "c", "test"); err != nil {
		t.Fatal(err)
	}
	cooling := acquire(alias)
	cooling.Throttled(time.Hour)
	cooling.Release()
	// d's one slot is taken and a request waits for it.
	held := acquire(queued)
	defer held.Release()
	wait(queued)
	// gone's one slot is taken and a request waits for it; a reload drops both.
	retired := acquire(old)
	defer retired.Release()
	wait(old)
	apply(second, 1_700_000_001_000)
	// a: two in flight.
	defer acquire(chat).Release()
	defer acquire(alias).Release()

	want := `kaiak_backend_active_requests{kaiak_backend_id="a"} 2
kaiak_backend_active_requests{kaiak_backend_id="b"} 0
kaiak_backend_active_requests{kaiak_backend_id="c"} 0
kaiak_backend_active_requests{kaiak_backend_id="d"} 1
kaiak_backend_active_requests{kaiak_backend_id="gone"} 1
kaiak_backend_active_requests_limit{kaiak_backend_id="a"} 4
kaiak_backend_active_requests_limit{kaiak_backend_id="c"} 2
kaiak_backend_active_requests_limit{kaiak_backend_id="d"} 1
kaiak_circuit_state{kaiak_backend_id="a",kaiak_deployment_model="x",kaiak_circuit_state="closed"} 1
kaiak_circuit_state{kaiak_backend_id="a",kaiak_deployment_model="x",kaiak_circuit_state="half_open"} 0
kaiak_circuit_state{kaiak_backend_id="a",kaiak_deployment_model="x",kaiak_circuit_state="open"} 0
kaiak_circuit_state{kaiak_backend_id="b",kaiak_deployment_model="o",kaiak_circuit_state="closed"} 0
kaiak_circuit_state{kaiak_backend_id="b",kaiak_deployment_model="o",kaiak_circuit_state="half_open"} 0
kaiak_circuit_state{kaiak_backend_id="b",kaiak_deployment_model="o",kaiak_circuit_state="open"} 1
kaiak_circuit_state{kaiak_backend_id="c",kaiak_deployment_model="e",kaiak_circuit_state="closed"} 0
kaiak_circuit_state{kaiak_backend_id="c",kaiak_deployment_model="e",kaiak_circuit_state="half_open"} 1
kaiak_circuit_state{kaiak_backend_id="c",kaiak_deployment_model="e",kaiak_circuit_state="open"} 0
kaiak_circuit_state{kaiak_backend_id="d",kaiak_deployment_model="q",kaiak_circuit_state="closed"} 1
kaiak_circuit_state{kaiak_backend_id="d",kaiak_deployment_model="q",kaiak_circuit_state="half_open"} 0
kaiak_circuit_state{kaiak_backend_id="d",kaiak_deployment_model="q",kaiak_circuit_state="open"} 0
kaiak_deployment_cooling_down{kaiak_backend_id="a",kaiak_deployment_model="x"} 1
kaiak_deployment_cooling_down{kaiak_backend_id="b",kaiak_deployment_model="o"} 0
kaiak_deployment_cooling_down{kaiak_backend_id="c",kaiak_deployment_model="e"} 0
kaiak_deployment_cooling_down{kaiak_backend_id="d",kaiak_deployment_model="q"} 0
kaiak_queue_size{gen_ai_request_model="alias"} 0
kaiak_queue_size{gen_ai_request_model="chat"} 0
kaiak_queue_size{gen_ai_request_model="embed"} 0
kaiak_queue_size{gen_ai_request_model="old"} 1
kaiak_queue_size{gen_ai_request_model="opened"} 0
kaiak_queue_size{gen_ai_request_model="queued"} 1
`
	var got strings.Builder
	for line := range strings.Lines(text(reg)) {
		for _, family := range []string{"kaiak_backend_active_requests", "kaiak_backend_active_requests_limit",
			"kaiak_circuit_state", "kaiak_deployment_cooling_down", "kaiak_queue_size"} {
			if strings.HasPrefix(line, family+"{") {
				got.WriteString(line)
			}
		}
	}
	if got.String() != want {
		t.Errorf("routing gauges:\n%s\nwant:\n%s", got.String(), want)
	}
}
