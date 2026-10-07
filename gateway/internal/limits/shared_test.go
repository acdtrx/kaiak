package limits

import (
	"bytes"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"kaiak/internal/accounting"
	"kaiak/internal/config"
)

// contactState is the tests' control-plane contact, for NewShared.
type contactState struct {
	mu        sync.Mutex
	connected bool
	last      time.Time
	waiting   time.Time
	uncounted time.Time
}

func (c *contactState) get() Contact {
	c.mu.Lock()
	defer c.mu.Unlock()
	return Contact{Connected: c.connected, Last: c.last, UsageWaitingSince: c.waiting, UsageUncountedSince: c.uncounted}
}

func (c *contactState) setUncounted(since time.Time) {
	c.mu.Lock()
	c.uncounted = since
	c.mu.Unlock()
}

func (c *contactState) setWaiting(since time.Time) {
	c.mu.Lock()
	c.waiting = since
	c.mu.Unlock()
}

func (c *contactState) set(connected bool, last time.Time) {
	c.mu.Lock()
	c.connected, c.last = connected, last
	c.mu.Unlock()
}

// shared returns a control-plane-mode limiter on the clock, in contact.
func (c *clock) shared(h *config.Holder) (*Limiter, *contactState) {
	cs := &contactState{connected: true, last: c.t}
	return NewShared(h, c.now, cs.get, nil), cs
}

const (
	hourLimit = `[{ "type": "tokens_per_hour", "value": 1000 }]`
	usdLimit  = `[{ "type": "usd_per_month", "value": 1 }]`
)

// inGeneration is rec sealed in usage batch generation g.
func inGeneration(g uint64, rec accounting.UsageRecord) accounting.UsageRecord {
	rec.Generation = g
	return rec
}

func pushedWindow(group string, typ config.LimitType, start string, used int64) PushedWindow {
	return PushedWindow{Group: group, Type: typ, Start: at(start), Used: used}
}

func TestSharedWindowsCountPushedTotalsPlusOwnUsage(t *testing.T) {
	c := newClock("2026-09-24T10:30:00Z")
	l, _ := c.shared(holderOf(snapshot(t, limitsDoc{workload: hourLimit, team: usdLimit})))
	l.TakeTotals(Totals{LiveGateways: 2, Windows: []PushedWindow{
		pushedWindow("w", config.LimitTokensPerHour, "2026-09-24T10:00:00Z", 600),
		pushedWindow("t", config.LimitUSDPerMonth, "2026-09-01T00:00:00Z", 900_000_000),
	}}, 0)

	// 600 pushed + a 300 reservation fits 1000; another 200 does not.
	res := admitN(t, l, workload, 1, 300)[0]
	if rej := refused(t, l, workload, 200); rej.Group != "w" || rej.Used != 900 || rej.Limit != 1000 {
		t.Errorf("rejection %+v, want the workload hour limit at 900 of 1000", rej)
	}
	// Settled at 50 tokens and 0.1 USD: the team's money is spent (0.9 pushed + 0.1).
	l.Settle(res, record(40, 0, 0, 10, 0, 100_000_000))
	if got := used(t, l, "w", config.LimitTokensPerHour); got != 650 {
		t.Errorf("hour used %d, want 600 pushed + 50 own", got)
	}
	if rej := refused(t, l, workload, 10); rej.Group != "t" || rej.Measure != MeasureCost {
		t.Errorf("rejection %+v, want the team's USD limit", rej)
	}
	// m2 is unpriced: no budget counts it.
	admitN(t, l, workload.on("m2"), 1, 10)
}

// The control plane counts a batch; the push that includes it reports its generation
// counted: the gateway's own copy leaves in the same step the totals holding it
// arrive, never before and never twice.
func TestCountedUsageIsNeitherDoubledNorDropped(t *testing.T) {
	c := newClock("2026-09-24T10:30:00Z")
	l, _ := c.shared(holderOf(snapshot(t, limitsDoc{workload: hourLimit})))
	hour := func() int64 { return used(t, l, "w", config.LimitTokensPerHour) }

	const sealed = 1 // batch 1 holds the first record, batch 2 the second
	l.Settle(admitN(t, l, workload, 1, 10)[0], inGeneration(sealed, record(100, 0, 0, 0, 0, 0)))
	l.Settle(admitN(t, l, workload, 1, 10)[0], inGeneration(2, record(50, 0, 0, 0, 0, 0)))
	if got := hour(); got != 150 {
		t.Fatalf("hour used %d, want 150 own", got)
	}

	// Another gateway used 600; the push counts batch 1 (700 in all).
	window := pushedWindow("w", config.LimitTokensPerHour, "2026-09-24T10:00:00Z", 700)
	l.TakeTotals(Totals{Windows: []PushedWindow{window}}, sealed)
	if got := hour(); got != 750 {
		t.Errorf("after the push: hour used %d, want 700 pushed + 50 not yet counted", got)
	}
	// The same totals again (nothing new counted): batch 1 is not retired twice.
	l.TakeTotals(Totals{Windows: []PushedWindow{window}}, sealed)
	if got := hour(); got != 750 {
		t.Errorf("after the same push: hour used %d, want 750 still", got)
	}
	// The push that counts batch 2: its 50 move from the own usage into the base.
	l.TakeTotals(Totals{Windows: []PushedWindow{
		pushedWindow("w", config.LimitTokensPerHour, "2026-09-24T10:00:00Z", 750)}}, 2)
	if got := hour(); got != 750 {
		t.Errorf("after batch 2 counted: hour used %d, want 750 pushed and nothing own", got)
	}
}

