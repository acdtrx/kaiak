package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"kaiak/internal/accounting"
)

// Usage delivery (CONTROL-PROTOCOL.md, Usage batches): settled records fill a batch;
// the batch is sealed every BatchInterval or at BatchMaxRecords, handed to the batch
// store (the spool, or memory with no data directory) under the next sequence of the
// epoch, and queued. One goroutine seals and saves, another sends the head of the
// queue — at most one batch outstanding — and retries it with the same ID until the
// control plane acknowledges or refuses it. Record, the accounting sink, only appends
// in memory: nothing on the request path waits for the disk or the network.

// DefaultBatchInterval seals the filling batch when Options leave it zero; a batch
// also seals at MaxBatchRecords (the default BatchMaxRecords).
const DefaultBatchInterval = 5 * time.Second

// usageTimeout bounds one POST /v1/usage.
const usageTimeout = 30 * time.Second

// Batch send results, as UsageObserver hears them.
const (
	BatchAcked    = "acked"
	BatchRejected = "rejected"
	BatchFailed   = "failed"
)

// UsageObserver is told how usage delivery goes, for metrics. Its methods are called
// from the client's goroutines, and from Record's caller for the spool depth, and
// must not block.
type UsageObserver interface {
	// UsageBatchSent reports one POST /v1/usage and its result (BatchAcked,
	// BatchRejected, BatchFailed), at the time it ended.
	UsageBatchSent(result string, at time.Time)
	// UsageSpoolDepth reports the sealed batches not yet acknowledged (queued, or
	// waiting to be written), the records in them, and the encoded bytes of those
	// held in memory, which count against Options.UsageMemoryBytes.
	UsageSpoolDepth(batches, records int, memoryBytes int64)
	// UsageRecordsDropped reports records dropped before reaching the control
	// plane, with the reason: DroppedInvalid, DroppedSpoolFull or DroppedMemoryBound.
	UsageRecordsDropped(reason string, records int)
}

// Reasons usage records are dropped, as UsageObserver hears them.
const (
	// DroppedInvalid: the record fails the usage record's checks, so the control
	// plane would refuse its whole batch; it is set aside alone.
	DroppedInvalid = "invalid"
	// DroppedSpoolFull: the spool could not be written for so long that the sealed
	// records in memory passed their bound (Options.UsageMemoryBytes); the oldest
	// were dropped.
	DroppedSpoolFull = "spool_unwritable"
	// DroppedMemoryBound: with no data directory the queued batches live in memory;
	// the control plane left them unacknowledged until their records passed the
	// bound (Options.UsageMemoryBytes), and the oldest were dropped.
	DroppedMemoryBound = "memory_bound"
)

// batchRefusals are the error codes that mean the batch itself can never be accepted
// (CONTROL-PROTOCOL.md, Usage intake and Request checks): it is set aside rather than
// retried, or it would block every batch behind it. Any other refusal — the token,
// the protocol version, an unknown answer, a proxy's — is not the batch's fault and
// is retried, so no billing data is dropped over a configuration problem.
var batchRefusals = map[string]bool{
	"usage-batch-invalid":      true,
	CodeRecordInstanceMismatch: true,
	CodeRecordIDDuplicate:      true,
	CodeTimestampInvalid:       true,
	"instance-mismatch":        true,
	"request-invalid":          true,
}

// batchRefused reports whether e means the batch itself can never be accepted: one of
// batchRefusals' codes answered 400 or 413 (CONTROL-PROTOCOL.md, Sending). The same
// code on another status is retried — a host's own throttling or auth in front of the
// protocol's routes can carry it, labelled by the host's framework, and dropping the
// batch over it would lose billing data.
func batchRefused(e *statusError) bool {
	return (e.status == http.StatusBadRequest || e.status == http.StatusRequestEntityTooLarge) && batchRefusals[e.code]
}

