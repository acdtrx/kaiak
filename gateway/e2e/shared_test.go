package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"testing"
	"time"

	"kaiak/internal/config"
	"kaiak/internal/fakebackend"
	"kaiak/internal/fakecontrol"
)

// TestSharedLimitsAcrossGateways runs two kaiak processes against one control plane
// (the Go test double, its totals scripted by the test): the per-minute limit and
// the backend cap split between them, a USD budget spent through one enforced on the other once the totals
// are pushed, and the outage policy — money-limited models refused past a shortened
// grace while others serve, and served again once the control plane is back.
func TestSharedLimitsAcrossGateways(t *testing.T) {
	backend := fakebackend.New()
	defer backend.Close()
	const token = "e2e-shared-token"
	cp := fakecontrol.New(token)
	defer cp.Close()
	cp.PushTotalsOnChange()
	cp.SetLiveGateways(2)
	evalKey, evalHash := newKey()
	_, annHash := newKey()
	cfg := testConfig(backend.URL(), evalHash, annHash, "")
	cfg["global"].(map[string]any)["control_outage_grace_ms"] = 2000
	cfg["backends"].(map[string]any)["fake"].(map[string]any)["max_in_flight"] = 4
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cp.Publish(data)

	start := func(instance string) *gateway {
		env := append(controlEnv(cp.URL(), token), "KAIAK_INSTANCE_ID="+instance)
		g := startGatewayEnv(t, env)
		g.waitMetric(t, instance+"'s first totals", "kaiak_control_totals_applied_timestamp_seconds",
			func(float64) bool { return true })
		return g
	}
	a, b := start("gw-a"), start("gw-b")

	t.Run("the per-minute limit is split between the live gateways", func(t *testing.T) {
		// metered's 2 requests a minute, two live gateways: 1 each.
		for _, g := range []*gateway{a, b} {
			r := g.post(t, "/v1/chat/completions", rpmKey, "", chatBody("rpm", false, nil))
			if r.StatusCode != http.StatusOK || r.Header.Get("x-ratelimit-limit-requests") != "1" ||
				r.Header.Get("x-ratelimit-remaining-requests") != "0" {
				t.Fatalf("first request: %d, limit %q remaining %q: %s", r.StatusCode,
					r.Header.Get("x-ratelimit-limit-requests"), r.Header.Get("x-ratelimit-remaining-requests"), r.body)
			}
			if r := g.post(t, "/v1/chat/completions", rpmKey, "", chatBody("rpm", false, nil)); r.StatusCode != http.StatusTooManyRequests {
				t.Fatalf("second request: %d, want 429 on the share: %s", r.StatusCode, r.body)
			}
		}
	})

	t.Run("the backend cap is split between the live gateways", func(t *testing.T) {
		// max_in_flight 4, two live gateways: 2 each.
		for _, g := range []*gateway{a, b} {
			g.waitMetric(t, "the enforced cap", `kaiak_backend_max_in_flight{backend="fake"}`,
				func(v float64) bool { return v == 2 })
		}
	})

	t.Run("a budget spent through one gateway is enforced on the other", func(t *testing.T) {
		if r := a.post(t, "/v1/chat/completions", budgetKey, "e2e-spend", chatBody("priced", false, nil)); r.StatusCode != http.StatusOK {
			t.Fatalf("spend on gw-a: %d %s", r.StatusCode, r.body)
		}
		waitUsage(t, cp, "gw-a's batch", func(e fakecontrol.UsageEvent) bool {
			return e.Outcome == fakecontrol.OutcomeCounted && countedIDs(t, cp)["e2e-spend"] == 1
		})
		cost := countedRecord(t, cp, "e2e-spend")["cost_nano_usd"].(float64)
		if cost <= 100_000 {
			t.Fatalf("spend cost %v nano-USD, want past the 100000 budget", cost)
		}
		now := time.Now().UTC()
		month := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339)
		cp.SetWindows(fmt.Appendf(nil, `[{"group":"budgeted","type":"usd_per_month","window_start":%q,"used":"%d"}]`,
			month, int64(cost)))
		// Totals made before the windows were set may still be on their way to gw-b
		// (pushed when gw-a's batch was counted, or in an ack), so a newer totals
		// timestamp does not show gw-b took these windows. A live count of 1, set after
		// them, marks the totals that carry them: gw-b's cap share doubles once it has
		// applied such totals.
		cp.SetLiveGateways(1)
		b.waitMetric(t, "gw-b taking the pushed windows", `kaiak_backend_max_in_flight{backend="fake"}`,
			func(v float64) bool { return v == 4 })
		r := b.post(t, "/v1/chat/completions", budgetKey, "", chatBody("priced", false, nil))
		if r.StatusCode != http.StatusTooManyRequests || r.errorCode(t) != "budget_exceeded" {
			t.Fatalf("gw-b after the push: %d %s, want 429 budget_exceeded", r.StatusCode, r.body)
		}
		if r := b.post(t, "/v1/chat/completions", evalKey, "", chatBody("priced", false, nil)); r.StatusCode != http.StatusOK {
			t.Fatalf("gw-b on a priced model outside every USD limit: %d %s", r.StatusCode, r.body)
		}
		cp.SetLiveGateways(2)
		for _, g := range []*gateway{a, b} {
			g.waitMetric(t, "two live gateways again", `kaiak_backend_max_in_flight{backend="fake"}`,
				func(v float64) bool { return v == 2 })
		}
	})

	t.Run("past the grace money-limited models fail closed, others serve", func(t *testing.T) {
		cp.SetDown(true)
		cp.CloseStreams()
		a.waitMetric(t, "the outage", "kaiak_control_outage", func(v float64) bool { return v == 1 })
		if got := a.metric(t, "kaiak_control_connected"); got != 0 {
			t.Errorf("connected = %v during the outage", got)
		}
		r := a.post(t, "/v1/chat/completions", budgetKey, "", chatBody("priced", false, nil))
		if r.StatusCode != http.StatusServiceUnavailable || r.errorCode(t) != "budget_unavailable" {
			t.Fatalf("money-limited model in the outage: %d %s, want 503 budget_unavailable", r.StatusCode, r.body)
		}
		if r := a.post(t, "/v1/chat/completions", evalKey, "", chatBody("priced", false, nil)); r.StatusCode != http.StatusOK {
			t.Fatalf("priced model outside every USD limit in the outage: %d %s", r.StatusCode, r.body)
		}
		if got := a.metric(t, `kaiak_errors_total{class="budget_unavailable"}`); got != 1 {
			t.Errorf("budget_unavailable errors = %v, want 1", got)
		}
	})

	t.Run("the control plane back ends the outage", func(t *testing.T) {
		cp.SetDown(false)
		a.waitMetricWithin(t, "the reconnect", "kaiak_control_connected", recoverLimit, func(v float64) bool { return v == 1 })
		if got := a.metric(t, "kaiak_control_outage"); got != 0 {
			t.Errorf("outage = %v once connected", got)
		}
		// Served again, and still over its budget.
		r := a.post(t, "/v1/chat/completions", budgetKey, "", chatBody("priced", false, nil))
		if r.StatusCode != http.StatusTooManyRequests || r.errorCode(t) != "budget_exceeded" {
			t.Fatalf("money-limited model after the outage: %d %s, want 429 budget_exceeded", r.StatusCode, r.body)
		}
	})

	a.stop(t)
	b.stop(t)
}