// A pushed window newer than the local one starts that window — the control plane's
// clock leads. Usage the gateway settled in its 10:00 hour is counted in 10:00 by the
// control plane too (its previous window), so it does not follow into 11:00.
func TestPushedWindowRolloverMidBatch(t *testing.T) {
	c := newClock("2026-09-24T10:59:58Z")
	l, _ := c.shared(holderOf(snapshot(t, limitsDoc{workload: hourLimit})))
	hour := func() int64 { return used(t, l, "w", config.LimitTokensPerHour) }
	l.TakeTotals(Totals{Windows: []PushedWindow{
		pushedWindow("w", config.LimitTokensPerHour, "2026-09-24T10:00:00Z", 500)}}, 0)
	const sealed = 1
	l.Settle(admitN(t, l, workload, 1, 10)[0], inGeneration(sealed, record(100, 0, 0, 0, 0, 0)))
	if got := hour(); got != 600 {
		t.Fatalf("hour used %d, want 600", got)
	}
	// Mid-batch, the control plane's 11:00 window starts (30 from others).
	l.TakeTotals(Totals{Windows: []PushedWindow{
		pushedWindow("w", config.LimitTokensPerHour, "2026-09-24T11:00:00Z", 30)}}, 0)
	if got := hour(); got != 30 {
		t.Errorf("hour used %d, want 30 pushed for 11:00 (the 100 belongs to 10:00)", got)
	}
	// The batch lands in 10:00 at the control plane.
	l.TakeTotals(Totals{Windows: []PushedWindow{
		pushedWindow("w", config.LimitTokensPerHour, "2026-09-24T11:00:00Z", 30)}}, sealed)
	if got := hour(); got != 30 {
		t.Errorf("hour used %d, want 30", got)
	}
	// The gateway's clock reaching 11:00 changes nothing; 12:00 with no push yet
	// leaves the 11:00 base behind.
	c.set("2026-09-24T11:00:01Z")
	if got := hour(); got != 30 {
		t.Errorf("hour used %d, want 30", got)
	}
	c.set("2026-09-24T12:00:00Z")
	if got := hour(); got != 0 {
		t.Errorf("hour used %d at 12:00, want 0", got)
	}
}

// N-M2, the reviewer's scenario: the control plane's clock steps 90 minutes ahead and
// pushes a 12:00 window while the gateway is in 10:00 — a warning, and the window
// moves to 12:00 (the control plane's newest window leads). Its clock fixed, the
// control plane pushes 10:00 again: that matches the gateway's own clock window, so
// it is taken — the window goes back and the hour limit is enforced again, instead
// of counting nothing pushed until the gateway's clock reaches 12:00.
func TestEarlierPushedWindowMatchingTheGatewayClockIsTaken(t *testing.T) {
	c := newClock("2026-09-24T10:30:00Z")
	var logs bytes.Buffer
	cs := &contactState{connected: true, last: c.t}
	l := NewShared(holderOf(snapshot(t, limitsDoc{workload: hourLimit})), c.now, cs.get,
		slog.New(slog.NewTextHandler(&logs, nil)))
	hour := func() int64 { return used(t, l, "w", config.LimitTokensPerHour) }
	push := func(start string, n int64) {
		l.TakeTotals(Totals{Windows: []PushedWindow{
			pushedWindow("w", config.LimitTokensPerHour, start, n)}}, 0)
	}
	push("2026-09-24T10:00:00Z", 700)
	if got := hour(); got != 700 {
		t.Fatalf("hour used %d, want 700", got)
	}
	push("2026-09-24T12:00:00Z", 0)
	if got := hour(); got != 0 {
		t.Errorf("hour used %d after the 12:00 push, want 0 (the control plane's newest window)", got)
	}
	if !strings.Contains(logs.String(), `msg="pushed window ahead of the gateway's clock"`) {
		t.Errorf("no warning for the window ahead of the gateway's clock:\n%s", logs.String())
	}
	push("2026-09-24T10:00:00Z", 800)
	if got := hour(); got != 800 {
		t.Errorf("hour used %d after the control plane's 10:00 push again, want 800", got)
	}
	if rej := refused(t, l, workload, 300); rej.Used != 800 || rej.Limit != 1000 {
		t.Errorf("rejection %+v, want the hour limit at 800 of 1000", rej)
	}
	// An earlier window that is not the gateway's own clock window does not move it
	// back: 09:00 is the control plane lagging.
	push("2026-09-24T09:00:00Z", 50)
	if got := hour(); got != 0 {
		t.Errorf("hour used %d after a 09:00 push, want 0 (nothing pushed for 10:00)", got)
	}
}

