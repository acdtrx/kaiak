package limits

import (
	"time"

	"kaiak/internal/config"
	"kaiak/internal/logattr"
)

// Control-plane mode (docs/specs/GATEWAY.md, Limits → Control-plane mode). The hour
// and month windows count the control plane's totals — pushed on the config stream —
// plus this gateway's own usage the control plane has not counted yet. That usage is
// tagged by usage generation: each record carries the generation of the usage batch
// the control client sealed it in, and TakeTotals drops every generation the control
// plane has counted, in the same step as it adopts the totals that include it, so
// nothing is counted twice or dropped in between.
//
// Totals apply whatever config the gateway runs: windows are counted per scope and
// type whatever the config, so a window means the same under every config, and each
// one is matched to the count of its group (or global) and type. The first totals of
// each stream connection are complete; the others list only the windows that changed,
// and a window not listed keeps its base.

// Totals are the control plane's totals as the limiter takes them.
type Totals struct {
	// LiveGateways is the live-gateway count per-minute shares divide by; 0 counts
	// as 1.
	LiveGateways int64
	// Complete: Windows lists every scope and type with usage — a count with no window
	// has used nothing in the control plane's current window. Otherwise Windows lists
	// only the ones that changed, and every other count keeps its base.
	Complete bool
	Windows  []PushedWindow
}

// PushedWindow is one scope's current window of one type as the control plane counts
// it, identified as a reload identifies a count.
type PushedWindow struct {
	// Group is the group the count belongs to; "" for global.
	Group string
	Type  config.LimitType
	Start time.Time
	// Used counts tokens or nano-USD, as the counter does.
	Used int64
}

// TakeTotals takes one totals message, in one step under the limiter's lock: its
// live-gateway count applies at once (per-minute shares keep what they counted), and
// its windows become the bases of the hour and month counts of their scope and type —
// complete totals replace every base (0 without a window), others only those they
// list. Windows of scopes the config does not have are kept for a reload that adds
// them, until their window has passed. counted, when not 0, is the newest usage
// generation the message shows counted: it leaves the counters' own usage now, as the
// totals that include it are applied. A changes-only message touches only the windows
// it lists, the per-minute shares when the live count changed, and the counters
// holding own usage.
func (l *Limiter) TakeTotals(t Totals, counted uint64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sync()
	now := l.now()
	if l.totalsAt.IsZero() {
		close(l.firstTotals)
	}
	live := max(t.LiveGateways, 1)
	reshare := live != l.live
	l.live = live
	l.totalsAt = now
	if t.Complete {
		l.pushed = make(map[counterKey]PushedWindow, len(t.Windows))
	}
	for _, w := range t.Windows {
		l.pushed[keyOf(w.Group, w.Type)] = w
		l.warnAheadLocked(w, now)
	}
	if t.Complete {
		for _, c := range l.counters {
			l.applyLimit(c)
		}
	} else {
		if reshare {
			for _, c := range l.minute {
				l.applyLimit(c)
			}
		}
		for _, w := range t.Windows {
			if c, ok := l.counters[keyOf(w.Group, w.Type)]; ok {
				l.applyLimit(c)
			}
		}
	}
	l.retireCountedLocked(counted)
	l.pruneRetainedLocked(now)
	l.warnSmallSharesLocked()
}

// prunePushedLocked drops the pushed windows that have ended: a changes-only message
// never lists a window again once its hour or month is over, so without this a
// long-lived stream would keep every scope it ever saw. Callers hold l.mu.
func (l *Limiter) prunePushedLocked(now time.Time) {
	for k, w := range l.pushed {
		if w.Start.Before(windowStart(kindOf(k.typ.Window()), now)) {
			delete(l.pushed, k)
		}
	}
}

// warnSmallSharesLocked logs, once per applied config and live-gateway count, each
// per-minute token limit whose share is below the default output of a model its scope
// may use (a group's allowed models; every model for a global limit): output default
// × live gateways > limit. Such a request is admitted only while this gateway's window
// for the limit is empty (counter.admits), so the limit works as about one request a
// minute per gateway. Callers hold l.mu.
func (l *Limiter) warnSmallSharesLocked() {
	if l.applied == nil || (l.applied == l.sharesChecked.snapshot && l.live == l.sharesChecked.live) {
		return
	}
	l.sharesChecked.snapshot, l.sharesChecked.live = l.applied, l.live
	for _, c := range l.counters {
		if c.key.typ.Counted() || c.measure != config.MeasureTokens || c.w.limit >= effectiveLimit(c.limit) {
			continue
		}
		group := l.applied.Groups[c.key.group]
		for _, name := range l.applied.ModelNames {
			m := l.applied.Models[name]
			if m.OutputLimit == nil || m.OutputLimit.Default <= c.w.limit {
				continue
			}
			if group != nil && !group.AllowedModels.Allows(name) {
				continue
			}
			l.logger.Warn("per-minute share below the model's default output: a request at the default is admitted only while this gateway's window is empty",
				append(identityAttrs(c.key.group), "kaiak.model.name", name,
					"kaiak.limit.configured", effectiveLimit(c.limit), "kaiak.limit.live_gateways", l.live,
					"kaiak.limit.enforced", c.w.limit, "kaiak.model.output_default", m.OutputLimit.Default)...)
		}
	}
}