// usageSender owns the filling batch, the queue of sealed batches and their sending.
type usageSender struct {
	c        *Client
	store    batchStore
	logger   *slog.Logger
	instance string
	max      int
	interval time.Duration
	observer UsageObserver
	// backoff is the sender goroutine's own: the config follower has another.
	backoff backoff

	mu sync.Mutex
	// generation is the filling batch's usage generation: the batches restored from
	// the spool at start take 1 to restoredGeneration, the process's first sealed
	// batch the next, one more per sealed batch. A totals message showing a batch
	// counted names its generation to the consumer (TotalsUpdate.Counted).
	generation         uint64
	restoredGeneration uint64
	filling            []accounting.UsageRecord
	sealed             []sealedBatch // sealed, not yet saved: no batch ID yet
	sealedRecords      int
	queue              []spoolEntry // saved, not acknowledged, in send order; queue[0] is outstanding
	queuedRecords      int
	// sealedBytes and queuedBytes are the encoded sizes of the checked sealed
	// batches and of the queued ones sealed by this process (memoryBytesLocked).
	sealedBytes int64
	queuedBytes int64
	// changed is closed and replaced whenever the queue shrinks (FlushUsage waits on it).
	changed chan struct{}
	// waitingSince is when the sealed and queued batches started waiting for the
	// control plane's next answer: when the first of them was sealed or restored, or
	// the control plane's last answer to one (ack or refusal) since. Zero while none
	// waits.
	waitingSince time.Time

	// sealWake asks the sealer to write sealed batches now; queued tells the sender a
	// batch joined the queue. Both hold at most one pending signal.
	sealWake chan struct{}
	queued   chan struct{}

	// persistMu serializes saves (the sealer and FlushUsage); index is guarded by
	// it.
	persistMu sync.Mutex
	index     spoolIndex

	// outstanding caches the head batch's records while it is being sent; the sender
	// goroutine alone uses it.
	outstanding *UsageBatch
}

func newUsageSender(c *Client) *usageSender {
	var store batchStore = newMemoryStore()
	if c.opts.Dir != nil {
		store = diskStore{dir: c.opts.Dir, logger: c.logger}
	}
	u := &usageSender{
		c:          c,
		store:      store,
		logger:     c.logger,
		instance:   c.opts.Instance,
		max:        c.opts.BatchMaxRecords,
		interval:   c.opts.BatchInterval,
		observer:   c.opts.Observer,
		backoff:    backoff{base: c.opts.BackoffBase, cap: c.opts.BackoffCap, random: c.opts.random},
		changed:    make(chan struct{}),
		generation: 1,
		sealWake:   make(chan struct{}, 1),
		queued:     make(chan struct{}, 1),
	}
	u.restore()
	return u
}

// signal leaves one pending signal on ch without blocking.
func signal(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// Record is the accounting batcher (accounting.Batcher): it appends the record to the
// filling batch, seals the batch when it is full, and returns the usage generation of
// the batch the record joined — read under the lock that seals, so the record is
// sealed in exactly that generation. It never blocks on I/O.
func (c *Client) Record(rec accounting.UsageRecord) uint64 {
	u := c.usage
	u.mu.Lock()
	generation := u.generation
	u.filling = append(u.filling, rec)
	full := len(u.filling) >= u.max
	if full {
		u.sealLocked()
	}
	u.mu.Unlock()
	if full {
		signal(u.sealWake)
	}
	return generation
}

// sealedBatch is a sealed batch's records and its usage generation; checked is set
// once its records were checked (usageSender.checkSealed), and bytes, their encoded
// size, with it.
type sealedBatch struct {
	records    []accounting.UsageRecord
	generation uint64
	checked    bool
	bytes      int64
}

// sealLocked moves the filling batch, if it holds records, to the sealed batches, and
// starts the next generation.
func (u *usageSender) sealLocked() {
	if len(u.filling) == 0 {
		return
	}
	u.sealed = append(u.sealed, sealedBatch{records: u.filling, generation: u.generation})
	u.generation++
	u.sealedRecords += len(u.filling)
	u.filling = nil
	u.depthChangedLocked()
}

// seal seals the filling batch and saves the sealed batches; trigger
// names what asked for it (interval, stop, a flush's trigger).
func (u *usageSender) seal(trigger string) {
	u.mu.Lock()
	u.sealLocked()
	u.mu.Unlock()
	u.logger.Debug("usage batches sealing", "kaiak.trigger", trigger)
	u.persist()
}

// depthChangedLocked follows a change to the sealed and queued batches: the ack
// clock (waitingSince) and the depth metrics. Callers hold u.mu.
func (u *usageSender) depthChangedLocked() {
	switch {
	case len(u.queue)+len(u.sealed) == 0:
		u.waitingSince = time.Time{}
	case u.waitingSince.IsZero():
		u.waitingSince = time.Now()
	}
	if u.observer != nil {
		u.observer.UsageSpoolDepth(len(u.queue)+len(u.sealed), u.queuedRecords+u.sealedRecords, u.memoryBytesLocked())
	}
}

// memoryBytesLocked is the encoded size of the unacknowledged records held in memory,
// what Options.UsageMemoryBytes bounds: the checked sealed batches, and the queued
// ones when the queue itself is in memory (no data directory). A batch counts from
// its check, which runs before it is saved: the sealed batches not yet checked wait
// at most one seal tick, and the filling batch is bounded by BatchMaxRecords. Callers
// hold u.mu.
func (u *usageSender) memoryBytesLocked() int64 {
	if u.store.inMemory() {
		return u.sealedBytes + u.queuedBytes
	}
	return u.sealedBytes
}

// runSealer seals the filling batch every interval, and saves sealed batches when
// Record sealed a full one. When ctx ends it seals and saves what is left, so a
// stop keeps every settled record in the store.
func (u *usageSender) runSealer(ctx context.Context) {
	ticker := time.NewTicker(u.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			u.seal("stop")
			return
		case <-ticker.C:
			u.seal("interval")
		case <-u.sealWake:
			// Record sealed a full batch; the one filling now waits for its own seal.
			u.persist()
		}
	}
}

