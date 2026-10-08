// Package limits enforces the configured limits locally: window counters checked and
// reserved before a request is routed, settled with the request's actual usage once it
// is over (docs/specs/GATEWAY.md, Limits). Every scope — global and each group — has an
// hour and a month count whether or not it has a limit of that type, and a limit is a
// check over its scope's count; a scope's counts outlive the config that has it while
// own usage or a running request holds them. Per-minute windows exist only for the
// limits that have them. In file mode the counters enforce every window at the full
// limit, and every count starts from zero at each start. In control-plane mode
// (NewShared) the hour and month windows
// enforce the control plane's pushed totals plus this gateway's own usage not yet
// counted there, per-minute windows enforce the limit's share among the live
// gateways, and money-limited requests are refused once the control plane has been
// out of reach past the outage grace.
package limits

import (
	"log/slog"
	"math"
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

// effectiveLimit converts a configured limit value into the counter's integer unit:
// requests and tokens as they are (the schema makes them integers), USD in
// nano-dollars.
func effectiveLimit(l config.Limit) int64 {
	v := l.Value
	if l.Type.Measure() == config.MeasureCost {
		v = math.Round(v * 1e9)
	}
	if v >= math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(v)
}

// counterKey identifies a count across reloads: its group (or global) and type. A
// scope has at most one limit of a type, so the key also names that limit. A reload
// keeps the counter of every key that still exists, its limit added, edited or removed.
// Group IDs are unique across the tree, so the group alone names the scope.
type counterKey struct {
	// group is the group the limit belongs to; "" = global (IDs are never empty).
	group string
	typ   config.LimitType
}

func keyOf(group string, typ config.LimitType) counterKey {
	return counterKey{group: group, typ: typ}
}

func (k counterKey) scope() Scope { return scopeOf(k.group) }

// counter is one scope's window of one type. limited: the scope has a limit of the
// type, limit; a counter without one only counts, and never refuses. refs counts the
// running requests holding a reservation on it, of any amount: a retained counter
// stays while one does, whatever its window holds. measure is its type's: a cost
// counter counts nano-USD, accounting's cost unit.
type counter struct {
	key     counterKey
	limited bool
	limit   config.Limit
	measure config.Measure
	w       *window
	refs    int
}

// Limiter holds every counter of the live config — each scope's hour and month counts
// and each per-minute limit's window — and the hour and month counts of scopes a reload
// removed, while their window holds own usage or a running request holds them. Its lock guards all counters, so
// checking and reserving across a request's scopes is all-or-nothing: two concurrent
// requests can never both take the last unit.
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
	// minute are the per-minute counters of counters: the ones a live-gateway count
	// re-shares.
	minute []*counter
	// retained are the hour and month counts of scopes the live config does not have,
	// kept while their current window holds own usage or a running request holds a
	// reservation on them: a group created again under its ID takes them back, and the
	// requests running settle on them (docs/specs/GATEWAY.md, Limits → A count outlives
	// its scope's config).
	retained map[counterKey]*counter
	// housekeptAt is the start of the hour the retained counts and the pushed windows
	// were last pruned in (housekeepLocked).
	housekeptAt time.Time
	// owning are the shared counters holding settled own usage by generation, the ones
	// retiring a generation visits; a counter that rolled its window may stay listed
	// with nothing left.
	owning map[*counter]struct{}

	// Control-plane mode (shared.go). live is the live-gateway count per-minute shares
	// divide by (at least 1); pushed is every window the stream's totals have listed
	// since its last complete totals, by scope and type, those of scopes the config
	// does not have included — a reload that adds the scope finds its base.
	live   int64
	pushed map[counterKey]PushedWindow
	// totalsAt is when totals were last applied; zero before any: the hour and month
	// spend is unknown then, not zero (noTotalsLocked).
	totalsAt time.Time
	// firstTotals is closed when the first totals are taken (FirstTotals).
	firstTotals chan struct{}
	// counted is the newest usage generation dropped from the counters' own usage — a
	// record of that generation or an older one settled later is already inside the
	// applied bases.
	counted uint64
	// outageSince is when the outage the log announced began (its grace ran out);
	// zero while none is announced (outageLocked).
	outageSince time.Time
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
// (time.Now outside tests); logger (nil: discard) hears of a counter that went
// negative (a bug, clamped).
func New(holder *config.Holder, now func() time.Time, logger *slog.Logger) *Limiter {
	return &Limiter{holder: holder, now: now, logger: orDiscard(logger), counters: map[counterKey]*counter{},
		retained: map[counterKey]*counter{}, owning: map[*counter]struct{}{}}
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
	// UsageUncountedSince is when the oldest batch acknowledged but not yet shown
	// counted by applied totals was acknowledged; zero while none waits
	// (docs/specs/GATEWAY.md, Limits → Usage acks count for money limits).
	UsageUncountedSince time.Time
}

// NewShared returns a control-plane-mode limiter: its hour and month windows count
// the totals TakeTotals takes plus the usage not yet counted by the control plane,
// its per-minute windows a share of their limit. contact reports the control plane's
// contact now; it decides the outage (Outage). logger (nil: discard) also hears of
// per-minute shares too small for a model's default output.
func NewShared(holder *config.Holder, now func() time.Time, contact func() Contact, logger *slog.Logger) *Limiter {
	return &Limiter{holder: holder, now: now, contact: contact, logger: orDiscard(logger),
		counters: map[counterKey]*counter{}, retained: map[counterKey]*counter{}, owning: map[*counter]struct{}{},
		live: 1, pushed: map[counterKey]PushedWindow{}, firstTotals: make(chan struct{})}
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

// sync matches the counters to the live snapshot when it changed: every scope gets
// its hour and month counts, limited or not, and each per-minute limit its window.
// Counts whose scope and type still exist keep their counter (a limit added, changed or
// removed applies to the count so far), and so does a scope created again while its
// counts are retained; new ones start empty (in control-plane mode, from their pushed
// base). The hour and month counts of a deleted scope are retained while they hold own
// usage or a running request holds them; removed per-minute limits are dropped.
// Callers hold l.mu.
func (l *Limiter) sync() {
	l.housekeepLocked(l.now())
	snap := l.holder.Current()
	if snap == nil || snap == l.applied {
		return
	}
	if l.observeSync != nil {
		start := time.Now()
		defer func() { l.observeSync(time.Since(start)) }()
	}
	next := make(map[counterKey]*counter, len(l.counters))
	// Every scope is counted in each counted type, limited or not; the per-minute
	// types are local shares, kept only for their limits.
	types := config.LimitTypes()
	build := func(group string, limits []config.Limit) []*counter {
		out := make([]*counter, 0, len(types)+len(limits))
		take := func(typ config.LimitType) *counter {
			k := keyOf(group, typ)
			c, ok := l.counters[k]
			if !ok {
				c, ok = l.retained[k]
				delete(l.retained, k)
			}
			if !ok {
				c = l.newCounter(k)
			}
			c.limited, c.limit = false, config.Limit{Type: typ}
			next[k] = c
			out = append(out, c)
			return c
		}
		for _, typ := range types {
			if typ.Counted() {
				take(typ)
			}
		}
		for _, lim := range limits {
			c, ok := next[keyOf(group, lim.Type)]
			if !ok {
				c = take(lim.Type)
			}
			c.limited, c.limit = true, lim
		}
		return out
	}
	l.global = build("", snap.GlobalLimits)
	l.byGroup = make(map[string][]*counter, len(snap.Groups))
	for id, g := range snap.Groups {
		l.byGroup[id] = build(id, g.Limits)
	}
	l.minute = l.minute[:0]
	for _, c := range next {
		l.applyLimit(c)
		if !c.key.typ.Counted() {
			l.minute = append(l.minute, c)
		}
	}
	for k, c := range l.counters {
		if _, kept := next[k]; !kept && k.typ.Counted() {
			c.limited, c.limit = false, config.Limit{Type: k.typ}
			l.retained[k] = c
		}
	}
	l.counters = next
	l.applied = snap
	l.pruneRetainedLocked(l.now())
	if l.shared() {
		// The counters were rebuilt from the pushed windows already (by group or global
		// and type): only the small-share warning runs again.
		l.warnSmallSharesLocked()
	}
}

// pruneRetainedLocked drops the retained counts no running request holds whose
// current window holds no own usage: the window they kept usage for has ended, or it
// was all shown counted. A count a request holds stays, whatever its window holds — a
// USD reservation holds nothing, and a token reservation's window may have rolled —
// so the request settles on the counter a group created again takes back. Callers
// hold l.mu.
func (l *Limiter) pruneRetainedLocked(now time.Time) {
	for k, c := range l.retained {
		c.w.roll(now)
		if c.refs == 0 && c.w.used == 0 {
			delete(l.retained, k)
			delete(l.owning, c)
		}
	}
}

// housekeepLocked prunes, once an hour, on whatever brings the limiter into use, what
// outlives its window: the retained counts (pruneRetainedLocked) and the pushed
// windows (prunePushedLocked). Both end on an hour boundary, and neither a reload nor
// a totals event (file mode has none) need come after it. Callers hold l.mu.
func (l *Limiter) housekeepLocked(now time.Time) {
	hour := windowStart(UTCHour, now)
	if !hour.After(l.housekeptAt) {
		return
	}
	l.housekeptAt = hour
	l.pruneRetainedLocked(now)
	l.prunePushedLocked(now)
}

// newCounter is the counter of a scope and type no existing counter has: it starts
// empty (docs/specs/GATEWAY.md, Limits → Config reload). Callers hold l.mu.
func (l *Limiter) newCounter(k counterKey) *counter {
	c := &counter{key: k, measure: k.typ.Measure(), w: newWindow(kindOf(k.typ.Window()), 0)}
	c.w.shared = l.shared() && k.typ.Counted()
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
func LogValue(m config.Measure, v int64) any {
	if m == config.MeasureCost {
		return float64(v) / 1e9
	}
	return v
}

// applyLimit sets c's effective limit — the configured value, or in control-plane
// mode a per-minute window's share; 0 for a count without a limit, which is never
// checked — and a shared window's pushed base. Callers hold l.mu.
func (l *Limiter) applyLimit(c *counter) {
	limit := int64(0)
	if c.limited {
		limit = effectiveLimit(c.limit)
	}
	if !l.shared() {
		c.w.limit = limit
		return
	}
	if !c.key.typ.Counted() {
		c.w.limit = share(limit, l.live)
		return
	}
	c.w.limit = limit
	if p, ok := l.pushed[c.key]; ok {
		c.w.setBase(l.now(), p.Start, p.Used)
	} else {
		c.w.base = 0 // not listed since the last complete totals: nothing used there
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
	return !c.key.typ.Counted() && need > c.w.limit && need <= effectiveLimit(c.limit) && c.w.usedAt(now) == 0
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
	if c.measure != config.MeasureTokens {
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
	return !c.key.typ.Counted() && need > c.w.limit && need <= effectiveLimit(c.limit) && settled == 0
}

// share is a per-minute limit's share among live gateways: rounded down, never below
// 1 unless the limit is 0.
func share(limit, live int64) int64 {
	if limit == 0 {
		return 0
	}
	return max(limit/max(live, 1), 1)
}

// Subject is who a request's usage counts toward.
type Subject struct {
	// Groups is the key's group path, top-level first: the request's group scopes.
	Groups []string
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
// each group on its path, limited or not — a request counts on every one, and only the
// limited ones are checked. USD counters apply
// only to a priced request (Subject.Priced): an unpriced model costs nothing, so no
// budget refuses it or is spent by it (docs/specs/GATEWAY.md, Limits → Unpriced
// models); a request that generates nothing (Subject.RequestsOnly) meets only request
// counters. Callers hold l.mu and have synced.
func (l *Limiter) applicable(s Subject) []*counter {
	var out []*counter
	add := func(cs []*counter) {
		for _, c := range cs {
			if (s.Priced || c.measure != config.MeasureCost) && (!s.RequestsOnly || c.measure == config.MeasureRequests) {
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
	case config.MeasureRequests:
		return 1
	case config.MeasureTokens:
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
			if c.limited && c.measure == config.MeasureCost {
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
		if !c.limited || c.admits(now, n) {
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
		c.refs++
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
// Settle runs once per reservation. A per-minute counter a reload dropped since is
// settled harmlessly; an hour or month counter a reload removed is retained until its
// last reservation settles.
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
		hc.c.refs--
		if hc.c.measure == config.MeasureRequests {
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
			l.addOwnLocked(hc.c, now, amountOf(hc.c.measure, rec), rec.Generation)
		}
	}
}

// addOwnLocked counts settled usage of generation on c at now; a shared window keeps it
// as own usage until the generation is shown counted. Callers hold l.mu.
func (l *Limiter) addOwnLocked(c *counter, now time.Time, amount int64, generation uint64) {
	c.w.add(now, amount, false, generation)
	if c.w.shared && amount != 0 {
		l.owning[c] = struct{}{}
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

// amountOf is what rec counts on a token or cost counter: its cost, or its
// config.CountedUnits, the tokens that load the backend.
func amountOf(m config.Measure, rec accounting.UsageRecord) int64 {
	if m == config.MeasureCost {
		return rec.CostNanoUSD
	}
	return rec.Units.Sum(config.CountedUnits)
}

// Used is the count of group's ("" = global) counter of type typ, unsettled
// reservations included, in the counter's unit — requests, tokens or nano-USD; false
// when the live config gives the scope no counter of the type. Only tests read it:
// the gateway reports no count.
func (l *Limiter) Used(group string, typ config.LimitType) (int64, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sync()
	c, ok := l.counters[keyOf(group, typ)]
	if !ok {
		return 0, false
	}
	return c.w.usedAt(l.now()), true
}
