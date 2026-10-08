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
	"kaiak/internal/fixturetest"
	"kaiak/internal/limits"
)

const (
	testToken    = "cp-secret-token"
	testInstance = "gw-test"
	// testWaitLimit bounds every wait on the client.
	testWaitLimit = 10 * time.Second
	// maxTestDelay caps the real time a scheduled backoff delay takes in tests.
	maxTestDelay = 5 * time.Millisecond
)

// fixtureConfig reads the config inside a config event fixture.
func fixtureConfig(t *testing.T, dir, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(fixturesDir, "config-event", dir, name))
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
func configA(t *testing.T) []byte { return fixtureConfig(t, "valid", "minimal.json") }

func configB(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(fixturetest.Dir("config", "valid", "users-child-defaults.json"))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func configRejected(t *testing.T) []byte {
	return fixtureConfig(t, "invalid", "config-semantic-invalid.json")
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
	logs   *syncBuffer
	loads  chan load
	totals chan totalsUpdate

	mu     sync.Mutex
	delays []time.Duration
	// delayed receives every scheduled delay, when set.
	delayed chan time.Duration
	// bootStreams is how many streams were opened when the last boot returned: a boot
	// closes its stream once it has the config, so nextStream skips them.
	bootStreams int
}

// newHarness starts a fake control plane; client builds clients against it.
func newHarness(t *testing.T) *harness {
	t.Helper()
	cp := fakecontrol.New(testToken)
	t.Cleanup(cp.Close)
	// The client's tests script every totals message they expect: none follows the
	// first config unasked.
	cp.HoldTotalsOnConnect(true)
	logs := &syncBuffer{}
	h := &harness{t: t, cp: cp, logs: logs, holder: &config.Holder{},
		loads: make(chan load, 100), totals: make(chan totalsUpdate, 10)}
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
		func(l config.Load) { h.loads <- load{l.Trigger, l.Applied()} })
	// A short boot wait: a boot with the control plane down retries (at test speed)
	// until it ends.
	opts := Options{URL: u, Token: testToken, Instance: testInstance, Applier: applier, Logger: logger,
		OnTotals: func(totals limits.Totals, counted uint64) { h.totals <- totalsUpdate{totals, counted} }, BootWait: 200 * time.Millisecond,
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

// boot runs c.Boot, then marks the streams opened so far as the boot's: nextStream
// skips them.
func (h *harness) boot(c *Client) error {
	err := c.Boot(context.Background())
	h.mu.Lock()
	h.bootStreams = h.cp.Opened()
	h.mu.Unlock()
	return err
}

// nextStream waits for the next config stream to connect after the last boot.
func (h *harness) nextStream() *fakecontrol.Stream {
	h.t.Helper()
	for {
		select {
		case st := <-h.cp.Connected():
			h.mu.Lock()
			boot := st.Seq <= h.bootStreams
			h.mu.Unlock()
			if !boot {
				return st
			}
		case <-time.After(testWaitLimit):
			h.t.Fatal("no stream connected")
			return nil
		}
	}
}

func wantApplied(t *testing.T, c *Client, want string) {
	t.Helper()
	if got, ok := c.AppliedConfigHash(); !ok || got != want {
		t.Fatalf("applied config %s (%v), want %s", got, ok, want)
	}
}

func TestBootAppliesTheStreamsFirstConfigAndSendsTheProtocolHeaders(t *testing.T) {
	h := newHarness(t)
	hash := h.cp.Publish(configA(t))
	c := h.client(nil)

	if err := h.boot(c); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	h.wantLoad(load{TriggerControl, true})
	wantApplied(t, c, hash)
	if !h.holder.Loaded() || h.holder.Current().Models["llama"] == nil {
		t.Fatal("config not in the holder")
	}
	reqs := h.cp.Requests()
	if len(reqs) != 1 || reqs[0].Path != "/v1/stream" || reqs[0].Query != "" {
		t.Fatalf("requests %+v, want one stream with no parameters", reqs)
	}
	for name, want := range map[string]string{"Authorization": "Bearer " + testToken, "Kaiak-Protocol": "5",
		"Kaiak-Instance": testInstance} {
		if got := reqs[0].Header.Values(name); len(got) != 1 || got[0] != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if !strings.Contains(h.logs.String(), `msg="config applied" kaiak.trigger=control kaiak.config.hash=`+hash) {
		t.Errorf("applied line missing:\n%s", h.logs.String())
	}
}

func TestNoConfigPublishedThenOneArrives(t *testing.T) {
	h := newHarness(t)
	c := h.client(nil)
	if err := h.boot(c); err == nil {
		t.Fatal("Boot reports a config with none published")
	}
	if h.holder.Loaded() {
		t.Fatal("holder loaded")
	}
	if !strings.Contains(h.logs.String(), "nothing published yet") {
		t.Errorf("no config within the boot wait not logged:\n%s", h.logs.String())
	}

	h.run(c)
	h.nextStream() // Run's: open, waiting for a config
	hash := h.cp.Publish(configA(t))
	h.wantLoad(load{TriggerControl, true})
	wantApplied(t, c, hash)
}

func TestStreamAppliesUpdatesAndIgnoresHeartbeats(t *testing.T) {
	h := newHarness(t)
	h.cp.Publish(configA(t))
	c := h.client(nil)
	h.boot(c)
	h.wantLoad(load{TriggerControl, true})
	h.run(c)

	st := h.nextStream() // its first config is the one booted: skipped
	st.Comment("heartbeat")
	hash := h.cp.Publish(configB(t))
	h.wantLoad(load{TriggerControl, true})
	wantApplied(t, c, hash)
	if h.holder.Current().Models["large"] == nil {
		t.Fatal("config B not in the holder")
	}
	// The heartbeat neither ended the stream nor reached the apply path.
	select {
	case <-st.Done():
		t.Fatal("stream ended")
	default:
	}
	h.noLoadPending()
	if !strings.Contains(h.logs.String(), "config event skipped: the config already applied or rejected") {
		t.Errorf("the stream's first config, the one booted, not skipped:\n%s", h.logs.String())
	}
}

// A reconnect takes the control plane's current config: the one already running is
// skipped, a newer one applied.
func TestReconnectTakesTheCurrentConfig(t *testing.T) {
	h := newHarness(t)
	h.cp.Publish(configA(t))
	c := h.client(nil)
	h.boot(c)
	h.wantLoad(load{TriggerControl, true})
	h.run(c)

	st := h.nextStream()
	h.cp.Publish(configB(t))
	h.wantLoad(load{TriggerControl, true})
	st.Close()
	h.nextStream()
	h.noLoadPending() // B again: running
	hash := h.cp.Publish(configA(t))
	h.wantLoad(load{TriggerControl, true})
	wantApplied(t, c, hash)
	for _, r := range h.cp.Gets() {
		if r.Path != "/v1/stream" || r.Query != "" {
			t.Errorf("request %s?%s, want only streams with no parameters", r.Path, r.Query)
		}
	}
}

// A control plane restored to an older config (a backup, an in-memory store started
// over) sends it, and the gateway applies it: the control plane is the authority on
// which config is current.
func TestRestoredOlderConfigIsApplied(t *testing.T) {
	h := newHarness(t)
	older := h.cp.Publish(configA(t))
	h.cp.Publish(configB(t))
	c := h.client(nil)
	h.boot(c)
	h.wantLoad(load{TriggerControl, true})
	h.run(c)
	h.nextStream()

	h.cp.Restart()
	h.cp.Publish(configA(t))
	h.wantLoad(load{TriggerControl, true})
	wantApplied(t, c, older)
	if h.holder.Current().Models["llama"] == nil {
		t.Fatal("the restored config is not in the holder")
	}
}

// Every config event is applied whatever it replaces — a config the gateway ran
// before included — except one identical to the running config.
func TestEveryConfigEventIsApplied(t *testing.T) {
	h := newHarness(t)
	a := h.cp.Publish(configA(t))
	c := h.client(nil)
	h.boot(c)
	h.wantLoad(load{TriggerControl, true})
	h.run(c)
	st := h.nextStream()

	st.SendConfig(configB(t))
	h.wantLoad(load{TriggerControl, true})
	st.SendConfig(configA(t))
	h.wantLoad(load{TriggerControl, true})
	wantApplied(t, c, a)
	st.SendConfig(configA(t))
	st.Comment("after")
	h.cp.Publish(configB(t)) // a load to wait on: nothing came between
	h.wantLoad(load{TriggerControl, true})
	h.noLoadPending()
}

func TestRejectedConfigIsKeptOutAndReported(t *testing.T) {
	h := newHarness(t)
	applied := h.cp.Publish(configA(t))
	c := h.client(nil)
	h.boot(c)
	h.wantLoad(load{TriggerControl, true})
	h.run(c)
	st := h.nextStream()
	running := h.holder.Current()

	rejected := h.cp.Publish(configRejected(t))
	h.wantLoad(load{TriggerControl, false})
	if h.holder.Current() != running {
		t.Fatal("a rejected config replaced the running one")
	}
	wantApplied(t, c, applied)
	r, ok := c.LastRejection()
	if !ok || r.ConfigHash != rejected || !slices.Equal(r.Codes, []string{config.CodeKeyGroupUnknown}) {
		t.Fatalf("last rejection %+v (%v), want %s [key-group-unknown]", r, ok, rejected)
	}
	if out := h.logs.String(); !strings.Contains(out, `msg="config rejected" kaiak.trigger=control kaiak.config.hash=`+rejected) ||
		!strings.Contains(out, "kaiak.config.running=kept") {
		t.Errorf("rejection not logged:\n%s", out)
	}

	// A reconnect gets the rejected config again: skipped by its hash, not checked
	// again — the next load is config B's. A config applied ends the report.
	st.Close()
	h.nextStream()
	h.cp.Publish(configB(t))
	h.wantLoad(load{TriggerControl, true})
	if r, ok := c.LastRejection(); ok {
		t.Errorf("rejection %+v still reported after config B applied", r)
	}
}

// The running config published again — the usual undo of a bad publish — is the
// latest config received, and it runs: the rejection is cleared and a status report
// says so, with nothing reloaded (AUDIT-2 2M10).
func TestRunningConfigReceivedAgainClearsTheRejection(t *testing.T) {
	h := newHarness(t)
	applied := h.cp.Publish(configA(t))
	c := h.client(nil)
	h.boot(c)
	h.wantLoad(load{TriggerControl, true})
	h.run(c)
	h.nextStream()
	h.cp.Publish(configRejected(t))
	h.wantLoad(load{TriggerControl, false})
	h.statusUntil("the rejection reported", func(s Status) bool { return s.LastRejection != nil })

	h.cp.Publish(configA(t))
	h.statusUntil("the rejection cleared", func(s Status) bool {
		return s.LastRejection == nil && s.AppliedConfigHash != nil && *s.AppliedConfigHash == applied
	})
	if r, ok := c.LastRejection(); ok {
		t.Errorf("rejection %+v still reported with the running config received again", r)
	}
	h.noLoadPending()
}

// The process exits on a failed boot (TestBootFailsWithNoSourceOfConfig); the
// client itself still follows the control plane from nothing, as it does after a
// seed boot.
func TestClientFollowsTheControlPlaneFromNoConfig(t *testing.T) {
	h := newHarness(t)
	h.cp.SetDown(true)
	c := h.client(nil)
	if err := h.boot(c); err == nil {
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

// A stream endpoint that never answers does not hold the boot past its wait.
func TestBootWaitBoundsTheWaitForAConfig(t *testing.T) {
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
	go func() { done <- h.boot(c) }()
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
// gateway — gives the gateway its config: boot opens the stream again with jittered
// backoff (250 ms doubling, capped at 2 s) instead of giving up after one attempt.
func TestBootRetriesUntilTheControlPlaneComesUp(t *testing.T) {
	for name, c := range map[string]struct{ down, up func(h *harness) }{
		"unreachable": {
			down: func(h *harness) { h.cp.Publish(configA(t)); h.cp.SetDown(true) },
			up:   func(h *harness) { h.cp.SetDown(false) },
		},
		"500": {
			down: func(h *harness) { h.cp.Publish(configA(t)); h.cp.SetProtocol("") },
			up:   func(h *harness) { h.cp.SetProtocol("5") },
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
			go func() { done <- h.boot(client) }()
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
			if _, ok := client.AppliedConfigHash(); !ok {
				t.Fatal("no config applied")
			}
			h.mu.Lock()
			h.delayed = nil
			h.mu.Unlock()
			close(delayed)
		})
	}
}

// A control plane with nothing published keeps the boot's stream open; the config
// published within the boot wait arrives on it, with no retry.
func TestBootTakesAConfigPublishedWithinTheWait(t *testing.T) {
	h := newHarness(t)
	client := h.client(func(o *Options) { o.BootWait = testWaitLimit })
	done := make(chan error, 1)
	go func() { done <- h.boot(client) }()
	h.nextStream() // the boot's, open with nothing to send
	hash := h.cp.Publish(configA(t))
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Boot: %v", err)
		}
	case <-time.After(testWaitLimit):
		t.Fatal("Boot did not return once a config was published")
	}
	h.wantLoad(load{TriggerControl, true})
	wantApplied(t, client, hash)
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.delays) != 0 {
		t.Errorf("boot retried %d times, want the open stream to deliver the config", len(h.delays))
	}
}

// D7: what the operator must fix is never retried — a refused token, a config the
// gateway rejects: boot exits at once.
func TestBootDoesNotRetryWhatTheOperatorMustFix(t *testing.T) {
	for name, c := range map[string]struct {
		setup func(h *harness)
		token string
		want  string
	}{
		"refused token":   {func(h *harness) { h.cp.Publish(configA(t)) }, "wrong", "401"},
		"rejected config": {func(h *harness) { h.cp.Publish(configRejected(t)) }, testToken, "was rejected"},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			c.setup(h)
			client := h.client(func(o *Options) { o.Token = c.token; o.BootWait = testWaitLimit })
			started := time.Now()
			err := h.boot(client)
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
	h.boot(c)
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
	h2.boot(c2)
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
	h.boot(c)
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
	go func() { done <- c.followStream(context.Background(), false) }()
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
	if err := h.boot(c); err == nil {
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
	h.cp.SetProtocol("4")
	c := h.client(nil)
	if err := h.boot(c); err == nil {
		t.Fatal("Boot applied a config from a control plane speaking another version")
	}
	out := h.logs.String()
	if !strings.Contains(out, "level=ERROR") || !strings.Contains(out, "protocol version mismatch") ||
		!strings.Contains(out, "gateway speaks 5") {
		t.Errorf("mismatch not logged as an error:\n%s", out)
	}

	h.mu.Lock()
	h.delayed = make(chan time.Duration, 100)
	h.mu.Unlock()
	h.run(c)
	<-h.delayed // retried and failed again, still waiting
	h.cp.SetProtocol("5")
	h.wantLoad(load{TriggerControl, true})
	if n := len(h.cp.Gets()); n < 3 {
		t.Errorf("%d requests, want the boot's stream and at least two retries", n)
	}
}

func TestTotalsEventsReachTheConsumer(t *testing.T) {
	h := newHarness(t)
	h.cp.Publish(configA(t))
	c := h.client(nil)
	h.boot(c)
	h.wantLoad(load{TriggerControl, true})
	h.run(c)
	h.nextStream()

	raw, err := os.ReadFile(filepath.Join(fixturesDir, "totals", "valid", "windows.json"))
	if err != nil {
		t.Fatal(err)
	}
	var compacted bytes.Buffer // an event's data is one line
	if err := json.Compact(&compacted, raw); err != nil {
		t.Fatal(err)
	}
	data := compacted.Bytes()
	h.cp.PushTotals(data)
	want, err := DecodeTotals(data)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-h.totals:
		if got.totals.LiveGateways != want.LiveGateways || len(got.totals.Windows) != len(want.Windows) ||
			len(got.totals.Windows) == 0 || got.counted != 0 {
			t.Errorf("totals %+v, want %+v and nothing counted", got, want)
		}
	case <-time.After(testWaitLimit):
		t.Fatal("no totals delivered")
	}
}

// A totals event reaches the limiter as sent: the live-gateway count, whether the
// totals are complete — the stream's first are — and every window by group and type,
// with its start and amount.
func TestLimitsTotalsCarryWindowsByGroupAndType(t *testing.T) {
	h := newHarness(t)
	h.cp.Publish(configA(t))
	c := h.client(nil)
	h.boot(c)
	h.run(c)
	st := h.nextStream()

	st.Send("totals", []byte(`{"live_gateways":3,"counted_through":[],"windows":[`+
		`{"group":"team","type":"usd_per_month","window_start":"2026-09-01T00:00:00Z","used":"42"},`+
		`{"type":"tokens_per_hour","window_start":"2026-09-01T10:00:00Z","used":"7"}]}`))
	want := []limits.PushedWindow{
		{Group: "team", Type: config.LimitUSDPerMonth, Start: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), Used: 42},
		{Type: config.LimitTokensPerHour, Start: time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC), Used: 7},
	}
	u := h.nextTotals()
	if u.totals.LiveGateways != 3 || !u.totals.Complete || !slices.Equal(u.totals.Windows, want) {
		t.Errorf("totals %+v, want 3 live gateways, complete, and %+v", u.totals, want)
	}
	st.Send("totals", []byte(`{"live_gateways":3,"counted_through":[],"windows":[]}`))
	if u := h.nextTotals(); u.totals.Complete {
		t.Errorf("the stream's second totals %+v, want changes only", u.totals)
	}
}

// A totals event that cannot be decoded ends the stream: each later totals event lists
// only what changed since the one before, so the stream cannot go on without it. The
// reconnect's first totals are complete (AUDIT-3 3H1, [C] C5).
func TestMalformedTotalsEndTheStream(t *testing.T) {
	h := newHarness(t)
	h.cp.Publish(configA(t))
	c := h.client(nil)
	if err := h.boot(c); err != nil {
		t.Fatal(err)
	}
	h.run(c)
	st := h.nextStream()
	st.Send("totals", scriptedTotals(1))
	if u := h.nextTotals(); !u.totals.Complete {
		t.Fatalf("first totals %+v, want complete", u)
	}
	st.Send("totals", []byte(`{"live_gateways":1,"counted_through":[],"windows":[{"type":"usd_per_month","window_start":"2026-10-01T00:00:00Z","used":"oops"}]}`))
	st.Send("totals", scriptedTotals(2)) // a change after the lost one: never applied
	again := h.nextStream()
	select {
	case u := <-h.totals:
		t.Fatalf("totals %+v applied after a malformed one on the same stream", u)
	default:
	}
	again.Send("totals", scriptedTotals(3))
	if u := h.nextTotals(); !u.totals.Complete || u.totals.LiveGateways != 3 {
		t.Errorf("totals %+v after the reconnect, want the complete ones of the new stream", u)
	}
	if !strings.Contains(h.logs.String(), `level=ERROR msg="config stream failed"`) ||
		!strings.Contains(h.logs.String(), "totals event malformed") {
		t.Errorf("the ended stream not logged as an error:\n%s", h.logs.String())
	}
}
