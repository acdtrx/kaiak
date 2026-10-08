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

	"kaiak/internal/logattr"
	"kaiak/internal/schemacheck"
)

// Trigger names what asked for a config load, in the log (kaiak.trigger) and the
// metrics. Every source of config documents loads under its own trigger.
type Trigger string

const (
	// TriggerStartup: the file loader's first load, at start (file mode).
	TriggerStartup Trigger = "startup"
	// TriggerSIGHUP: the file loader's reload on SIGHUP (file mode).
	TriggerSIGHUP Trigger = "sighup"
	// TriggerControl: a config from the control plane's config stream.
	TriggerControl Trigger = "control"
	// TriggerSeed: the seed config, applied when the control plane is unavailable at
	// boot.
	TriggerSeed Trigger = "seed"
)

// Triggers is every config load trigger.
var Triggers = []Trigger{TriggerStartup, TriggerSIGHUP, TriggerControl, TriggerSeed}

// LoadResult is what came of a config load.
type LoadResult string

const (
	LoadApplied  LoadResult = "applied"
	LoadRejected LoadResult = "rejected"
)

// LoadResults is every config load result.
var LoadResults = []LoadResult{LoadApplied, LoadRejected}

// Load is the notice of one config load, applied or rejected, that the Applier hands
// its onLoad: the applied-config notification. Whatever follows the config in force —
// routing's caps, connection pools, the model check, the ops metrics — takes an
// applied config from Snapshot here.
type Load struct {
	// Trigger names what asked for the load; At is when the load ended.
	Trigger Trigger
	At      time.Time
	// Snapshot is the config swapped in; nil when the load was rejected.
	Snapshot *Snapshot
	// Document: there was a document to validate — false for a load that failed
	// before (Reject). Only then are Bytes, the document's size, and Duration, the time
	// from the start of its validation to the swap or the rejection, set.
	Document bool
	Bytes    int
	Duration time.Duration
}

// Applied reports whether the load swapped its config in.
func (l Load) Applied() bool { return l.Snapshot != nil }

// Result is what came of the load: applied or rejected.
func (l Load) Result() LoadResult {
	if l.Applied() {
		return LoadApplied
	}
	return LoadRejected
}

// Applier is the one path by which a config document becomes the live snapshot,
// whatever its source (config file, control plane, seed config): validate it
// completely, swap it into the Holder atomically, log the result with its trigger and
// hand its Load to onLoad. Sources read their documents and hand them over; they never
// swap the Holder themselves.
type Applier struct {
	holder    *Holder
	logger    *slog.Logger
	lookupEnv func(string) (string, bool)
	onLoad    func(Load)
	// mu serializes loads, so two triggers never race to swap, and onLoad hears them
	// one at a time, in the order they were swapped in.
	mu sync.Mutex
}

// NewApplier returns the apply path into holder. lookupEnv is how it reads the process
// environment (os.LookupEnv outside tests); onLoad, told of every load, may be nil.
func NewApplier(holder *Holder, logger *slog.Logger, lookupEnv func(string) (string, bool), onLoad func(Load)) *Applier {
	return &Applier{holder: holder, logger: logger, lookupEnv: lookupEnv, onLoad: onLoad}
}

// Apply validates data and swaps it in. A config that fails anywhere is logged with
// its issue codes and not applied; the running snapshot, if any, stays, and the error
// is a *schemacheck.ValidationError. trigger names what asked for the load; attrs describe the
// source (file, config hash) for both log lines.
func (a *Applier) Apply(trigger Trigger, data []byte, attrs ...any) (*Snapshot, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	// The clock starts once the lock is held: a load waiting for another is not
	// slower for it.
	start := time.Now()
	snapshot, err := Check(data, a.lookupEnv)
	if err != nil {
		a.reject(Load{Trigger: trigger, Document: true, Bytes: len(data), Duration: time.Since(start)}, err, attrs)
		return nil, err
	}
	a.holder.Swap(snapshot)
	load := Load{Trigger: trigger, Snapshot: snapshot, Document: true, Bytes: len(data), Duration: time.Since(start)}
	a.logger.Info("config applied", append(append([]any{"kaiak.trigger", string(trigger)}, attrs...),
		"kaiak.config.backends", len(snapshot.Backends), "kaiak.config.models", len(snapshot.Models),
		"kaiak.config.keys", len(snapshot.keysByHash), "kaiak.config.size", load.Bytes,
		logattr.SecondsMicro("kaiak.duration", load.Duration))...)
	a.report(load)
	return snapshot, nil
}

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
func (a *Applier) Reject(trigger Trigger, err error, attrs ...any) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.reject(Load{Trigger: trigger}, err, attrs)
	return err
}

// reject logs and counts a failed load. Callers hold a.mu.
func (a *Applier) reject(load Load, err error, attrs []any) {
	logAttrs := append(append([]any{"kaiak.trigger", string(load.Trigger)}, attrs...), "exception.message", err)
	if invalid, ok := errors.AsType[*schemacheck.ValidationError](err); ok {
		logAttrs = append(logAttrs, "kaiak.config.issue_codes", invalid.Codes())
	}
	if a.holder.Loaded() {
		logAttrs = append(logAttrs, "kaiak.config.running", "kept")
	}
	if load.Document {
		logAttrs = append(logAttrs, "kaiak.config.size", load.Bytes, logattr.SecondsMicro("kaiak.duration", load.Duration))
	}
	a.logger.Error("config rejected", logAttrs...)
	a.report(load)
}

func (a *Applier) report(load Load) {
	if a.onLoad != nil {
		load.At = time.Now()
		a.onLoad(load)
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
func (l *FileLoader) Load(trigger Trigger) error {
	data, err := os.ReadFile(l.path)
	if err != nil {
		return fmt.Errorf("load config %s: %w", l.path, l.applier.Reject(trigger, err, "file.path", l.path))
	}
	if _, err := l.applier.Apply(trigger, data, "file.path", l.path); err != nil {
		return fmt.Errorf("load config %s: %w", l.path, err)
	}
	return nil
}

// checkCredentials rejects a config whose backends name API-key variables the process
// environment does not set: such a backend could never authenticate. Only presence is
// checked; the value is read by the provider that uses it.
func checkCredentials(s *Snapshot, lookupEnv func(string) (string, bool)) error {
	var issues []schemacheck.Issue
	for _, id := range slices.Sorted(maps.Keys(s.Backends)) {
		name := s.Backends[id].APIKeyEnv
		if name == "" {
			continue
		}
		if value, ok := lookupEnv(name); !ok || value == "" {
			issues = append(issues, schemacheck.Issue{
				Code:    CodeAPIKeyEnvUnset,
				Path:    schemacheck.Pointer("/backends", id, "api_key_env"),
				Message: fmt.Sprintf("environment variable %s is not set", name),
			})
		}
	}
	if len(issues) > 0 {
		return &schemacheck.ValidationError{Subject: rejectedSubject, Issues: issues}
	}
	return nil
}
