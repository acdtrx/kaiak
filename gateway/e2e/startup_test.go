package e2e

// Startup in control-plane mode through the built binary (docs/specs/GATEWAY.md,
// Control-plane mode → Boot and Readiness waits for the first totals): the boot waits
// for a control plane coming up, and the listeners bind only once the first totals
// say what was spent.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"kaiak/internal/fakebackend"
	"kaiak/internal/fakecontrol"
)

// find returns the first line matching match logged so far, without waiting.
func (l *logLines) find(match func(map[string]any) bool) (map[string]any, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, entry := range l.lines {
		if match(entry) {
			return entry, true
		}
	}
	return nil, false
}

// spentBudgetControlPlane is a control plane serving testConfig whose group budgeted
// has spent its budget (0.0001 USD) this month.
func spentBudgetControlPlane(t *testing.T, backendURL, evalHash, annHash string) *fakecontrol.Server {
	t.Helper()
	cp := fakecontrol.New(startupToken)
	t.Cleanup(cp.Close)
	data, err := json.Marshal(testConfig(backendURL, evalHash, annHash, ""))
	if err != nil {
		t.Fatal(err)
	}
	cp.Publish(data)
	month := time.Now().UTC().Format("2006-01") + "-01T00:00:00Z"
	cp.SetWindows([]byte(`[{"group":"budgeted","type":"usd_per_month","window_start":"` + month +
		`","used":"200000"}]`))
	return cp
}

const startupToken = "e2e-startup-token"

// D7: a control plane that comes up 3 s after the gateway — both restarted together —
// gives the gateway its config: the boot retries within its wait (60 s by default)
// instead of exiting after one attempt.
func TestBootWaitsForAControlPlaneComingUp(t *testing.T) {
	backend := fakebackend.New()
	defer backend.Close()
	evalKey, evalHash := newKey()
	_, annHash := newKey()
	cp := fakecontrol.New(startupToken)
	defer cp.Close()
	data, err := json.Marshal(testConfig(backend.URL(), evalHash, annHash, ""))
	if err != nil {
		t.Fatal(err)
	}
	cp.Publish(data)
	cp.SetDown(true)
	up := time.AfterFunc(3*time.Second, func() { cp.SetDown(false) }) // the control plane's own start
	defer up.Stop()

	g := startGatewayEnv(t, controlEnv(cp.URL(), startupToken))
	g.logs.wait(t, "a boot retry", msg("config not received at startup: retrying within the boot wait", "kaiak.control.attempt", "1"))
	g.logs.wait(t, "the boot from the control plane", msg("config applied", "kaiak.trigger", "control"))
	if r := g.post(t, "/v1/chat/completions", evalKey, "", chatBody("chat", false, nil)); r.StatusCode != http.StatusOK {
		t.Fatalf("chat: %d %s", r.StatusCode, r.body)
	}
	g.stop(t)
}

// D8, the independent review's reproduction: a fresh stateless gateway against a
// control plane whose budget is spent, the totals that follow the stream's first
// config held back. The gateway must not be ready before they arrive — ready, it
// admitted the priced request as if nothing were spent — and once they arrive it
// refuses it.
func TestReadinessWaitsForTheFirstTotals(t *testing.T) {
	backend := fakebackend.New()
	defer backend.Close()
	_, evalHash := newKey()
	_, annHash := newKey()
	cp := spentBudgetControlPlane(t, backend.URL(), evalHash, annHash)
	cp.HoldTotalsOnConnect(true)

	g := startProcess(t, "", controlEnv(cp.URL(), startupToken))
	select {
	case <-cp.Connected():
	case <-time.After(waitLimit):
		t.Fatal("the config stream never connected")
	}
	// The totals stay delayed for a second: the time a readiness probe needs to find a
	// gateway that bound without them.
	time.Sleep(time.Second)
	if line, bound := g.logs.find(msg("listening", "kaiak.listener.name", "api")); bound {
		g.api = listenerURL(line)
		r := g.post(t, "/v1/chat/completions", budgetKey, "", chatBody("priced", false, nil))
		t.Fatalf("the API listener bound before the first totals; the priced request on a spent budget answered %d %s",
			r.StatusCode, r.body)
	}
	g.logs.wait(t, "the wait", msg("waiting for the first totals"))

	cp.PushCurrentTotals()
	g.api = listenerURL(g.logs.wait(t, "the API listener", msg("listening", "kaiak.listener.name", "api")))
	g.admin = listenerURL(g.logs.wait(t, "the admin listener", msg("listening", "kaiak.listener.name", "admin")))
	g.logs.wait(t, "the end of the wait", msg("first totals received"))
	if status, body := g.get(t, "/readyz", ""); status != http.StatusOK {
		t.Fatalf("/readyz = %d %s, want 200", status, body)
	}
	r := g.post(t, "/v1/chat/completions", budgetKey, "", chatBody("priced", false, nil))
	if r.StatusCode != http.StatusTooManyRequests || r.errorCode(t) != "budget_exceeded" {
		t.Errorf("priced request after the first totals: %d %s, want 429 budget_exceeded", r.StatusCode, r.body)
	}
	g.stop(t)
}

// D8, the wait running out: the gateway becomes ready without its first totals, and
// refuses priced USD-limited requests budget_unavailable until they arrive — other
// models serve; once they arrive, the spent budget refuses budget_exceeded.
func TestFirstTotalsLateRefuseBudgetsUntilTheyArrive(t *testing.T) {
	backend := fakebackend.New()
	defer backend.Close()
	evalKey, evalHash := newKey()
	_, annHash := newKey()
	cp := spentBudgetControlPlane(t, backend.URL(), evalHash, annHash)
	cp.HoldTotalsOnConnect(true)

	g := startGatewayEnv(t, append(controlEnv(cp.URL(), startupToken), "KAIAK_CONTROL_BOOT_WAIT_MS=1500"))
	g.logs.wait(t, "the wait running out", msg("first totals not received within the boot wait: priced USD-limited requests are refused until they arrive"))
	r := g.post(t, "/v1/chat/completions", budgetKey, "", chatBody("priced", false, nil))
	if r.StatusCode != http.StatusServiceUnavailable || r.errorCode(t) != "budget_unavailable" {
		t.Errorf("priced request with no totals: %d %s, want 503 budget_unavailable", r.StatusCode, r.body)
	}
	if !strings.Contains(string(r.body), "not known") {
		t.Errorf("budget_unavailable message %s, want it to say the spend is not known", r.body)
	}
	// A priced model no USD limit covers costs nothing any budget counts: it serves
	// while the spend is unknown.
	if r := g.post(t, "/v1/chat/completions", evalKey, "", chatBody("priced", false, nil)); r.StatusCode != http.StatusOK {
		t.Errorf("priced model outside every USD limit: %d %s, want 200", r.StatusCode, r.body)
	}

	cp.PushCurrentTotals()
	g.waitMetric(t, "the totals applied", "kaiak_control_totals_applied_timestamp_seconds", func(float64) bool { return true })
	r = g.post(t, "/v1/chat/completions", budgetKey, "", chatBody("priced", false, nil))
	if r.StatusCode != http.StatusTooManyRequests || r.errorCode(t) != "budget_exceeded" {
		t.Errorf("priced request after the totals: %d %s, want 429 budget_exceeded", r.StatusCode, r.body)
	}
	g.stop(t)
}
