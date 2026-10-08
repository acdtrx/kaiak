package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"time"

	"kaiak/internal/accounting"
	"kaiak/internal/config"
	"kaiak/internal/metrics"
	"kaiak/internal/provider"
	"kaiak/internal/routing"
)

// attempt is one try of a routed request on one deployment: its slot, the meter the
// relay feeds, the backend's answer and what became of it.
type attempt struct {
	slot       routing.Slot
	deployment config.Deployment
	wait       routing.Wait
	meter      *accounting.Meter
	// status is the backend's response status; 0 when the backend gave no response.
	status int
	// err is the provider's failure to get a response, or the error that broke off
	// the relayed response. It may name backend addresses, never credentials.
	err error
	// errorCode and errorType are the error code and type the backend's error names —
	// an error event's code, a relayed 4xx's or a 5xx's body (backendErrorFields): logged,
	// never its message.
	errorCode, errorType string
	// failure is the gateway's answer to a failure before any response (nil when the
	// backend answered); resp is a backend error answer held back while the next
	// attempt looks for a slot — relayed if none is found, closed otherwise.
	failure *apiError
	resp    provider.Response
	// retryReason is the outcome the attempt was retried for; "" for the request's last
	// attempt.
	retryReason metrics.AttemptOutcome
	// start is when the attempt was sent.
	start time.Time
	// released: the circuit breaker was told and the slot freed; settled: the
	// attempt's usage was settled (or it was decided it has none).
	released, settled bool
}

// attempts is the pipeline's routing, accounting and provider stages, run as one
// attempt loop (docs/specs/GATEWAY.md, Request pipeline and Routing and reliability:
// retries), with what every attempt goes through: the router's slots, the recorder
// that settles usage, the providers that send upstream, the retry budget, the memory
// of endpoints missing from backends' servers, and the logger for the warning a newly
// missing endpoint raises.
type attempts struct {
	router    *routing.Router
	recorder  *accounting.Recorder
	providers *provider.Registry
	budget    *retryBudget
	missing   *MissingEndpoints
	logger    *slog.Logger
}

// run is the attempts stage: each attempt takes a slot (routing: the eligible
// deployment with the fewest in flight, else a wait in the model's queue), opens its
// meter (accounting) and sends upstream (provider). An attempt that failed before
// anything reached the client is retried while the model's max_attempts allow and its
// outcome is retryable; the attempt that answers — or the last one — is relayed or
// answered with its error. A request refused before its first attempt has a slot
// never reaches accounting: no usage record; the limits finisher releases its
// reservation. Only the model's deployments whose backend serves the endpoint take
// part (rq.serving); a model with none was refused before (docs/specs/GATEWAY.md,
// Providers → Endpoint support). model_access has checked the model exists in the
// snapshot.
func (l *attempts) run(ctx context.Context, rq *request) *apiError {
	m := l.missing.exclude(rq.serving, rq.endpoint.api, time.Now())
	// However the request ends, its last attempt settles, tells the circuit breaker
	// and frees its slot.
	rq.finishers = append(rq.finishers, func() { l.end(rq) })
	var avoid routing.Avoid
	for {
		prev := rq.lastAttempt()
		if prev != nil && ctx.Err() != nil {
			// The client left, or the drain cut the request, between attempts: no
			// further attempt.
			return canceledAnswer(ctx)
		}
		slot, wait, err := l.router.Acquire(ctx, m, avoid)
		rq.queueWait.Queued = rq.queueWait.Queued || wait.Queued
		rq.queueWait.Duration += wait.Duration
		if err != nil && prev == nil {
			return routingRefusal(ctx, rq, m, err)
		}
		if err != nil {
			return rq.retryRefused(ctx, err)
		}
		if prev != nil {
			prev.dropResponse()
			l.settle(rq, prev)
			// The retry is sent: counted now, not when the request is over.
			rq.ops.CountRetry(rq.model, prev.deployment.Backend.ID, prev.retryReason)
		}

		at := &attempt{slot: slot, deployment: slot.Deployment, wait: wait,
			meter: accounting.NewMeter(rq.endpoint.api, rq.input.Total), start: time.Now()}
		rq.attempts = append(rq.attempts, at)

		if prev == nil {
			l.budget.attempt(rq.model)
		} else {
			l.budget.retry(rq.model)
		}
		resp, failure := l.send(ctx, rq, at)
		at.failure = failure
		// Nothing of the attempt is relayed yet.
		outcome, _, _ := classifyAttempt(at, "")
		if attemptRules[outcome].throttle {
			cooldown := throttleCooldownDefault
			if resp != nil {
				cooldown = throttleCooldown(resp.Header(), time.Now())
			}
			slot.Throttled(cooldown)
		}
		retry := retryable(outcome) && len(rq.attempts) < m.MaxAttempts
		if retry && !l.budget.allowRetry(rq.model) {
			retry = false
			rq.retryRefusal = "retry_budget"
		}
		if !retry {
			if failure != nil {
				return failure
			}
			relayResponse(ctx, rq, resp)
			resp.Close()
			return nil
		}
		at.resp, at.retryReason = resp, outcome
		rq.releaseAttempt(at)
		avoid = avoidAfter(avoid, m, at)
	}
}

