package e2e

// The stateless gateway through the built binary (docs/specs/GATEWAY.md,
// Configuration sources and Lifecycle): the minimal Kubernetes setup — the control
// plane's URL and token, nothing else, a read-only working directory — the boot
// order with the seed config, and the drain's flush reserve delivering the records of
// the requests it cuts.

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"kaiak/internal/fakebackend"
	"kaiak/internal/fakecontrol"
)

// minimalControlEnv is the minimal control-plane setup: the control plane's URL and
// token. The listen addresses are set only so parallel test runs never collide on the
// default ports.
func minimalControlEnv(controlURL, token string) []string {
	return []string{"KAIAK_CONTROL_URL=" + controlURL, "KAIAK_CONTROL_TOKEN=" + token,
		"KAIAK_LISTEN_ADDR=127.0.0.1:0", "KAIAK_ADMIN_ADDR=127.0.0.1:0"}
}

// readOnlyDir is an empty directory nobody may write to, removed at test cleanup.
func readOnlyDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "ro")
	if err := os.Mkdir(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	return dir
}

// wantEmpty fails when dir holds anything.
func wantEmpty(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) > 0 {
		t.Errorf("%s holds %d entries, want none", dir, len(entries))
	}
}

// exitCode waits for g to exit and returns its exit code (0 when it succeeded).
func (g *gateway) exitCode(t *testing.T, within time.Duration) int {
	t.Helper()
	select {
	case <-g.exited:
	case <-time.After(within):
		t.Fatalf("gateway did not exit within %s", within)
	}
	var exit *exec.ExitError
	switch {
	case g.err == nil:
		return 0
	case errors.As(g.err, &exit):
		return exit.ExitCode()
	}
	t.Fatalf("gateway ended with %v", g.err)
	return 0
}

// exitError returns the error the gateway logged as it stopped with an error.
func (g *gateway) exitError(t *testing.T) string {
	t.Helper()
	return g.logs.wait(t, "the exit error", msg("kaiak stopped with an error"))["exception.message"].(string)
}

// freeConfig is testConfig without its priced models: what a seed may hold.
func freeConfig(backendURL, evalHash, annHash string) map[string]any {
	cfg := testConfig(backendURL, evalHash, annHash, "")
	models := cfg["models"].(map[string]any)
	delete(models, "chat")
	delete(models, "priced")
	global := cfg["global"].(map[string]any)
	global["limits"] = []any{}
	groups := cfg["groups"].(map[string]any)
	groups["users"] = map[string]any{"child_defaults": map[string]any{"allowed_models": []any{"rpm", "embed"}}}
	return cfg
}

// The target deployment: only the control plane's URL and token, a read-only working
// directory. The gateway boots from the control plane, serves, reports usage and
// status, drains, and writes nothing.
func TestMinimalControlPlaneSetup(t *testing.T) {
	backend := fakebackend.New()
	defer backend.Close()
	const token = "e2e-control-token"
	cp := fakecontrol.New(token)
	defer cp.Close()
	evalKey, evalHash := newKey()
	_, annHash := newKey()
	data, err := json.Marshal(testConfig(backend.URL(), evalHash, annHash, ""))
	if err != nil {
		t.Fatal(err)
	}
	cp.Publish(data)
	cwd := readOnlyDir(t)

	g := startGatewayIn(t, cwd, minimalControlEnv(cp.URL(), token))
	g.logs.wait(t, "the boot from the control plane", msg("config applied", "kaiak.trigger", "control", "kaiak.config.version", "1"))
	g.logs.wait(t, "memory mode", msg("usage batches kept in memory until acknowledged: no data directory"))
	if r := g.post(t, "/v1/chat/completions", evalKey, "e2e-minimal-1", chatBody("chat", false, nil)); r.StatusCode != http.StatusOK {
		t.Fatalf("chat: %d %s", r.StatusCode, r.body)
	}
	waitUsage(t, cp, "the batch with e2e-minimal-1", func(e fakecontrol.UsageEvent) bool {
		return e.Outcome == fakecontrol.OutcomeCounted && countedIDs(t, cp)["e2e-minimal-1"] == 1
	})
	if r := g.post(t, "/v1/chat/completions", evalKey, "e2e-minimal-2", chatBody("chat", false, nil)); r.StatusCode != http.StatusOK {
		t.Fatalf("chat: %d %s", r.StatusCode, r.body)
	}
	g.stop(t)
	if n := countedIDs(t, cp)["e2e-minimal-2"]; n != 1 {
		t.Errorf("e2e-minimal-2 counted %d times by the drain's flush, want 1", n)
	}
	for _, body := range cp.Statuses() {
		var st map[string]any
		if err := json.Unmarshal(body, &st); err != nil || (st["state"] != "ready" && st["state"] != "draining") {
			t.Errorf("status %s", body)
		}
	}
	for _, line := range []string{"not written", "lock", "last-known-good", "spool"} {
		if strings.Contains(g.logs.text(), line) {
			t.Errorf("log mentions %q in the minimal setup", line)
		}
	}
	wantEmpty(t, cwd)
}

