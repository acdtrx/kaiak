package e2e

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"kaiak/internal/fakebackend"
	"kaiak/internal/fakecontrol"
)

// TestControlModeEndToEnd runs kaiak from a control plane (the Go test double; the
// sample control plane joins in its own end-to-end test): boot from the stream, a
// served request, a config pushed on the stream; usage batches reaching the control
// plane, a batch whose ack was lost resent with its ID and counted once, the drain
// flushing the last records.
func TestControlModeEndToEnd(t *testing.T) {
	backend := fakebackend.New()
	defer backend.Close()
	const token = "e2e-control-token"
	cp := fakecontrol.New(token)
	defer cp.Close()
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

	g := startGatewayEnv(t, controlEnv(cp.URL(), token))
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
		if got := g.metric(t, `kaiak_config_loads_total{kaiak_trigger="control",kaiak_config_result="applied"}`); got != 2 {
			t.Errorf("control loads applied = %v, want 2", got)
		}
	})

	t.Run("usage batches reach the control plane", func(t *testing.T) {
		const acked = `kaiak_usage_batch_sends_total{kaiak_usage_batch_result="acked"}`
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

	t.Run("a batch whose ack is lost is resent with its ID, counted once", func(t *testing.T) {
		cp.FailUsage(fakecontrol.UsageFault{DropAck: true})
		if r := g.post(t, "/v1/chat/completions", evalKey, "e2e-usage-2", chatBody("chat", false, nil)); r.StatusCode != http.StatusOK {
			t.Fatalf("chat: %d %s", r.StatusCode, r.body)
		}
		lost := waitUsage(t, cp, "the batch whose ack is dropped", func(e fakecontrol.UsageEvent) bool {
			return e.Outcome == fakecontrol.OutcomeAckDropped
		})
		resent := waitUsage(t, cp, "the resent batch", func(e fakecontrol.UsageEvent) bool {
			return e.Outcome == fakecontrol.OutcomeDuplicate
		})
		if resent.Batch != lost.Batch || lost.Batch.Sequence == 0 {
			t.Errorf("resent batch %+v, lost ack for %+v", resent.Batch, lost.Batch)
		}
		if n := countedIDs(t, cp)["e2e-usage-2"]; n != 1 {
			t.Errorf("e2e-usage-2 counted %d times, want 1", n)
		}
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
	g := startGatewayEnv(t, controlEnv(cp.URL(), token))
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
