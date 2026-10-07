package control

// The usage sender against fakecontrol, which de-duplicates batches by ID as the
// protocol settles, so exactly-once counting is checked from the gateway's side.
// Waits are on events the double or the observer reports, never fixed sleeps.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"kaiak/internal/accounting"
	"kaiak/internal/fakecontrol"
)

// testRecord is a valid usage record of the test instance; n makes it unique.
func testRecord(n int) accounting.UsageRecord {
	return accounting.UsageRecord{
		RecordID:        fmt.Sprintf("%032x", n+1),
		RequestID:       fmt.Sprintf("req-%d", n),
		GatewayInstance: testInstance,
		KeyID:           "k-eval",
		Groups:          []string{"research", "eval"},
		Model:           "llama",
		Deployment:      accounting.Deployment{Backend: "local", Model: "llama-3"},
		Units: accounting.Units{"tokens_in": 10, "tokens_cached": 0, "tokens_cache_write": 0, "tokens_out": 5,
			"tokens_reasoning": 0},
		CostNanoUSD: int64(n),
		GatewayTime: time.Date(2026, 9, 24, 10, 0, 0, n, time.UTC),
	}
}

// recordSize is rec's encoded size, what the usage memory bound counts.
func recordSize(t *testing.T, rec accounting.UsageRecord) int64 {
	t.Helper()
	data, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	return int64(len(data))
}

// testObserver records what the sender reports to metrics.
type testObserver struct {
	results chan string

	mu sync.Mutex
	// changed is closed and replaced on every depth report.
	changed     chan struct{}
	batches     int
	records     int
	memoryBytes int64
	maxDepth    int
	dropped     map[string]int
}

func newTestObserver() *testObserver {
	return &testObserver{results: make(chan string, 1000), changed: make(chan struct{}), dropped: map[string]int{}}
}

func (o *testObserver) UsageRecordsDropped(reason string, records int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.dropped[reason] += records
	close(o.changed)
	o.changed = make(chan struct{})
}

// waitDropped waits until records were dropped for reason.
func (o *testObserver) waitDropped(t *testing.T, reason string, records int) {
	t.Helper()
	timeout := time.After(testWaitLimit)
	for {
		o.mu.Lock()
		got, changed := o.dropped[reason], o.changed
		o.mu.Unlock()
		if got == records {
			return
		}
		select {
		case <-changed:
		case <-timeout:
			t.Fatalf("%d records dropped (%s), want %d", got, reason, records)
		}
	}
}

func (o *testObserver) UsageBatchSent(result string, _ time.Time) {
	select {
	case o.results <- result:
	default:
	}
}

func (o *testObserver) UsageQueueDepth(batches, records int, memoryBytes int64) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.batches, o.records, o.memoryBytes = batches, records, memoryBytes
	o.maxDepth = max(o.maxDepth, batches)
	close(o.changed)
	o.changed = make(chan struct{})
}

// waitDepth waits until the queue depth is batches.
func (o *testObserver) waitDepth(t *testing.T, batches int) {
	t.Helper()
	timeout := time.After(testWaitLimit)
	for {
		o.mu.Lock()
		got, changed := o.batches, o.changed
		o.mu.Unlock()
		if got == batches {
			return
		}
		select {
		case <-changed:
		case <-timeout:
			t.Fatalf("queue depth %d batches, want %d", got, batches)
		}
	}
}

func (o *testObserver) depth() (batches, records, maxBatches int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.batches, o.records, o.maxDepth
}

func (o *testObserver) next(t *testing.T) string {
	t.Helper()
	select {
	case r := <-o.results:
		return r
	case <-time.After(testWaitLimit):
		t.Fatal("no batch send reported")
		return ""
	}
}

