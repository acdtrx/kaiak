package otlp

import (
	"context"
	"errors"
	"time"
)

// batchInterval is how often a queued exporter sends what is queued when no full
// batch is: every second, fixed (docs/specs/GATEWAY.md, Observability → OTLP log
// export and OTLP trace export: delivery).
const batchInterval = time.Second

// ErrShutdown is ForceFlush's answer once the sender has stopped.
var ErrShutdown = errors.New("otlp: exporter shut down")

// Sender is the background sender of an exporter that queues what it exports: one
// goroutine calls the exporter's send — for full batches when Wake says one is
// queued, for everything queued at each tick and at each ForceFlush — so one export
// is in flight at a time, until Stop. The queue, what makes a batch full and what
// a send does are the exporter's.
type Sender struct {
	wake    chan struct{}
	flushes chan chan error
	stop    context.CancelFunc
	done    chan struct{}
}

// StartSender starts the sender. send exports from the exporter's queue until ctx
// ends or no batch is left: only full batches, or with all everything queued; ctx
// ends with Stop, cutting short the export in flight. tick fires a send of
// everything queued; nil is a ticker every second.
func StartSender(tick <-chan time.Time, send func(ctx context.Context, all bool)) *Sender {
	ctx, stop := context.WithCancel(context.Background())
	s := &Sender{
		wake:    make(chan struct{}, 1),
		flushes: make(chan chan error),
		stop:    stop,
		done:    make(chan struct{}),
	}
	var ticker *time.Ticker
	if tick == nil {
		ticker = time.NewTicker(batchInterval)
		tick = ticker.C
	}
	go func() {
		defer close(s.done)
		if ticker != nil {
			defer ticker.Stop()
		}
		s.run(ctx, tick, send)
	}()
	return s
}

// Wake tells the sender a full batch is queued. It never waits.
func (s *Sender) Wake() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *Sender) run(ctx context.Context, tick <-chan time.Time, send func(context.Context, bool)) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.wake:
			send(ctx, false)
		case <-tick:
			send(ctx, true)
		case reply := <-s.flushes:
			send(ctx, true)
			if ctx.Err() != nil {
				reply <- ErrShutdown
			} else {
				reply <- nil
			}
		}
	}
}

// ForceFlush sends what is queued until the queue is empty — items queued meanwhile
// included — and returns nil then, or ctx's error when ctx ends first, or
// ErrShutdown once Stop has run. The sender carries on either way. A ctx already
// ended starts no send: its error comes back at once.
func (s *Sender) ForceFlush(ctx context.Context) error {
	// With ctx ended and the sender idle, both of the select's first cases are ready
	// and it picks either: a flush handed over then would start an export past ctx's
	// end.
	if err := ctx.Err(); err != nil {
		return err
	}
	reply := make(chan error, 1)
	select {
	case s.flushes <- reply:
	case <-ctx.Done():
		return ctx.Err()
	case <-s.done:
		return ErrShutdown
	}
	select {
	case err := <-reply:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Stop ends the sender, cutting short the export in flight, and waits for it to
// end. It is safe to call more than once.
func (s *Sender) Stop() {
	s.stop()
	<-s.done
}
