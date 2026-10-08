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
	"kaiak/internal/config"
	"kaiak/internal/fakecontrol"
	"kaiak/internal/limits"
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
		if u := h.nextTotals(); u.totals.LiveGateways != live+1 || u.counted != 0 {
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
	if u, through := h.nextTotals(), lastCounted(c); u.counted != 1 || len(through) != 1 || through[c.usage.epoch] != 1 {
		t.Fatalf("push update %+v counting through %v, want batch 1's generation counted", u, through)
	}
	// Retired once: the next push counts nothing new.
	h.cp.PushCurrentTotals()
	if u := h.nextTotals(); u.counted != 0 {
		t.Errorf("second push update %+v, want nothing counted", u)
	}
}

// A push that already counts the outstanding batch (its ack lost) reports its
// generation counted; the ack of its resend finds the batch covered and remembers
// nothing, so the next push counts nothing new, and nothing is counted twice.
func TestPushCountingTheOutstandingBatch(t *testing.T) {
	h := newHarness(t)
	h.cp.Publish(configA(t))
	// The batch is counted but its ack is lost; the resends are refused until the
	// test lets them through.
	h.cp.FailUsage(fakecontrol.UsageFault{DropAck: true})
	h.cp.SetUsageFault(&fakecontrol.UsageFault{Status: http.StatusServiceUnavailable, Code: "internal-error"})
	c, obs, _ := h.usageClient(1, nil)
	h.nextStream() // the push needs the stream open
	c.Record(testRecord(1))
	h.wantUsage(fakecontrol.OutcomeAckDropped, 1, 1)

	h.cp.PushCurrentTotals()
	u, through := h.nextTotals(), lastCounted(c)
	if u.counted != 1 || len(through) != 1 || through[c.usage.epoch] != 1 {
		t.Fatalf("push update %+v counting through %v, want totals counting through batch 1 and generation 1 counted", u, through)
	}

	h.cp.SetUsageFault(nil)
	for e := h.nextUsage(); e.Outcome != fakecontrol.OutcomeDuplicate; e = h.nextUsage() {
	}
	for obs.next(t) != BatchAcked {
	}
	if got := c.LimitsContact().UsageUncountedSince; !got.IsZero() {
		t.Errorf("the covered batch waits to be shown counted since %v", got)
	}
	h.cp.PushCurrentTotals()
	if u := h.nextTotals(); u.counted != 0 {
		t.Errorf("update %+v after the ack, want nothing newly counted", u)
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
	if u := h.nextTotals(); u.counted != 0 {
		t.Errorf("update %+v, want nothing counted", u)
	}
}

// counted_through covers batches in send order: every acknowledged or queued batch at
// or before the epoch's entry; acknowledged batches it covers are forgotten.
func TestCountedThroughCoversBatchesInSendOrder(t *testing.T) {
	const epoch = "00000000000000000000000000000001"
	u := &usageSender{
		acked: []ackedBatch{
			{id: BatchID{Epoch: epoch, Sequence: 1}, generation: 1},
			{id: BatchID{Epoch: epoch, Sequence: 2}, generation: 2},
		},
		queue: []queuedBatch{
			{id: BatchID{Epoch: epoch, Sequence: 3}, generation: 3},
			{id: BatchID{Epoch: epoch, Sequence: 4}, generation: 4},
		},
	}
	if got := u.countedGeneration([]BatchPosition{{Epoch: epoch, Sequence: 1}}); got != 1 || len(u.acked) != 1 {
		t.Fatalf("through the first acknowledged batch: generation %d, %d acknowledged left; want 1 and 1", got, len(u.acked))
	}
	if got := u.countedGeneration([]BatchPosition{{Epoch: epoch, Sequence: 3}}); got != 3 || len(u.acked) != 0 {
		t.Fatalf("through the first queued batch: generation %d, %d acknowledged left; want 3 and 0", got, len(u.acked))
	}
	if got := u.countedGeneration([]BatchPosition{{Epoch: "00000000000000000000000000000003", Sequence: 1}}); got != 0 {
		t.Errorf("another epoch: generation %d, want 0", got)
	}
	if got := u.countedGeneration(nil); got != 0 {
		t.Errorf("no entry: generation %d, want 0", got)
	}
}

// Contact starts at the client's creation, so a gateway booting from its seed with
// the control plane down counts its outage from its start; an open stream is contact.
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
	h.wantLoad(load{config.TriggerControl, true})
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
// entry, whatever the other's.
func TestEachEpochIsCoveredByItsOwnEntry(t *testing.T) {
	const oldEpoch, newEpoch = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	u := &usageSender{acked: []ackedBatch{{id: BatchID{Epoch: newEpoch, Sequence: 1}, generation: 1}}}
	if got := u.countedGeneration([]BatchPosition{{Epoch: oldEpoch, Sequence: 1}, {Epoch: newEpoch, Sequence: 1}}); got != 1 {
		t.Fatalf("totals counting both epochs retire generation %d, want 1", got)
	}
}

// An acknowledged batch moves from the queue to the acknowledged batches in one step:
// totals handled at the same moment find it in one or the other, and retire it.
func TestAckHandOffNeverHidesABatchFromTotals(t *testing.T) {
	const epoch = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	batch := UsageBatch{Batch: BatchID{Instance: "gw", Epoch: epoch, Sequence: 1}, Records: []accounting.UsageRecord{testRecord(1)}}
	batch.Records[0].GatewayTime = time.Now()
	for range 2000 {
		e := queuedBatch{id: batch.Batch, generation: 1, records: batch.Records}
		u := &usageSender{queue: []queuedBatch{e}, queuedRecords: 1, changed: make(chan struct{})}
		var counted atomic.Uint64
		c := &Client{usage: u, opts: Options{OnTotals: func(_ limits.Totals, generation uint64) { counted.Store(generation) }}}
		start := make(chan struct{})
		var acked atomic.Bool
		var both sync.WaitGroup
		both.Go(func() {
			<-start
			u.acknowledged(e)
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

// At most ackedRemembered acknowledged batches are remembered: past that the oldest is
// forgotten — a later batch shown counted retires every earlier generation — and the
// wait to be shown counted keeps its time.
func TestAcknowledgedBatchesAreBounded(t *testing.T) {
	const epoch = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	u := &usageSender{changed: make(chan struct{})}
	start := time.Now().Add(-time.Hour)
	for i := range ackedRemembered {
		u.acked = append(u.acked, ackedBatch{id: BatchID{Instance: "gw", Epoch: epoch, Sequence: int64(i + 1)},
			generation: uint64(i + 1), ackedAt: start})
	}
	u.uncountedSince = start
	e := queuedBatch{id: BatchID{Instance: "gw", Epoch: epoch, Sequence: ackedRemembered + 1}, generation: ackedRemembered + 1,
		records: []accounting.UsageRecord{testRecord(1)}}
	u.queue = []queuedBatch{e}
	u.acknowledged(e)
	if len(u.acked) != ackedRemembered || u.acked[0].id.Sequence != 2 {
		t.Errorf("%d acknowledged from sequence %d, want %d from 2", len(u.acked), u.acked[0].id.Sequence, ackedRemembered)
	}
	if !u.uncountedSince.Equal(start) {
		t.Errorf("the wait restarted at %v, want it kept at %v", u.uncountedSince, start)
	}
	if got := u.countedGeneration([]BatchPosition{{Epoch: epoch, Sequence: 5}}); got != 5 || len(u.acked) != ackedRemembered-4 {
		t.Errorf("covering 5 retired generation %d with %d left, want 5 with %d", got, len(u.acked), ackedRemembered-4)
	}
}

// An acknowledged batch waits to be shown counted from its ack until totals cover it,
// heartbeats and further acks notwithstanding: totals that stopped coming are an
// outage once the grace passes. A batch the totals applied
// last already cover does not wait.
func TestAnAcknowledgedBatchWaitsToBeShownCounted(t *testing.T) {
	h := newHarness(t)
	h.cp.Publish(configA(t))
	c, obs, _ := h.usageClient(1, nil)
	st := h.nextStream()
	st.Send("totals", scriptedTotals(1))
	h.nextTotals()
	before := time.Now()
	c.Record(testRecord(1))
	first := h.wantUsage(fakecontrol.OutcomeCounted, 1, 1)
	if r := obs.next(t); r != BatchAcked {
		t.Fatalf("result %s, want %s", r, BatchAcked)
	}
	since := c.LimitsContact().UsageUncountedSince
	if since.Before(before) {
		t.Fatalf("waiting to be shown counted since %v after the ack, want from the ack", since)
	}
	if !c.LimitsContact().UsageWaitingSince.IsZero() {
		t.Error("an acknowledged batch still waits for an answer")
	}
	c.Record(testRecord(2))
	h.wantUsage(fakecontrol.OutcomeCounted, 2, 1)
	if r := obs.next(t); r != BatchAcked {
		t.Fatalf("result %s, want %s", r, BatchAcked)
	}
	if got := c.LimitsContact().UsageUncountedSince; !got.Equal(since) {
		t.Errorf("a later ack moved the wait to %v, want it kept at %v", got, since)
	}
	st.Send("totals", fmt.Appendf(nil, `{"live_gateways":1,"counted_through":[{"epoch":%q,"sequence":2}],"windows":[]}`,
		first.Batch.Epoch))
	h.nextTotals()
	if got := c.LimitsContact().UsageUncountedSince; !got.IsZero() {
		t.Errorf("still waiting since %v with every batch shown counted", got)
	}
}

// An ack is not contact: it brings no totals, so a stream that delivers nothing while
// acks still come is an outage once the grace passes.
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
