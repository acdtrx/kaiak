package limits

import (
	"log/slog"
	"slices"
	"time"

	"kaiak/internal/config"
)

// Rejection says why a request was refused: the limit that refused it (the one that
// frees up last, when several do) and when it will have room.
type Rejection struct {
	// Group is the limit's group's ID, "" for a global limit — for the operator's log
	// only, never for the client.
	Group   string
	Type    config.LimitType
	Measure config.Measure
	// Limit, Used and Requested are in the limit's unit (requests, tokens, nano-USD).
	// Limit is what this gateway enforces — a per-minute window's share in
	// control-plane mode; Max is the limit's full value, which a request above can
	// never have. Requested is 0 for a cost limit: cost is known only after a
	// request, so a cost limit refuses once its window has reached the limit.
	Limit, Max, Used, Requested int64
	// RetryAfter is how long until every refusing limit has room for the request; a
	// token limit blocked only by requests still running counts inFlightRetry.
	RetryAfter time.Duration
	Headers    Headers
	// Unavailable: the request's priced model is covered by a USD limit (Group, Type)
	// whose spend is unknown — the control plane out of reach past the outage grace,
	// or no totals yet since the start; Used, Requested, RetryAfter and Headers are not
	// set.
	Unavailable bool
}

// Scope is the kind of scope the refusing limit belongs to.
func (r *Rejection) Scope() Scope { return scopeOf(r.Group) }

// LogAttrs are the refusal's log fields: the limit's kind of scope and its group's ID
// (absent for a global limit), its type, the value enforced (a per-minute limit's
// share among the live gateways) and the value configured, what the window had used
// (unknown for a budget refused as unavailable) and, for a token limit, what the
// request asked for. Counts are in the limit's unit; USD limits are in dollars, as
// kaiak.usage.cost_usd.
func (r *Rejection) LogAttrs() []slog.Attr {
	value := func(key string, v int64) slog.Attr { return slog.Any(key, logValue(r.Measure, v)) }
	attrs := append(identityAttrs(r.Group), slog.String("kaiak.limit.type", string(r.Type)),
		value("kaiak.limit.enforced", r.Limit), value("kaiak.limit.configured", r.Max))
	if r.Unavailable {
		return attrs
	}
	attrs = append(attrs, value("kaiak.limit.used", r.Used))
	if r.Measure == config.MeasureTokens {
		attrs = append(attrs, slog.Int64("kaiak.limit.requested", r.Requested))
	}
	return attrs
}

// Headers are the x-ratelimit-* values for requests and tokens, each from the
// applicable limit with the least remaining; nil when no such limit applies (a count
// without a limit has no headers). Cost
// limits have no headers.
type Headers struct {
	Requests, Tokens *HeaderValues
}

// HeaderValues are one limit's numbers: the limit, what remains of it now, and how
// long until its window holds nothing — inFlightRetry for a limit refusing only
// because of requests still running.
type HeaderValues struct {
	Limit, Remaining int64
	Reset            time.Duration
}

// headersFor computes the header values over the request's applicable counters;
// running are the refusing counters blocked only by requests still running (nil when
// admitted). Callers hold the limiter's lock.
func headersFor(counters []*counter, now time.Time, running []*counter) Headers {
	var h Headers
	for _, c := range counters {
		if !c.limited {
			continue
		}
		var slot **HeaderValues
		switch c.measure {
		case config.MeasureRequests:
			slot = &h.Requests
		case config.MeasureTokens:
			slot = &h.Tokens
		default:
			continue
		}
		v := &HeaderValues{Limit: c.w.limit, Remaining: max(c.w.limit-c.w.usedAt(now), 0), Reset: c.w.resetIn(now)}
		if slices.Contains(running, c) {
			v.Reset = inFlightRetry
		}
		if *slot == nil || v.Remaining < (*slot).Remaining {
			*slot = v
		}
	}
	return h
}