// The cooldown of a deployment that answered 429 (docs/specs/GATEWAY.md, Routing and
// reliability: 429 cooldown): the time its Retry-After-Ms or Retry-After asks for, at
// most throttleCooldownMax — a backend asking for longer is back in rotation after
// it, and a new 429 starts another — or throttleCooldownDefault without either.
const (
	throttleCooldownDefault = 5 * time.Second
	throttleCooldownMax     = 60 * time.Second
)

// throttleCooldown is the cooldown a 429 with headers h asks for at now:
// Retry-After-Ms (Azure's, in milliseconds, finer) when readable, else Retry-After
// (seconds or an HTTP date), else the default. A wait of 0 (or a date past) starts
// none.
func throttleCooldown(h http.Header, now time.Time) time.Duration {
	if ms, err := strconv.ParseFloat(h.Get("Retry-After-Ms"), 64); err == nil && ms >= 0 {
		if ms >= float64(throttleCooldownMax/time.Millisecond) {
			return throttleCooldownMax
		}
		return time.Duration(ms * float64(time.Millisecond))
	}
	v := h.Get("Retry-After")
	if secs, err := strconv.ParseInt(v, 10, 64); err == nil && secs >= 0 {
		return time.Duration(min(secs, int64(throttleCooldownMax/time.Second))) * time.Second
	}
	if at, err := http.ParseTime(v); err == nil {
		return min(max(at.Sub(now), 0), throttleCooldownMax)
	}
	return throttleCooldownDefault
}

// servingDeployments is model m as routing sees it for a request to api: only the
// deployments whose backend serves api — m itself when every one does, nil when none
// does (docs/specs/GATEWAY.md, Request pipeline: routing).
func servingDeployments(m *config.Model, api provider.Endpoint) *config.Model {
	n := 0
	for _, d := range m.Deployments {
		if serves(d, api) {
			n++
		}
	}
	switch n {
	case len(m.Deployments):
		return m
	case 0:
		return nil
	}
	view := *m
	view.Deployments = make([]config.Deployment, 0, n)
	for _, d := range m.Deployments {
		if serves(d, api) {
			view.Deployments = append(view.Deployments, d)
		}
	}
	return &view
}

// serves reports whether deployment d's backend serves api.
func serves(d config.Deployment, api provider.Endpoint) bool {
	return provider.Serves(d.Backend.Type, api)
}

// send sends the request to attempt at's deployment and waits for the first event. A
// failure before any response is returned as the answer it would get. The attempt's
// meter hears when the request was written in full (it counts the input of an attempt
// that then gets no answer), and how the backend answered.
func (l *attempts) send(ctx context.Context, rq *request, at *attempt) (provider.Response, *apiError) {
	resp, err := l.providers.For(at.deployment.Backend).Send(ctx, &provider.Request{
		Endpoint:     rq.endpoint.api,
		Deployment:   at.deployment,
		Body:         rq.body,
		Stream:       rq.inbound.Stream,
		IncludeUsage: rq.inbound.IncludeUsage,
		RequestID:    rq.id,
		PublicModel:  rq.model,
		Params:       rq.params,
		Sent:         at.meter.Sent,
	})
	if err != nil {
		failure := upstreamFailure(ctx, at, err)
		if perr, ok := errors.AsType[*provider.Error](at.err); ok && failureRuleOf(perr).refused {
			at.meter.Refused()
			if perr.Code == provider.CodeEndpointMissing &&
				l.missing.remember(at.deployment.Backend.ID, rq.endpoint.api, time.Now(), rq.snapshot.Circuit.ProbeInterval) {
				// Not a circuit failure, so it shows here, once per interval: the
				// operator upgrades the server; meanwhile routing leaves it out for the
				// endpoint.
				l.logger.Warn("the backend's server lacks an endpoint its type serves: an older version?",
					"kaiak.request.id", rq.id, "kaiak.backend.id", at.deployment.Backend.ID,
					"kaiak.deployment.model", at.deployment.Model, "kaiak.endpoint", rq.endpoint.name)
			}
		}
		if _, ok := errors.AsType[*provider.RefusalError](at.err); ok {
			at.meter.Refused()
		}
		return nil, failure
	}
	at.status = resp.Status()
	at.meter.Answered(resp.Status(), resp.Stream())
	return resp, nil
}

