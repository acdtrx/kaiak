package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"kaiak/internal/accounting"
	"kaiak/internal/config"
	"kaiak/internal/metrics"
	"kaiak/internal/provider"
	"kaiak/internal/routing"
)

// Retry reasons: why a failed attempt may be retried (docs/specs/GATEWAY.md, Routing
// and reliability: retries). They are the reason label of kaiak_retries_total.
const (
	retryUnavailable  = "unavailable"   // connect error, lost before the first event
	retryTimeout      = "timeout"       // a stream's first-event timeout
	retryServerError  = "server_error"  // a backend 5xx
	retryRateLimited  = "rate_limited"  // a backend 429
	retryAuthFailed   = "auth_failed"   // the backend refused the gateway's credential
	retryModelMissing = "model_missing" // the backend does not serve the deployment's model
	retryPathMissing  = "path_missing"  // the backend's base_url leads to no endpoint
)

// attempt is one try of a routed request on one deployment: its slot, the meter the
// relay feeds, and what became of it.
type attempt struct {
	slot       routing.Slot
	deployment config.Deployment
	wait       routing.Wait
	meter      *accounting.Meter
	// failure is the gateway's answer to a failure before any response (nil when the
	// backend answered); resp is a backend error answer held back while the next
	// attempt looks for a slot — relayed if none is found, closed otherwise.
	failure *apiError
	resp    provider.Response
	// retryReason is why the attempt was retried; "" for the request's last attempt.
	retryReason string
	// outcome is what the attempt got, for the log line's tried field: the backend's
	// status, or the gateway's error code.
	outcome string
	// start is when the attempt was sent.
	start time.Time
	// released: the circuit breaker was told and the slot freed; settled: the
	// attempt's usage was settled (or it was decided it has none).
	released, settled bool
}

