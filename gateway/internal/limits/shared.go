package limits

import (
	"time"

	"kaiak/internal/config"
	"kaiak/internal/logattr"
)

// Control-plane mode (docs/specs/GATEWAY.md, Limits → Control-plane mode). The hour
// and month windows count the control plane's totals — pushed on the config stream
// and in every usage ack — plus this gateway's own usage the control plane has not
// counted yet. That usage is tagged by usage generation: each record carries the
// generation of the usage batch the control client sealed it in, and TakeTotals drops
// every generation the control plane has counted, in the same step as it adopts the
// totals that include it, so nothing is counted twice or dropped in between.
//
// Totals apply only to the config they were computed under: a message whose
// (config_epoch, config_version) is not the applied config's changes neither the
// bases nor the own usage — its windows describe another config's limits, and the
// bases it would replace are the only record of what this config's limits spent. It
// waits, and applies when its config does.

// Totals are the control plane's totals as the limiter takes them.
type Totals struct {
	// Config is the config the totals were computed under.
	Config config.Version
	// LiveGateways is the live-gateway count per-minute shares divide by; 0 counts
	// as 1.
	LiveGateways int64
	// Windows is complete: a counter with no window has used nothing in the control
	// plane's current window.
	Windows []PushedWindow
}

// PushedWindow is one limit's current window as the control plane counts it,
// identified as a config reload identifies a counter.
type PushedWindow struct {
	// Group is the group the limit belongs to; "" for a global limit.
	Group string
	Type  config.LimitType
	Start time.Time
	// Used counts tokens or nano-USD, as the counter does.
	Used int64
}

// TakeTotals takes one totals message, in one step under the limiter's lock. t, when
// not nil, is the newest totals: its live-gateway count applies at once (per-minute
// shares keep what they counted), and its windows become the bases — each hour and
// month counter's base its pushed window, 0 without one, a window matching no counter
// ignored — when t was computed under the applied config; otherwise t waits for its
// config (sync applies it then) and the bases stay. counted, when not 0, is the newest
// usage generation the message shows counted: it leaves the counters' own usage now
// when the newest totals are applied (they include it), else once they are. It
// reports whether t was applied.
func (l *Limiter) TakeTotals(t *Totals, counted uint64) (applied bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sync()
	now := l.now()
	if t != nil {
		applied = l.applied != nil && t.Config == l.applied.Version
		if !l.firstClosed {
			l.firstClosed = true
			close(l.firstTotals)
		}
		l.live = max(t.LiveGateways, 1)
		l.latest, l.haveLatest = t.Config, true
		l.waiting = t
		if applied {
			l.applyWaitingLocked(now)
		}
		for _, c := range l.counters {
			l.applyLimit(c)
		}
	}
	l.pendingCounted = max(l.pendingCounted, counted)
	l.retireCountedLocked()
	l.noteMismatchLocked(now)
	l.warnSmallSharesLocked()
	return applied
}

// warnSmallSharesLocked logs, once per applied config and live-gateway count, each
// per-minute token limit whose share is below the default output of a model it
// covers: output default × live gateways > limit. Such a request is admitted only
// while this gateway's window for the limit is empty (counter.admits), so the limit
// works as about one request a minute per gateway. Callers hold l.mu.
func (l *Limiter) warnSmallSharesLocked() {
	if l.applied == nil || (l.applied == l.sharesChecked.snapshot && l.live == l.sharesChecked.live) {
		return
	}
	l.sharesChecked.snapshot, l.sharesChecked.live = l.applied, l.live
	for _, c := range l.counters {
		if c.limit.Type != config.LimitTokensPerMinute || c.w.limit >= effectiveLimit(c.limit) {
			continue
		}
		for _, name := range l.applied.ModelNames {
			m := l.applied.Models[name]
			if m.OutputLimit == nil || m.OutputLimit.Default <= c.w.limit {
				continue
			}
			l.logger.Warn("per-minute share below the model's default output: a request at the default is admitted only while this gateway's window is empty",
				append(identityAttrs(c.key.group), "kaiak.model.name", name,
					"kaiak.limit.configured", effectiveLimit(c.limit), "kaiak.limit.live_gateways", l.live,
					"kaiak.limit.enforced", c.w.limit, "kaiak.model.output_default", m.OutputLimit.Default)...)
		}
	}
}

