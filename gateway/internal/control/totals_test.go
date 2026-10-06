package control

// Totals ordering and counted_through, from the gateway's side (CONTROL-PROTOCOL.md,
// Messages → Totals).

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"kaiak/internal/fakecontrol"
)

// scriptedTotals is a totals message of the given config epoch with the given
// revision and live-gateway count.
func scriptedTotals(epoch string, revision, live int64) []byte {
	return fmt.Appendf(nil, `{"revision":%d,"config_epoch":%q,"config_version":1,"live_gateways":%d,"counted_through":null,"windows":[]}`,
		revision, epoch, live)
}

func TestTotalsAreAppliedInRevisionOrder(t *testing.T) {
	h := newHarness(t)
	h.cp.Publish(configA(t))
	c := h.client(nil)
	c.Boot(context.Background())
	h.run(c)
	st := h.nextStream()
	epoch := h.cp.ConfigEpoch()

	// live_gateways tells the messages apart; each step names the next one applied.
	steps := []struct {
		revision, live int64
		applied        bool
	}{
		{5, 1, true},
		{4, 2, false}, // older: another process's delayed answer, or a reordered ack
		{5, 3, false}, // the same revision
		{6, 4, true},
	}
	for _, s := range steps {
		st.Send("totals", "", scriptedTotals(epoch, s.revision, s.live))
	}
	for _, s := range steps {
		if !s.applied {
			continue
		}
		u := h.nextTotals()
		if u.Totals == nil || u.Totals.LiveGateways != s.live || u.Counted != 0 {
			t.Fatalf("update %+v, want the totals of revision %d (live %d)", u, s.revision, s.live)
		}
	}
	select {
	case u := <-h.totals:
		t.Errorf("unexpected update %+v", u)
	default:
	}
}

// A push that already counts the outstanding batch works like its ack: the batch's
// generation is reported counted with those totals, and the ack that follows (same
// revision) reports it again without totals — nothing is counted twice or lost.
func TestPushCountingTheOutstandingBatch(t *testing.T) {
	h := newHarness(t)
	h.cp.Publish(configA(t))
	// The batch is counted but its ack is lost; the resends are refused until the
	// test lets them through.
	h.cp.FailUsage(fakecontrol.UsageFault{DropAck: true})
	h.cp.SetUsageFault(&fakecontrol.UsageFault{Status: http.StatusServiceUnavailable, Code: "internal-error"})
	c, _, _ := h.usageClient(1, nil)
	h.nextStream() // the push needs the stream open
	c.Record(testRecord(1))
	h.wantUsage(fakecontrol.OutcomeAckDropped, 1, 1)

	h.cp.PushCurrentTotals()
	u := h.nextTotals()
	if u.Totals == nil || u.Counted != 1 || u.Totals.CountedThrough == nil || u.Totals.CountedThrough.Sequence != 1 {
		t.Fatalf("push update %+v, want totals counting through batch 1 and generation 1 counted", u)
	}

	h.cp.SetUsageFault(nil)
	for e := h.nextUsage(); e.Outcome != fakecontrol.OutcomeDuplicate; e = h.nextUsage() {
	}
	u = h.nextTotals()
	if u.Totals != nil || u.Counted != 1 {
		t.Errorf("ack update %+v, want generation 1 counted and no totals (same revision as the push)", u)
	}
	if n := len(h.cp.CountedRecords()); n != 1 {
		t.Errorf("%d records counted, want 1", n)
	}
}

// counted_through of another epoch counts none of this process's batches.
func TestCountedThroughOfAnotherEpoch(t *testing.T) {
	h := newHarness(t)
	h.cp.Publish(configA(t))
	h.cp.SetUsageFault(&fakecontrol.UsageFault{Status: http.StatusServiceUnavailable, Code: "internal-error"})
	c, _, _ := h.usageClient(1, nil)
	st := h.nextStream()
	c.Record(testRecord(1))
	h.wantUsage(fakecontrol.OutcomeRefused, 1, 1)
	st.Send("totals", "", []byte(`{"revision":1,"config_epoch":"`+h.cp.ConfigEpoch()+`","config_version":1,"live_gateways":1,`+
		`"counted_through":{"epoch":"00000000000000000000000000000001","sequence":9},"windows":[]}`))
	if u := h.nextTotals(); u.Totals == nil || u.Counted != 0 {
		t.Errorf("update %+v, want totals and nothing counted", u)
	}
}

