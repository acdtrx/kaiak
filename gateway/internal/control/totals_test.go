package control

// Totals and counted_through, from the gateway's side (CONTROL-PROTOCOL.md, Messages →
// Totals).

import (
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"kaiak/internal/accounting"
	"kaiak/internal/fakecontrol"
)

// scriptedTotals is a totals message with the given live-gateway count.
func scriptedTotals(live int64) []byte {
	return fmt.Appendf(nil, `{"live_gateways":%d,"counted_through":[],"windows":[]}`, live)
}

// Every totals event is applied, in the order the stream delivers it: keeping that
// order is the control plane's job (CONTROL-PROTOCOL.md, Config stream → Order).
func TestEveryTotalsEventIsAppliedInStreamOrder(t *testing.T) {
	h := newHarness(t)
	h.cp.Publish(configA(t))
	c := h.client(nil)
	h.boot(c)
	h.run(c)
	st := h.nextStream()

	for live := range int64(3) {
		st.Send("totals", scriptedTotals(live+1))
	}
	for live := range int64(3) {
		if u := h.nextTotals(); u.Totals.LiveGateways != live+1 || u.Counted != 0 {
			t.Fatalf("update %+v, want the totals with live %d", u, live+1)
		}
	}
	select {
	case u := <-h.totals:
		t.Errorf("unexpected update %+v", u)
	default:
	}
}

// An ack carries no totals and retires nothing: the acknowledged batch's usage leaves
// with the stream totals that show it counted.
func TestAckedBatchLeavesWithTheTotalsThatCountIt(t *testing.T) {
	h := newHarness(t)
	h.cp.Publish(configA(t))
	c, obs, _ := h.usageClient(1, nil)
	h.nextStream() // the push needs the stream open
	c.Record(testRecord(1))
	h.wantUsage(fakecontrol.OutcomeCounted, 1, 1)
	if r := obs.next(t); r != BatchAcked {
		t.Fatalf("result %s, want %s", r, BatchAcked)
	}
	select {
	case u := <-h.totals:
		t.Fatalf("update %+v on an ack, want none: totals come on the stream", u)
	default:
	}

	h.cp.PushCurrentTotals()
	if u := h.nextTotals(); u.Counted != 1 || len(u.Totals.CountedThrough) != 1 || u.Totals.CountedThrough[0].Sequence != 1 {
		t.Fatalf("push update %+v, want batch 1's generation counted", u)
	}
	// Retired once: the next push counts nothing new.
	h.cp.PushCurrentTotals()
	if u := h.nextTotals(); u.Counted != 0 {
		t.Errorf("second push update %+v, want nothing counted", u)
	}
}

// A push that already counts the outstanding batch (its ack lost) reports its
// generation counted; the ack of its resend delivers nothing, and nothing is counted
// twice.
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
	if u.Counted != 1 || len(u.Totals.CountedThrough) != 1 || u.Totals.CountedThrough[0].Sequence != 1 {
		t.Fatalf("push update %+v, want totals counting through batch 1 and generation 1 counted", u)
	}

	h.cp.SetUsageFault(nil)
	for e := h.nextUsage(); e.Outcome != fakecontrol.OutcomeDuplicate; e = h.nextUsage() {
	}
	h.cp.PushCurrentTotals()
	if u := h.nextTotals(); u.Counted != 1 {
		t.Errorf("update %+v after the ack, want generation 1 counted again and nothing else", u)
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
	st.Send("totals", []byte(`{"live_gateways":1,`+
		`"counted_through":[{"epoch":"00000000000000000000000000000001","sequence":9}],"windows":[]}`))
	if u := h.nextTotals(); u.Counted != 0 {
		t.Errorf("update %+v, want nothing counted", u)
	}
}

