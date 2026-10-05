package control

import (
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"

	"kaiak/internal/accounting"
	"kaiak/internal/state"
)

// The usage spool (docs/specs/GATEWAY.md, Control-plane mode → Usage spool), the
// batch store with a data directory: one file per sealed batch not yet acknowledged,
// and an index naming the epoch and the next sequence. The files are the queue;
// memory holds only their IDs and record counts, and the batch being sent. Every file
// carries spoolFormat; a file with another version is discarded when read
// (state.Dir.ReadVersioned).
const (
	// SpoolFile is the spool's index: instance, epoch, next sequence.
	SpoolFile = "usage-spool.json"
	// spoolBatchPrefix and spoolRejectedPrefix start the names of queued batches and
	// of batches the control plane refused: <prefix><epoch>-<sequence>.json.
	spoolBatchPrefix    = "usage-batch-"
	spoolRejectedPrefix = "usage-rejected-"
	// spoolFormat is the format version of every spool file.
	spoolFormat = 3
	// rejectedKept is how many refused batches and records are kept for inspection;
	// older ones are deleted as new ones are set aside.
	rejectedKept = 10
)

// diskStore is the batch store in the data directory.
type diskStore struct {
	dir    *state.Dir
	logger *slog.Logger
}

func batchFileName(prefix string, id BatchID) string {
	return fmt.Sprintf("%s%s-%d.json", prefix, id.Epoch, id.Sequence)
}

// parseBatchFileName reads the epoch and sequence out of a batch file name.
func parseBatchFileName(prefix, name string) (epoch string, sequence int64, ok bool) {
	rest, found := strings.CutPrefix(name, prefix)
	if !found {
		return "", 0, false
	}
	rest, found = strings.CutSuffix(rest, ".json")
	if !found {
		return "", 0, false
	}
	epoch, seq, found := strings.Cut(rest, "-")
	if !found || !hex32Pattern.MatchString(epoch) {
		return "", 0, false
	}
	sequence, err := strconv.ParseInt(seq, 10, 64)
	if err != nil || sequence < 1 || strconv.FormatInt(sequence, 10) != seq {
		return "", 0, false
	}
	return epoch, sequence, true
}

// open reads the spool left in the data directory: its queued batches, and the epoch
// and next sequence when the index belongs to this instance. Otherwise — no index,
// one of another format version (discarded), an unreadable one, or another
// instance's — a fresh epoch starts. The index is written back before open returns.
func (s diskStore) open(instance string) (spoolIndex, []spoolEntry, string) {
	var idx spoolIndex
	found, err := s.dir.ReadVersioned(SpoolFile, spoolFormat, &idx)
	if err != nil {
		s.logger.Error("usage spool index unreadable: starting a new epoch", "file.name", SpoolFile, "exception.message", err)
		found = false
	}
	entries := s.readQueuedBatches()

	reason := ""
	switch {
	case !found:
		reason = "no usage spool"
	case idx.Instance != instance:
		reason = "usage spool of instance " + strconv.Quote(idx.Instance)
	case !hex32Pattern.MatchString(idx.Epoch) || idx.NextSequence < 1:
		reason = "usage spool index invalid"
	}
	if reason == "" {
		for _, e := range entries {
			if e.id.Epoch == idx.Epoch && e.id.Instance == instance && e.id.Sequence >= idx.NextSequence {
				// Written just before a crash, ahead of the index: the sequence
				// continues after it.
				idx.NextSequence = e.id.Sequence + 1
			}
		}
	} else {
		idx = freshIndex(instance)
	}
	if err := s.dir.WriteVersioned(SpoolFile, spoolFormat, idx); err != nil {
		s.logger.Error("usage spool index not written; retried with the next batch", "file.name", SpoolFile, "exception.message", err)
	}
	return idx, entries, reason
}

