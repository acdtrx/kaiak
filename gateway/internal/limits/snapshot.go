package limits

import (
	"time"

	"kaiak/internal/config"
	"kaiak/internal/state"
)

// The file-mode usage snapshot (docs/specs/GATEWAY.md, Limits): the settled usage of
// every hour and month window, so a restart does not reset them. Per-minute windows
// are not kept: a restart costs at most one minute of their history. File mode only:
// in control-plane mode the control plane holds these totals.
const (
	SnapshotFile = "limits.json"
	// snapshotVersion is the file's format version; a file with another is discarded.
	snapshotVersion = 3
	// SnapshotInterval is how often the running gateway writes the snapshot; it is
	// also written on shutdown.
	SnapshotInterval = 30 * time.Second
)

type snapshotData struct {
	Windows []savedWindow `json:"windows"`
}

// savedWindow is one counter's window: which limit it belongs to (the same identity
// a config reload matches on), when the window started and what was settled in it.
type savedWindow struct {
	// Group is the group the limit belongs to; "" (omitted) for a global limit.
	Group string           `json:"group,omitempty"`
	Type  config.LimitType `json:"type"`
	Start time.Time        `json:"window_start"`
	Used  int64            `json:"used"`
}

// SaveSnapshot writes the settled usage of every hour and month window that holds
// any to dir, and returns how many windows it wrote. Unsettled reservations are left
// out: the requests holding them settle before a clean shutdown, and after a crash
// they never will.
func (l *Limiter) SaveSnapshot(dir *state.Dir) (int, error) {
	l.mu.Lock()
	l.sync()
	now := l.now()
	data := snapshotData{Windows: []savedWindow{}}
	for _, c := range l.counters {
		if c.w.kind == SlidingMinute {
			continue
		}
		used := c.w.settled(now)
		if used == 0 {
			continue
		}
		data.Windows = append(data.Windows, savedWindow{Group: c.key.group, Type: c.limit.Type,
			Start: time.Unix(c.w.start, 0).UTC(), Used: used})
	}
	l.mu.Unlock()
	return len(data.Windows), dir.WriteVersioned(SnapshotFile, snapshotVersion, data)
}

// LoadSnapshot restores hour and month windows from dir, before traffic starts. A
// saved window is restored when its limit still exists in the live config and its
// window is the current one; the others are dropped. A missing file, or one with
// another format version (discarded by state and logged), restores nothing.
func (l *Limiter) LoadSnapshot(dir *state.Dir) (restored, dropped int, err error) {
	var data snapshotData
	ok, err := dir.ReadVersioned(SnapshotFile, snapshotVersion, &data)
	if err != nil || !ok {
		return 0, 0, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sync()
	now := l.now()
	for _, saved := range data.Windows {
		c, ok := l.counters[keyOf(saved.Group, config.Limit{Type: saved.Type})]
		if !ok || c.w.kind == SlidingMinute || !saved.Start.Equal(windowStart(c.w.kind, now)) {
			dropped++
			continue
		}
		c.w.roll(now)
		c.w.used = c.w.held + saved.Used
		restored++
	}
	return restored, dropped, nil
}
