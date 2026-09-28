package control

// The usage sender against fakecontrol, which de-duplicates batches by ID as the
// protocol settles, so exactly-once counting is checked from the gateway's side.
// Waits are on events the double or the observer reports, never fixed sleeps.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
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
		Units:           accounting.Units{"tokens_in": 10, "tokens_cached": 0, "tokens_out": 5, "tokens_reasoning": 0},
		CostNanoUSD:     int64(n),
		GatewayTime:     time.Date(2026, 9, 24, 10, 0, 0, n, time.UTC),
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

func (o *testObserver) UsageSpoolDepth(batches, records int, memoryBytes int64) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.batches, o.records, o.memoryBytes = batches, records, memoryBytes
	o.maxDepth = max(o.maxDepth, batches)
	close(o.changed)
	o.changed = make(chan struct{})
}

// waitDepth waits until the spool depth is batches.
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
			t.Fatalf("spool depth %d batches, want %d", got, batches)
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
	c.Boot(context.Background())
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

func (h *harness) spoolFiles(prefix string) []string {
	h.t.Helper()
	files, err := h.dir.List(prefix)
	if err != nil {
		h.t.Fatal(err)
	}
	var names []string
	for _, f := range files {
		names = append(names, f.Name)
	}
	return names
}

func flush(t *testing.T, c *Client) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testWaitLimit)
	defer cancel()
	if !c.FlushUsage(ctx, "test") {
		t.Fatal("FlushUsage did not empty the spool")
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
	for _, want := range []uint64{1, 2} {
		if u := h.nextTotals(); u.Counted != want || u.Totals == nil || u.Totals.ConfigVersion != 1 {
			t.Errorf("totals update %+v, want generation %d counted and totals under version 1", u, want)
		}
	}
	// The seventh record waits for the next seal; a flush seals it.
	flush(t, c)
	h.wantUsage(fakecontrol.OutcomeCounted, 3, 1)
	if got := len(h.cp.CountedRecords()); got != 7 {
		t.Errorf("%d records counted, want 7", got)
	}
	if files := h.spoolFiles(spoolBatchPrefix); len(files) != 0 {
		t.Errorf("acknowledged batches still spooled: %v", files)
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
	h.cp.SetUsageFault(&fakecontrol.UsageFault{Status: http.StatusServiceUnavailable, Code: "config-unavailable"})
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
		t.Error("flush did not see the spool empty")
	}
	if batches, records, maxBatches := obs.depth(); batches != 0 || records != 0 || maxBatches != 3 {
		t.Errorf("spool depth %d batches %d records (max %d), want 0 0 (max 3)", batches, records, maxBatches)
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
	if u := h.nextTotals(); u.Counted != 1 || u.Totals == nil {
		t.Errorf("totals update %+v, want the batch's generation counted with the totals", u)
	}
	if got := []string{obs.next(t), obs.next(t)}; !slices.Equal(got, []string{BatchFailed, BatchAcked}) {
		t.Errorf("results %v", got)
	}
}

func TestRefusedBatchIsSetAsideAndTheNextSent(t *testing.T) {
	h := newHarness(t)
	h.cp.Publish(configA(t))
	h.cp.FailUsage(fakecontrol.UsageFault{Status: http.StatusBadRequest, Code: "usage-batch-invalid"})
	c, obs, _ := h.usageClient(1, nil)
	c.Record(testRecord(1))
	c.Record(testRecord(2))
	refused := h.wantUsage(fakecontrol.OutcomeRefused, 1, 1)
	h.wantUsage(fakecontrol.OutcomeCounted, 2, 1)
	if got := []string{obs.next(t), obs.next(t)}; !slices.Equal(got, []string{BatchRejected, BatchAcked}) {
		t.Errorf("results %v", got)
	}
	want := batchFileName(spoolRejectedPrefix, BatchID(refused.Batch))
	if got := h.spoolFiles(spoolRejectedPrefix); !slices.Equal(got, []string{want}) {
		t.Errorf("refused files %v, want %s", got, want)
	}
	if out := h.logs.String(); !strings.Contains(out, "level=ERROR") ||
		!strings.Contains(out, "usage batch refused by the control plane") || !strings.Contains(out, "code=usage-batch-invalid") {
		t.Errorf("refusal not logged as an error with its code:\n%s", out)
	}
}

func TestRefusedBatchesKeptAreBounded(t *testing.T) {
	h := newHarness(t)
	h.cp.Publish(configA(t))
	n := rejectedKept + 2
	for range n {
		h.cp.FailUsage(fakecontrol.UsageFault{Status: http.StatusBadRequest, Code: "record-id-duplicate"})
	}
	c, _, _ := h.usageClient(1, nil)
	for i := range n + 1 {
		c.Record(testRecord(i))
	}
	for range n {
		h.nextUsage()
	}
	h.wantUsage(fakecontrol.OutcomeCounted, int64(n+1), 1)
	files := h.spoolFiles(spoolRejectedPrefix)
	if len(files) != rejectedKept {
		t.Fatalf("%d refused batches kept, want %d: %v", len(files), rejectedKept, files)
	}
	if slices.ContainsFunc(files, func(f string) bool { return strings.HasSuffix(f, "-1.json") }) {
		t.Errorf("the oldest refused batch was kept: %v", files)
	}
}

func TestConfigProblemsAreRetriedAndLoggedAsErrors(t *testing.T) {
	h := newHarness(t)
	h.cp.Publish(configA(t))
	h.cp.SetUsageFault(&fakecontrol.UsageFault{Status: http.StatusUnauthorized, Code: "unauthorized"})
	c, obs, _ := h.usageClient(1, nil)
	c.Record(testRecord(1))
	h.wantUsage(fakecontrol.OutcomeRefused, 1, 1)
	h.wantUsage(fakecontrol.OutcomeRefused, 1, 1) // retried, not set aside
	if got := obs.next(t); got != BatchFailed {
		t.Fatalf("result %q, want failed", got)
	}
	if out := h.logs.String(); !strings.Contains(out, `level=ERROR msg="usage batch not delivered; retrying"`) {
		t.Errorf("not logged as an error:\n%s", out)
	}
	h.cp.SetUsageFault(nil)
	for e := h.nextUsage(); e.Outcome != fakecontrol.OutcomeCounted; e = h.nextUsage() {
	}
	if files := h.spoolFiles(spoolRejectedPrefix); len(files) != 0 {
		t.Errorf("batch set aside over a configuration problem: %v", files)
	}
}

// A gateway stopped between a send and its ack (the ack lost) restarts from its
// spool: same epoch, the outstanding batch resent with its ID and records, the
// sequence continuing — and the control plane counts every record once.
func TestSpoolSurvivesARestart(t *testing.T) {
	h := newHarness(t)
	h.cp.Publish(configA(t))
	h.cp.FailUsage(fakecontrol.UsageFault{DropAck: true})
	h.cp.SetUsageFault(&fakecontrol.UsageFault{Status: http.StatusServiceUnavailable, Code: "config-unavailable"})
	c, _, stop := h.usageClient(2, nil)
	for i := range 3 {
		c.Record(testRecord(i))
	}
	first := h.wantUsage(fakecontrol.OutcomeAckDropped, 1, 2)
	h.wantUsage(fakecontrol.OutcomeRefused, 1, 2)
	stop() // the third record is sealed into the spool as the client stops
	if files := h.spoolFiles(spoolBatchPrefix); len(files) != 2 {
		t.Fatalf("spool after stop: %v, want 2 batches", files)
	}

	h.cp.SetUsageFault(nil)
	c2, _, _ := h.usageClient(2, nil)
	resent := h.wantUsage(fakecontrol.OutcomeDuplicate, 1, 2)
	if resent.Batch != first.Batch || !slices.Equal(recordIDs(t, resent.Body), recordIDs(t, first.Body)) {
		t.Errorf("resent %+v, want batch %+v with the same records", resent.Batch, first.Batch)
	}
	second := h.wantUsage(fakecontrol.OutcomeCounted, 2, 1)
	c2.Record(testRecord(3))
	flush(t, c2)
	third := h.wantUsage(fakecontrol.OutcomeCounted, 3, 1)
	if second.Batch.Epoch != first.Batch.Epoch || third.Batch.Epoch != first.Batch.Epoch {
		t.Errorf("epochs %s %s %s: want one epoch across the restart", first.Batch.Epoch, second.Batch.Epoch,
			third.Batch.Epoch)
	}
	if n := len(h.cp.CountedRecords()); n != 4 {
		t.Errorf("%d records counted, want 4, each once", n)
	}
	if !strings.Contains(h.logs.String(), `msg="usage spool restored"`) {
		t.Errorf("restore not logged:\n%s", h.logs.String())
	}
}

// A spool of the previous format (records named an owner, not groups) is discarded.
func TestSpoolOfAnotherFormatStartsANewEpoch(t *testing.T) {
	h := newHarness(t)
	h.cp.Publish(configA(t))
	c, _, stop := h.usageClient(1, nil)
	c.Record(testRecord(1))
	old := h.wantUsage(fakecontrol.OutcomeCounted, 1, 1)
	h.nextTotals() // the ack is taken: nothing left to resend
	stop()

	if err := h.dir.WriteVersioned(SpoolFile, spoolFormat-1, map[string]any{}); err != nil {
		t.Fatal(err)
	}
	c2, _, _ := h.usageClient(1, nil)
	c2.Record(testRecord(2))
	fresh := h.wantUsage(fakecontrol.OutcomeCounted, 1, 1)
	if fresh.Batch.Epoch == old.Batch.Epoch {
		t.Errorf("epoch %s kept after the spool was discarded", fresh.Batch.Epoch)
	}
	out := h.logs.String()
	if !strings.Contains(out, "discarded data file with a different format version") ||
		!strings.Contains(out, `msg="usage spool: new epoch"`) {
		t.Errorf("discard and new epoch not logged:\n%s", out)
	}
}

// A spool left by the format-1 gateway — an index and a queued batch whose record
// names an owner, not groups — is discarded whole: the batch is never sent, and the
// first batch after the start opens a new epoch. The version is written literally, so
// the test does not move with spoolFormat.
func TestFormatOneSpoolIsDiscardedWithItsBatches(t *testing.T) {
	const oldEpoch = "0123456789abcdef0123456789abcdef"
	h := newHarness(t)
	h.cp.Publish(configA(t))
	index := json.RawMessage(`{"instance":"` + testInstance + `","epoch":"` + oldEpoch + `","next_sequence":2}`)
	batchFile := spoolBatchPrefix + oldEpoch + "-1.json"
	batch := json.RawMessage(`{"batch":{"instance":"` + testInstance + `","epoch":"` + oldEpoch + `","sequence":1},
  "records":[{"record_id":"` + fmt.Sprintf("%032x", 99) + `","request_id":"req-old","gateway_instance":"` + testInstance + `",
    "key_id":"k-eval","owner":{"team":"research","workload":"eval"},"model":"llama",
    "deployment":{"backend":"local","model":"llama-3"},
    "units":{"tokens_in":10,"tokens_cached":0,"tokens_out":5,"tokens_reasoning":0},
    "cost_nano_usd":1,"estimated":false,"partial":false,"gateway_time":"2026-09-24T10:00:00Z"}]}`)
	if err := h.dir.WriteVersioned(SpoolFile, 1, index); err != nil {
		t.Fatal(err)
	}
	if err := h.dir.WriteVersioned(batchFile, 1, batch); err != nil {
		t.Fatal(err)
	}

	c, _, _ := h.usageClient(1, nil)
	c.Record(testRecord(1))
	fresh := h.wantUsage(fakecontrol.OutcomeCounted, 1, 1)
	if fresh.Batch.Epoch == oldEpoch {
		t.Errorf("epoch %s of the discarded spool kept", oldEpoch)
	}
	counted := h.cp.CountedRecords()
	if len(counted) != 1 {
		t.Fatalf("%d records counted, want only the new one", len(counted))
	}
	var rec struct {
		Groups []string `json:"groups"`
	}
	if err := json.Unmarshal(counted[0], &rec); err != nil || len(rec.Groups) == 0 {
		t.Errorf("counted record %s has no groups (err %v)", counted[0], err)
	}
	if files := h.spoolFiles(spoolBatchPrefix); slices.Contains(files, batchFile) {
		t.Errorf("format-1 batch file still spooled: %v", files)
	}
	if n := strings.Count(h.logs.String(), "discarded data file with a different format version"); n != 2 {
		t.Errorf("%d discards logged, want 2 (index and batch):\n%s", n, h.logs.String())
	}
}

func TestFreshDataDirectoryStartsANewEpoch(t *testing.T) {
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

// A spool left by another instance ID (a container whose hostname changed) is still
// delivered, under that instance; the new instance seals in an epoch of its own.
func TestSpoolOfAnotherInstanceIsDeliveredUnderIt(t *testing.T) {
	h := newHarness(t)
	h.cp.Publish(configA(t))
	h.cp.SetUsageFault(&fakecontrol.UsageFault{Status: http.StatusServiceUnavailable, Code: "config-unavailable"})
	c, _, stop := h.usageClient(1, nil)
	c.Record(testRecord(1))
	h.wantUsage(fakecontrol.OutcomeRefused, 1, 1)
	stop()

	h.cp.SetUsageFault(nil)
	renamed := testRecord(2)
	renamed.GatewayInstance = "gw-renamed"
	c2, _, _ := h.usageClient(1, func(o *Options) { o.Instance = "gw-renamed" })
	old := h.wantUsage(fakecontrol.OutcomeCounted, 1, 1)
	c2.Record(renamed)
	fresh := h.wantUsage(fakecontrol.OutcomeCounted, 1, 1)
	if old.Batch.Instance != testInstance || fresh.Batch.Instance != "gw-renamed" || old.Batch.Epoch == fresh.Batch.Epoch {
		t.Errorf("batches %+v then %+v", old.Batch, fresh.Batch)
	}
}

func TestFlushLeavesUndeliveredBatchesSpooled(t *testing.T) {
	h := newHarness(t)
	h.cp.Publish(configA(t))
	h.cp.SetDown(true)
	c, _, _ := h.usageClient(MaxBatchRecords, nil)
	c.Record(testRecord(1))
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if c.FlushUsage(ctx, "drain") {
		t.Fatal("FlushUsage reports an empty spool with the control plane down")
	}
	if files := h.spoolFiles(spoolBatchPrefix); len(files) != 1 {
		t.Errorf("spool %v, want the sealed batch", files)
	}
	if !strings.Contains(h.logs.String(), "usage not flushed: left in the spool for the next start") {
		t.Errorf("not logged:\n%s", h.logs.String())
	}
}

// Record only appends in memory: with the sender stuck on an unreachable control
// plane, many concurrent requests settle without waiting, and the batches seal into
// the spool.
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
	// The depth counts sealed batches as soon as they seal; the files follow.
	flushCtx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	c.FlushUsage(flushCtx, "test") // writes what is sealed; the control plane stays down
	if n := len(h.spoolFiles(spoolBatchPrefix)); n != want {
		t.Fatalf("%d batches spooled, want %d", n, want)
	}
	if batches, records, _ := obs.depth(); batches != want || records != writers*each {
		t.Errorf("spool depth %d batches %d records, want %d %d", batches, records, want, writers*each)
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
// would make the control plane refuse its whole batch. It is set aside alone at
// seal time; the rest of the batch is counted.
func TestInvalidRecordIsSetAsideAloneAtSeal(t *testing.T) {
	h := newHarness(t)
	h.cp.Publish(configA(t))
	c, obs, _ := h.usageClient(3, nil)
	bad := testRecord(2)
	bad.Units = accounting.Units{"tokens_in": 1 << 53, "tokens_cached": 0, "tokens_out": 0, "tokens_reasoning": 0}
	c.Record(testRecord(1))
	c.Record(bad)
	c.Record(testRecord(3))
	e := h.wantUsage(fakecontrol.OutcomeCounted, 1, 2)
	if ids := recordIDs(t, e.Body); slices.Contains(ids, bad.RecordID) {
		t.Errorf("the invalid record was sent: %v", ids)
	}
	obs.waitDropped(t, DroppedInvalid, 1)
	if got := h.spoolFiles(spoolRejectedPrefix); !slices.Equal(got, []string{spoolRejectedPrefix + "record-" + bad.RecordID + ".json"}) {
		t.Errorf("refused files %v, want the record's", got)
	}
	if out := h.logs.String(); !strings.Contains(out, "usage record refused by the protocol's checks") ||
		!strings.Contains(out, "request_id="+bad.RequestID) {
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

// L16: while the spool cannot be written, sealed records wait in memory up to their
// bound (their encoded bytes, here 4 records' worth: testRecord(1) to (9) are the
// same size); past it the oldest sealed batches are dropped (logged, counted), so a disk
// that stays unwritable cannot grow the process without bound. Once the disk
// recovers, what was kept is written and sent.
func TestSealedRecordsInMemoryAreBoundedWhileTheSpoolCannotBeWritten(t *testing.T) {
	h := newHarness(t)
	h.cp.Publish(configA(t))
	c, obs, _ := h.usageClient(2, func(o *Options) { o.UsageMemoryBytes = 4 * recordSize(t, testRecord(9)) })
	if err := os.Chmod(h.dir.Path(), 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(h.dir.Path(), 0o700) })
	for i := range 10 {
		c.Record(testRecord(i))
	}
	obs.waitDropped(t, DroppedSpoolFull, 6)
	if out := h.logs.String(); !strings.Contains(out, "oldest sealed usage batches dropped to bound memory") {
		t.Errorf("drop not logged:\n%s", out)
	}
	if err := os.Chmod(h.dir.Path(), 0o700); err != nil {
		t.Fatal(err)
	}
	flush(t, c)
	var sent []string
	for _, raw := range h.cp.CountedRecords() {
		var rec accounting.UsageRecord
		if err := json.Unmarshal(raw, &rec); err != nil {
			t.Fatal(err)
		}
		sent = append(sent, rec.RequestID)
	}
	if want := []string{"req-6", "req-7", "req-8", "req-9"}; !slices.Equal(sent, want) {
		t.Errorf("records counted %v, want the newest %v", sent, want)
	}
}