// Contact starts at the client's creation, so a gateway booting from its last-known-
// good copy with the control plane down counts its outage from its start; a snapshot
// and an open stream are contact.
func TestContact(t *testing.T) {
	h := newHarness(t)
	h.cp.SetDown(true)
	before := time.Now()
	c := h.client(func(o *Options) { o.BootWait = 50 * time.Millisecond })
	connected, created := c.Contact()
	if connected || created.Before(before) || created.After(time.Now()) {
		t.Fatalf("Contact() = %v, %v at creation", connected, created)
	}
	c.Boot(context.Background())
	if connected, last := c.Contact(); connected || !last.Equal(created) {
		t.Errorf("Contact() = %v, %v after a failed boot, want no contact since creation", connected, last)
	}

	h.cp.Publish(configA(t))
	h.cp.SetDown(false)
	h.run(c)
	h.wantLoad(load{TriggerControl, true})
	if _, last := c.Contact(); !last.After(created) {
		t.Errorf("last contact %v, want the snapshot fetch after %v", last, created)
	}
	st := h.nextStream()
	st.Send("totals", "", scriptedTotals(h.cp.ConfigEpoch(), 1, 1))
	h.nextTotals() // the stream is being read
	if connected, _ := c.Contact(); !connected {
		t.Error("not connected with the stream open")
	}
}

// Totals apply only when they are of the config epoch the gateway runs (CONTROL-
// PROTOCOL.md, Messages → Totals): a store that started over takes a new epoch, so a
// delayed answer from the old one (A 100 → B 200 → A 150) can never replace the new
// store's totals, and the new store's totals wait for its config. Ordering starts
// again in each epoch. counted_through counts whatever the epoch of the totals: those
// batches were counted before.
func TestTotalsFollowTheRunningConfigEpoch(t *testing.T) {
	const (
		batchEpoch = "00000000000000000000000000000001"
		storeA     = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		storeB     = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	)
	type applied struct {
		used    int64 // -1: no totals
		counted uint64
	}
	var got []applied
	c := &Client{logger: slog.New(slog.DiscardHandler), opts: Options{OnTotals: func(u TotalsUpdate) {
		a := applied{used: -1, counted: u.Counted}
		if u.Totals != nil {
			a.used = u.Totals.Windows[0].Used
		}
		got = append(got, a)
	}}}
	c.usage = &usageSender{queue: []spoolEntry{
		{id: BatchID{Epoch: batchEpoch, Sequence: 1}, generation: 1},
		{id: BatchID{Epoch: batchEpoch, Sequence: 2}, generation: 2},
		{id: BatchID{Epoch: batchEpoch, Sequence: 3}, generation: 3},
	}}
	take := func(store string, revision, used, countedThrough int64, acked uint64) {
		c.takeTotals(Totals{Revision: Revision(revision), ConfigEpoch: store,
			CountedThrough: &BatchPosition{Epoch: batchEpoch, Sequence: countedThrough},
			Windows:        []TotalsWindow{{Used: used}}}, acked)
	}
	c.applied = &configPosition{epoch: storeA, version: 4}
	take(storeA, 10, 100, 1, 1) // A acks batch 1
	take(storeB, 1, 200, 1, 0)  // A started over as B: B's totals wait for B's config
	c.applied = &configPosition{epoch: storeB, version: 1}
	take(storeB, 1, 200, 1, 0)  // B's config applied: B's totals, ordering anew
	take(storeA, 11, 150, 2, 2) // A's delayed ack of batch 2, after B's totals
	take(storeB, 1, 210, 2, 0)  // not newer in B's epoch
	take(storeB, 2, 260, 3, 3)  // B acks batch 3
	want := []applied{{100, 1}, {-1, 1}, {200, 1}, {-1, 2}, {-1, 2}, {260, 3}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("updates %v, want %v (used -1: no totals)", got, want)
	}
	if c.lastTotals.epoch != storeB || c.lastTotals.revision != 2 {
		t.Errorf("last totals %+v, want B's 2", *c.lastTotals)
	}
}
