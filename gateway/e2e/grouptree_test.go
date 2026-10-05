package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"kaiak/internal/fakebackend"
	"kaiak/internal/fakecontrol"
)

// labelSentinel is a label value no refusal, log line or metric may carry.
const labelSentinel = "Cost Center 4711"

// groupTreeConfig is testConfig with a four-level tree in place of its groups:
// team acme → projects acme-rag and acme-search → envs acme-rag-prod and
// acme-rag-dev → workloads rag-api (prod) and rag-sandbox (dev); search-api is a
// workload straight under acme-search. Limits: the prod env allows 2 "chat" requests
// a minute, the rag project 10 "chat" and 3 "rpm" requests a minute (shared by both
// envs) and 0.0001 USD a month on "priced". Global has no limits; the backend takes
// 4 requests at a time (its share shows which totals a gateway applied).
func groupTreeConfig(backendURL string, hashes map[string]string) map[string]any {
	cfg := testConfig(backendURL, hashes["k-prod"], hashes["k-dev"], "")
	cfg["global"] = map[string]any{}
	cfg["backends"].(map[string]any)["fake"].(map[string]any)["max_in_flight"] = 4
	rpm := func(value int, model string) map[string]any {
		return map[string]any{"type": "requests_per_minute", "value": value, "models": []any{model}}
	}
	cfg["groups"] = map[string]any{
		"acme": map[string]any{"labels": map[string]any{"kind": "team", "cost_center": labelSentinel}},
		"acme-rag": map[string]any{
			"parent": "acme",
			"labels": map[string]any{"kind": "project"},
			"limits": []any{rpm(10, "chat"), rpm(3, "rpm"),
				map[string]any{"type": "usd_per_month", "value": 0.0001, "models": []any{"priced"}}},
		},
		"acme-rag-prod": map[string]any{
			"parent": "acme-rag",
			"labels": map[string]any{"kind": "env", "env": "prod"},
			"limits": []any{rpm(2, "chat")},
		},
		"acme-rag-dev": map[string]any{"parent": "acme-rag", "labels": map[string]any{"kind": "env", "env": "dev"}},
		"rag-api":      map[string]any{"parent": "acme-rag-prod", "labels": map[string]any{"kind": "workload"}},
		"rag-sandbox":  map[string]any{"parent": "acme-rag-dev", "labels": map[string]any{"kind": "workload"}},
		"acme-search":  map[string]any{"parent": "acme", "labels": map[string]any{"kind": "project"}},
		"search-api":   map[string]any{"parent": "acme-search", "labels": map[string]any{"kind": "workload"}},
	}
	cfg["keys"] = map[string]any{
		"k-prod":   map[string]any{"hash": hashes["k-prod"], "group": "rag-api"},
		"k-dev":    map[string]any{"hash": hashes["k-dev"], "group": "rag-sandbox"},
		"k-search": map[string]any{"hash": hashes["k-search"], "group": "search-api"},
	}
	return cfg
}