// D4, [A]'s scenario: a workload using 60% of its hour limit every hour through a
// 2-hour outage. Uncounted usage stays in the window it was settled in — the control
// plane counts it there too (its gateway_time window, current or previous) — so each
// new hour starts from nothing of its own, and nothing is refused.
func TestUncountedUsageStaysInItsOwnWindow(t *testing.T) {
	c := newClock("2026-09-24T10:00:00Z")
	l, contact := c.shared(holderOf(snapshot(t, limitsDoc{workload: hourLimit})))
	hour := func() int64 { return used(t, l, "w", config.LimitTokensPerHour) }
	l.TakeTotals(Totals{}, 0)
	contact.set(false, c.t) // no totals and no acks from here: an outage
	generation := uint64(1)
	for range 3 {
		for range 6 {
			rec := inGeneration(generation, record(100, 0, 0, 0, 0, 0))
			rec.GatewayTime = c.t
			l.Settle(admitN(t, l, workload, 1, 100)[0], rec)
			c.advance(10 * time.Minute)
			generation++
		}
		if got := hour(); got != 0 {
			t.Errorf("hour used %d at the start of a new hour, want 0: the last hour's usage stays there", got)
		}
	}

	// A record settled in the last second of an hour and handed to the limiter in
	// the next is not charged to the new hour.
	c.set("2026-09-24T13:00:00.5Z")
	rec := inGeneration(generation, record(100, 0, 0, 0, 0, 0))
	rec.GatewayTime = at("2026-09-24T12:59:59.9Z")
	l.Settle(admitN(t, l, workload, 1, 100)[0], rec)
	if got := hour(); got != 0 {
		t.Errorf("hour used %d, want 0: the record belongs to 12:00", got)
	}

	// Recovery: the backlog is counted and the totals for 13:00 apply.
	contact.set(true, c.t)
	l.TakeTotals(Totals{Windows: []PushedWindow{
		pushedWindow("w", config.LimitTokensPerHour, "2026-09-24T13:00:00Z", 600)}}, generation)
	if got := hour(); got != 600 {
		t.Errorf("hour used %d after recovery, want the control plane's 600", got)
	}
}

// Every scope is counted, limited or not: complete totals set every base (0 for a
// scope not listed); later totals replace only the bases they list, a window at "0"
// included; a scope the config does not have keeps its pushed window for a reload
// that adds it; a limit added by a reload checks the count so far.
func TestTotalsMatching(t *testing.T) {
	c := newClock("2026-09-24T10:30:00Z")
	h := holderOf(snapshot(t, limitsDoc{workload: hourLimit}))
	l, _ := c.shared(h)
	hourW := pushedWindow("w", config.LimitTokensPerHour, "2026-09-24T10:00:00Z", 400)
	hourT := pushedWindow("t", config.LimitTokensPerHour, "2026-09-24T10:00:00Z", 250)
	monthT := pushedWindow("t", config.LimitUSDPerMonth, "2026-09-01T00:00:00Z", 1)
	elsewhere := pushedWindow("gone", config.LimitTokensPerHour, "2026-09-24T10:00:00Z", 70)
	l.TakeTotals(Totals{Complete: true, Windows: []PushedWindow{hourW, hourT, monthT, elsewhere}}, 0)
	for _, w := range []PushedWindow{hourW, hourT, monthT} {
		if got := used(t, l, w.Group, w.Type); got != w.Used {
			t.Errorf("%s %s %d, want %d whether or not it has a limit", w.Group, w.Type, got, w.Used)
		}
	}

	// Changes only: what is not listed keeps its base.
	l.TakeTotals(Totals{Windows: []PushedWindow{pushedWindow("t", config.LimitTokensPerHour, "2026-09-24T10:00:00Z", 300)}}, 0)
	if got := used(t, l, "w", config.LimitTokensPerHour); got != 400 {
		t.Errorf("workload hour %d after a change elsewhere, want 400 kept", got)
	}
	if got := used(t, l, "t", config.LimitTokensPerHour); got != 300 {
		t.Errorf("team hour %d, want the listed 300", got)
	}
	// A window listed at "0" (the control plane's store restored to less) is 0.
	l.TakeTotals(Totals{Windows: []PushedWindow{pushedWindow("w", config.LimitTokensPerHour, "2026-09-24T10:00:00Z", 0)}}, 0)
	if got := used(t, l, "w", config.LimitTokensPerHour); got != 0 {
		t.Errorf("workload hour %d, want the listed 0", got)
	}

	// A reload adding the group whose window was pushed while the config lacked it,
	// and a limit on t: both count from the pushed bases.
	h.Swap(snapshot(t, limitsDoc{workload: hourLimit, team: hourLimit, extraGroup: "gone"}))
	if got := used(t, l, "t", config.LimitTokensPerHour); got != 300 {
		t.Errorf("team hour %d after the reload, want 300", got)
	}
	if got := used(t, l, "gone", config.LimitTokensPerHour); got != 70 {
		t.Errorf("added group's hour %d after the reload, want the 70 pushed before it", got)
	}

	// Complete totals again: a scope they do not list is back to 0.
	l.TakeTotals(Totals{Complete: true, Windows: []PushedWindow{hourT}}, 0)
	if got := used(t, l, "t", config.LimitUSDPerMonth); got != 0 {
		t.Errorf("team month %d, want 0: not listed in complete totals", got)
	}
	if got := used(t, l, "t", config.LimitTokensPerHour); got != 250 {
		t.Errorf("team hour %d, want 250", got)
	}
}

func TestPerMinuteShares(t *testing.T) {
	for _, tc := range []struct {
		name        string
		limit, live int64
		want        int64
	}{
		{"divided, rounded down", 10, 3, 3},
		{"no live gateways counts as one", 10, 0, 10},
		{"never below 1", 2, 5, 1},
		{"a zero limit stays zero", 0, 4, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := share(tc.limit, tc.live); got != tc.want {
				t.Errorf("share(%d, %d) = %d, want %d", tc.limit, tc.live, got, tc.want)
			}
		})
	}

	c := newClock("2026-09-24T10:30:00Z")
	l, _ := c.shared(holderOf(snapshot(t, limitsDoc{workload: `[{ "type": "requests_per_minute", "value": 10 }]`})))
	// Before any totals: one live gateway, the full limit.
	res := admitN(t, l, workload, 3, 10)
	if h := res[2].Headers().Requests; h == nil || h.Limit != 10 || h.Remaining != 7 {
		t.Errorf("headers %+v, want limit 10, remaining 7", h)
	}
	// Four live gateways: a share of 2, and the 3 counted stay counted.
	l.TakeTotals(Totals{LiveGateways: 4}, 0)
	rej := refused(t, l, workload, 10)
	if rej.Limit != 2 || rej.Used != 3 || rej.Headers.Requests == nil || rej.Headers.Requests.Limit != 2 {
		t.Errorf("rejection %+v, want the share 2 with 3 used, in the headers too", rej)
	}
	l.TakeTotals(Totals{LiveGateways: 1}, 0)
	admitN(t, l, workload, 7, 10)
}

