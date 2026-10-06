package control

// Totals and counted_through, from the gateway's side (CONTROL-PROTOCOL.md, Messages →
// Totals).

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"kaiak/internal/fakecontrol"
)

// scriptedTotals is a totals message with the given live-gateway count.
func scriptedTotals(live int64) []byte {
	return fmt.Appendf(nil, `{"live_gateways":%d,"counted_through":null,"windows":[]}`, live)
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
	if u := h.nextTotals(); u.Counted != 1 || u.Totals.CountedThrough == nil || u.Totals.CountedThrough.Sequence != 1 {
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
	if u.Counted != 1 || u.Totals.CountedThrough == nil || u.Totals.CountedThrough.Sequence != 1 {
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
		`"counted_through":{"epoch":"00000000000000000000000000000001","sequence":9},"windows":[]}`))
	if u := h.nextTotals(); u.Counted != 0 {
		t.Errorf("update %+v, want nothing counted", u)
	}
}

// counted_through covers batches in send order: every acknowledged or queued batch up
// to the one it names, restored batches of an earlier epoch before the current one;
// acknowledged batches it covers are forgotten.
func TestCountedThroughCoversBatchesInSendOrder(t *testing.T) {
	const restored, current = "00000000000000000000000000000001", "00000000000000000000000000000002"
	u := &usageSender{
		acked:     []BatchPosition{{Epoch: restored, Sequence: 7}, {Epoch: current, Sequence: 1}},
		ackedGens: []uint64{1, 2},
		queue: []spoolEntry{
			{id: BatchID{Epoch: current, Sequence: 2}, generation: 3},
			{id: BatchID{Epoch: current, Sequence: 3}, generation: 4},
		},
	}
	if got := u.countedGeneration(BatchPosition{Epoch: restored, Sequence: 7}); got != 1 || len(u.acked) != 1 {
		t.Fatalf("through the restored batch: generation %d, %d acknowledged left; want 1 and 1", got, len(u.acked))
	}
	if got := u.countedGeneration(BatchPosition{Epoch: current, Sequence: 2}); got != 3 || len(u.acked) != 0 {
		t.Fatalf("through the first queued batch: generation %d, %d acknowledged left; want 3 and 0", got, len(u.acked))
	}
	if got := u.countedGeneration(BatchPosition{Epoch: "00000000000000000000000000000003", Sequence: 1}); got != 0 {
		t.Errorf("another epoch: generation %d, want 0", got)
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