// retryable reports whether an attempt that came to outcome, having sent nothing to
// the client, may be retried (docs/specs/GATEWAY.md, Routing and reliability:
// retries): a connect error or a connection lost before the first event, a stream's
// first-event timeout, a backend 5xx or a busy backend (a stream whose first event was
// an error event among them, as the status its kind matches), the backend refusing
// the gateway's credential (another backend has its own), the backend not serving the
// deployment's model, and the backend's base_url or server leading to no endpoint
// (another deployment does serve). Not retried: a response relayed from the backend (a
// success, a caller's 4xx), a non-stream response timeout (the backend was working on
// a long answer; another would take as long), the client gone, the drain's cut, a
// gateway fault.
func retryable(outcome metrics.AttemptOutcome) bool {
	return slices.Contains(metrics.RetryableOutcomes, outcome)
}

// attemptRules are what a retried attempt's outcome means for the request's next
// attempts, and for its deployment:
//   - backendWide: the failure belongs to the backend rather than the deployment, so
//     the retries refuse all of the model's deployments on that backend — the
//     gateway's credential refused (the same credential would be refused), its
//     base_url leading to no endpoint and its server lacking the endpoint (every
//     deployment on it uses the same). A missing model is the deployment's alone:
//     another model on the same backend may be served.
//   - throttle: the backend is busy, so the deployment cools down
//     (docs/specs/GATEWAY.md, Routing and reliability: 429 cooldown).
var attemptRules = map[metrics.AttemptOutcome]struct{ backendWide, throttle bool }{
	metrics.AttemptAuthFailed:      {backendWide: true},
	metrics.AttemptPathMissing:     {backendWide: true},
	metrics.AttemptEndpointMissing: {backendWide: true},
	metrics.AttemptRateLimited:     {throttle: true},
}

// errorEventKind is the kind of the error event an attempt's error carries — before
// the first event (a CodeErrorEvent) or ending a relayed stream (ErrorEventEnd) — or 0.
func errorEventKind(err error) provider.ErrorEventKind {
	if perr, ok := errors.AsType[*provider.Error](err); ok && perr.Event != nil {
		return perr.Event.Kind
	}
	if end, ok := errors.AsType[*provider.ErrorEventEnd](err); ok {
		return end.Event.Kind
	}
	return 0
}

// avoidAfter adds what the failed attempt at rules out for the request's next
// attempts: retries are failover only, so at's deployment is refused — the client
// (its SDK) retries the same deployment itself, and a gateway retry there would
// multiply its attempts — and, for a failure that belongs to the backend
// (attemptRules), every deployment of m on its backend.
func avoidAfter(avoid routing.Avoid, m *config.Model, at *attempt) routing.Avoid {
	key := routing.IDOf(at.deployment)
	avoid.Refused = append(avoid.Refused, key)
	if attemptRules[at.retryReason].backendWide {
		for _, d := range m.Deployments {
			if d.Backend.ID == key.Backend {
				avoid.Refused = append(avoid.Refused, routing.IDOf(d))
			}
		}
	}
	return avoid
}

// routingRefusal answers a request whose first attempt got no slot.
func routingRefusal(ctx context.Context, rq *request, m *config.Model, err error) *apiError {
	switch {
	case errors.Is(err, routing.ErrQueueFull):
		rq.w.Header().Set("Retry-After", "1")
		return errQueueFull()
	case errors.Is(err, routing.ErrQueueTimeout):
		return errQueueTimeout(m.Queue.Timeout)
	case errors.Is(err, routing.ErrNoHealthyDeployment):
		return errNoHealthyDeployment()
	}
	return canceledAnswer(ctx)
}

