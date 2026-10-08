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
)

func text(r *metric.Registry) string {
	var buf bytes.Buffer
	metric.WritePrometheus(&buf, r.Collect())
	return buf.String()
}

func TestUsageMetricsCountsRecords(t *testing.T) {
	holder := &config.Holder{}
	holder.Swap(&config.Snapshot{KeyIDLabel: true, GroupLabel: true})
	reg := metric.NewRegistry()
	usage := NewUsageMetrics(reg, holder)
	units := accounting.Units{config.UnitTokensIn: 60, config.UnitTokensCached: 40,
		config.UnitTokensCacheWrite: 30, config.UnitTokensOut: 10, config.UnitTokensReasoning: 4}
	rec := accounting.UsageRecord{KeyID: "k-eval", Groups: []string{"research", "rag", "rag-prod", "eval"},
		Model: "pair", Deployment: accounting.Deployment{Backend: "local", Model: "pair-a"},
		Units: units, CostNanoUSD: 120_000}
	usage.Record(rec)
	usage.Record(rec)
	// A key on a top-level group: that group is its own root_group.
	topLevel := accounting.UsageRecord{KeyID: "k-ann", Groups: []string{"ann"}, Model: "open",
		Deployment: accounting.Deployment{Backend: "local", Model: "open"}, Units: units, Partial: true}
	usage.Record(topLevel)

	out := text(reg)
	wl := `key_group="eval",root_group="research",key_id="k-eval",model="pair",status="complete"`
	for _, want := range []string{
		`kaiak_usage_records_total{` + wl + `} 2`,
		`kaiak_usage_tokens_total{` + wl + `,unit="tokens_in"} 120`,
		`kaiak_usage_tokens_total{` + wl + `,unit="tokens_cached"} 80`,
		`kaiak_usage_tokens_total{` + wl + `,unit="tokens_cache_write"} 60`,
		`kaiak_usage_tokens_total{` + wl + `,unit="tokens_out"} 20`,
		`kaiak_usage_tokens_total{` + wl + `,unit="tokens_reasoning"} 8`,
		`kaiak_usage_cost_usd_total{` + wl + `} 0.00024`,
		`kaiak_usage_records_total{key_group="ann",root_group="ann",key_id="k-ann",model="open",status="partial"} 1`,
		`kaiak_usage_cost_usd_total{key_group="ann",root_group="ann",key_id="k-ann",model="open",status="partial"} 0`,
	} {
		if !strings.Contains(out, want+"\n") {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	// Intermediate levels of the path are not labels.
	if strings.Contains(out, `"rag"`) || strings.Contains(out, `"rag-prod"`) {
		t.Errorf("an intermediate group labels a series:\n%s", out)
	}

	// Switched off: new records carry no key_id label.
	holder.Swap(&config.Snapshot{KeyIDLabel: false, GroupLabel: true})
	usage.Record(topLevel)
	out = text(reg)
	if want := `kaiak_usage_records_total{key_group="ann",root_group="ann",model="open",status="partial"} 1`; !strings.Contains(out, want+"\n") {
		t.Errorf("missing %q in:\n%s", want, out)
	}

	// group_label switched off: new records carry no key_group label; root_group
	// stays; series written before stay.
	holder.Swap(&config.Snapshot{KeyIDLabel: true, GroupLabel: false})
	usage.Record(rec)
	out = text(reg)
	for _, want := range []string{
		`kaiak_usage_records_total{root_group="research",key_id="k-eval",model="pair",status="complete"} 1`,
		`kaiak_usage_records_total{` + wl + `} 2`,
	} {
		if !strings.Contains(out, want+"\n") {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	// No usage series is labeled by backend: backends multiply every group's series.
	if strings.Contains(out, "kaiak_usage_records_total{") && strings.Contains(out, `backend="local"`) {
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
	ops.ObserveRequest("chat_completions", "m", 429, 20*time.Millisecond)
	ops.ObserveRequest("", "", 404, time.Millisecond)
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
		`kaiak_backend_in_flight_requests{backend="busy"} 1`,
		`kaiak_backend_in_flight_requests{backend="idle"} 0`,
		`kaiak_backend_max_in_flight{backend="busy"} 1`,
		`kaiak_queued_requests{model="m"} 1`,
		`kaiak_queued_requests{model="quiet"} 0`,
		`kaiak_queue_wait_seconds_bucket{model="m",le="0.05"} 1`,
		`kaiak_queue_rejections_total{model="m",reason="timeout"} 1`,
		`kaiak_errors_total{class="queue_rejected"} 0`,
		`kaiak_retries_total{model="m",backend="busy",reason="server_error"} 1`,
		`kaiak_upstream_attempts_total{backend="busy",deployment_model="m",outcome="success"} 1`,
		`kaiak_upstream_attempts_total{backend="busy",deployment_model="m",outcome="server_error"} 1`,
		`kaiak_upstream_attempt_duration_seconds_bucket{backend="busy",le="0.05"} 1`,
		`kaiak_upstream_attempt_duration_seconds_bucket{backend="busy",le="2.5"} 2`,
		`kaiak_upstream_attempt_duration_seconds_count{backend="busy"} 2`,
		`kaiak_request_attempts_bucket{model="m",le="1"} 0`,
		`kaiak_request_attempts_bucket{model="m",le="2"} 1`,
		`kaiak_request_duration_seconds_bucket{endpoint="chat_completions",model="m",status_class="4xx",le="0.025"} 1`,
		`kaiak_request_duration_seconds_count{status_class="4xx"} 1`,
		`kaiak_errors_total{class="rate_limited"} 1`,
		`kaiak_errors_total{class="internal"} 0`,
		`kaiak_config_loads_total{trigger="startup",result="applied"} 1`,
		`kaiak_config_last_applied_timestamp_seconds 1.7000000005e+09`,
		`kaiak_connections_refused_total 2`,
	} {
		if !strings.Contains(out, want+"\n") {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, `kaiak_backend_max_in_flight{backend="idle"}`) {
		t.Errorf("a backend without a cap has a max_in_flight series:\n%s", out)
	}
}

func TestBuildInfo(t *testing.T) {
	reg := metric.NewRegistry()
	RegisterBuildInfo(reg, "0.6.0-3-gabc1234")
	want := `kaiak_build_info{version="0.6.0-3-gabc1234",go_version="` + runtime.Version() + `"} 1`
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
		`kaiak_config_apply_duration_seconds_count{trigger="startup",result="applied"} 0`,
		`kaiak_config_apply_duration_seconds_count{trigger="seed",result="rejected"} 0`,
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
		`kaiak_config_apply_duration_seconds_bucket{trigger="startup",result="applied",le="0.0005"} 0`,
		`kaiak_config_apply_duration_seconds_bucket{trigger="startup",result="applied",le="0.001"} 1`,
		`kaiak_config_apply_duration_seconds_count{trigger="startup",result="applied"} 1`,
		`kaiak_config_apply_duration_seconds_bucket{trigger="sighup",result="rejected",le="0.025"} 0`,
		`kaiak_config_apply_duration_seconds_bucket{trigger="sighup",result="rejected",le="0.05"} 1`,
		// The unreadable file is a load, counted, but no document was timed.
		`kaiak_config_apply_duration_seconds_count{trigger="sighup",result="rejected"} 1`,
		`kaiak_config_loads_total{trigger="sighup",result="rejected"} 2`,
		`kaiak_config_apply_duration_seconds_count{trigger="sighup",result="applied"} 0`,
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
		!strings.Contains(out, "kaiak_usage_queued_bytes 0\n") ||
		!strings.Contains(out, `kaiak_usage_batch_sends_total{result="rejected"} 0`+"\n") ||
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
		`kaiak_usage_batch_sends_total{result="acked"} 1`,
		`kaiak_usage_batch_sends_total{result="rejected"} 1`,
		`kaiak_usage_batch_sends_total{result="failed"} 1`,
		`kaiak_usage_last_ack_timestamp_seconds 1.7000000005e+09`,
		`kaiak_usage_queue_batches 3`,
		`kaiak_usage_queue_records 1200`,
		`kaiak_usage_queued_bytes 612345`,
	} {
		if !strings.Contains(out, want+"\n") {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestLogExportMetrics(t *testing.T) {
	reg := metric.NewRegistry()
	var exported, failed, dropped uint64
	RegisterLogExport(reg, func() (uint64, uint64, uint64) { return exported, failed, dropped })
	want := `# HELP kaiak_log_export_records_total Log records exported over OTLP, by outcome (exported: accepted by the collector; failed: in a batch given up; dropped: never sent, a full queue or still queued at exit).
# TYPE kaiak_log_export_records_total counter
kaiak_log_export_records_total{outcome="dropped"} 0
kaiak_log_export_records_total{outcome="exported"} 0
kaiak_log_export_records_total{outcome="failed"} 0
`
	if got := text(reg); got != want {
		t.Errorf("at startup:\n%s\nwant:\n%s", got, want)
	}
	exported, failed, dropped = 1024, 3, 7
	out := text(reg)
	for _, line := range []string{
		`kaiak_log_export_records_total{outcome="exported"} 1024`,
		`kaiak_log_export_records_total{outcome="failed"} 3`,
		`kaiak_log_export_records_total{outcome="dropped"} 7`,
	} {
		if !strings.Contains(out, line+"\n") {
			t.Errorf("missing %q in:\n%s", line, out)
		}
	}
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
		return strings.Count(text(reg), `kaiak_circuit_open{backend="local",deployment_model="llama"} `+value+"\n")
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
// and open, half-open and cooling deployments — a deployment two models share once.
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

	want := `kaiak_backend_in_flight_requests{backend="a"} 2
kaiak_backend_in_flight_requests{backend="b"} 0
kaiak_backend_in_flight_requests{backend="c"} 0
kaiak_backend_in_flight_requests{backend="d"} 1
kaiak_backend_in_flight_requests{backend="gone"} 1
kaiak_backend_max_in_flight{backend="a"} 4
kaiak_backend_max_in_flight{backend="c"} 2
kaiak_backend_max_in_flight{backend="d"} 1
kaiak_circuit_half_open{backend="a",deployment_model="x"} 0
kaiak_circuit_half_open{backend="b",deployment_model="o"} 0
kaiak_circuit_half_open{backend="c",deployment_model="e"} 1
kaiak_circuit_half_open{backend="d",deployment_model="q"} 0
kaiak_circuit_open{backend="a",deployment_model="x"} 0
kaiak_circuit_open{backend="b",deployment_model="o"} 1
kaiak_circuit_open{backend="c",deployment_model="e"} 0
kaiak_circuit_open{backend="d",deployment_model="q"} 0
kaiak_deployment_cooling_down{backend="a",deployment_model="x"} 1
kaiak_deployment_cooling_down{backend="b",deployment_model="o"} 0
kaiak_deployment_cooling_down{backend="c",deployment_model="e"} 0
kaiak_deployment_cooling_down{backend="d",deployment_model="q"} 0
kaiak_queued_requests{model="alias"} 0
kaiak_queued_requests{model="chat"} 0
kaiak_queued_requests{model="embed"} 0
kaiak_queued_requests{model="old"} 1
kaiak_queued_requests{model="opened"} 0
kaiak_queued_requests{model="queued"} 1
`
	var got strings.Builder
	for line := range strings.Lines(text(reg)) {
		for _, family := range []string{"kaiak_backend_in_flight_requests", "kaiak_backend_max_in_flight",
			"kaiak_circuit_half_open", "kaiak_circuit_open", "kaiak_deployment_cooling_down", "kaiak_queued_requests"} {
			if strings.HasPrefix(line, family+"{") {
				got.WriteString(line)
			}
		}
	}
	if got.String() != want {
		t.Errorf("routing gauges:\n%s\nwant:\n%s", got.String(), want)
	}
}
