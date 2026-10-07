package metrics

import (
	"bytes"
	"context"
	"fmt"
	"net/http/httptest"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"testing"
	"time"

	"kaiak/internal/accounting"
	"kaiak/internal/config"
	"kaiak/internal/control"
	"kaiak/internal/routing"
)

func text(r *Registry) string {
	var buf bytes.Buffer
	r.WriteText(&buf)
	return buf.String()
}

func TestExpositionGolden(t *testing.T) {
	reg := NewRegistry()
	requests := reg.Counter("test_requests_total", "Requests.\nSecond line with \\ backslash.", "path", "code")
	requests.Add(3, `/a"b`, "200")
	requests.Inc("line\nbreak\\", "500")
	requests.Add(0, "", "404") // an empty value leaves its label out
	reg.ScaledCounter("test_cost_usd_total", "Cost.", 1e9).Add(1_500_000_000)
	temperature := reg.Gauge("test_temperature", "Temp.", "room")
	temperature.Set(-1.5, "b")
	temperature.Set(21, "a")
	latency := reg.Histogram("test_latency_seconds", "Latency.", []float64{0.25, 1}, "op")
	latency.Observe(0.125, "read")
	latency.Observe(0.25, "read") // le is inclusive
	latency.Observe(3, "read")
	reg.GaugeFunc("test_in_flight", "In flight.", []string{"backend"}, func(emit func(float64, ...string)) {
		emit(2, "y")
		emit(0, "x")
	})

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
# HELP test_requests_total Requests.\nSecond line with \\ backslash.
# TYPE test_requests_total counter
test_requests_total{code="404"} 0
test_requests_total{path="/a\"b",code="200"} 3
test_requests_total{path="line\nbreak\\",code="500"} 1
# HELP test_temperature Temp.
# TYPE test_temperature gauge
test_temperature{room="a"} 21
test_temperature{room="b"} -1.5
`
	if got := text(reg); got != want {
		t.Errorf("exposition:\n%s\nwant:\n%s", got, want)
	}
	if got := text(reg); got != want {
		t.Error("a second write differs: output is not deterministic")
	}
}

func TestHandlerContentType(t *testing.T) {
	reg := NewRegistry()
	reg.Counter("x_total", "X.").Inc()
	w := httptest.NewRecorder()
	reg.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	if got := w.Header().Get("Content-Type"); got != "text/plain; version=0.0.4; charset=utf-8" {
		t.Errorf("Content-Type %q", got)
	}
	if w.Body.String() != "# HELP x_total X.\n# TYPE x_total counter\nx_total 1\n" {
		t.Errorf("body %q", w.Body.String())
	}
}

func expectPanic(t *testing.T, what string, f func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Errorf("%s: no panic", what)
		}
	}()
	f()
}

func TestRegistrationAndUseAreValidated(t *testing.T) {
	reg := NewRegistry()
	c := reg.Counter("ok_total", "Ok.", "a", "b")
	expectPanic(t, "duplicate name", func() { reg.Counter("ok_total", "Again.") })
	expectPanic(t, "invalid metric name", func() { reg.Counter("9lives", "Bad.") })
	expectPanic(t, "invalid label name", func() { reg.Counter("x_total", "Bad.", "bad-label") })
	expectPanic(t, "reserved label name", func() { reg.Counter("y_total", "Bad.", "__name") })
	expectPanic(t, "duplicate label name", func() { reg.Counter("z_total", "Bad.", "a", "a") })
	expectPanic(t, "le on a histogram", func() { reg.Histogram("h1", "Bad.", []float64{1}, "le") })
	expectPanic(t, "unsorted buckets", func() { reg.Histogram("h2", "Bad.", []float64{2, 1}) })
	expectPanic(t, "too few label values", func() { c.Inc("only-one") })
	expectPanic(t, "too many label values", func() { c.Inc("1", "2", "3") })
}

func TestConcurrentUpdatesAndWrites(t *testing.T) {
	reg := NewRegistry()
	c := reg.Counter("c_total", "C.", "worker")
	h := reg.Histogram("h_seconds", "H.", []float64{1, 10}, "worker")
	g := reg.Gauge("g", "G.", "worker")
	const workers, perWorker = 8, 1000
	var wg sync.WaitGroup
	stop := make(chan struct{})
	var scrapes sync.WaitGroup
	scrapes.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
				_ = text(reg)
			}
		}
	})
	for i := range workers {
		label := fmt.Sprint(i % 2) // two workers share each series
		wg.Go(func() {
			for range perWorker {
				c.Inc(label)
				h.Observe(2, label)
				g.Set(1, label)
			}
		})
	}
	wg.Wait()
	close(stop)
	scrapes.Wait()
	out := text(reg)
	for _, want := range []string{
		`c_total{worker="0"} 4000`, `c_total{worker="1"} 4000`,
		`h_seconds_bucket{worker="0",le="1"} 0`, `h_seconds_bucket{worker="0",le="10"} 4000`,
		`h_seconds_sum{worker="1"} 8000`, `h_seconds_count{worker="1"} 4000`,
	} {
		if !strings.Contains(out, want+"\n") {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestUsageSinkCountsRecords(t *testing.T) {
	holder := &config.Holder{}
	holder.Swap(&config.Snapshot{KeyIDLabel: true, GroupLabel: true})
	reg := NewRegistry()
	sink := NewUsageSink(reg, holder)
	units := accounting.Units{config.UnitTokensIn: 60, config.UnitTokensCached: 40,
		config.UnitTokensCacheWrite: 30, config.UnitTokensOut: 10, config.UnitTokensReasoning: 4}
	rec := accounting.UsageRecord{KeyID: "k-eval", Groups: []string{"research", "rag", "rag-prod", "eval"},
		Model: "pair", Deployment: accounting.Deployment{Backend: "local", Model: "pair-a"},
		Units: units, CostNanoUSD: 120_000}
	sink.Record(rec)
	sink.Record(rec)
	// A key on a top-level group: that group is its own root_group.
	topLevel := accounting.UsageRecord{KeyID: "k-ann", Groups: []string{"ann"}, Model: "open",
		Deployment: accounting.Deployment{Backend: "local", Model: "open"}, Units: units, Partial: true}
	sink.Record(topLevel)

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
	sink.Record(topLevel)
	out = text(reg)
	if want := `kaiak_usage_records_total{key_group="ann",root_group="ann",model="open",status="partial"} 1`; !strings.Contains(out, want+"\n") {
		t.Errorf("missing %q in:\n%s", want, out)
	}

	// group_label switched off: new records carry no key_group label; root_group
	// stays; series written before stay.
	holder.Swap(&config.Snapshot{KeyIDLabel: true, GroupLabel: false})
	sink.Record(rec)
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
	for router.QueuedByModel()["m"] != 1 {
		runtime.Gosched()
	}
	reg := NewRegistry()
	ops := NewOps(reg, router, holder)
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
	expectPanic(t, "unknown attempt outcome", func() { ops.ObserveUpstreamAttempt("busy", "m", "nope", 0) })
	ops.ObserveAttempts("m", 2)
	ops.ConfigLoaded(config.Load{Trigger: "startup", Applied: true, At: time.UnixMilli(1_700_000_000_500)})
	expectPanic(t, "unknown error class", func() { ops.CountError("nope") })

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
	if !strings.Contains(out, `kaiak_build_info{version="`) || !strings.Contains(out, `,go_version="go`) {
		t.Errorf("build info missing:\n%s", out)
	}
}

func TestConfigLoadMetrics(t *testing.T) {
	holder := &config.Holder{}
	reg := NewRegistry()
	ops := NewOps(reg, routing.New(routing.Options{}), holder)
	out := text(reg)
	for _, want := range []string{
		`kaiak_config_apply_duration_seconds_count{trigger="startup",result="applied"} 0`,
		`kaiak_config_apply_duration_seconds_count{trigger="last-known-good",result="rejected"} 0`,
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
	ops.ConfigLoaded(config.Load{Trigger: "startup", Applied: true, At: time.Now(), Document: true, Bytes: 2048,
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

func TestUsageDeliveryMetrics(t *testing.T) {
	reg := NewRegistry()
	d := NewUsageDelivery(reg)
	if out := text(reg); !strings.Contains(out, "kaiak_usage_spool_batches 0\n") ||
		!strings.Contains(out, "kaiak_usage_queued_bytes 0\n") ||
		!strings.Contains(out, `kaiak_usage_batch_sends_total{result="rejected"} 0`+"\n") ||
		strings.Contains(out, "kaiak_usage_last_ack_timestamp_seconds 0") {
		t.Errorf("initial exposition:\n%s", out)
	}
	// The control client reports with its own names for the results.
	d.UsageBatchSent(control.BatchFailed, time.UnixMilli(1_700_000_000_000))
	d.UsageBatchSent(control.BatchAcked, time.UnixMilli(1_700_000_000_500))
	d.UsageBatchSent(control.BatchRejected, time.UnixMilli(1_700_000_001_000))
	d.UsageSpoolDepth(3, 1200, 612_345)
	expectPanic(t, "unknown batch result", func() { d.UsageBatchSent("lost", time.Now()) })
	out := text(reg)
	for _, want := range []string{
		`kaiak_usage_batch_sends_total{result="acked"} 1`,
		`kaiak_usage_batch_sends_total{result="rejected"} 1`,
		`kaiak_usage_batch_sends_total{result="failed"} 1`,
		`kaiak_usage_last_ack_timestamp_seconds 1.7000000005e+09`,
		`kaiak_usage_spool_batches 3`,
		`kaiak_usage_spool_records 1200`,
		`kaiak_usage_queued_bytes 612345`,
	} {
		if !strings.Contains(out, want+"\n") {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestLogExportMetrics(t *testing.T) {
	reg := NewRegistry()
	counts := LogExportCounts{}
	RegisterLogExport(reg, func() LogExportCounts { return counts })
	want := `# HELP kaiak_log_export_records_total Log records exported over OTLP, by outcome (exported: accepted by the collector; failed: in a batch given up; dropped: never sent, a full queue or still queued at exit).
# TYPE kaiak_log_export_records_total counter
kaiak_log_export_records_total{outcome="dropped"} 0
kaiak_log_export_records_total{outcome="exported"} 0
kaiak_log_export_records_total{outcome="failed"} 0
`
	if got := text(reg); got != want {
		t.Errorf("at startup:\n%s\nwant:\n%s", got, want)
	}
	counts = LogExportCounts{Exported: 1024, Failed: 3, Dropped: 7}
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
	reg := NewRegistry()
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
	reg := NewRegistry()
	NewOps(reg, router, holder)
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