// counted_through covers batches in send order, each by its own epoch's entry: every
// acknowledged or queued batch at or before it, restored batches of an earlier epoch
// before the current one; acknowledged batches it covers are forgotten.
func TestCountedThroughCoversBatchesInSendOrder(t *testing.T) {
	const restored, current = "00000000000000000000000000000001", "00000000000000000000000000000002"
	future := time.Now().Add(time.Hour)
	u := &usageSender{
		acked: []ackedBatch{
			{id: BatchID{Epoch: restored, Sequence: 7}, generation: 1, clearedAt: future},
			{id: BatchID{Epoch: current, Sequence: 1}, generation: 2, clearedAt: future},
		},
		queue: []spoolEntry{
			{id: BatchID{Epoch: current, Sequence: 2}, generation: 3},
			{id: BatchID{Epoch: current, Sequence: 3}, generation: 4},
		},
	}
	if got := u.countedGeneration([]BatchPosition{{Epoch: restored, Sequence: 7}}); got != 1 || len(u.acked) != 1 {
		t.Fatalf("through the restored batch: generation %d, %d acknowledged left; want 1 and 1", got, len(u.acked))
	}
	if got := u.countedGeneration([]BatchPosition{{Epoch: restored, Sequence: 7}, {Epoch: current, Sequence: 2}}); got != 3 || len(u.acked) != 0 {
		t.Fatalf("through the first queued batch: generation %d, %d acknowledged left; want 3 and 0", got, len(u.acked))
	}
	if got := u.countedGeneration([]BatchPosition{{Epoch: "00000000000000000000000000000003", Sequence: 1}}); got != 0 {
		t.Errorf("another epoch: generation %d, want 0", got)
	}
	if got := u.countedGeneration(nil); got != 0 {
		t.Errorf("no entry: generation %d, want 0", got)
	}
}

// Contact starts at the client's creation, so a gateway booting from its last-known-
// good copy with the control plane down counts its outage from its start; an open
// stream is contact.
func TestContact(t *testing.T) {
	h := newHarness(t)
	h.cp.SetDown(true)
	before := time.Now()
	c := h.client(func(o *Options) { o.BootWait = 50 * time.Millisecond })
	connected, created := c.Contact()
	if connected || created.Before(before) || created.After(time.Now()) {
		t.Fatalf("Contact() = %v, %v at creation", connected, created)
	}
	h.boot(c)
	if connected, last := c.Contact(); connected || !last.Equal(created) {
		t.Errorf("Contact() = %v, %v after a failed boot, want no contact since creation", connected, last)
	}

	h.cp.Publish(configA(t))
	h.cp.SetDown(false)
	h.run(c)
	h.wantLoad(load{TriggerControl, true})
	if _, last := c.Contact(); !last.After(created) {
		t.Errorf("last contact %v, want the stream after %v", last, created)
	}
	st := h.nextStream()
	st.Send("totals", scriptedTotals(1))
	h.nextTotals() // the stream is being read
	if connected, _ := c.Contact(); !connected {
		t.Error("not connected with the stream open")
	}
}

// A gateway process replaced under the same instance name (a new epoch) has its first
// batch acknowledged while the old process's stalled write lands after it: the totals
// then name both epochs, and the new process retires its own batch by its epoch's
// entry, whatever the other's (AUDIT-2 2M2).
func TestEachEpochIsCoveredByItsOwnEntry(t *testing.T) {
	const oldEpoch, newEpoch = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	u := &usageSender{acked: []ackedBatch{
		{id: BatchID{Epoch: newEpoch, Sequence: 1}, generation: 1, clearedAt: time.Now().Add(time.Hour)}}}
	if got := u.countedGeneration([]BatchPosition{{Epoch: oldEpoch, Sequence: 1}, {Epoch: newEpoch, Sequence: 1}}); got != 1 {
		t.Fatalf("totals counting both epochs retire generation %d, want 1", got)
	}
}