// RestoredGeneration is the newest usage generation of the batches restored from the
// spool at start (0: none). Usage the previous run had not seen counted travels in
// them, so it is counted once generations up to this one are.
func (c *Client) RestoredGeneration() uint64 {
	c.usage.mu.Lock()
	defer c.usage.mu.Unlock()
	return c.usage.restoredGeneration
}

// UsageWaitingSince is when the usage batches not yet answered started waiting for
// the control plane's next answer — since the first was sealed, or since the last
// answer; zero while none waits. A config stream that stays open while this grows
// is a control plane that no longer takes usage (docs/specs/GATEWAY.md, Limits →
// Outage refusal).
func (c *Client) UsageWaitingSince() time.Time {
	c.usage.mu.Lock()
	defer c.usage.mu.Unlock()
	return c.usage.waitingSince
}

// countedGeneration is the newest usage generation among the queued batches at or
// before pos in its epoch: the control plane has counted them. 0 when none.
func (u *usageSender) countedGeneration(pos BatchPosition) uint64 {
	u.mu.Lock()
	defer u.mu.Unlock()
	var generation uint64
	for _, e := range u.queue {
		if e.id.Epoch == pos.Epoch && e.id.Sequence <= pos.Sequence {
			generation = max(generation, e.generation)
		}
	}
	return generation
}

// answered restarts the ack clock: the control plane answered the outstanding batch
// (ack or refusal), so what still waits waits from now — before the answer's totals
// reach the limiter.
func (u *usageSender) answered() {
	u.mu.Lock()
	u.waitingSince = time.Now()
	u.mu.Unlock()
}

// head returns the outstanding batch's entry: the queue's first.
func (u *usageSender) head() (spoolEntry, bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.queue) == 0 {
		return spoolEntry{}, false
	}
	return u.queue[0], true
}

// dropHead removes e, the outstanding batch, from the queue.
func (u *usageSender) dropHead(e spoolEntry) {
	u.outstanding = nil
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.queue) == 0 || u.queue[0].id != e.id {
		return
	}
	u.queue = u.queue[1:]
	u.queuedRecords -= e.records
	u.queuedBytes -= e.bytes
	u.depthChangedLocked()
	close(u.changed)
	u.changed = make(chan struct{})
}

// runSender sends the outstanding batch until the control plane acknowledges or
// refuses it, then the next; it waits for a batch when the queue is empty.
func (u *usageSender) runSender(ctx context.Context) {
	for ctx.Err() == nil {
		e, ok := u.head()
		if !ok {
			select {
			case <-ctx.Done():
				return
			case <-u.queued:
			}
			continue
		}
		batch, ok := u.load(e)
		if !ok {
			continue
		}
		u.sendOutstanding(ctx, e, batch)
	}
}

// load returns the outstanding batch, read from the store once. A batch that can no
// longer be read is set aside.
func (u *usageSender) load(e spoolEntry) (UsageBatch, bool) {
	if u.outstanding != nil && u.outstanding.Batch == e.id {
		return *u.outstanding, true
	}
	batch, err := u.store.load(e)
	if err == nil && (batch.Batch != e.id || len(batch.Records) == 0) {
		err = fmt.Errorf("holds batch %s/%d with %d records", batch.Batch.Epoch, batch.Batch.Sequence, len(batch.Records))
	}
	if err != nil {
		kept := u.setAside(e)
		u.logger.Error("usage batch unreadable; set aside", append([]any{"kaiak.usage.sequence", e.id.Sequence, "exception.message", err},
			fileAttr(kept)...)...)
		return UsageBatch{}, false
	}
	u.outstanding = &batch
	return batch, true
}