func TestOutageRefusesMoneyLimitedModelsAfterTheGrace(t *testing.T) {
	c := newClock("2026-09-24T10:30:00Z")
	l, contact := c.shared(holderOf(snapshot(t, limitsDoc{team: usdLimit, grace: "60000"})))
	l.TakeTotals(Totals{}, 0) // the first totals: the spend is known
	lost := c.t
	contact.set(false, lost)

	c.advance(time.Minute) // at the grace: still serving
	if l.Outage() {
		t.Fatal("outage at the grace, want only past it")
	}
	admitN(t, l, workload, 1, 10)

	c.advance(time.Millisecond)
	if !l.Outage() {
		t.Fatal("no outage past the grace")
	}
	rej := refused(t, l, workload, 10)
	if !rej.Unavailable || rej.Group != "t" || rej.Type != config.LimitUSDPerMonth {
		t.Errorf("rejection %+v, want unavailable, by the team's USD limit", rej)
	}
	// m2 has no USD limit, and ann no limits at all: they keep serving.
	admitN(t, l, workload.on("m2"), 1, 10)
	admitN(t, l, ann, 1, 10)

	// Any contact ends it.
	contact.set(true, c.t)
	if l.Outage() {
		t.Error("outage while the stream is open")
	}
	admitN(t, l, workload, 1, 10)
	contact.set(false, c.t)
	admitN(t, l, workload, 1, 10)

	// File mode never has an outage.
	if New(holderOf(snapshot(t, limitsDoc{team: usdLimit, grace: "0"})), c.now, nil).Outage() {
		t.Error("file mode reported an outage")
	}
}

// A grace of 0 fails closed as soon as contact is lost, but never while the stream
// is open (heartbeats are 15 s apart).
func TestZeroGrace(t *testing.T) {
	c := newClock("2026-09-24T10:30:00Z")
	l, contact := c.shared(holderOf(snapshot(t, limitsDoc{team: usdLimit, grace: "0"})))
	l.TakeTotals(Totals{}, 0)
	contact.set(true, c.t.Add(-time.Minute))
	admitN(t, l, workload, 1, 10)
	contact.set(false, c.t)
	c.advance(time.Millisecond)
	if rej := refused(t, l, workload, 10); !rej.Unavailable {
		t.Errorf("rejection %+v, want unavailable", rej)
	}
}

// A record settled after totals showed its batch counted — a retried attempt's record
// published mid-request, or a push faster than the request's end — is already inside
// the applied base: it is not counted again as the gateway's own.
func TestUsageSettledAfterItsBatchIsCountedIsNotCountedTwice(t *testing.T) {
	c := newClock("2026-09-24T10:30:00Z")
	l, _ := c.shared(holderOf(snapshot(t, limitsDoc{workload: hourLimit})))
	hour := func() int64 { return used(t, l, "w", config.LimitTokensPerHour) }

	res := admitN(t, l, workload, 1, 10)[0]
	// Batch 1 holds the request's record; totals counting it arrive before the request
	// settles.
	l.TakeTotals(Totals{Windows: []PushedWindow{
		pushedWindow("w", config.LimitTokensPerHour, "2026-09-24T10:00:00Z", 600)}}, 1)
	if got := hour(); got != 610 {
		t.Fatalf("hour used %d before settlement, want 600 pushed + the 10 reserved", got)
	}
	l.Settle(res, inGeneration(1, record(600, 0, 0, 0, 0, 0)))
	if got := hour(); got != 600 {
		t.Errorf("hour used %d, want 600: the record is in the pushed base", got)
	}
	// A record of the next batch still counts as the gateway's own.
	l.Settle(admitN(t, l, workload, 1, 10)[0], inGeneration(2, record(5, 0, 0, 0, 0, 0)))
	if got := hour(); got != 605 {
		t.Errorf("hour used %d, want 605", got)
	}
}

// Requests settle in any order: a record of an older batch settling after one of a
// newer batch still leaves when its batch is counted.
func TestOlderGenerationSettledLateLeavesWithItsBatch(t *testing.T) {
	c := newClock("2026-09-24T10:30:00Z")
	l, _ := c.shared(holderOf(snapshot(t, limitsDoc{workload: hourLimit})))
	hour := func() int64 { return used(t, l, "w", config.LimitTokensPerHour) }
	l.Settle(admitN(t, l, workload, 1, 10)[0], inGeneration(2, record(50, 0, 0, 0, 0, 0)))
	l.Settle(admitN(t, l, workload, 1, 10)[0], inGeneration(1, record(100, 0, 0, 0, 0, 0)))
	l.TakeTotals(Totals{Windows: []PushedWindow{
		pushedWindow("w", config.LimitTokensPerHour, "2026-09-24T10:00:00Z", 100)}}, 1)
	if got := hour(); got != 150 {
		t.Errorf("hour used %d, want 100 pushed (batch 1) + 50 of batch 2", got)
	}
}

