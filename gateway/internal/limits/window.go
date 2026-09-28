package limits

import (
	"cmp"
	"math"
	"slices"
	"time"
)

// Kind is a limit window's shape (docs/specs/GATEWAY.md, Limits).
type Kind int

const (
	// SlidingMinute counts the last 60 seconds, in one-second buckets.
	SlidingMinute Kind = iota
	// UTCHour counts the current clock hour, UTC.
	UTCHour
	// UTCMonth counts the current calendar month, UTC.
	UTCMonth
)

const minuteBuckets = 60

// window counts one quantity (requests, tokens or nano-USD) over one window kind,
// against an effective limit. It is not safe for concurrent use: the Limiter guards
// every window with its lock, so a check across several windows is atomic.
//
// Reservations are counted where they are made; settling releases a reservation (if
// its bucket or window is still current) and counts the actual amount at the settle
// time, so every amount is counted in exactly one bucket.
//
// A shared window is an hour or month window in control-plane mode: what it counts
// is the control plane's pushed total for the window plus this gateway's own usage
// the control plane has not counted yet — reservations in flight, and settled
// amounts tagged by usage generation until the generation is counted.
type window struct {
	kind Kind
	// limit is the effective limit the window enforces: the configured value, or in
	// control-plane mode a per-minute window's share.
	limit int64

	// SlidingMinute: bucket i holds the amount counted during unix second sec[i].
	sec [minuteBuckets]int64
	n   [minuteBuckets]int64

	// UTCHour, UTCMonth: the current window's start (unix seconds), everything counted
	// in it, and the part of that still held by unsettled reservations. incarnation
	// changes every time the window is cleared (clear): a hold names the incarnation
	// it was counted in, not the start, which can come back (a pushed window ahead of
	// the gateway's clock, then corrected).
	start       int64
	used        int64
	held        int64
	incarnation uint64

	// Shared windows only. base is the control plane's used amount for the window
	// starting at baseStart (unix seconds); it counts while that is the current
	// window. local is the settled part of used by usage generation, one entry per
	// generation in no particular order: what the control plane has not counted yet.
	shared    bool
	base      int64
	baseStart int64
	local     []generationAmount
}

// generationAmount is settled usage of one usage generation.
type generationAmount struct {
	generation uint64
	n          int64
}

// hold is one reservation: the amount and where it was counted — the bucket (a unix
// second, at) or the fixed window's incarnation.
type hold struct {
	at          int64
	incarnation uint64
	n           int64
}

func newWindow(kind Kind, limit int64) *window {
	return &window{kind: kind, limit: limit}
}

// windowStart is the start of the fixed window holding t.
func windowStart(kind Kind, t time.Time) time.Time {
	t = t.UTC()
	if kind == UTCMonth {
		return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
	}
	return t.Truncate(time.Hour)
}

// windowEnd is the end of the fixed window starting at start.
func windowEnd(kind Kind, start time.Time) time.Time {
	if kind == UTCMonth {
		return start.AddDate(0, 1, 0)
	}
	return start.Add(time.Hour)
}

// roll starts a new fixed window when now has left the current one. A shared
// window's current window is also at least the one the control plane last named, and
// never goes back. Its uncounted settled usage stays behind with the window it was
// settled in: the control plane counts a record in its gateway_time window when that
// is its current or previous one, so it never lands in the window starting now — and
// the window left behind is no longer enforced.
func (w *window) roll(now time.Time) {
	s := windowStart(w.kind, now).Unix()
	if w.shared {
		s = max(s, w.baseStart, w.start)
	}
	if s != w.start {
		w.clear(s)
	}
}

// clear starts the fixed window at start with nothing counted, as a new incarnation:
// no hold of an earlier one is ever released or kept in it.
func (w *window) clear(start int64) {
	w.start, w.used, w.held = start, 0, 0
	w.local = w.local[:0]
	w.incarnation++
}

