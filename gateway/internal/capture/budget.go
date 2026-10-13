package capture

import "sync"

// budget bounds the bytes every capture holds at once (docs/specs/GATEWAY.md,
// Request capture → Memory): the copy of a request, the answer as it is written to
// the client, the encoded span until its export ends. A capture takes its bytes
// before it holds them and gives them back when it lets them go. A take that does
// not fit takes nothing: nothing waits for room, so nothing can stall on it.
type budget struct {
	limit int64

	mu   sync.Mutex
	used int64
}

// take takes n bytes; false, taking nothing, when fewer are left.
func (b *budget) take(n int64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if n > b.limit-b.used {
		return false
	}
	b.used += n
	return true
}

// give gives back n bytes taken earlier.
func (b *budget) give(n int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.used -= n
}

// swap replaces a charge of from bytes with one of to bytes, in one step: a smaller
// charge always fits. When a larger one does not, swap gives the from bytes back,
// takes nothing and returns false.
func (b *budget) swap(from, to int64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if to-from > b.limit-b.used {
		b.used -= from
		return false
	}
	b.used += to - from
	return true
}

// inUse is the bytes taken now.
func (b *budget) inUse() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.used
}