// Totals apply whatever config the gateway runs: windows are counted per scope and
// type whatever the config, so a gateway that keeps its own config (it rejected the
// control plane's) enforces its budgets on the stream's totals, and the usage they
// show counted leaves its own count. A config applied later keeps them by group and
// type.
func TestTotalsApplyWhateverTheConfig(t *testing.T) {
	c := newClock("2026-09-24T10:30:00Z")
	doc := limitsDoc{team: usdLimit, workload: hourLimit, grace: "60000"}
	h := holderOf(snapshot(t, doc))
	l, _ := c.shared(h)
	month := func() int64 { return used(t, l, "t", config.LimitUSDPerMonth) }
	hour := func() int64 { return used(t, l, "w", config.LimitTokensPerHour) }
	spent := pushedWindow("t", config.LimitUSDPerMonth, "2026-09-01T00:00:00Z", 1_100_000_000)
	l.TakeTotals(Totals{Windows: []PushedWindow{spent}}, 0)
	if rej := refused(t, l, workload, 10); rej.Measure != MeasureCost || rej.Unavailable {
		t.Fatalf("rejection %+v, want the spent budget", rej)
	}
	// A request on m2 (unpriced: no budget refuses it) is recorded in batch 1.
	l.Settle(admitN(t, l, workload.on("m2"), 1, 10)[0], inGeneration(1, record(10, 0, 0, 0, 0, 50_000_000)))

	// The control plane runs a config this gateway rejected; its totals keep coming,
	// batch 1 counted in them. They are applied: the spend grows, batch 1 leaves the
	// own usage, and past the grace the budget is refused as spent — never unavailable.
	widened := pushedWindow("t", config.LimitUSDPerMonth, "2026-09-01T00:00:00Z", 1_150_000_000)
	counted := pushedWindow("w", config.LimitTokensPerHour, "2026-09-24T10:00:00Z", 10)
	l.TakeTotals(Totals{Windows: []PushedWindow{widened, counted}}, 1)
	if got := month(); got != 1_150_000_000 {
		t.Errorf("team month used %d, want the stream's 1.15 USD", got)
	}
	if got := hour(); got != 10 {
		t.Errorf("workload hour used %d, want the pushed 10 and batch 1 retired", got)
	}
	c.advance(time.Minute + time.Millisecond)
	if rej := refused(t, l, workload, 10); rej.Measure != MeasureCost || rej.Unavailable {
		t.Errorf("rejection %+v past the grace, want the spent budget", rej)
	}

	// A config raising the budget applies: the window is kept by group and type.
	h.Swap(snapshot(t, limitsDoc{team: `[{ "type": "usd_per_month", "value": 2 }]`, workload: hourLimit, grace: "60000"}))
	if got := month(); got != 1_150_000_000 {
		t.Errorf("team month used %d under the new config, want 1.15 USD kept", got)
	}
	admitN(t, l, workload, 1, 10)
}

// D6: a model with no price in force costs nothing, so no budget ever refuses it —
// neither a spent USD limit nor the outage refusal, which exists because spend is
// unknown; priced models under the same limit are refused.
func TestUnpricedModelsAreNeverRefusedForBudgets(t *testing.T) {
	c := newClock("2026-09-24T10:30:00Z")
	l, contact := c.shared(holderOf(snapshot(t, limitsDoc{global: `[{ "type": "usd_per_month", "value": 1 }]`, grace: "60000"})))

	// Outage past the grace: priced m1 is unavailable, free m2 serves.
	contact.set(false, c.t)
	c.advance(2 * time.Minute)
	if rej := refused(t, l, workload, 10); !rej.Unavailable || rej.Scope != ScopeGlobal {
		t.Errorf("m1 in outage: %+v, want unavailable by the global USD limit", rej)
	}
	admitN(t, l, workload.on("m2"), 1, 10)

	// Back in contact with the budget spent: m1 refused, m2 still serves and spends
	// nothing.
	contact.set(true, c.t)
	l.TakeTotals(Totals{Windows: []PushedWindow{
		pushedWindow("", config.LimitUSDPerMonth, "2026-09-01T00:00:00Z", 1_000_000_000),
	}}, 0)
	if rej := refused(t, l, workload, 10); rej.Unavailable || rej.Measure != MeasureCost {
		t.Errorf("m1 with the budget spent: %+v, want the USD limit refusing", rej)
	}
	res := admitN(t, l, workload.on("m2"), 1, 10)[0]
	l.Settle(res, inGeneration(1, record(10, 0, 0, 10, 0, 0)))
	if got := used(t, l, "", config.LimitUSDPerMonth); got != 1_000_000_000 {
		t.Errorf("global USD used %d, want the pushed 1 USD only", got)
	}
}