// usageClient is a client booted and running, sealing by size only (maxRecords)
// unless tune says otherwise.
func (h *harness) usageClient(maxRecords int, tune func(*Options)) (*Client, *testObserver, func()) {
	h.t.Helper()
	obs := newTestObserver()
	c := h.client(func(o *Options) {
		o.BatchInterval = time.Hour
		o.BatchMaxRecords = maxRecords
		o.Observer = obs
		if tune != nil {
			tune(o)
		}
	})
	h.boot(c)
	return c, obs, h.run(c)
}

// nextUsage waits for the next batch the control plane takes, and checks its body
// against the usage batch schema and rules.
func (h *harness) nextUsage() fakecontrol.UsageEvent {
	h.t.Helper()
	select {
	case e := <-h.cp.UsageEvents():
		if _, err := DecodeUsageBatch(e.Body); err != nil {
			h.t.Fatalf("batch %+v breaks the protocol: %v", e.Batch, err)
		}
		return e
	case <-time.After(testWaitLimit):
		h.t.Fatal("no usage batch received")
		return fakecontrol.UsageEvent{}
	}
}

func (h *harness) wantUsage(outcome string, sequence int64, records int) fakecontrol.UsageEvent {
	h.t.Helper()
	e := h.nextUsage()
	if e.Outcome != outcome || e.Batch.Sequence != sequence || e.Records != records {
		h.t.Fatalf("batch %+v %s (%d records), want sequence %d %s (%d records)", e.Batch, e.Outcome, e.Records,
			sequence, outcome, records)
	}
	return e
}

func (h *harness) nextTotals() TotalsUpdate {
	h.t.Helper()
	select {
	case u := <-h.totals:
		return u
	case <-time.After(testWaitLimit):
		h.t.Fatal("no totals delivered")
		return TotalsUpdate{}
	}
}

func recordIDs(t *testing.T, body []byte) []string {
	t.Helper()
	b, err := DecodeUsageBatch(body)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, len(b.Records))
	for i, r := range b.Records {
		ids[i] = r.RecordID
	}
	return ids
}

func flush(t *testing.T, c *Client) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testWaitLimit)
	defer cancel()
	if !c.FlushUsage(ctx, "test") {
		t.Fatal("FlushUsage did not empty the queue")
	}
}

func TestUsageBatchesSealAtTheSizeLimit(t *testing.T) {
	h := newHarness(t)
	h.cp.Publish(configA(t))
	c, _, _ := h.usageClient(3, nil)
	for i := range 7 {
		c.Record(testRecord(i))
	}
	first := h.wantUsage(fakecontrol.OutcomeCounted, 1, 3)
	second := h.wantUsage(fakecontrol.OutcomeCounted, 2, 3)
	if first.Batch.Epoch != second.Batch.Epoch || first.Batch.Instance != testInstance {
		t.Errorf("batches %+v and %+v", first.Batch, second.Batch)
	}
	// The seventh record waits for the next seal; a flush seals it.
	flush(t, c)
	h.wantUsage(fakecontrol.OutcomeCounted, 3, 1)
	if got := len(h.cp.CountedRecords()); got != 7 {
		t.Errorf("%d records counted, want 7", got)
	}
}

func TestUsageBatchesSealOnTheInterval(t *testing.T) {
	h := newHarness(t)
	h.cp.Publish(configA(t))
	c, _, _ := h.usageClient(MaxBatchRecords, func(o *Options) { o.BatchInterval = 20 * time.Millisecond })
	c.Record(testRecord(1))
	c.Record(testRecord(2))
	h.wantUsage(fakecontrol.OutcomeCounted, 1, 2)
	c.Record(testRecord(3))
	h.wantUsage(fakecontrol.OutcomeCounted, 2, 1)
	// Intervals with nothing settled send nothing: an empty batch would be refused.
	flush(t, c)
	select {
	case e := <-h.cp.UsageEvents():
		t.Errorf("unexpected batch %+v %s", e.Batch, e.Outcome)
	default:
	}
}

