// Package limits enforces the configured limits locally: one window counter per
// limit of the global scope and of each group, checked and reserved before a request
// is routed, settled with the request's actual usage once it is over
// (docs/specs/GATEWAY.md, Limits). In file mode the counters enforce every window at
// the full limit, and the hour and month windows survive restarts through a snapshot
// in the data directory. In control-plane mode (NewShared) the hour and month windows
// enforce the control plane's pushed totals plus this gateway's own usage not yet
// counted there, per-minute windows enforce the limit's share among the live
// gateways, and money-limited requests are refused once the control plane has been
// out of reach past the outage grace.
package limits

import (
	"log/slog"
	"math"
	"slices"
	"strings"
	"sync"
	"time"

	"kaiak/internal/accounting"
	"kaiak/internal/config"
)

// Scope is the kind of scope a limit belongs to: global, or a group of the tree.
type Scope string

const (
	ScopeGlobal Scope = "global"
	ScopeGroup  Scope = "group"
)

// scopeOf is the kind of scope of a limit of group ("" = global).
func scopeOf(group string) Scope {
	if group == "" {
		return ScopeGlobal
	}
	return ScopeGroup
}

// Measure is what a limit counts.
type Measure int

const (
	MeasureRequests Measure = iota
	MeasureTokens
	// MeasureCost counts nano-USD, accounting's cost unit.
	MeasureCost
)

// shape is the window kind and measure of a limit type.
func shape(t config.LimitType) (Kind, Measure) {
	switch t {
	case config.LimitRequestsPerMinute:
		return SlidingMinute, MeasureRequests
	case config.LimitTokensPerMinute:
		return SlidingMinute, MeasureTokens
	case config.LimitTokensPerHour:
		return UTCHour, MeasureTokens
	}
	return UTCMonth, MeasureCost
}

// effectiveLimit converts a configured limit value into the counter's integer unit:
// requests and tokens as they are (the schema makes them integers), USD in
// nano-dollars.
func effectiveLimit(l config.Limit) int64 {
	v := l.Value
	if _, m := shape(l.Type); m == MeasureCost {
		v = math.Round(v * 1e9)
	}
	if v >= math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(v)
}

// counterKey identifies a limit across config versions: its group (or global) and
// type, the one limit of that type in its scope. A reload keeps the counter of every
// limit whose key is unchanged, its value edited or not. Group IDs are unique across
// the tree, so the group alone names the scope.
type counterKey struct {
	// group is the group the limit belongs to; "" = global (IDs are never empty).
	group string
	typ   config.LimitType
}

func keyOf(group string, l config.Limit) counterKey {
	return counterKey{group: group, typ: l.Type}
}

func (k counterKey) scope() Scope { return scopeOf(k.group) }

// counter is one limit's window.
type counter struct {
	key     counterKey
	limit   config.Limit
	measure Measure
	w       *window
}