// [A]'s numbers: 60 000 tokens a minute over 4 gateways is a 15 000 share, below one
// request at the 16 384 default output. A share never makes a request impossible: it
// is admitted when the gateway's window is empty, as long as it fits the full limit;
// only a request above the full limit is "too large". The config apply warns.
func TestShareNeverMakesARequestImpossible(t *testing.T) {
	c := newClock("2026-09-24T10:30:00Z")
	var logs bytes.Buffer
	cs := &contactState{connected: true, last: c.t}
	l := NewShared(holderOf(snapshot(t, limitsDoc{team: `[{ "type": "tokens_per_minute", "value": 60000 }]`})), c.now, cs.get,
		slog.New(slog.NewTextHandler(&logs, nil)))
	l.TakeTotals(Totals{LiveGateways: 4}, 0)
	const request = 16384 + 100

	res := admitN(t, l, workload, 1, request)[0]
	// The window holds a request now: the next one waits for it to leave.
	rej := refused(t, l, workload, request)
	if rej.Limit != 15000 || rej.Max != 60000 || rej.RetryAfter <= 0 || rej.RetryAfter > time.Minute {
		t.Errorf("rejection %+v, want the 15 000 share of 60 000, retry within the minute", rej)
	}
	l.Settle(res, inGeneration(1, record(100, 0, 0, 16384, 0, 0)))
	c.advance(61 * time.Second)
	admitN(t, l, workload, 1, request)
	c.advance(61 * time.Second)
	if rej := refused(t, l, workload, 60001); rej.Requested <= rej.Max {
		t.Errorf("rejection %+v, want a request above the full limit", rej)
	}

	if n := strings.Count(logs.String(), "per-minute share below the model's default output"); n != 1 {
		t.Errorf("%d share warnings, want 1:\n%s", n, logs.String())
	}
	if !strings.Contains(logs.String(), "kaiak.model.name=m1") || !strings.Contains(logs.String(), "kaiak.limit.scope=group") {
		t.Errorf("warning names the model and scope kind:\n%s", logs.String())
	}
	// The same live count again: no new warning.
	l.TakeTotals(Totals{LiveGateways: 4}, 0)
	if n := strings.Count(logs.String(), "per-minute share below"); n != 1 {
		t.Errorf("%d share warnings after the same count, want 1", n)
	}
}

// The small-share warning names only models the limit's scope may use: a group allowed
// only m2 (no default output) gets no warning about m1's ([G] L1 in the 2026-10-07
// review).
func TestShareWarningNamesOnlyModelsTheScopeMayUse(t *testing.T) {
	c := newClock("2026-09-24T10:30:00Z")
	var logs bytes.Buffer
	cs := &contactState{connected: true, last: c.t}
	doc := limitsDoc{team: `[{ "type": "tokens_per_minute", "value": 60000 }]`, teamModels: `["m2"]`}
	l := NewShared(holderOf(snapshot(t, doc)), c.now, cs.get, slog.New(slog.NewTextHandler(&logs, nil)))
	l.TakeTotals(Totals{LiveGateways: 4}, 0)
	if strings.Contains(logs.String(), "per-minute share below") {
		t.Errorf("a share warning for a model group t may not use:\n%s", logs.String())
	}
}

// O6: the outage's start is logged once as a warning, its end once at info — on
// the transitions, whatever the requests and scrapes in between.
func TestOutageStartAndEndAreLoggedOnce(t *testing.T) {
	c := newClock("2026-09-24T10:30:00Z")
	var logs bytes.Buffer
	contact := &contactState{connected: true, last: c.t}
	l := NewShared(holderOf(snapshot(t, limitsDoc{team: usdLimit, grace: "60000"})), c.now, contact.get,
		slog.New(slog.NewTextHandler(&logs, nil)))
	l.TakeTotals(Totals{}, 0)
	const (
		started = `level=WARN msg="control plane outage: priced USD-limited models refused"`
		over    = `level=INFO msg="control plane outage over: contact is back"`
	)
	lines := func(prefix string) []string {
		var out []string
		for line := range strings.SplitSeq(logs.String(), "\n") {
			if _, rest, ok := strings.Cut(line, prefix); ok {
				out = append(out, strings.TrimSpace(rest))
			}
		}
		return out
	}

	contact.set(false, c.t)
	c.advance(time.Minute) // at the grace: no outage yet
	l.Outage()
	admitN(t, l, workload, 1, 10)
	if got := lines("msg=\"control plane outage"); len(got) != 0 {
		t.Fatalf("outage logged at the grace: %v", got)
	}
	c.advance(time.Millisecond)
	for range 3 {
		refused(t, l, workload, 10)
		l.Outage()
	}
	if got := lines(started); len(got) != 1 || got[0] != `kaiak.reason="no contact" kaiak.control.since_contact=60.001 kaiak.control.outage_grace=60` {
		t.Errorf("start lines %q, want one, with the reason, the time since contact and the grace", got)
	}
	c.advance(30 * time.Second)
	contact.set(true, c.t)
	admitN(t, l, workload, 2, 10)
	l.Outage()
	if got := lines(over); len(got) != 1 || got[0] != "kaiak.lasted=30.001" {
		t.Errorf("end lines %q, want one, with how long it lasted", got)
	}

	// Usage waiting past the grace with the stream open: its own reason.
	logs.Reset()
	contact.setWaiting(c.t)
	c.advance(61 * time.Second)
	contact.set(true, c.t)
	refused(t, l, workload, 10)
	refused(t, l, workload, 10)
	if got := lines(started); len(got) != 1 ||
		got[0] != `kaiak.reason="usage not acknowledged" kaiak.control.since_contact=0 kaiak.control.outage_grace=60 kaiak.control.usage_waiting=61` {
		t.Errorf("start lines %q, want one, for the unacknowledged usage", got)
	}
	contact.setWaiting(time.Time{})
	admitN(t, l, workload, 1, 10)
	if got := lines(over); len(got) != 1 || got[0] != "kaiak.lasted=1" {
		t.Errorf("end lines %q, want one", got)
	}

	// Before the first totals, requests are refused for that alone; a request still
	// logs the outage once the grace has passed.
	logs.Reset()
	boot := &contactState{last: c.t}
	l = NewShared(holderOf(snapshot(t, limitsDoc{team: usdLimit, grace: "60000"})), c.now, boot.get,
		slog.New(slog.NewTextHandler(&logs, nil)))
	c.advance(time.Minute + time.Second)
	refused(t, l, workload, 10)
	if got := lines(started); len(got) != 1 {
		t.Errorf("start lines %q before the first totals, want one", got)
	}
}

