package limits

import (
	"time"

	"kaiak/internal/accounting"
	"kaiak/internal/config"
	"kaiak/internal/state"
)

// The control-plane-mode limits state (docs/specs/GATEWAY.md, Limits → Control-plane
// mode: Restart keeps the last totals): the last applied totals — each scope's hour and
// month pushed base for the windows still current, by group (or global) and type,
// limited or not, the counted_through those bases include, and the live-gateway count —
// so a restart, above all one with the control plane down, keeps enforcing what was
// spent instead of counting from zero until totals arrive again. The gateway's own
// usage is not saved here: it is rebuilt from the usage spool (RestoreOwn), which keeps
// every batch until a save covering it has completed. Written in the background and at
// shutdown, never per request; restored at boot, before traffic. A cache: the control
// plane's next totals replace it.
const (
	SharedFile = "totals.json"
	// sharedVersion is the file's format version; a file with another is discarded.
	sharedVersion = 5
)

type sharedData struct {
	LiveGateways   int64          `json:"live_gateways"`
	CountedThrough []CountedBatch `json:"counted_through"`
	Windows        []savedShared  `json:"windows"`
}

// savedShared is one pushed base: its scope and type, and the control plane's used
// amount for the window starting at Start.
type savedShared struct {
	// Group is the group the count belongs to; "" (omitted) for global.
	Group string           `json:"group,omitempty"`
	Type  config.LimitType `json:"type"`
	Start time.Time        `json:"window_start"`
	Used  int64            `json:"used"`
}

// SavedShared is what SaveShared wrote.
type SavedShared struct {
	// Windows counts the bases written.
	Windows int
	// CountedThrough is the counted_through the written bases include: once the write
	// has completed, every spooled batch it covers is held by the file and can leave
	// the spool.
	CountedThrough []CountedBatch
}

// SaveShared writes the control-plane-mode limits state to dir. Nothing is written
// before totals were applied or restored: the bases are unknown until then.
func (l *Limiter) SaveShared(dir *state.Dir) (SavedShared, error) {
	l.mu.Lock()
	if !l.shared() || !l.totalsKnown {
		l.mu.Unlock()
		return SavedShared{}, nil
	}
	now := l.now()
	data := sharedData{LiveGateways: l.live, CountedThrough: append([]CountedBatch{}, l.countedThrough...),
		Windows: make([]savedShared, 0, len(l.pushed))}
	for k, w := range l.pushed {
		kind, _ := shape(k.typ)
		if w.Start.Before(windowStart(kind, now)) {
			continue // ended: no longer counted anywhere
		}
		data.Windows = append(data.Windows, savedShared{Group: k.group, Type: k.typ, Start: w.Start.UTC(), Used: w.Used})
	}
	l.mu.Unlock()
	if err := dir.WriteVersioned(SharedFile, sharedVersion, data); err != nil {
		return SavedShared{}, err
	}
	return SavedShared{Windows: len(data.Windows), CountedThrough: data.CountedThrough}, nil
}

// SharedRestore is what LoadShared restored.
type SharedRestore struct {
	// Found: a file of the current format was read.
	Found bool
	// Discarded names why the whole file was not used ("" when it was): not in
	// control-plane mode, or no config in force.
	Discarded string
	// Restored and Dropped count the bases restored and those whose window has ended.
	Restored, Dropped int
	// CountedThrough is the counted_through the restored bases include: the spooled
	// batches it covers are inside them, the others are rebuilt (RestoreOwn).
	CountedThrough []CountedBatch
}

// LoadShared restores the control-plane-mode limits state from dir, before traffic
// starts: the pushed bases still current, matched to the counts by group (or global)
// and type as totals are, and the live-gateway count. The spend is known from then on
// (noTotalsLocked).
func (l *Limiter) LoadShared(dir *state.Dir) (SharedRestore, error) {
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
	}
	now := l.now()
	l.live = max(data.LiveGateways, 1)
	for _, saved := range data.Windows {
		kind, _ := shape(saved.Type)
		if saved.Start.Before(windowStart(kind, now)) {
			out.Dropped++
			continue
		}
		l.pushed[keyOf(saved.Group, saved.Type)] = PushedWindow(saved)
		out.Restored++
	}
	for _, c := range l.counters {
		l.applyLimit(c)
	}
	l.countedThrough = data.CountedThrough
	out.CountedThrough = data.CountedThrough
	l.totalsKnown = true
	return out, nil
}

// OwnBatch is a usage batch the spool kept and the restored bases do not include: its
// usage generation and its records.
type OwnBatch struct {
	Generation uint64
	Records    []accounting.UsageRecord
}

// RestoreOwn rebuilds the own usage of batches restored from the usage spool, before
// traffic starts: each record counts on the hour and month counts of global and every
// group on its path, in its own window when that is still the current one, as usage of
// its batch's generation — as if just settled. A scope the booted config no longer has
// keeps the usage on a retained count. Control-plane mode only.
func (l *Limiter) RestoreOwn(batches []OwnBatch) (records int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sync()
	if !l.shared() {
		return 0
	}
	now := l.now()
	for _, b := range batches {
		for _, rec := range b.Records {
			records++
			for _, group := range append([]string{""}, rec.Groups...) {
				for _, typ := range countedTypes {
					c := l.countOf(group, typ)
					c.w.roll(now)
					if !windowStart(c.w.kind, rec.GatewayTime).Equal(time.Unix(c.w.start, 0)) {
						continue
					}
					l.addOwnLocked(c, now, amountOf(c.measure, rec), b.Generation)
				}
			}
		}
	}
	return records
}
