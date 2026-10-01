package control

// The control client against fakecontrol, a scripted control plane. Waits are on
// events — a config load reported by the apply path, a stream connecting, a request
// arriving — never fixed sleeps. The backoff's timer is replaced: it records each
// delay and waits at most a few milliseconds.

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"kaiak/internal/config"
	"kaiak/internal/fakecontrol"
	"kaiak/internal/state"
)

const (
	testToken    = "cp-secret-token"
	testInstance = "gw-test"
	// testWaitLimit bounds every wait on the client.
	testWaitLimit = 10 * time.Second
	// maxTestDelay caps the real time a scheduled backoff delay takes in tests.
	maxTestDelay = 5 * time.Millisecond
)

// snapshotConfig reads the config inside a config snapshot fixture.
func snapshotConfig(t *testing.T, dir, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(fixturesDir, "config-snapshot", dir, name))
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		Config json.RawMessage `json:"config"`
	}
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatal(err)
	}
	return s.Config
}

// Configs the tests publish: two valid ones that tell apart by their models, one the
// gateway rejects.
func configA(t *testing.T) []byte { return snapshotConfig(t, "valid", "minimal.json") }

func configB(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile("../../../protocol/fixtures/config/valid/users-child-defaults.json")
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func configRejected(t *testing.T) []byte {
	return snapshotConfig(t, "invalid", "config-semantic-invalid.json")
}

// syncBuffer lets the test read logs written by the client's goroutine.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type load struct {
	trigger string
	applied bool
}

type harness struct {
	t      *testing.T
	cp     *fakecontrol.Server
	holder *config.Holder
	dir    *state.Dir
	logs   *syncBuffer
	loads  chan load
	totals chan TotalsUpdate

	mu     sync.Mutex
	delays []time.Duration
	// delayed receives every scheduled delay, when set.
	delayed chan time.Duration
}

// newHarness starts a fake control plane and prepares a data directory; client builds
// clients against them.
func newHarness(t *testing.T) *harness {
	t.Helper()
	cp := fakecontrol.New(testToken)
	t.Cleanup(cp.Close)
	// The client's tests script every totals message they expect: none follows the
	// replay unasked.
	cp.HoldTotalsOnConnect(true)
	logs := &syncBuffer{}
	dir, err := state.Open(t.TempDir(), slog.New(slog.NewTextHandler(logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, cp: cp, dir: dir, logs: logs, holder: &config.Holder{},
		loads: make(chan load, 100), totals: make(chan TotalsUpdate, 10)}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("client log:\n%s", logs.String())
		}
		if strings.Contains(logs.String(), testToken) {
			t.Error("the control token reached the log")
		}
	})
	return h
}

