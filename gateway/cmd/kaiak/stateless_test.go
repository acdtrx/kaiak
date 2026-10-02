package main

// E1–E3 at the process level (in-process run): no data directory by default, the boot
// order with the seed, the seed's startup checks and the drain's flush reserve.

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"kaiak/internal/fakecontrol"
)

// absFixture is the minimal config fixture's absolute path, usable after a Chdir.
func absFixture(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs(minimalFixture)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// wantEmptyDir fails when dir holds anything.
func wantEmptyDir(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if len(names) > 0 {
		t.Errorf("files written with no data directory: %v", names)
	}
}

// E1: with KAIAK_DATA_DIR unset the gateway writes nothing — no data directory, no
// lock, no spool, no last-known-good copy, no totals, no limits snapshot — in either
// mode, through boot, serving and the drain.
func TestRunWithoutADataDirectoryWritesNothing(t *testing.T) {
	fixture := absFixture(t)
	data, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	cwd := t.TempDir()
	t.Chdir(cwd)

	t.Run("control-plane mode", func(t *testing.T) {
		cp := fakecontrol.New("cp-token")
		defer cp.Close()
		cp.Publish(data)
		var logs syncBuffer
		env := envOf(map[string]string{"KAIAK_CONTROL_URL": cp.URL(), "KAIAK_CONTROL_TOKEN": "cp-token",
			"KAIAK_INSTANCE_ID": "test-1", "KAIAK_LISTEN_ADDR": "127.0.0.1:0", "KAIAK_ADMIN_ADDR": "127.0.0.1:0",
			"KAIAK_DRAIN_GRACE_MS": "0"})
		stop := make(chan os.Signal, 1)
		stopped := stopWhenServing(&logs, stop)
		if err := run(context.Background(), slog.New(slog.NewTextHandler(&logs, nil)), env, make(chan os.Signal), stop); err != nil {
			t.Fatalf("run returned %v:\n%s", err, logs.String())
		}
		stopped()
		out := logs.String()
		for _, want := range []string{`msg="config applied" trigger=control`, "usage batches kept in memory until acknowledged",
			`msg="usage flushed"`} {
			if !strings.Contains(out, want) {
				t.Errorf("log misses %q:\n%s", want, out)
			}
		}
		for _, unwanted := range []string{"last-known-good", "limits totals", "data directory lock"} {
			if strings.Contains(out, unwanted) {
				t.Errorf("log mentions %q with no data directory:\n%s", unwanted, out)
			}
		}
		wantEmptyDir(t, cwd)
	})
	t.Run("file mode", func(t *testing.T) {
		var logs syncBuffer
		env := envOf(map[string]string{"KAIAK_CONFIG_FILE": fixture, "KAIAK_INSTANCE_ID": "test-1",
			"KAIAK_LISTEN_ADDR": "127.0.0.1:0", "KAIAK_ADMIN_ADDR": "127.0.0.1:0", "KAIAK_DRAIN_GRACE_MS": "0"})
		stop := make(chan os.Signal, 1)
		stop <- syscall.SIGTERM
		if err := run(context.Background(), slog.New(slog.NewTextHandler(&logs, nil)), env, make(chan os.Signal), stop); err != nil {
			t.Fatalf("run returned %v:\n%s", err, logs.String())
		}
		if out := logs.String(); strings.Contains(out, "limits snapshot") {
			t.Errorf("limits snapshot used with no data directory:\n%s", out)
		}
		wantEmptyDir(t, cwd)
	})
}

func TestDataDirectoryHasNoDefault(t *testing.T) {
	s, err := readSettings(envOf(map[string]string{"KAIAK_CONFIG_FILE": "c.json", "KAIAK_INSTANCE_ID": "i"}))
	if err != nil {
		t.Fatal(err)
	}
	if s.dataDir != "" {
		t.Errorf("data directory %q by default, want none", s.dataDir)
	}
}

// E2: in control-plane mode with no config from anywhere, run exits with the reason
// before binding anything.
func TestRunExitsWithNoConfigAtBoot(t *testing.T) {
	cp := fakecontrol.New("cp-token")
	cp.Close() // nothing listens there any more
	env := envOf(map[string]string{"KAIAK_CONTROL_URL": cp.URL(), "KAIAK_CONTROL_TOKEN": "cp-token",
		"KAIAK_INSTANCE_ID": "test-1", "KAIAK_LISTEN_ADDR": "127.0.0.1:0", "KAIAK_ADMIN_ADDR": "127.0.0.1:0",
		"KAIAK_CONTROL_BOOT_WAIT_MS": "500"})
	started := time.Now()
	err := run(context.Background(), slog.New(slog.DiscardHandler), env, make(chan os.Signal), make(chan os.Signal))
	if err == nil || !strings.Contains(err.Error(), "no config: control plane unavailable and no seed") {
		t.Fatalf("run returned %v, want the no-config error", err)
	}
	if took := time.Since(started); took > 5*time.Second {
		t.Errorf("run took %v to give up, want about the boot wait", took)
	}
	if left := ourGoroutines(); len(left) > 0 {
		t.Errorf("goroutines left after run returned:\n%s", strings.Join(left, "\n\n"))
	}
}

// writeSeed writes a seed config: the minimal fixture with the given changes to its
// "llama" model and "local" backend.
func writeSeed(t *testing.T, model, backend map[string]any) string {
	t.Helper()
	doc := map[string]any{
		"format_version": 4,
		"global":         map[string]any{},
		"backends":       map[string]any{"local": merged(map[string]any{"type": "openai-compatible", "base_url": "http://localhost:8080/v1"}, backend)},
		"models": map[string]any{"llama": merged(map[string]any{
			"deployments": []any{map[string]any{"backend": "local", "model": "llama"}},
			"metadata": map[string]any{"context_length": 8192,
				"capabilities": map[string]any{"streaming": true, "tools": false, "vision": false, "reasoning": false}},
		}, model)},
		"groups": map[string]any{"me": map[string]any{}},
		"keys": map[string]any{"k-me": map[string]any{"group": "me",
			"hash": "sha256:59f8f6709d858b919a10541cd215a0de7b4299bbc6c11a2e0bf4ca03ec6df717"}},
	}
	path := filepath.Join(t.TempDir(), "seed.json")
	writeJSONFile(t, path, doc)
	return path
}

func merged(base, extra map[string]any) map[string]any {
	for k, v := range extra {
		base[k] = v
	}
	return base
}

// E2, N-P4: the seed is checked completely at startup — credentials included — and
// may hold no priced model: it serves during an outage, when nothing could be billed
// against a budget the control plane keeps.
func TestSeedIsCheckedAtStartup(t *testing.T) {
	controlEnv := map[string]string{"KAIAK_CONTROL_URL": "http://cp.example", "KAIAK_CONTROL_TOKEN": "t",
		"KAIAK_INSTANCE_ID": "gw-1"}
	with := func(seed string, extra map[string]string) func(string) (string, bool) {
		m := map[string]string{"KAIAK_SEED_CONFIG_FILE": seed}
		for k, v := range controlEnv {
			m[k] = v
		}
		for k, v := range extra {
			m[k] = v
		}
		return envOf(m)
	}
	free := writeSeed(t, nil, nil)
	if _, err := readSettings(with(free, nil)); err != nil {
		t.Errorf("free seed refused: %v", err)
	}
	priced := writeSeed(t, map[string]any{"prices": []any{map[string]any{"effective_from": "2026-01-01",
		"tiers": []any{map[string]any{"above_input_tokens": 0,
			"usd_per_million": map[string]any{"tokens_in": 1, "tokens_out": 2}}}}}}, nil)
	if _, err := readSettings(with(priced, nil)); err == nil || !strings.Contains(err.Error(), `model "llama" is priced`) {
		t.Errorf("priced seed: %v, want an error naming the model", err)
	}
	keyed := writeSeed(t, nil, map[string]any{"api_key_env": "SEED_BACKEND_KEY"})
	if _, err := readSettings(with(keyed, nil)); err == nil || !strings.Contains(err.Error(), "api-key-env-unset") {
		t.Errorf("seed naming an unset key variable: %v, want api-key-env-unset", err)
	}
	if _, err := readSettings(with(keyed, map[string]string{"SEED_BACKEND_KEY": "secret"})); err != nil {
		t.Errorf("seed with its key variable set: %v", err)
	}
}

// E2: a seed boot serves, and the status says ready (N-P11).
func TestRunBootsFromTheSeedWithTheControlPlaneDown(t *testing.T) {
	cp := fakecontrol.New("cp-token")
	cp.Close()
	var logs syncBuffer
	env := envOf(map[string]string{"KAIAK_CONTROL_URL": cp.URL(), "KAIAK_CONTROL_TOKEN": "cp-token",
		"KAIAK_INSTANCE_ID": "test-1", "KAIAK_LISTEN_ADDR": "127.0.0.1:0", "KAIAK_ADMIN_ADDR": "127.0.0.1:0",
		"KAIAK_CONTROL_BOOT_WAIT_MS": "200", "KAIAK_DRAIN_GRACE_MS": "0", "KAIAK_SEED_CONFIG_FILE": writeSeed(t, nil, nil)})
	stop := make(chan os.Signal, 1)
	stopped := stopWhenServing(&logs, stop)
	if err := run(context.Background(), slog.New(slog.NewTextHandler(&logs, nil)), env, make(chan os.Signal), stop); err != nil {
		t.Fatalf("run returned %v:\n%s", err, logs.String())
	}
	stopped()
	if out := logs.String(); !strings.Contains(out, `msg="config applied" trigger=seed`) {
		t.Errorf("no seed boot:\n%s", out)
	}
}

func TestDrainFlushReserveSettings(t *testing.T) {
	base := map[string]string{"KAIAK_CONTROL_URL": "http://cp.example", "KAIAK_CONTROL_TOKEN": "t", "KAIAK_INSTANCE_ID": "i"}
	with := func(extra map[string]string) func(string) (string, bool) {
		m := map[string]string{}
		for k, v := range base {
			m[k] = v
		}
		for k, v := range extra {
			m[k] = v
		}
		return envOf(m)
	}
	for name, c := range map[string]struct {
		env  map[string]string
		want time.Duration
	}{
		"default":                     {nil, 10 * time.Second},
		"default under a short drain": {map[string]string{"KAIAK_DRAIN_TIMEOUT_MS": "4000"}, 2 * time.Second},
		"set":                         {map[string]string{"KAIAK_DRAIN_FLUSH_RESERVE_MS": "2500"}, 2500 * time.Millisecond},
		"zero":                        {map[string]string{"KAIAK_DRAIN_FLUSH_RESERVE_MS": "0"}, 0},
		"the whole timeout":           {map[string]string{"KAIAK_DRAIN_TIMEOUT_MS": "3000", "KAIAK_DRAIN_FLUSH_RESERVE_MS": "3000"}, 3 * time.Second},
	} {
		s, err := readSettings(with(c.env))
		if err != nil || s.drainReserve != c.want {
			t.Errorf("%s: reserve %v (%v), want %v", name, s.drainReserve, err, c.want)
		}
	}
	for _, bad := range []map[string]string{
		{"KAIAK_DRAIN_FLUSH_RESERVE_MS": "-1"},
		{"KAIAK_DRAIN_FLUSH_RESERVE_MS": "5s"},
		{"KAIAK_DRAIN_TIMEOUT_MS": "3000", "KAIAK_DRAIN_FLUSH_RESERVE_MS": "3001"},
	} {
		if _, err := readSettings(with(bad)); err == nil || !strings.Contains(err.Error(), "KAIAK_DRAIN_FLUSH_RESERVE_MS") {
			t.Errorf("%v: got %v, want an error naming KAIAK_DRAIN_FLUSH_RESERVE_MS", bad, err)
		}
	}
}

func writeJSONFile(t *testing.T, path string, v any) {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}