func TestOneBatchOutstandingOthersQueueBehindIt(t *testing.T) {
	h := newHarness(t)
	h.cp.Publish(configA(t))
	h.cp.SetUsageFault(&fakecontrol.UsageFault{Status: http.StatusServiceUnavailable, Code: "internal-error"})
	c, obs, _ := h.usageClient(1, nil)
	for i := range 3 {
		c.Record(testRecord(i))
	}
	// Retried with the same ID, and nothing else sent meanwhile.
	firstBody := h.wantUsage(fakecontrol.OutcomeRefused, 1, 1).Body
	retry := h.wantUsage(fakecontrol.OutcomeRefused, 1, 1)
	if !slices.Equal(recordIDs(t, retry.Body), recordIDs(t, firstBody)) {
		t.Error("the retry carries other records than the first attempt")
	}
	if got := obs.next(t); got != BatchFailed {
		t.Errorf("result %q, want failed", got)
	}
	flushCtx, cancel := context.WithCancel(context.Background())
	flushed := make(chan bool, 1)
	go func() { flushed <- c.FlushUsage(flushCtx, "test") }()
	defer cancel()

	h.cp.SetUsageFault(nil)
	var sequences []int64
	for len(sequences) < 3 {
		if e := h.nextUsage(); e.Outcome == fakecontrol.OutcomeCounted {
			sequences = append(sequences, e.Batch.Sequence)
		} else if e.Batch.Sequence != 1 {
			t.Fatalf("batch %d sent while batch 1 was outstanding", e.Batch.Sequence)
		}
	}
	if !slices.Equal(sequences, []int64{1, 2, 3}) {
		t.Errorf("counted %v, want 1 2 3 in order", sequences)
	}
	if !<-flushed {
		t.Error("flush did not see the queue empty")
	}
	if batches, records, maxBatches := obs.depth(); batches != 0 || records != 0 || maxBatches != 3 {
		t.Errorf("queue depth %d batches %d records (max %d), want 0 0 (max 3)", batches, records, maxBatches)
	}
}

func TestLostAckIsResentAndCountedOnce(t *testing.T) {
	h := newHarness(t)
	h.cp.Publish(configA(t))
	h.cp.FailUsage(fakecontrol.UsageFault{DropAck: true})
	c, obs, _ := h.usageClient(2, nil)
	c.Record(testRecord(1))
	c.Record(testRecord(2))
	sent := h.wantUsage(fakecontrol.OutcomeAckDropped, 1, 2)
	resent := h.wantUsage(fakecontrol.OutcomeDuplicate, 1, 2)
	if sent.Batch != resent.Batch || !slices.Equal(recordIDs(t, sent.Body), recordIDs(t, resent.Body)) {
		t.Errorf("resent %+v, first sent %+v: want the same batch", resent.Batch, sent.Batch)
	}
	if n := len(h.cp.CountedRecords()); n != 2 {
		t.Errorf("%d records counted, want 2", n)
	}
	if got := []string{obs.next(t), obs.next(t)}; !slices.Equal(got, []string{BatchFailed, BatchAcked}) {
		t.Errorf("results %v", got)
	}
}

