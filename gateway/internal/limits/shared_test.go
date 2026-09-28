package limits

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
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
}

func (c *contactState) get() Contact {
	c.mu.Lock()
	defer c.mu.Unlock()
	return Contact{Connected: c.connected, Last: c.last, UsageWaitingSince: c.waiting}
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
	hourLimit  = `[{ "type": "tokens_per_hour", "value": 1000 }]`
	usdLimitM1 = `[{ "type": "usd_per_month", "value": 1, "models": ["m1"] }]`
)

// inGeneration is rec sealed in usage batch generation g.
func inGeneration(g uint64, rec accounting.UsageRecord) accounting.UsageRecord {
	rec.Generation = g
	return rec
}

func pushedWindow(group string, typ config.LimitType, models []string, start string, used int64) PushedWindow {
	return PushedWindow{Group: group, Type: typ, Models: models, Start: at(start), Used: used}
}

func TestSharedWindowsCountPushedTotalsPlusOwnUsage(t *testing.T) {
	c := newClock("2026-09-24T10:30:00Z")
	l, _ := c.shared(holderOf(snapshot(t, limitsDoc{workload: hourLimit, team: usdLimitM1})))
	l.TakeTotals(&Totals{LiveGateways: 2, Windows: []PushedWindow{
		pushedWindow("w", config.LimitTokensPerHour, nil, "2026-09-24T10:00:00Z", 600),
		pushedWindow("t", config.LimitUSDPerMonth, []string{"m1"}, "2026-09-01T00:00:00Z", 900_000_000),
	}}, 0)

	// 600 pushed + a 300 reservation fits 1000; another 200 does not.
	res := admitN(t, l, workload, 1, 300)[0]
	if rej := refused(t, l, workload, 200); rej.ID != "w" || rej.Used != 900 || rej.Limit != 1000 {
		t.Errorf("rejection %+v, want the workload hour limit at 900 of 1000", rej)
	}
	// Settled at 50 tokens and 0.1 USD: the team's money is spent (0.9 pushed + 0.1).
	l.Settle(res, record(40, 0, 10, 0, 100_000_000))
	if got := used(t, l, "w", config.LimitTokensPerHour); got != 650 {
		t.Errorf("hour used %d, want 600 pushed + 50 own", got)
	}
	if rej := refused(t, l, workload, 10); rej.ID != "t" || rej.Measure != MeasureCost {
		t.Errorf("rejection %+v, want the team's USD limit", rej)
	}
	// m2 is outside the USD limit's model set.
	admitN(t, l, workload.on("m2"), 1, 10)
}

// The control plane counts a batch; the push that includes it and the ack that
// follows both report its generation counted: the gateway's own copy leaves in the
// same step the totals holding it arrive, never before and never twice.
func TestCountedUsageIsNeitherDoubledNorDropped(t *testing.T) {
	c := newClock("2026-09-24T10:30:00Z")
	l, _ := c.shared(holderOf(snapshot(t, limitsDoc{workload: hourLimit})))
	hour := func() int64 { return used(t, l, "w", config.LimitTokensPerHour) }

	const sealed = 1 // batch 1 holds the first record, batch 2 the second
	l.Settle(admitN(t, l, workload, 1, 10)[0], inGeneration(sealed, record(100, 0, 0, 0, 0)))
	l.Settle(admitN(t, l, workload, 1, 10)[0], inGeneration(2, record(50, 0, 0, 0, 0)))
	if got := hour(); got != 150 {
		t.Fatalf("hour used %d, want 150 own", got)
	}

	// Another gateway used 600; the push counts batch 1 (700 in all).
	window := pushedWindow("w", config.LimitTokensPerHour, nil, "2026-09-24T10:00:00Z", 700)
	l.TakeTotals(&Totals{Windows: []PushedWindow{window}}, sealed)
	if got := hour(); got != 750 {
		t.Errorf("after the push: hour used %d, want 700 pushed + 50 not yet counted", got)
	}
	// The ack of batch 1, older than the push: no totals, the generation again.
	l.TakeTotals(nil, sealed)
	if got := hour(); got != 750 {
		t.Errorf("after the ack: hour used %d, want 750 still", got)
	}
	// An ack's totals older than the push are never applied, but a stale totals-less
	// update keeps the base: nothing regresses.
	l.TakeTotals(nil, 0)
	if got := hour(); got != 750 {
		t.Errorf("hour used %d, want 750", got)
	}
}

