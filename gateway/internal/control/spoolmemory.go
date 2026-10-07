package control

import (
	"errors"
	"sync"

	"kaiak/internal/accounting"
)

// memoryStore is the batch store with no data directory: queued batches live in
// memory until acknowledged, bounded by Options.UsageMemoryBytes (usageSender.boundQueued),
// and a fresh epoch starts with every process. Nothing is written anywhere; a refused
// batch or record is logged and dropped.
type memoryStore struct {
	mu      sync.Mutex
	batches map[BatchID][]accounting.UsageRecord
}

func newMemoryStore() *memoryStore {
	return &memoryStore{batches: map[BatchID][]accounting.UsageRecord{}}
}

func (*memoryStore) open(instance string) (spoolIndex, []spoolEntry, string) {
	return freshIndex(instance), nil, "no data directory"
}

func (m *memoryStore) save(b UsageBatch, _ spoolIndex) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.batches[b.Batch] = b.Records
	return "", nil
}

func (m *memoryStore) load(e spoolEntry) (UsageBatch, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	records, ok := m.batches[e.id]
	if !ok {
		return UsageBatch{}, errors.New("batch no longer held")
	}
	return UsageBatch{Batch: e.id, Records: records}, nil
}

// acknowledge forgets the batch's records: with no data directory nothing survives the
// process, so the acknowledged batch is remembered by the sender alone.
func (m *memoryStore) acknowledge(e spoolEntry, _ spoolIndex) error { return m.remove(e) }

func (*memoryStore) writeIndex(spoolIndex) error { return nil }

func (m *memoryStore) remove(e spoolEntry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.batches, e.id)
	return nil
}

func (m *memoryStore) setAside(e spoolEntry) string {
	_ = m.remove(e) // cannot fail
	return ""
}

func (*memoryStore) setAsideRecord(accounting.UsageRecord, string) string { return "" }

func (*memoryStore) inMemory() bool { return true }
