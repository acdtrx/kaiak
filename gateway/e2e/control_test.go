package e2e

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kaiak/internal/fakebackend"
	"kaiak/internal/fakecontrol"
)

// TestControlModeEndToEnd runs kaiak from a control plane (the Go test double; the
// sample control plane joins in its own end-to-end test): boot from the stream, a
// served request, a config pushed on the stream; usage batches reaching the control
// plane, a batch whose ack was lost resent with its ID after the gateway is killed,
// the drain flushing the last records; then a restart with the control plane gone
// that boots from the last-known-good config and keeps its usage spooled.
func TestControlModeEndToEnd(t *testing.T) {
	backend := fakebackend.New()
	defer backend.Close()
	const token = "e2e-control-token"
	cp := fakecontrol.New(token)
	defer cp.Close()
	dataDir := filepath.Join(t.TempDir(), "data")
	evalKey, evalHash := newKey()
	_, annHash := newKey()
	publish := func(extraModel string) {
		data, err := json.Marshal(testConfig(backend.URL(), evalHash, annHash, extraModel))
		if err != nil {
			t.Fatal(err)
		}
		cp.Publish(data)
	}
	publish("")

	g := startGatewayEnv(t, controlEnv(cp.URL(), token, dataDir))
	g.logs.wait(t, "the boot from the control plane", msg("config applied", "kaiak.trigger", "control"))

	t.Run("serves from the pushed config", func(t *testing.T) {
		if r := g.post(t, "/v1/chat/completions", evalKey, "", chatBody("chat", false, nil)); r.StatusCode != http.StatusOK {
			t.Fatalf("chat: %d %s", r.StatusCode, r.body)
		}
		// The boot's stream, then the one the gateway follows.
		g.logs.waitCount(t, "the stream", 2, waitLimit, msg("config stream connected"))
		publish("chat-2")
		g.logs.waitCount(t, "the pushed config", 2, waitLimit, msg("config applied", "kaiak.trigger", "control"))
		if status, body := g.get(t, "/v1/models/chat-2", evalKey); status != http.StatusOK {
			t.Fatalf("/v1/models/chat-2 after the push = %d %s", status, body)
		}
		if got := g.metric(t, `kaiak_config_loads_total{trigger="control",result="applied"}`); got != 2 {
			t.Errorf("control loads applied = %v, want 2", got)
		}
	})

	t.Run("usage batches reach the control plane", func(t *testing.T) {
		const acked = `kaiak_usage_batch_sends_total{result="acked"}`
		ackedBefore, _ := g.metricValue(t, acked)
		totalsBefore := g.metric(t, "kaiak_control_totals_applied_timestamp_seconds")
		if r := g.post(t, "/v1/chat/completions", evalKey, "e2e-usage-1", chatBody("chat", false, nil)); r.StatusCode != http.StatusOK {
			t.Fatalf("chat: %d %s", r.StatusCode, r.body)
		}
		// Sealed on the 5 s interval.
		waitUsage(t, cp, "the batch with e2e-usage-1", func(e fakecontrol.UsageEvent) bool {
			return e.Outcome == fakecontrol.OutcomeCounted && countedIDs(t, cp)["e2e-usage-1"] == 1
		})
		// The ack carries no totals: taken, it leaves the applied totals as they were;
		// the stream's next totals are applied.
		g.waitMetric(t, "the ack taken", acked, func(v float64) bool { return v > ackedBefore })
		if got := g.metric(t, "kaiak_control_totals_applied_timestamp_seconds"); got != totalsBefore {
			t.Errorf("totals applied at %v after the ack, want %v: an ack carries none", got, totalsBefore)
		}
		cp.PushCurrentTotals()
		g.waitMetric(t, "the pushed totals applied", "kaiak_control_totals_applied_timestamp_seconds",
			func(v float64) bool { return v > totalsBefore })
		rec := countedRecord(t, cp, "e2e-usage-1")
		units, _ := rec["units"].(map[string]any)
		if rec["gateway_instance"] != "e2e" || rec["model"] != "chat" || rec["key_id"] == "" ||
			units["tokens_out"] == nil || units["tokens_out"].(float64) <= 0 {
			t.Errorf("counted record %v", rec)
		}
		var statuses int
		for _, body := range cp.Statuses() {
			var st map[string]any
			if err := json.Unmarshal(body, &st); err != nil || st["instance"] != "e2e" || st["state"] != "ready" {
				t.Errorf("status %s", body)
			}
			statuses++
		}
		if statuses == 0 {
			t.Error("no status reported")
		}
	})

	var lost fakecontrol.UsageEvent
	t.Run("a gateway killed before an ack keeps the batch", func(t *testing.T) {
		cp.SetUsageFault(&fakecontrol.UsageFault{DropAck: true})
		if r := g.post(t, "/v1/chat/completions", evalKey, "e2e-usage-2", chatBody("chat", false, nil)); r.StatusCode != http.StatusOK {
			t.Fatalf("chat: %d %s", r.StatusCode, r.body)
		}
		lost = waitUsage(t, cp, "the batch whose ack is dropped", func(e fakecontrol.UsageEvent) bool {
			return e.Outcome == fakecontrol.OutcomeAckDropped
		})
	})
	// Killed between the send and the ack: nothing flushed, no drain.
	if err := g.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	<-g.exited
	cp.SetUsageFault(nil)
	g = startGatewayEnv(t, controlEnv(cp.URL(), token, dataDir))

	t.Run("the restarted gateway resends it with its ID, counted once", func(t *testing.T) {
		resent := waitUsage(t, cp, "the resent batch", func(e fakecontrol.UsageEvent) bool {
			return e.Outcome == fakecontrol.OutcomeDuplicate
		})
		if resent.Batch != lost.Batch || lost.Batch.Sequence == 0 {
			t.Errorf("resent batch %+v, lost ack for %+v", resent.Batch, lost.Batch)
		}
		if n := countedIDs(t, cp)["e2e-usage-2"]; n != 1 {
			t.Errorf("e2e-usage-2 counted %d times, want 1", n)
		}
		g.logs.wait(t, "the restored spool", msg("usage spool restored", "kaiak.usage.batches", "1"))
	})

	t.Run("SIGTERM flushes the last records and reports draining", func(t *testing.T) {
		if r := g.post(t, "/v1/chat/completions", evalKey, "e2e-usage-3", chatBody("chat", false, nil)); r.StatusCode != http.StatusOK {
			t.Fatalf("chat: %d %s", r.StatusCode, r.body)
		}
		g.stop(t) // well within the 5 s seal interval
		if n := countedIDs(t, cp)["e2e-usage-3"]; n != 1 {
			t.Errorf("e2e-usage-3 counted %d times, want 1", n)
		}
		statuses := cp.Statuses()
		if last := string(statuses[len(statuses)-1]); !strings.Contains(last, `"state":"draining"`) {
			t.Errorf("last status %s, want draining", last)
		}
		g.logs.wait(t, "the flush", msg("usage flushed", "kaiak.trigger", "drain"))
	})

	cp.Close()

	t.Run("boots from the last-known-good config with the control plane down", func(t *testing.T) {
		g := startGatewayEnv(t, append(controlEnv(cp.URL(), token, dataDir), "KAIAK_DRAIN_TIMEOUT_MS=500",
			"KAIAK_CONTROL_BOOT_WAIT_MS=1000"))
		g.logs.wait(t, "the last-known-good boot",
			msg("config applied", "kaiak.trigger", "last-known-good"))
		if status, body := g.get(t, "/v1/models/chat-2", evalKey); status != http.StatusOK {
			t.Fatalf("/v1/models/chat-2 from last-known-good = %d %s", status, body)
		}
		if r := g.post(t, "/v1/chat/completions", evalKey, "", chatBody("chat", false, nil)); r.StatusCode != http.StatusOK {
			t.Fatalf("chat from last-known-good: %d %s", r.StatusCode, r.body)
		}
		g.stop(t)
		// The drain could not deliver: the batch stays spooled for the next start.
		g.logs.wait(t, "the failed flush", msg("usage not flushed: left in the spool for the next start", "kaiak.usage.batches", "1"))
		batches, err := filepath.Glob(filepath.Join(dataDir, "usage-batch-*.json"))
		if err != nil || len(batches) != 1 {
			t.Errorf("spooled batches %v (%v), want 1", batches, err)
		}
	})
}