// previousWindow reports whether usage settled at t belongs to the window before the
// current one at now — where the control plane counts it too — rather than to the
// current window. A zero t is usage settled now.
func (w *window) previousWindow(now, t time.Time) bool {
	w.roll(now)
	if t.IsZero() {
		return false
	}
	current := time.Unix(w.start, 0).UTC()
	previous := current.Add(-time.Hour)
	if w.kind == UTCMonth {
		previous = current.AddDate(0, -1, 0)
	}
	return windowStart(w.kind, t).Equal(previous)
}

// pushed is the control plane's amount counting in the current window: its base when
// it names that window, else nothing yet. Callers have rolled the window.
func (w *window) pushed() int64 {
	if w.shared && w.baseStart == w.start {
		return w.base
	}
	return 0
}

// live reports whether the minute bucket for second s is inside the window at now.
func live(s int64, now time.Time) bool {
	n := now.Unix()
	return s > n-minuteBuckets && s <= n
}

// usedAt is the amount counted in the window at now, reservations included (and a
// shared window's pushed total).
func (w *window) usedAt(now time.Time) int64 {
	if w.kind != SlidingMinute {
		w.roll(now)
		return saturatingAdd(w.used, w.pushed())
	}
	var sum int64
	for i := range minuteBuckets {
		if live(w.sec[i], now) {
			sum = saturatingAdd(sum, w.n[i])
		}
	}
	return sum
}

// saturatingAdd is a + b for non-negative b, math.MaxInt64 when that overflows.
// Counts never wrap: a wrapped count would read as room under every limit
// (docs/specs/GATEWAY.md, Limits → Check and reserve).
func saturatingAdd(a, b int64) int64 {
	if b > math.MaxInt64-a {
		return math.MaxInt64
	}
	return a + b
}

// fits reports whether need more fits under limit with used counted, in a form
// that cannot overflow. need 0 asks whether used is still below the limit.
func fits(used, need, limit int64) bool {
	if need == 0 {
		return used < limit
	}
	return used <= limit && need <= limit-used
}

// add counts amount (non-negative) at now; held marks a reservation, which settles
// later. A shared window tags settled amounts with generation, the usage generation
// they belong to. A count saturates at the int64 maximum; the hold records what was
// actually added, so releasing it takes back exactly that.
func (w *window) add(now time.Time, amount int64, held bool, generation uint64) hold {
	if w.kind != SlidingMinute {
		w.roll(now)
		amount = saturatingAdd(w.used, amount) - w.used
		w.used += amount
		switch {
		case held:
			w.held += amount
		case w.shared && amount != 0:
			w.addLocal(generation, amount)
		}
		return hold{incarnation: w.incarnation, n: amount}
	}
	s := now.Unix()
	i := s % minuteBuckets
	if w.sec[i] != s {
		w.sec[i], w.n[i] = s, 0
	}
	amount = saturatingAdd(w.n[i], amount) - w.n[i]
	w.n[i] += amount
	return hold{at: s, n: amount}
}

// reserve counts a reservation of amount at now.
func (w *window) reserve(now time.Time, amount int64) hold {
	return w.add(now, amount, true, 0)
}

// release removes a reservation. A reservation whose bucket or window incarnation has
// already passed is no longer counted, so there is nothing to remove.
func (w *window) release(now time.Time, h hold) {
	if w.kind != SlidingMinute {
		w.roll(now)
		if h.incarnation == w.incarnation {
			w.used -= h.n
			w.held -= h.n
		}
		return
	}
	i := h.at % minuteBuckets
	if w.sec[i] == h.at {
		w.n[i] -= h.n
	}
}

// keep turns a reservation into settled usage where it was counted.
func (w *window) keep(now time.Time, h hold) {
	if w.kind == SlidingMinute {
		return
	}
	w.roll(now)
	if h.incarnation == w.incarnation {
		w.held -= h.n
	}
}

// addLocal adds amount to a shared window's settled usage of generation. Requests
// settle in any order, so a record of an older generation can settle after a newer
// one.
func (w *window) addLocal(generation uint64, amount int64) {
	for i := len(w.local) - 1; i >= 0; i-- {
		if w.local[i].generation == generation {
			w.local[i].n += amount
			return
		}
	}
	w.local = append(w.local, generationAmount{generation: generation, n: amount})
}