// Two gateways on one control plane, a config only one of them can apply (a backend
// whose api_key_env is set on gw-a alone). Totals apply whatever config a gateway runs
// — windows are counted per group and type — so gw-b, still on v1, keeps enforcing
// v1's budget on the stream's totals, while gw-a enforces v2's on the same windows.
func TestRejectedConfigKeepsBudgetsEnforcedFromStreamTotals(t *testing.T) {
	backend := fakebackend.New()
	defer backend.Close()
	const token = "e2e-rejected-token"
	const keyEnv = "E2E_ONLY_ON_GW_A"
	cp := fakecontrol.New(token)
	defer cp.Close()
	cp.PushTotalsOnChange()
	cp.SetLiveGateways(2)
	evalKey, evalHash := newKey()
	_, annHash := newKey()
	publish := func(cfg map[string]any) string {
		data, err := json.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		return cp.Publish(data)
	}
	v1 := testConfig(backend.URL(), evalHash, annHash, "")
	v1["backends"].(map[string]any)["fake"].(map[string]any)["max_in_flight"] = 4
	publish(v1)

	start := func(instance string, extra ...string) *gateway {
		env := append(controlEnv(cp.URL(), token), "KAIAK_INSTANCE_ID="+instance)
		g := startGatewayEnv(t, append(env, extra...))
		g.waitMetric(t, instance+"'s first totals", "kaiak_control_totals_applied_timestamp_seconds",
			func(float64) bool { return true })
		return g
	}
	a, b := start("gw-a", keyEnv+"=secret"), start("gw-b")
	// liveMarker sets the live count carried by every totals message from now on and
	// waits for both gateways to take such totals: the cap share shows it
	// (max_in_flight 4 ÷ live).
	liveMarker := func(t *testing.T, live int64) {
		t.Helper()
		cp.SetLiveGateways(live)
		for _, g := range []*gateway{a, b} {
			g.waitMetric(t, "the live count", `kaiak_backend_max_in_flight{backend="fake"}`,
				func(v float64) bool { return v == float64(4/live) })
		}
	}
	chat := func(g *gateway, model string) *response {
		return g.post(t, "/v1/chat/completions", budgetKey, "", chatBody(model, false, nil))
	}
	budgetExceeded := func(t *testing.T, g *gateway, what string) {
		t.Helper()
		if r := chat(g, "priced"); r.StatusCode != http.StatusTooManyRequests || r.errorCode(t) != "budget_exceeded" {
			t.Fatalf("%s: %d %s, want 429 budget_exceeded", what, r.StatusCode, r.body)
		}
	}
	month := func() string {
		now := time.Now().UTC()
		return time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339)
	}

	// v1's budget (0.0001 USD) is spent.
	cp.SetWindows(fmt.Appendf(nil, `[{"group":"budgeted","type":"usd_per_month","window_start":%q,"used":"200000"}]`, month()))
	liveMarker(t, 1)
	for _, g := range []*gateway{a, b} {
		budgetExceeded(t, g, "v1's spent budget")
	}

	// v2 adds a backend whose key only gw-a has, and raises the budget tenfold.
	v2 := testConfig(backend.URL(), evalHash, annHash, "")
	v2["backends"].(map[string]any)["fake"].(map[string]any)["max_in_flight"] = 4
	v2["backends"].(map[string]any)["keyed"] = map[string]any{"type": "openai-compatible",
		"base_url": backend.URL() + "/v1", "api_key_env": keyEnv}
	v2["groups"].(map[string]any)["budgeted"].(map[string]any)["limits"] = []any{
		map[string]any{"type": "usd_per_month", "value": 0.001}}
	hashV2 := publish(v2)
	a.logs.wait(t, "gw-a applying v2", msg("config applied", "kaiak.trigger", "control", "kaiak.config.hash", hashV2))
	b.logs.wait(t, "gw-b rejecting v2", msg("config rejected", "kaiak.trigger", "control", "kaiak.config.hash", hashV2))
	if got := b.metric(t, `kaiak_config_loads_total{trigger="control",result="rejected"}`); got != 1 {
		t.Errorf("gw-b's rejected control loads = %v, want 1", got)
	}

	// The month's spend is 0.0005 USD: above v1's budget, below v2's. Both gateways
	// read the same window; each judges it against the limit of the config it runs.
	cp.SetWindows(fmt.Appendf(nil, `[{"group":"budgeted","type":"usd_per_month","window_start":%q,"used":"500000"}]`, month()))
	liveMarker(t, 2)
	if r := chat(a, "priced"); r.StatusCode != http.StatusOK {
		t.Errorf("gw-a under v2's budget: %d %s, want 200", r.StatusCode, r.body)
	}
	budgetExceeded(t, b, "gw-b under v1's budget on the stream's totals")
	if r := b.post(t, "/v1/chat/completions", evalKey, "", chatBody("priced", false, nil)); r.StatusCode != http.StatusOK {
		t.Errorf("gw-b on a priced model outside every USD limit: %d %s, want 200", r.StatusCode, r.body)
	}
	// Nothing spent: gw-b serves under v1's budget too — the stream's totals, not stale
	// bases, decide.
	cp.SetWindows(fmt.Appendf(nil, `[{"group":"budgeted","type":"usd_per_month","window_start":%q,"used":"0"}]`, month()))
	liveMarker(t, 1)
	if r := chat(b, "priced"); r.StatusCode != http.StatusOK {
		t.Errorf("gw-b with nothing spent: %d %s, want 200", r.StatusCode, r.body)
	}
	// The rejection is reported: gw-b's status names v2 by its hash.
	var rejected bool
	for _, body := range cp.Statuses() {
		var st struct {
			Instance string `json:"instance"`
			Rejected *struct {
				ConfigHash string   `json:"config_hash"`
				Codes      []string `json:"codes"`
			} `json:"last_rejection"`
		}
		if json.Unmarshal(body, &st) == nil && st.Instance == "gw-b" && st.Rejected != nil &&
			st.Rejected.ConfigHash == hashV2 && slices.Contains(st.Rejected.Codes, config.CodeAPIKeyEnvUnset) {
			rejected = true
		}
	}
	if !rejected {
		t.Error("no status from gw-b reports v2 rejected for the unset api_key_env")
	}
	a.stop(t)
	b.stop(t)
}