// An acknowledged batch moves from the queue to the acknowledged batches in one step:
// totals handled at the same moment find it in one or the other, and retire it
// (AUDIT-2 2M3).
func TestAckHandOffNeverHidesABatchFromTotals(t *testing.T) {
	const epoch = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	batch := UsageBatch{Batch: BatchID{Instance: "gw", Epoch: epoch, Sequence: 1}, Records: []accounting.UsageRecord{testRecord(1)}}
	batch.Records[0].GatewayTime = time.Now()
	for range 2000 {
		e := spoolEntry{id: batch.Batch, generation: 1, records: 1}
		u := &usageSender{queue: []spoolEntry{e}, queuedRecords: 1, changed: make(chan struct{})}
		var counted atomic.Uint64
		c := &Client{usage: u, opts: Options{OnTotals: func(update TotalsUpdate) { counted.Store(update.Counted) }}}
		start := make(chan struct{})
		var acked atomic.Bool
		var both sync.WaitGroup
		both.Go(func() {
			<-start
			u.acknowledged(e, batch)
			acked.Store(true)
		})
		both.Go(func() {
			<-start
			// Wait for the ack to hold the lock (or be done), then ask for it: the
			// totals come in as the batch moves.
			for !acked.Load() && u.mu.TryLock() {
				u.mu.Unlock()
			}
			c.takeTotals(Totals{CountedThrough: []BatchPosition{{Epoch: epoch, Sequence: 1}}}, false)
		})
		close(start)
		both.Wait()
		if got := counted.Load(); got != 1 {
			t.Fatalf("totals counting the batch during its ack retired generation %d, want 1", got)
		}
	}
}

// The acknowledged batches whose usage the limiter's windows have already left behind
// are forgotten at the next ack: covering them retires nothing, and while the stream is
// down and acks still come they would pile up (AUDIT-2 2L6). A batch that cost nothing
// is held only by the hour of its last record; one that cost something, by its month.
func TestAcknowledgedBatchesAreForgottenOnceTheirWindowsPassed(t *testing.T) {
	at := func(s string) time.Time {
		t.Helper()
		v, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	free := testRecord(0)
	free.GatewayTime = at("2026-09-24T10:59:59.5Z")
	priced := testRecord(1)
	priced.CostNanoUSD, priced.GatewayTime = 7, at("2026-09-24T10:00:00Z")
	if got := clearedAt(UsageBatch{Records: []accounting.UsageRecord{free}}); !got.Equal(at("2026-09-24T11:00:00Z")) {
		t.Errorf("free batch cleared at %v, want the end of its hour", got)
	}
	if got := clearedAt(UsageBatch{Records: []accounting.UsageRecord{free, priced}}); !got.Equal(at("2026-10-01T00:00:00Z")) {
		t.Errorf("priced batch cleared at %v, want the end of its month", got)
	}

	const epoch = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	recent := testRecord(2)
	recent.GatewayTime = time.Now()
	u := &usageSender{changed: make(chan struct{}), acked: []ackedBatch{
		{id: BatchID{Epoch: epoch, Sequence: 1}, generation: 1, clearedAt: time.Now().Add(-time.Second)},
		{id: BatchID{Epoch: epoch, Sequence: 2}, generation: 2, clearedAt: time.Now().Add(time.Hour)},
	}}
	e := spoolEntry{id: BatchID{Epoch: epoch, Sequence: 3}, generation: 3, records: 1}
	u.queue = []spoolEntry{e}
	u.acknowledged(e, UsageBatch{Batch: e.id, Records: []accounting.UsageRecord{recent}})
	if len(u.acked) != 2 || u.acked[0].id.Sequence != 2 || u.acked[1].id.Sequence != 3 {
		t.Errorf("acknowledged batches %+v, want 2 and 3: 1's windows have passed", u.acked)
	}
}

// An ack is not contact: it brings no totals, so a stream that delivers nothing while
// acks still come is an outage once the grace passes (AUDIT-2 2M6).
func TestAnAckIsNotContact(t *testing.T) {
	h := newHarness(t)
	h.cp.Publish(configA(t))
	c, obs, _ := h.usageClient(1, nil)
	st := h.nextStream()
	st.Send("totals", scriptedTotals(1))
	h.nextTotals()
	_, before := c.Contact()
	c.Record(testRecord(1))
	h.wantUsage(fakecontrol.OutcomeCounted, 1, 1)
	if r := obs.next(t); r != BatchAcked {
		t.Fatalf("result %s, want %s", r, BatchAcked)
	}
	if _, last := c.Contact(); !last.Equal(before) {
		t.Errorf("last contact moved from %v to %v on an ack, want it unmoved", before, last)
	}
}