// waitUsage reads the control plane's usage events until match accepts one.
func waitUsage(t *testing.T, cp *fakecontrol.Server, what string, match func(fakecontrol.UsageEvent) bool) fakecontrol.UsageEvent {
	t.Helper()
	deadline := time.After(waitLimit)
	for {
		select {
		case e := <-cp.UsageEvents():
			if match(e) {
				return e
			}
		case <-deadline:
			t.Fatalf("no usage batch: %s", what)
			return fakecontrol.UsageEvent{}
		}
	}
}

// countedIDs counts the counted records per request ID.
func countedIDs(t *testing.T, cp *fakecontrol.Server) map[string]int {
	t.Helper()
	ids := map[string]int{}
	for _, raw := range cp.CountedRecords() {
		var rec struct {
			RequestID string `json:"request_id"`
		}
		if err := json.Unmarshal(raw, &rec); err != nil {
			t.Fatal(err)
		}
		ids[rec.RequestID]++
	}
	return ids
}

func countedRecord(t *testing.T, cp *fakecontrol.Server, requestID string) map[string]any {
	t.Helper()
	for _, raw := range cp.CountedRecords() {
		var rec map[string]any
		if err := json.Unmarshal(raw, &rec); err != nil {
			t.Fatal(err)
		}
		if rec["request_id"] == requestID {
			return rec
		}
	}
	t.Fatalf("no counted record of %s", requestID)
	return nil
}