// Limiter holds every limit's counter for the live config. Its lock guards all
// counters, so checking and reserving across a request's scopes is all-or-nothing:
// two concurrent requests can never both take the last unit.
type Limiter struct {
	holder *config.Holder
	now    func() time.Time
	logger *slog.Logger
	// contact is set in control-plane mode (shared windows): the control plane's
	// contact now.
	contact func() Contact

	mu sync.Mutex
	// observeSync, when set, hears how long each sync to a new snapshot took.
	observeSync func(time.Duration)
	// applied is the snapshot the counters were last matched to. The limiter follows
	// the holder's live snapshot, not each request's: a changed limit applies at once.
	applied  *config.Snapshot
	counters map[counterKey]*counter
	global   []*counter
	byGroup  map[string][]*counter

	// Control-plane mode (shared.go). live is the live-gateway count per-minute shares
	// divide by (at least 1); pushed is the applied totals' windows by limit.
	live   int64
	pushed map[counterKey]PushedWindow
	// totalsAt is when totals were last applied; zero before any.
	totalsAt time.Time
	// totalsKnown: totals were applied or restored since the start — before that, the
	// hour and month spend is unknown, not zero (noTotalsLocked). firstTotals is
	// closed, and firstClosed set, when the first totals are taken (FirstTotals).
	totalsKnown bool
	firstTotals chan struct{}
	firstClosed bool
	// counted is the newest usage generation dropped from the counters' own usage — a
	// record of that generation or an older one settled later is already inside the
	// applied bases.
	counted uint64
	// outageSince is when the outage the log announced began (its grace ran out);
	// zero while none is announced (outageLocked).
	outageSince time.Time
	// restoredUntagged: uncounted usage restored at boot is tagged generation 0 (no
	// spooled batch carried it), to leave with the first totals applied (LoadShared).
	restoredUntagged bool
	// aheadWarned is the pushed window start the ahead-of-the-clock warning last
	// named, so a control plane running ahead is logged once per window, not per
	// message.
	aheadWarned time.Time
	// sharesChecked is the config and live-gateway count the small-share warning
	// last ran for.
	sharesChecked struct {
		snapshot *config.Snapshot
		live     int64
	}
}

// New returns a file-mode limiter for the config held by holder. now is the clock
// (time.Now outside tests); logger (nil: discard) hears of limits that carry their
// usage across a config change.
func New(holder *config.Holder, now func() time.Time, logger *slog.Logger) *Limiter {
	return &Limiter{holder: holder, now: now, logger: orDiscard(logger), counters: map[counterKey]*counter{}}
}

func orDiscard(logger *slog.Logger) *slog.Logger {
	if logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return logger
}

// Contact is the control plane's contact as the outage is decided on it
// (docs/specs/GATEWAY.md, Limits → Outage refusal).
type Contact struct {
	// Connected: a config stream is open. Last: when the control plane was last in
	// contact (before any, when the gateway started).
	Connected bool
	Last      time.Time
	// UsageWaitingSince is when the usage batches not yet answered started waiting
	// for the control plane's next answer; zero while none waits.
	UsageWaitingSince time.Time
}

// NewShared returns a control-plane-mode limiter: its hour and month windows count
// the totals ApplyTotals pushes plus the usage not yet counted by the control plane,
// its per-minute windows a share of their limit. contact reports the control plane's
// contact now; it decides the outage (Outage). logger (nil: discard) also hears of
// per-minute shares too small for a model's default output.
func NewShared(holder *config.Holder, now func() time.Time, contact func() Contact, logger *slog.Logger) *Limiter {
	return &Limiter{holder: holder, now: now, contact: contact, logger: orDiscard(logger),
		counters: map[counterKey]*counter{}, live: 1, pushed: map[counterKey]PushedWindow{}, firstTotals: make(chan struct{})}
}

func (l *Limiter) shared() bool { return l.contact != nil }

// ObserveSyncs has f told how long each match of the counters to a newly applied
// config took (the ops metrics): the request that finds the new config waits for it
// under the limiter's lock, and every other request behind it. The time is the
// process clock's, not the limiter's.
func (l *Limiter) ObserveSyncs(f func(time.Duration)) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.observeSync = f
}

// sync matches the counters to the live snapshot when it changed: limits that still
// exist keep their counters (a changed value applies to the count so far), a limit
// whose only change is its model set keeps its predecessor's (carryOver), other new
// ones start empty, removed ones are dropped. Callers hold l.mu.
func (l *Limiter) sync() {
	snap := l.holder.Current()
	if snap == nil || snap == l.applied {
		return
	}
	if l.observeSync != nil {
		start := time.Now()
		defer func() { l.observeSync(time.Since(start)) }()
	}
	next := make(map[counterKey]*counter, len(l.counters))
	build := func(group string, limits []config.Limit) []*counter {
		out := make([]*counter, len(limits))
		for i, lim := range limits {
			k := keyOf(group, lim)
			c, ok := l.counters[k]
			if !ok {
				c = l.newCounter(k)
			}
			c.limit = lim
			next[k] = c
			out[i] = c
		}
		return out
	}
	l.global = build("", snap.GlobalLimits)
	l.byGroup = make(map[string][]*counter, len(snap.Groups))
	for id, g := range snap.Groups {
		if len(g.Limits) > 0 {
			l.byGroup[id] = build(id, g.Limits)
		}
	}
	for _, c := range next {
		l.applyLimit(c)
	}
	l.counters = next
	l.applied = snap
	if l.shared() {
		l.configChangedLocked()
	}
}

