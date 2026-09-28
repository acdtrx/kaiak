package control

import (
	"cmp"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strings"

	"kaiak/internal/accounting"
)

// The usage batches between their seal and their ack (docs/specs/GATEWAY.md,
// Control-plane mode → Usage batches): each sealed batch takes the next sequence of
// the epoch and joins the queue the sender works through. Where the queued batches
// live is the store's (batchStore): the data directory's spool files when one is set
// (diskStore, spooldisk.go), memory otherwise (memoryStore, spoolmemory.go). Sealing,
// checking, queueing, bounding and sending are the same code for both.
//
// Invariants, for exactly-once counting:
//   - a batch ID is bound to one set of records: a batch is sent only once the store
//     holds it and an index whose next sequence is past it, so no sequence is ever
//     sealed with other records (across restarts too, with the disk store);
//   - batches of one epoch are sent in sequence order, and every batch of an epoch is
//     sent before any batch of the next (the control plane counts a different epoch as
//     a fresh spool and never goes back to an older one).
const (
	// spoolWarnBatches: each time the queue grows past another multiple of it, a
	// warning is logged. The disk queue itself is not bounded: usage is billing data.
	spoolWarnBatches = 1000
)

// DefaultUsageMemoryBytes is the bound on the encoded usage records held in memory
// waiting for delivery (Options.UsageMemoryBytes, KAIAK_USAGE_MEMORY_BYTES;
// docs/specs/GATEWAY.md, Usage batches): the sealed records the spool cannot write,
// or, with no data directory, every queued record. 64 MiB, about 130 000 typical
// records of about 500 bytes.
const DefaultUsageMemoryBytes = 64 << 20

// batchStore keeps the queued batches until they are acknowledged or refused.
type batchStore interface {
	// open returns the epoch and next sequence to seal under and the batches still
	// queued from an earlier run, in any order; reason is why a new epoch starts, ""
	// when an earlier one continues.
	open(instance string) (idx spoolIndex, queued []spoolEntry, reason string)
	// save keeps batch b, then records next as the index; the batch may be sent once
	// save returns nil. It returns where b is kept ("" in memory).
	save(b UsageBatch, next spoolIndex) (file string, err error)
	// load returns the queued batch e.
	load(e spoolEntry) (UsageBatch, error)
	// remove forgets e, acknowledged or dropped.
	remove(e spoolEntry) error
	// setAside takes the refused (or unreadable) batch e out of the queue's keeping,
	// for inspection where the store can keep it; it returns where ("": nowhere).
	setAside(e spoolEntry) string
	// setAsideRecord keeps a record the protocol's checks refuse, for inspection where
	// the store can; it returns where ("": nowhere).
	setAsideRecord(rec accounting.UsageRecord, issues string) string
	// inMemory reports whether the queued batches are held in memory: they then count
	// toward Options.UsageMemoryBytes, and nothing survives the process.
	inMemory() bool
}

// spoolIndex is the epoch the gateway seals batches in and the sequence of the next
// one; the disk store keeps it as the spool's index file.
type spoolIndex struct {
	Instance     string `json:"instance"`
	Epoch        string `json:"epoch"`
	NextSequence int64  `json:"next_sequence"`
}

// freshIndex starts a new epoch for instance.
func freshIndex(instance string) spoolIndex {
	return spoolIndex{Instance: instance, Epoch: newEpoch(), NextSequence: 1}
}

// spoolEntry is one queued batch: its ID, where the store keeps it ("" in memory),
// how many records it holds, their encoded size (0 for a batch restored from the
// spool: it is on disk, not in memory) and its usage generation.
type spoolEntry struct {
	id         BatchID
	file       string
	records    int
	bytes      int64
	generation uint64
}

func newEpoch() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never returns an error
	return hex.EncodeToString(b[:])
}

