package server

import (
	"context"
	"errors"
	"fmt"
	"io"
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
	m := l.missing.exclude(rq.serving, rq.endpoint, time.Now())
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
			meter: accounting.NewMeter(providerEndpoint(rq.endpoint), rq.input.Total), start: time.Now()}
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

// servingDeployments is model m as routing sees it for a request to ep: only the
// deployments whose backend serves ep — m itself when every one does, nil when none
// does (docs/specs/GATEWAY.md, Request pipeline: routing).
func servingDeployments(m *config.Model, ep endpoint) *config.Model {
	n := 0
	for _, d := range m.Deployments {
		if serves(d, ep) {
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
		if serves(d, ep) {
			view.Deployments = append(view.Deployments, d)
		}
	}
	return &view
}

// serves reports whether deployment d's backend serves endpoint ep.
func serves(d config.Deployment, ep endpoint) bool {
	return provider.Serves(d.Backend.Type, providerEndpoint(ep))
}

// send sends the request to attempt at's deployment and waits for the first event. A
// failure before any response is returned as the answer it would get. The attempt's
// meter hears when the request was written in full (it counts the input of an attempt
// that then gets no answer), and how the backend answered.
func (l *attempts) send(ctx context.Context, rq *request, at *attempt) (provider.Response, *apiError) {
	resp, err := l.providers.For(at.deployment.Backend).Send(ctx, &provider.Request{
		Endpoint:     providerEndpoint(rq.endpoint),
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
				l.missing.remember(at.deployment.Backend.ID, rq.endpoint, time.Now(), rq.snapshot.Circuit.ProbeInterval) {
				// Not a circuit failure, so it shows here, once per interval: the
				// operator upgrades the server; meanwhile routing leaves it out for the
				// endpoint.
				l.logger.Warn("the backend's server lacks an endpoint its type serves: an older version?",
					"kaiak.request.id", rq.id, "kaiak.backend.id", at.deployment.Backend.ID,
					"kaiak.deployment.model", at.deployment.Model, "kaiak.endpoint", rq.endpoint.name())
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
	key := routing.DeploymentID{Backend: at.deployment.Backend.ID, Model: at.deployment.Model}
	avoid.Refused = append(avoid.Refused, key)
	if attemptRules[at.retryReason].backendWide {
		for _, d := range m.Deployments {
			if d.Backend.ID == key.Backend {
				avoid.Refused = append(avoid.Refused, routing.DeploymentID{Backend: d.Backend.ID, Model: d.Model})
			}
		}
	}
	return avoid
}

// attemptOutcome is what attempt at got, for the log line: the backend's status, else
// the gateway's error code.
func attemptOutcome(at *attempt) string {
	if at.status != 0 {
		return strconv.Itoa(at.status)
	}
	if at.failure != nil {
		return at.failure.code
	}
	return ""
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
	if rq.endpoint.counts() || (at.retryReason != "" && !at.meter.SentUnanswered()) {
		return
	}
	rec := l.recorder.Settle(accounting.Request{
		RequestID:  rq.id,
		KeyID:      rq.identity.KeyID,
		Groups:     rq.identity.Group.PathIDs,
		Model:      rq.snapshot.Models[rq.model],
		Deployment: at.deployment,
		Start:      rq.start,
	}, at.meter, at.retryReason == "" && rq.relayEnd == "")
	rq.records = append(rq.records, rec)
	rq.usage = &rec
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
// outcome (the ops metrics' label), its class for the circuit breaker, and the reason
// of a failure. A failure to get a response is classified by its provider code
// (failureRules) or its error event's kind (errorEventRules); a provider's refusal is
// the caller's mistake, nothing asked of the deployment; the client gone or the
// drain's cut before the first event, and a gateway fault, teach nothing. After a
// response: broken off upstream, stalled or ended incomplete is a failure; a
// non-stream response timeout after the first bytes is neutral (a large body still
// arriving: the backend was working); a busy status (429, 529: provider.BusyStatus)
// and another 4xx (the caller's) are neutral, another 5xx a failure; a response
// relayed to its end, or until the client left (or the drain cut it), is a success —
// the backend answered and was serving.
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

// providerEndpoint maps a body endpoint to the provider's operation.
func providerEndpoint(ep endpoint) provider.Endpoint {
	switch ep {
	case endpointCompletions:
		return provider.Completions
	case endpointEmbeddings:
		return provider.Embeddings
	case endpointMessages:
		return provider.Messages
	case endpointMessagesCountTokens:
		return provider.MessagesCountTokens
	case endpointResponses:
		return provider.Responses
	case endpointResponsesInputTokens:
		return provider.ResponsesInputTokens
	}
	return provider.ChatCompletions
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

// relayResponse writes the backend's answer to the request's last attempt — its
// status, headers and events — to the client.
// Stream events are flushed one by one as they arrive; nothing is buffered beyond one
// event. The first data event of a stream, or a body's first bytes, under a status
// below 400 tells the circuit breaker the response started. Every event, hidden or not, passes through here: the meter reads each one
// before it is written, so what the backend sent counts even when the client is gone.
// The request body is dropped first: once a response relays no retry can use it, and
// the attempts' meters took its size when they opened. A backend 5xx is answered by
// the gateway instead (answerBackendFault), and so is a 4xx whose body broke before
// its first byte (nothing of it can be relayed).
func relayResponse(ctx context.Context, rq *request, resp provider.Response) {
	rq.releaseBody()
	at := rq.lastAttempt()
	if resp.Status() >= 500 {
		rq.answerBackendFault(at, resp)
		return
	}
	if resp.Status() >= 400 {
		ev, err := resp.Next()
		if err != nil && !errors.Is(err, io.EOF) && ctx.Err() == nil {
			rq.answerBackendFault(at, &peekedResponse{Response: resp, err: err})
			return
		}
		resp = &peekedResponse{Response: resp, ev: ev, err: err}
	}
	header := rq.w.Header()
	for name, values := range resp.Header() {
		header[name] = values
	}
	if resp.Stream() {
		// nginx-style proxies in front of the gateway buffer responses unless told
		// not to, which would hold stream events back.
		header.Set("X-Accel-Buffering", "no")
	}
	at.status = resp.Status()
	defer func() { rq.relayDone = time.Now() }()
	// A relayed error answer's code and type go to the log line, read from its
	// first backendErrorReadMax bytes as they pass.
	var errText []byte
	if resp.Status() >= 400 {
		defer func() { at.errorCode, at.errorType = backendErrorFields(errText) }()
	}
	rq.w.WriteHeader(resp.Status())
	flusher := http.NewResponseController(rq.w)
	started := false
	for {
		ev, err := resp.Next()
		if errors.Is(err, io.EOF) {
			return
		}
		if err != nil {
			if ctx.Err() != nil {
				rq.endRelayCanceled(ctx)
				return
			}
			rq.relayEnd = upstreamRelayEnd(err)
			at.err = err
			rq.abort = true
			return
		}
		at.meter.Observe(ev)
		if resp.Status() >= 400 && len(errText) < backendErrorReadMax {
			errText = append(errText, ev.Data...)
		}
		if !started && (!resp.Stream() || ev.Payload != nil) {
			started = true
			// The response started — a stream's first data event (a comment block
			// proves the connection, not generation), a body's first bytes: a
			// half-open trial is decided now, not at the relay's end
			// (docs/specs/GATEWAY.md, Routing and reliability: half-open).
			if resp.Status() < 400 {
				at.slot.ResponseStarted()
			}
		}
		if rq.firstContent.IsZero() && resp.Stream() && at.meter.StreamContentSeen() {
			// Time to first token is the answering attempt's, from its send: an
			// earlier attempt's failure is not this backend's latency. Published now,
			// while the stream may run on for minutes.
			rq.firstContent = time.Now()
			rq.ttft = rq.firstContent.Sub(at.start)
			rq.ops.ObserveTimeToFirstToken(rq.model, at.deployment.Backend.ID, rq.ttft)
		}
		if ev.Hidden {
			continue
		}
		if _, err := rq.w.Write(ev.Data); err != nil {
			rq.endRelayCanceled(ctx)
			return
		}
		if resp.Stream() {
			if err := flusher.Flush(); err != nil {
				rq.endRelayCanceled(ctx)
				return
			}
		}
	}
}

// peekedResponse hands out an event already read from its response (or the error
// read instead) before the rest.
type peekedResponse struct {
	provider.Response
	ev     provider.Event
	err    error
	served bool
}

func (p *peekedResponse) Next() (provider.Event, error) {
	if !p.served {
		p.served = true
		return p.ev, p.err
	}
	return p.Response.Next()
}

// answerBackendFault answers attempt at's backend 5xx with the gateway's upstream_error under
// the backend's status and its Retry-After headers: the fault is the backend's, and
// its text may name backend internals (hosts, devices, stack traces) that are not
// the caller's business. Its error code and type go to the log line; its message
// never does — a backend can echo request content in it. A 4xx whose body broke
// before its first byte gets the same answer: the refusal is known (the status),
// its text is not.
func (rq *request) answerBackendFault(at *attempt, resp provider.Response) {
	at.status = resp.Status()
	defer func() { rq.relayDone = time.Now() }()
	var text []byte
	for len(text) < backendErrorReadMax {
		ev, err := resp.Next()
		if err != nil {
			break
		}
		at.meter.Observe(ev)
		text = append(text, ev.Data...)
	}
	at.errorCode, at.errorType = backendErrorFields(text)
	header := rq.w.Header()
	for name, values := range resp.Header() {
		if name != "Content-Type" {
			header[name] = values
		}
	}
	rq.failure = errUpstreamFault(resp.Status())
	if resp.Status() == provider.StatusOverloaded {
		rq.failure = errUpstreamOverloaded(provider.StatusOverloaded)
	}
	writeError(rq.w, rq.failure, rq.errorShape())
}

// Why a relayed response stopped early (the log line's kaiak.relay_end;
// docs/specs/GATEWAY.md, Providers: upstream failures).
const (
	relayClientClosed       = "client_closed"
	relayShutdown           = "shutdown"
	relayUpstreamFailed     = "upstream_failed"     // the connection to the backend broke
	relayUpstreamStalled    = "upstream_stalled"    // a stream silent for the stall timeout
	relayUpstreamIncomplete = "upstream_incomplete" // a successful response ended before it was complete
	relayUpstreamTimeout    = "upstream_timeout"    // a non-stream response past its response timeout
)

// upstreamRelayEnd is the kaiak.relay_end of a relay the backend side broke off with err.
func upstreamRelayEnd(err error) string {
	switch {
	case errors.Is(err, provider.ErrStalled):
		return relayUpstreamStalled
	case errors.Is(err, provider.ErrIncomplete):
		return relayUpstreamIncomplete
	case errors.Is(err, provider.ErrResponseTimeout):
		return relayUpstreamTimeout
	}
	return relayUpstreamFailed
}

// endRelayCanceled ends a relay whose client side went away: the drain cut it off
// ("shutdown": the connection is cut, so the truncated response cannot look
// complete), or the client left ("client_closed").
func (rq *request) endRelayCanceled(ctx context.Context) {
	if cutOff(ctx) {
		rq.relayEnd = relayShutdown
		rq.abort = true
		return
	}
	rq.relayEnd = relayClientClosed
}
