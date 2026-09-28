package control

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// nextStatus waits for the next status report the control plane accepts, checked
// against the status schema and rules.
func (h *harness) nextStatus() Status {
	h.t.Helper()
	select {
	case body := <-h.cp.StatusEvents():
		s, err := DecodeStatus(body)
		if err != nil {
			h.t.Fatalf("status %s breaks the protocol: %v", body, err)
		}
		return s
	case <-time.After(testWaitLimit):
		h.t.Fatal("no status report received")
		return Status{}
	}
}

// statusUntil waits for a status report that match accepts.
func (h *harness) statusUntil(what string, match func(Status) bool) Status {
	h.t.Helper()
	deadline := time.After(testWaitLimit)
	for {
		select {
		case body := <-h.cp.StatusEvents():
			s, err := DecodeStatus(body)
			if err != nil {
				h.t.Fatalf("status %s breaks the protocol: %v", body, err)
			}
			if match(s) {
				return s
			}
		case <-deadline:
			h.t.Fatalf("no status report with %s", what)
			return Status{}
		}
	}
}

func TestFirstStatusReportsWhatTheGatewayRuns(t *testing.T) {
	h := newHarness(t)
	h.cp.Publish(configA(t))
	started := time.Date(2026, 9, 24, 9, 58, 12, 500_000_000, time.FixedZone("EEST", 3*3600))
	c := h.client(func(o *Options) {
		o.StartedAt = started
		o.Serving = func() Serving {
			return Serving{
				Backends: map[string]BackendStatus{
					"local": {InFlight: 2, MaxInFlight: 4, Deployments: map[string]DeploymentStatus{"llama": {Circuit: CircuitClosed}}},
					"gone":  {InFlight: 1},
				},
				Models: map[string]ModelStatus{"llama": {Queued: 0}},
			}
		}
	})
	c.Boot(context.Background())
	h.run(c)
	s := h.nextStatus()
	if s.Instance != testInstance || s.ProtocolVersion != 2 || s.State != StateReady || !s.StartedAt.Equal(started) ||
		s.AppliedConfigVersion == nil || *s.AppliedConfigVersion != 1 || s.LastRejection != nil ||
		len(s.Backends) != 2 || len(s.Models) != 1 {
		t.Errorf("status %+v", s)
	}
	if b := s.Backends["local"]; b.InFlight != 2 || b.MaxInFlight != 4 || b.Deployments["llama"].Circuit != CircuitClosed {
		t.Errorf("backend local %+v", b)
	}
	if b := s.Backends["gone"]; b.InFlight != 1 || b.MaxInFlight != 0 || b.Deployments == nil || len(b.Deployments) != 0 {
		t.Errorf("backend gone %+v, want its in-flight count, no cap and no deployments", b)
	}
	if s.StartedAt.Location() != time.UTC {
		t.Errorf("started_at %v, want UTC", s.StartedAt)
	}
}

func TestStatusIsStartingUntilAConfigIsApplied(t *testing.T) {
	h := newHarness(t)
	c := h.client(nil)
	c.Boot(context.Background())
	h.run(c)
	if s := h.nextStatus(); s.State != StateStarting || s.AppliedConfigVersion != nil || s.Backends == nil || s.Models == nil {
		t.Errorf("status %+v, want starting with nothing applied", s)
	}
	h.cp.Publish(configA(t))
	h.statusUntil("state ready", func(s Status) bool {
		return s.State == StateReady && s.AppliedConfigVersion != nil && *s.AppliedConfigVersion == 1
	})
}

func TestStatusIsSentOnTheInterval(t *testing.T) {
	h := newHarness(t)
	h.cp.Publish(configA(t))
	c := h.client(func(o *Options) { o.StatusInterval = 10 * time.Millisecond })
	c.Boot(context.Background())
	h.run(c)
	for range 4 {
		h.nextStatus()
	}
}

func TestStatusFollowsRejectionsAndTheDrain(t *testing.T) {
	h := newHarness(t)
	h.cp.Publish(configA(t))
	h.cp.Publish(configA(t))
	h.cp.Publish(configA(t))
	c := h.client(nil) // the interval (10 s) never fires here: every report below is a change
	c.Boot(context.Background())
	h.run(c)
	h.nextStatus()
	h.nextStream()

	// A rejected config after a control-plane restart: version 1, below the applied 3.
	h.cp.Restart()
	h.cp.Publish(configRejected(t))
	s := h.statusUntil("the rejection", func(s Status) bool { return s.LastRejection != nil })
	if s.LastRejection.Version != 1 || *s.AppliedConfigVersion != 3 || len(s.LastRejection.Codes) == 0 {
		t.Errorf("status %+v %+v, want rejection 1 with version 3 applied", s, s.LastRejection)
	}
	h.cp.Publish(configB(t))
	h.statusUntil("version 2 applied and no rejection", func(s Status) bool {
		return s.LastRejection == nil && *s.AppliedConfigVersion == 2
	})

	c.SetDraining()
	h.statusUntil("state draining", func(s Status) bool { return s.State == StateDraining })
	if err := c.ReportStatus(context.Background(), "test"); err != nil {
		t.Fatal(err)
	}
	if s := h.nextStatus(); s.State != StateDraining {
		t.Errorf("final report state %s", s.State)
	}
}

