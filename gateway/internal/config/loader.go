package config

import (
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"slices"
	"sync"
	"time"

	"kaiak/internal/schemacheck"
)

// LoadObserver is told the result of every config load: its trigger, whether the
// config was applied, and when (the ops metrics).
type LoadObserver func(trigger string, applied bool, at time.Time)

// Applier is the one path by which a config document becomes the live snapshot,
// whatever its source (config file, control plane, last-known-good copy): validate it
// completely, swap it into the Holder atomically, log the result with its trigger and
// report it to the observer. Sources read their documents and hand them over; they
// never swap the Holder themselves.
type Applier struct {
	holder    *Holder
	logger    *slog.Logger
	lookupEnv func(string) (string, bool)
	observe   LoadObserver
	// mu serializes loads, so two triggers never race to swap.
	mu sync.Mutex
}

// NewApplier returns the apply path into holder. lookupEnv is how it reads the process
// environment (os.LookupEnv outside tests); observe may be nil.
func NewApplier(holder *Holder, logger *slog.Logger, lookupEnv func(string) (string, bool), observe LoadObserver) *Applier {
	return &Applier{holder: holder, logger: logger, lookupEnv: lookupEnv, observe: observe}
}

// Apply validates data and swaps it in. A config that fails anywhere is logged with
// its issue codes and not applied; the running snapshot, if any, stays, and the error
// is a *ValidationError. trigger names what asked for the load; attrs describe the
// source (file, config version) for both log lines.
func (a *Applier) Apply(trigger string, data []byte, attrs ...any) (*Snapshot, error) {
	return a.apply(trigger, data, Version{}, attrs)
}

// ApplyPublished is Apply for a config a control plane published as version v: the
// snapshot carries v (Snapshot.Version).
func (a *Applier) ApplyPublished(trigger string, data []byte, v Version, attrs ...any) (*Snapshot, error) {
	return a.apply(trigger, data, v, attrs)
}

func (a *Applier) apply(trigger string, data []byte, v Version, attrs []any) (*Snapshot, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	snapshot, err := Check(data, a.lookupEnv)
	if err != nil {
		a.reject(trigger, err, attrs)
		return nil, err
	}
	snapshot.Version = v
	a.holder.Swap(snapshot)
	a.logger.Info("config applied", append(append([]any{"trigger", trigger}, attrs...),
		"backends", len(snapshot.Backends), "models", len(snapshot.Models), "keys", len(snapshot.Keys))...)
	a.report(trigger, true)
	return snapshot, nil
}

// Loaded reports whether a config is in force.
func (a *Applier) Loaded() bool { return a.holder.Loaded() }

// Check validates data completely, as the apply path does before a swap — syntax,
// schema, semantic rules and backend credentials (lookupEnv) — without applying it.
func Check(data []byte, lookupEnv func(string) (string, bool)) (*Snapshot, error) {
	snapshot, err := Parse(data)
	if err != nil {
		return nil, err
	}
	if err := checkCredentials(snapshot, lookupEnv); err != nil {
		return nil, err
	}
	return snapshot, nil
}

// Reject records a load that failed before there was a document to validate (the
// file could not be read): logged and counted like a rejected config. It returns err.
func (a *Applier) Reject(trigger string, err error, attrs ...any) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.reject(trigger, err, attrs)
	return err
}

// reject logs and counts a failed load. Callers hold a.mu.
func (a *Applier) reject(trigger string, err error, attrs []any) {
	logAttrs := append(append([]any{"trigger", trigger}, attrs...), "error", err)
	var invalid *ValidationError
	if errors.As(err, &invalid) {
		logAttrs = append(logAttrs, "codes", invalid.Codes())
	}
	if a.holder.Loaded() {
		logAttrs = append(logAttrs, "running_config", "kept")
	}
	a.logger.Error("config rejected", logAttrs...)
	a.report(trigger, false)
}

func (a *Applier) report(trigger string, applied bool) {
	if a.observe != nil {
		a.observe(trigger, applied, time.Now())
	}
}

// FileLoader loads the config document from a file (file mode). Load is the
// mechanism; its triggers (startup, SIGHUP) are wired by the caller and name
// themselves in the log (CODING-RULES §7).
type FileLoader struct {
	path    string
	applier *Applier
}

// NewFileLoader returns a loader for the config file at path, applying through
// applier.
func NewFileLoader(path string, applier *Applier) *FileLoader {
	return &FileLoader{path: path, applier: applier}
}

// Load reads the config file and applies it. A file that cannot be read or fails
// validation is logged and not applied; the running snapshot, if any, stays. trigger
// names what asked for the load, for the log.
func (l *FileLoader) Load(trigger string) error {
	data, err := os.ReadFile(l.path)
	if err != nil {
		return fmt.Errorf("load config %s: %w", l.path, l.applier.Reject(trigger, err, "file", l.path))
	}
	if _, err := l.applier.Apply(trigger, data, "file", l.path); err != nil {
		return fmt.Errorf("load config %s: %w", l.path, err)
	}
	return nil
}

// checkCredentials rejects a config whose backends name API-key variables the process
// environment does not set: such a backend could never authenticate. Only presence is
// checked; the value is read by the provider that uses it.
func checkCredentials(s *Snapshot, lookupEnv func(string) (string, bool)) error {
	var issues []Issue
	for _, id := range slices.Sorted(maps.Keys(s.Backends)) {
		name := s.Backends[id].APIKeyEnv
		if name == "" {
			continue
		}
		if value, ok := lookupEnv(name); !ok || value == "" {
			issues = append(issues, Issue{
				Code:    CodeAPIKeyEnvUnset,
				Path:    schemacheck.Pointer("/backends", id, "api_key_env"),
				Message: fmt.Sprintf("environment variable %s is not set", name),
			})
		}
	}
	if len(issues) > 0 {
		return &ValidationError{Issues: issues}
	}
	return nil
}
