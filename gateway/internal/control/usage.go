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
	"kaiak/internal/netfail"
)

// Usage delivery (CONTROL-PROTOCOL.md, Usage batches): settled records fill a batch;
// the batch is sealed every BatchInterval or at BatchMaxRecords, checked, and queued in
// memory under the next sequence of the epoch (queue.go). One goroutine seals and
// queues, another sends the head of the queue — at most one batch outstanding — and
// retries it with the same ID until the control plane acknowledges or refuses it.
// Record, the accounting sink, only appends in memory: nothing on the request path
// waits for the network.

// DefaultBatchInterval seals the filling batch when Options leave it zero; a batch
// also seals at MaxBatchRecords (the default BatchMaxRecords).
const DefaultBatchInterval = 5 * time.Second

// ackedRemembered bounds the acknowledged batches remembered until shown counted
// (docs/specs/GATEWAY.md, Usage batches): past it the oldest is forgotten.
const ackedRemembered = 10_000

// usageTimeout bounds one POST /v1/usage.
const usageTimeout = 30 * time.Second

// Batch send results, as UsageObserver hears them.
const (
	BatchAcked    = "acked"
	BatchRejected = "rejected"
	BatchFailed   = "failed"
)

// UsageObserver is told how usage delivery goes, for metrics. Its methods are called
// from the client's goroutines, and from Record's caller for the queue depth, and
// must not block.
type UsageObserver interface {
	// UsageBatchSent reports one POST /v1/usage and its result (BatchAcked,
	// BatchRejected, BatchFailed), at the time it ended.
	UsageBatchSent(result string, at time.Time)
	// UsageQueueDepth reports the sealed batches not yet acknowledged, the records in
	// them, and the encoded bytes of the queued ones, which count against
	// Options.UsageMemoryBytes.
	UsageQueueDepth(batches, records int, queuedBytes int64)
	// UsageRecordsDropped reports records dropped before reaching the control
	// plane, with the reason: DroppedInvalid or DroppedMemoryBound.
	UsageRecordsDropped(reason string, records int)
}

// Reasons usage records are dropped, as UsageObserver hears them.
const (
	// DroppedInvalid: the record fails the usage record's checks, so the control
	// plane would refuse its whole batch; it is dropped alone.
	DroppedInvalid = "invalid"
	// DroppedMemoryBound: the control plane left the queued batches unacknowledged
	// until their records passed the bound (Options.UsageMemoryBytes), and the oldest
	// were dropped.
	DroppedMemoryBound = "memory_bound"
)

// batchRefusals are the error codes that mean the batch itself can never be accepted
// (CONTROL-PROTOCOL.md, Usage intake and Request checks): it is dropped rather than
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
	logger   *slog.Logger
	instance string
	// epoch is the process's batch epoch: new with every process, so no batch ID is
	// ever reused.
	epoch    string
	max      int
	interval time.Duration
	observer UsageObserver
	// backoff is the sender goroutine's own: the config follower has another.
	backoff backoff

	mu sync.Mutex
	// generation is the filling batch's usage generation: the process's first batch
	// takes 1, one more per sealed batch. A totals message showing a batch counted
	// names its generation to the consumer (TotalsUpdate.Counted).
	generation    uint64
	filling       []accounting.UsageRecord
	sealed        []sealedBatch // sealed, not yet checked and queued: no batch ID yet
	sealedRecords int
	queue         []queuedBatch // not acknowledged, in send order; queue[0] is outstanding
	// acked are the batches acknowledged and not yet shown counted, in send order: an
	// ack only stops a batch being sent, its usage leaves the limiter once applied
	// totals show it counted (countedGeneration, which forgets it then). At most
	// ackedRemembered are kept.
	acked []ackedBatch
	// lastCounted is the counted_through of the totals applied last, by epoch: a batch
	// it covers is acknowledged already covered.
	lastCounted map[string]int64
	// uncountedSince is when the oldest acknowledged batch not yet covered was
	// acknowledged; zero while none waits. Forgetting the oldest past ackedRemembered
	// keeps it.
	uncountedSince time.Time
	queuedRecords  int
	// queuedBytes is the encoded size of the queued records (Options.UsageMemoryBytes
	// bounds it).
	queuedBytes int64
	// changed is closed and replaced whenever the queue shrinks (FlushUsage waits on it).
	changed chan struct{}
	// waitingSince is when the sealed and queued batches started waiting for the
	// control plane's next answer: when the first of them was sealed, or the control
	// plane's last answer to one (ack or refusal) since. Zero while none waits.
	waitingSince time.Time

	// sealWake asks the sealer to queue sealed batches now; queued tells the sender a
	// batch joined the queue. Both hold at most one pending signal.
	sealWake chan struct{}
	queued   chan struct{}

	// queueMu serializes queueSealed (the sealer and FlushUsage); nextSequence, the
	// sequence the next queued batch takes, is guarded by it.
	queueMu      sync.Mutex
	nextSequence int64
}

