package main

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"

	"kaiak/internal/server"
)

// KAIAK_BODY_MEMORY_BYTES sets the body budget, KAIAK_UPLOAD_MEMORY_BYTES the upload
// budget apart from it: whole bytes above 0.
func TestBodyMemorySettings(t *testing.T) {
	base := map[string]string{"KAIAK_CONFIG_FILE": "c.json", "KAIAK_INSTANCE_ID": "i"}
	with := func(name, value string) map[string]string {
		m := map[string]string{name: value}
		for k, v := range base {
			m[k] = v
		}
		return m
	}
	s, err := readSettings(envOf(base))
	if err != nil || s.bodyMemory != server.DefaultBodyMemory || s.uploadMemory != server.DefaultUploadMemory {
		t.Errorf("defaults %d and %d, %v; want %d and %d", s.bodyMemory, s.uploadMemory, err, server.DefaultBodyMemory,
			server.DefaultUploadMemory)
	}
	for name, got := range map[string]func(settings) int64{
		"KAIAK_BODY_MEMORY_BYTES":   func(s settings) int64 { return s.bodyMemory },
		"KAIAK_UPLOAD_MEMORY_BYTES": func(s settings) int64 { return s.uploadMemory },
	} {
		if s, err := readSettings(envOf(with(name, "1048576"))); err != nil || got(s) != 1<<20 {
			t.Errorf("%s set %d, %v; want 1048576", name, got(s), err)
		}
		for _, bad := range []string{"0", "-1", "1.5", "1MiB", "99999999999999999999"} {
			if _, err := readSettings(envOf(with(name, bad))); err == nil || !strings.Contains(err.Error(), name) {
				t.Errorf("%s=%q: got %v, want an error naming it", name, bad, err)
			}
		}
	}
}

// A config whose body cap exceeds the whole body budget, or whose upload cap exceeds
// the whole upload budget, is applied, with a warning: bodies above the budget are
// refused as too large.
func TestCapAboveItsBudgetIsWarned(t *testing.T) {
	for budget, warning := range map[string]string{
		"KAIAK_BODY_MEMORY_BYTES":   "max_request_body_bytes exceeds the body budget",
		"KAIAK_UPLOAD_MEMORY_BYTES": "max_upload_bytes exceeds the upload budget",
	} {
		var logs syncBuffer
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // stop at once: the startup load is what the test reads
		env := envOf(map[string]string{"KAIAK_CONFIG_FILE": minimalFixture,
			"KAIAK_INSTANCE_ID": "test-1", "KAIAK_LISTEN_ADDR": "127.0.0.1:0", "KAIAK_ADMIN_ADDR": "127.0.0.1:0",
			budget: "1000"})
		if err := run(ctx, slog.New(slog.NewTextHandler(&logs, nil)), env, make(chan os.Signal), make(chan os.Signal)); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(logs.String(), warning) {
			t.Errorf("%s=1000: no warning %q:\n%s", budget, warning, logs.String())
		}
		if other := strings.Count(logs.String(), "exceeds the"); other != 1 {
			t.Errorf("%s=1000: %d budget warnings, want the one:\n%s", budget, other, logs.String())
		}
	}
}

// KAIAK_USAGE_MEMORY_BYTES (whole bytes above 0, default 64 MiB) bounds the usage
// held in memory; KAIAK_MAX_CONNECTIONS (0 or more, default 0: no cap) caps the API
// listener's connections.
func TestUsageMemoryAndConnectionSettings(t *testing.T) {
	base := map[string]string{"KAIAK_CONTROL_URL": "http://cp.example", "KAIAK_CONTROL_TOKEN": "t", "KAIAK_INSTANCE_ID": "i"}
	with := func(name, value string) map[string]string {
		m := map[string]string{name: value}
		for k, v := range base {
			m[k] = v
		}
		return m
	}
	s, err := readSettings(envOf(base))
	if err != nil || s.usageMemory != 64<<20 || s.maxConnections != 0 {
		t.Errorf("defaults %d bytes, %d connections, %v; want 67108864 and 0", s.usageMemory, s.maxConnections, err)
	}
	if s, err := readSettings(envOf(with("KAIAK_USAGE_MEMORY_BYTES", "1048576"))); err != nil || s.usageMemory != 1<<20 {
		t.Errorf("usage memory set %d, %v; want 1048576", s.usageMemory, err)
	}
	if s, err := readSettings(envOf(with("KAIAK_MAX_CONNECTIONS", "5000"))); err != nil || s.maxConnections != 5000 {
		t.Errorf("max connections set %d, %v; want 5000", s.maxConnections, err)
	}
	for name, bads := range map[string][]string{
		"KAIAK_USAGE_MEMORY_BYTES": {"0", "-1", "1.5", "64MiB", "99999999999999999999"},
		"KAIAK_MAX_CONNECTIONS":    {"-1", "1.5", "many", "99999999999999999999"},
	} {
		for _, bad := range bads {
			if _, err := readSettings(envOf(with(name, bad))); err == nil || !strings.Contains(err.Error(), name) {
				t.Errorf("%s=%q: got %v, want an error naming it", name, bad, err)
			}
		}
	}
}

// KAIAK_METRICS_TOKEN is the bearer token /metrics requires; unset or empty,
// /metrics is open.
func TestMetricsTokenSetting(t *testing.T) {
	base := map[string]string{"KAIAK_CONFIG_FILE": "c.json", "KAIAK_INSTANCE_ID": "i"}
	if s, err := readSettings(envOf(base)); err != nil || s.metricsToken != "" {
		t.Errorf("default %q, %v; want none", s.metricsToken, err)
	}
	base["KAIAK_METRICS_TOKEN"] = "s3cret"
	if s, err := readSettings(envOf(base)); err != nil || s.metricsToken != "s3cret" {
		t.Errorf("set %q, %v; want s3cret", s.metricsToken, err)
	}
}
