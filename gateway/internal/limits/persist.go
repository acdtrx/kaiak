package limits

import (
	"time"

	"kaiak/internal/config"
	"kaiak/internal/state"
)

// The control-plane-mode limits state (docs/specs/GATEWAY.md, Limits → Control-plane
// mode: Restart): each hour and month counter's pushed base and the usage the control
// plane had not counted yet, with the config they were matched to, so a restart —
// above all one with the control plane down — keeps enforcing what was spent instead
// of counting from zero until totals arrive again. Written when totals are applied and
// at shutdown, never per request; restored at boot, before traffic. A cache: the
// control plane's next totals replace it.
const (
	SharedFile = "totals.json"
	// sharedVersion is the file's format version; a file with another is discarded.
	sharedVersion = 2
)

type sharedData struct {
	// ConfigEpoch and ConfigVersion identify the config the counters were matched
	// to; a file of another config is discarded.
	ConfigEpoch   string        `json:"config_epoch"`
	ConfigVersion int64         `json:"config_version"`
	LiveGateways  int64         `json:"live_gateways"`
	Windows       []savedShared `json:"windows"`
}

// savedShared is one hour or month counter: its limit identity, the control plane's
// used amount for the window starting at BaseStart (zero time: none), and the usage
// not yet counted there, settled in the window starting at Start.
type savedShared struct {
	// Group is the group the limit belongs to; "" (omitted) for a global limit.
	Group     string           `json:"group,omitempty"`
	Type      config.LimitType `json:"type"`
	Models    []string         `json:"models"`
	BaseStart time.Time        `json:"base_window_start"`
	Base      int64            `json:"base"`
	Start     time.Time        `json:"window_start"`
	Uncounted int64            `json:"uncounted"`
}

// SaveShared writes the control-plane-mode limits state to dir and returns how many
// counters it holds. Unsettled reservations are left out, as in the file-mode
// snapshot. Nothing is written before a config is in force.
func (l *Limiter) SaveShared(dir *state.Dir) (int, error) {
	l.mu.Lock()
	l.sync()
	if !l.shared() || l.applied == nil {
		l.mu.Unlock()
		return 0, nil
	}
	now := l.now()
	data := sharedData{ConfigEpoch: l.applied.Version.Epoch, ConfigVersion: l.applied.Version.Number,
		LiveGateways: l.live, Windows: []savedShared{}}
	for _, c := range l.counters {
		if !c.w.shared {
			continue
		}
		c.w.roll(now)
		var uncounted int64
		for _, g := range c.w.local {
			uncounted += g.n
		}
		if c.w.base == 0 && uncounted == 0 {
			continue
		}
		saved := savedShared{Group: c.key.group, Type: c.limit.Type, Models: c.limit.Models,
			Start: time.Unix(c.w.start, 0).UTC(), Uncounted: uncounted}
		if c.w.base != 0 {
			saved.BaseStart, saved.Base = time.Unix(c.w.baseStart, 0).UTC(), c.w.base
		}
		data.Windows = append(data.Windows, saved)
	}
	l.mu.Unlock()
	return len(data.Windows), dir.WriteVersioned(SharedFile, sharedVersion, data)
}

// SharedRestore is what LoadShared restored.
type SharedRestore struct {
	// Found: a file of the current format was read.
	Found bool
	// Discarded names why the whole file was not used ("" when it was): no config in
	// force, or the file belongs to another config than the one booted.
	Discarded string
	// Restored and Dropped count the counters restored and those whose limit no
	// longer exists.
	Restored, Dropped int
}

// LoadShared restores the control-plane-mode limits state from dir, before traffic
// starts: each counter's pushed base, and its uncounted usage when that was settled
// in the current window. The uncounted usage is tagged with restoredGeneration, the
// newest usage generation the control client gave the batches it restored from its
// spool — the batches that usage travels in — so it leaves once they are counted; 0
// (no batches restored: they were acknowledged, or lost) lets it leave with the
// first totals applied. A file whose config is not the applied one is discarded: its
// bases describe other limits.
func (l *Limiter) LoadShared(dir *state.Dir, restoredGeneration uint64) (SharedRestore, error) {
	var data sharedData
	found, err := dir.ReadVersioned(SharedFile, sharedVersion, &data)
	if err != nil || !found {
		return SharedRestore{}, err
	}
	out := SharedRestore{Found: true}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sync()
	switch {
	case !l.shared():
		out.Discarded = "not in control-plane mode"
		return out, nil
	case l.applied == nil:
		out.Discarded = "no config in force"
		return out, nil
	case l.applied.Version != (config.Version{Epoch: data.ConfigEpoch, Number: data.ConfigVersion}):
		out.Discarded = "another config"
		return out, nil
	}
	now := l.now()
	l.live = max(data.LiveGateways, 1)
	for _, saved := range data.Windows {
		key := keyOf(saved.Group, config.Limit{Type: saved.Type, Models: saved.Models})
		c, ok := l.counters[key]
		if !ok || !c.w.shared {
			out.Dropped++
			continue
		}
		if !saved.BaseStart.IsZero() {
			l.pushed[key] = PushedWindow{Group: saved.Group, Type: saved.Type, Models: saved.Models,
				Start: saved.BaseStart, Used: saved.Base}
		}
		l.applyLimit(c)
		c.w.roll(now)
		if saved.Uncounted > 0 && saved.Start.Unix() == c.w.start {
			c.w.used += saved.Uncounted
			c.w.addLocal(restoredGeneration, saved.Uncounted)
		}
		out.Restored++
	}
	for _, c := range l.counters {
		l.applyLimit(c)
	}
	l.restoredUntagged = restoredGeneration == 0
	l.totalsKnown = true
	return out, nil
}