func newUsageSender(c *Client) *usageSender {
	u := &usageSender{
		c:            c,
		logger:       c.logger,
		instance:     c.opts.Instance,
		epoch:        newEpoch(),
		max:          c.opts.BatchMaxRecords,
		interval:     c.opts.BatchInterval,
		observer:     c.opts.Observer,
		backoff:      backoff{base: c.opts.BackoffBase, cap: c.opts.BackoffCap, random: c.opts.random},
		changed:      make(chan struct{}),
		generation:   1,
		sealWake:     make(chan struct{}, 1),
		queued:       make(chan struct{}, 1),
		nextSequence: 1,
	}
	u.logger.Info("usage batches kept in memory until acknowledged", "kaiak.usage.epoch", u.epoch)
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

// sealedBatch is a sealed batch's records and its usage generation.
type sealedBatch struct {
	records    []accounting.UsageRecord
	generation uint64
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

// seal seals the filling batch and queues the sealed batches; trigger names what
// asked for it (interval, stop, a flush's trigger).
func (u *usageSender) seal(trigger string) {
	u.mu.Lock()
	u.sealLocked()
	u.mu.Unlock()
	u.logger.Debug("usage batches sealing", "kaiak.trigger", trigger)
	u.queueSealed()
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
		u.observer.UsageQueueDepth(len(u.queue)+len(u.sealed), u.queuedRecords+u.sealedRecords, u.queuedBytes)
	}
}

// runSealer seals the filling batch every interval, and queues sealed batches when
// Record sealed a full one. When ctx ends it seals and queues what is left, so the
// drain's flush finds every settled record queued.
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
			u.queueSealed()
		}
	}
}

// throughOf is counted_through by epoch.
func throughOf(counted []BatchPosition) map[string]int64 {
	through := make(map[string]int64, len(counted))
	for _, pos := range counted {
		through[pos.Epoch] = pos.Sequence
	}
	return through
}

// covers reports whether through — counted_through by epoch — covers batch id.
func covers(through map[string]int64, id BatchID) bool {
	last, ok := through[id.Epoch]
	return ok && id.Sequence <= last
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

// UsageUncountedSince is when the oldest batch acknowledged but not yet shown counted
// by applied totals was acknowledged; zero while none waits. A config stream that
// stays open while this grows brings no totals that count this gateway's usage: its
// bases are frozen (docs/specs/GATEWAY.md, Limits → Usage acks count for money
// limits).
func (c *Client) UsageUncountedSince() time.Time {
	c.usage.mu.Lock()
	defer c.usage.mu.Unlock()
	return c.usage.uncountedSince
}

// countedGeneration is the newest usage generation among the batches a totals event
// shows counted: in send order — acknowledged ones first, then the queue — every
// batch at or before the epoch's entry in counted. Batches go out one at a time, so
// the control plane counts them in that order: every acknowledged batch sent before a
// covered one is covered too. The acknowledged batches it covers are forgotten — their
// usage leaves the limiter now. 0 when it covers none.
func (u *usageSender) countedGeneration(counted []BatchPosition) uint64 {
	through := throughOf(counted)
	u.mu.Lock()
	defer u.mu.Unlock()
	u.lastCounted = through
	var generation uint64
	covered := -1 // the last acknowledged batch covered
	for i, a := range u.acked {
		if covers(through, a.id) {
			covered = i
		}
	}
	for _, b := range u.queue {
		if covers(through, b.id) {
			covered, generation = len(u.acked)-1, max(generation, b.generation)
		}
	}
	for _, a := range u.acked[:covered+1] {
		generation = max(generation, a.generation)
	}
	if covered >= 0 {
		u.acked = u.acked[covered+1:]
		u.uncountedChangedLocked()
	}
	return generation
}

// uncountedChangedLocked sets uncountedSince to the acknowledgement of the oldest
// acknowledged batch still remembered; zero when none is. Callers hold u.mu.
func (u *usageSender) uncountedChangedLocked() {
	u.uncountedSince = time.Time{}
	if len(u.acked) > 0 {
		u.uncountedSince = u.acked[0].ackedAt
	}
}

// answered restarts the ack clock: the control plane answered the outstanding batch
// (ack or refusal), so what still waits waits from now.
func (u *usageSender) answered() {
	u.mu.Lock()
	u.waitingSince = time.Now()
	u.mu.Unlock()
}

// head returns the outstanding batch: the queue's first.
func (u *usageSender) head() (queuedBatch, bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.queue) == 0 {
		return queuedBatch{}, false
	}
	return u.queue[0], true
}

