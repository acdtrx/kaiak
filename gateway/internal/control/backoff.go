package control

import "time"

// backoff is the reconnect delay: exponential from base, capped at cap, with full
// jitter (a uniform random delay up to the exponential step), so gateways that lost
// the control plane together do not return together. It schedules the next attempt;
// it never waits for a readiness signal. Not safe for concurrent use: the client's
// one goroutine owns it.
type backoff struct {
	base, cap time.Duration
	// random returns a value in [0, 1).
	random  func() float64
	attempt int
}

// next returns the delay before the next attempt and counts the attempt.
func (b *backoff) next() time.Duration {
	step := b.cap
	if b.attempt < 32 && b.base<<b.attempt < b.cap && b.base<<b.attempt > 0 {
		step = b.base << b.attempt
	}
	b.attempt++
	return time.Duration(b.random() * float64(step))
}

// reset starts the delays again from base, after a connection that stayed healthy.
func (b *backoff) reset() { b.attempt = 0 }