// client returns a client of the harness's control plane; tune adjusts its options.
func (h *harness) client(tune func(*Options)) *Client {
	u, err := url.Parse(h.cp.URL())
	if err != nil {
		h.t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	applier := config.NewApplier(h.holder, logger, func(string) (string, bool) { return "", false },
		func(l config.Load) { h.loads <- load{l.Trigger, l.Applied} })
	// A short boot wait: a boot with the control plane down retries (at test speed)
	// until it ends.
	opts := Options{URL: u, Token: testToken, Instance: testInstance, Applier: applier, Dir: h.dir, Logger: logger,
		OnTotals: func(u TotalsUpdate) { h.totals <- u }, BootWait: 200 * time.Millisecond,
		wait: h.wait, random: func() float64 { return 0.5 }}
	if tune != nil {
		tune(&opts)
	}
	return New(opts)
}

func (h *harness) wait(ctx context.Context, d time.Duration) error {
	h.mu.Lock()
	h.delays = append(h.delays, d)
	delayed := h.delayed
	h.mu.Unlock()
	if delayed != nil {
		select {
		case delayed <- d:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return waitTimer(ctx, min(d, maxTestDelay))
}

// run runs c.Run until the test ends, or until the returned stop is called: stop
// returns once Run has.
func (h *harness) run(c *Client) (stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		c.Run(ctx)
		close(done)
	}()
	stop = func() {
		cancel()
		select {
		case <-done:
		case <-time.After(testWaitLimit):
			h.t.Error("Run did not return after cancel")
		}
	}
	h.t.Cleanup(stop)
	return stop
}

// nextLoad waits for the next config load the apply path reports.
func (h *harness) nextLoad() load {
	h.t.Helper()
	select {
	case l := <-h.loads:
		return l
	case <-time.After(testWaitLimit):
		h.t.Fatal("no config load")
		return load{}
	}
}

func (h *harness) wantLoad(want load) {
	h.t.Helper()
	if got := h.nextLoad(); got != want {
		h.t.Fatalf("load %+v, want %+v", got, want)
	}
}

// noLoadPending fails when a load was reported that the test did not expect.
func (h *harness) noLoadPending() {
	h.t.Helper()
	select {
	case l := <-h.loads:
		h.t.Fatalf("unexpected load %+v", l)
	default:
	}
}

// nextStream waits for the next config stream to connect.
func (h *harness) nextStream() *fakecontrol.Stream {
	h.t.Helper()
	select {
	case st := <-h.cp.Connected():
		return st
	case <-time.After(testWaitLimit):
		h.t.Fatal("no stream connected")
		return nil
	}
}

func wantApplied(t *testing.T, c *Client, want int64) {
	t.Helper()
	if got, ok := c.AppliedVersion(); !ok || got != want {
		t.Fatalf("applied version %d (%v), want %d", got, ok, want)
	}
}

// savedEpoch is the config epoch in the last-known-good file; "" when there is none.
func (h *harness) savedEpoch() string {
	h.t.Helper()
	var saved lastKnownGood
	if _, err := h.dir.ReadVersioned(LastKnownGoodFile, lastKnownGoodFormat, &saved); err != nil {
		h.t.Fatal(err)
	}
	return saved.ConfigEpoch
}

// savedVersion is the version in the last-known-good file; 0 when there is none.
func (h *harness) savedVersion() int64 {
	h.t.Helper()
	var saved lastKnownGood
	found, err := h.dir.ReadVersioned(LastKnownGoodFile, lastKnownGoodFormat, &saved)
	if err != nil {
		h.t.Fatal(err)
	}
	if !found {
		return 0
	}
	return saved.Version
}

func TestBootAppliesTheSnapshotAndSendsTheProtocolHeaders(t *testing.T) {
	h := newHarness(t)
	h.cp.Publish(configA(t))
	c := h.client(nil)

	if err := c.Boot(context.Background()); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	h.wantLoad(load{TriggerControl, true})
	wantApplied(t, c, 1)
	if !h.holder.Loaded() || h.holder.Current().Models["llama"] == nil {
		t.Fatal("snapshot not in the holder")
	}
	if got, want := h.holder.Current().Version, (config.Version{Epoch: h.cp.ConfigEpoch(), Number: 1}); got != want {
		t.Errorf("snapshot version %+v, want %+v (totals apply to it by this identity)", got, want)
	}
	if got := h.savedVersion(); got != 1 {
		t.Errorf("last-known-good version %d, want 1", got)
	}
	reqs := h.cp.Requests()
	if len(reqs) != 1 || reqs[0].Path != "/v1/config" {
		t.Fatalf("requests %+v", reqs)
	}
	for name, want := range map[string]string{"Authorization": "Bearer " + testToken, "Kaiak-Protocol": "3",
		"Kaiak-Instance": testInstance} {
		if got := reqs[0].Header.Values(name); len(got) != 1 || got[0] != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if !strings.Contains(h.logs.String(), `msg="config applied" trigger=control config_version=1`) {
		t.Errorf("applied line missing:\n%s", h.logs.String())
	}
}

func TestNoConfigPublishedThenOneArrives(t *testing.T) {
	h := newHarness(t)
	c := h.client(nil)
	if err := c.Boot(context.Background()); err == nil {
		t.Fatal("Boot reports a config with none published")
	}
	if h.holder.Loaded() {
		t.Fatal("holder loaded")
	}
	if !strings.Contains(h.logs.String(), "config-unavailable") {
		t.Errorf("503 config-unavailable not logged:\n%s", h.logs.String())
	}

	h.mu.Lock()
	h.delayed = make(chan time.Duration, 100)
	h.mu.Unlock()
	h.run(c)
	<-h.delayed // Run failed once more and is waiting to retry
	h.cp.Publish(configA(t))
	h.wantLoad(load{TriggerControl, true})
	wantApplied(t, c, 1)
	if st := h.nextStream(); st.Since != 1 {
		t.Errorf("stream since %d, want 1", st.Since)
	}
}

func TestStreamAppliesUpdatesAndIgnoresHeartbeats(t *testing.T) {
	h := newHarness(t)
	h.cp.Publish(configA(t))
	c := h.client(nil)
	c.Boot(context.Background())
	h.wantLoad(load{TriggerControl, true})
	stop := h.run(c)

	st := h.nextStream()
	if st.Since != 1 {
		t.Fatalf("stream since %d, want 1", st.Since)
	}
	st.Comment("heartbeat")
	h.cp.Publish(configB(t))
	h.wantLoad(load{TriggerControl, true})
	wantApplied(t, c, 2)
	if h.holder.Current().Models["large"] == nil {
		t.Fatal("version 2 not in the holder")
	}
	// The heartbeat neither ended the stream nor reached the apply path.
	select {
	case <-st.Done():
		t.Fatal("stream ended")
	default:
	}
	h.noLoadPending()
	stop() // the last-known-good write follows the apply on the client's goroutine
	if got := h.savedVersion(); got != 2 {
		t.Errorf("last-known-good version %d, want 2", got)
	}
}

func TestReconnectResumesAfterTheAppliedVersion(t *testing.T) {
	h := newHarness(t)
	h.cp.Publish(configA(t))
	c := h.client(nil)
	c.Boot(context.Background())
	h.wantLoad(load{TriggerControl, true})
	h.run(c)

	st := h.nextStream()
	h.cp.Publish(configB(t))
	h.wantLoad(load{TriggerControl, true})
	st.Close()
	next := h.nextStream()
	if next.Since != 2 {
		t.Fatalf("reconnected with since %d, want 2", next.Since)
	}
	h.cp.Publish(configA(t))
	h.wantLoad(load{TriggerControl, true})
	wantApplied(t, c, 3)
	if n := len(h.cp.Gets()); n != 3 { // snapshot, two streams
		t.Errorf("%d GET requests, want 3", n)
	}
}

func TestResyncAppliesALowerVersion(t *testing.T) {
	h := newHarness(t)
	h.cp.Publish(configA(t))
	h.cp.Publish(configA(t))
	h.cp.Publish(configA(t))
	c := h.client(nil)
	c.Boot(context.Background())
	h.wantLoad(load{TriggerControl, true})
	wantApplied(t, c, 3)
	h.run(c)
	h.nextStream()

	// The control plane restarts with an empty store: its first version is 1, and the
	// gateway's since=3 is ahead of it.
	h.cp.Restart()
	h.cp.Publish(configB(t))
	h.wantLoad(load{TriggerControl, true})
	wantApplied(t, c, 1)
	if h.holder.Current().Models["large"] == nil {
		t.Fatal("the resync snapshot is not in the holder")
	}
	if st := h.nextStream(); st.Since != 1 {
		t.Errorf("stream after resync since %d, want 1", st.Since)
	}
	if !strings.Contains(h.logs.String(), "config stream ended: resync") {
		t.Errorf("resync not logged:\n%s", h.logs.String())
	}
}

func TestStreamNeverGoesBackAVersion(t *testing.T) {
	h := newHarness(t)
	h.cp.Publish(configA(t))
	h.cp.Publish(configB(t))
	c := h.client(nil)
	c.Boot(context.Background())
	h.wantLoad(load{TriggerControl, true})
	h.run(c)
	st := h.nextStream()

	running := h.holder.Current()
	st.SendConfig(1, configA(t))
	st.SendConfig(2, configA(t))
	st.SendConfig(3, configA(t))
	h.wantLoad(load{TriggerControl, true}) // only version 3
	wantApplied(t, c, 3)
	if h.holder.Current() == running || h.holder.Current().Models["llama"] == nil {
		t.Fatal("version 3 not applied")
	}
	h.noLoadPending()
	if !strings.Contains(h.logs.String(), "config event ignored: version already taken") {
		t.Errorf("ignored versions not logged:\n%s", h.logs.String())
	}
}

func TestRejectedConfigIsKeptOutAndReported(t *testing.T) {
	h := newHarness(t)
	h.cp.Publish(configA(t))
	c := h.client(nil)
	c.Boot(context.Background())
	h.wantLoad(load{TriggerControl, true})
	h.run(c)
	st := h.nextStream()
	running := h.holder.Current()

	h.cp.Publish(configRejected(t))
	h.wantLoad(load{TriggerControl, false})
	if h.holder.Current() != running {
		t.Fatal("a rejected config replaced the running one")
	}
	wantApplied(t, c, 1)
	r, ok := c.LastRejection()
	if !ok || r.Version != 2 || !slices.Equal(r.Codes, []string{config.CodeKeyGroupUnknown}) {
		t.Fatalf("last rejection %+v (%v), want version 2 [key-group-unknown]", r, ok)
	}
	if got := h.savedVersion(); got != 1 {
		t.Errorf("last-known-good version %d, want 1: a rejected config is never saved", got)
	}
	if out := h.logs.String(); !strings.Contains(out, `msg="config rejected" trigger=control config_version=2`) ||
		!strings.Contains(out, "running_config=kept") {
		t.Errorf("rejection not logged:\n%s", out)
	}

	// A reconnect resumes after the rejected version: it is not replayed.
	st.Close()
	if next := h.nextStream(); next.Since != 2 {
		t.Fatalf("reconnected with since %d, want 2", next.Since)
	}
	h.noLoadPending()

	// A newer applied version ends the report.
	h.cp.Publish(configB(t))
	h.wantLoad(load{TriggerControl, true})
	if r, ok := c.LastRejection(); ok {
		t.Errorf("rejection %+v still reported after version 3 applied", r)
	}
}

// After a control-plane restart its versions count from 1 again: a rejected config
// with a lower version than the one in force is still the latest config received,
// so it is reported; the next config applied ends the report.
func TestRejectionBelowTheAppliedVersionIsReported(t *testing.T) {
	h := newHarness(t)
	h.cp.Publish(configA(t))
	h.cp.Publish(configA(t))
	h.cp.Publish(configA(t))
	c := h.client(nil)
	c.Boot(context.Background())
	h.wantLoad(load{TriggerControl, true})
	h.run(c)
	h.nextStream()

	h.cp.Restart()
	h.cp.Publish(configRejected(t))
	h.wantLoad(load{TriggerControl, false}) // the resync snapshot, version 1
	wantApplied(t, c, 3)
	if r, ok := c.LastRejection(); !ok || r.Version != 1 {
		t.Fatalf("last rejection %+v (%v), want version 1 while version 3 is applied", r, ok)
	}

	h.nextStream()
	h.cp.Publish(configB(t))
	h.wantLoad(load{TriggerControl, true})
	wantApplied(t, c, 2)
	if r, ok := c.LastRejection(); ok {
		t.Errorf("rejection %+v still reported after version 2 applied", r)
	}
}

func TestRejectedSnapshotAtBootFallsBackToLastKnownGood(t *testing.T) {
	h := newHarness(t)
	h.cp.Publish(configA(t))
	h.client(nil).Boot(context.Background())
	h.wantLoad(load{TriggerControl, true})

	h.cp.Publish(configRejected(t))
	h.holder.Swap(nil) // a new process
	c := h.client(nil)
	if err := c.Boot(context.Background()); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	h.wantLoad(load{TriggerControl, false})
	h.wantLoad(load{TriggerLastKnownGood, true})
	wantApplied(t, c, 1)
	if r, ok := c.LastRejection(); !ok || r.Version != 2 {
		t.Errorf("last rejection %+v (%v), want version 2", r, ok)
	}
	h.run(c)
	if st := h.nextStream(); st.Since != 2 {
		t.Errorf("stream since %d, want 2 (after the rejected snapshot)", st.Since)
	}
}

func TestBootFromLastKnownGoodWhenTheControlPlaneIsDown(t *testing.T) {
	h := newHarness(t)
	h.cp.Publish(configA(t))
	h.cp.Publish(configB(t))
	h.client(nil).Boot(context.Background())
	h.wantLoad(load{TriggerControl, true})

	h.cp.SetDown(true)
	h.holder.Swap(nil) // a new process on the same data directory
	c := h.client(nil)
	if err := c.Boot(context.Background()); err != nil {
		t.Fatalf("Boot did not fall back to the last-known-good config: %v", err)
	}
	h.wantLoad(load{TriggerLastKnownGood, true})
	wantApplied(t, c, 2)
	if h.holder.Current().Models["large"] == nil {
		t.Fatal("last-known-good config not in the holder")
	}
	if got, want := h.holder.Current().Version, (config.Version{Epoch: h.cp.ConfigEpoch(), Number: 2}); got != want {
		t.Errorf("last-known-good snapshot version %+v, want %+v", got, want)
	}
	if !strings.Contains(h.logs.String(), `msg="config applied" trigger=last-known-good config_version=2`) {
		t.Errorf("last-known-good load not logged:\n%s", h.logs.String())
	}

	// Once the control plane is back, the stream resumes after the saved version.
	h.run(c)
	h.cp.SetDown(false)
	if st := h.nextStream(); st.Since != 2 {
		t.Fatalf("stream since %d, want 2", st.Since)
	}
	h.noLoadPending()
}

// The audit's scenario (M10): the last-known-good copy is version 2 of a store that
// is gone; the control plane now runs a new store that has also reached version 2,
// with another config. The stream resumes from version 2 of the old epoch, so the
// control plane answers resync and the gateway takes the new version 2.
func TestLastKnownGoodFromAnotherEpochIsReplacedAtTheSameVersion(t *testing.T) {
	h := newHarness(t)
	h.cp.Publish(configA(t))
	h.cp.Publish(configA(t))
	h.client(nil).Boot(context.Background())
	h.wantLoad(load{TriggerControl, true})
	oldEpoch := h.cp.ConfigEpoch()
	if got := h.savedEpoch(); got != oldEpoch {
		t.Fatalf("last-known-good epoch %q, want the snapshot's %q", got, oldEpoch)
	}

	// The control plane starts over with a new store, which reaches version 2 again.
	h.cp.Restart()
	h.cp.Publish(configB(t))
	h.cp.Publish(configB(t))
	h.cp.SetDown(true)
	h.holder.Swap(nil) // a new process on the same data directory
	c := h.client(nil)
	if err := c.Boot(context.Background()); err != nil {
		t.Fatalf("Boot did not fall back to the last-known-good config: %v", err)
	}
	h.wantLoad(load{TriggerLastKnownGood, true})
	wantApplied(t, c, 2)
	if h.holder.Current().Models["llama"] == nil {
		t.Fatal("the last-known-good config is not in the holder")
	}

	stop := h.run(c)
	h.cp.SetDown(false)
	h.wantLoad(load{TriggerControl, true}) // the resync snapshot: the new store's version 2
	resumed := url.Values{"since": {"2"}, "config_epoch": {oldEpoch}}.Encode()
	if !slices.ContainsFunc(h.cp.Gets(), func(r fakecontrol.Request) bool {
		return r.Path == "/v1/stream" && r.Query == resumed
	}) {
		t.Fatalf("no stream resumed from version 2 of the old epoch: %+v", h.cp.Gets())
	}
	wantApplied(t, c, 2)
	if h.holder.Current().Models["large"] == nil {
		t.Fatal("the new store's version 2 is not in the holder")
	}
	if next := h.nextStream(); next.Since != 2 || next.SinceEpoch != h.cp.ConfigEpoch() {
		t.Errorf("stream after resync since %d in epoch %q, want 2 in %q", next.Since, next.SinceEpoch,
			h.cp.ConfigEpoch())
	}
	stop() // the last-known-good write follows the apply on the client's goroutine
	if got := h.savedEpoch(); got != h.cp.ConfigEpoch() {
		t.Errorf("last-known-good epoch %q, want the new store's %q", got, h.cp.ConfigEpoch())
	}
}

// A last-known-good file whose epoch is not an epoch (N-P10) is discarded at boot, with
// a log line: resuming from it would be refused (400 since-invalid) on every reconnect.
func TestMalformedLastKnownGoodIsDiscarded(t *testing.T) {
	for name, saved := range map[string]lastKnownGood{
		"epoch":   {ConfigEpoch: "NOT-AN-EPOCH", Version: 2},
		"version": {ConfigEpoch: "0123456789abcdef0123456789abcdef", Version: 0},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			saved.Config = configA(t)
			if err := h.dir.WriteVersioned(LastKnownGoodFile, lastKnownGoodFormat, saved); err != nil {
				t.Fatal(err)
			}
			h.cp.SetDown(true)
			c := h.client(nil)
			if err := c.Boot(context.Background()); err == nil {
				t.Fatal("Boot applied a last-known-good copy with a malformed position")
			}
			if h.holder.Loaded() {
				t.Fatal("holder loaded")
			}
			if !strings.Contains(h.logs.String(), `msg="last-known-good config discarded: malformed position"`) {
				t.Errorf("discard not logged:\n%s", h.logs.String())
			}
		})
	}
}

// A stream the control plane refuses to open with 400 since-invalid (N-P10) is not
// retried from the same position forever: the client fetches the snapshot, whose
// position the next stream resumes from.
func TestSinceInvalidFetchesTheSnapshot(t *testing.T) {
	h := newHarness(t)
	h.cp.Publish(configA(t))
	c := h.client(nil)
	if err := c.Boot(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.wantLoad(load{TriggerControl, true})
	c.mu.Lock()
	c.position.epoch = "NOT-AN-EPOCH" // a position the control plane refuses
	c.mu.Unlock()
	h.run(c)
	h.wantLoad(load{TriggerControl, true}) // the snapshot again
	if st := h.nextStream(); st.Since != 1 || st.SinceEpoch != h.cp.ConfigEpoch() {
		t.Errorf("stream since %d in epoch %q, want 1 in %q", st.Since, st.SinceEpoch, h.cp.ConfigEpoch())
	}
}

// A config event of another epoch is another store's count: applied whatever its
// version, lower included.
func TestStreamConfigFromAnotherEpochIsAppliedWhateverItsVersion(t *testing.T) {
	h := newHarness(t)
	h.cp.Publish(configA(t))
	h.cp.Publish(configA(t))
	h.cp.Publish(configA(t))
	c := h.client(nil)
	c.Boot(context.Background())
	h.wantLoad(load{TriggerControl, true})
	h.run(c)
	st := h.nextStream()
	if st.SinceEpoch != h.cp.ConfigEpoch() {
		t.Fatalf("stream epoch %q, want %q", st.SinceEpoch, h.cp.ConfigEpoch())
	}

	other := "0123456789abcdef0123456789abcdef"
	st.Send("config", "1", fakecontrol.Snapshot(other, 1, configB(t)))
	h.wantLoad(load{TriggerControl, true})
	wantApplied(t, c, 1)
	if h.holder.Current().Models["large"] == nil {
		t.Fatal("the other epoch's version 1 is not in the holder")
	}
	// Within the new epoch the usual rule holds again.
	st.Send("config", "1", fakecontrol.Snapshot(other, 1, configA(t)))
	st.SendConfig(4, configA(t)) // the old epoch, a higher number: another store's count too
	h.wantLoad(load{TriggerControl, true})
	wantApplied(t, c, 4)
	h.noLoadPending()
	if !strings.Contains(h.logs.String(), "config event from another config epoch") {
		t.Errorf("epoch change not logged:\n%s", h.logs.String())
	}
}

// The process exits on a failed boot (TestBootFailsWithNoSourceOfConfig); the
// client itself still follows the control plane from nothing, as it does after a
// seed boot.
func TestClientFollowsTheControlPlaneFromNoConfig(t *testing.T) {
	h := newHarness(t)
	h.cp.SetDown(true)
	c := h.client(nil)
	if err := c.Boot(context.Background()); err == nil {
		t.Fatal("Boot reports a config with no source")
	}
	if h.holder.Loaded() {
		t.Fatal("holder loaded")
	}

	h.mu.Lock()
	h.delayed = make(chan time.Duration, 100)
	h.mu.Unlock()
	h.run(c)
	<-h.delayed
	<-h.delayed // two failed retries while down
	h.cp.Publish(configA(t))
	h.cp.SetDown(false)
	h.wantLoad(load{TriggerControl, true})
	if !h.holder.Loaded() {
		t.Fatal("holder not loaded after the control plane came up")
	}
}

func TestBootWaitBoundsTheSnapshotFetch(t *testing.T) {
	hung := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-hung:
		}
	}))
	defer srv.Close()
	defer close(hung)
	h := newHarness(t)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	c := h.client(func(o *Options) { o.URL = u; o.BootWait = 50 * time.Millisecond })
	done := make(chan error)
	go func() { done <- c.Boot(context.Background()) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Boot reports a config")
		}
	case <-time.After(testWaitLimit):
		t.Fatal("Boot did not return after its wait")
	}
}