// M16 at the limiter: an open stream is contact, but not while usage batches have
// waited past the grace for an answer.
func TestUsageWaitingPastTheGraceIsAnOutage(t *testing.T) {
	c := newClock("2026-09-24T10:30:00Z")
	l, contact := c.shared(holderOf(snapshot(t, limitsDoc{team: usdLimit, grace: "60000"})))
	l.TakeTotals(Totals{}, 0)
	contact.setWaiting(c.t)
	c.advance(59 * time.Second)
	contact.set(true, c.t)
	admitN(t, l, workload, 1, 10)
	c.advance(2 * time.Second)
	contact.set(true, c.t)
	if rej := refused(t, l, workload, 10); !rej.Unavailable {
		t.Errorf("rejection %+v, want unavailable", rej)
	}
	if !l.Outage() {
		t.Error("Outage() false with usage waiting past the grace")
	}
	contact.setWaiting(time.Time{})
	admitN(t, l, workload, 1, 10)
}

// Usage acknowledged but not shown counted past the grace is an outage, the stream
// and its heartbeats notwithstanding: totals that stopped coming leave the bases
// frozen, and each gateway would enforce only its own view (AUDIT-3 3H1). It is logged
// with its own reason.
func TestUsageNotShownCountedPastTheGraceIsAnOutage(t *testing.T) {
	c := newClock("2026-10-07T10:30:00Z")
	var logs bytes.Buffer
	h := holderOf(snapshot(t, limitsDoc{team: usdLimit, grace: "60000"}))
	contact := &contactState{connected: true, last: c.t}
	l := NewShared(h, c.now, contact.get, slog.New(slog.NewTextHandler(&logs, nil)))
	l.TakeTotals(Totals{}, 0)
	contact.setUncounted(c.t)
	c.advance(59 * time.Second)
	contact.set(true, c.t) // heartbeats keep coming
	admitN(t, l, workload, 1, 10)
	c.advance(2 * time.Second)
	contact.set(true, c.t)
	if rej := refused(t, l, workload, 10); !rej.Unavailable {
		t.Errorf("rejection %+v, want unavailable", rej)
	}
	if !strings.Contains(logs.String(), `kaiak.reason="usage not shown counted"`) {
		t.Errorf("outage not logged with its reason:\n%s", logs.String())
	}
	contact.setUncounted(time.Time{})
	admitN(t, l, workload, 1, 10)
}

// A changes-only stream never lists a window again once its hour or month is over:
// the pushed windows that ended are dropped, those of deleted scopes too, so a
// long-lived stream does not keep every scope it ever saw (AUDIT-3 3M1, [C] C8).
func TestEndedPushedWindowsAreDropped(t *testing.T) {
	c := newClock("2026-10-07T12:30:00Z")
	l, _ := c.shared(holderOf(snapshot(t, limitsDoc{})))
	l.TakeTotals(Totals{Complete: true, Windows: []PushedWindow{
		pushedWindow("deleted", config.LimitUSDPerMonth, "2026-10-01T00:00:00Z", 100),
		pushedWindow("deleted", config.LimitTokensPerHour, "2026-10-07T12:00:00Z", 100),
	}}, 0)
	c.set("2026-10-07T13:30:00Z")
	l.TakeTotals(Totals{}, 0)
	if _, kept := l.pushed[keyOf("deleted", config.LimitTokensPerHour)]; kept {
		t.Error("an ended hour window kept")
	}
	if _, kept := l.pushed[keyOf("deleted", config.LimitUSDPerMonth)]; !kept {
		t.Error("a current month window dropped")
	}
	c.set("2026-12-07T12:30:00Z")
	l.TakeTotals(Totals{}, 0)
	if _, kept := l.pushed[keyOf("deleted", config.LimitUSDPerMonth)]; kept {
		t.Error("an ended month window of a deleted group kept")
	}
}

// An edited limit keeps its spend in control-plane mode: group and type identify it,
// so the pushed base and the gateway's own usage stay with the new value.
func TestEditedLimitKeepsTheSpend(t *testing.T) {
	c := newClock("2026-09-24T10:30:00Z")
	holder := holderOf(snapshot(t, limitsDoc{team: usdLimit}))
	l, _ := c.shared(holder)
	l.TakeTotals(Totals{Windows: []PushedWindow{
		pushedWindow("t", config.LimitUSDPerMonth, "2026-09-01T00:00:00Z", 900_000_000),
	}}, 0)
	l.Settle(admitN(t, l, workload, 1, 10)[0], inGeneration(1, record(10, 0, 0, 0, 0, 100_000_000)))
	if rej := refused(t, l, workload, 10); rej.Measure != MeasureCost {
		t.Fatalf("rejection %+v, want the spent USD limit", rej)
	}

	holder.Swap(snapshot(t, limitsDoc{team: `[{ "type": "usd_per_month", "value": 2 }]`}))
	if got := used(t, l, "t", config.LimitUSDPerMonth); got != 1_000_000_000 {
		t.Errorf("team USD used %d after the edit, want 1 USD kept (0.9 pushed + 0.1 own)", got)
	}
	admitN(t, l, workload, 1, 10) // under the raised budget
}

