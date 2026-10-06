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
// one is matched to the counter of its group (or global) and type.

// Totals are the control plane's totals as the limiter takes them.
type Totals struct {
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

// TakeTotals takes one totals message, in one step under the limiter's lock: its
// live-gateway count applies at once (per-minute shares keep what they counted), and
// its windows become the bases — each hour and month counter's base its pushed window,
// 0 without one, a window matching no counter ignored. counted, when not 0, is the
// newest usage generation the message shows counted: it leaves the counters' own
// usage now, as the totals that include it are applied.
func (l *Limiter) TakeTotals(t Totals, counted uint64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sync()
	now := l.now()
	if !l.firstClosed {
		l.firstClosed = true
		close(l.firstTotals)
	}
	l.live = max(t.LiveGateways, 1)
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
	for _, c := range l.counters {
		l.applyLimit(c)
	}
	l.retireCountedLocked(counted)
	l.warnSmallSharesLocked()
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

// configChangedLocked follows a newly applied config: the counters were rebuilt from
// the pushed windows already (by group or global and type), so only the small-share
// warning runs again. Callers hold l.mu, have rebuilt the counters, and have l.applied
// set.
func (l *Limiter) configChangedLocked() {
	l.warnSmallSharesLocked()
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
// the first totals). A restored copy of earlier totals does not close it. Never
// closed in file mode.
func (l *Limiter) FirstTotals() <-chan struct{} { return l.firstTotals }

// noTotalsLocked reports whether the spend of this gateway's limits is still unknown:
// control-plane mode, and no totals applied or restored since the start. It is not
// "totals with no usage" — a counter with no pushed window then counts from zero
// because the control plane said so. Callers hold l.mu.
func (l *Limiter) noTotalsLocked() bool {
	return l.shared() && !l.totalsKnown
}