// newCounter is the counter of a limit identity no existing counter has: it starts
// empty (docs/specs/GATEWAY.md, Limits → Config reload). Callers hold l.mu.
func (l *Limiter) newCounter(k counterKey) *counter {
	kind, measure := shape(k.typ)
	c := &counter{key: k, measure: measure, w: newWindow(kind, 0)}
	c.w.shared = l.shared() && kind != SlidingMinute
	return c
}

// identityAttrs are a limit's scope and group log fields: the group is absent for a
// global limit, whose scope says global (docs/specs/GATEWAY.md, Observability → Logs).
func identityAttrs(group string) []any {
	if group == "" {
		return []any{"kaiak.limit.scope", ScopeGlobal}
	}
	return []any{"kaiak.limit.scope", ScopeGroup, "kaiak.limit.group", group}
}

// LogValue is an amount in a limit's unit as the logs write it: requests and tokens as
// they are, nano-USD in dollars, as kaiak.usage.cost_usd.
func LogValue(m Measure, v int64) any {
	if m == MeasureCost {
		return float64(v) / 1e9
	}
	return v
}

// applyLimit sets c's effective limit — the configured value, or in control-plane
// mode a per-minute window's share — and a shared window's pushed base. Callers hold
// l.mu.
func (l *Limiter) applyLimit(c *counter) {
	limit := effectiveLimit(c.limit)
	if !l.shared() {
		c.w.limit = limit
		return
	}
	if c.w.kind == SlidingMinute {
		c.w.limit = share(limit, l.live)
		return
	}
	c.w.limit = limit
	if p, ok := l.pushed[c.key]; ok {
		c.w.setBase(l.now(), p.Start, p.Used)
	} else {
		c.w.base = 0 // the totals are complete: no window, nothing used there
	}
}

// admits reports whether c has room for need at now. A per-minute share never makes
// a request impossible: one that fits the full limit is admitted when the window is
// empty, however small the share (docs/specs/GATEWAY.md, Limits → Control-plane mode:
// Per-minute shares); otherwise it waits for the window to empty.
func (c *counter) admits(now time.Time, need int64) bool {
	if c.w.admits(now, need) {
		return true
	}
	return c.w.kind == SlidingMinute && need > c.w.limit && need <= effectiveLimit(c.limit) && c.w.usedAt(now) == 0
}

// inFlightRetry is what a token limit blocked only by requests still running counts
// toward Retry-After, and its x-ratelimit-reset-tokens on that refusal
// (docs/specs/GATEWAY.md, Limits → Refusal): a reservation holds the input estimate
// plus the output limit, a request settles at a fraction of it, and when the running
// requests end is unknown — a short guess, not a promise.
const inFlightRetry = 2 * time.Second

// blockedByRunning reports whether c, refusing need at now, would admit it were its
// in-flight reservations gone — a token limit only: a request limit keeps counting the
// request once it settles.
func (c *counter) blockedByRunning(now time.Time, need int64) bool {
	if c.measure != MeasureTokens {
		return false
	}
	held := c.w.inFlight(now)
	if held == 0 {
		return false
	}
	settled := c.w.usedAt(now) - held
	if fits(settled, need, c.w.limit) {
		return true
	}
	return c.w.kind == SlidingMinute && need > c.w.limit && need <= effectiveLimit(c.limit) && settled == 0
}