// A gateway that boots without reaching the control plane does not know what was
// spent: it refuses priced USD-limited requests from the start (D8: no totals yet), and
// past the grace since it started the outage rule refuses them too.
func TestBootWithoutTheControlPlaneIsAnOutageAfterTheGrace(t *testing.T) {
	c := newClock("2026-09-24T10:30:00Z")
	doc := limitsDoc{team: usdLimit, grace: "60000"}
	started := c.t
	l, contact := c.shared(holderOf(snapshot(t, doc)))
	contact.set(false, started) // never reached: contact counts from the start
	if rej := refused(t, l, workload, 10); !rej.Unavailable {
		t.Errorf("no totals: rejection %+v, want unavailable from the start", rej)
	}
	c.advance(time.Minute + time.Millisecond)
	if rej := refused(t, l, workload, 10); !rej.Unavailable {
		t.Errorf("rejection %+v, want unavailable past the grace since the start", rej)
	}
}

// The independent audit's finding 7: a reservation made in a window that a pushed
// future window then cleared must never be released against a later incarnation of
// the window — even when a clock correction brings the same start back. Released
// there, it subtracted a hold that no longer existed, and the negative count
// saturated to "everything used": every request refused.
func TestHoldOfAClearedWindowIsNeverReleasedAfterAClockCorrection(t *testing.T) {
	c := newClock("2026-09-24T10:30:00Z")
	l, _ := c.shared(holderOf(snapshot(t, limitsDoc{workload: hourLimit})))
	push := func(start string) {
		l.TakeTotals(Totals{Windows: []PushedWindow{pushedWindow("w", config.LimitTokensPerHour, start, 0)}}, 0)
	}
	push("2026-09-24T10:00:00Z")
	res := admitN(t, l, workload, 1, 900)[0]
	push("2026-09-24T12:00:00Z") // a control-plane clock ahead
	_ = l.Usage()                // the window rolls into the pushed one, the hold cleared
	push("2026-09-24T10:00:00Z") // corrected: back to the gateway's window
	l.Settle(res, record(10, 0, 0, 0, 0, 0))
	if got := used(t, l, "w", config.LimitTokensPerHour); got != 10 {
		t.Errorf("used %d after the correction, want the settled 10", got)
	}
	admitN(t, l, workload, 1, 1)
}

// D8: a fresh gateway does not know what was spent until its first totals arrive.
// "No totals yet" is not "totals with no usage": priced USD-limited requests are
// refused as unavailable until then; token limits keep counting locally from zero;
// the first totals end it.
func TestNoTotalsYetRefusesMoneyLimitedModels(t *testing.T) {
	c := newClock("2026-09-24T10:30:00Z")
	doc := limitsDoc{team: usdLimit, workload: hourLimit, grace: "60000"}
	l, _ := c.shared(holderOf(snapshot(t, doc)))
	select {
	case <-l.FirstTotals():
		t.Fatal("first totals reported before any")
	default:
	}
	rej := refused(t, l, workload, 10)
	if !rej.Unavailable || rej.Group != "t" || rej.Type != config.LimitUSDPerMonth {
		t.Fatalf("rejection %+v, want unavailable by the team's USD limit", rej)
	}
	// Not the unpriced model, and not the hour token limit: they count from zero.
	admitN(t, l, workload.on("m2"), 1, 10)

	// The first totals end it, and are reported (the readiness wait ends): the budget
	// is spent, so budget_exceeded.
	spent := pushedWindow("t", config.LimitUSDPerMonth, "2026-09-01T00:00:00Z", 1_000_000_000)
	l.TakeTotals(Totals{Windows: []PushedWindow{spent}}, 0)
	if rej := refused(t, l, workload, 10); rej.Unavailable || rej.Measure != MeasureCost {
		t.Fatalf("rejection %+v after the totals, want the spent budget", rej)
	}
	select {
	case <-l.FirstTotals():
	default:
		t.Fatal("first totals not reported once taken")
	}

	// File mode has no such state.
	admitN(t, New(holderOf(snapshot(t, doc)), c.now, nil), workload, 1, 10)
}

// A gateway still enforcing a limit the control plane's current config dropped (it
// rejected that config) keeps the limit's spend: the totals list every scope with
// usage whatever the config, and a later push that does not list the scope — it did
// not change — leaves its base as it is, after the gateway's own usage was retired
// into it (AUDIT-2 2H3).
func TestALimitKeepsItsSpendWhateverTheControlPlanesConfig(t *testing.T) {
	c := newClock("2026-10-07T12:30:00Z")
	l, _ := c.shared(holderOf(snapshot(t, limitsDoc{workload: hourLimit})))
	l.TakeTotals(Totals{Complete: true}, 0)
	l.Settle(admitN(t, l, workload, 1, 100)[0], inGeneration(1, record(100, 0, 0, 0, 0, 0)))
	l.TakeTotals(Totals{Windows: []PushedWindow{
		pushedWindow("w", config.LimitTokensPerHour, "2026-10-07T12:00:00Z", 100)}}, 1)
	l.TakeTotals(Totals{Windows: []PushedWindow{
		pushedWindow("t", config.LimitTokensPerHour, "2026-10-07T12:00:00Z", 140)}}, 1)
	if got := used(t, l, "w", config.LimitTokensPerHour); got != 100 {
		t.Fatalf("the running limit counts %d tokens after its usage was retired, want 100", got)
	}
}
