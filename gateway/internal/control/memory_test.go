package control

// E1: with no data directory (Options.Dir nil) the client keeps everything in memory:
// usage batches until acknowledged (bounded), a fresh epoch per process, no
// last-known-good copy. The filesystem side — nothing written at all — is checked on
// the whole process (cmd/kaiak).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"kaiak/internal/accounting"
	"kaiak/internal/fakecontrol"
)

func withoutDataDir(o *Options) { o.Dir = nil }

func TestWithoutADataDirectoryBatchesAreDeliveredFromMemory(t *testing.T) {
	h := newHarness(t)
	h.cp.Publish(configA(t))
	c, _, _ := h.usageClient(2, withoutDataDir)
	for i := range 5 {
		c.Record(testRecord(i))
	}
	first := h.wantUsage(fakecontrol.OutcomeCounted, 1, 2)
	h.wantUsage(fakecontrol.OutcomeCounted, 2, 2)
	flush(t, c)
	h.wantUsage(fakecontrol.OutcomeCounted, 3, 1)
	if got := len(h.cp.CountedRecords()); got != 5 {
		t.Errorf("%d records counted, want 5", got)
	}
	if files := h.spoolFiles("usage-"); len(files) != 0 {
		t.Errorf("files in the (unused) data directory: %v", files)
	}
	if !strings.Contains(h.logs.String(), "usage batches kept in memory until acknowledged: no data directory") {
		t.Errorf("memory mode not logged:\n%s", h.logs.String())
	}

	// Every process starts a fresh epoch: nothing ties it to the previous one.
	c2, _, _ := h.usageClient(1, withoutDataDir)
	c2.Record(testRecord(9))
	if e := h.wantUsage(fakecontrol.OutcomeCounted, 1, 1); e.Batch.Epoch == first.Batch.Epoch {
		t.Errorf("second process reused epoch %s", e.Batch.Epoch)
	}
}

// O7: batches the flush could not deliver are lost with the process — billing data
// lost, logged at error level like the other losses.
func TestWithoutADataDirectoryAnUndeliveredFlushIsLoggedAsLost(t *testing.T) {
	h := newHarness(t)
	h.cp.Publish(configA(t))
	h.cp.SetDown(true)
	c, _, _ := h.usageClient(MaxBatchRecords, withoutDataDir)
	c.Record(testRecord(1))
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if c.FlushUsage(ctx, "drain") {
		t.Fatal("FlushUsage reports everything delivered with the control plane down")
	}
	logged := false
	for line := range strings.SplitSeq(h.logs.String(), "\n") {
		logged = logged || strings.Contains(line, `level=ERROR msg="usage not flushed: lost at exit (no data directory)"`) &&
			strings.HasSuffix(line, "kaiak.trigger=drain kaiak.usage.batches=1 kaiak.usage.records=1")
	}
	if !logged {
		t.Errorf("no error-level loss line for 1 batch, 1 record:\n%s", h.logs.String())
	}
}

// Past the bound the oldest queued batches are dropped — but not the outstanding one,
// which may be on the wire — logged and counted with their own reason. The bound is
// 4 records' worth of encoded bytes (testRecord(1) to (9) are the same size,
// testRecord(0) smaller).
func TestWithoutADataDirectoryQueuedRecordsAreBounded(t *testing.T) {
	h := newHarness(t)
	h.cp.Publish(configA(t))
	h.cp.SetUsageFault(&fakecontrol.UsageFault{Status: http.StatusServiceUnavailable, Code: "internal-error"})
	c, obs, _ := h.usageClient(2, func(o *Options) {
		withoutDataDir(o)
		o.UsageMemoryBytes = 4 * recordSize(t, testRecord(9))
	})
	for i := range 10 {
		c.Record(testRecord(i))
	}
	obs.waitDropped(t, DroppedMemoryBound, 6)
	if out := h.logs.String(); !strings.Contains(out, "oldest queued usage batches dropped to bound memory (no data directory)") {
		t.Errorf("drop not logged:\n%s", out)
	}
	h.cp.SetUsageFault(nil)
	flush(t, c)
	var sent []string
	for _, raw := range h.cp.CountedRecords() {
		var rec accounting.UsageRecord
		if err := json.Unmarshal(raw, &rec); err != nil {
			t.Fatal(err)
		}
		sent = append(sent, rec.RequestID)
	}
	// Batch 1 (req-0, req-1) was outstanding and stays; batches 2 to 4 were dropped
	// to keep within 4 records' worth.
	if want := []string{"req-0", "req-1", "req-8", "req-9"}; !slices.Equal(sent, want) {
		t.Errorf("records counted %v, want the outstanding batch and the newest %v", sent, want)
	}
}

