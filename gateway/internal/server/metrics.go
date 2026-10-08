package server

import (
	"time"

	"kaiak/internal/config"
	"kaiak/internal/metrics"
	"kaiak/internal/provider"
)

// observeRequest feeds the ops metrics once a request is over (after settlement, so
// the settled output tokens are known); each attempt was observed as it ended, and
// the time to first token as it came. Label values are only names the config
// declares: the model once it passed the access check, the backend once routed.
func (a *API) observeRequest(rq *request) {
	model := ""
	if rq.modelAllowed {
		model = rq.model
	}
	a.ops.ObserveRequest(rq.endpoint.name(), model, rq.w.status, time.Since(rq.start))
	if class, ok := errorClass(rq); ok {
		a.ops.CountError(class)
		code := errorCode(rq)
		if code == "" {
			code = rq.relayEnd
		}
		a.ops.CountRequestError(rq.identity.Group, rq.keyID, model, code)
	}
	if rej := rq.rejection; rej != nil && !rej.Unavailable {
		a.ops.CountLimitRejection(string(rej.Scope), rej.Type)
	}
	a.observeQueue(rq, model)
	if len(rq.attempts) > 0 {
		a.ops.ObserveAttempts(model, len(rq.attempts))
	}
	if rq.firstContent.IsZero() {
		return
	}
	backend := rq.answeringAttempt().deployment.Backend.ID
	// Decode speed: the tokens after the first over the time after the first, for
	// streams that ran to their end.
	if rq.relayEnd != "" || rq.usage == nil {
		return
	}
	out := rq.usage.Units[config.UnitTokensOut]
	gen := rq.relayDone.Sub(rq.firstContent).Seconds()
	if out >= 2 && gen > 0 {
		a.ops.ObserveOutputRate(model, backend, float64(out-1)/gen)
	}
}

// observeQueue records what the model's queue did for a body request, attempt by
// attempt: the wait of each queued attempt that got its slot, and a refusal — of the
// first attempt (the request's answer) or of a retry.
func (a *API) observeQueue(rq *request, model string) {
	for _, at := range rq.attempts {
		if at.wait.Queued {
			a.ops.ObserveQueueWait(model, at.wait.Duration)
		}
	}
	refusal := rq.retryRefusal
	if rq.failure != nil && len(rq.attempts) == 0 {
		refusal = rq.failure.code
	}
	switch refusal {
	case "queue_full":
		a.ops.CountQueueRejection(model, metrics.QueueFull)
	case "queue_timeout":
		a.ops.CountQueueRejection(model, metrics.QueueTimeout)
	}
}

// errorClass classifies how a request failed, if it did: the gateway's own error
// answer, a relayed backend error status, or a response that broke off.
func errorClass(rq *request) (metrics.ErrorClass, bool) {
	if rq.failure != nil {
		return errorCodeClass(rq.failure.code), true
	}
	at := rq.answeringAttempt()
	switch rq.relayEnd {
	case relayClientClosed:
		return metrics.ErrorClientClosed, true
	case relayUpstreamIncomplete:
		// An error event ending the stream is classed by its kind; a stream ended
		// incomplete without one is the backend's error.
		if rule, ok := errorEventRules[errorEventKind(at.err)]; ok {
			return rule.class, true
		}
		return metrics.ErrorUpstreamError, true
	case relayUpstreamFailed, relayUpstreamStalled:
		return metrics.ErrorUpstreamError, true
	case relayUpstreamTimeout:
		return metrics.ErrorUpstreamTimeout, true
	case relayShutdown:
		return metrics.ErrorShuttingDown, true
	}
	if at.deployment.Backend != nil && rq.w.status >= 400 {
		return relayedStatusClass(rq.w.status), true
	}
	return "", false
}

// errorCode is the request log line's error.type: the gateway's own error answer, or
// for a backend error status relayed as it came, its class; "" for a request that
// got neither — a success, or a response broken off after it started
// (kaiak.relay_end says how).
func errorCode(rq *request) string {
	switch {
	case rq.failure != nil:
		return rq.failure.code
	case rq.answeringAttempt().deployment.Backend != nil && rq.w.status >= 400:
		return string(relayedStatusClass(rq.w.status))
	}
	return ""
}

// relayedStatusClass classifies a backend error status relayed to the client, by
// whose problem it is. A 4xx is the caller's (context too long, a bad parameter):
// the platform cannot fix it. A busy status (provider.BusyStatus) is the backend's
// capacity or quota — the caller did nothing wrong — and gets its own class, apart
// from the gateway's own limits (rate_limited) and from backend faults (5xx). Backend
// 401/403 never get here: they answer upstream_auth_failed.
func relayedStatusClass(status int) metrics.ErrorClass {
	switch {
	case provider.BusyStatus(status):
		return metrics.ErrorUpstreamRateLimited
	case status < 500:
		return metrics.ErrorUpstreamClientError
	}
	return metrics.ErrorUpstreamError
}

// errorCodeClass maps an error code the gateway answers with (docs/specs/GATEWAY.md,
// Client API table) to its class.
func errorCodeClass(code string) metrics.ErrorClass {
	switch code {
	case "missing_api_key", "invalid_api_key":
		return metrics.ErrorAuth
	case "model_not_found", "unknown_url":
		return metrics.ErrorNotFound
	case "invalid_json", "duplicate_member", "invalid_body", "missing_required_parameter", "invalid_type",
		"invalid_value", "n_too_large", "request_too_large", "method_not_allowed", "stateful_responses_unsupported",
		"hosted_tool_unsupported", "price_option_unsupported", "endpoint_not_served", "stored_object_unsupported":
		return metrics.ErrorInvalidRequest
	case "rate_limit_exceeded", "concurrency_limit_exceeded":
		return metrics.ErrorRateLimited
	case "queue_full", "queue_timeout":
		return metrics.ErrorQueueRejected
	case "budget_exceeded":
		return metrics.ErrorBudgetExceeded
	case "budget_unavailable":
		return metrics.ErrorBudgetUnavailable
	case "no_healthy_deployment":
		return metrics.ErrorNoHealthyDeployment
	case "upstream_unavailable":
		return metrics.ErrorUpstreamUnavailable
	case "upstream_timeout":
		return metrics.ErrorUpstreamTimeout
	case "upstream_auth_failed", "upstream_model_missing", "upstream_path_missing", "upstream_endpoint_missing",
		"upstream_error":
		return metrics.ErrorUpstreamError
	case "upstream_overloaded":
		return metrics.ErrorUpstreamRateLimited
	case "upstream_refused":
		return metrics.ErrorUpstreamClientError
	case "client_closed":
		return metrics.ErrorClientClosed
	case "server_shutting_down":
		return metrics.ErrorShuttingDown
	case "config_not_loaded":
		return metrics.ErrorNotReady
	case "server_busy":
		return metrics.ErrorServerBusy
	}
	return metrics.ErrorInternal
}
