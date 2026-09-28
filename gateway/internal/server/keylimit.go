package server

import "sync"

// keyInFlight counts each key's requests in flight on this gateway, for
// global.max_concurrent_requests_per_key (docs/specs/GATEWAY.md, Limits → per-key
// concurrency). A key with nothing in flight has no entry, so the map holds at most
// the keys with requests in flight.
type keyInFlight struct {
	mu sync.Mutex
	n  map[string]int64
}

func newKeyInFlight() *keyInFlight { return &keyInFlight{n: make(map[string]int64)} }

// enter counts one more request for keyID; false, counting nothing, when keyID
// already has limit in flight.
func (k *keyInFlight) enter(keyID string, limit int64) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.n[keyID] >= limit {
		return false
	}
	k.n[keyID]++
	return true
}

// exit counts one of keyID's requests out.
func (k *keyInFlight) exit(keyID string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.n[keyID] <= 1 {
		delete(k.n, keyID)
		return
	}
	k.n[keyID]--
}

// count is keyID's requests in flight.
func (k *keyInFlight) count(keyID string) int64 {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.n[keyID]
}

// limitKeyConcurrency counts the request against its key's concurrent-request limit
// (the snapshot's max_concurrent_requests_per_key) from authentication to the
// request's end: every request of the key counts, the model endpoints too, and the
// count is taken before any body is read, so idle connections declaring bodies are
// bounded per key. The slot is given back by a finisher, which runs however the
// request ends — the handler's panic that cuts a broken-off response included.
func limitKeyConcurrency(rq *request, keys *keyInFlight) *apiError {
	limit := rq.snapshot.MaxConcurrentRequestsPerKey
	if !keys.enter(rq.keyID, limit) {
		rq.w.Header().Set("Retry-After", "1")
		return errConcurrencyLimited(limit)
	}
	keyID := rq.keyID
	rq.finishers = append(rq.finishers, func() { keys.exit(keyID) })
	return nil
}