// retryRefused ends a request whose retry got no slot: the queue refused it, or no
// deployment is left that it may use. The last attempt answers — its held backend
// response relayed, or its error — and the refusal is logged. A client that left
// (or the drain's cut) gets the canceled answer.
func (rq *request) retryRefused(ctx context.Context, err error) *apiError {
	switch {
	case errors.Is(err, routing.ErrQueueFull):
		rq.retryRefusal = "queue_full"
	case errors.Is(err, routing.ErrQueueTimeout):
		rq.retryRefusal = "queue_timeout"
	case errors.Is(err, routing.ErrNoHealthyDeployment):
		rq.retryRefusal = "no_deployment_left"
	default:
		return canceledAnswer(ctx)
	}
	last := rq.lastAttempt()
	if last.resp == nil {
		return last.failure
	}
	resp := last.resp
	last.resp = nil
	relayResponse(ctx, rq, resp)
	resp.Close()
	return nil
}

// canceledAnswer is the answer of a request whose context ended: the drain cut it
// off, or its client left.
func canceledAnswer(ctx context.Context) *apiError {
	if cutOff(ctx) {
		return errShuttingDown()
	}
	return errClientClosed()
}

// lastAttempt is the request's latest attempt; nil before the first has a slot.
func (rq *request) lastAttempt() *attempt {
	if len(rq.attempts) == 0 {
		return nil
	}
	return rq.attempts[len(rq.attempts)-1]
}

// answeringAttempt is the attempt the log line and the metrics report once the
// request is over: its last, the one that answered — or an empty attempt (no
// deployment, no backend answer) for a request that made none.
func (rq *request) answeringAttempt() *attempt {
	if at := rq.lastAttempt(); at != nil {
		return at
	}
	return &attempt{}
}

// dropResponse closes a held backend error answer that will not be relayed.
func (at *attempt) dropResponse() {
	if at.resp != nil {
		at.resp.Close()
		at.resp = nil
	}
}

// releaseAttempt tells the circuit breaker what attempt at says about its deployment,
// publishes its outcome and duration in the ops metrics — the same moment and
// classification (docs/specs/GATEWAY.md, Observability: upstream attempts) — and
// frees its slot. It runs once per attempt: as a retry is decided — the attempt's end,
// nothing of it relayed — or when the request is over, its relay ended.
func (rq *request) releaseAttempt(at *attempt) {
	if at.released {
		return
	}
	at.released = true
	result, outcome, reason := classifyAttempt(at, rq.relayEnd)
	at.slot.Report(outcome, reason)
	rq.ops.ObserveUpstreamAttempt(at.deployment.Backend.ID, at.deployment.Model, result, time.Since(at.start))
	at.slot.Release()
}

// settle settles attempt at's usage (docs/specs/GATEWAY.md, Usage across
// attempts), once. A retried attempt has a record only when its request reached the
// backend in full and got no answer (the first-event timeout, a connection lost after
// sending): estimated input, no output, estimated and partial. The request's last
// attempt always has one, as every routed request settles — except on a
// token-counting endpoint, where nothing is generated or billed and no attempt has a
// record. rq.usage is the latest record.
func (l *attempts) settle(rq *request, at *attempt) {
	if at.settled {
		return
	}
	at.settled = true
	if rq.endpoint.counts || (at.retryReason != "" && !at.meter.SentUnanswered()) {
		return
	}
	rec := l.recorder.Settle(accounting.Request{
		RequestID:  rq.id,
		KeyID:      rq.identity.KeyID,
		Groups:     rq.identity.Group.PathIDs,
		Model:      rq.snapshot.Models[rq.model],
		Deployment: at.deployment,
		Operation:  rq.endpoint.operation,
		Start:      rq.start,
	}, at.meter, at.retryReason == "" && rq.relayEnd == "")
	rq.records = append(rq.records, rec)
	rq.usage = &rec
}

// totalUsage is the request's usage over all its records (one per attempt that has
// usage): each unit's amount and the cost in nano-USD, summed.
func (rq *request) totalUsage() (accounting.Units, int64) {
	units := make(accounting.Units)
	var cost int64
	for _, rec := range rq.records {
		for unit, n := range rec.Units {
			units[unit] += n
		}
		cost += rec.CostNanoUSD
	}
	return units, cost
}