// The group tree through the binary, in control-plane mode: every group on a key's
// path is a scope — an env's limit refuses while its project has room, a project's
// limit is shared by its envs — usage records carry the path, and totals naming a
// group apply to that group's keys alone.
func TestGroupTreeEndToEnd(t *testing.T) {
	backend := fakebackend.New()
	defer backend.Close()
	const token = "e2e-group-token"
	cp := fakecontrol.New(token)
	defer cp.Close()
	keys, hashes := map[string]string{}, map[string]string{}
	for _, id := range []string{"k-prod", "k-dev", "k-search"} {
		keys[id], hashes[id] = newKey()
	}
	data, err := json.Marshal(groupTreeConfig(backend.URL(), hashes))
	if err != nil {
		t.Fatal(err)
	}
	cp.Publish(data)
	g := startGatewayEnv(t, controlEnv(cp.URL(), token, filepath.Join(t.TempDir(), "data")))
	g.logs.wait(t, "the boot from the control plane", msg("config applied", "kaiak.trigger", "control", "kaiak.config.version", "1"))
	g.waitMetric(t, "the first totals", "kaiak_control_totals_applied_timestamp_seconds", func(float64) bool { return true })

	chat := func(t *testing.T, key, id, model string) *response {
		t.Helper()
		return g.post(t, "/v1/chat/completions", keys[key], id, chatBody(model, false, nil))
	}
	ok := func(t *testing.T, key, id, model string) {
		t.Helper()
		if r := chat(t, key, id, model); r.StatusCode != http.StatusOK {
			t.Fatalf("%s %s: %d %s, want 200", key, model, r.StatusCode, r.body)
		}
	}
	// refused requires a 429 with code, naming a group limit and no ID or label, and
	// a log line naming the group that refused.
	refused := func(t *testing.T, key, id, model, code, limitID, group string) {
		t.Helper()
		r := chat(t, key, id, model)
		if r.StatusCode != http.StatusTooManyRequests || r.errorCode(t) != code {
			t.Fatalf("%s %s: %d %s, want 429 %s", key, model, r.StatusCode, r.body, code)
		}
		body := string(r.body)
		if !strings.Contains(body, "group limit") || strings.Contains(body, limitID) || strings.Contains(body, labelSentinel) {
			t.Errorf("refusal %s: want \"group limit\" with no group ID or label", body)
		}
		line := g.settled(t, id)
		for field, want := range map[string]any{"kaiak.limit.scope": "group", "kaiak.limit.id": limitID, "kaiak.key.group": group} {
			if line[field] != want {
				t.Errorf("log line %s = %v, want %v", field, line[field], want)
			}
		}
	}

	t.Run("an env limit refuses while its project has room", func(t *testing.T) {
		ok(t, "k-prod", "prod-chat-1", "chat")
		ok(t, "k-prod", "prod-chat-2", "chat")
		refused(t, "k-prod", "prod-chat-3", "chat", "rate_limit_exceeded", "acme-rag-prod", "rag-api")
		// The project's 10 have room: the dev env, under the same project, serves.
		ok(t, "k-dev", "dev-chat-1", "chat")
	})

	t.Run("a project limit is shared by its envs", func(t *testing.T) {
		ok(t, "k-prod", "prod-rpm-1", "rpm")
		ok(t, "k-prod", "prod-rpm-2", "rpm")
		ok(t, "k-dev", "dev-rpm-1", "rpm")
		refused(t, "k-dev", "dev-rpm-2", "rpm", "rate_limit_exceeded", "acme-rag", "rag-sandbox")
		refused(t, "k-prod", "prod-rpm-3", "rpm", "rate_limit_exceeded", "acme-rag", "rag-api")
		// Another project under the same team has no such limit.
		ok(t, "k-search", "search-rpm-1", "rpm")
		if got := g.metric(t, `kaiak_limit_rejections_total{scope_kind="group",type="requests_per_minute"}`); got != 3 {
			t.Errorf("group rejections = %v, want 3", got)
		}
	})

	t.Run("usage records carry the key's path, metrics its group and root", func(t *testing.T) {
		want := map[string][]string{
			"prod-chat-1":  {"acme", "acme-rag", "acme-rag-prod", "rag-api"},
			"dev-chat-1":   {"acme", "acme-rag", "acme-rag-dev", "rag-sandbox"},
			"search-rpm-1": {"acme", "acme-search", "search-api"},
		}
		waitUsage(t, cp, "the batch with every request", func(e fakecontrol.UsageEvent) bool {
			ids := countedIDs(t, cp)
			return e.Outcome == fakecontrol.OutcomeCounted && ids["prod-chat-1"] == 1 && ids["dev-chat-1"] == 1 && ids["search-rpm-1"] == 1
		})
		for id, path := range want {
			var got []string
			for _, v := range countedRecord(t, cp, id)["groups"].([]any) {
				got = append(got, v.(string))
			}
			if !slices.Equal(got, path) {
				t.Errorf("%s: record groups %v, want %v", id, got, path)
			}
		}
		for series, want := range map[string]float64{
			`kaiak_usage_records_total{key_group="rag-api",root_group="acme",key_id="k-prod",model="chat",status="complete"}`:     2,
			`kaiak_usage_records_total{key_group="rag-sandbox",root_group="acme",key_id="k-dev",model="rpm",status="complete"}`:   1,
			`kaiak_usage_records_total{key_group="search-api",root_group="acme",key_id="k-search",model="rpm",status="complete"}`: 1,
		} {
			if got := g.metric(t, series); got != want {
				t.Errorf("%s = %v, want %v", series, got, want)
			}
		}
	})

	t.Run("totals for a group apply to that group's keys alone", func(t *testing.T) {
		now := time.Now().UTC()
		month := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339)
		cp.SetWindows(fmt.Appendf(nil, `[{"group":"acme-rag","type":"usd_per_month","models":["priced"],"window_start":%q,"used":"200000"}]`, month))
		// A live count of 2, set after the windows, marks the totals that carry them:
		// the backend cap's share halves once the gateway has applied such totals.
		cp.SetLiveGateways(2)
		cp.PushCurrentTotals()
		g.waitMetric(t, "the pushed windows applied", `kaiak_backend_max_in_flight{backend="fake"}`,
			func(v float64) bool { return v == 2 })
		refused(t, "k-dev", "dev-priced", "priced", "budget_exceeded", "acme-rag", "rag-sandbox")
		refused(t, "k-prod", "prod-priced", "priced", "budget_exceeded", "acme-rag", "rag-api")
		// The other project has no budget: its keys serve.
		ok(t, "k-search", "search-priced", "priced")
	})

	g.stop(t)
	if text := g.logs.text(); strings.Contains(text, labelSentinel) {
		t.Errorf("a label reached the log")
	}
}