// readQueuedBatches reads every queued batch file for its ID and record count. A file
// that cannot be read is set aside with the refused batches, for inspection.
func (s diskStore) readQueuedBatches() []spoolEntry {
	files, err := s.dir.List(spoolBatchPrefix)
	if err != nil {
		s.logger.Error("usage spool not listed: queued batches wait for the next start", "exception.message", err)
		return nil
	}
	var entries []spoolEntry
	for _, f := range files {
		if _, _, ok := parseBatchFileName(spoolBatchPrefix, f.Name); !ok {
			s.logger.Warn("usage spool: file name not understood; left alone", "file.name", f.Name)
			continue
		}
		var batch UsageBatch
		found, err := s.dir.ReadVersioned(f.Name, spoolFormat, &batch)
		if err != nil || (found && len(batch.Records) == 0) {
			s.logger.Error("usage spool: batch file unreadable; set aside", "file.name", f.Name, "exception.message", err)
			s.moveAside(f.Name, strings.Replace(f.Name, spoolBatchPrefix, spoolRejectedPrefix, 1))
			continue
		}
		if !found {
			continue // another format version: discarded and logged by the read
		}
		entries = append(entries, spoolEntry{id: batch.Batch, file: f.Name, records: len(batch.Records)})
	}
	return entries
}

// save writes the batch file, then the index past it: both durable before the batch
// may be sent. A failed index write leaves the batch file behind; the retry writes
// the same batch under the same name.
func (s diskStore) save(b UsageBatch, next spoolIndex) (string, error) {
	name := batchFileName(spoolBatchPrefix, b.Batch)
	if err := s.dir.WriteVersioned(name, spoolFormat, b); err != nil {
		return "", fmt.Errorf("batch file %s: %w", name, err)
	}
	if err := s.dir.WriteVersioned(SpoolFile, spoolFormat, next); err != nil {
		return "", fmt.Errorf("index %s: %w", SpoolFile, err)
	}
	return name, nil
}

func (s diskStore) load(e spoolEntry) (UsageBatch, error) {
	var batch UsageBatch
	found, err := s.dir.ReadVersioned(e.file, spoolFormat, &batch)
	if err != nil {
		return UsageBatch{}, err
	}
	if !found {
		return UsageBatch{}, fmt.Errorf("batch file %s is gone or of another format version", e.file)
	}
	return batch, nil
}

func (s diskStore) remove(e spoolEntry) error { return s.dir.Remove(e.file) }

// setAside moves the batch's file to the refused files, keeping the newest
// rejectedKept of them.
func (s diskStore) setAside(e spoolEntry) string {
	name := batchFileName(spoolRejectedPrefix, e.id)
	s.moveAside(e.file, name)
	s.pruneRejected()
	return name
}

// setAsideRecord writes the refused record to a refused-record file, kept with the
// refused batches.
func (s diskStore) setAsideRecord(rec accounting.UsageRecord, issues string) string {
	name := spoolRejectedPrefix + "record-" + rec.RecordID + ".json"
	if err := s.dir.WriteVersioned(name, spoolFormat, refusedRecord{Record: rec, Issues: issues}); err != nil {
		s.logger.Error("refused usage record not written", "file.name", name, "exception.message", err)
		return ""
	}
	s.pruneRejected()
	return name
}

func (diskStore) inMemory() bool { return false }

// pruneRejected deletes refused batch and record files beyond the newest
// rejectedKept.
func (s diskStore) pruneRejected() {
	files, err := s.dir.List(spoolRejectedPrefix)
	if err != nil {
		s.logger.Warn("refused usage batches not listed; older ones are not pruned", "exception.message", err)
		return
	}
	if len(files) <= rejectedKept {
		return
	}
	slices.SortStableFunc(files, func(a, b state.File) int { return a.ModTime.Compare(b.ModTime) })
	for _, f := range files[:len(files)-rejectedKept] {
		if err := s.dir.Remove(f.Name); err != nil {
			s.logger.Warn("refused usage batch not pruned", "file.name", f.Name, "exception.message", err)
		}
	}
}

func (s diskStore) moveAside(from, to string) {
	if err := s.dir.Rename(from, to); err != nil {
		s.logger.Error("usage batch file not set aside; removed instead", "file.name", from, "exception.message", err)
		if err := s.dir.Remove(from); err != nil {
			s.logger.Error("usage batch file not removed", "file.name", from, "exception.message", err)
		}
	}
}