// D7: a control plane that comes up within the boot wait — restarting beside the
// gateway, or not yet published — gives the gateway its config: boot retries the
// snapshot with jittered backoff (250 ms doubling, capped at 2 s) instead of giving
// up after one attempt.
func TestBootRetriesUntilTheControlPlaneComesUp(t *testing.T) {
	for name, c := range map[string]struct{ down, up func(h *harness) }{
		"unreachable": {
			down: func(h *harness) { h.cp.Publish(configA(t)); h.cp.SetDown(true) },
			up:   func(h *harness) { h.cp.SetDown(false) },
		},
		"503 config-unavailable": {
			down: func(*harness) {},
			up:   func(h *harness) { h.cp.Publish(configA(t)) },
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			c.down(h)
			delayed := make(chan time.Duration, 100)
			h.mu.Lock()
			h.delayed = delayed
			h.mu.Unlock()
			client := h.client(func(o *Options) { o.BootWait = testWaitLimit })
			done := make(chan error, 1)
			go func() { done <- client.Boot(context.Background()) }()
			var delays []time.Duration
			for range 2 {
				select {
				case d := <-delayed:
					delays = append(delays, d)
				case err := <-done:
					t.Fatalf("Boot returned %v without retrying", err)
				case <-time.After(testWaitLimit):
					t.Fatal("no boot retry scheduled")
				}
			}
			// random is 0.5: half of 250 ms, then half of 500 ms.
			if want := []time.Duration{125 * time.Millisecond, 250 * time.Millisecond}; !slices.Equal(delays, want) {
				t.Errorf("boot retry delays %v, want %v", delays, want)
			}
			c.up(h)
			go func() {
				for range delayed { // later retries until the control plane answers
				}
			}()
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("Boot: %v", err)
				}
			case <-time.After(testWaitLimit):
				t.Fatal("Boot did not return once the control plane came up")
			}
			h.wantLoad(load{TriggerControl, true})
			wantApplied(t, client, 1)
			h.mu.Lock()
			h.delayed = nil
			h.mu.Unlock()
			close(delayed)
		})
	}
}