// ackedBatch is a batch acknowledged and not yet shown counted: its ID, its usage
// generation, and when it was acknowledged.
type ackedBatch struct {
	id         BatchID
	generation uint64
	ackedAt    time.Time
}

// dropHeadLocked removes b, the outstanding batch, from the queue. Callers hold u.mu.
func (u *usageSender) dropHeadLocked(b queuedBatch) {
	if len(u.queue) == 0 || u.queue[0].id != b.id {
		return
	}
	u.queue = u.queue[1:]
	u.queuedRecords -= len(b.records)
	u.queuedBytes -= b.bytes
	u.depthChangedLocked()
	close(u.changed)
	u.changed = make(chan struct{})
}

// dropHead removes b, the outstanding batch, from the queue.
func (u *usageSender) dropHead(b queuedBatch) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.dropHeadLocked(b)
}

// acknowledged moves b, the outstanding batch, from the queue to the acknowledged
// batches, under one hold of the lock: a totals event handled meanwhile finds it in
// one or the other, never in neither. A batch the totals applied last already cover is
// not remembered (its ack was lost, and it was sent again). Past ackedRemembered the
// oldest is forgotten — which only over-counts until a later batch is shown counted,
// since that retires every earlier generation — keeping the wait's time.
func (u *usageSender) acknowledged(b queuedBatch) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.dropHeadLocked(b)
	if covers(u.lastCounted, b.id) {
		return
	}
	if len(u.acked) >= ackedRemembered {
		u.acked = u.acked[1:]
	}
	now := time.Now()
	u.acked = append(u.acked, ackedBatch{id: b.id, generation: b.generation, ackedAt: now})
	if u.uncountedSince.IsZero() {
		u.uncountedSince = now
	}
}

// runSender sends the outstanding batch until the control plane acknowledges or
// refuses it, then the next; it waits for a batch when the queue is empty.
func (u *usageSender) runSender(ctx context.Context) {
	for ctx.Err() == nil {
		b, ok := u.head()
		if !ok {
			select {
			case <-ctx.Done():
				return
			case <-u.queued:
			}
			continue
		}
		u.sendOutstanding(ctx, b)
	}
}

// sendOutstanding sends one attempt of the outstanding batch and acts on the answer:
// acknowledged → the batch is not sent again, and waits among the acknowledged
// batches until a totals event shows it counted; refused → dropped; anything else →
// the backoff delay, then the same batch again.
func (u *usageSender) sendOutstanding(ctx context.Context, b queuedBatch) {
	attrs := []any{"kaiak.usage.epoch", b.id.Epoch, "kaiak.usage.sequence", b.id.Sequence, "kaiak.usage.records", len(b.records)}
	_, err := u.post(ctx, UsageBatch{Batch: b.id, Records: b.records})
	if err != nil && ctx.Err() != nil {
		return // stopping: the batch stays queued
	}
	var refused *statusError
	switch {
	case err == nil:
		u.answered()
		u.acknowledged(b)
		u.backoff.reset()
		u.observe(BatchAcked)
		u.logger.Debug("usage batch acknowledged", attrs...)
	case errors.As(err, &refused) && batchRefused(refused):
		u.answered()
		u.dropHead(b)
		u.observe(BatchRejected)
		u.logger.Error("usage batch refused by the control plane; dropped and the next one sent",
			append(attrs, "http.response.status_code", refused.status, "error.type", refused.code)...)
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
		return UsageAck{}, fmt.Errorf("read usage ack: %s", netfail.Class(err))
	}
	if len(data) > maxMessageBytes {
		return UsageAck{}, fmt.Errorf("usage ack exceeds %d bytes", maxMessageBytes)
	}
	ack, err := DecodeUsageAck(data)
	if err != nil {
		return UsageAck{}, err
	}
	if ack.Batch != batch.Batch {
		return UsageAck{}, fmt.Errorf("usage ack names batch %s/%d, sent %s/%d", ack.Batch.Epoch, ack.Batch.Sequence,
			batch.Batch.Epoch, batch.Batch.Sequence)
	}
	return ack, nil
}

// FlushUsage seals the filling batch, queues it, and waits until every queued batch is
// acknowledged or dropped, or until ctx ends; it reports whether nothing is left. The
// sending itself is Run's: FlushUsage is called while Run still runs (the drain, before
// the client stops). What is not delivered is lost with the process.
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
			// other losses are.
			c.logger.Error("usage not flushed: lost at exit", "kaiak.trigger", trigger,
				"kaiak.usage.batches", batches, "kaiak.usage.records", records)
			return false
		}
	}
}
