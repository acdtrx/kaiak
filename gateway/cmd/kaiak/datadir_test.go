package main

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kaiak/internal/state"
)

// M11: a gateway refuses to start on a data directory another one holds, naming the
// lock file.
func TestRunRefusesADataDirectoryInUse(t *testing.T) {
	dataDir := t.TempDir()
	holder, err := state.Open(dataDir, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	env := envOf(map[string]string{"KAIAK_CONFIG_FILE": minimalFixture, "KAIAK_DATA_DIR": dataDir,
		"KAIAK_INSTANCE_ID": "test-1", "KAIAK_LISTEN_ADDR": "127.0.0.1:0", "KAIAK_ADMIN_ADDR": "127.0.0.1:0"})
	err = run(context.Background(), slog.New(slog.DiscardHandler), env, make(chan os.Signal), make(chan os.Signal))
	if err == nil || !strings.Contains(err.Error(), filepath.Join(dataDir, state.LockFile)) {
		t.Fatalf("run on a directory in use: %v, want an error naming the lock file", err)
	}
}

// M11: KAIAK_SEED_CONFIG_FILE belongs to control-plane mode and must hold a valid
// config, checked at startup rather than in the outage it is for.
func TestSeedConfigSetting(t *testing.T) {
	dir := t.TempDir()
	invalid := filepath.Join(dir, "invalid.json")
	if err := os.WriteFile(invalid, []byte(`{"format_version": 4}`), 0o600); err != nil {
		t.Fatal(err)
	}
	control := map[string]string{"KAIAK_CONTROL_URL": "http://cp.example", "KAIAK_CONTROL_TOKEN": "t0ken",
		"KAIAK_INSTANCE_ID": "gw-1"}
	with := func(base map[string]string, seed string) map[string]string {
		m := map[string]string{"KAIAK_SEED_CONFIG_FILE": seed}
		for k, v := range base {
			m[k] = v
		}
		return m
	}
	s, err := readSettings(envOf(with(control, minimalFixture)))
	if err != nil {
		t.Fatal(err)
	}
	if s.control.seedFile != minimalFixture || len(s.control.seed) == 0 {
		t.Errorf("seed %q (%d bytes), want the fixture", s.control.seedFile, len(s.control.seed))
	}
	if s, err := readSettings(envOf(control)); err != nil || s.control.seed != nil {
		t.Errorf("no seed set: %v, %v", s.control, err)
	}
	for name, c := range map[string]struct {
		env  map[string]string
		want string
	}{
		"file mode": {with(map[string]string{"KAIAK_CONFIG_FILE": minimalFixture, "KAIAK_INSTANCE_ID": "i"}, minimalFixture),
			"KAIAK_SEED_CONFIG_FILE is for control-plane mode"},
		"missing": {with(control, filepath.Join(dir, "none.json")), "KAIAK_SEED_CONFIG_FILE"},
		"invalid": {with(control, invalid), "KAIAK_SEED_CONFIG_FILE"},
	} {
		if _, err := readSettings(envOf(c.env)); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want an error containing %q", name, err, c.want)
		}
	}
}