// An ack naming another batch is not the outstanding batch's: taken as its ack, the
// batch would leave the queue uncounted. It stays queued, is resent with the same ID
// and records, and the mismatch is logged.
func TestAckNamingAnotherBatchIsNotTaken(t *testing.T) {
	h := newHarness(t)
	h.cp.Publish(configA(t))
	h.cp.FailUsage(fakecontrol.UsageFault{AckOther: true})
	c, obs, _ := h.usageClient(1, nil)
	c.Record(testRecord(1))
	sent := h.wantUsage(fakecontrol.OutcomeAckOther, 1, 1)
	resent := h.wantUsage(fakecontrol.OutcomeCounted, 1, 1)
	if sent.Batch != resent.Batch || !slices.Equal(recordIDs(t, sent.Body), recordIDs(t, resent.Body)) {
		t.Errorf("resent %+v, first sent %+v: want the same batch", resent.Batch, sent.Batch)
	}
	if got := []string{obs.next(t), obs.next(t)}; !slices.Equal(got, []string{BatchFailed, BatchAcked}) {
		t.Errorf("results %v", got)
	}
	if n := len(h.cp.CountedRecords()); n != 1 {
		t.Errorf("%d records counted, want 1", n)
	}
	if out := h.logs.String(); !strings.Contains(out, "usage batch not delivered; retrying") ||
		!strings.Contains(out, fmt.Sprintf("usage ack names batch %s/2, sent %s/1", sent.Batch.Epoch, sent.Batch.Epoch)) {
		t.Errorf("mismatched ack not logged:\n%s", out)
	}
}

func TestRefusedBatchIsDroppedAndTheNextSent(t *testing.T) {
	h := newHarness(t)
	h.cp.Publish(configA(t))
	h.cp.FailUsage(fakecontrol.UsageFault{Status: http.StatusBadRequest, Code: "usage-batch-invalid"})
	c, obs, _ := h.usageClient(1, nil)
	c.Record(testRecord(1))
	c.Record(testRecord(2))
	h.wantUsage(fakecontrol.OutcomeRefused, 1, 1)
	h.wantUsage(fakecontrol.OutcomeCounted, 2, 1)
	if got := []string{obs.next(t), obs.next(t)}; !slices.Equal(got, []string{BatchRejected, BatchAcked}) {
		t.Errorf("results %v", got)
	}
	if n := len(h.cp.CountedRecords()); n != 1 {
		t.Errorf("%d records counted, want only the next batch's", n)
	}
	if out := h.logs.String(); !strings.Contains(out, "level=ERROR") ||
		!strings.Contains(out, "usage batch refused by the control plane; dropped") || !strings.Contains(out, "error.type=usage-batch-invalid") {
		t.Errorf("refusal not logged as an error with its code:\n%s", out)
	}
}

func TestBatchRefusalCodeOnAnotherStatusIsRetried(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusUnauthorized} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			h := newHarness(t)
			h.cp.Publish(configA(t))
			h.cp.FailUsage(fakecontrol.UsageFault{Status: status, Code: "request-invalid"})
			c, obs, _ := h.usageClient(1, nil)
			c.Record(testRecord(1))
			refused := h.wantUsage(fakecontrol.OutcomeRefused, 1, 1)
			resent := h.wantUsage(fakecontrol.OutcomeCounted, 1, 1)
			if refused.Batch != resent.Batch {
				t.Errorf("resent %+v, refused %+v: want the same batch", resent.Batch, refused.Batch)
			}
			if got := []string{obs.next(t), obs.next(t)}; !slices.Equal(got, []string{BatchFailed, BatchAcked}) {
				t.Errorf("results %v", got)
			}
		})
	}
}

func TestConfigProblemsAreRetriedAndLoggedAsErrors(t *testing.T) {
	h := newHarness(t)
	h.cp.Publish(configA(t))
	h.cp.SetUsageFault(&fakecontrol.UsageFault{Status: http.StatusUnauthorized, Code: "unauthorized"})
	c, obs, _ := h.usageClient(1, nil)
	c.Record(testRecord(1))
	h.wantUsage(fakecontrol.OutcomeRefused, 1, 1)
	h.wantUsage(fakecontrol.OutcomeRefused, 1, 1) // retried, not dropped
	if got := obs.next(t); got != BatchFailed {
		t.Fatalf("result %q, want failed", got)
	}
	if out := h.logs.String(); !strings.Contains(out, `level=ERROR msg="usage batch not delivered; retrying"`) {
		t.Errorf("not logged as an error:\n%s", out)
	}
	h.cp.SetUsageFault(nil)
	for e := h.nextUsage(); e.Outcome != fakecontrol.OutcomeCounted; e = h.nextUsage() {
	}
}

