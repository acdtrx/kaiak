package limits

import (
	"bytes"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"kaiak/internal/accounting"
	"kaiak/internal/config"
	"kaiak/internal/state"
)

// clock is the tests' injected time.
type clock struct{ t time.Time }

func (c *clock) now() time.Time                    { return c.t }
func (c *clock) advance(d time.Duration)           { c.t = c.t.Add(d) }
func newClock(s string) *clock                     { return &clock{t: at(s)} }
func (c *clock) set(s string)                      { c.t = at(s) }
func (c *clock) limiter(h *config.Holder) *Limiter { return New(h, c.now, nil) }

// limitsDoc holds the limit lists of the test config, as JSON arrays ("" = none), and
// global.control_outage_grace_ms ("" = the default).
type limitsDoc struct {
	global, defaultUser, team, workload, ann string
	grace                                    string
}

func list(s string) string {
	if s == "" {
		return "[]"
	}
	return s
}

// snapshot builds a config with models m1 (priced, output limit 16 384) and m2
// (unpriced, no output limit), top-level group t with its child w, and a top-level
// users group whose child_defaults (defaultUser) apply to its children ann (with her
// own overrides) and bob (the defaults only).
func snapshot(t *testing.T, l limitsDoc) *config.Snapshot {
	t.Helper()
	model := `{ "deployments": [{ "backend": "b", "model": "x" }],
      "metadata": { "context_length": 32768,
        "capabilities": { "streaming": true, "tools": false, "vision": false, "reasoning": false } } }`
	doc := `{
  "format_version": 4,
  "global": {` + graceField(l.grace) + ` "limits": ` + list(l.global) + ` },
  "backends": { "b": { "type": "openai-compatible", "base_url": "http://localhost:1/v1" } },
  "models": { "m1": ` + strings.Replace(model, `"metadata"`, `"prices": [{ "effective_from": "2020-01-01",
        "tiers": [{ "above_input_tokens": 0, "usd_per_million": { "tokens_in": 1 } }] }],
      "output_limit": { "default": 16384, "ceiling": 16384 }, "metadata"`, 1) + `,
    "m2": ` + model + ` },
  "groups": {
    "t": { "limits": ` + list(l.team) + ` },
    "w": { "parent": "t", "limits": ` + list(l.workload) + ` },
    "users": { "child_defaults": { "limits": ` + list(l.defaultUser) + ` } },
    "ann": { "parent": "users", "limits": ` + list(l.ann) + ` },
    "bob": { "parent": "users" }
  },
  "keys": { "k": { "hash": "sha256:` + strings.Repeat("0", 64) + `", "group": "w" } }
}`
	s, err := config.Parse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func graceField(ms string) string {
	if ms == "" {
		return ""
	}
	return ` "control_outage_grace_ms": ` + ms + `,`
}

func holderOf(s *config.Snapshot) *config.Holder {
	h := &config.Holder{}
	h.Swap(s)
	return h
}

// The subjects use m1, priced in the test config (on changes the model, and whether
// it is priced with it).
var (
	workload = Subject{Groups: []string{"t", "w"}, Model: "m1", Priced: true}
	ann      = Subject{Groups: []string{"users", "ann"}, Model: "m1", Priced: true}
	bob      = Subject{Groups: []string{"users", "bob"}, Model: "m1", Priced: true}
)

func (s Subject) on(model string) Subject { s.Model, s.Priced = model, model == "m1"; return s }

// admitN reserves n requests of tokens each and fails if any is refused.
func admitN(t *testing.T, l *Limiter, s Subject, n int, tokens int64) []*Reservation {
	t.Helper()
	var out []*Reservation
	for i := range n {
		res, rej := l.Reserve(s, tokens)
		if rej != nil {
			t.Fatalf("request %d refused: %+v", i+1, rej)
		}
		out = append(out, res)
	}
	return out
}

func refused(t *testing.T, l *Limiter, s Subject, tokens int64) *Rejection {
	t.Helper()
	res, rej := l.Reserve(s, tokens)
	if rej == nil {
		t.Fatalf("admitted, want refused (headers %+v)", res.Headers())
	}
	return rej
}

// used is the count of group's (""= global) counter of type typ.
func used(t *testing.T, l *Limiter, group string, typ config.LimitType) int64 {
	t.Helper()
	for _, u := range l.Usage() {
		if u.Group == group && u.Type == typ {
			return u.Used
		}
	}
	t.Fatalf("no %s counter for group %q", typ, group)
	return 0
}

const rpm2 = `[{ "type": "requests_per_minute", "value": 2 }]`

func TestEveryScopeIsEnforced(t *testing.T) {
	for _, c := range []struct {
		name    string
		doc     limitsDoc
		subject Subject
		scope   Scope
		group   string
	}{
		{"global, workload key", limitsDoc{global: rpm2}, workload, ScopeGlobal, ""},
		{"global, personal key", limitsDoc{global: rpm2}, ann, ScopeGlobal, ""},
		{"top-level group", limitsDoc{team: rpm2}, workload, ScopeGroup, "t"},
		{"key's group", limitsDoc{workload: rpm2}, workload, ScopeGroup, "w"},
		{"child_defaults", limitsDoc{defaultUser: rpm2}, bob, ScopeGroup, "bob"},
		{"child override", limitsDoc{defaultUser: `[{ "type": "requests_per_minute", "value": 5 }]`, ann: rpm2}, ann, ScopeGroup, "ann"},
	} {
		t.Run(c.name, func(t *testing.T) {
			l := newClock("2026-09-24T10:00:00Z").limiter(holderOf(snapshot(t, c.doc)))
			admitN(t, l, c.subject, 2, 10)
			rej := refused(t, l, c.subject, 10)
			if rej.Scope != c.scope || rej.ID != c.group || rej.Measure != MeasureRequests || rej.Limit != 2 || rej.Used != 2 {
				t.Errorf("rejection %+v, want scope %s %q, 2 of 2 used", rej, c.scope, c.group)
			}
		})
	}

	// One group's limits are not another's, and bob's default is his own counter, not
	// shared with ann: a child_defaults limit is each child's own.
	l := newClock("2026-09-24T10:00:00Z").limiter(holderOf(snapshot(t, limitsDoc{workload: rpm2, defaultUser: rpm2})))
	admitN(t, l, workload, 2, 10)
	admitN(t, l, ann, 2, 10)
	admitN(t, l, bob, 2, 10)

	// ann's own limit replaced the default: ann gets 5, bob 2.
	l = newClock("2026-09-24T10:00:00Z").limiter(holderOf(snapshot(t, limitsDoc{
		defaultUser: rpm2, ann: `[{ "type": "requests_per_minute", "value": 5 }]`})))
	admitN(t, l, ann, 5, 10)
	refused(t, l, ann, 10)
	admitN(t, l, bob, 2, 10)
	refused(t, l, bob, 10)
}

func TestModelSets(t *testing.T) {
	c := newClock("2026-09-24T10:00:00Z")
	l := c.limiter(holderOf(snapshot(t, limitsDoc{
		team:     `[{ "type": "requests_per_minute", "value": 1, "models": ["m1"] }]`,
		workload: `[{ "type": "requests_per_minute", "value": 3 }]`,
	})))
	admitN(t, l, workload, 1, 10)
	if rej := refused(t, l, workload, 10); rej.ID != "t" {
		t.Errorf("refused by %s, want the m1 team limit", rej.Scope)
	}
	// m2 is outside the team limit's model set; the workload limit counts all models
	// together: m1's request plus two on m2 reach its 3.
	admitN(t, l, workload.on("m2"), 2, 10)
	if rej := refused(t, l, workload.on("m2"), 10); rej.ID != "w" {
		t.Errorf("refused by %s, want the all-models workload limit", rej.Scope)
	}
}

func TestConcurrentRequestsCannotOvershoot(t *testing.T) {
	l := newClock("2026-09-24T10:00:00Z").limiter(holderOf(snapshot(t, limitsDoc{
		team:     `[{ "type": "requests_per_minute", "value": 10 }]`,
		workload: `[{ "type": "tokens_per_minute", "value": 100000 }]`,
	})))
	var wg sync.WaitGroup
	var mu sync.Mutex
	admitted := 0
	for range 200 {
		wg.Go(func() {
			if _, rej := l.Reserve(workload, 7); rej == nil {
				mu.Lock()
				admitted++
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if admitted != 10 {
		t.Errorf("%d admitted, want exactly 10", admitted)
	}
	// Refused requests reserved nothing anywhere.
	if got := used(t, l, "w", config.LimitTokensPerMinute); got != 70 {
		t.Errorf("workload tokens %d, want 70 (10 admitted × 7)", got)
	}
}

func TestRefusalReservesNothing(t *testing.T) {
	l := newClock("2026-09-24T10:00:00Z").limiter(holderOf(snapshot(t, limitsDoc{
		global:   `[{ "type": "tokens_per_minute", "value": 1000000 }]`,
		team:     `[{ "type": "requests_per_minute", "value": 1 }]`,
		workload: `[{ "type": "tokens_per_hour", "value": 1000 }, { "type": "requests_per_minute", "value": 9 }]`,
	})))
	admitN(t, l, workload, 1, 100)
	refused(t, l, workload, 100) // the team's requests limit
	for _, c := range []struct {
		group string
		typ   config.LimitType
		want  int64
	}{
		{"", config.LimitTokensPerMinute, 100},
		{"w", config.LimitTokensPerHour, 100},
		{"w", config.LimitRequestsPerMinute, 1},
	} {
		if got := used(t, l, c.group, c.typ); got != c.want {
			t.Errorf("%q %s used %d after a refusal, want %d", c.group, c.typ, got, c.want)
		}
	}
}

func record(in, cached, cacheWrite, out, reasoning, costNano int64) accounting.UsageRecord {
	return accounting.UsageRecord{
		Units: accounting.Units{config.UnitTokensIn: in, config.UnitTokensCached: cached,
			config.UnitTokensCacheWrite: cacheWrite, config.UnitTokensOut: out, config.UnitTokensReasoning: reasoning},
		CostNanoUSD: costNano,
	}
}

func TestSettlementReplacesReservationsWithActuals(t *testing.T) {
	c := newClock("2026-09-24T10:00:00Z")
	l := c.limiter(holderOf(snapshot(t, limitsDoc{workload: `[
    { "type": "requests_per_minute", "value": 100 },
    { "type": "tokens_per_minute", "value": 100000 },
    { "type": "tokens_per_hour", "value": 100000 } ]`})))
	tokens := func() (int64, int64) {
		return used(t, l, "w", config.LimitTokensPerMinute),
			used(t, l, "w", config.LimitTokensPerHour)
	}

	// Success: the reservation (input estimate + output limit) becomes the actual
	// tokens processed — input and output, not the 50 read from the cache; reasoning is
	// inside output.
	res := admitN(t, l, workload, 1, 5000)[0]
	if m, h := tokens(); m != 5000 || h != 5000 {
		t.Fatalf("reserved %d/%d, want 5000", m, h)
	}
	c.advance(3 * time.Second)
	l.Settle(res, record(100, 50, 0, 30, 10, 0))
	if m, h := tokens(); m != 130 || h != 130 {
		t.Errorf("after settling %d/%d tokens, want 130", m, h)
	}
	l.Settle(res, record(100, 50, 0, 30, 10, 0)) // a second settle changes nothing
	if m, _ := tokens(); m != 130 {
		t.Errorf("settling twice counted again: %d", m)
	}

	// Upstream failure or disconnect before an answer: the record has zero units, so
	// the reservation is released; the request still counts.
	res = admitN(t, l, workload, 1, 5000)[0]
	l.Settle(res, record(0, 0, 0, 0, 0, 0))
	// No record at all (refused downstream before accounting): same.
	res = admitN(t, l, workload, 1, 5000)[0]
	l.Settle(res)
	if m, h := tokens(); m != 130 || h != 130 {
		t.Errorf("tokens %d/%d after released reservations, want 130", m, h)
	}
	if got := used(t, l, "w", config.LimitRequestsPerMinute); got != 3 {
		t.Errorf("requests %d, want 3: failed requests still took a slot", got)
	}
}

// Token limits count the tokens that load the backend: plain input, input written to
// the cache and output. Input read from the cache does not count.
func TestSettlementCountsInputWrittenToTheCacheNotReadFromIt(t *testing.T) {
	c := newClock("2026-09-24T10:00:00Z")
	l := c.limiter(holderOf(snapshot(t, limitsDoc{workload: `[
    { "type": "tokens_per_minute", "value": 100000 },
    { "type": "tokens_per_hour", "value": 100000 } ]`})))
	res := admitN(t, l, workload, 1, 5000)[0]
	l.Settle(res, record(3, 1024, 1009, 40, 12, 0))
	for _, typ := range []config.LimitType{config.LimitTokensPerMinute, config.LimitTokensPerHour} {
		if got := used(t, l, "w", typ); got != 1052 {
			t.Errorf("%s used %d, want 1052 (3 + 1009 written + 40; the 1024 read do not count)", typ, got)
		}
	}
}

func TestSettlementSumsTheRequestsRecords(t *testing.T) {
	c := newClock("2026-09-24T10:00:00Z")
	l := c.limiter(holderOf(snapshot(t, limitsDoc{workload: `[
    { "type": "requests_per_minute", "value": 100 },
    { "type": "tokens_per_minute", "value": 100000 },
    { "type": "usd_per_month", "value": 1 } ]`})))
	// A request with two records (a timed-out attempt, then the answer) counts once,
	// with the tokens and cost of both.
	res := admitN(t, l, workload, 1, 5000)[0]
	l.Settle(res, record(25, 0, 0, 0, 0, 100), record(20, 5, 0, 30, 0, 700))
	if got := used(t, l, "w", config.LimitTokensPerMinute); got != 75 {
		t.Errorf("tokens %d, want 75 (25 + 20 + 30; the 5 read from the cache do not count)", got)
	}
	if got := used(t, l, "w", config.LimitUSDPerMonth); got != 800 {
		t.Errorf("cost %d nano-USD, want 800", got)
	}
	if got := used(t, l, "w", config.LimitRequestsPerMinute); got != 1 {
		t.Errorf("requests %d, want 1", got)
	}
}

func TestTokenLimitRefusesAnOversizeRequest(t *testing.T) {
	l := newClock("2026-09-24T10:00:00Z").limiter(holderOf(snapshot(t, limitsDoc{
		workload: `[{ "type": "tokens_per_minute", "value": 1000 }]`})))
	rej := refused(t, l, workload, 1001)
	if rej.Measure != MeasureTokens || rej.Requested != 1001 || rej.Used != 0 {
		t.Errorf("rejection %+v", rej)
	}
}

// The follow-up audit's N-M1 reproduction: a team with 1000 tokens per hour, 10
// already used, and a request reserving the int64 maximum. A wrapped used + need
// would admit it and leave the counter negative — admitting every later request
// until it ended. It is refused as too large and the counter keeps its 10.
func TestAHugeReservationCannotWrapTheCounter(t *testing.T) {
	for _, typ := range []config.LimitType{config.LimitTokensPerHour, config.LimitTokensPerMinute} {
		t.Run(string(typ), func(t *testing.T) {
			l := newClock("2026-09-24T10:00:00Z").limiter(holderOf(snapshot(t, limitsDoc{
				team: `[{ "type": "` + string(typ) + `", "value": 1000 }]`})))
			res := admitN(t, l, workload, 1, 10)[0]
			l.Settle(res, record(10, 0, 0, 0, 0, 0))

			rej := refused(t, l, workload, math.MaxInt64)
			if rej.Measure != MeasureTokens || rej.Requested != math.MaxInt64 || rej.Requested <= rej.Max {
				t.Errorf("rejection %+v, want a request too large for the limit", rej)
			}
			if got := used(t, l, "t", typ); got != 10 {
				t.Fatalf("used %d after the refusal, want 10", got)
			}
			// The limit still holds: 991 more does not fit.
			refused(t, l, workload, 991)
			admitN(t, l, workload, 1, 990)
		})
	}
}

func TestCostLimitRefusesOnceTheBudgetIsSpent(t *testing.T) {
	c := newClock("2026-09-24T10:00:00Z")
	// 0.000002 USD = 2000 nano-USD.
	l := c.limiter(holderOf(snapshot(t, limitsDoc{team: `[{ "type": "usd_per_month", "value": 0.000002 }]`})))
	// Cost is known only afterwards, so concurrent requests all pass the check…
	first := admitN(t, l, workload, 2, 10)
	l.Settle(first[0], record(1, 0, 0, 1, 0, 1500))
	admitN(t, l, workload, 1, 10)
	// …and the budget refuses once the settled cost reaches it (overshoot allowed).
	l.Settle(first[1], record(1, 0, 0, 1, 0, 1500))
	rej := refused(t, l, workload, 10)
	if rej.Measure != MeasureCost || rej.ID != "t" || rej.Limit != 2000 || rej.Used != 3000 {
		t.Errorf("rejection %+v", rej)
	}
	if want := at("2026-10-01T00:00:00Z").Sub(c.now()); rej.RetryAfter != want {
		t.Errorf("retry after %v, want %v (month end)", rej.RetryAfter, want)
	}
	if rej.Headers.Requests != nil || rej.Headers.Tokens != nil {
		t.Errorf("cost limits have no rate-limit headers: %+v", rej.Headers)
	}
	c.set("2026-10-01T00:00:00Z")
	admitN(t, l, workload, 1, 10)
}

func TestHeadersNameTheTightestLimit(t *testing.T) {
	c := newClock("2026-09-24T10:15:00Z")
	l := c.limiter(holderOf(snapshot(t, limitsDoc{
		global:   `[{ "type": "requests_per_minute", "value": 100 }, { "type": "tokens_per_minute", "value": 5000 }]`,
		team:     `[{ "type": "tokens_per_hour", "value": 1000 }]`,
		workload: `[{ "type": "requests_per_minute", "value": 3 }]`,
	})))
	res := admitN(t, l, workload, 1, 400)[0]
	h := res.Headers()
	if h.Requests == nil || *h.Requests != (HeaderValues{Limit: 3, Remaining: 2, Reset: 60 * time.Second}) {
		t.Errorf("requests headers %+v, want the workload's 3/2/60s", h.Requests)
	}
	if h.Tokens == nil || *h.Tokens != (HeaderValues{Limit: 1000, Remaining: 600, Reset: 45 * time.Minute}) {
		t.Errorf("tokens headers %+v, want the team hour's 1000/600/45m", h.Tokens)
	}

	c.advance(10 * time.Second)
	admitN(t, l, workload, 2, 100)
	rej := refused(t, l, workload, 100)
	// Requests at t+0, t+10 (two): the first frees a slot at t+60, 50 s from now.
	if rej.RetryAfter != 50*time.Second || rej.ID != "w" {
		t.Errorf("retry after %v by %s, want 50s by the workload", rej.RetryAfter, rej.Scope)
	}
	if rej.Headers.Requests.Remaining != 0 || rej.Headers.Tokens.Remaining != 400 {
		t.Errorf("headers on refusal %+v %+v", rej.Headers.Requests, rej.Headers.Tokens)
	}

	// No applicable limit of a kind: no header for it.
	l = c.limiter(holderOf(snapshot(t, limitsDoc{})))
	if h := admitN(t, l, workload, 1, 10)[0].Headers(); h.Requests != nil || h.Tokens != nil {
		t.Errorf("headers without limits: %+v", h)
	}
}

// With several limits refusing, Retry-After is the longest wait among them — a
// client retrying sooner is refused again — whichever counter is checked first.
func TestRetryAfterIsTheLongestWaitAmongRefusingLimits(t *testing.T) {
	const rpm, tph = `[{ "type": "requests_per_minute", "value": 2 }]`, `[{ "type": "tokens_per_hour", "value": 1000 }]`
	for _, c := range []struct {
		name string
		doc  limitsDoc
		// scope and id name the hour limit, the longer wait.
		scope Scope
		id    string
	}{
		{"minute checked first", limitsDoc{global: rpm, team: tph}, ScopeGroup, "t"},
		{"hour checked first", limitsDoc{global: tph, team: rpm}, ScopeGlobal, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			clk := newClock("2026-09-24T10:15:00Z")
			l := clk.limiter(holderOf(snapshot(t, c.doc)))
			admitN(t, l, workload, 1, 500)
			clk.advance(10 * time.Second)
			admitN(t, l, workload, 1, 500)
			// Requests are spent until the first slides out at 10:16:00 (50 s); tokens
			// until the hour ends at 11:00:00.
			rej := refused(t, l, workload, 100)
			if want := 44*time.Minute + 50*time.Second; rej.RetryAfter != want {
				t.Errorf("retry after %v, want %v (the hour limit's wait, not the minute's 50s)", rej.RetryAfter, want)
			}
			if rej.Scope != c.scope || rej.ID != c.id || rej.Type != config.LimitTokensPerHour {
				t.Errorf("rejection %+v, want the hour limit of scope %s %q", rej, c.scope, c.id)
			}
		})
	}
}

func TestReloadKeepsMatchingCounters(t *testing.T) {
	c := newClock("2026-09-24T10:00:00Z")
	holder := holderOf(snapshot(t, limitsDoc{
		team:     `[{ "type": "requests_per_minute", "value": 10 }, { "type": "tokens_per_hour", "value": 5000, "models": ["m1", "m2"] }]`,
		workload: `[{ "type": "requests_per_minute", "value": 10 }]`,
	}))
	l := c.limiter(holder)
	admitN(t, l, workload, 4, 100)

	// Team requests limit lowered to 4 (applies to the count so far); the team token
	// limit kept with its model set written in another order; the workload's limit
	// removed and a workload token limit added.
	holder.Swap(snapshot(t, limitsDoc{
		team:     `[{ "type": "requests_per_minute", "value": 4 }, { "type": "tokens_per_hour", "value": 5000, "models": ["m2", "m1"] }]`,
		workload: `[{ "type": "tokens_per_minute", "value": 1000 }]`,
	}))
	rej := refused(t, l, workload, 100)
	if rej.ID != "t" || rej.Limit != 4 || rej.Used != 4 {
		t.Errorf("rejection %+v, want the lowered team limit with the count kept", rej)
	}
	if got := used(t, l, "t", config.LimitTokensPerHour); got != 400 {
		t.Errorf("team tokens %d, want 400 kept", got)
	}
	if got := used(t, l, "w", config.LimitTokensPerMinute); got != 0 {
		t.Errorf("new workload limit starts at %d, want 0", got)
	}
	for _, u := range l.Usage() {
		if u.Group == "w" && u.Type == config.LimitRequestsPerMinute {
			t.Errorf("removed limit still counted: %+v", u)
		}
	}

	// D5: a limit whose only change is its model set keeps its count — group and
	// type identify it across the change.
	holder.Swap(snapshot(t, limitsDoc{team: `[{ "type": "tokens_per_hour", "value": 5000, "models": ["m1"] }]`}))
	if got := used(t, l, "t", config.LimitTokensPerHour); got != 400 {
		t.Errorf("limit with another model set at %d, want the 400 carried over", got)
	}
}

func TestSyncIsObservedOncePerNewConfig(t *testing.T) {
	c := newClock("2026-09-24T10:00:00Z")
	holder := holderOf(snapshot(t, limitsDoc{team: rpm2}))
	l := c.limiter(holder)
	var syncs []time.Duration
	l.ObserveSyncs(func(d time.Duration) { syncs = append(syncs, d) })

	admitN(t, l, workload, 2, 100) // the first request matches the counters to the config
	_ = l.Usage()
	if len(syncs) != 1 {
		t.Fatalf("%d syncs observed for one config, want 1", len(syncs))
	}
	holder.Swap(snapshot(t, limitsDoc{team: `[{ "type": "requests_per_minute", "value": 3 }]`}))
	admitN(t, l, workload, 1, 100)
	_ = l.Usage()
	if len(syncs) != 2 {
		t.Fatalf("%d syncs observed for two configs, want 2", len(syncs))
	}
	for _, d := range syncs {
		// Timed on the process clock, which moves; the limiter's clock stood still.
		if d <= 0 {
			t.Errorf("sync duration %v, want the time it took", d)
		}
	}
}

func openDir(t *testing.T) (*state.Dir, *bytes.Buffer) {
	t.Helper()
	var logs bytes.Buffer
	dir, err := state.Open(t.TempDir(), slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return dir, &logs
}

var persisted = limitsDoc{
	global:   `[{ "type": "usd_per_month", "value": 100 }]`,
	team:     `[{ "type": "tokens_per_hour", "value": 100000, "models": ["m1"] }]`,
	workload: `[{ "type": "tokens_per_minute", "value": 100000 }, { "type": "requests_per_minute", "value": 100 }]`,
	ann:      `[{ "type": "tokens_per_hour", "value": 100000 }]`,
}

func TestSnapshotRoundTrip(t *testing.T) {
	dir, _ := openDir(t)
	c := newClock("2026-09-24T10:20:00Z")
	l := c.limiter(holderOf(snapshot(t, persisted)))
	res := admitN(t, l, workload, 2, 1000)
	l.Settle(res[0], record(100, 0, 0, 20, 0, 7000))
	l.Settle(res[1], record(10, 0, 0, 5, 0, 500))
	l.Settle(admitN(t, l, ann, 1, 1000)[0], record(42, 0, 0, 0, 0, 0))
	admitN(t, l, workload, 1, 999) // in flight at save time: not saved
	n, err := l.SaveSnapshot(dir)
	if err != nil || n != 3 {
		t.Fatalf("saved %d windows (%v), want 3: global month, team hour, ann hour", n, err)
	}

	c.advance(20 * time.Minute) // restart within the same hour
	restarted := c.limiter(holderOf(snapshot(t, persisted)))
	restored, dropped, err := restarted.LoadSnapshot(dir)
	if err != nil || restored != 3 || dropped != 0 {
		t.Fatalf("restored %d, dropped %d, err %v; want 3, 0", restored, dropped, err)
	}
	for _, w := range []struct {
		group string
		typ   config.LimitType
		want  int64
	}{
		{"", config.LimitUSDPerMonth, 7500},
		{"t", config.LimitTokensPerHour, 135},
		{"ann", config.LimitTokensPerHour, 42},
		{"w", config.LimitTokensPerMinute, 0}, // minute windows are not kept
		{"w", config.LimitRequestsPerMinute, 0},
	} {
		if got := used(t, restarted, w.group, w.typ); got != w.want {
			t.Errorf("%q %s restored as %d, want %d", w.group, w.typ, got, w.want)
		}
	}
}

func TestSnapshotDropsPassedWindowsAndRemovedLimits(t *testing.T) {
	dir, _ := openDir(t)
	c := newClock("2026-09-24T10:59:00Z")
	l := c.limiter(holderOf(snapshot(t, persisted)))
	l.Settle(admitN(t, l, workload, 1, 10)[0], record(100, 0, 0, 0, 0, 1000))
	l.Settle(admitN(t, l, ann, 1, 10)[0], record(100, 0, 0, 0, 0, 0))
	if _, err := l.SaveSnapshot(dir); err != nil {
		t.Fatal(err)
	}

	// Next hour, with ann's limit removed: the team hour has passed, ann's limit is
	// gone, the global month is still current.
	c.set("2026-09-24T11:00:30Z")
	next := persisted
	next.ann = ""
	restarted := c.limiter(holderOf(snapshot(t, next)))
	restored, dropped, err := restarted.LoadSnapshot(dir)
	if err != nil || restored != 1 || dropped != 2 {
		t.Fatalf("restored %d, dropped %d, err %v; want 1, 2", restored, dropped, err)
	}
	if got := used(t, restarted, "t", config.LimitTokensPerHour); got != 0 {
		t.Errorf("passed hour restored as %d", got)
	}
	if got := used(t, restarted, "", config.LimitUSDPerMonth); got != 1000 {
		t.Errorf("month restored as %d, want 1000", got)
	}
}

// The previous format (version 1, limits named by scope and owner ID) is discarded
// and the discard logged, as any other version is.
func TestSnapshotWithAnotherVersionIsDiscarded(t *testing.T) {
	dir, logs := openDir(t)
	path := filepath.Join(dir.Path(), SnapshotFile)
	old := `{"format_version": 1, "data": {"windows": [{"scope": "team", "id": "t", "type": "tokens_per_hour",
		"models": null, "window_start": "2026-09-24T10:00:00Z", "used": 135}]}}`
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	l := newClock("2026-09-24T10:00:00Z").limiter(holderOf(snapshot(t, persisted)))
	restored, dropped, err := l.LoadSnapshot(dir)
	if err != nil || restored != 0 || dropped != 0 {
		t.Fatalf("restored %d, dropped %d, err %v", restored, dropped, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("the file with another version was kept")
	}
	if !strings.Contains(logs.String(), "kaiak.data_file.found_version=1") || !strings.Contains(logs.String(), "kaiak.data_file.want_version=2") {
		t.Errorf("discard not logged:\n%s", logs)
	}
	// No file at all restores nothing, without error.
	if restored, _, err := l.LoadSnapshot(dir); err != nil || restored != 0 {
		t.Errorf("missing file: restored %d, err %v", restored, err)
	}
}

// The independent audit's finding 6: whether USD limits apply to a request is decided
// by the request's own config snapshot — the one its usage is priced from — not by
// the config in force at admission. A reload removing m1's prices while a request
// that took the priced snapshot is still reading its body left the spent budget
// unchecked, and the request's cost was then charged past it.
func TestBillabilityComesFromTheRequestsOwnSnapshot(t *testing.T) {
	c := newClock("2026-09-24T10:30:00Z")
	priced := snapshot(t, limitsDoc{team: usdLimitM1})
	free := snapshot(t, limitsDoc{team: usdLimitM1})
	free.Models["m1"].Prices = nil
	h := holderOf(priced)
	l := c.limiter(h)
	// A request reserved while priced, settled after a reload made m1 free: its
	// cost still counts.
	res := admitN(t, l, workload, 1, 1)[0]
	h.Swap(free)
	l.Settle(res, record(0, 0, 0, 0, 0, 1_000_000_000))
	if got := used(t, l, "t", config.LimitUSDPerMonth); got != 1_000_000_000 {
		t.Fatalf("team spent %d, want the settled $1", got)
	}

	// The budget is spent. A request that took the priced snapshot before the reload
	// is refused by it; one under the free snapshot is not held by it.
	if rej := refused(t, l, workload, 10); rej.Type != config.LimitUSDPerMonth {
		t.Errorf("refused by %s, want the spent usd_per_month", rej.Type)
	}
	unpriced := workload
	unpriced.Priced = false
	admitN(t, l, unpriced, 1, 10)

	// The reverse reload: m1 priced again, a request from the free snapshot is still
	// free.
	h.Swap(priced)
	admitN(t, l, unpriced, 1, 10)
	refused(t, l, workload, 10)
}

// treeSnapshot is a team → project → env → workload branch: team research, project
// rag (tokens_per_minute 1000 shared by its envs), envs rag-prod and rag-dev (each
// requests_per_minute 2 from rag's child_defaults; rag-dev overrides it to 3), and a
// workload under each env.
func treeSnapshot(t *testing.T) *config.Snapshot {
	t.Helper()
	doc := `{
  "format_version": 4,
  "global": {},
  "backends": { "b": { "type": "openai-compatible", "base_url": "http://localhost:1/v1" } },
  "models": { "m1": { "deployments": [{ "backend": "b", "model": "x" }],
    "metadata": { "context_length": 32768,
      "capabilities": { "streaming": true, "tools": false, "vision": false, "reasoning": false } } } },
  "groups": {
    "research": {},
    "rag": { "parent": "research", "limits": [{ "type": "tokens_per_minute", "value": 1000 }],
      "child_defaults": { "limits": [{ "type": "requests_per_minute", "value": 2 }] } },
    "rag-prod": { "parent": "rag" },
    "rag-dev": { "parent": "rag", "limits": [{ "type": "requests_per_minute", "value": 3 }] },
    "prod-chat": { "parent": "rag-prod" },
    "dev-chat": { "parent": "rag-dev" }
  },
  "keys": {}
}`
	s, err := config.Parse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// A request's scopes are every group on its path: an env's limit refuses while its
// project still has room, and two envs share their project's limit.
func TestEveryGroupOnThePathIsEnforced(t *testing.T) {
	s := treeSnapshot(t)
	prod := Subject{Groups: s.Groups["prod-chat"].PathIDs, Model: "m1"}
	dev := Subject{Groups: s.Groups["dev-chat"].PathIDs, Model: "m1"}

	l := newClock("2026-09-24T10:00:00Z").limiter(holderOf(s))
	admitN(t, l, prod, 2, 100)
	rej := refused(t, l, prod, 100)
	if rej.Scope != ScopeGroup || rej.ID != "rag-prod" || rej.Type != config.LimitRequestsPerMinute {
		t.Errorf("rejection %+v, want rag-prod's requests limit (from rag's child_defaults)", rej)
	}
	if got := used(t, l, "rag", config.LimitTokensPerMinute); got != 200 {
		t.Errorf("project tokens %d, want 200 of 1000: it still has room", got)
	}

	// rag-dev's own limit replaced the default (3), and both envs count toward rag's
	// 1000 tokens: 200 from prod, 3 × 250 from dev, then dev's next request no
	// longer fits the project.
	admitN(t, l, dev, 3, 250)
	if got := used(t, l, "rag", config.LimitTokensPerMinute); got != 950 {
		t.Errorf("project tokens %d, want 950 shared by both envs", got)
	}
	l2 := newClock("2026-09-24T10:00:00Z").limiter(holderOf(s))
	admitN(t, l2, prod, 1, 600)
	rej = refused(t, l2, dev, 600)
	if rej.ID != "rag" || rej.Type != config.LimitTokensPerMinute || rej.Used != 600 {
		t.Errorf("rejection %+v, want rag's tokens limit spent by the other env", rej)
	}
}

// chainSnapshot is a team → env → workload chain with a tokens_per_hour limit on each
// level; withWorkload false leaves the workload group out (deleted).
func chainSnapshot(t *testing.T, withWorkload bool) *config.Snapshot {
	t.Helper()
	limit := `"limits": [{ "type": "tokens_per_hour", "value": 100000 }]`
	workloadGroup := ""
	if withWorkload {
		workloadGroup = `, "workload": { "parent": "env", ` + limit + ` }`
	}
	doc := `{
  "format_version": 4,
  "global": {},
  "backends": { "b": { "type": "openai-compatible", "base_url": "http://localhost:1/v1" } },
  "models": { "m1": { "deployments": [{ "backend": "b", "model": "x" }],
    "metadata": { "context_length": 32768,
      "capabilities": { "streaming": true, "tools": false, "vision": false, "reasoning": false } } } },
  "groups": {
    "team": { ` + limit + ` },
    "env": { "parent": "team", ` + limit + ` }` + workloadGroup + `
  },
  "keys": {}
}`
	s, err := config.Parse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// A request admitted before its group was deleted still settles on the surviving
// ancestors: their windows hold the request's actual tokens, not its reservation and
// not nothing. In control-plane mode the same, as usage not yet counted there.
func TestSettlementReachesAncestorsOfADeletedGroup(t *testing.T) {
	for _, mode := range []string{"file", "shared"} {
		t.Run(mode, func(t *testing.T) {
			c := newClock("2026-09-24T10:30:00Z")
			holder := holderOf(chainSnapshot(t, true))
			l := c.limiter(holder)
			if mode == "shared" {
				l, _ = c.shared(holder)
				l.TakeTotals(&Totals{}, 0)
			}
			subject := Subject{Groups: []string{"team", "env", "workload"}, Model: "m1"}
			res := admitN(t, l, subject, 1, 5000)[0]

			// The limiter follows the reload before the request settles (the next
			// request's check syncs it): the workload's counter is dropped.
			holder.Swap(chainSnapshot(t, false))
			for _, group := range []string{"team", "env"} {
				if got := used(t, l, group, config.LimitTokensPerHour); got != 5000 {
					t.Fatalf("%s tokens %d after the reload, want the 5000 reserved", group, got)
				}
			}
			c.advance(time.Second)
			l.Settle(res, inGeneration(1, record(100, 50, 0, 30, 0, 0)))
			for _, group := range []string{"team", "env"} {
				if got := used(t, l, group, config.LimitTokensPerHour); got != 130 {
					t.Errorf("%s tokens %d, want the 130 settled", group, got)
				}
			}
		})
	}
}

// A carry-over across a model-set edit stays within one group: B's limit, whose
// model set changed while A's limit of the same type went away, starts empty — it
// never inherits A's usage.
func TestCarryOverStaysWithinItsGroup(t *testing.T) {
	c := newClock("2026-09-24T10:00:00Z")
	holder := holderOf(snapshot(t, limitsDoc{
		team: `[{ "type": "tokens_per_hour", "value": 5000, "models": ["m1"] }]`,
		ann:  `[{ "type": "tokens_per_hour", "value": 5000, "models": ["m1"] }]`,
	}))
	l := c.limiter(holder)
	admitN(t, l, workload, 1, 400)
	if got := used(t, l, "t", config.LimitTokensPerHour); got != 400 {
		t.Fatalf("team tokens %d, want 400", got)
	}

	holder.Swap(snapshot(t, limitsDoc{ann: `[{ "type": "tokens_per_hour", "value": 5000, "models": ["m1", "m2"] }]`}))
	if got := used(t, l, "ann", config.LimitTokensPerHour); got != 0 {
		t.Errorf("ann tokens %d after the edit, want 0: the team's usage is not hers", got)
	}
}

// A limit split by a model-set edit into two new identities carries its predecessor's
// usage to both: the first takes the counter over, reservations in flight included;
// the second gets a copy of the settled usage only — the in-flight reservation settles
// on the counter it was made on, so a copied hold would never be released. The two
// count separately from then on.
func TestCarryOverToTwoNewLimitsFromOnePredecessor(t *testing.T) {
	c := newClock("2026-09-24T10:00:00Z")
	holder := holderOf(snapshot(t, limitsDoc{team: `[{ "type": "tokens_per_hour", "value": 5000, "models": ["m1", "m2"] }]`}))
	l := c.limiter(holder)
	l.Settle(admitN(t, l, workload, 1, 5000)[0], record(400, 0, 0, 0, 0, 0))
	inFlight := admitN(t, l, workload, 1, 1000)[0]

	holder.Swap(snapshot(t, limitsDoc{team: `[
    { "type": "tokens_per_hour", "value": 5000, "models": ["m1"] },
    { "type": "tokens_per_hour", "value": 5000, "models": ["m2"] } ]`}))
	usedOn := func(model string) int64 {
		t.Helper()
		for _, u := range l.Usage() {
			if u.Group == "t" && u.Type == config.LimitTokensPerHour && slices.Equal(u.Models, []string{model}) {
				return u.Used
			}
		}
		t.Fatalf("no team tokens_per_hour counter on %s", model)
		return 0
	}
	if m1, m2 := usedOn("m1"), usedOn("m2"); m1 != 1400 || m2 != 400 {
		t.Fatalf("after the split m1 %d, m2 %d; want 1400 (400 settled + 1000 in flight) and 400 (settled only)", m1, m2)
	}

	c.advance(time.Second)
	l.Settle(inFlight, record(200, 0, 0, 0, 0, 0))
	if m1, m2 := usedOn("m1"), usedOn("m2"); m1 != 600 || m2 != 400 {
		t.Errorf("after the in-flight request settled m1 %d, m2 %d; want 600 and 400 (no phantom hold)", m1, m2)
	}

	l.Settle(admitN(t, l, workload, 1, 100)[0], record(100, 0, 0, 0, 0, 0))
	if m1, m2 := usedOn("m1"), usedOn("m2"); m1 != 700 || m2 != 400 {
		t.Errorf("after a request on m1: m1 %d, m2 %d; want 700 and 400 (separate counters)", m1, m2)
	}
	l.Settle(admitN(t, l, workload.on("m2"), 1, 50)[0], record(50, 0, 0, 0, 0, 0))
	if m1, m2 := usedOn("m1"), usedOn("m2"); m1 != 700 || m2 != 450 {
		t.Errorf("after a request on m2: m1 %d, m2 %d; want 700 and 450", m1, m2)
	}
}

// usersDoc is a users group with children child groups, each given one
// tokens_per_hour limit on models (a JSON array) by users' child_defaults.
func usersDoc(tb testing.TB, children int, models string) *config.Snapshot {
	tb.Helper()
	model := `{ "deployments": [{ "backend": "b", "model": "x" }], "metadata": { "context_length": 32768,
      "capabilities": { "streaming": true, "tools": false, "vision": false, "reasoning": false } } }`
	var b strings.Builder
	b.WriteString(`{ "format_version": 4, "global": {},
  "backends": { "b": { "type": "openai-compatible", "base_url": "http://localhost:1/v1" } },
  "models": { "m1": ` + model + `, "m2": ` + model + ` },
  "groups": { "users": { "child_defaults": { "limits": [{ "type": "tokens_per_hour", "value": 1000, "models": ` + models + ` }] } }`)
	for i := range children {
		fmt.Fprintf(&b, `, "u%d": { "parent": "users" }`, i)
	}
	b.WriteString(` }, "keys": {} }`)
	s, err := config.Parse([]byte(b.String()))
	if err != nil {
		tb.Fatal(err)
	}
	return s
}

// A model-set edit of a child_defaults limit gives every child a new limit identity
// whose predecessor is dropped: the reload that carries them over runs under the
// limiter's lock, so its time grows with the number of children, not their square.
func BenchmarkReloadCarryOver(b *testing.B) {
	const children = 40000
	snaps := []*config.Snapshot{usersDoc(b, children, `["m1", "m2"]`), usersDoc(b, children, `["m1"]`)}
	holder := holderOf(snaps[1])
	l := newClock("2026-09-24T10:00:00Z").limiter(holder)
	l.Usage()
	b.ResetTimer()
	for i := range b.N {
		holder.Swap(snaps[i%2])
		l.mu.Lock()
		l.sync()
		l.mu.Unlock()
	}
}