// restore takes what the store kept from an earlier run: the epoch and next sequence
// to continue, and the batches still queued. Queued batches are delivered whatever
// the index said: each carries its own ID, and another instance's batches are sent
// under that instance (a container whose hostname changed). The queue sends batches
// of other epochs first, one epoch at a time, then the current epoch's.
func (u *usageSender) restore() {
	idx, entries, reason := u.store.open(u.instance)
	u.index = idx
	current := func(e spoolEntry) bool { return e.id.Epoch == u.index.Epoch && e.id.Instance == u.instance }
	slices.SortStableFunc(entries, func(a, b spoolEntry) int {
		if ca, cb := current(a), current(b); ca != cb {
			if cb {
				return -1
			}
			return 1
		}
		return cmp.Or(strings.Compare(a.id.Instance, b.id.Instance), strings.Compare(a.id.Epoch, b.id.Epoch),
			cmp.Compare(a.id.Sequence, b.id.Sequence))
	})

	// Restored batches take the first generations, in send order, so a totals message
	// showing one counted names it to the limiter like any other; the filling batch
	// continues after them.
	records := 0
	for i := range entries {
		records += entries[i].records
		entries[i].generation = uint64(i + 1)
	}
	u.mu.Lock()
	u.generation = uint64(len(entries)) + 1
	u.restoredGeneration = uint64(len(entries))
	u.queue = entries
	u.queuedRecords = records
	u.depthChangedLocked()
	u.mu.Unlock()
	attrs := []any{"epoch", u.index.Epoch, "next_sequence", u.index.NextSequence, "batches", len(entries),
		"records", records}
	switch {
	case u.store.inMemory():
		u.logger.Info("usage batches kept in memory until acknowledged: no data directory", "epoch", u.index.Epoch)
	case reason != "":
		u.logger.Info("usage spool: new epoch", append(attrs, "reason", reason)...)
	default:
		u.logger.Info("usage spool restored", attrs...)
	}
}

// persist hands the sealed batches to the store, oldest first, each under the next
// sequence of the epoch, and queues them for sending. Each batch's records are checked
// first (checkSealed): a record the protocol refuses is set aside alone. A batch joins
// the queue only once the store holds it and the index past it; a save that fails
// leaves it sealed in memory with its records unchanged, so the retry (the next seal
// tick) saves the same batch under the same ID — within Options.UsageMemoryBytes
// (boundSealed). persist is the only code that removes sealed batches, so while it
// holds persistMu the sealed batches keep their positions (Record only appends).
func (u *usageSender) persist() {
	u.persistMu.Lock()
	defer u.persistMu.Unlock()
	for {
		u.mu.Lock()
		if len(u.sealed) == 0 {
			u.mu.Unlock()
			return
		}
		sealed := u.sealed[0]
		u.mu.Unlock()
		if !sealed.checked {
			if !u.checkSealed(0, sealed) {
				continue
			}
			u.mu.Lock()
			sealed = u.sealed[0]
			u.mu.Unlock()
		}
		records := sealed.records

		id := BatchID{Instance: u.instance, Epoch: u.index.Epoch, Sequence: u.index.NextSequence}
		next := u.index
		next.NextSequence++
		file, err := u.store.save(UsageBatch{Batch: id, Records: records}, next)
		if err != nil {
			u.logger.Error("usage batch not written to the spool; kept in memory and retried", "sequence", id.Sequence,
				"records", len(records), "error", err)
			u.checkAllSealed()
			u.boundSealed()
			return
		}
		u.index = next

		u.mu.Lock()
		u.sealed = u.sealed[1:]
		u.sealedRecords -= len(records)
		u.sealedBytes -= sealed.bytes
		u.queue = append(u.queue, spoolEntry{id: id, file: file, records: len(records), bytes: sealed.bytes,
			generation: sealed.generation})
		u.queuedRecords += len(records)
		u.queuedBytes += sealed.bytes
		depth, queuedRecords := len(u.queue), u.queuedRecords
		u.depthChangedLocked()
		u.mu.Unlock()
		signal(u.queued)
		u.logger.Debug("usage batch sealed", "epoch", id.Epoch, "sequence", id.Sequence, "records", len(records))
		if u.store.inMemory() {
			u.boundQueued()
		} else if depth%spoolWarnBatches == 0 {
			u.logger.Warn("usage spool keeps growing: the control plane is not acknowledging batches",
				"batches", depth, "records", queuedRecords)
		}
	}
}

// checkSealed checks each record of the sealed batch b, at position i, against the
// usage record schema and rules, as the control plane will: a record it would refuse
// would make the whole batch refused and set aside, so it is set aside alone now —
// kept for inspection where the store can, logged, counted as dropped — and the rest
// of the batch goes on. The encoding the check makes gives the batch's size in
// memory (memoryBytesLocked). It reports whether the batch still holds records; an
// emptied batch is dropped. Callers hold persistMu.
func (u *usageSender) checkSealed(i int, b sealedBatch) bool {
	good := make([]accounting.UsageRecord, 0, len(b.records))
	var bad int
	var size int64
	for _, rec := range b.records {
		n, issues := recordIssues(rec)
		if issues == "" {
			good = append(good, rec)
			size += int64(n)
			continue
		}
		bad++
		kept := u.store.setAsideRecord(rec, issues)
		u.logger.Error("usage record refused by the protocol's checks; set aside, the rest of its batch is sent",
			append([]any{"request_id", rec.RequestID, "record_id", rec.RecordID, "issues", issues}, fileAttr(kept)...)...)
	}
	u.mu.Lock()
	u.sealed[i].checked = true
	u.sealed[i].records = good
	u.sealed[i].bytes = size
	u.sealedRecords -= bad
	u.sealedBytes += size
	if len(good) == 0 {
		u.sealed = slices.Delete(u.sealed, i, i+1)
	}
	u.depthChangedLocked()
	u.mu.Unlock()
	if bad > 0 {
		u.observeDropped(DroppedInvalid, bad)
	}
	return len(good) > 0
}

