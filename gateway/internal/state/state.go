// Package state owns the files in the gateway's data directory, an opt-in: with none
// configured nothing opens a Dir and nothing is written. Every file carries a format
// version; a file with a different version is discarded (and the discard logged),
// never migrated. Files are cache and snapshots, never the record: losing the
// directory loses nothing authoritative.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"kaiak/internal/schemacheck"
)

// LockFile is the file in the data directory a gateway holds an exclusive lock on
// for as long as it has the directory open: two gateways writing one spool or one
// last-known-good copy would corrupt each other's state. The file stays when the
// lock is released; its presence means nothing, only the lock does.
const LockFile = "kaiak.lock"

// errLocked: another process (or another Open) holds the directory's lock.
var errLocked = errors.New("lock held")

// Dir is the data directory.
type Dir struct {
	path   string
	logger *slog.Logger
	// lock is the open lock file; closing it releases the lock.
	lock *os.File
}

// Open returns the data directory at path, creating it if missing, and takes its
// lock until Close. A directory another gateway holds is an error naming the lock
// file.
func Open(path string, logger *slog.Logger) (*Dir, error) {
	if err := os.MkdirAll(path, 0o750); err != nil {
		return nil, fmt.Errorf("create data directory %s: %w", path, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("data directory %s: %w", path, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("data directory %s is not a directory", path)
	}
	lockPath := filepath.Join(path, LockFile)
	lock, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file %s: %w", lockPath, err)
	}
	if err := lockFile(lock); err != nil {
		_ = lock.Close() // the lock error is the one to report
		if errors.Is(err, errLocked) {
			return nil, fmt.Errorf("data directory %s is in use by another kaiak process (it holds the lock on %s); "+
				"give each gateway its own data directory", path, lockPath)
		}
		return nil, fmt.Errorf("lock %s: %w", lockPath, err)
	}
	return &Dir{path: path, logger: logger, lock: lock}, nil
}

// Close releases the directory's lock. The Dir is not used after it.
func (d *Dir) Close() error {
	if err := d.lock.Close(); err != nil {
		return fmt.Errorf("release lock file: %w", err)
	}
	return nil
}

// Path returns the directory's path.
func (d *Dir) Path() string { return d.path }

// versioned is the on-disk envelope of every versioned file.
type versioned struct {
	FormatVersion int             `json:"format_version"`
	Data          json.RawMessage `json:"data"`
}

// WriteVersioned replaces the file name with data (JSON-encoded) stamped with version.
// The write is atomic: a temporary file in the same directory is synced, then renamed
// over the old one, so a crash leaves the old file or the new one, never a torn one.
func (d *Dir) WriteVersioned(name string, version int, data any) error {
	path, err := d.file(name)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("encode %s: %w", name, err)
	}
	encoded, err := json.Marshal(versioned{FormatVersion: version, Data: payload})
	if err != nil {
		return fmt.Errorf("encode %s: %w", name, err)
	}

	tmp, err := os.CreateTemp(d.path, "."+name+".*.tmp")
	if err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}
	committed := false
	defer func() {
		if !committed {
			// Best effort: the temporary file is garbage once the write failed, and the
			// write's own error is the one worth reporting.
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()
	if _, err := tmp.Write(encoded); err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync %s: %w", name, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("replace %s: %w", name, err)
	}
	committed = true
	return d.syncDir()
}

// ReadVersioned decodes the file name into into when its format version is version,
// and reports whether it did. A missing file reports false. A file with another
// version is removed, logged, and reports false. A file that is not a versioned
// envelope, repeats an object member anywhere, or whose data does not decode into
// into, is an error: the caller decides
// whether to start without it.
func (d *Dir) ReadVersioned(name string, version int, into any) (bool, error) {
	path, err := d.file(name)
	if err != nil {
		return false, err
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read %s: %w", name, err)
	}
	// A repeated member would decode as whichever occurrence encoding/json keeps (or
	// merge, into a map); the gateway writes none, so one means the file was edited.
	if path, found := schemacheck.FirstDuplicateMember(raw); found {
		return false, fmt.Errorf("read %s: member %s appears more than once in its object", name, path)
	}
	var envelope versioned
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return false, fmt.Errorf("read %s: %w", name, err)
	}
	if envelope.FormatVersion != version {
		if err := os.Remove(path); err != nil {
			return false, fmt.Errorf("discard %s: %w", name, err)
		}
		d.logger.Warn("discarded data file with a different format version",
			"file", path, "found_version", envelope.FormatVersion, "want_version", version)
		return false, nil
	}
	if err := json.Unmarshal(envelope.Data, into); err != nil {
		return false, fmt.Errorf("decode %s: %w", name, err)
	}
	return true, nil
}

// File is one file of the directory, as List reports it.
type File struct {
	Name    string
	ModTime time.Time
}

// List returns the regular files whose names start with prefix, sorted by name.
// Temporary files of writes in progress start with "." and match no prefix that does
// not.
func (d *Dir) List(prefix string) ([]File, error) {
	entries, err := os.ReadDir(d.path)
	if err != nil {
		return nil, fmt.Errorf("list data directory: %w", err)
	}
	var files []File
	for _, e := range entries {
		if !e.Type().IsRegular() || !strings.HasPrefix(e.Name(), prefix) {
			continue
		}
		info, err := e.Info()
		if errors.Is(err, fs.ErrNotExist) {
			continue // removed since the listing: not a file of the directory any more
		}
		if err != nil {
			return nil, fmt.Errorf("list data directory: %w", err)
		}
		files = append(files, File{Name: e.Name(), ModTime: info.ModTime()})
	}
	slices.SortFunc(files, func(a, b File) int { return strings.Compare(a.Name, b.Name) })
	return files, nil
}

// Remove deletes the file name; a missing file is not an error. The removal is made
// durable before Remove returns.
func (d *Dir) Remove(name string) error {
	path, err := d.file(name)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", name, err)
	}
	return d.syncDir()
}

// Rename moves the file from to the name to, replacing any file there, atomically and
// durably.
func (d *Dir) Rename(from, to string) error {
	src, err := d.file(from)
	if err != nil {
		return err
	}
	dst, err := d.file(to)
	if err != nil {
		return err
	}
	if err := os.Rename(src, dst); err != nil {
		return fmt.Errorf("rename %s to %s: %w", from, to, err)
	}
	return d.syncDir()
}

// file resolves name to a path inside the directory; name is a plain file name.
func (d *Dir) file(name string) (string, error) {
	if name == "" || name != filepath.Base(name) || name == "." || name == ".." {
		return "", fmt.Errorf("data file name %q is not a plain file name", name)
	}
	return filepath.Join(d.path, name), nil
}

// syncDir makes the rename durable.
func (d *Dir) syncDir() error {
	dir, err := os.Open(d.path)
	if err != nil {
		return fmt.Errorf("sync data directory: %w", err)
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return fmt.Errorf("sync data directory: %w", err)
	}
	return nil
}