// E2: the minimal setup with the control plane down at boot and no seed exits
// non-zero with the reason, once the boot wait (retrying all along, D7) is over.
func TestNoConfigAtBootExits(t *testing.T) {
	cp := fakecontrol.New("t")
	cp.Close()
	started := time.Now()
	g := startProcess(t, readOnlyDir(t), append(minimalControlEnv(cp.URL(), "t"), "KAIAK_CONTROL_BOOT_WAIT_MS=2000"))
	if code := g.exitCode(t, waitLimit); code == 0 {
		t.Fatal("exited 0 with no config")
	}
	if took := time.Since(started); took < 2*time.Second || took > 2*time.Second+3*time.Second {
		t.Errorf("gave up after %s, want about the 2 s boot wait", took)
	}
	g.logs.wait(t, "the retries", msg("config snapshot not fetched at startup: retrying within the boot wait", "kaiak.control.attempt", "1"))
	if e := g.exitError(t); !strings.Contains(e, "no config: control plane unavailable and no seed") {
		t.Errorf("exit error %q", e)
	}
}

// E2: with the control plane down the seed config serves its free models, and the
// gateway is ready.
func TestSeedServesWithTheControlPlaneDown(t *testing.T) {
	backend := fakebackend.New()
	defer backend.Close()
	cp := fakecontrol.New("t")
	cp.Close()
	evalKey, evalHash := newKey()
	_, annHash := newKey()
	seed := filepath.Join(t.TempDir(), "seed.json")
	writeJSON(t, seed, freeConfig(backend.URL(), evalHash, annHash))

	g := startGatewayIn(t, readOnlyDir(t), append(minimalControlEnv(cp.URL(), "t"), "KAIAK_SEED_CONFIG_FILE="+seed,
		"KAIAK_CONTROL_BOOT_WAIT_MS=500", "KAIAK_DRAIN_GRACE_MS=0", "KAIAK_DRAIN_TIMEOUT_MS=1000"))
	g.logs.wait(t, "the seed boot", msg("config applied", "kaiak.trigger", "seed"))
	if r := g.post(t, "/v1/chat/completions", evalKey, "", chatBody("rpm", false, nil)); r.StatusCode != http.StatusOK {
		t.Fatalf("chat on the seed: %d %s", r.StatusCode, r.body)
	}
	g.stop(t)
	g.logs.wait(t, "the undelivered usage", msg("usage not flushed: lost at exit (no data directory)", "level", "ERROR", "kaiak.usage.batches", "1"))
}

