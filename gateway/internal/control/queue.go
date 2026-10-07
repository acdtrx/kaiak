package control

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"slices"

	"kaiak/internal/accounting"
)

// The usage batches between their seal and their ack (docs/specs/GATEWAY.md,
// Control-plane mode → Usage batches): each sealed batch has its records checked,
// takes the next sequence of the process's epoch and joins the queue the sender works
// through. Everything is in memory: nothing survives the process.
//
// Invariants, for exactly-once counting:
//   - a batch ID is bound to one set of records: the sequence is taken as the batch
//     joins the queue, and never again within the epoch;
//   - batches are sent in sequence order, one at a time, so usage generations follow
//     send order: a totals event retires own usage up to the newest generation it shows
//     counted (countedGeneration), and every batch sent before a covered one is
//     covered too.

// DefaultUsageMemoryBytes is the bound on the encoded usage records held in memory
// waiting for delivery (Options.UsageMemoryBytes, KAIAK_USAGE_MEMORY_BYTES;
// docs/specs/GATEWAY.md, Usage batches in memory). 64 MiB, about 130 000 typical
// records of about 500 bytes.
const DefaultUsageMemoryBytes = 64 << 20

// queuedBatch is one batch in the queue: its ID, its records, their encoded size and
// its usage generation.
type queuedBatch struct {
	id         BatchID
	records    []accounting.UsageRecord
	bytes      int64
	generation uint64
}

func newEpoch() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never returns an error
	return hex.EncodeToString(b[:])
}

// queueSealed moves the sealed batches to the queue, oldest first, each under the next
// sequence of the epoch. Each batch's records are checked first (checkRecords): a
// record the protocol refuses is dropped alone, and a batch left empty takes no
// sequence. queueSealed is the only code that removes sealed batches and takes
// sequences, and queueMu serializes it (the sealer and FlushUsage), so the checks run
// outside u.mu, off the request path's lock.
func (u *usageSender) queueSealed() {
	u.queueMu.Lock()
	defer u.queueMu.Unlock()
	for {
		u.mu.Lock()
		if len(u.sealed) == 0 {
			u.mu.Unlock()
			return
		}
		sealed := u.sealed[0]
		u.mu.Unlock()
		records, size := u.checkRecords(sealed.records)

		u.mu.Lock()
		u.sealed = u.sealed[1:]
		u.sealedRecords -= len(sealed.records)
		if len(records) == 0 {
			u.depthChangedLocked()
			u.mu.Unlock()
			continue
		}
		id := BatchID{Instance: u.instance, Epoch: u.epoch, Sequence: u.nextSequence}
		u.nextSequence++
		u.queue = append(u.queue, queuedBatch{id: id, records: records, bytes: size, generation: sealed.generation})
		u.queuedRecords += len(records)
		u.queuedBytes += size
		u.depthChangedLocked()
		u.mu.Unlock()
		signal(u.queued)
		u.logger.Debug("usage batch sealed", "kaiak.usage.epoch", id.Epoch, "kaiak.usage.sequence", id.Sequence, "kaiak.usage.records", len(records))
		u.boundQueued()
	}
}

// checkRecords checks each record against the usage record schema and rules, as the
// control plane will: a record it would refuse would make the whole batch refused, so
// it is dropped alone now — logged and counted — and the rest of the batch goes on. It
// returns the records that pass and their encoded size, what Options.UsageMemoryBytes
// bounds.
func (u *usageSender) checkRecords(records []accounting.UsageRecord) ([]accounting.UsageRecord, int64) {
	good := make([]accounting.UsageRecord, 0, len(records))
	var size int64
	for _, rec := range records {
		n, issues := recordIssues(rec)
		if issues == "" {
			good = append(good, rec)
			size += int64(n)
			continue
		}
		u.logger.Error("usage record refused by the protocol's checks; dropped, the rest of its batch is sent",
			"kaiak.request.id", rec.RequestID, "kaiak.usage.record_id", rec.RecordID, "kaiak.usage.issues", issues)
	}
	if bad := len(records) - len(good); bad > 0 {
		u.observeDropped(DroppedInvalid, bad)
	}
	return good, size
}

// recordIssues checks rec as it is sent against the usage record's schema and rules:
// issues is "" when it passes, and size is its encoded length then.
func recordIssues(rec accounting.UsageRecord) (size int, issues string) {
	data, err := json.Marshal(rec)
	if err != nil {
		return 0, err.Error()
	}
	if _, err := DecodeUsageRecord(data); err != nil {
		return 0, err.Error()
	}
	return len(data), ""
}

// boundQueued drops the oldest queued batches while the queued records exceed their
// bound (Options.UsageMemoryBytes): the control plane has not been acknowledging, and
// memory growing without bound would end the process and lose every record anyway.
// The outstanding batch, queue[0], is kept: it may be on the wire, and its ack must
// still find it. Each drop is logged and counted.
func (u *usageSender) boundQueued() {
	u.mu.Lock()
	var batches, dropped int
	bound := u.c.opts.UsageMemoryBytes
	for len(u.queue) > 1 && u.queuedBytes > bound {
		b := u.queue[1]
		u.queue = slices.Delete(u.queue, 1, 2)
		u.queuedRecords -= len(b.records)
		u.queuedBytes -= b.bytes
		dropped += len(b.records)
		batches++
	}
	if dropped > 0 {
		u.depthChangedLocked()
	}
	kept, keptBytes := u.queuedRecords+u.sealedRecords, u.queuedBytes
	u.mu.Unlock()
	if dropped == 0 {
		return
	}
	u.logger.Error("usage batches not acknowledged: oldest queued usage batches dropped to bound memory",
		"kaiak.usage.batches", batches, "kaiak.usage.records", dropped, "kaiak.usage.kept_records", kept, "kaiak.usage.kept_size", keptBytes, "kaiak.usage.max_size", bound)
	u.observeDropped(DroppedMemoryBound, dropped)
}

func (u *usageSender) observeDropped(reason string, records int) {
	if u.observer != nil {
		u.observer.UsageRecordsDropped(reason, records)
	}
}