// end ends the request's last attempt once the request is over, however it ended: a
// held response is closed, its usage settled, the circuit breaker told and its slot
// freed. The last attempt is never retried, even one a retry was decided for (the
// retry found no slot, or the client left first).
func (l *attempts) end(rq *request) {
	at := rq.lastAttempt()
	if at == nil {
		return
	}
	at.retryReason = ""
	at.dropResponse()
	l.settle(rq, at)
	rq.releaseAttempt(at)
}

// classifyAttempt classifies what attempt at's end says about its deployment
// (docs/specs/GATEWAY.md, Routing and reliability: outcome classes), its relay having
// ended for relayEnd ("" when it ran to its end, or nothing was relayed): the named
// outcome (the ops metrics' kaiak.attempt.outcome), its class for the circuit breaker,
// and the reason of a failure. A failure to get a response is classified by its
// provider code (failureRules) or its error event's kind (errorEventRules); a
// provider's refusal is the caller's mistake, nothing asked of the deployment; the
// client gone or the drain's cut before the first event, and a gateway fault, teach
// nothing. After a response: broken off upstream, stalled or ended incomplete is a
// failure; a non-stream response timeout after the first bytes is neutral (a large
// body still arriving: the backend was working); a busy status (429, 529:
// provider.BusyStatus) and another 4xx (the caller's) are neutral, another 5xx a
// failure; a response relayed to its end, or until the client left (or the drain cut
// it), is a success — the backend answered and was serving.
func classifyAttempt(at *attempt, relayEnd string) (metrics.AttemptOutcome, routing.Outcome, string) {
	if at.status == 0 {
		if _, refused := errors.AsType[*provider.RefusalError](at.err); refused {
			return metrics.AttemptClientError, routing.Neutral, ""
		}
		perr, ok := errors.AsType[*provider.Error](at.err)
		switch {
		case !ok && at.err == nil:
			return metrics.AttemptCanceled, routing.Neutral, ""
		case !ok:
			return metrics.AttemptInternal, routing.Neutral, ""
		}
		rule := failureRuleOf(perr)
		if rule.circuit == routing.Neutral {
			return rule.outcome, rule.circuit, ""
		}
		return rule.outcome, rule.circuit, at.err.Error()
	}
	switch relayEnd {
	case relayUpstreamIncomplete:
		if rule, ok := errorEventRules[errorEventKind(at.err)]; ok && rule.circuit == routing.Neutral {
			return rule.outcome, rule.circuit, ""
		}
		return metrics.AttemptBrokeOff, routing.Failure, "response broke off: " + at.err.Error()
	case relayUpstreamFailed, relayUpstreamStalled:
		return metrics.AttemptBrokeOff, routing.Failure, "response broke off: " + at.err.Error()
	case relayUpstreamTimeout:
		return metrics.AttemptResponseTimeout, routing.Neutral, ""
	}
	switch status := at.status; {
	case provider.BusyStatus(status):
		return metrics.AttemptRateLimited, routing.Neutral, ""
	case status >= 500:
		return metrics.AttemptServerError, routing.Failure, fmt.Sprintf("backend answered %d", status)
	case status >= 400:
		return metrics.AttemptClientError, routing.Neutral, ""
	}
	return metrics.AttemptSuccess, routing.Success, ""
}

// failureRule is what a failure to get a response from the backend means: the
// attempt's outcome and its class for the circuit breaker; refused — the backend
// refused the request outright, an answer with nothing processed, so the meter bills
// no input (accounting.Meter.Refused); and the client's answer when the attempt
// answers the request.
type failureRule struct {
	outcome metrics.AttemptOutcome
	circuit routing.Outcome
	refused bool
	answer  func(*provider.Error) *apiError
}

