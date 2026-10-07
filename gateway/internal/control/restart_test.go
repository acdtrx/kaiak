package control

// The spool across a restart with totals.json: acknowledged batches stay spooled until
// a save covering them has completed, are never sent again, and their usage is rebuilt
// into the limits unless the saved totals include it (docs/specs/GATEWAY.md, Limits →
// Control-plane mode: Restart keeps the last totals; Usage spool → Write policy).

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"kaiak/internal/config"
	"kaiak/internal/fakecontrol"
	"kaiak/internal/limits"
)

// After a restart the acknowledged batches are remembered, not sent; those the saved
// totals cover leave the spool, the others are handed over for rebuilding and wait to
// be shown counted from the restart.
func TestAcknowledgedBatchesSurviveARestartUntilCovered(t *testing.T) {
	h := newHarness(t)
	h.cp.Publish(configA(t))
	c, _, stop := h.usageClient(1, nil)
	for i := range 3 {
		c.Record(testRecord(i))
	}
	h.wantUsage(fakecontrol.OutcomeCounted, 1, 1)
	h.wantUsage(fakecontrol.OutcomeCounted, 2, 1)
	last := h.wantUsage(fakecontrol.OutcomeCounted, 3, 1)
	flush(t, c)
	stop()
	epoch := last.Batch.Epoch

	before := time.Now()
	c2 := h.client(func(o *Options) { o.BatchMaxRecords = 1; o.BatchInterval = time.Hour })
	if err := h.boot(c2); err != nil {
		t.Fatal(err)
	}
	// The saved totals include batch 1: its usage is in the restored bases.
	spooled := c2.RestoreSpooled([]BatchPosition{{Epoch: epoch, Sequence: 1}})
	if len(spooled) != 2 || len(spooled[0].Records) != 1 || spooled[0].Generation >= spooled[1].Generation {
		t.Fatalf("restored %+v, want batches 2 and 3 in send order", spooled)
	}
	if files := h.spoolFiles(spoolBatchPrefix); len(files) != 2 {
		t.Errorf("spool %v, want batch 1 gone and 2 and 3 kept", files)
	}
	if since := c2.UsageUncountedSince(); since.Before(before) {
		t.Errorf("restored acknowledged batches wait since %v, want since the restart", since)
	}

	h.run(c2)
	st := h.nextStream()
	c2.Record(testRecord(4))
	h.wantUsage(fakecontrol.OutcomeCounted, 4, 1) // nothing acknowledged is sent again
	st.Send("totals", fmt.Appendf(nil, `{"live_gateways":1,"counted_through":[{"epoch":%q,"sequence":4}],"windows":[]}`, epoch))
	if u := h.nextTotals(); u.Counted != spooled[1].Generation+1 {
		t.Errorf("totals counted generation %d, want %d: the restored batches and the new one", u.Counted, spooled[1].Generation+1)
	}
	if since := c2.UsageUncountedSince(); !since.IsZero() {
		t.Errorf("still waiting since %v with every batch shown counted", since)
	}
}

// Spend settled and spooled after the last totals.json write survives a crash: the
// restored totals plus the usage rebuilt from the spool refuse a request the budget no
// longer has room for, the control plane still down (AUDIT-3 3H2, [C] C2).
func TestACrashRestoresTheSpooledSpendIntoTheLimits(t *testing.T) {
	h := newHarness(t)
	var doc map[string]any
	if err := json.Unmarshal(configA(t), &doc); err != nil {
		t.Fatal(err)
	}
	doc["global"] = map[string]any{"limits": []any{map[string]any{"type": "usd_per_month", "value": 1}}}
	doc["models"].(map[string]any)["llama"].(map[string]any)["prices"] = []any{map[string]any{"effective_from": "2020-01-01",
		"tiers": []any{map[string]any{"above_input_tokens": 0, "usd_per_million": map[string]any{"tokens_in": 1}}}}}
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := config.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	h.holder.Swap(snap)
	now := time.Now().UTC()
	contact := func() limits.Contact { return limits.Contact{Connected: true, Last: now} }
	l := limits.NewShared(h.holder, func() time.Time { return now }, contact, nil)
	l.TakeTotals(limits.Totals{Complete: true}, 0)
	if _, err := l.SaveShared(h.dir); err != nil { // the last write before the crash
		t.Fatal(err)
	}
	client := h.client(nil)
	subject := limits.Subject{Groups: []string{"me"}, Model: "llama", Priced: true}
	reservation, rejection := l.Reserve(subject, 1)
	if rejection != nil {
		t.Fatal(rejection)
	}
	rec := testRecord(1)
	rec.Groups, rec.GatewayTime, rec.CostNanoUSD = subject.Groups, now, 1_000_000_000
	rec.Generation = client.Record(rec)
	l.Settle(reservation, rec)
	client.usage.seal("test") // spooled; the process dies before the next write

	restarted := h.client(nil)
	after := limits.NewShared(h.holder, func() time.Time { return now }, contact, nil)
	r, err := after.LoadShared(h.dir)
	if err != nil || !r.Found {
		t.Fatalf("restore %+v (%v)", r, err)
	}
	counted := make([]BatchPosition, len(r.CountedThrough))
	for i, c := range r.CountedThrough {
		counted[i] = BatchPosition{Epoch: c.Epoch, Sequence: c.Sequence}
	}
	var own []limits.OwnBatch
	for _, b := range restarted.RestoreSpooled(counted) {
		own = append(own, limits.OwnBatch{Generation: b.Generation, Records: b.Records})
	}
	after.RestoreOwn(own)
	if _, rejection := after.Reserve(subject, 1); rejection == nil {
		t.Fatal("a restart admitted a priced request though its spooled spend fills the budget")
	}
}
