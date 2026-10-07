package limits

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
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
	// teamModels, when set, is group t's allowed_models list (JSON).
	teamModels string
	// extraGroup, when set, adds a top-level group of that ID with no limits.
	extraGroup string
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
  "format_version": 5,
  "global": {` + graceField(l.grace) + ` "limits": ` + list(l.global) + ` },
  "backends": { "b": { "type": "openai-compatible", "base_url": "http://localhost:1/v1" } },
  "models": { "m1": ` + strings.Replace(model, `"metadata"`, `"prices": [{ "effective_from": "2020-01-01",
        "tiers": [{ "above_input_tokens": 0, "usd_per_million": { "tokens_in": 1 } }] }],
      "output_limit": { "default": 16384, "ceiling": 16384 }, "metadata"`, 1) + `,
    "m2": ` + model + ` },
  "groups": {
    "t": { ` + allowedModels(l.teamModels) + `"limits": ` + list(l.team) + ` },
    "w": { "parent": "t", "limits": ` + list(l.workload) + ` },
    "users": { "child_defaults": { "limits": ` + list(l.defaultUser) + ` } },
    "ann": { "parent": "users", "limits": ` + list(l.ann) + ` },
    "bob": { "parent": "users" }` + extraGroup(l.extraGroup) + `
  },
  "keys": { "k": { "hash": "sha256:` + strings.Repeat("0", 64) + `", "group": "w" } }
}`
	s, err := config.Parse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func extraGroup(id string) string {
	if id == "" {
		return ""
	}
	return `, "` + id + `": {}`
}

func allowedModels(list string) string {
	if list == "" {
		return ""
	}
	return `"allowed_models": ` + list + `, `
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
			if rej.Scope != c.scope || rej.Group != c.group || rej.Measure != MeasureRequests || rej.Limit != 2 || rej.Used != 2 {
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

// Every limit of a scope counts every request of that scope, whatever its model.
func TestEveryLimitOfAScopeCountsEveryModel(t *testing.T) {
	c := newClock("2026-09-24T10:00:00Z")
	l := c.limiter(holderOf(snapshot(t, limitsDoc{
		team:     `[{ "type": "requests_per_minute", "value": 2 }]`,
		workload: `[{ "type": "requests_per_minute", "value": 3 }]`,
	})))
	admitN(t, l, workload, 1, 10)
	admitN(t, l, workload.on("m2"), 1, 10)
	if rej := refused(t, l, workload.on("m2"), 10); rej.Group != "t" {
		t.Errorf("refused by %s, want the team limit counting m1 and m2 together", rej.Scope)
	}
	if rej := refused(t, l, workload, 10); rej.Group != "t" {
		t.Errorf("refused by %s, want the team limit on m1 too", rej.Scope)
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
	if rej.Measure != MeasureCost || rej.Group != "t" || rej.Limit != 2000 || rej.Used != 3000 {
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
	if rej.RetryAfter != 50*time.Second || rej.Group != "w" {
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
			// Settled, so the hour is blocked by settled usage, not by running
			// requests (TestRefusalBlockedOnlyByRunningRequestsAnswersAShortRetry).
			l.Settle(admitN(t, l, workload, 1, 500)[0], record(500, 0, 0, 0, 0, 0))
			clk.advance(10 * time.Second)
			l.Settle(admitN(t, l, workload, 1, 500)[0], record(500, 0, 0, 0, 0, 0))
			// Requests are spent until the first slides out at 10:16:00 (50 s); tokens
			// until the hour ends at 11:00:00.
			rej := refused(t, l, workload, 100)
			if want := 44*time.Minute + 50*time.Second; rej.RetryAfter != want {
				t.Errorf("retry after %v, want %v (the hour limit's wait, not the minute's 50s)", rej.RetryAfter, want)
			}
			if rej.Scope != c.scope || rej.Group != c.id || rej.Type != config.LimitTokensPerHour {
				t.Errorf("rejection %+v, want the hour limit of scope %s %q", rej, c.scope, c.id)
			}
		})
	}
}

// A token limit blocked only by requests still running — it would admit the request
// were their reservations gone — answers a short fixed retry, in Retry-After and in
// x-ratelimit-reset-tokens: reservations hold the full input estimate plus the output
// limit, and requests settle at a fraction of it within seconds (docs/specs/GATEWAY.md,
// Limits → Refusal). The 8th agent of the review's repro was told 59 s and admitted 3 s
// later; the hour limit's 55m, 5 s later.
func TestRefusalBlockedOnlyByRunningRequestsAnswersAShortRetry(t *testing.T) {
	const shortRetry = 2 * time.Second // the spec's fixed value
	for _, c := range []struct {
		name     string
		limit    string
		inFlight int
		tokens   int64
		// wait is how long after the refusal the first request settles, small.
		wait time.Duration
	}{
		{"minute", `[{ "type": "tokens_per_minute", "value": 500000 }]`, 7, 64000, 3 * time.Second},
		{"hour", `[{ "type": "tokens_per_hour", "value": 1000000 }]`, 9, 104000, 5 * time.Second},
	} {
		t.Run(c.name, func(t *testing.T) {
			clk := newClock("2026-10-05T10:05:00Z")
			l := clk.limiter(holderOf(snapshot(t, limitsDoc{team: c.limit})))
			running := admitN(t, l, workload, c.inFlight, c.tokens)
			rej := refused(t, l, workload, c.tokens)
			if rej.RetryAfter != shortRetry {
				t.Errorf("retry after %v, want %v: only running requests block", rej.RetryAfter, shortRetry)
			}
			if h := rej.Headers.Tokens; h == nil || h.Reset != shortRetry {
				t.Errorf("tokens headers on the refusal %+v, want reset %v", h, shortRetry)
			}

			clk.advance(c.wait)
			l.Settle(running[0], record(2000, 30000, 0, 500, 0, 0))
			res, rej := l.Reserve(workload, c.tokens)
			if rej != nil {
				t.Fatalf("refused again after the first request settled small: %+v", rej)
			}
			// Admitted responses keep the time until the window holds nothing.
			if h := res.Headers().Tokens; h == nil || h.Reset == shortRetry {
				t.Errorf("tokens headers on the admitted request %+v, want the window's reset", h)
			}
		})
	}
}

// A token limit blocked by settled usage keeps its time: the minute's slot expiry, the
// hour's end — the short retry is for running requests only.
func TestRefusalBlockedBySettledUsageKeepsItsTime(t *testing.T) {
	for _, c := range []struct {
		name    string
		limit   string
		settled int
		tokens  int64
		want    time.Duration
	}{
		// Settled at 10:05:00; refused 10 s later: the bucket leaves at 10:06:00.
		{"minute", `[{ "type": "tokens_per_minute", "value": 500000 }]`, 7, 64000, 50 * time.Second},
		// The hour ends at 11:00:00.
		{"hour", `[{ "type": "tokens_per_hour", "value": 1000000 }]`, 9, 104000, 54*time.Minute + 50*time.Second},
	} {
		t.Run(c.name, func(t *testing.T) {
			clk := newClock("2026-10-05T10:05:00Z")
			l := clk.limiter(holderOf(snapshot(t, limitsDoc{team: c.limit})))
			for _, res := range admitN(t, l, workload, c.settled, c.tokens) {
				l.Settle(res, record(c.tokens, 0, 0, 0, 0, 0))
			}
			// One more running request: settled usage alone already blocks.
			admitN(t, l, workload, 1, 1000)
			clk.advance(10 * time.Second)
			rej := refused(t, l, workload, c.tokens)
			if rej.RetryAfter != c.want {
				t.Errorf("retry after %v, want %v", rej.RetryAfter, c.want)
			}
			if h := rej.Headers.Tokens; h == nil || h.Reset != c.want {
				t.Errorf("tokens headers on the refusal %+v, want reset %v", h, c.want)
			}
		})
	}
}

func TestReloadKeepsMatchingCounters(t *testing.T) {
	c := newClock("2026-09-24T10:00:00Z")
	holder := holderOf(snapshot(t, limitsDoc{
		team:     `[{ "type": "requests_per_minute", "value": 10 }, { "type": "tokens_per_hour", "value": 5000 }]`,
		workload: `[{ "type": "requests_per_minute", "value": 10 }]`,
	}))
	l := c.limiter(holder)
	admitN(t, l, workload, 4, 100)

	// Team requests limit lowered to 4 (applies to the count so far); the team token
	// limit kept and moved after the requests limit; the workload's limit removed and a
	// workload token limit added.
	holder.Swap(snapshot(t, limitsDoc{
		team:     `[{ "type": "tokens_per_hour", "value": 5000 }, { "type": "requests_per_minute", "value": 4 }]`,
		workload: `[{ "type": "tokens_per_minute", "value": 1000 }]`,
	}))
	rej := refused(t, l, workload, 100)
	if rej.Group != "t" || rej.Limit != 4 || rej.Used != 4 {
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

	// An edited value keeps the count: group and type identify the limit.
	holder.Swap(snapshot(t, limitsDoc{team: `[{ "type": "tokens_per_hour", "value": 9000 }]`}))
	if got := used(t, l, "t", config.LimitTokensPerHour); got != 400 {
		t.Errorf("limit with another value at %d, want the 400 kept", got)
	}
}

// A group deleted and created again under its ID within the window keeps its spend,
// in both modes: its hour and month counts outlive the config that had them, own usage
// and in-flight reservations included (AUDIT-3 3M1, [C] C3). Once their window has
// ended, the counts of a deleted group are dropped.
func TestARecreatedGroupKeepsItsSpend(t *testing.T) {
	for _, shared := range []bool{false, true} {
		t.Run(fmt.Sprint("shared=", shared), func(t *testing.T) {
			c := newClock("2026-10-07T12:30:00Z")
			h := holderOf(snapshot(t, limitsDoc{extraGroup: "temporary"}))
			l := c.limiter(h)
			if shared {
				l, _ = c.shared(h)
				l.TakeTotals(Totals{Complete: true}, 0)
			}
			subject := Subject{Groups: []string{"temporary"}, Model: "m1", Priced: true}
			l.Settle(admitN(t, l, subject, 1, 10)[0], inGeneration(1, record(100, 0, 0, 0, 0, 1_000_000_000)))
			running := admitN(t, l, subject, 1, 10)[0]
			h.Swap(snapshot(t, limitsDoc{}))
			_ = l.Usage() // the deletion applied
			l.Settle(running, inGeneration(1, record(5, 0, 0, 0, 0, 0)))
			recreated := snapshot(t, limitsDoc{extraGroup: "temporary"})
			recreated.Groups["temporary"].Limits = []config.Limit{{Type: config.LimitUSDPerMonth, Value: 1}}
			h.Swap(recreated)
			if got := used(t, l, "temporary", config.LimitUSDPerMonth); got != 1_000_000_000 {
				t.Errorf("re-created group's month %d, want the 1 USD spent before", got)
			}
			if got := used(t, l, "temporary", config.LimitTokensPerHour); got != 105 {
				t.Errorf("re-created group's hour %d, want 100 + the 5 settled while it was deleted", got)
			}
			if _, rejected := l.Reserve(subject, 1); rejected == nil {
				t.Error("request admitted against the re-created group's spent budget")
			}

			h.Swap(snapshot(t, limitsDoc{}))
			_ = l.Usage()
			c.set("2026-11-01T00:30:00Z")
			_ = l.Usage()
			l.mu.Lock()
			l.pruneRetainedLocked(c.t)
			left := len(l.retained)
			l.mu.Unlock()
			if left != 0 {
				t.Errorf("%d counts of the deleted group kept after their windows ended", left)
			}
		})
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
	team:     `[{ "type": "tokens_per_hour", "value": 100000 }]`,
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
	// Every scope with usage is saved, limited or not: hours of global, t, w, users
	// and ann; months of global, t and w (ann's usage cost nothing).
	n, err := l.SaveSnapshot(dir)
	if err != nil || n != 8 {
		t.Fatalf("saved %d windows (%v), want 8: five hours and three months", n, err)
	}

	c.advance(20 * time.Minute) // restart within the same hour
	restarted := c.limiter(holderOf(snapshot(t, persisted)))
	restored, dropped, err := restarted.LoadSnapshot(dir)
	if err != nil || restored != 8 || dropped != 0 {
		t.Fatalf("restored %d, dropped %d, err %v; want 8, 0", restored, dropped, err)
	}
	for _, w := range []struct {
		group string
		typ   config.LimitType
		want  int64
	}{
		{"", config.LimitUSDPerMonth, 7500},
		{"", config.LimitTokensPerHour, 177},
		{"t", config.LimitTokensPerHour, 135},
		{"t", config.LimitUSDPerMonth, 7500},
		{"w", config.LimitTokensPerHour, 135},
		{"users", config.LimitTokensPerHour, 42},
		{"ann", config.LimitTokensPerHour, 42},
		{"w", config.LimitTokensPerMinute, 0}, // minute windows are not kept
		{"w", config.LimitRequestsPerMinute, 0},
	} {
		if got := used(t, restarted, w.group, w.typ); got != w.want {
			t.Errorf("%q %s restored as %d, want %d", w.group, w.typ, got, w.want)
		}
	}
}

// A scope whose limit a reload removed keeps its count: the snapshot restores it.
// A window that has passed is dropped.
func TestSnapshotKeepsUnlimitedScopesAndDropsPassedWindows(t *testing.T) {
	dir, _ := openDir(t)
	c := newClock("2026-09-24T10:59:00Z")
	l := c.limiter(holderOf(snapshot(t, persisted)))
	l.Settle(admitN(t, l, workload, 1, 10)[0], record(100, 0, 0, 0, 0, 1000))
	l.Settle(admitN(t, l, ann, 1, 10)[0], record(100, 0, 0, 0, 0, 0))
	if _, err := l.SaveSnapshot(dir); err != nil {
		t.Fatal(err)
	}
	next := persisted
	next.ann = ""

	// The same hour, with ann's limit removed: ann's hour is still counted.
	c.set("2026-09-24T10:59:30Z")
	restarted := c.limiter(holderOf(snapshot(t, next)))
	restored, dropped, err := restarted.LoadSnapshot(dir)
	if err != nil || restored != 8 || dropped != 0 {
		t.Fatalf("same hour: restored %d, dropped %d, err %v; want 8, 0", restored, dropped, err)
	}
	if got := used(t, restarted, "ann", config.LimitTokensPerHour); got != 100 {
		t.Errorf("ann's hour restored as %d without its limit, want 100", got)
	}

	// The next hour: the five hours have passed, the three months are current.
	c.set("2026-09-24T11:00:30Z")
	restarted = c.limiter(holderOf(snapshot(t, next)))
	restored, dropped, err = restarted.LoadSnapshot(dir)
	if err != nil || restored != 3 || dropped != 5 {
		t.Fatalf("next hour: restored %d, dropped %d, err %v; want 3, 5", restored, dropped, err)
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
	if !strings.Contains(logs.String(), "kaiak.data_file.found_version=1") || !strings.Contains(logs.String(), "kaiak.data_file.want_version=3") {
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
	priced := snapshot(t, limitsDoc{team: usdLimit})
	free := snapshot(t, limitsDoc{team: usdLimit})
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
  "format_version": 5,
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
	if rej.Scope != ScopeGroup || rej.Group != "rag-prod" || rej.Type != config.LimitRequestsPerMinute {
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
	if rej.Group != "rag" || rej.Type != config.LimitTokensPerMinute || rej.Used != 600 {
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
  "format_version": 5,
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
				l.TakeTotals(Totals{}, 0)
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

// An operational line about a global limit carries no kaiak.limit.group:
// kaiak.limit.scope says global, as on the request line.
func TestGlobalLimitLinesCarryNoGroup(t *testing.T) {
	clk := newClock("2026-10-05T10:00:00Z")
	holder := holderOf(snapshot(t, limitsDoc{global: `[{"type":"tokens_per_minute","value":60000}]`}))
	var logs bytes.Buffer
	cs := &contactState{connected: true, last: clk.t}
	l := NewShared(holder, clk.now, cs.get, slog.New(slog.NewJSONHandler(&logs, nil)))
	l.TakeTotals(Totals{LiveGateways: 4}, 0) // a 15 000 share, below m1's default output
	var line map[string]any
	if err := json.Unmarshal(bytes.SplitN(bytes.TrimSpace(logs.Bytes()), []byte("\n"), 2)[0], &line); err != nil {
		t.Fatalf("decode the share line: %v; %s", err, logs.Bytes())
	}
	if g, ok := line["kaiak.limit.group"]; ok || line["kaiak.limit.scope"] != "global" {
		t.Errorf("global limit's line: scope %v, kaiak.limit.group %q present %v; want global and no group",
			line["kaiak.limit.scope"], g, ok)
	}
}
