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

const (
	controlPlaneA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	controlPlaneB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// scriptedTotals is a totals message with the given revision and live-gateway count.
func scriptedTotals(controlPlane string, sequence, live int64) []byte {
	return fmt.Appendf(nil, `{"revision":{"control_plane":%q,"sequence":%d},"config_epoch":"c0ffee00c0ffee00c0ffee00c0ffee00","config_version":1,"live_gateways":%d,"counted_through":null,"windows":[]}`,
		controlPlane, sequence, live)
}

func TestTotalsAreAppliedInRevisionOrder(t *testing.T) {
	h := newHarness(t)
	h.cp.Publish(configA(t))
	c := h.client(nil)
	c.Boot(context.Background())
	h.run(c)
	st := h.nextStream()

	// live_gateways tells the messages apart; each step names the next one applied.
	steps := []struct {
		controlPlane   string
		sequence, live int64
		applied        bool
	}{
		{controlPlaneA, 5, 1, true},
		{controlPlaneA, 4, 2, false}, // older
		{controlPlaneA, 5, 3, false}, // the same revision
		{controlPlaneA, 6, 4, true},
		{controlPlaneB, 1, 5, true},  // a restarted control plane: adopted, lower sequence and all
		{controlPlaneB, 1, 6, false}, // ordered from there
		{controlPlaneB, 2, 7, true},
	}
	for _, s := range steps {
		st.Send("totals", "", scriptedTotals(s.controlPlane, s.sequence, s.live))
	}
	for _, s := range steps {
		if !s.applied {
			continue
		}
		u := h.nextTotals()
		if u.Totals == nil || u.Totals.LiveGateways != s.live || u.Counted != 0 {
			t.Fatalf("update %+v, want the totals of %s/%d (live %d)", u, s.controlPlane[:1], s.sequence, s.live)
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
	st.Send("totals", "", []byte(`{"revision":{"control_plane":"`+controlPlaneA+`","sequence":1},"config_epoch":"c0ffee00c0ffee00c0ffee00c0ffee00","config_version":1,"live_gateways":1,`+
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
	st.Send("totals", "", scriptedTotals(controlPlaneA, 1, 1))
	h.nextTotals() // the stream is being read
	if connected, _ := c.Contact(); !connected {
		t.Error("not connected with the stream open")
	}
}

// The independent audit's finding 5: a totals message from a control-plane process
// the gateway has moved away from — a delayed ack from the process a restart
// replaced — never replaces the current process's totals (A 100 → B 200 → A 150
// applied 150 and reopened spent budget). Its counted_through still retires the
// batches it counted: that process counted them before it was replaced, so the
// replacement's totals include them — nothing counted twice, nothing lost.
func TestTotalsFromAReplacedControlPlaneAreIgnored(t *testing.T) {
	const epoch = "00000000000000000000000000000001"
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
		{id: BatchID{Epoch: epoch, Sequence: 1}, generation: 1},
		{id: BatchID{Epoch: epoch, Sequence: 2}, generation: 2},
		{id: BatchID{Epoch: epoch, Sequence: 3}, generation: 3},
	}}
	take := func(process string, seq, used, countedThrough int64, acked uint64) {
		c.takeTotals(Totals{Revision: Revision{ControlPlane: process, Sequence: seq},
			CountedThrough: &BatchPosition{Epoch: epoch, Sequence: countedThrough},
			Windows:        []TotalsWindow{{Used: used}}}, acked)
	}
	take(controlPlaneA, 10, 100, 1, 1) // A acks batch 1
	take(controlPlaneB, 1, 200, 1, 0)  // A restarted as B: B's stream
	take(controlPlaneA, 11, 150, 2, 2) // A's delayed ack of batch 2, after B's totals
	take(controlPlaneA, 12, 170, 2, 0) // anything else of A's
	take(controlPlaneB, 2, 260, 3, 3)  // B acks batch 3
	want := []applied{{100, 1}, {200, 1}, {-1, 2}, {-1, 2}, {260, 3}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("updates %v, want %v (used -1: no totals)", got, want)
	}
	if c.revision.ControlPlane != controlPlaneB || c.revision.Sequence != 2 {
		t.Errorf("revision %+v, want B's 2", *c.revision)
	}

	// The processes moved away from are remembered up to a bound: the oldest is
	// forgotten first.
	for i := range maxRetiredControlPlanes + 1 {
		take(fmt.Sprintf("%032x", i+1), 1, 300+int64(i), 3, 0)
	}
	if len(c.retired) != maxRetiredControlPlanes || c.retired[0] == controlPlaneA || c.retired[0] == controlPlaneB {
		t.Errorf("retired %v, want the latest %d, A and B forgotten", c.retired, maxRetiredControlPlanes)
	}
}