// M5: a gateway restarted while the control plane is down keeps enforcing the spent
// budget it last knew — its applied totals are kept in the data directory and
// restored before traffic — instead of counting the month from zero until totals
// arrive again.
func TestRestartWithTheControlPlaneDownKeepsASpentBudget(t *testing.T) {
	backend := fakebackend.New()
	defer backend.Close()
	const token = "e2e-control-token"
	cp := fakecontrol.New(token)
	defer cp.Close()
	dataDir := filepath.Join(t.TempDir(), "data")
	_, evalHash := newKey()
	_, annHash := newKey()
	data, err := json.Marshal(testConfig(backend.URL(), evalHash, annHash, ""))
	if err != nil {
		t.Fatal(err)
	}
	cp.Publish(data)

	g := startGatewayEnv(t, controlEnv(cp.URL(), token, dataDir))
	g.logs.waitCount(t, "the stream", 2, waitLimit, msg("config stream connected"))
	// The group budgeted's budget (0.0001 USD) is spent this month.
	month := time.Now().UTC().Format("2006-01") + "-01T00:00:00Z"
	cp.SetWindows([]byte(`[{"group":"budgeted","type":"usd_per_month","window_start":"` + month +
		`","used":"200000"}]`))
	cp.PushCurrentTotals()
	deadline := time.Now().Add(waitLimit)
	for {
		r := g.post(t, "/v1/chat/completions", budgetKey, "", chatBody("priced", false, nil))
		if r.StatusCode == http.StatusTooManyRequests && strings.Contains(string(r.body), "budget_exceeded") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the pushed spent budget never refused: %d %s", r.StatusCode, r.body)
		}
		time.Sleep(20 * time.Millisecond)
	}
	g.stop(t)
	cp.Close()

	g = startGatewayEnv(t, append(controlEnv(cp.URL(), token, dataDir), "KAIAK_DRAIN_TIMEOUT_MS=500",
		"KAIAK_CONTROL_BOOT_WAIT_MS=1000"))
	g.logs.wait(t, "the last-known-good boot", msg("config applied", "kaiak.trigger", "last-known-good"))
	r := g.post(t, "/v1/chat/completions", budgetKey, "", chatBody("priced", false, nil))
	if r.StatusCode != http.StatusTooManyRequests || !strings.Contains(string(r.body), "budget_exceeded") {
		t.Errorf("after the restart: %d %s, want the spent budget still refusing", r.StatusCode, r.body)
	}
	g.stop(t)
}

// A control plane restored to an older config — a backup, an in-memory store started
// over — sends it on the stream, and the gateway applies it: the control plane is the
// authority on which config is current, whatever the gateway ran before.
func TestRestoredOlderConfigIsApplied(t *testing.T) {
	backend := fakebackend.New()
	defer backend.Close()
	const token = "e2e-control-token"
	cp := fakecontrol.New(token)
	defer cp.Close()
	evalKey, evalHash := newKey()
	_, annHash := newKey()
	publish := func(extraModel string) string {
		data, err := json.Marshal(testConfig(backend.URL(), evalHash, annHash, extraModel))
		if err != nil {
			t.Fatal(err)
		}
		return cp.Publish(data)
	}
	older := publish("")
	g := startGatewayEnv(t, controlEnv(cp.URL(), token, ""))
	g.logs.wait(t, "the boot", msg("config applied", "kaiak.trigger", "control", "kaiak.config.hash", older))
	newer := publish("chat-2")
	g.logs.wait(t, "the newer config", msg("config applied", "kaiak.trigger", "control", "kaiak.config.hash", newer))
	if status, body := g.get(t, "/v1/models/chat-2", evalKey); status != http.StatusOK {
		t.Fatalf("/v1/models/chat-2 under the newer config = %d %s", status, body)
	}

	cp.Restart() // the store comes back with the older config only
	publish("")
	g.logs.waitCount(t, "the older config applied again", 2, waitLimit,
		msg("config applied", "kaiak.trigger", "control", "kaiak.config.hash", older))
	if status, _ := g.get(t, "/v1/models/chat-2", evalKey); status != http.StatusNotFound {
		t.Errorf("/v1/models/chat-2 after the restore = %d, want 404: the older config serves", status)
	}
	g.stop(t)
}