// share is a per-minute limit's share among live gateways: rounded down, never below
// 1 unless the limit is 0.
func share(limit, live int64) int64 {
	if limit == 0 {
		return 0
	}
	return max(limit/max(live, 1), 1)
}

// Subject is who a request's usage counts toward and the model it uses.
type Subject struct {
	// Groups is the key's group path, top-level first: the request's group scopes.
	Groups []string
	Model  string
	// Priced: the model has a price in force for this request, by the request's own
	// config snapshot and arrival — the entry its usage is priced from
	// (accounting.Cost), whatever a reload changed since.
	Priced bool
	// RequestsOnly: the request generates and costs nothing (a token-counting
	// endpoint) — only requests-per-minute limits apply; no token or cost limit
	// refuses it, nor does an unknown budget.
	RequestsOnly bool
}

// applicable lists the counters of every scope the subject belongs to: global and
// each group on its path. USD counters apply
// only to a priced request (Subject.Priced): an unpriced model costs nothing, so no
// budget refuses it or is spent by it (docs/specs/GATEWAY.md, Limits → Unpriced
// models); a request that generates nothing (Subject.RequestsOnly) meets only request
// counters. Callers hold l.mu and have synced.
func (l *Limiter) applicable(s Subject) []*counter {
	var out []*counter
	add := func(cs []*counter) {
		for _, c := range cs {
			if (s.Priced || c.measure != MeasureCost) && (!s.RequestsOnly || c.measure == MeasureRequests) {
				out = append(out, c)
			}
		}
	}
	add(l.global)
	for _, group := range s.Groups {
		add(l.byGroup[group])
	}
	return out
}

// need is what a request reserves on c: one request, its token estimate, or nothing
// for cost (known only after the request; the check is then that the window is still
// below its limit).
func need(c *counter, tokens int64) int64 {
	switch c.measure {
	case MeasureRequests:
		return 1
	case MeasureTokens:
		return tokens
	}
	return 0
}

// Reservation is what a request holds on its counters until it settles.
type Reservation struct {
	holds   []heldCounter
	headers Headers
	settled bool
}

type heldCounter struct {
	c *counter
	h hold
}

// Headers returns the rate-limit header values computed when the request was
// admitted.
func (r *Reservation) Headers() Headers { return r.headers }

// Reserve checks the subject's request against every applicable limit and, if all
// admit it, reserves one request and tokens estimated tokens on each. tokens is the
// request's input estimate plus its effective output limit. On refusal nothing is
// reserved and the Rejection says why.
func (l *Limiter) Reserve(s Subject, tokens int64) (*Reservation, *Rejection) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sync()
	now := l.now()
	counters := l.applicable(s)

	// The outage first: deciding it on every request is what logs its start and end.
	if l.outageLocked(now) || l.noTotalsLocked() {
		for _, c := range counters {
			if c.measure == MeasureCost {
				return nil, &Rejection{Scope: c.key.scope(), Group: c.key.group, Type: c.limit.Type, Measure: c.measure,
					Limit: c.w.limit, Max: effectiveLimit(c.limit), Unavailable: true}
			}
		}
	}

	var rej *Rejection
	// running are the refusing counters blocked only by requests still running.
	var running []*counter
	for _, c := range counters {
		n := need(c, tokens)
		if c.admits(now, n) {
			continue
		}
		var wait time.Duration
		if c.blockedByRunning(now, n) {
			wait = inFlightRetry
			running = append(running, c)
		} else {
			wait = c.w.waitFor(now, n)
		}
		if rej == nil || wait > rej.RetryAfter {
			rej = &Rejection{Scope: c.key.scope(), Group: c.key.group, Type: c.limit.Type, Measure: c.measure, Limit: c.w.limit,
				Max: effectiveLimit(c.limit), Used: c.w.usedAt(now), Requested: n, RetryAfter: wait}
		}
	}
	if rej != nil {
		rej.Headers = headersFor(counters, now, running)
		return nil, rej
	}

	res := &Reservation{holds: make([]heldCounter, 0, len(counters))}
	for _, c := range counters {
		res.holds = append(res.holds, heldCounter{c: c, h: c.w.reserve(now, need(c, tokens))})
	}
	res.headers = headersFor(counters, now, nil)
	return res, nil
}

