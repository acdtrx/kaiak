package config

import "sync/atomic"

// Holder holds the live snapshot and swaps it atomically. Readers call Current once
// per request and keep the snapshot they got; a swap never changes a snapshot already
// handed out. The zero Holder holds no snapshot.
type Holder struct {
	current atomic.Pointer[Snapshot]
}

// Current returns the live snapshot, or nil before the first config is loaded.
func (h *Holder) Current() *Snapshot {
	return h.current.Load()
}

// Swap makes next the live snapshot.
func (h *Holder) Swap(next *Snapshot) {
	h.current.Store(next)
}

// Loaded reports whether a config has been loaded: the gateway's readiness condition
// for config.
func (h *Holder) Loaded() bool {
	return h.current.Load() != nil
}
