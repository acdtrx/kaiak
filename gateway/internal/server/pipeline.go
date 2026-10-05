package server

import (
	"context"
	"net/http"
	"time"

	"kaiak/internal/accounting"
	"kaiak/internal/auth"
	"kaiak/internal/config"
	"kaiak/internal/limits"
	"kaiak/internal/metrics"
	"kaiak/internal/provider"
	"kaiak/internal/routing"
)

// endpoint is a client API operation the gateway serves. The zero value is none: a
// request refused before it matched an endpoint.
type endpoint int

const (
	endpointNone endpoint = iota
	endpointChatCompletions
	endpointCompletions
	endpointEmbeddings
	endpointListModels
	endpointGetModel
	endpointModelProps
)

// path is the endpoint's route, as the client API documents it.
func (e endpoint) path() string {
	switch e {
	case endpointChatCompletions:
		return "/v1/chat/completions"
	case endpointCompletions:
		return "/v1/completions"
	case endpointEmbeddings:
		return "/v1/embeddings"
	case endpointListModels:
		return "/v1/models"
	case endpointGetModel:
		return "/v1/models/{id}"
	case endpointModelProps:
		return "/v1/models/{id}/props"
	}
	return ""
}

// name is the endpoint's metric label value.
func (e endpoint) name() string {
	switch e {
	case endpointChatCompletions:
		return "chat_completions"
	case endpointCompletions:
		return "completions"
	case endpointEmbeddings:
		return "embeddings"
	case endpointListModels:
		return "list_models"
	case endpointGetModel:
		return "get_model"
	case endpointModelProps:
		return "model_props"
	}
	return ""
}

// operationName is the endpoint's gen_ai.operation.name on the log line: the GenAI
// convention's well-known value, "" for the model endpoints, which have none.
func (e endpoint) operationName() string {
	switch e {
	case endpointChatCompletions:
		return "chat"
	case endpointCompletions:
		return "text_completion"
	case endpointEmbeddings:
		return "embeddings"
	}
	return ""
}

// takesBody reports whether the endpoint is a POST carrying a JSON request body.
func (e endpoint) takesBody() bool {
	return e == endpointChatCompletions || e == endpointCompletions || e == endpointEmbeddings
}

// namesModel reports whether a request to the endpoint names one model (in its body
// or its path), which the caller must be allowed to use.
func (e endpoint) namesModel() bool {
	return e != endpointListModels
}

// request is the state of one client request as it moves through the pipeline. Each
// stage reads what earlier stages set and adds its own part.
type request struct {
	w        *statusWriter
	r        *http.Request
	endpoint endpoint
	// id is the request ID: the client's x-request-id when valid, else generated.
	id    string
	start time.Time
	// snapshot is the config the request runs under, taken once at its start.
	snapshot *config.Snapshot
	// ops are the ops metrics: attempts are observed as they end, not only the
	// request once it is over.
	ops *metrics.Ops

	// Set by the auth stage. The key itself is never kept.
	identity auth.Identity

	// model is the public model name the client asked for: set by the route for the
	// model endpoints, by the inbound stage from the body otherwise.
	model string
	// modelAllowed: model passed the model-access check, so it is a name the config
	// declares (safe as a metric label value).
	modelAllowed bool
	// Set by the inbound stage: the raw body, kept for passthrough, the fields the
	// gateway owns, and the input estimate every stage reads (limits, the output
	// default, estimated records) — taken once, from the body as received.
	body    []byte
	inbound inboundFields
	input   accounting.InputEstimate
	// bodies is the budget the body's bytes are taken from; bodyHeld the bytes taken
	// and not yet given back.
	bodies   *BodyBudget
	bodyHeld int64

	// Set by the model_params stage for the body endpoints: the top-level parameters
	// the model's config sets, and the request's effective output limit — the most
	// tokens it may generate as sent to the backend; nil when unbounded or unknown.
	params      []provider.Param
	outputLimit *int64

	// Set by the attempt loop (routing, accounting, provider) for the body endpoints:
	// every attempt, the latest last; the latest attempt's deployment and the meter
	// its relay feeds; what the model's queue did for the request over all its
	// attempts (queued at least once, the waits summed); why a retry got no slot, if
	// one did not ("queue_full", "queue_timeout", "no_deployment_left"), or why a
	// retry was not sent ("retry_budget").
	attempts     []*attempt
	deployment   config.Deployment
	meter        *accounting.Meter
	queueWait    routing.Wait
	retryRefusal string
	// records are the request's usage records, one per attempt that has one; usage is
	// the latest — the answering attempt's once the request is over. Settled before
	// the finishers registered earlier run (limits), and before the log line.
	records []accounting.UsageRecord
	usage   *accounting.UsageRecord

	// finishers run once the request is over, whatever stage ended it, in reverse
	// order of registration: they release what a stage holds for the request's
	// whole run.
	finishers []func()

	// For the request log line.
	failure *apiError
	// rejection is the limit that refused the request, when one did.
	rejection   *limits.Rejection
	authFailure auth.Code
	keyID       string
	// upstreamErr is the latest attempt's provider failure, or the error that broke
	// off a relayed response. It may name backend addresses, never credentials.
	upstreamErr error
	// upstreamErrorCode and upstreamErrorType are the error code and type a backend
	// error answer's body names (backendErrorFields) — a relayed 4xx, a 5xx answered
	// by the gateway: logged, never its message.
	upstreamErrorCode, upstreamErrorType string
	// upstreamStatus is the latest attempt's backend response status; 0 when the
	// backend gave no response.
	upstreamStatus int
	// firstContent is when a relayed stream carried its first generated content;
	// ttft is the time from the answering attempt's send to it; relayDone is when the
	// relay stopped. Zero when it did not happen.
	firstContent time.Time
	ttft         time.Duration
	relayDone    time.Time
	// relayEnd says why a relayed response stopped early (relayClientClosed, …);
	// "" when it ran to its end.
	relayEnd string
	// abort: the response broke off after it started, so the client connection is
	// cut instead of ending the body cleanly — a truncated response must not look
	// complete.
	abort bool
}

