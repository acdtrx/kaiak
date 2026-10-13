package otlp

import (
	"context"
	"errors"
	"testing"
	"time"
)

// sends records each send's kind, and lets a test hold a send in flight.
type sends struct {
	calls chan bool
	// hold, when set, keeps each send until it is closed or the send's ctx ends.
	hold chan struct{}
}

func (s *sends) send(ctx context.Context, all bool) {
	s.calls <- all
	if s.hold != nil {
		select {
		case <-s.hold:
		case <-ctx.Done():
		}
	}
}

func (s *sends) next(t *testing.T) bool {
	t.Helper()
	select {
	case all := <-s.calls:
		return all
	case <-time.After(10 * time.Second):
		t.Fatal("no send")
		return false
	}
}

func (s *sends) none(t *testing.T) {
	t.Helper()
	select {
	case all := <-s.calls:
		t.Fatalf("unexpected send (all %t)", all)
	default:
	}
}

func TestSenderSendsFullBatchesOnWakeAndEverythingOnTickAndFlush(t *testing.T) {
	s := &sends{calls: make(chan bool, 10)}
	tick := make(chan time.Time)
	sender := StartSender(tick, s.send)
	defer sender.Stop()

	sender.Wake()
	if all := s.next(t); all {
		t.Fatal("a wake sent everything, want full batches only")
	}
	tick <- time.Now()
	if all := s.next(t); !all {
		t.Fatal("a tick sent full batches only, want everything")
	}
	if err := sender.ForceFlush(context.Background()); err != nil {
		t.Fatalf("ForceFlush = %v, want nil", err)
	}
	if all := s.next(t); !all {
		t.Fatal("a flush sent full batches only, want everything")
	}
	s.none(t)
}

// Wake never waits: the wake is kept, once, while a send is in flight.
func TestSenderWakeNeverWaits(t *testing.T) {
	s := &sends{calls: make(chan bool, 10), hold: make(chan struct{})}
	sender := StartSender(make(chan time.Time), s.send)
	defer sender.Stop()
	sender.Wake()
	s.next(t) // the first send is held
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 1000 {
			sender.Wake()
		}
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Wake waited on a send in flight")
	}
	close(s.hold)
	if all := s.next(t); all {
		t.Fatal("the kept wake sent everything")
	}
	if err := sender.ForceFlush(context.Background()); err != nil {
		t.Fatal(err)
	}
	s.next(t) // the flush's send: the thousand wakes made one send
	s.none(t)
}

func TestSenderForceFlushEndsWithItsContext(t *testing.T) {
	s := &sends{calls: make(chan bool, 10), hold: make(chan struct{})}
	sender := StartSender(make(chan time.Time), s.send)
	defer sender.Stop()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := sender.ForceFlush(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ForceFlush = %v, want the deadline", err)
	}
	s.next(t) // the flush's send carries on
	close(s.hold)
}

// Stop cuts the send in flight; after it, ForceFlush answers ErrShutdown at once.
func TestSenderStopCutsTheSendInFlight(t *testing.T) {
	s := &sends{calls: make(chan bool, 10), hold: make(chan struct{})}
	sender := StartSender(make(chan time.Time), s.send)
	sender.Wake()
	s.next(t)
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		sender.Stop()
	}()
	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("Stop waited on the send in flight")
	}
	sender.Stop()
	if err := sender.ForceFlush(context.Background()); !errors.Is(err, ErrShutdown) {
		t.Fatalf("ForceFlush after Stop = %v, want ErrShutdown", err)
	}
	sender.Wake()
	s.none(t)
}