// counted drops a shared window's settled usage of every generation up to
// generation: the control plane's totals include it now.
func (w *window) counted(generation uint64) {
	kept := w.local[:0]
	for _, l := range w.local {
		if l.generation <= generation {
			w.used -= l.n
			continue
		}
		kept = append(kept, l)
	}
	w.local = kept
}

// setBase takes the control plane's used amount for the window starting at start, at
// now. The window never goes back, with one exception: a window ahead of the gateway's
// own clock window got there from an earlier push (a control-plane clock that ran
// ahead), and a push naming the gateway's clock window takes it back there — only the
// control plane's newest window moves the window past the gateway's clock. Otherwise
// enforcement would count nothing pushed until the gateway's clock caught up.
func (w *window) setBase(now, start time.Time, used int64) {
	w.baseStart, w.base = start.Unix(), used
	clock := windowStart(w.kind, now).Unix()
	if w.start > clock && w.baseStart == clock {
		w.clear(clock)
	}
}

// clampNegative sets a negative count back to 0 and reports whether there was one.
// No count goes below 0 — every release takes back what its hold added, in the
// incarnation or bucket it was added to — so one is a bug; clamped, it cannot turn
// into a saturated "everything used" (saturatingAdd assumes non-negative counts).
func (w *window) clampNegative() bool {
	negative := w.used < 0 || w.held < 0
	w.used, w.held = max(w.used, 0), max(w.held, 0)
	for i := range w.n {
		negative = negative || w.n[i] < 0
		w.n[i] = max(w.n[i], 0)
	}
	return negative
}

// admits reports whether the window has room for need more at now. need 0 asks
// whether the window is still below its limit (the check for amounts known only
// after the request, cost).
func (w *window) admits(now time.Time, need int64) bool {
	return fits(w.usedAt(now), need, w.limit)
}

// waitFor is how long until the window admits need (0 when it already does). A fixed
// window frees everything at its end; a sliding one as its oldest buckets expire. When
// need exceeds the limit itself the window never admits it; the wait returned is then
// until the window is empty.
func (w *window) waitFor(now time.Time, need int64) time.Duration {
	if w.admits(now, need) {
		return 0
	}
	if w.kind != SlidingMinute {
		return windowEnd(w.kind, time.Unix(w.start, 0).UTC()).Sub(now)
	}
	used := w.usedAt(now)
	var wait time.Duration
	for _, b := range w.liveBuckets(now) {
		used -= b.n
		wait = time.Unix(b.at+minuteBuckets, 0).Sub(now)
		if fits(used, need, w.limit) {
			return wait
		}
	}
	return wait
}

// resetIn is how long until the window holds nothing: the x-ratelimit-reset-* value.
func (w *window) resetIn(now time.Time) time.Duration {
	if w.kind != SlidingMinute {
		if w.usedAt(now) == 0 {
			return 0
		}
		return windowEnd(w.kind, time.Unix(w.start, 0).UTC()).Sub(now)
	}
	buckets := w.liveBuckets(now)
	if len(buckets) == 0 {
		return 0
	}
	return time.Unix(buckets[len(buckets)-1].at+minuteBuckets, 0).Sub(now)
}

// liveBuckets lists the non-empty minute buckets inside the window at now, oldest
// first.
func (w *window) liveBuckets(now time.Time) []hold {
	var out []hold
	for i := range minuteBuckets {
		if live(w.sec[i], now) && w.n[i] != 0 {
			out = append(out, hold{at: w.sec[i], n: w.n[i]})
		}
	}
	slices.SortFunc(out, func(a, b hold) int { return cmp.Compare(a.at, b.at) })
	return out
}

// settled is the amount counted in the current fixed window minus unsettled
// reservations: what the file-mode snapshot keeps.
func (w *window) settled(now time.Time) int64 {
	w.roll(now)
	return w.used - w.held
}

// copySettled returns a copy of w without its unsettled reservations — they belong to
// the requests holding w and settle there: a limit carried over from w to a second
// new identity starts from what w counted.
func (w *window) copySettled(now time.Time) *window {
	w.roll(now)
	c := *w
	c.local = slices.Clone(w.local)
	c.used -= c.held
	c.held = 0
	return &c
}