// E2: a seed with a priced model fails the start, naming the model.
func TestPricedSeedFailsTheStart(t *testing.T) {
	cp := fakecontrol.New("t")
	defer cp.Close()
	_, evalHash := newKey()
	_, annHash := newKey()
	seed := filepath.Join(t.TempDir(), "seed.json")
	writeJSON(t, seed, testConfig("http://127.0.0.1:1", evalHash, annHash, ""))
	g := startProcess(t, readOnlyDir(t), append(minimalControlEnv(cp.URL(), "t"), "KAIAK_SEED_CONFIG_FILE="+seed))
	if code := g.exitCode(t, waitLimit); code == 0 {
		t.Fatal("exited 0 with a priced seed")
	}
	if e := g.exitError(t); !strings.Contains(e, `model "chat" is priced`) {
		t.Errorf("exit error %q", e)
	}
	if n := len(cp.Gets()); n != 0 {
		t.Errorf("%d control-plane requests before the seed was checked, want none", n)
	}
}

// streamInBackground sends a streaming chat request and reads its answer to the end,
// off the test goroutine; done is closed when the answer ends however it ends.
func streamInBackground(g *gateway, key, requestID, model string) (done <-chan struct{}) {
	ch := make(chan struct{})
	body, _ := json.Marshal(chatBody(model, true, nil)) // a map of plain values always encodes
	req, _ := http.NewRequest(http.MethodPost, g.api+"/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("X-Request-Id", requestID)
	go func() {
		defer close(ch)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}()
	return ch
}

// E3: with no data directory, batches keep going out on the 5 s interval through the
// drain, and a stream still running at drain timeout − flush reserve is cut there;
// its partial record reaches the control plane in the reserve, before the exit.
func TestDrainReserveDeliversTheCutRequestsUsage(t *testing.T) {
	backend := fakebackend.New()
	defer backend.Close()
	const token = "e2e-control-token"
	cp := fakecontrol.New(token)
	defer cp.Close()
	evalKey, evalHash := newKey()
	_, annHash := newKey()
	data, err := json.Marshal(testConfig(backend.URL(), evalHash, annHash, ""))
	if err != nil {
		t.Fatal(err)
	}
	cp.Publish(data)

	g := startGatewayEnv(t, append(controlEnv(cp.URL(), token, ""),
		"KAIAK_DRAIN_TIMEOUT_MS=8000", "KAIAK_DRAIN_FLUSH_RESERVE_MS=1500"))
	g.logs.wait(t, "the stream", msg("config stream connected", "kaiak.config.since", "1"))

	pace := make(chan struct{})
	backend.QueueReplies(fakebackend.Reply{Pace: pace, Chunks: []string{"a", "b"}}, fakebackend.Reply{HangAfter: 1})
	finishing := streamInBackground(g, evalKey, "e2e-drain-finishing", "chat")
	<-backend.Arrivals()
	cut := streamInBackground(g, evalKey, "e2e-drain-cut", "chat")
	<-backend.Arrivals()

	g.signal(t, syscall.SIGTERM)
	g.logs.wait(t, "the drain", msg("draining: refusing new requests"))
	close(pace) // the first stream ends during the drain
	<-finishing
	// Its record goes out with the next 5 s seal, while the other stream still runs.
	waitUsage(t, cp, "the finished stream's batch during the drain", func(e fakecontrol.UsageEvent) bool {
		return e.Outcome == fakecontrol.OutcomeCounted && countedIDs(t, cp)["e2e-drain-finishing"] == 1
	})
	select {
	case <-cut:
		t.Fatal("the hanging stream ended before the cut")
	default:
	}

	g.logs.wait(t, "the cut", msg("drain: cutting off in-flight requests"))
	started := time.Now()
	<-cut
	g.waitExit(t)
	if took := time.Since(started); took > 3*time.Second {
		t.Errorf("exit %s after the cut, want within the 1.5 s reserve (and the final status)", took)
	}
	rec := countedRecord(t, cp, "e2e-drain-cut")
	if rec["partial"] != true {
		t.Errorf("cut stream's record %v, want partial", rec)
	}
	g.logs.wait(t, "the flush", msg("usage flushed", "kaiak.trigger", "drain"))
}
