package config

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"kaiak/internal/schemacheck"
)

// fullEnv sets the API-key variables full.json names.
func fullEnv(name string) (string, bool) {
	switch name {
	case "VLLM_B_API_KEY", "AZURE_WESTEUROPE_API_KEY":
		return "secret-value", true
	}
	return "", false
}

func noEnv(string) (string, bool) { return "", false }

func copyFixture(t *testing.T, name, to string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(fixturesDir, "valid", name))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func newTestLoader(t *testing.T, env func(string) (string, bool)) (*FileLoader, *Holder, string, *bytes.Buffer) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	var logs bytes.Buffer
	holder := &Holder{}
	loader := NewFileLoader(path, NewApplier(holder, slog.New(slog.NewTextHandler(&logs, nil)), env, nil))
	return loader, holder, path, &logs
}

func TestApplierTellsOnLoadOfEveryLoad(t *testing.T) {
	var loads []Load
	var logs bytes.Buffer
	holder := &Holder{}
	applier := NewApplier(holder, slog.New(slog.NewTextHandler(&logs, nil)), noEnv,
		func(l Load) { loads = append(loads, l) })
	valid, err := os.ReadFile(filepath.Join(fixturesDir, "valid", "minimal.json"))
	if err != nil {
		t.Fatal(err)
	}

	before := time.Now()
	applied, err := applier.Apply("control", valid, "kaiak.config.hash", strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := applier.Apply("control", []byte(`{`), "kaiak.config.hash", strings.Repeat("b", 64)); err == nil {
		t.Fatal("a broken document was applied")
	}
	_ = applier.Reject("sighup", os.ErrNotExist, "file", "x.json")

	type load struct {
		trigger  Trigger
		applied  bool
		document bool
		bytes    int
	}
	var got []load
	for _, l := range loads {
		got = append(got, load{l.Trigger, l.Applied(), l.Document, l.Bytes})
		if l.At.Before(before) {
			t.Errorf("load %+v: no end time", l)
		}
		// A document load is timed; the clock never reads zero across a parse.
		if l.Document != (l.Duration > 0) {
			t.Errorf("load %+v: a duration only for a document, and always then", l)
		}
	}
	want := []load{{"control", true, true, len(valid)}, {"control", false, true, 1}, {"sighup", false, false, 0}}
	if !slices.Equal(got, want) {
		t.Errorf("loads %+v, want %+v", got, want)
	}
	// An applied load carries the snapshot it swapped in.
	if len(loads) == len(want) && (loads[0].Snapshot != applied || holder.Current() != applied) {
		t.Errorf("applied load's snapshot %p, want the one applied, %p", loads[0].Snapshot, applied)
	}
	out := logs.String()
	for _, line := range []string{
		fmt.Sprintf(`msg="config applied" kaiak.trigger=control kaiak.config.hash=`+strings.Repeat("a", 64)+` kaiak.config.backends=1 kaiak.config.models=1 kaiak.config.keys=1 kaiak.config.size=%d kaiak.duration=`, len(valid)),
		`msg="config rejected" kaiak.trigger=control kaiak.config.hash=` + strings.Repeat("b", 64),
		"kaiak.config.issue_codes=[syntax] kaiak.config.running=kept kaiak.config.size=1 kaiak.duration=",
	} {
		if !strings.Contains(out, line) {
			t.Errorf("log misses %q:\n%s", line, out)
		}
	}
	// A load with no document has no size or duration to log.
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if last := lines[len(lines)-1]; !strings.Contains(last, "kaiak.trigger=sighup") ||
		strings.Contains(last, "kaiak.config.size=") || strings.Contains(last, "kaiak.duration=") {
		t.Errorf("the unreadable file's line %q: want no kaiak.config.size or kaiak.duration", last)
	}
}

func TestLoadAppliesAValidFile(t *testing.T) {
	loader, holder, path, logs := newTestLoader(t, fullEnv)
	copyFixture(t, "full.json", path)

	if err := loader.Load("startup"); err != nil {
		t.Fatal(err)
	}
	if !holder.Loaded() || len(holder.Current().Models) != 4 {
		t.Fatal("snapshot not applied")
	}
	if out := logs.String(); !strings.Contains(out, "config applied") || !strings.Contains(out, "kaiak.trigger=startup") {
		t.Errorf("log misses the applied line:\n%s", out)
	}
	if strings.Contains(logs.String(), "secret-value") {
		t.Error("a credential reached the log")
	}
}

func TestFailedReloadKeepsTheRunningSnapshot(t *testing.T) {
	loader, holder, path, logs := newTestLoader(t, noEnv)
	copyFixture(t, "minimal.json", path)
	if err := loader.Load("startup"); err != nil {
		t.Fatal(err)
	}
	running := holder.Current()

	edited := strings.Replace(mustRead(t, path), `"context_length": 8192`, `"context_length": 0`, 1)
	if err := os.WriteFile(path, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	err := loader.Load("sighup")

	var invalid *schemacheck.ValidationError
	if !errors.As(err, &invalid) || !slices.Equal(invalid.Codes(), []string{schemacheck.CodeSchema}) {
		t.Fatalf("want a schema rejection, got %v", err)
	}
	if holder.Current() != running {
		t.Error("a rejected config replaced the running one")
	}
	out := logs.String()
	for _, want := range []string{"config rejected", "kaiak.trigger=sighup", "kaiak.config.issue_codes=[schema]", "kaiak.config.running=kept", "/models/llama/metadata/context_length"} {
		if !strings.Contains(out, want) {
			t.Errorf("log misses %q:\n%s", want, out)
		}
	}

	// Fixing the file and reloading again applies it.
	copyFixture(t, "users-child-defaults.json", path)
	if err := loader.Load("sighup"); err != nil {
		t.Fatal(err)
	}
	if holder.Current() == running || holder.Current().Models["large"] == nil {
		t.Error("a valid reload was not applied")
	}
}

func TestInitialLoadFailureLeavesNothingLoaded(t *testing.T) {
	loader, holder, _, _ := newTestLoader(t, noEnv)
	err := loader.Load("startup") // the file does not exist
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("want a not-exist error, got %v", err)
	}
	if holder.Loaded() {
		t.Error("holder reports a config after a failed first load")
	}
}

func TestLoadRejectsUnsetAPIKeyVariables(t *testing.T) {
	loader, holder, path, _ := newTestLoader(t, func(name string) (string, bool) {
		if name == "VLLM_B_API_KEY" {
			return "", true // set but empty counts as unset
		}
		return fullEnv(name)
	})
	copyFixture(t, "full.json", path)

	err := loader.Load("startup")
	var invalid *schemacheck.ValidationError
	if !errors.As(err, &invalid) || !slices.Equal(invalid.Codes(), []string{CodeAPIKeyEnvUnset}) {
		t.Fatalf("want an %s rejection, got %v", CodeAPIKeyEnvUnset, err)
	}
	if len(invalid.Issues) != 1 || invalid.Issues[0].Path != "/backends/vllm-b/api_key_env" {
		t.Errorf("issues = %v", invalid.Issues)
	}
	if holder.Loaded() {
		t.Error("config applied despite a missing credential")
	}
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// A second "backends" member could hold a backend the schema walker never sees (the
// generic tree keeps the last member, the typed decode merges both), naming a
// KAIAK_ variable as its credential. Duplicate members are
// refused before either decoder runs, at any depth.
func TestDuplicateMembersCannotHideABackendFromTheSchema(t *testing.T) {
	doc := minimalDoc(`{ "type": "openai-compatible", "base_url": "http://x/v1" }`)
	doc = strings.Replace(doc, `"format_version": 5,`, `"backends": { "sneaky": { "type": "openai-compatible",
    "base_url": "https://collector.example/v1", "api_key_env": "KAIAK_CONTROL_TOKEN" } },
  "format_version": 5,`, 1)
	doc = strings.Replace(doc, `{ "backend": "local", "model": "llama" }`, `{ "backend": "sneaky", "model": "llama" }`, 1)

	_, err := Check([]byte(doc), func(string) (string, bool) { return "dummy-secret", true })
	var invalid *schemacheck.ValidationError
	if !errors.As(err, &invalid) || !slices.Equal(invalid.Codes(), []string{schemacheck.CodeDuplicateMember}) {
		t.Fatalf("want a %s rejection, got %v", schemacheck.CodeDuplicateMember, err)
	}
	if len(invalid.Issues) != 1 || invalid.Issues[0].Path != "/backends" {
		t.Errorf("issues = %v, want one at /backends", invalid.Issues)
	}

	nested := strings.Replace(minimalDoc(`{ "type": "openai-compatible", "base_url": "http://x/v1" }`),
		`"context_length": 8192.0,`, `"context_length": 8192.0, "context_length": 1,`, 1)
	invalid = mustReject(t, nested)
	if !slices.Equal(invalid.Codes(), []string{schemacheck.CodeDuplicateMember}) || invalid.Issues[0].Path != "/models/llama/metadata/context_length" {
		t.Errorf("nested duplicate: %v", invalid)
	}
}