// sendAttempts is the pipeline's routing, accounting and provider stages, run as one
// attempt loop (docs/specs/GATEWAY.md, Request pipeline and Routing and reliability:
// retries): each attempt takes a slot (routing: the eligible deployment with the
// fewest in flight, else a wait in the model's queue), opens its meter (accounting)
// and sends upstream (provider). An attempt that failed before anything reached the
// client is retried while the model's max_attempts allow and retryReason says so;
// the attempt that answers — or the last one — is relayed or answered with its
// error. A request refused before its first attempt has a slot never reaches
// accounting: no usage record; the limits finisher releases its reservation.
// model_access has checked the model exists in the snapshot.
func sendAttempts(ctx context.Context, rq *request, router *routing.Router, recorder *accounting.Recorder,
	providers *provider.Registry, budget *retryBudget) *apiError {
	if !rq.endpoint.takesBody() {
		return nil
	}
	m := rq.snapshot.Models[rq.model]
	// However the request ends, its last attempt settles, tells the circuit breaker
	// and frees its slot.
	rq.finishers = append(rq.finishers, func() { rq.endAttempt(recorder) })
	var avoid routing.Avoid
	for {
		prev := rq.lastAttempt()
		if prev != nil && ctx.Err() != nil {
			// The client left, or the drain cut the request, between attempts: no
			// further attempt.
			return canceledAnswer(ctx)
		}
		slot, wait, err := router.Acquire(ctx, m, avoid)
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
			rq.settleAttempt(prev, recorder)
			// The retry is sent: counted now, not when the request is over.
			rq.ops.CountRetry(rq.model, prev.deployment.Backend.ID, prev.retryReason)
		}

		at := &attempt{slot: slot, deployment: slot.Deployment, wait: wait,
			meter: accounting.NewMeter(providerEndpoint(rq.endpoint), rq.input.Total), start: time.Now()}
		rq.attempts = append(rq.attempts, at)
		rq.deployment, rq.meter = at.deployment, at.meter
		rq.upstreamErr, rq.upstreamStatus = nil, 0

		if prev == nil {
			budget.attempt(rq.model)
		} else {
			budget.retry(rq.model)
		}
		resp, failure := sendAttempt(ctx, rq, providers)
		if rq.upstreamStatus == http.StatusTooManyRequests {
			slot.Throttled(throttleCooldown(resp.Header(), time.Now()))
		}
		reason := retryReason(rq)
		retry := reason != "" && len(rq.attempts) < m.MaxAttempts
		if retry && !budget.allowRetry(rq.model) {
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
		at.failure, at.resp, at.retryReason = failure, resp, reason
		at.outcome = attemptOutcome(rq, failure)
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

// sendAttempt sends the request to its current attempt's deployment and waits for
// the first event. A failure before any response is returned as the answer it would
// get. The attempt's meter hears when the request was written in full (it counts the
// input of an attempt that then gets no answer), and how the backend answered.
func sendAttempt(ctx context.Context, rq *request, providers *provider.Registry) (provider.Response, *apiError) {
	resp, err := providers.For(rq.deployment.Backend).Send(ctx, &provider.Request{
		Endpoint:     providerEndpoint(rq.endpoint),
		Deployment:   rq.deployment,
		Body:         rq.body,
		Stream:       rq.inbound.Stream,
		IncludeUsage: rq.inbound.IncludeUsage,
		RequestID:    rq.id,
		PublicModel:  rq.model,
		Params:       rq.params,
		Sent:         rq.meter.Sent,
	})
	if err != nil {
		failure := upstreamFailure(ctx, rq, err)
		if perr, ok := errors.AsType[*provider.Error](rq.upstreamErr); ok &&
			(perr.Code == provider.CodeAuthFailed || perr.Code == provider.CodeModelMissing ||
				perr.Code == provider.CodePathMissing) {
			rq.meter.Refused()
		}
		return nil, failure
	}
	rq.upstreamStatus = resp.Status()
	rq.meter.Answered(resp.Status(), resp.Stream())
	return resp, nil
}

// retryReason is the one retry decision (docs/specs/GATEWAY.md, Routing and
// reliability: retries), for the current attempt, which has sent nothing to the
// client yet: a connect error or a connection lost before the first event, a
// stream's first-event timeout, a backend 5xx or 429, the backend refusing the
// gateway's credential (another backend has its own), the backend not serving the
// deployment's model and the backend's base_url leading to no endpoint (another
// deployment does serve) may be retried; "" otherwise —
// a response relayed from the backend (a success, a caller's 4xx), a non-stream
// response timeout (the backend was working on a long answer; another would take as
// long), the client gone, the drain's cut, a gateway fault.
func retryReason(rq *request) string {
	if status := rq.upstreamStatus; status != 0 {
		switch {
		case status == http.StatusTooManyRequests:
			return retryRateLimited
		case status >= 500:
			return retryServerError
		}
		return ""
	}
	perr, ok := errors.AsType[*provider.Error](rq.upstreamErr)
	if !ok {
		return ""
	}
	switch perr.Code {
	case provider.CodeTimeout:
		return retryTimeout
	case provider.CodeResponseTimeout:
		return ""
	case provider.CodeAuthFailed:
		return retryAuthFailed
	case provider.CodeModelMissing:
		return retryModelMissing
	case provider.CodePathMissing:
		return retryPathMissing
	}
	return retryUnavailable
}

// avoidAfter adds what the failed attempt at rules out for the request's next
// attempts: retries are failover only, so at's deployment is refused — the client
// (its SDK) retries the same deployment itself, and a gateway retry there would
// multiply its attempts. A failure that belongs to the backend rather than the
// deployment refuses all of m's deployments on that backend: the gateway's
// credential refused (the same credential would be refused) and its base_url leading
// to no endpoint (every deployment on it uses the same base_url). A missing model is
// the deployment's alone: another model on the same backend may be served.
func avoidAfter(avoid routing.Avoid, m *config.Model, at *attempt) routing.Avoid {
	key := routing.DeploymentID{Backend: at.deployment.Backend.ID, Model: at.deployment.Model}
	avoid.Refused = append(avoid.Refused, key)
	if at.retryReason == retryAuthFailed || at.retryReason == retryPathMissing {
		for _, d := range m.Deployments {
			if d.Backend.ID == key.Backend {
				avoid.Refused = append(avoid.Refused, routing.DeploymentID{Backend: d.Backend.ID, Model: d.Model})
			}
		}
	}
	return avoid
}

// attemptOutcome is what the current attempt got, for the log line: the backend's
// status, else the gateway's error code.
func attemptOutcome(rq *request, failure *apiError) string {
	if rq.upstreamStatus != 0 {
		return strconv.Itoa(rq.upstreamStatus)
	}
	if failure != nil {
		return failure.code
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

// dropResponse closes a held backend error answer that will not be relayed.
func (at *attempt) dropResponse() {
	if at.resp != nil {
		at.resp.Close()
		at.resp = nil
	}
}

// releaseAttempt tells the circuit breaker what the attempt, the current one, says
// about its deployment, publishes its outcome and duration in the ops metrics — the
// same moment and classification (docs/specs/GATEWAY.md, Observability: upstream
// attempts) — and frees its slot. It runs once per attempt: as a retry is decided —
// the attempt's end, nothing of it relayed — or when the request is over, its relay
// ended.
func (rq *request) releaseAttempt(at *attempt) {
	if at.released {
		return
	}
	at.released = true
	result, outcome, reason := classifyAttempt(rq)
	at.slot.Report(outcome, reason)
	rq.ops.ObserveUpstreamAttempt(at.deployment.Backend.ID, at.deployment.Model, result, time.Since(at.start))
	at.slot.Release()
}

// settleAttempt settles an attempt's usage (docs/specs/GATEWAY.md, Usage across
// attempts), once. A retried attempt has a record only when its request reached the
// backend in full and got no answer (the first-event timeout, a connection lost after
// sending): estimated input, no output, estimated and partial. The request's last
// attempt always has one, as every routed request settles. rq.usage is the latest
// record.
func (rq *request) settleAttempt(at *attempt, recorder *accounting.Recorder) {
	if at.settled {
		return
	}
	at.settled = true
	if at.retryReason != "" && !at.meter.SentUnanswered() {
		return
	}
	rec := recorder.Settle(accounting.Request{
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

// endAttempt ends the request's last attempt once the request is over, however it
// ended: a held response is closed, its usage settled, the circuit breaker told and
// its slot freed. The last attempt is never retried, even one a retry was decided
// for (the retry found no slot, or the client left first).
func (rq *request) endAttempt(recorder *accounting.Recorder) {
	at := rq.lastAttempt()
	if at == nil {
		return
	}
	at.retryReason = ""
	at.dropResponse()
	rq.settleAttempt(at, recorder)
	rq.releaseAttempt(at)
}

// classifyAttempt classifies what an attempt's end says about its deployment
// (docs/specs/GATEWAY.md, Routing and reliability: outcome classes): the named
// outcome (the ops metrics' label), its class for the circuit breaker, and the
// reason of a failure. Failures: no response (connect error, broken before the
// first event), a stream's first-event timeout, the backend refusing the gateway's
// credential (every request would fail the same way), the backend not serving the
// deployment's model, the backend's base_url leading to no endpoint, a backend 5xx, a response broken off upstream, stalled or
// ended incomplete. A non-stream response timeout before the first bytes is its own
// class, neutral but for a half-open trial and a run of them (routing.ResponseTimeout).
// Neutral: a non-stream response timeout after the first bytes (a large body still
// arriving: the backend was working), a backend 429 (busy,
// not broken), another backend 4xx (the caller's), the client gone or the drain's
// cut before the first event (nothing learned), a gateway fault. Success: a
// response relayed to its end, or until the client left (or the drain cut it) — the
// backend answered and was serving.
func classifyAttempt(rq *request) (metrics.AttemptOutcome, routing.Outcome, string) {
	if rq.upstreamStatus == 0 {
		perr, ok := errors.AsType[*provider.Error](rq.upstreamErr)
		switch {
		case !ok && rq.upstreamErr == nil:
			return metrics.AttemptCanceled, routing.Neutral, ""
		case !ok:
			return metrics.AttemptInternal, routing.Neutral, ""
		}
		switch perr.Code {
		case provider.CodeResponseTimeout:
			return metrics.AttemptResponseTimeout, routing.ResponseTimeout, rq.upstreamErr.Error()
		case provider.CodeTimeout:
			return metrics.AttemptTimeout, routing.Failure, rq.upstreamErr.Error()
		case provider.CodeAuthFailed:
			return metrics.AttemptAuthFailed, routing.Failure, rq.upstreamErr.Error()
		case provider.CodeModelMissing:
			return metrics.AttemptModelMissing, routing.Failure, rq.upstreamErr.Error()
		case provider.CodePathMissing:
			return metrics.AttemptPathMissing, routing.Failure, rq.upstreamErr.Error()
		}
		return metrics.AttemptUnavailable, routing.Failure, rq.upstreamErr.Error()
	}
	switch rq.relayEnd {
	case relayUpstreamFailed, relayUpstreamStalled, relayUpstreamIncomplete:
		return metrics.AttemptBrokeOff, routing.Failure, "response broke off: " + rq.upstreamErr.Error()
	case relayUpstreamTimeout:
		return metrics.AttemptResponseTimeout, routing.Neutral, ""
	}
	switch status := rq.upstreamStatus; {
	case status >= 500:
		return metrics.AttemptServerError, routing.Failure, fmt.Sprintf("backend answered %d", status)
	case status == http.StatusTooManyRequests:
		return metrics.AttemptRateLimited, routing.Neutral, ""
	case status >= 400:
		return metrics.AttemptClientError, routing.Neutral, ""
	}
	return metrics.AttemptSuccess, routing.Success, ""
}

// providerEndpoint maps a body endpoint to the provider's operation.
func providerEndpoint(ep endpoint) provider.Endpoint {
	switch ep {
	case endpointCompletions:
		return provider.Completions
	case endpointEmbeddings:
		return provider.Embeddings
	}
	return provider.ChatCompletions
}

// upstreamFailure maps a provider error to the client's answer.
func upstreamFailure(ctx context.Context, rq *request, err error) *apiError {
	if ctx.Err() != nil {
		return canceledAnswer(ctx)
	}
	rq.upstreamErr = err
	if perr, ok := errors.AsType[*provider.Error](err); ok {
		return errUpstream(perr.Code)
	}
	return errInternal()
}

// relayResponse writes the backend's status, headers and events to the client.
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
	if resp.Status() >= 500 {
		rq.answerBackendFault(resp)
		return
	}
	if resp.Status() >= 400 {
		ev, err := resp.Next()
		if err != nil && !errors.Is(err, io.EOF) && ctx.Err() == nil {
			rq.answerBackendFault(&peekedResponse{Response: resp, err: err})
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
	rq.upstreamStatus = resp.Status()
	defer func() { rq.relayDone = time.Now() }()
	// A relayed error answer's code and type go to the log line, read from its
	// first backendErrorReadMax bytes as they pass.
	var errText []byte
	if resp.Status() >= 400 {
		defer func() { rq.upstreamErrorCode, rq.upstreamErrorType = backendErrorFields(errText) }()
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
			rq.upstreamErr = err
			rq.abort = true
			return
		}
		rq.meter.Observe(ev)
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
				rq.lastAttempt().slot.ResponseStarted()
			}
		}
		if rq.firstContent.IsZero() && resp.Stream() && rq.meter.StreamContentSeen() {
			// Time to first token is the answering attempt's, from its send: an
			// earlier attempt's failure is not this backend's latency. Published now,
			// while the stream may run on for minutes.
			rq.firstContent = time.Now()
			at := rq.lastAttempt()
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

// answerBackendFault answers a backend 5xx with the gateway's upstream_error under
// the backend's status and its Retry-After headers: the fault is the backend's, and
// its text may name backend internals (hosts, devices, stack traces) that are not
// the caller's business. Its error code and type go to the log line; its message
// never does — a backend can echo request content in it. A 4xx whose body broke
// before its first byte gets the same answer: the refusal is known (the status),
// its text is not.
func (rq *request) answerBackendFault(resp provider.Response) {
	rq.upstreamStatus = resp.Status()
	defer func() { rq.relayDone = time.Now() }()
	var text []byte
	for len(text) < backendErrorReadMax {
		ev, err := resp.Next()
		if err != nil {
			break
		}
		rq.meter.Observe(ev)
		text = append(text, ev.Data...)
	}
	rq.upstreamErrorCode, rq.upstreamErrorType = backendErrorFields(text)
	header := rq.w.Header()
	for name, values := range resp.Header() {
		if name != "Content-Type" {
			header[name] = values
		}
	}
	rq.failure = errUpstreamFault(resp.Status())
	writeError(rq.w, rq.failure)
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
