package server

import (
	"fmt"
	"math"
	"net/http"
	"strconv"
	"time"

	"kaiak/internal/accounting"
	"kaiak/internal/config"
	"kaiak/internal/limits"
)

// checkLimits checks a body request against every limit that applies to its scopes
// (global and each group on its key's path) and model and reserves its share: one request, and its whole input estimate (every
// prompt of a batch) plus its
// effective output limit once per sequence it asks for, in tokens (the input estimate
// alone when the output is unbounded), capped at accounting.MaxAmount — no amount the
// protocol carries is larger, and the limiter's arithmetic stays far from int64's
// edge. A token-counting request reserves no tokens and meets only
// requests-per-minute limits: nothing is generated or billed there, so no token or
// cost limit — nor an unknown budget — refuses it. The reservation settles in a finisher registered before the attempt
// loop's, so it runs after settlement and reads the request's usage records — the
// sum of its attempts'.
func checkLimits(rq *request, limiter *limits.Limiter) *apiError {
	if !rq.endpoint.takesBody() {
		return nil
	}
	tokens := rq.input.Total
	if rq.endpoint.counts() {
		tokens = 0
	}
	if rq.outputLimit != nil {
		tokens = saturatingAdd(tokens, saturatingMul(*rq.outputLimit, rq.inbound.Sequences))
	}
	tokens = min(tokens, accounting.MaxAmount)
	// Billability comes from the request's own snapshot and arrival, as its cost
	// does (accounting.Cost): a reload since changes neither.
	_, priced := accounting.PriceAt(rq.snapshot.Models[rq.model].Prices, rq.start)
	subject := limits.Subject{Groups: rq.identity.Group.PathIDs, Priced: priced, RequestsOnly: rq.endpoint.counts()}
	res, rej := limiter.Reserve(subject, tokens)
	rq.rejection = rej
	if rej != nil && rej.Unavailable {
		return errBudgetUnavailable(rej)
	}
	if rej != nil {
		setRateLimitHeaders(rq.w.Header(), rej.Headers)
		rq.w.Header().Set("Retry-After", strconv.FormatInt(ceilSeconds(rej.RetryAfter), 10))
		return errLimited(rej)
	}
	setRateLimitHeaders(rq.w.Header(), res.Headers())
	rq.finishers = append(rq.finishers, func() { limiter.Settle(res, rq.records...) })
	return nil
}

// setRateLimitHeaders writes OpenAI's x-ratelimit-* headers, on refusals and on
// admitted requests alike (the relayed response keeps them: backend rate-limit
// headers never reach the client).
func setRateLimitHeaders(h http.Header, v limits.Headers) {
	set := func(kind string, hv *limits.HeaderValues) {
		if hv == nil {
			return
		}
		h.Set("x-ratelimit-limit-"+kind, strconv.FormatInt(hv.Limit, 10))
		h.Set("x-ratelimit-remaining-"+kind, strconv.FormatInt(hv.Remaining, 10))
		h.Set("x-ratelimit-reset-"+kind, (time.Duration(ceilSeconds(hv.Reset)) * time.Second).String())
	}
	set("requests", v.Requests)
	set("tokens", v.Tokens)
}

// ceilSeconds rounds d up to whole seconds, at least 1 when d is positive.
func ceilSeconds(d time.Duration) int64 {
	if d <= 0 {
		return 0
	}
	return int64(math.Ceil(d.Seconds()))
}

// errLimited answers a request a limit refused. Request and token limits answer as
// OpenAI does (type "requests" or "tokens", code rate_limit_exceeded); an exhausted
// cost limit has its own code, so clients can tell a budget from a rate. The message
// names the kind of scope ("group limit", "global limit"), never a group's ID or
// labels: which group refused is the operator's to read in the log line.
func errLimited(r *limits.Rejection) *apiError {
	e := &apiError{status: http.StatusTooManyRequests, code: "rate_limit_exceeded"}
	retry := time.Duration(ceilSeconds(r.RetryAfter)) * time.Second
	switch r.Measure {
	case limits.MeasureRequests:
		e.errType = "requests"
		e.message = fmt.Sprintf("Rate limit reached: %s limit of %d requests per minute (used %d). Retry after %s.",
			r.Scope, r.Limit, r.Used, retry)
	case limits.MeasureTokens:
		e.errType = "tokens"
		e.message = fmt.Sprintf("Rate limit reached: %s limit of %d tokens per %s (used %d, requested %d). Retry after %s.",
			r.Scope, r.Limit, tokenWindow(r), r.Used, r.Requested, retry)
		if r.Requested > r.Max {
			e.message = fmt.Sprintf("Request too large: it needs %d tokens (input estimate plus output limit per sequence) and the %s limit is %d tokens per %s.",
				r.Requested, r.Scope, r.Max, tokenWindow(r))
		}
	default:
		e.errType = "budget"
		e.code = "budget_exceeded"
		e.message = fmt.Sprintf("Budget exhausted: %s limit of %s USD per month reached (used %s USD). Retry after %s.",
			r.Scope, usd(r.Limit), usd(r.Used), retry)
	}
	return e
}

// errBudgetUnavailable answers a request covered by a USD limit whose spend is unknown
// — the control plane, which keeps the budget totals, out of reach past the outage
// grace, or no totals yet since the start: the budget cannot be checked, so the model
// fails closed
// (docs/specs/CONTROL-PROTOCOL.md, Control-plane outage). A platform condition, not the
// caller's: 503, no Retry-After — nobody knows when the totals come.
func errBudgetUnavailable(r *limits.Rejection) *apiError {
	return &apiError{status: http.StatusServiceUnavailable, errType: typeServer, code: "budget_unavailable",
		message: fmt.Sprintf("Budget unavailable: the model has a %s USD limit and its spend is not known "+
			"(the budget service is unreachable or has not reported it yet); requests to it are refused until it is.", r.Scope)}
}

func tokenWindow(r *limits.Rejection) string {
	if r.Type == config.LimitTokensPerHour {
		return "hour"
	}
	return "minute"
}

// usd writes nano-USD as dollars, with as many digits as the amount needs.
func usd(nano int64) string {
	return strconv.FormatFloat(float64(nano)/1e9, 'f', -1, 64)
}