func TestServingChangeIsReported(t *testing.T) {
	h := newHarness(t)
	h.cp.Publish(configA(t))
	var queued atomic.Int64
	c := h.client(func(o *Options) {
		o.Serving = func() Serving {
			return Serving{Backends: map[string]BackendStatus{}, Models: map[string]ModelStatus{"llama": {Queued: queued.Load()}}}
		}
	}) // the interval (10 s) never fires here
	c.Boot(context.Background())
	h.run(c)
	h.nextStatus()
	queued.Store(2)
	c.ServingChanged()
	h.statusUntil("the queued requests", func(s Status) bool { return s.Models["llama"].Queued == 2 })
}

// TestServingChangesAreSpacedByTheMinimumGap drives the reporter's loop alone (no
// config stream, so no connect or apply report interleaves) on a test clock: a
// burst of routing changes inside the gap becomes one report at the gap's end with
// the latest state; a change after the gap is reported at once.
func TestServingChangesAreSpacedByTheMinimumGap(t *testing.T) {
	h := newHarness(t)
	var queued atomic.Int64
	var mu sync.Mutex
	now := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	gapTimers := make(chan time.Duration, 10)
	gapEnd := make(chan time.Time)
	c := h.client(func(o *Options) {
		o.StatusMinGap = time.Second
		o.Serving = func() Serving {
			return Serving{Backends: map[string]BackendStatus{}, Models: map[string]ModelStatus{"llama": {Queued: queued.Load()}}}
		}
		o.statusNow = func() time.Time {
			mu.Lock()
			defer mu.Unlock()
			return now
		}
		o.statusAfter = func(d time.Duration) <-chan time.Time {
			gapTimers <- d
			return gapEnd
		}
	}) // the interval (10 s) never fires here
	advance := func(d time.Duration) {
		mu.Lock()
		now = now.Add(d)
		mu.Unlock()
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		c.status.run(ctx)
		close(done)
	}()
	defer func() {
		cancel()
		<-done
	}()
	if s := h.nextStatus(); s.Models["llama"].Queued != 0 {
		t.Fatalf("start report %+v", s.Models)
	}

	// A burst 200 ms after the start report: the first change arms the gap's
	// remaining 800 ms; the others fold into it.
	advance(200 * time.Millisecond)
	for q := range int64(5) {
		queued.Store(q + 1)
		c.ServingChanged()
	}
	select {
	case d := <-gapTimers:
		if d != 800*time.Millisecond {
			t.Errorf("gap timer %v, want the 800 ms left of the gap", d)
		}
	case <-time.After(testWaitLimit):
		t.Fatal("no gap timer armed")
	}
	select {
	case body := <-h.cp.StatusEvents():
		t.Fatalf("report %s sent inside the gap", body)
	default:
	}
	advance(800 * time.Millisecond)
	gapEnd <- now
	if s := h.nextStatus(); s.Models["llama"].Queued != 5 {
		t.Errorf("report at the gap's end carries queued %d, want the latest 5", s.Models["llama"].Queued)
	}

	// Past the gap a change goes at once, with no timer: gapEnd is never fed again.
	advance(5 * time.Second)
	queued.Store(0)
	c.ServingChanged()
	if s := h.nextStatus(); s.Models["llama"].Queued != 0 {
		t.Errorf("report after the gap carries queued %d, want 0", s.Models["llama"].Queued)
	}
	if n := len(h.cp.Statuses()); n != 3 {
		t.Errorf("%d reports, want 3: start, the burst's one, the later change", n)
	}
}

// An immediate trigger is not held by the gap: a draining gateway says so at once,
// and that report carries a pending routing change too.
func TestDrainIsReportedInsideTheGap(t *testing.T) {
	h := newHarness(t)
	var queued atomic.Int64
	c := h.client(func(o *Options) {
		o.StatusMinGap = time.Hour
		o.Serving = func() Serving {
			return Serving{Backends: map[string]BackendStatus{}, Models: map[string]ModelStatus{"llama": {Queued: queued.Load()}}}
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		c.status.run(ctx)
		close(done)
	}()
	defer func() {
		cancel()
		<-done
	}()
	h.nextStatus()
	queued.Store(3)
	c.ServingChanged()
	c.SetDraining()
	h.statusUntil("draining with the queued requests", func(s Status) bool {
		return s.State == StateDraining && s.Models["llama"].Queued == 3
	})
}