// D7: what the operator must fix is never retried — a refused token, a snapshot the
// gateway rejects: boot goes on at once (last-known-good, else exit).
func TestBootDoesNotRetryWhatTheOperatorMustFix(t *testing.T) {
	for name, c := range map[string]struct {
		setup func(h *harness)
		token string
		want  string
	}{
		"refused token":     {func(h *harness) { h.cp.Publish(configA(t)) }, "wrong", "401"},
		"rejected snapshot": {func(h *harness) { h.cp.Publish(configRejected(t)) }, testToken, "was rejected"},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			c.setup(h)
			client := h.client(func(o *Options) { o.Token = c.token; o.BootWait = testWaitLimit })
			started := time.Now()
			err := client.Boot(context.Background())
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("Boot error %v, want one naming %q", err, c.want)
			}
			h.mu.Lock()
			delays := len(h.delays)
			h.mu.Unlock()
			if delays != 0 || time.Since(started) > testWaitLimit/2 {
				t.Errorf("%d retries over %s, want an immediate exit", delays, time.Since(started))
			}
		})
	}
}

func TestBackoffGrowsCapsAndResets(t *testing.T) {
	b := backoff{base: 500 * time.Millisecond, cap: 30 * time.Second, random: func() float64 { return 0.999999 }}
	var got []time.Duration
	for range 9 {
		got = append(got, b.next().Round(time.Millisecond))
	}
	want := []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second,
		16 * time.Second, 30 * time.Second, 30 * time.Second, 30 * time.Second}
	if !slices.Equal(got, want) {
		t.Errorf("delays %v, want %v", got, want)
	}
	for range 100 {
		b.next() // no overflow however long the outage
	}
	if d := b.next(); d <= 0 || d > 30*time.Second {
		t.Errorf("delay after many attempts %v", d)
	}
	b.reset()
	if d := b.next().Round(time.Millisecond); d != 500*time.Millisecond {
		t.Errorf("delay after reset %v, want 500ms", d)
	}
	b.random = func() float64 { return 0 }
	if d := b.next(); d != 0 {
		t.Errorf("full jitter allows 0, got %v", d)
	}
}