// failureRules are the failure rules by provider code (docs/specs/GATEWAY.md, Routing
// and reliability: outcome classes). Circuit failures: no response, a stream's
// first-event timeout, the backend refusing the gateway's credential (every request
// would fail the same way), not serving the deployment's model, its base_url leading
// to no endpoint. A non-stream response timeout is its own class, neutral but for a
// half-open trial and a run of them (routing.ResponseTimeout); an endpoint missing
// from the backend's server is neutral: the deployment serves its other endpoints. A
// CodeErrorEvent is ruled by its event's kind (errorEventRules); a code without a rule
// is a CodeUnavailable.
var failureRules = map[provider.Code]failureRule{
	provider.CodeUnavailable: {metrics.AttemptUnavailable, routing.Failure, false,
		upstreamAnswer(http.StatusBadGateway, provider.CodeUnavailable, metrics.ErrorUpstreamUnavailable,
			"The model backend could not be reached.")},
	provider.CodeTimeout: {metrics.AttemptTimeout, routing.Failure, false,
		upstreamAnswer(http.StatusGatewayTimeout, provider.CodeTimeout, metrics.ErrorUpstreamTimeout,
			"The model backend did not respond in time.")},
	provider.CodeResponseTimeout: {metrics.AttemptResponseTimeout, routing.ResponseTimeout, false,
		upstreamAnswer(http.StatusGatewayTimeout, provider.CodeTimeout, metrics.ErrorUpstreamTimeout,
			"The model backend did not respond in time.")},
	provider.CodeAuthFailed: {metrics.AttemptAuthFailed, routing.Failure, true,
		upstreamAnswer(http.StatusBadGateway, provider.CodeAuthFailed, metrics.ErrorUpstreamError,
			"The model backend refused the gateway's credentials.")},
	provider.CodeModelMissing: {metrics.AttemptModelMissing, routing.Failure, true,
		upstreamAnswer(http.StatusBadGateway, provider.CodeModelMissing, metrics.ErrorUpstreamError,
			"The model backend does not serve the model.")},
	provider.CodePathMissing: {metrics.AttemptPathMissing, routing.Failure, true,
		upstreamAnswer(http.StatusBadGateway, provider.CodePathMissing, metrics.ErrorUpstreamError,
			"The model backend's address is misconfigured.")},
	provider.CodeEndpointMissing: {metrics.AttemptEndpointMissing, routing.Neutral, true,
		upstreamAnswer(http.StatusBadGateway, provider.CodeEndpointMissing, metrics.ErrorUpstreamError,
			"The model backend's server does not have this endpoint.")},
}

// errorEventRules are what an error event says, by its kind, read as the HTTP status
// the kind matches would be (docs/specs/GATEWAY.md, Providers: error events): the
// backend failing as a 5xx, busy as a 429, the caller's fault as a 4xx. As a stream's
// first event it is a failure to get a response: the backend gave up before its
// answer started, refusing the request, and the client is answered as that status.
// Ending a stream already relayed, class is the request's error class, and the busy
// and the caller's kinds keep their outcome; the backend failing classifies as the
// break it is.
var errorEventRules = map[provider.ErrorEventKind]struct {
	failureRule
	class metrics.ErrorClass
}{
	provider.ErrorEventFailure: {failureRule{metrics.AttemptServerError, routing.Failure, true,
		func(*provider.Error) *apiError { return errUpstreamFault(http.StatusBadGateway) }}, metrics.ErrorUpstreamError},
	provider.ErrorEventBusy: {failureRule{metrics.AttemptRateLimited, routing.Neutral, true,
		func(*provider.Error) *apiError { return errUpstreamOverloaded(http.StatusServiceUnavailable) }},
		metrics.ErrorUpstreamRateLimited},
	provider.ErrorEventCaller: {failureRule{metrics.AttemptClientError, routing.Neutral, true,
		func(perr *provider.Error) *apiError { return errUpstreamRefused(backendIdentifier(perr.Event.Code)) }},
		metrics.ErrorUpstreamClientError},
}

// failureRuleOf is the rule of a failure to get a response: its error event's kind's
// for a CodeErrorEvent (a kind without a rule is the backend failing), else its
// code's.
func failureRuleOf(perr *provider.Error) failureRule {
	if perr.Code == provider.CodeErrorEvent {
		if rule, ok := errorEventRules[perr.Event.Kind]; ok {
			return rule.failureRule
		}
		return errorEventRules[provider.ErrorEventFailure].failureRule
	}
	if rule, ok := failureRules[perr.Code]; ok {
		return rule
	}
	return failureRules[provider.CodeUnavailable]
}

// upstreamFailure records attempt at's provider error and maps it to the client's
// answer: a refusal before sending is the caller's 400, a failure to get a response
// its rule's answer (failureRuleOf).
func upstreamFailure(ctx context.Context, at *attempt, err error) *apiError {
	if ctx.Err() != nil {
		return canceledAnswer(ctx)
	}
	at.err = err
	if refusal, ok := errors.AsType[*provider.RefusalError](err); ok {
		return errRefused(refusal)
	}
	if perr, ok := errors.AsType[*provider.Error](err); ok {
		if perr.Event != nil {
			at.errorCode = backendIdentifier(perr.Event.Code)
		}
		return failureRuleOf(perr).answer(perr)
	}
	return errInternal()
}