func TestEveryClientStartsANewEpoch(t *testing.T) {
	a := newHarness(t)
	a.cp.Publish(configA(t))
	ca, _, _ := a.usageClient(1, nil)
	ca.Record(testRecord(1))
	b := newHarness(t)
	b.cp.Publish(configA(t))
	cb, _, _ := b.usageClient(1, nil)
	cb.Record(testRecord(1))
	if ea, eb := a.nextUsage().Batch.Epoch, b.nextUsage().Batch.Epoch; ea == eb || !hex32Pattern.MatchString(ea) {
		t.Errorf("epochs %q and %q: want two random 32-hex epochs", ea, eb)
	}
}

// Record only appends in memory: with the sender stuck on an unreachable control
// plane, many concurrent requests settle without waiting, and the batches seal into
// the queue.
func TestRecordNeverBlocksOnTheSender(t *testing.T) {
	h := newHarness(t)
	h.cp.Publish(configA(t))
	h.cp.SetDown(true)
	c, obs, _ := h.usageClient(MaxBatchRecords, func(o *Options) {
		o.BackoffBase, o.BackoffCap = time.Hour, time.Hour
	})
	const writers, each = 8, 1250
	var wg sync.WaitGroup
	for w := range writers {
		wg.Go(func() {
			for i := range each {
				c.Record(testRecord(w*each + i))
			}
		})
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(testWaitLimit):
		t.Fatal("Record blocked")
	}
	want := writers * each / MaxBatchRecords
	obs.waitDepth(t, want)
	if batches, records, _ := obs.depth(); batches != want || records != writers*each {
		t.Errorf("queue depth %d batches %d records, want %d %d", batches, records, want, writers*each)
	}
}

func TestUsageBatchEncodesAsTheProtocolSays(t *testing.T) {
	batch := UsageBatch{Batch: BatchID{Instance: testInstance, Epoch: newEpoch(), Sequence: 1},
		Records: []accounting.UsageRecord{testRecord(1)}}
	data, err := json.Marshal(batch)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeUsageBatch(data); err != nil {
		t.Fatal(err)
	}
}

// H5: a record the protocol refuses (here a unit past 2^53 − 1, which settlement
// clamps — a record reaching the sender unclamped stands for any invalid record)
// would make the control plane refuse its whole batch. It is dropped alone at seal
// time; the rest of the batch is counted.
func TestInvalidRecordIsDroppedAloneAtSeal(t *testing.T) {
	h := newHarness(t)
	h.cp.Publish(configA(t))
	c, obs, _ := h.usageClient(3, nil)
	bad := testRecord(2)
	bad.Units = accounting.Units{"tokens_in": 1 << 53, "tokens_cached": 0, "tokens_cache_write": 0, "tokens_out": 0,
		"tokens_reasoning": 0}
	c.Record(testRecord(1))
	c.Record(bad)
	c.Record(testRecord(3))
	e := h.wantUsage(fakecontrol.OutcomeCounted, 1, 2)
	if ids := recordIDs(t, e.Body); slices.Contains(ids, bad.RecordID) {
		t.Errorf("the invalid record was sent: %v", ids)
	}
	obs.waitDropped(t, DroppedInvalid, 1)
	if out := h.logs.String(); !strings.Contains(out, "usage record refused by the protocol's checks") ||
		!strings.Contains(out, "kaiak.request.id="+bad.RequestID) {
		t.Errorf("refused record not logged with its request ID:\n%s", out)
	}
	// A batch of invalid records only is dropped whole: no empty batch, no sequence.
	c.Record(bad)
	c.Record(bad)
	c.Record(bad)
	obs.waitDropped(t, DroppedInvalid, 4)
	c.Record(testRecord(4))
	flush(t, c)
	h.wantUsage(fakecontrol.OutcomeCounted, 2, 1)
}