// Settle ends a reservation with the request's usage records on the counters it was
// checked against: token reservations are replaced by the tokens the records
// processed (amountOf: input read from the cache does not count), their cost is
// added to cost limits, and the request stays counted — once, however many records
// it has (one per attempt that produced usage, docs/specs/GATEWAY.md, Usage across
// attempts). A request that never produced a
// record passes none; its token reservations are then released, and the request still
// counts: it took a slot.
// Settle runs once per reservation; counters dropped by a reload since are settled
// harmlessly.
//
// In control-plane mode each record's usage counts in the hour and month windows as
// usage of its generation (UsageRecord.Generation, the batch it was sealed in) —
// unless the control plane has already been shown to count that batch and its
// totals are applied: the record is then inside the pushed base, and counting it
// again would count it twice. A record settled (GatewayTime) in the window before
// the current one belongs to that window, as the control plane counts it, and no
// longer counts here.
func (l *Limiter) Settle(r *Reservation, recs ...accounting.UsageRecord) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if r.settled {
		return
	}
	r.settled = true
	now := l.now()
	for _, hc := range r.holds {
		if hc.c.measure == MeasureRequests {
			hc.c.w.keep(now, hc.h)
			l.checkCountLocked(hc.c)
			continue
		}
		hc.c.w.release(now, hc.h)
		l.checkCountLocked(hc.c)
		for _, rec := range recs {
			if hc.c.w.shared && (l.countedLocked(rec.Generation) || hc.c.w.previousWindow(now, rec.GatewayTime)) {
				continue
			}
			hc.c.w.add(now, amountOf(hc.c.measure, rec), false, rec.Generation)
		}
	}
}

// checkCountLocked logs and clamps a negative count on c (window.clampNegative).
// Callers hold l.mu.
func (l *Limiter) checkCountLocked(c *counter) {
	if c.w.clampNegative() {
		l.logger.Error("limit counter went negative: clamped to 0", append(identityAttrs(c.key.group),
			"kaiak.limit.type", c.key.typ)...)
	}
}

// amountOf is what rec counts on a token or cost counter: its cost, or the tokens that
// load the backend — tokens_in + tokens_cache_write + tokens_out (reasoning is inside
// tokens_out). Input read from the cache does not count: a prefix-cache hit costs the
// backend almost nothing.
func amountOf(m Measure, rec accounting.UsageRecord) int64 {
	if m == MeasureCost {
		return rec.CostNanoUSD
	}
	var tokens int64
	for _, unit := range []config.Unit{config.UnitTokensIn, config.UnitTokensCacheWrite, config.UnitTokensOut} {
		tokens = saturatingAdd(tokens, rec.Units[unit])
	}
	return tokens
}

// CounterUsage is one counter's state, for reading (metrics, tests).
type CounterUsage struct {
	// Group is the group the limit belongs to; "" for a global limit.
	Group string
	Type  config.LimitType
	// Limit and Used are in the counter's unit: requests, tokens or nano-USD. Used
	// includes unsettled reservations.
	Limit int64
	Used  int64
}

// Usage returns every counter's current state, sorted by group (global first) and
// type.
func (l *Limiter) Usage() []CounterUsage {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sync()
	now := l.now()
	out := make([]CounterUsage, 0, len(l.counters))
	for _, c := range l.counters {
		out = append(out, CounterUsage{Group: c.key.group, Type: c.limit.Type,
			Limit: c.w.limit, Used: c.w.usedAt(now)})
	}
	slices.SortFunc(out, func(a, b CounterUsage) int {
		if c := strings.Compare(a.Group, b.Group); c != 0 {
			return c
		}
		return strings.Compare(string(a.Type), string(b.Type))
	})
	return out
}