// applyWaitingLocked makes the waiting totals the applied ones. Callers hold l.mu and
// apply the counters' limits after.
func (l *Limiter) applyWaitingLocked(now time.Time) {
	t := l.waiting
	l.waiting = nil
	l.totalsAt = now
	l.totalsKnown = true
	if l.restoredUntagged {
		// Restored uncounted usage whose batches were no longer spooled at boot: the
		// control plane counted them (or they were lost), so the first totals applied
		// hold all of it that will ever be counted.
		l.restoredUntagged = false
		for _, c := range l.counters {
			if c.w.shared {
				c.w.counted(0)
				l.checkCountLocked(c)
			}
		}
	}
	l.pushed = make(map[counterKey]PushedWindow, len(t.Windows))
	for _, w := range t.Windows {
		l.pushed[keyOf(w.Group, config.Limit{Type: w.Type})] = w
		l.warnAheadLocked(w, now)
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

// retireCountedLocked drops the generations shown counted from the counters' own
// usage, when the newest totals are the applied ones. Callers hold l.mu.
func (l *Limiter) retireCountedLocked() {
	if l.waiting != nil || l.pendingCounted <= l.counted {
		return
	}
	l.counted = l.pendingCounted
	for _, c := range l.counters {
		if c.w.shared {
			c.w.counted(l.counted)
			l.checkCountLocked(c)
		}
	}
}

// countedLocked reports whether usage of generation is inside the applied bases: its
// batch was shown counted and dropped from the own usage. Callers hold l.mu.
func (l *Limiter) countedLocked(generation uint64) bool {
	return l.counted != 0 && generation <= l.counted
}

// configChangedLocked follows a newly applied config: totals waiting for it apply now.
// Callers hold l.mu, have rebuilt the counters, and have l.applied set.
func (l *Limiter) configChangedLocked(now time.Time) {
	if l.waiting != nil && l.waiting.Config == l.applied.Version {
		l.applyWaitingLocked(now)
		for _, c := range l.counters {
			l.applyLimit(c)
		}
		l.retireCountedLocked()
	}
	l.noteMismatchLocked(now)
	l.warnSmallSharesLocked()
}

// noteMismatchLocked tracks whether the newest totals were computed under a config
// other than the applied one (the control plane counts another config's limits —
// typically one this gateway rejected), and since when. Callers hold l.mu.
func (l *Limiter) noteMismatchLocked(now time.Time) {
	mismatch := l.haveLatest && l.applied != nil && l.latest != l.applied.Version
	switch {
	case !mismatch:
		l.mismatchSince = time.Time{}
	case l.mismatchSince.IsZero():
		l.mismatchSince = now
	}
}

// mismatchPastGraceLocked reports whether the totals have been for another config for
// longer than the outage grace: the applied limits' spend is then unknown, as in an
// outage. Callers hold l.mu and have synced.
func (l *Limiter) mismatchPastGraceLocked(now time.Time) bool {
	return !l.mismatchSince.IsZero() && now.Sub(l.mismatchSince) > l.applied.ControlOutageGrace
}

// ConfigMismatch reports whether the newest totals were computed under a config
// other than the applied one — typically one this gateway rejected. Once that has
// lasted past the outage grace, priced USD-limited models are refused as in an
// outage (docs/specs/GATEWAY.md, Limits → Control-plane mode: totals follow their
// config). Always false in file mode.
func (l *Limiter) ConfigMismatch() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sync()
	return !l.mismatchSince.IsZero()
}

// TotalsAppliedAt is when totals were last applied; false before any.
func (l *Limiter) TotalsAppliedAt() (time.Time, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.totalsAt, !l.totalsAt.IsZero()
}

// LiveGateways is the live-gateway count of the latest totals taken or restored; 1
// before any, and in file mode.
func (l *Limiter) LiveGateways() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.live
}

// Outage reports whether the control plane has been out of reach for longer than
// the live config's outage grace (global.control_outage_grace_ms): no config stream
// open and no contact since — or usage batches waiting that long for an answer, an
// open stream notwithstanding (a control plane that serves config but no longer
// takes usage cannot count this gateway's spend). Always false in file mode.
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
	var began time.Time // when the grace ran out
	switch {
	case !c.Connected && now.Sub(c.Last) > grace:
		reason, began = "no contact", c.Last.Add(grace)
	case !c.UsageWaitingSince.IsZero() && now.Sub(c.UsageWaitingSince) > grace:
		reason, began = "usage not acknowledged", c.UsageWaitingSince.Add(grace)
	}
	switch {
	case reason != "" && l.outageSince.IsZero():
		l.outageSince = began
		attrs := []any{"kaiak.reason", reason, logattr.Seconds("kaiak.control.since_contact", now.Sub(c.Last)),
			logattr.Seconds("kaiak.control.outage_grace", grace)}
		if !c.UsageWaitingSince.IsZero() {
			attrs = append(attrs, logattr.Seconds("kaiak.control.usage_waiting", now.Sub(c.UsageWaitingSince)))
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
// the first totals). Totals computed under another config than the applied one close
// it too — the wait for matching ones could last until the operator fixes the
// config — but they do not end the no-totals refusal (noTotalsLocked). A restored copy
// of earlier totals does not close it. Never closed in file mode.
func (l *Limiter) FirstTotals() <-chan struct{} { return l.firstTotals }

// noTotalsLocked reports whether the spend of this gateway's limits is still unknown:
// control-plane mode, and no totals applied or restored since the start. It is not
// "totals with no usage" — a counter with no pushed window then counts from zero
// because the control plane said so. Callers hold l.mu.
func (l *Limiter) noTotalsLocked() bool {
	return l.shared() && !l.totalsKnown
}