func TestReconnectDelaysGrowWhileDownAndResetAfterAHealthyStream(t *testing.T) {
	h := newHarness(t)
	h.cp.Publish(configA(t))
	c := h.client(func(o *Options) {
		o.BackoffBase = 100 * time.Millisecond
		o.BackoffCap = 400 * time.Millisecond
		o.HealthyAfter = time.Hour
	})
	c.Boot(context.Background())
	h.wantLoad(load{TriggerControl, true})
	h.cp.SetDown(true)
	h.mu.Lock()
	h.delayed = make(chan time.Duration, 100)
	h.mu.Unlock()
	h.run(c)
	var got []time.Duration
	for range 5 {
		got = append(got, <-h.delayed)
	}
	// random is 0.5: half of 100, 200, 400 (cap), 400, 400 ms.
	want := []time.Duration{50 * time.Millisecond, 100 * time.Millisecond, 200 * time.Millisecond,
		200 * time.Millisecond, 200 * time.Millisecond}
	if !slices.Equal(got, want) {
		t.Fatalf("delays %v, want %v", got, want)
	}

	// With HealthyAfter 0 every stream that opened counts as healthy.
	h2 := newHarness(t)
	h2.cp.Publish(configA(t))
	c2 := h2.client(func(o *Options) { o.BackoffBase = 100 * time.Millisecond; o.HealthyAfter = time.Nanosecond })
	c2.Boot(context.Background())
	h2.wantLoad(load{TriggerControl, true})
	h2.mu.Lock()
	h2.delayed = make(chan time.Duration, 100)
	h2.mu.Unlock()
	h2.run(c2)
	for range 3 {
		st := h2.nextStream()
		st.Close()
		if d := <-h2.delayed; d != 50*time.Millisecond {
			t.Fatalf("delay after a healthy stream %v, want 50ms (reset)", d)
		}
	}
}

