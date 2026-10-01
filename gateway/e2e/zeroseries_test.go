package e2e

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"kaiak/internal/fakebackend"
)

// E5 (the audit's N-O4): every counter and histogram whose label set the config
// determines exists at 0 from startup, and a reload's new deployments, models and
// backends get theirs at once — so an increase() alert sees the first event (two
// scrapes: 0, then 1), not a series born at 1.
func TestSeriesStartAtZero(t *testing.T) {
	a, b := fakebackend.New(), fakebackend.New()
	defer a.Close()
	defer b.Close()
	dir := t.TempDir()
	key, hash := newKey()
	configFile := filepath.Join(dir, "config.json")
	writeJSON(t, configFile, reliabilityConfig(a.URL(), b.URL(), hash, nil))
	g := startGateway(t, configFile, filepath.Join(dir, "data"))

	outcomes := []string{"success", "unavailable", "timeout", "auth_failed", "model_missing", "path_missing",
		"server_error", "broke_off", "response_timeout", "rate_limited", "client_error", "canceled", "internal"}
	reasons := []string{"unavailable", "timeout", "server_error", "rate_limited", "auth_failed", "model_missing",
		"path_missing"}
	want := func(series string, value float64) {
		t.Helper()
		if v, ok := g.metricValue(t, series); !ok || v != value {
			t.Errorf("%s = %v (exposed %v), want %v", series, v, ok, value)
		}
	}
	deploymentSeries := func(model, backend, deploymentModel string) {
		t.Helper()
		for _, o := range outcomes {
			want(fmt.Sprintf(`kaiak_upstream_attempts_total{backend=%q,deployment_model=%q,outcome=%q}`, backend, deploymentModel, o), 0)
		}
		for _, to := range []string{"open", "half_open", "closed"} {
			want(fmt.Sprintf(`kaiak_circuit_transitions_total{backend=%q,deployment_model=%q,to=%q}`, backend, deploymentModel, to), 0)
		}
		for _, r := range []string{"success", "failure"} {
			want(fmt.Sprintf(`kaiak_probes_total{backend=%q,result=%q}`, backend, r), 0)
		}
		for _, r := range reasons {
			want(fmt.Sprintf(`kaiak_retries_total{model=%q,backend=%q,reason=%q}`, model, backend, r), 0)
		}
		want(fmt.Sprintf(`kaiak_upstream_attempt_duration_seconds_count{backend=%q}`, backend), 0)
		want(fmt.Sprintf(`kaiak_time_to_first_token_seconds_count{model=%q,backend=%q}`, model, backend), 0)
		want(fmt.Sprintf(`kaiak_output_tokens_per_second_count{model=%q,backend=%q}`, model, backend), 0)
	}
	modelSeries := func(model string) {
		t.Helper()
		for _, r := range []string{"full", "timeout"} {
			want(fmt.Sprintf(`kaiak_queue_rejections_total{model=%q,reason=%q}`, model, r), 0)
		}
		want(fmt.Sprintf(`kaiak_queue_wait_seconds_count{model=%q}`, model), 0)
		want(fmt.Sprintf(`kaiak_request_attempts_count{model=%q}`, model), 0)
	}

	want(`kaiak_config_loads_total{trigger="startup",result="applied"}`, 1)
	for _, trigger := range []string{"startup", "sighup", "control", "seed", "last-known-good"} {
		want(fmt.Sprintf(`kaiak_config_loads_total{trigger=%q,result="rejected"}`, trigger), 0)
	}
	want(`kaiak_errors_total{class="internal"}`, 0)
	want(`kaiak_usage_clamped_records_total`, 0)
	modelSeries("chat")
	deploymentSeries("chat", "a", reliableModel)
	deploymentSeries("chat", "b", reliableModel)

	// The first event moves a series from 0 to 1.
	a.SetReply(fakebackend.Reply{Status: http.StatusInternalServerError})
	chatOK(t, g, key, "first", "chat")
	want(fmt.Sprintf(`kaiak_upstream_attempts_total{backend="a",deployment_model=%q,outcome="server_error"}`, reliableModel), 1)
	want(`kaiak_retries_total{model="chat",backend="a",reason="server_error"}`, 1)

	// A rejected reload: 0, then 1.
	if err := os.WriteFile(configFile, []byte(`{"format_version": 3,`), 0o600); err != nil {
		t.Fatal(err)
	}
	g.signal(t, syscall.SIGHUP)
	g.logs.wait(t, "the rejected reload", msg("config rejected", "trigger", "sighup"))
	want(`kaiak_config_loads_total{trigger="sighup",result="rejected"}`, 1)

	// A reload adding backend c and a model on it: their series appear at 0.
	c := fakebackend.New()
	defer c.Close()
	writeJSON(t, configFile, reliabilityConfig(a.URL(), b.URL(), hash, func(cfg map[string]any) {
		cfg["backends"].(map[string]any)["c"] = map[string]any{"type": "openai-compatible", "base_url": c.URL() + "/v1"}
		model := reliableChatModel()
		model["deployments"] = []any{map[string]any{"backend": "c", "model": "other"}}
		cfg["models"].(map[string]any)["chat-c"] = model
	}))
	g.signal(t, syscall.SIGHUP)
	g.logs.wait(t, "the applied reload", msg("config applied", "trigger", "sighup"))
	modelSeries("chat-c")
	deploymentSeries("chat-c", "c", "other")
	want(`kaiak_config_loads_total{trigger="sighup",result="applied"}`, 1)
	g.stop(t)
}
