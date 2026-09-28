package state

import (
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
)

// M11: one gateway per data directory. A second Open of a directory another holds
// fails, naming the lock file; once the holder closes it, the directory opens again.
func TestSecondOpenOfADirectoryInUseFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data")
	first, err := Open(path, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	_, err = Open(path, slog.New(slog.DiscardHandler))
	if err == nil {
		t.Fatal("a second Open of a directory in use succeeded")
	}
	if want := filepath.Join(path, LockFile); !strings.Contains(err.Error(), want) {
		t.Errorf("error %q does not name the lock file %s", err, want)
	}

	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	again, err := Open(path, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("Open after the holder closed: %v", err)
	}
	if err := again.Close(); err != nil {
		t.Fatal(err)
	}
}