func TestSilentStreamIsReconnected(t *testing.T) {
	h := newHarness(t)
	h.cp.Publish(configA(t))
	c := h.client(func(o *Options) { o.IdleTimeout = 50 * time.Millisecond })
	c.Boot(context.Background())
	h.wantLoad(load{TriggerControl, true})
	h.run(c)
	first := h.nextStream()
	// Nothing is sent: the client gives up on the stream and connects again.
	h.nextStream()
	select {
	case <-first.Done():
	case <-time.After(testWaitLimit):
		t.Fatal("the silent stream was not closed")
	}
	if !strings.Contains(h.logs.String(), "config stream silent for 50ms") {
		t.Errorf("idle end not logged:\n%s", h.logs.String())
	}
}

// A stream endpoint that accepts the request and never answers (a stuck proxy) is
// given up on after the idle timeout, so the client reconnects instead of waiting
// forever.
func TestStreamOpenIsBounded(t *testing.T) {
	hung := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-hung:
		}
	}))
	defer srv.Close()
	defer close(hung)
	h := newHarness(t)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	c := h.client(func(o *Options) { o.URL = u; o.IdleTimeout = 50 * time.Millisecond })
	done := make(chan streamResult, 1)
	go func() { done <- c.followStream(context.Background()) }()
	select {
	case res := <-done:
		if res.err == nil || !strings.Contains(res.err.Error(), "config stream not open within 50ms") || res.lasted != 0 {
			t.Errorf("result %+v, want the opening bound's error", res)
		}
	case <-time.After(testWaitLimit):
		t.Fatal("opening the stream is still blocked")
	}
}

