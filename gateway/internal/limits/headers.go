package limits

import (
	"time"

	"kaiak/internal/config"
)

// Rejection says why a request was refused: the limit that refused it (the one that
// frees up last, when several do) and when it will have room.
type Rejection struct {
	// Scope is the kind of scope the limit belongs to; ID is its group's ID, "" for a
	// global limit — for the operator's log only, never for the client.
	Scope   Scope
	ID      string
	Type    config.LimitType
	Measure Measure
	// Limit, Used and Requested are in the limit's unit (requests, tokens, nano-USD).
	// Limit is what this gateway enforces — a per-minute window's share in
	// control-plane mode; Max is the limit's full value, which a request above can
	// never have. Requested is 0 for a cost limit: cost is known only after a
	// request, so a cost limit refuses once its window has reached the limit.
	Limit, Max, Used, Requested int64
	// RetryAfter is how long until every refusing limit has room for the request.
	RetryAfter time.Duration
	Headers    Headers
	// Unavailable: the request's priced model is covered by a USD limit (Scope, Type)
	// whose spend is unknown — the control plane out of reach, or its totals for
	// another config, past the outage grace, or no totals yet since the start; Used,
	// Requested, RetryAfter and Headers are not set.
	Unavailable bool
}

// Headers are the x-ratelimit-* values for requests and tokens, each from the
// applicable limit with the least remaining; nil when no such limit applies. Cost
// limits have no headers.
type Headers struct {
	Requests, Tokens *HeaderValues
}

// HeaderValues are one limit's numbers: the limit, what remains of it now, and how
// long until its window holds nothing.
type HeaderValues struct {
	Limit, Remaining int64
	Reset            time.Duration
}

// headersFor computes the header values over the request's applicable counters.
// Callers hold the limiter's lock.
func headersFor(counters []*counter, now time.Time) Headers {
	var h Headers
	for _, c := range counters {
		var slot **HeaderValues
		switch c.measure {
		case MeasureRequests:
			slot = &h.Requests
		case MeasureTokens:
			slot = &h.Tokens
		default:
			continue
		}
		v := &HeaderValues{Limit: c.w.limit, Remaining: max(c.w.limit-c.w.usedAt(now), 0), Reset: c.w.resetIn(now)}
		if *slot == nil || v.Remaining < (*slot).Remaining {
			*slot = v
		}
	}
	return h
}