// A pushed window newer than the local one starts that window — the control plane's
// clock leads. Usage the gateway settled in its 10:00 hour is counted in 10:00 by the
// control plane too (its previous window), so it does not follow into 11:00.
func TestPushedWindowRolloverMidBatch(t *testing.T) {
	c := newClock("2026-09-24T10:59:58Z")
	l, _ := c.shared(holderOf(snapshot(t, limitsDoc{workload: hourLimit})))
	hour := func() int64 { return used(t, l, "w", config.LimitTokensPerHour) }
	l.TakeTotals(&Totals{Windows: []PushedWindow{
		pushedWindow("w", config.LimitTokensPerHour, nil, "2026-09-24T10:00:00Z", 500)}}, 0)
	const sealed = 1
	l.Settle(admitN(t, l, workload, 1, 10)[0], inGeneration(sealed, record(100, 0, 0, 0, 0)))
	if got := hour(); got != 600 {
		t.Fatalf("hour used %d, want 600", got)
	}
	// Mid-batch, the control plane's 11:00 window starts (30 from others).
	l.TakeTotals(&Totals{Windows: []PushedWindow{
		pushedWindow("w", config.LimitTokensPerHour, nil, "2026-09-24T11:00:00Z", 30)}}, 0)
	if got := hour(); got != 30 {
		t.Errorf("hour used %d, want 30 pushed for 11:00 (the 100 belongs to 10:00)", got)
	}
	// The batch lands in 10:00 at the control plane.
	l.TakeTotals(&Totals{Windows: []PushedWindow{
		pushedWindow("w", config.LimitTokensPerHour, nil, "2026-09-24T11:00:00Z", 30)}}, sealed)
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
		l.TakeTotals(&Totals{Windows: []PushedWindow{
			pushedWindow("w", config.LimitTokensPerHour, nil, start, n)}}, 0)
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
	l.TakeTotals(&Totals{}, 0)
	contact.set(false, c.t) // no totals and no acks from here: an outage
	generation := uint64(1)
	for range 3 {
		for range 6 {
			rec := inGeneration(generation, record(100, 0, 0, 0, 0))
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
	rec := inGeneration(generation, record(100, 0, 0, 0, 0))
	rec.GatewayTime = at("2026-09-24T12:59:59.9Z")
	l.Settle(admitN(t, l, workload, 1, 100)[0], rec)
	if got := hour(); got != 0 {
		t.Errorf("hour used %d, want 0: the record belongs to 12:00", got)
	}

	// Recovery: the backlog is counted and the totals for 13:00 apply.
	contact.set(true, c.t)
	l.TakeTotals(&Totals{Windows: []PushedWindow{
		pushedWindow("w", config.LimitTokensPerHour, nil, "2026-09-24T13:00:00Z", 600)}}, generation)
	if got := hour(); got != 600 {
		t.Errorf("hour used %d after recovery, want the control plane's 600", got)
	}
}

// Totals are complete: a counter they do not list has a base of 0; a window naming
// no counter is ignored; a limit added by a reload takes its base from the totals
// already applied.
func TestTotalsMatching(t *testing.T) {
	c := newClock("2026-09-24T10:30:00Z")
	h := holderOf(snapshot(t, limitsDoc{workload: hourLimit}))
	l, _ := c.shared(h)
	hourW := pushedWindow("w", config.LimitTokensPerHour, nil, "2026-09-24T10:00:00Z", 400)
	hourT := pushedWindow("t", config.LimitTokensPerHour, nil, "2026-09-24T10:00:00Z", 250)
	stray := pushedWindow("t", config.LimitUSDPerMonth, []string{"m2", "m1"}, "2026-09-01T00:00:00Z", 1)
	l.TakeTotals(&Totals{Windows: []PushedWindow{hourW, hourT, stray}}, 0)
	if got := used(t, l, "w", config.LimitTokensPerHour); got != 400 {
		t.Errorf("workload hour %d, want 400", got)
	}
	h.Swap(snapshot(t, limitsDoc{workload: hourLimit, team: hourLimit}))
	if got := used(t, l, "t", config.LimitTokensPerHour); got != 250 {
		t.Errorf("team hour %d after the reload, want the applied totals' 250", got)
	}
	l.TakeTotals(&Totals{Windows: []PushedWindow{hourT}}, 0)
	if got := used(t, l, "w", config.LimitTokensPerHour); got != 0 {
		t.Errorf("workload hour %d, want 0: not listed", got)
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
	l.TakeTotals(&Totals{LiveGateways: 4}, 0)
	rej := refused(t, l, workload, 10)
	if rej.Limit != 2 || rej.Used != 3 || rej.Headers.Requests == nil || rej.Headers.Requests.Limit != 2 {
		t.Errorf("rejection %+v, want the share 2 with 3 used, in the headers too", rej)
	}
	l.TakeTotals(&Totals{LiveGateways: 1}, 0)
	admitN(t, l, workload, 7, 10)
}

func TestOutageRefusesMoneyLimitedModelsAfterTheGrace(t *testing.T) {
	c := newClock("2026-09-24T10:30:00Z")
	l, contact := c.shared(holderOf(snapshot(t, limitsDoc{team: usdLimitM1, grace: "60000"})))
	l.TakeTotals(&Totals{}, 0) // the first totals: the spend is known
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
	if !rej.Unavailable || rej.ID != "t" || rej.Type != config.LimitUSDPerMonth {
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
	if New(holderOf(snapshot(t, limitsDoc{team: usdLimitM1, grace: "0"})), c.now, nil).Outage() {
		t.Error("file mode reported an outage")
	}
}

// A grace of 0 fails closed as soon as contact is lost, but never while the stream
// is open (heartbeats are 15 s apart).
func TestZeroGrace(t *testing.T) {
	c := newClock("2026-09-24T10:30:00Z")
	l, contact := c.shared(holderOf(snapshot(t, limitsDoc{team: usdLimitM1, grace: "0"})))
	l.TakeTotals(&Totals{}, 0)
	contact.set(true, c.t.Add(-time.Minute))
	admitN(t, l, workload, 1, 10)
	contact.set(false, c.t)
	c.advance(time.Millisecond)
	if rej := refused(t, l, workload, 10); !rej.Unavailable {
		t.Errorf("rejection %+v, want unavailable", rej)
	}
}

// A record settled after its batch's ack — a retried attempt's record published mid-
// request, or an ack faster than the request's end — is already inside the applied
// base: it is not counted again as the gateway's own.
func TestUsageSettledAfterItsBatchIsCountedIsNotCountedTwice(t *testing.T) {
	c := newClock("2026-09-24T10:30:00Z")
	l, _ := c.shared(holderOf(snapshot(t, limitsDoc{workload: hourLimit})))
	hour := func() int64 { return used(t, l, "w", config.LimitTokensPerHour) }

	res := admitN(t, l, workload, 1, 10)[0]
	// Batch 1 holds the request's record; its ack arrives before the request settles.
	l.TakeTotals(&Totals{Windows: []PushedWindow{
		pushedWindow("w", config.LimitTokensPerHour, nil, "2026-09-24T10:00:00Z", 600)}}, 1)
	if got := hour(); got != 610 {
		t.Fatalf("hour used %d before settlement, want 600 pushed + the 10 reserved", got)
	}
	l.Settle(res, inGeneration(1, record(600, 0, 0, 0, 0)))
	if got := hour(); got != 600 {
		t.Errorf("hour used %d, want 600: the record is in the pushed base", got)
	}
	// A record of the next batch still counts as the gateway's own.
	l.Settle(admitN(t, l, workload, 1, 10)[0], inGeneration(2, record(5, 0, 0, 0, 0)))
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
	l.Settle(admitN(t, l, workload, 1, 10)[0], inGeneration(2, record(50, 0, 0, 0, 0)))
	l.Settle(admitN(t, l, workload, 1, 10)[0], inGeneration(1, record(100, 0, 0, 0, 0)))
	l.TakeTotals(&Totals{Windows: []PushedWindow{
		pushedWindow("w", config.LimitTokensPerHour, nil, "2026-09-24T10:00:00Z", 100)}}, 1)
	if got := hour(); got != 150 {
		t.Errorf("hour used %d, want 100 pushed (batch 1) + 50 of batch 2", got)
	}
}

// versioned is s as the control plane's version n of epoch "e1".
func versioned(s *config.Snapshot, n int64) *config.Snapshot {
	s.Version = config.Version{Epoch: "e1", Number: n}
	return s
}

// H3, [B]'s scenario: v1 has a $1 team budget on m1, spent. v2 widens it to m1 and m2
// and this gateway rejects v2 (a credential missing here), so it keeps v1. The control
// plane counts v2's limits and every ack carries v2 totals: they must not erase v1's
// spend, nor retire the gateway's own usage they were not applied with. Past the
// outage grace the budget cannot be known, as in an outage; when a config whose
// totals come applies, they do.
func TestTotalsOfAnotherConfigKeepTheSpentBudget(t *testing.T) {
	c := newClock("2026-09-24T10:30:00Z")
	doc := limitsDoc{team: usdLimitM1, workload: hourLimit, grace: "60000"}
	h := holderOf(versioned(snapshot(t, doc), 1))
	l, _ := c.shared(h)
	month := func() int64 { return used(t, l, "t", config.LimitUSDPerMonth) }
	hour := func() int64 { return used(t, l, "w", config.LimitTokensPerHour) }
	v1 := config.Version{Epoch: "e1", Number: 1}
	v2 := config.Version{Epoch: "e1", Number: 2}
	spent := pushedWindow("t", config.LimitUSDPerMonth, []string{"m1"}, "2026-09-01T00:00:00Z", 1_100_000_000)
	l.TakeTotals(&Totals{Config: v1, Windows: []PushedWindow{spent}}, 0)
	if rej := refused(t, l, workload, 10); rej.Measure != MeasureCost || rej.Unavailable {
		t.Fatalf("rejection %+v, want the spent budget", rej)
	}
	if l.ConfigMismatch() {
		t.Fatal("mismatch reported with totals for the applied config")
	}
	// A request on m2 (not under v1's budget) is recorded in batch 1.
	l.Settle(admitN(t, l, workload.on("m2"), 1, 10)[0], inGeneration(1, record(10, 0, 0, 0, 50_000_000)))

	// v2 rejected here; acks keep coming with v2's totals (its widened limit's window,
	// and the other limits unlisted), batch 1 among them.
	widened := pushedWindow("t", config.LimitUSDPerMonth, []string{"m1", "m2"}, "2026-09-01T00:00:00Z", 1_150_000_000)
	for range 3 {
		l.TakeTotals(&Totals{Config: v2, Windows: []PushedWindow{widened}}, 1)
		if got := month(); got != 1_100_000_000 {
			t.Fatalf("team month used %d after v2 totals, want v1's 1.1 USD kept", got)
		}
		if rej := refused(t, l, workload, 10); rej.Measure != MeasureCost || rej.Unavailable {
			t.Fatalf("rejection %+v, want the budget still spent", rej)
		}
		if got := hour(); got != 10 {
			t.Fatalf("workload hour used %d, want batch 1's 10 tokens still the gateway's own", got)
		}
	}

	// N-P3: the mismatch shows at once (the gauge), before the grace.
	if !l.ConfigMismatch() {
		t.Fatal("no mismatch reported with totals for v2 and v1 applied")
	}
	if l.Outage() {
		t.Fatal("a mismatch reported as an outage")
	}
	// Past the grace with the totals still for v2: USD-limited requests are refused as
	// in an outage; others keep serving.
	c.advance(time.Minute + time.Millisecond)
	if rej := refused(t, l, workload, 10); !rej.Unavailable {
		t.Errorf("rejection %+v past the grace, want unavailable", rej)
	}
	admitN(t, l, workload.on("m2"), 1, 10)

	// v3 is published and totals for it arrive first: they wait for the config, and
	// apply with it (v3 keeps v1's budget; the control plane counts 1.2 USD there).
	v3 := config.Version{Epoch: "e1", Number: 3}
	l.TakeTotals(&Totals{Config: v3, Windows: []PushedWindow{
		pushedWindow("t", config.LimitUSDPerMonth, []string{"m1"}, "2026-09-01T00:00:00Z", 200_000_000)}}, 0)
	if got := month(); got != 1_100_000_000 {
		t.Errorf("team month used %d before v3 applies, want 1.1 USD", got)
	}
	h.Swap(versioned(snapshot(t, doc), 3))
	if got := month(); got != 200_000_000 {
		t.Errorf("team month used %d once v3 applies, want v3's totals", got)
	}
	if l.ConfigMismatch() {
		t.Error("mismatch still reported once v3, the totals' config, applies")
	}
	if got := hour(); got != 10 {
		t.Errorf("workload hour used %d once v3 applies, want batch 1 retired, the m2 reservation left", got)
	}
	admitN(t, l, workload, 1, 10)
}

// D6: a model with no price in force costs nothing, so no budget ever refuses it —
// neither a spent USD limit covering it nor the outage and mismatch refusals, which
// exist because spend is unknown; priced models covered by the same limit are
// refused.
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
	l.TakeTotals(&Totals{Windows: []PushedWindow{
		pushedWindow("", config.LimitUSDPerMonth, nil, "2026-09-01T00:00:00Z", 1_000_000_000),
	}}, 0)
	if rej := refused(t, l, workload, 10); rej.Unavailable || rej.Measure != MeasureCost {
		t.Errorf("m1 with the budget spent: %+v, want the USD limit refusing", rej)
	}
	res := admitN(t, l, workload.on("m2"), 1, 10)[0]
	l.Settle(res, inGeneration(1, record(10, 0, 10, 0, 0)))
	if got := used(t, l, "", config.LimitUSDPerMonth); got != 1_000_000_000 {
		t.Errorf("global USD used %d, want the pushed 1 USD only", got)
	}

	// Totals for another config past the grace: the same split.
	l.TakeTotals(&Totals{Config: config.Version{Epoch: "other", Number: 9}, Windows: nil}, 0)
	c.advance(2 * time.Minute)
	if rej := refused(t, l, workload, 10); !rej.Unavailable {
		t.Errorf("m1 on mismatched totals: %+v, want unavailable", rej)
	}
	admitN(t, l, workload.on("m2"), 1, 10)
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
	l.TakeTotals(&Totals{LiveGateways: 4}, 0)
	const request = 16384 + 100

	res := admitN(t, l, workload, 1, request)[0]
	// The window holds a request now: the next one waits for it to leave.
	rej := refused(t, l, workload, request)
	if rej.Limit != 15000 || rej.Max != 60000 || rej.RetryAfter <= 0 || rej.RetryAfter > time.Minute {
		t.Errorf("rejection %+v, want the 15 000 share of 60 000, retry within the minute", rej)
	}
	l.Settle(res, inGeneration(1, record(100, 0, 16384, 0, 0)))
	c.advance(61 * time.Second)
	admitN(t, l, workload, 1, request)
	c.advance(61 * time.Second)
	if rej := refused(t, l, workload, 60001); rej.Requested <= rej.Max {
		t.Errorf("rejection %+v, want a request above the full limit", rej)
	}

	if n := strings.Count(logs.String(), "per-minute share below the model's default output"); n != 1 {
		t.Errorf("%d share warnings, want 1:\n%s", n, logs.String())
	}
	if !strings.Contains(logs.String(), "model=m1") || !strings.Contains(logs.String(), "scope=group") {
		t.Errorf("warning names the model and scope kind:\n%s", logs.String())
	}
	// The same live count again: no new warning.
	l.TakeTotals(&Totals{LiveGateways: 4}, 0)
	if n := strings.Count(logs.String(), "per-minute share below"); n != 1 {
		t.Errorf("%d share warnings after the same count, want 1", n)
	}
}

// M16 at the limiter: an open stream is contact, but not while usage batches have
// waited past the grace for an answer.
func TestUsageWaitingPastTheGraceIsAnOutage(t *testing.T) {
	c := newClock("2026-09-24T10:30:00Z")
	l, contact := c.shared(holderOf(snapshot(t, limitsDoc{team: usdLimitM1, grace: "60000"})))
	l.TakeTotals(&Totals{}, 0)
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

// D5 in control-plane mode, [A]'s scenario: a team's spent monthly budget on m1 is
// edited to cover m1 and m2. The limit keeps its spend — pushed base and own usage —
// until totals for the new config (which carry it too) arrive. Two predecessors of
// one limit (ambiguous) carry the larger, and say so in the log.
func TestModelSetEditKeepsTheSpend(t *testing.T) {
	c := newClock("2026-09-24T10:30:00Z")
	var logs bytes.Buffer
	cs := &contactState{connected: true, last: c.t}
	holder := holderOf(snapshot(t, limitsDoc{team: usdLimitM1}))
	l := NewShared(holder, c.now, cs.get, slog.New(slog.NewTextHandler(&logs, nil)))
	l.TakeTotals(&Totals{Windows: []PushedWindow{
		pushedWindow("t", config.LimitUSDPerMonth, []string{"m1"}, "2026-09-01T00:00:00Z", 900_000_000),
	}}, 0)
	l.Settle(admitN(t, l, workload, 1, 10)[0], inGeneration(1, record(10, 0, 0, 0, 100_000_000)))
	if rej := refused(t, l, workload, 10); rej.Measure != MeasureCost {
		t.Fatalf("rejection %+v, want the spent USD limit", rej)
	}

	holder.Swap(snapshot(t, limitsDoc{team: `[{ "type": "usd_per_month", "value": 1, "models": ["m1", "m2"] }]`}))
	if got := used(t, l, "t", config.LimitUSDPerMonth); got != 1_000_000_000 {
		t.Errorf("team USD used %d after the model-set edit, want 1 USD carried (0.9 pushed + 0.1 own)", got)
	}
	if rej := refused(t, l, workload, 10); rej.Measure != MeasureCost {
		t.Errorf("rejection %+v, want the edited limit still spent", rej)
	}
	if !strings.Contains(logs.String(), "limit keeps its usage across a model-set change") {
		t.Errorf("carry-over not logged:\n%s", logs.String())
	}

	// Two limits on m1 and m2 replaced by one on both: the larger spend carries.
	c2 := newClock("2026-09-24T10:30:00Z")
	logs.Reset()
	holder2 := holderOf(snapshot(t, limitsDoc{team: `[{ "type": "tokens_per_hour", "value": 5000, "models": ["m1"] },
      { "type": "tokens_per_hour", "value": 5000, "models": ["m2"] }]`}))
	l2 := NewShared(holder2, c2.now, (&contactState{connected: true, last: c2.t}).get, slog.New(slog.NewTextHandler(&logs, nil)))
	l2.TakeTotals(&Totals{Windows: []PushedWindow{
		pushedWindow("t", config.LimitTokensPerHour, []string{"m1"}, "2026-09-24T10:00:00Z", 300),
		pushedWindow("t", config.LimitTokensPerHour, []string{"m2"}, "2026-09-24T10:00:00Z", 500),
	}}, 0)
	holder2.Swap(snapshot(t, limitsDoc{team: `[{ "type": "tokens_per_hour", "value": 5000, "models": ["m1", "m2"] }]`}))
	if got := used(t, l2, "t", config.LimitTokensPerHour); got != 500 {
		t.Errorf("team hour used %d, want the larger predecessor's 500", got)
	}
	if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "predecessors=2") {
		t.Errorf("ambiguous carry-over not logged as a warning:\n%s", logs.String())
	}
}

// M5 at the limiter: the pushed bases and the uncounted usage survive a restart; the
// usage leaves once the restored spool batches carrying it are counted (or with the
// first totals when none were restored); a file of another config is discarded, and
// uncounted usage of a past window is not brought back.
func TestSharedStateSurvivesARestart(t *testing.T) {
	c := newClock("2026-09-24T10:30:00Z")
	dir, _ := openDir(t)
	doc := limitsDoc{workload: hourLimit, team: usdLimitM1}
	l, _ := c.shared(holderOf(snapshot(t, doc)))
	l.TakeTotals(&Totals{LiveGateways: 3, Windows: []PushedWindow{
		pushedWindow("w", config.LimitTokensPerHour, nil, "2026-09-24T10:00:00Z", 600),
		pushedWindow("t", config.LimitUSDPerMonth, []string{"m1"}, "2026-09-01T00:00:00Z", 1_000_000_000),
	}}, 0)
	l.Settle(admitN(t, l, workload.on("m2"), 1, 10)[0], inGeneration(1, record(50, 0, 0, 0, 0)))
	if n, err := l.SaveShared(dir); err != nil || n != 2 {
		t.Fatalf("saved %d windows (%v), want 2", n, err)
	}

	restart := func(restoredGeneration uint64) *Limiter {
		t.Helper()
		l, _ := c.shared(holderOf(snapshot(t, doc)))
		r, err := l.LoadShared(dir, restoredGeneration)
		if err != nil || !r.Found || r.Discarded != "" || r.Restored != 2 {
			t.Fatalf("restore %+v (%v), want both windows", r, err)
		}
		return l
	}
	l = restart(2)
	if got := used(t, l, "w", config.LimitTokensPerHour); got != 650 {
		t.Errorf("hour used %d after the restart, want 600 pushed + 50 uncounted", got)
	}
	if rej := refused(t, l, workload, 10); rej.Measure != MeasureCost || rej.Unavailable {
		t.Errorf("rejection %+v, want the spent budget enforced", rej)
	}
	// The restored batch 1 counted: its usage may be in there, but batch 2 may carry
	// some too; batch 2 counted: the usage leaves.
	l.TakeTotals(&Totals{Windows: []PushedWindow{
		pushedWindow("w", config.LimitTokensPerHour, nil, "2026-09-24T10:00:00Z", 620)}}, 1)
	if got := used(t, l, "w", config.LimitTokensPerHour); got != 670 {
		t.Errorf("hour used %d, want 620 pushed + the 50 still uncounted", got)
	}
	l.TakeTotals(&Totals{Windows: []PushedWindow{
		pushedWindow("w", config.LimitTokensPerHour, nil, "2026-09-24T10:00:00Z", 650)}}, 2)
	if got := used(t, l, "w", config.LimitTokensPerHour); got != 650 {
		t.Errorf("hour used %d, want the control plane's 650", got)
	}

	// No batches restored: the usage leaves with the first totals applied.
	l = restart(0)
	l.TakeTotals(&Totals{Windows: []PushedWindow{
		pushedWindow("w", config.LimitTokensPerHour, nil, "2026-09-24T10:00:00Z", 650)}}, 0)
	if got := used(t, l, "w", config.LimitTokensPerHour); got != 650 {
		t.Errorf("hour used %d, want the control plane's 650 and nothing restored of its own", got)
	}

	// The next hour: the base names 10:00 and the uncounted usage was 10:00's.
	c.set("2026-09-24T11:05:00Z")
	l = restart(2)
	if got := used(t, l, "w", config.LimitTokensPerHour); got != 0 {
		t.Errorf("hour used %d at 11:05, want 0", got)
	}

	// Booted on another config: discarded.
	other := snapshot(t, doc)
	other.Version = config.Version{Epoch: "e", Number: 7}
	l2, _ := c.shared(holderOf(other))
	if r, err := l2.LoadShared(dir, 2); err != nil || r.Discarded != "another config" || r.Restored != 0 {
		t.Errorf("restore on another config %+v (%v), want discarded", r, err)
	}
}

// M5: restored totals enforce what was spent, but they do not lift the outage rule:
// a gateway that boots without reaching the control plane refuses priced USD-limited
// requests once the grace has passed since it started. Without restored totals it
// refuses them from the start (D8: no totals yet).
func TestBootWithoutTheControlPlaneIsAnOutageAfterTheGrace(t *testing.T) {
	for _, restore := range []bool{false, true} {
		c := newClock("2026-09-24T10:30:00Z")
		dir, _ := openDir(t)
		doc := limitsDoc{team: usdLimitM1, grace: "60000"}
		if restore {
			l, _ := c.shared(holderOf(snapshot(t, doc)))
			l.TakeTotals(&Totals{}, 0)
			if _, err := l.SaveShared(dir); err != nil {
				t.Fatal(err)
			}
		}
		started := c.t
		l, contact := c.shared(holderOf(snapshot(t, doc)))
		contact.set(false, started) // never reached: contact counts from the start
		if _, err := l.LoadShared(dir, 0); err != nil {
			t.Fatal(err)
		}
		if restore {
			admitN(t, l, workload, 1, 10)
		} else if rej := refused(t, l, workload, 10); !rej.Unavailable {
			t.Errorf("no totals: rejection %+v, want unavailable from the start", rej)
		}
		c.advance(time.Minute + time.Millisecond)
		if rej := refused(t, l, workload, 10); !rej.Unavailable {
			t.Errorf("restored=%v: rejection %+v, want unavailable past the grace since the start", restore, rej)
		}
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
		l.TakeTotals(&Totals{Windows: []PushedWindow{pushedWindow("w", config.LimitTokensPerHour, nil, start, 0)}}, 0)
	}
	push("2026-09-24T10:00:00Z")
	res := admitN(t, l, workload, 1, 900)[0]
	push("2026-09-24T12:00:00Z") // a control-plane clock ahead
	_ = l.Usage()                // the window rolls into the pushed one, the hold cleared
	push("2026-09-24T10:00:00Z") // corrected: back to the gateway's window
	l.Settle(res, record(10, 0, 0, 0, 0))
	if got := used(t, l, "w", config.LimitTokensPerHour); got != 10 {
		t.Errorf("used %d after the correction, want the settled 10", got)
	}
	admitN(t, l, workload, 1, 1)
}

// D8: a fresh gateway does not know what was spent until its first totals for the
// applied config arrive. "No totals yet" is not "totals with no usage": priced
// USD-limited requests are refused as unavailable until then; token limits keep
// counting locally from zero; totals for another config do not end it; totals for
// the applied config (or the restored copy of them) do.
func TestNoTotalsYetRefusesMoneyLimitedModels(t *testing.T) {
	c := newClock("2026-09-24T10:30:00Z")
	doc := limitsDoc{team: usdLimitM1, workload: hourLimit, grace: "60000"}
	h := holderOf(versioned(snapshot(t, doc), 1))
	l, _ := c.shared(h)
	select {
	case <-l.FirstTotals():
		t.Fatal("first totals reported before any")
	default:
	}
	rej := refused(t, l, workload, 10)
	if !rej.Unavailable || rej.ID != "t" || rej.Type != config.LimitUSDPerMonth {
		t.Fatalf("rejection %+v, want unavailable by the team's USD limit", rej)
	}
	// Not the unpriced model, and not the hour token limit: they count from zero.
	admitN(t, l, workload.on("m2"), 1, 10)

	// Totals for another config wait, and do not end it — though the control plane has
	// answered: the first totals are reported (the readiness wait ends).
	l.TakeTotals(&Totals{Config: config.Version{Epoch: "e1", Number: 2}}, 0)
	if rej := refused(t, l, workload, 10); !rej.Unavailable {
		t.Fatalf("rejection %+v with totals for another config, want unavailable", rej)
	}
	select {
	case <-l.FirstTotals():
	default:
		t.Fatal("first totals not reported once taken")
	}
	// Totals for the applied config end it: the budget is spent, so budget_exceeded.
	spent := pushedWindow("t", config.LimitUSDPerMonth, []string{"m1"}, "2026-09-01T00:00:00Z", 1_000_000_000)
	l.TakeTotals(&Totals{Config: config.Version{Epoch: "e1", Number: 1}, Windows: []PushedWindow{spent}}, 0)
	if rej := refused(t, l, workload, 10); rej.Unavailable || rej.Measure != MeasureCost {
		t.Fatalf("rejection %+v after the totals, want the spent budget", rej)
	}

	// Restored totals are known totals: a restart with a data directory serves on them.
	dir, _ := openDir(t)
	if _, err := l.SaveShared(dir); err != nil {
		t.Fatal(err)
	}
	restarted, _ := c.shared(holderOf(versioned(snapshot(t, doc), 1)))
	if _, err := restarted.LoadShared(dir, 0); err != nil {
		t.Fatal(err)
	}
	if rej := refused(t, restarted, workload, 10); rej.Unavailable || rej.Measure != MeasureCost {
		t.Fatalf("rejection %+v after a restore, want the restored spent budget", rej)
	}
	select {
	case <-restarted.FirstTotals():
		t.Fatal("a restore reported as the first totals from the control plane")
	default:
	}

	// File mode has no such state.
	admitN(t, New(holderOf(snapshot(t, doc)), c.now, nil), workload, 1, 10)
}

// totals.json of the previous format (version 1, limits named by scope and owner ID)
// is discarded and the discard logged: nothing is restored from it.
func TestSharedStateOfAnotherVersionIsDiscarded(t *testing.T) {
	c := newClock("2026-09-24T10:30:00Z")
	dir, logs := openDir(t)
	path := filepath.Join(dir.Path(), SharedFile)
	old := `{"format_version": 1, "data": {"config_epoch": "", "config_version": 0, "live_gateways": 1,
		"windows": [{"scope": "workload", "id": "w", "type": "tokens_per_hour", "models": null,
		"base_window_start": "2026-09-24T10:00:00Z", "base": 600,
		"window_start": "2026-09-24T10:00:00Z", "uncounted": 0}]}}`
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	l, _ := c.shared(holderOf(snapshot(t, limitsDoc{workload: hourLimit})))
	if r, err := l.LoadShared(dir, 0); err != nil || r.Found || r.Restored != 0 {
		t.Fatalf("restore %+v (%v), want nothing found", r, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("the file with another version was kept")
	}
	if !strings.Contains(logs.String(), "found_version=1") || !strings.Contains(logs.String(), "want_version=2") {
		t.Errorf("discard not logged:\n%s", logs)
	}
}