func TestControlPlaneAnswersHaveAHeaderTimeout(t *testing.T) {
	c := defaultHTTPClient()
	transport, ok := c.Transport.(*http.Transport)
	if !ok || transport.ResponseHeaderTimeout != responseHeaderTimeout || c.Timeout != 0 {
		t.Errorf("default client %+v", c)
	}
}

func TestRedirectsAreNotFollowedWithTheToken(t *testing.T) {
	var reached atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()
	u, err := url.Parse(redirector.URL + "/v1")
	if err != nil {
		t.Fatal(err)
	}

	h := newHarness(t)
	c := h.client(func(o *Options) { o.URL = u })
	if err := c.Boot(context.Background()); err == nil {
		t.Fatal("Boot applied a config through a redirect")
	}
	if n := reached.Load(); n != 0 {
		t.Errorf("the redirect target was reached %d times", n)
	}
	if out := h.logs.String(); !strings.Contains(out, "control plane answered 307, a redirect") {
		t.Errorf("the redirect is not named in the log:\n%s", out)
	}
}

func TestProtocolMismatchIsLoggedAsAnErrorAndRetried(t *testing.T) {
	h := newHarness(t)
	h.cp.Publish(configA(t))
	h.cp.SetProtocol("2")
	c := h.client(nil)
	if err := c.Boot(context.Background()); err == nil {
		t.Fatal("Boot applied a config from a control plane speaking another version")
	}
	out := h.logs.String()
	if !strings.Contains(out, "level=ERROR") || !strings.Contains(out, "protocol version mismatch") ||
		!strings.Contains(out, "gateway speaks 3") {
		t.Errorf("mismatch not logged as an error:\n%s", out)
	}

	h.mu.Lock()
	h.delayed = make(chan time.Duration, 100)
	h.mu.Unlock()
	h.run(c)
	<-h.delayed // retried and failed again, still waiting
	h.cp.SetProtocol("3")
	h.wantLoad(load{TriggerControl, true})
	if n := len(h.cp.Gets()); n < 3 {
		t.Errorf("%d requests, want the boot fetch and at least two retries", n)
	}
}

func TestTotalsEventsReachTheConsumer(t *testing.T) {
	h := newHarness(t)
	h.cp.Publish(configA(t))
	c := h.client(nil)
	c.Boot(context.Background())
	h.wantLoad(load{TriggerControl, true})
	h.run(c)
	st := h.nextStream()

	data, err := os.ReadFile(filepath.Join(fixturesDir, "totals", "valid", "windows.json"))
	if err != nil {
		t.Fatal(err)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, data); err != nil {
		t.Fatal(err)
	}
	st.Send("totals", "", []byte(`{"broken":`)) // malformed: logged, skipped
	h.cp.PushTotals(compact.Bytes())
	want, err := DecodeTotals(data)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-h.totals:
		if got.Totals == nil || got.Totals.ConfigVersion != want.ConfigVersion ||
			len(got.Totals.Windows) != len(want.Windows) || len(got.Totals.Windows) == 0 || got.Counted != 0 {
			t.Errorf("totals %+v, want %+v and nothing counted", got, want)
		}
	case <-time.After(testWaitLimit):
		t.Fatal("no totals delivered")
	}
	if !strings.Contains(h.logs.String(), "totals event ignored: malformed") {
		t.Errorf("malformed totals not logged:\n%s", h.logs.String())
	}
}
