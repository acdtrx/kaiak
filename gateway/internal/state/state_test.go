package state

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type sample struct {
	Counts map[string]int `json:"counts"`
}

func openTestDir(t *testing.T) (*Dir, *bytes.Buffer) {
	t.Helper()
	var logs bytes.Buffer
	d, err := Open(filepath.Join(t.TempDir(), "nested", "data"), slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d, &logs
}

func TestOpenCreatesTheDirectory(t *testing.T) {
	d, _ := openTestDir(t)
	if info, err := os.Stat(d.Path()); err != nil || !info.IsDir() {
		t.Fatalf("data directory not created: %v", err)
	}
}

func TestOpenRejectsAFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path, slog.New(slog.DiscardHandler)); err == nil {
		t.Fatal("Open accepted a regular file")
	}
}

func TestVersionedRoundTrip(t *testing.T) {
	d, _ := openTestDir(t)
	want := sample{Counts: map[string]int{"a": 1}}
	if err := d.WriteVersioned("usage.json", 3, want); err != nil {
		t.Fatal(err)
	}
	// Overwriting replaces the content.
	want.Counts["a"] = 2
	if err := d.WriteVersioned("usage.json", 3, want); err != nil {
		t.Fatal(err)
	}

	var got sample
	found, err := d.ReadVersioned("usage.json", 3, &got)
	if err != nil || !found {
		t.Fatalf("found=%v err=%v", found, err)
	}
	if got.Counts["a"] != 2 {
		t.Errorf("read %+v", got)
	}

	entries, err := os.ReadDir(d.Path())
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		if e.Name() != LockFile {
			names = append(names, e.Name())
		}
	}
	if len(names) != 1 {
		t.Errorf("directory holds %v beside the lock file, want only usage.json (no temporary files left)", names)
	}
}

func TestReadMissingFileReportsNotFound(t *testing.T) {
	d, _ := openTestDir(t)
	var got sample
	found, err := d.ReadVersioned("usage.json", 1, &got)
	if err != nil || found {
		t.Fatalf("found=%v err=%v", found, err)
	}
}

func TestReadDiscardsAnotherFormatVersion(t *testing.T) {
	d, logs := openTestDir(t)
	if err := d.WriteVersioned("usage.json", 1, sample{}); err != nil {
		t.Fatal(err)
	}

	var got sample
	found, err := d.ReadVersioned("usage.json", 2, &got)
	if err != nil || found {
		t.Fatalf("found=%v err=%v", found, err)
	}
	if _, err := os.Stat(filepath.Join(d.Path(), "usage.json")); !os.IsNotExist(err) {
		t.Error("the stale file was not discarded")
	}
	if out := logs.String(); !strings.Contains(out, "discarded data file") || !strings.Contains(out, "found_version=1") {
		t.Errorf("discard not logged:\n%s", out)
	}
}

func TestReadCorruptFileIsAnError(t *testing.T) {
	d, _ := openTestDir(t)
	if err := os.WriteFile(filepath.Join(d.Path(), "usage.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	var got sample
	if _, err := d.ReadVersioned("usage.json", 1, &got); err == nil {
		t.Fatal("corrupt file read without error")
	}
}

// A repeated member (here merged into the map by encoding/json) means the file was
// not written by the gateway: it is refused, not read as either occurrence.
func TestReadRefusesARepeatedMember(t *testing.T) {
	d, _ := openTestDir(t)
	raw := `{"format_version":1,"data":{"counts":{"a":1},"counts":{"b":2}}}`
	if err := os.WriteFile(filepath.Join(d.Path(), "usage.json"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	var got sample
	_, err := d.ReadVersioned("usage.json", 1, &got)
	if err == nil || !strings.Contains(err.Error(), "/data/counts") {
		t.Fatalf("err = %v, want a repeated-member error naming /data/counts", err)
	}
}

func TestFileNamesStayInsideTheDirectory(t *testing.T) {
	d, _ := openTestDir(t)
	for _, name := range []string{"", ".", "..", "../escape.json", "sub/file.json"} {
		if err := d.WriteVersioned(name, 1, sample{}); err == nil {
			t.Errorf("WriteVersioned accepted %q", name)
		}
	}
}

func TestListRemoveAndRename(t *testing.T) {
	d, _ := openTestDir(t)
	for _, name := range []string{"spool-b.json", "spool-a.json", "other.json"} {
		if err := d.WriteVersioned(name, 1, sample{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(d.Path(), ".spool-c.json.123.tmp"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	names := func() []string {
		files, err := d.List("spool-")
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, f := range files {
			if f.ModTime.IsZero() {
				t.Errorf("%s has no modification time", f.Name)
			}
			out = append(out, f.Name)
		}
		return out
	}
	if got := strings.Join(names(), ","); got != "spool-a.json,spool-b.json" {
		t.Fatalf("listed %s", got)
	}
	if err := d.Rename("spool-a.json", "spool-z.json"); err != nil {
		t.Fatal(err)
	}
	if err := d.Remove("spool-b.json"); err != nil {
		t.Fatal(err)
	}
	if err := d.Remove("spool-b.json"); err != nil {
		t.Errorf("removing a missing file: %v", err)
	}
	if got := strings.Join(names(), ","); got != "spool-z.json" {
		t.Fatalf("listed %s after rename and remove", got)
	}
	if err := d.Rename("../x", "y"); err == nil {
		t.Error("Rename accepted a path")
	}
}