// The bound weighs records by their encoded size, not their number: two records with
// long request IDs take the room of more than four short ones, and the queued bytes
// are reported for the metrics.
func TestWithoutADataDirectoryTheBoundCountsEncodedBytes(t *testing.T) {
	h := newHarness(t)
	h.cp.Publish(configA(t))
	h.cp.SetUsageFault(&fakecontrol.UsageFault{Status: http.StatusServiceUnavailable, Code: "internal-error"})
	small := recordSize(t, testRecord(1))
	c, obs, _ := h.usageClient(2, func(o *Options) {
		withoutDataDir(o)
		o.UsageMemoryBytes = 4 * small
	})
	long := func(n int) accounting.UsageRecord {
		rec := testRecord(n)
		rec.RequestID = fmt.Sprintf("req-%d-%s", n, strings.Repeat("x", 120))
		return rec
	}
	c.Record(testRecord(1))
	c.Record(testRecord(2)) // batch 1: outstanding, never dropped
	c.Record(long(3))
	c.Record(long(4)) // batch 2: 2 records, over the room of 2 short ones
	obs.waitDropped(t, DroppedMemoryBound, 2)
	c.Record(testRecord(5))
	c.Record(testRecord(6)) // batch 3: 4 short records held, at the bound
	obs.waitMemoryBytes(t, 4*small)
	obs.mu.Lock()
	dropped := obs.dropped[DroppedMemoryBound]
	obs.mu.Unlock()
	if dropped != 2 {
		t.Errorf("%d records dropped at the bound, want 2", dropped)
	}
	h.cp.SetUsageFault(nil)
	flush(t, c)
	var sent []string
	for _, raw := range h.cp.CountedRecords() {
		var rec accounting.UsageRecord
		if err := json.Unmarshal(raw, &rec); err != nil {
			t.Fatal(err)
		}
		sent = append(sent, rec.RequestID)
	}
	if want := []string{"req-1", "req-2", "req-5", "req-6"}; !slices.Equal(sent, want) {
		t.Errorf("records counted %v, want %v", sent, want)
	}
	obs.mu.Lock()
	defer obs.mu.Unlock()
	if obs.memoryBytes != 0 {
		t.Errorf("%d bytes held after the flush, want 0", obs.memoryBytes)
	}
}

// waitMemoryBytes waits until the reported bytes held in memory are want.
func (o *testObserver) waitMemoryBytes(t *testing.T, want int64) {
	t.Helper()
	timeout := time.After(testWaitLimit)
	for {
		o.mu.Lock()
		got, changed := o.memoryBytes, o.changed
		o.mu.Unlock()
		if got == want {
			return
		}
		select {
		case <-changed:
		case <-timeout:
			t.Fatalf("%d bytes held in memory, want %d", got, want)
		}
	}
}

func TestWithoutADataDirectoryNoLastKnownGoodIsWrittenOrRead(t *testing.T) {
	h := newHarness(t)
	h.cp.Publish(configA(t))
	if err := h.boot(h.client(withoutDataDir)); err != nil {
		t.Fatal(err)
	}
	h.wantLoad(load{TriggerControl, true})
	if got := h.savedHash(); got != "" {
		t.Errorf("last-known-good %s written with no data directory", got)
	}

	// A copy in the directory is not read without it.
	h.boot(h.client(nil))
	h.wantLoad(load{TriggerControl, true})
	h.cp.SetDown(true)
	h.holder.Swap(nil)
	if err := h.boot(h.client(withoutDataDir)); err == nil {
		t.Fatal("Boot found a config with the control plane down, no seed and no data directory")
	}
	h.noLoadPending()
}
