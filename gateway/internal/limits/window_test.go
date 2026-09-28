package limits

import (
	"math"
	"testing"
	"time"
)

func at(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestSlidingMinuteTripsAndSlides(t *testing.T) {
	w := newWindow(SlidingMinute, 3)
	t0 := at("2026-09-24T10:00:00Z")
	for _, offset := range []time.Duration{0, 20 * time.Second, 40 * time.Second} {
		now := t0.Add(offset)
		if !w.admits(now, 1) {
			t.Fatalf("refused at +%v with room left", offset)
		}
		w.reserve(now, 1)
	}
	now := t0.Add(50 * time.Second)
	if w.admits(now, 1) {
		t.Fatal("admitted a fourth request within the minute")
	}
	// The oldest bucket (t0) leaves the window at t0+60s.
	if got := w.waitFor(now, 1); got != 10*time.Second {
		t.Errorf("wait %v, want 10s", got)
	}
	if got := w.resetIn(now); got != 50*time.Second {
		t.Errorf("reset %v, want 50s (the newest bucket expires at +100s)", got)
	}
	if !w.admits(t0.Add(60*time.Second), 1) {
		t.Error("still refused once the first request slid out")
	}
	if got := w.usedAt(t0.Add(80 * time.Second)); got != 1 {
		t.Errorf("used %d at +80s, want 1 (only the +40s request)", got)
	}
	if got := w.usedAt(t0.Add(100 * time.Second)); got != 0 {
		t.Errorf("used %d at +100s, want 0", got)
	}
}

func TestSlidingMinuteWaitsForEnoughRoom(t *testing.T) {
	w := newWindow(SlidingMinute, 100)
	t0 := at("2026-09-24T10:00:00Z")
	w.reserve(t0, 30)
	w.reserve(t0.Add(10*time.Second), 30)
	w.reserve(t0.Add(20*time.Second), 30)
	now := t0.Add(30 * time.Second)
	// 90 used; 50 more fits once 20 have left… after two buckets expire (t0+10 → +70s).
	if got := w.waitFor(now, 50); got != 40*time.Second {
		t.Errorf("wait %v, want 40s", got)
	}
	// Larger than the limit itself: never fits; the wait is until the window is empty.
	if got := w.waitFor(now, 101); got != 50*time.Second {
		t.Errorf("wait for an oversize request %v, want 50s", got)
	}
}

func TestUTCHourTripsAndRecoversAtTheHour(t *testing.T) {
	w := newWindow(UTCHour, 500)
	now := at("2026-09-24T10:59:30Z")
	w.reserve(now, 500)
	if w.admits(now, 1) {
		t.Fatal("admitted past the hourly limit")
	}
	if got := w.waitFor(now, 1); got != 30*time.Second {
		t.Errorf("wait %v, want 30s to the top of the hour", got)
	}
	next := at("2026-09-24T11:00:00Z")
	if !w.admits(next, 500) || w.usedAt(next) != 0 {
		t.Errorf("the new hour starts with %d used", w.usedAt(next))
	}
}

func TestUTCMonthTripsAndRecoversAtTheMonth(t *testing.T) {
	w := newWindow(UTCMonth, 1000)
	now := at("2026-12-31T23:00:00Z")
	w.add(now, 1000, false, 0)
	// A cost check asks only whether the window is below its limit.
	if w.admits(now, 0) {
		t.Fatal("admitted with the month's budget spent")
	}
	if got := w.waitFor(now, 0); got != time.Hour {
		t.Errorf("wait %v, want 1h to the new year", got)
	}
	if !w.admits(at("2027-01-01T00:00:00Z"), 0) {
		t.Error("the new month still refuses")
	}
	// Months are calendar months, in UTC whatever the clock's zone.
	local := time.FixedZone("UTC+3", 3*3600)
	if got := windowStart(UTCMonth, time.Date(2026, 10, 1, 1, 0, 0, 0, local)); !got.Equal(at("2026-09-01T00:00:00Z")) {
		t.Errorf("month start %v, want September (it is still September in UTC)", got)
	}
}

func TestReleaseSettleAndKeep(t *testing.T) {
	for _, kind := range []Kind{SlidingMinute, UTCHour} {
		w := newWindow(kind, 1000)
		now := at("2026-09-24T10:30:00Z")
		h := w.reserve(now, 400)
		w.release(now.Add(5*time.Second), h)
		w.add(now.Add(5*time.Second), 120, false, 0)
		if got := w.usedAt(now.Add(5 * time.Second)); got != 120 {
			t.Errorf("kind %d: used %d after settling, want 120", kind, got)
		}
		later := now.Add(10 * time.Second)
		h = w.reserve(later, 50)
		w.release(later, h)
		if got := w.usedAt(later); got != 120 {
			t.Errorf("kind %d: used %d after a release, want 120", kind, got)
		}
	}

	// A reservation from a window that has passed is not counted any more: releasing
	// it must not take from the new window.
	w := newWindow(UTCHour, 1000)
	h := w.reserve(at("2026-09-24T10:59:00Z"), 300)
	later := at("2026-09-24T11:01:00Z")
	w.add(later, 10, false, 0)
	w.release(later, h)
	w.add(later, 200, false, 0)
	if got := w.usedAt(later); got != 210 {
		t.Errorf("used %d, want 210 (the actual counted in the new hour)", got)
	}

	// Kept reservations become settled usage; open ones are not settled.
	w = newWindow(UTCHour, 1000)
	now := at("2026-09-24T10:00:00Z")
	kept := w.reserve(now, 5)
	w.reserve(now, 7)
	w.keep(now, kept)
	if got := w.settled(now); got != 5 {
		t.Errorf("settled %d, want 5", got)
	}
}

// Window arithmetic never overflows: a need near the int64 maximum on a window that
// already counts something is refused, never admitted by a wrapped sum, and counts
// near the maximum saturate instead of wrapping.
func TestWindowArithmeticDoesNotOverflow(t *testing.T) {
	now := at("2026-09-24T10:00:30Z")
	for _, kind := range []Kind{SlidingMinute, UTCHour, UTCMonth} {
		w := newWindow(kind, 1000)
		w.reserve(now, 10)
		if w.admits(now, math.MaxInt64) {
			t.Errorf("kind %d: admitted the int64 maximum with 10 used", kind)
		}
		if w.waitFor(now, math.MaxInt64) == 0 {
			t.Errorf("kind %d: no wait for the int64 maximum", kind)
		}
		w.add(now, math.MaxInt64, false, 0)
		if got := w.usedAt(now); got != math.MaxInt64 {
			t.Errorf("kind %d: used %d after adding the int64 maximum, want it saturated", kind, got)
		}
		if w.admits(now, 1) || w.admits(now, 0) {
			t.Errorf("kind %d: a saturated window admits", kind)
		}
	}
}

// A count that went negative — a bug, as every release takes back what its hold
// added — is detected and clamped to 0, never read as saturated usage.
func TestNegativeCountIsClampedNotSaturated(t *testing.T) {
	now := at("2026-09-24T10:30:00Z")
	w := newWindow(UTCHour, 1000)
	h := w.reserve(now, 900)
	w.release(now, h)
	w.release(now, h) // released twice: the bug
	if !w.clampNegative() {
		t.Error("negative count not detected")
	}
	if got := w.usedAt(now); got != 0 {
		t.Errorf("used %d, want 0", got)
	}
	if w.clampNegative() {
		t.Error("clamped count still reads negative")
	}
	if !w.admits(now, 1000) {
		t.Error("the full limit is refused on an empty window")
	}
}