// sendOutstanding sends one attempt of the outstanding batch and acts on the answer:
// acknowledged → the totals go to the consumer (the batch counted, whatever the
// totals' revision) and the batch leaves the store;
// refused → set aside; anything else → the backoff delay, then the same batch again.
func (u *usageSender) sendOutstanding(ctx context.Context, e spoolEntry, batch UsageBatch) {
	attrs := []any{"kaiak.usage.epoch", e.id.Epoch, "kaiak.usage.sequence", e.id.Sequence, "kaiak.usage.records", e.records}
	if e.id.Instance != u.instance {
		attrs = append(attrs, "kaiak.usage.batch_instance", e.id.Instance)
	}
	ack, err := u.post(ctx, batch)
	if err != nil && ctx.Err() != nil {
		return // stopping: the batch stays queued
	}
	var refused *statusError
	switch {
	case err == nil:
		u.answered()
		u.c.takeTotals(ack.Totals, e.generation)
		if err := u.store.remove(e); err != nil {
			u.logger.Warn("acknowledged usage batch not removed from the spool; a restart resends it and the control plane acknowledges it again without counting",
				append(attrs, "exception.message", err)...)
		}
		u.dropHead(e)
		u.backoff.reset()
		u.observe(BatchAcked)
		u.logger.Debug("usage batch acknowledged", attrs...)
	case errors.As(err, &refused) && batchRefused(refused):
		u.answered()
		kept := u.setAside(e)
		u.observe(BatchRejected)
		u.logger.Error("usage batch refused by the control plane; set aside and the next one sent",
			append(append(attrs, "http.response.status_code", refused.status, "error.type", refused.code), fileAttr(kept)...)...)
	default:
		u.observe(BatchFailed)
		level := slog.LevelWarn
		if configProblem(err) {
			level = slog.LevelError
		}
		u.logger.Log(ctx, level, "usage batch not delivered; retrying", append(attrs, "exception.message", err)...)
		_ = u.c.opts.wait(ctx, u.backoff.next()) // cancelled: the loop sees ctx and stops
	}
}

// configProblem reports whether a failed send points at a configuration or version
// problem (the token, the protocol version, an endpoint that answers what it should
// not) rather than an outage: logged at error level, still retried.
func configProblem(err error) bool {
	if errors.Is(err, errProtocolMismatch) {
		return true
	}
	var s *statusError
	return errors.As(err, &s) && s.status < 500
}

func (u *usageSender) observe(result string) {
	if u.observer != nil {
		u.observer.UsageBatchSent(result, time.Now())
	}
}

// post sends the batch and returns the control plane's acknowledgement of it.
func (u *usageSender) post(ctx context.Context, batch UsageBatch) (UsageAck, error) {
	body, err := json.Marshal(batch)
	if err != nil {
		return UsageAck{}, fmt.Errorf("encode usage batch: %w", err)
	}
	reqCtx, cancel := context.WithTimeout(ctx, usageTimeout)
	defer cancel()
	resp, err := u.c.post(reqCtx, "/usage", batch.Batch.Instance, body, http.StatusOK)
	if err != nil {
		return UsageAck{}, fmt.Errorf("send usage batch: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxMessageBytes+1))
	if err != nil {
		return UsageAck{}, fmt.Errorf("read usage ack: %w", err)
	}
	if len(data) > maxMessageBytes {
		return UsageAck{}, fmt.Errorf("usage ack exceeds %d bytes", maxMessageBytes)
	}
	ack, err := DecodeUsageAck(data)
	if err != nil {
		return UsageAck{}, err
	}
	u.c.touch()
	if ack.Batch != batch.Batch {
		return UsageAck{}, fmt.Errorf("usage ack names batch %s/%d, sent %s/%d", ack.Batch.Epoch, ack.Batch.Sequence,
			batch.Batch.Epoch, batch.Batch.Sequence)
	}
	return ack, nil
}

// FlushUsage seals the filling batch, saves it, and waits until every queued batch is
// acknowledged or set aside, or until ctx ends; it reports whether nothing is left.
// The sending itself is Run's: FlushUsage is called while Run still runs (the drain,
// before the client stops). What is not delivered stays in the spool for the next
// start — or, with no data directory, is lost with the process.
func (c *Client) FlushUsage(ctx context.Context, trigger string) bool {
	u := c.usage
	u.seal(trigger)
	for {
		u.mu.Lock()
		batches, records := len(u.queue)+len(u.sealed), u.queuedRecords+u.sealedRecords
		empty := batches == 0 && len(u.filling) == 0
		changed := u.changed
		u.mu.Unlock()
		if empty {
			c.logger.Info("usage flushed", "kaiak.trigger", trigger)
			return true
		}
		select {
		case <-changed:
		case <-ctx.Done():
			// Batches lost at exit are billing data lost, logged at error level as the
			// other losses are; spooled ones wait for the next start.
			if u.store.inMemory() {
				c.logger.Error("usage not flushed: lost at exit (no data directory)", "kaiak.trigger", trigger,
					"kaiak.usage.batches", batches, "kaiak.usage.records", records)
				return false
			}
			c.logger.Warn("usage not flushed: left in the spool for the next start", "kaiak.trigger", trigger,
				"kaiak.usage.batches", batches, "kaiak.usage.records", records)
			return false
		}
	}
}