// fileAttr is the log attribute naming where the store kept something; none when it
// kept nothing.
func fileAttr(file string) []any {
	if file == "" {
		return nil
	}
	return []any{"file", file}
}

// refusedRecord is a refused-record file's data: the record and why it was refused.
type refusedRecord struct {
	Record accounting.UsageRecord `json:"record"`
	Issues string                 `json:"issues"`
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

// checkAllSealed checks every sealed batch not checked yet, so each counts its size
// before boundSealed weighs them. Callers hold persistMu.
func (u *usageSender) checkAllSealed() {
	for i := 0; ; {
		u.mu.Lock()
		if i >= len(u.sealed) {
			u.mu.Unlock()
			return
		}
		b := u.sealed[i]
		u.mu.Unlock()
		if b.checked || u.checkSealed(i, b) {
			i++
		}
	}
}

// boundSealed drops the oldest sealed batches while the sealed records not yet saved
// exceed their bound (Options.UsageMemoryBytes): the spool has been failing to write
// (a full or read-only disk), and memory growing without bound would end the process
// and lose every record anyway. The newest sealed batch is always kept. Each drop is
// logged and counted.
func (u *usageSender) boundSealed() {
	u.mu.Lock()
	var dropped, batches int
	bound := u.c.opts.UsageMemoryBytes
	for len(u.sealed) > 1 && u.sealedBytes > bound {
		b := u.sealed[0]
		u.sealed = u.sealed[1:]
		u.sealedRecords -= len(b.records)
		u.sealedBytes -= b.bytes
		dropped += len(b.records)
		batches++
	}
	if dropped > 0 {
		u.depthChangedLocked()
	}
	kept, keptBytes := u.sealedRecords, u.sealedBytes
	u.mu.Unlock()
	if dropped > 0 {
		u.logger.Error("usage spool not writable: oldest sealed usage batches dropped to bound memory",
			"batches", batches, "records", dropped, "kept_records", kept, "kept_bytes", keptBytes, "bound_bytes", bound)
		u.observeDropped(DroppedSpoolFull, dropped)
	}
}

// boundQueued drops the oldest queued batches while the records held in memory exceed
// their bound (Options.UsageMemoryBytes), when the queue itself is in memory (no data
// directory): the control plane has not been acknowledging, and memory growing
// without bound would end the process and lose every record anyway. The outstanding
// batch, queue[0], is kept: it may be on the wire, and its ack must still find it.
// Each drop is logged and counted.
func (u *usageSender) boundQueued() {
	u.mu.Lock()
	var gone []spoolEntry
	var dropped int
	bound := u.c.opts.UsageMemoryBytes
	for len(u.queue) > 1 && u.memoryBytesLocked() > bound {
		e := u.queue[1]
		u.queue = slices.Delete(u.queue, 1, 2)
		u.queuedRecords -= e.records
		u.queuedBytes -= e.bytes
		dropped += e.records
		gone = append(gone, e)
	}
	if dropped > 0 {
		u.depthChangedLocked()
	}
	kept, keptBytes := u.queuedRecords+u.sealedRecords, u.memoryBytesLocked()
	u.mu.Unlock()
	if dropped == 0 {
		return
	}
	for _, e := range gone {
		_ = u.store.remove(e) // the memory store's remove cannot fail
	}
	u.logger.Error("usage batches not acknowledged: oldest queued usage batches dropped to bound memory (no data directory)",
		"batches", len(gone), "records", dropped, "kept_records", kept, "kept_bytes", keptBytes, "bound_bytes", bound)
	u.observeDropped(DroppedMemoryBound, dropped)
}

func (u *usageSender) observeDropped(reason string, records int) {
	if u.observer != nil {
		u.observer.UsageRecordsDropped(reason, records)
	}
}

// setAside takes a batch the control plane refused, or one that can no longer be
// read, out of the queue, kept for inspection where the store can; it returns where.
func (u *usageSender) setAside(e spoolEntry) string {
	kept := u.store.setAside(e)
	u.dropHead(e)
	return kept
}