// aheadMargin is how far ahead of the gateway's clock a pushed window may start
// before it is logged: a control plane whose clock leads by seconds pushes the next
// window a little early at every boundary, which is harmless.
const aheadMargin = time.Minute

// warnAheadLocked logs a pushed window starting more than aheadMargin ahead of the
// gateway's clock — the control plane's clock runs ahead, and the window follows it
// until a push names the gateway's own window again (window.setBase). Once per window
// start. Callers hold l.mu.
func (l *Limiter) warnAheadLocked(w PushedWindow, now time.Time) {
	if !w.Start.After(now.Add(aheadMargin)) || w.Start.Equal(l.aheadWarned) {
		return
	}
	l.aheadWarned = w.Start
	l.logger.Warn("pushed window ahead of the gateway's clock", append(identityAttrs(w.Group),
		"kaiak.limit.type", w.Type, "kaiak.limit.window_start", w.Start.UTC(), "kaiak.gateway_time", now.UTC())...)
}

// retireCountedLocked drops the generations up to counted from the counters' own
// usage: the totals just applied include them. Callers hold l.mu.
func (l *Limiter) retireCountedLocked(counted uint64) {
	if counted <= l.counted {
		return
	}
	l.counted = counted
	for c := range l.owning {
		c.w.counted(l.counted)
		l.checkCountLocked(c)
		if len(c.w.local) == 0 {
			delete(l.owning, c)
		}
	}
}

// countedLocked reports whether usage of generation is inside the applied bases: its
// batch was shown counted and dropped from the own usage. Callers hold l.mu.
func (l *Limiter) countedLocked(generation uint64) bool {
	return l.counted != 0 && generation <= l.counted
}

// TotalsAppliedAt is when totals were last applied; false before any.
func (l *Limiter) TotalsAppliedAt() (time.Time, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.totalsAt, !l.totalsAt.IsZero()
}

// Outage reports whether the control plane has been out of reach for longer than
// the live config's outage grace (global.control_outage_grace_ms): no config stream
// open and no contact since — or usage waiting that long, an open stream
// notwithstanding: for an answer (a control plane that serves config but no longer
// takes usage cannot count this gateway's spend), or acknowledged but not yet shown
// counted by applied totals (totals that stopped coming leave the bases frozen).
// Always false in file mode.
func (l *Limiter) Outage() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sync()
	return l.outageLocked(l.now())
}

// outageLocked is Outage at now. Nothing signals an outage's start — the grace runs
// out — so it is logged when first decided, by a request or a metrics scrape, and so
// is its end: one line each, never one per request. Callers hold l.mu and have
// synced.
func (l *Limiter) outageLocked(now time.Time) bool {
	if !l.shared() || l.applied == nil {
		return false
	}
	c, grace := l.contact(), l.applied.ControlOutageGrace
	var reason string
	var began, waiting time.Time // when the grace ran out; since when usage waited
	switch {
	case !c.Connected && now.Sub(c.Last) > grace:
		reason, began = "no contact", c.Last.Add(grace)
	case !c.UsageWaitingSince.IsZero() && now.Sub(c.UsageWaitingSince) > grace:
		reason, began, waiting = "usage not acknowledged", c.UsageWaitingSince.Add(grace), c.UsageWaitingSince
	case !c.UsageUncountedSince.IsZero() && now.Sub(c.UsageUncountedSince) > grace:
		reason, began, waiting = "usage not shown counted", c.UsageUncountedSince.Add(grace), c.UsageUncountedSince
	}
	switch {
	case reason != "" && l.outageSince.IsZero():
		l.outageSince = began
		attrs := []any{"kaiak.reason", reason, logattr.Seconds("kaiak.control.since_contact", now.Sub(c.Last)),
			logattr.Seconds("kaiak.control.outage_grace", grace)}
		if !waiting.IsZero() {
			attrs = append(attrs, logattr.Seconds("kaiak.control.usage_waiting", now.Sub(waiting)))
		}
		l.logger.Warn("control plane outage: priced USD-limited models refused", attrs...)
	case reason == "" && !l.outageSince.IsZero():
		l.logger.Info("control plane outage over: contact is back",
			logattr.Seconds("kaiak.lasted", now.Sub(l.outageSince)))
		l.outageSince = time.Time{}
	}
	return reason != ""
}

// FirstTotals is closed once the first totals message is taken: the control plane has
// said what was spent (docs/specs/GATEWAY.md, Control-plane mode → Readiness waits for
// the first totals). Never closed in file mode.
func (l *Limiter) FirstTotals() <-chan struct{} { return l.firstTotals }

// noTotalsLocked reports whether the spend of this gateway's limits is still unknown:
// control-plane mode, and no totals applied since the start. It is not "totals with no
// usage" — a counter with no pushed window then counts from zero because the control
// plane said so. Callers hold l.mu.
func (l *Limiter) noTotalsLocked() bool {
	return l.shared() && l.totalsAt.IsZero()
}