// stage is one pipeline step. A non-nil error ends the pipeline: it is written as the
// response and no later stage runs.
type stage struct {
	name string
	run  func(ctx context.Context, rq *request) *apiError
}

// finish runs the request's finishers. It is deferred by the request's handler, so it
// runs even when the handler cuts the connection by panicking.
func (rq *request) finish() {
	for i := len(rq.finishers) - 1; i >= 0; i-- {
		rq.finishers[i]()
	}
}

// newPipeline returns the stages every client request passes, in order
// (docs/specs/GATEWAY.md, Request pipeline). A new stage is a new entry here.
// Admission comes first: a draining gateway, or one with no config, refuses before
// reading anything. The per-key concurrency limit follows auth, before the body is
// read: a key's idle connections declaring bodies count against it. The attempts stage is routing, accounting and provider run per
// attempt — retries go through all three again — and answers the body endpoints; the
// model endpoints reach the terminal stage. Accounting opens each attempt's meter
// before its provider call because it must see the response as it is relayed, and
// settles however the request ends (a finisher). Limits run after model_params
// (they reserve the effective output limit) and before routing — once per client
// request, whatever the attempts; their finisher is registered before the attempt
// loop's, so it runs after settlement.
func newPipeline(drain *Drain, keys *keyInFlight, bodies *BodyBudget, providers *provider.Registry, limiter *limits.Limiter,
	router *routing.Router, recorder *accounting.Recorder) []stage {
	budget := newRetryBudget(time.Now)
	return []stage{
		{"admission", func(_ context.Context, rq *request) *apiError { return admit(drain, rq) }},
		{"auth", authenticateKey},
		{"key_concurrency", func(_ context.Context, rq *request) *apiError { return limitKeyConcurrency(rq, keys) }},
		{"inbound", func(ctx context.Context, rq *request) *apiError { return readInbound(ctx, rq, bodies) }},
		{"model_access", authorizeModel},
		{"model_params", applyModelParams},
		{"limits", func(_ context.Context, rq *request) *apiError { return checkLimits(rq, limiter) }},
		{"attempts", func(ctx context.Context, rq *request) *apiError {
			return sendAttempts(ctx, rq, router, recorder, providers, budget)
		}},
		{"models", answerModelEndpoint},
	}
}

// authenticateKey resolves the bearer key before any body is read, so an
// unauthenticated client never makes the gateway buffer a request body.
func authenticateKey(_ context.Context, rq *request) *apiError {
	id, err := auth.Authenticate(rq.snapshot, rq.r.Header.Get("Authorization"), rq.start)
	if err != nil {
		rq.authFailure = err.Code
		rq.keyID = err.KeyID
		return authError(err)
	}
	rq.identity = id
	rq.keyID = id.KeyID
	return nil
}

// authorizeModel applies the one model-access check (auth.Identity.AuthorizeModel).
// Later stages may assume the model exists in the snapshot.
func authorizeModel(_ context.Context, rq *request) *apiError {
	if !rq.endpoint.namesModel() {
		return nil
	}
	if err := rq.identity.AuthorizeModel(rq.model); err != nil {
		return authError(err)
	}
	rq.modelAllowed = true
	return nil
}
