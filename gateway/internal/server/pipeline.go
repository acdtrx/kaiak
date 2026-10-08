package server

import (
	"context"
	"log/slog"
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

// endpoint is a client API operation the gateway serves: one row, whose fields are
// what the pipeline decides by endpoint. A request refused before it matched one has
// none (nil).
type endpoint struct {
	// name is the endpoint's name on log lines (kaiak.endpoint).
	name string
	// path is a model endpoint's route as the client API documents it; a body
	// endpoint's is its API's (route).
	path string
	// operation is the endpoint's gen_ai.operation.name on the log line and on its
	// usage records (the usage metrics' label): the GenAI convention's well-known
	// value — a chat operation in any of its APIs is "chat" — and "" for the model and
	// token-counting endpoints, which have none and settle no usage record.
	operation string
	// body: the endpoint is a POST carrying a JSON request body in api's format,
	// passed through to a backend serving api; its route is api's path under /v1/
	// (route).
	body bool
	api  provider.Endpoint
	// counts: the endpoint only counts a request's tokens: nothing is generated or
	// billed there, so it reserves no tokens and settles into no usage record
	// (docs/specs/GATEWAY.md, Client API → token-counting endpoints).
	counts bool
	// outputLimitKeys are the endpoint's output-limit parameters; the first is the one
	// a default is set under. Chat takes max_completion_tokens, OpenAI's current
	// field, which vLLM, SGLang, llama-server, OpenAI and Azure all read — and which
	// OpenAI's and Azure's reasoning models require (they refuse max_tokens) — and
	// still honors the older max_tokens. Completions and Messages have only
	// max_tokens (Messages requires it, so a model with an output limit always sends
	// one), Responses only max_output_tokens. Embeddings and the token-counting
	// endpoints generate nothing. The inbound stage reads these keys alone: another
	// output-limit key the client sends is not the endpoint's and passes untouched.
	outputLimitKeys []string
	// anthropicOnHeader: the endpoint answers in Anthropic's shape when the request
	// carries an anthropic-version header (answersAnthropic).
	anthropicOnHeader bool
}

// bodyEndpoints are the endpoints served as a POST carrying a JSON request body, one
// row per provider.Endpoint the client API serves.
var bodyEndpoints = []*endpoint{
	{body: true, api: provider.ChatCompletions, name: "chat_completions", operation: "chat",
		outputLimitKeys: []string{"max_completion_tokens", "max_tokens"}},
	{body: true, api: provider.Completions, name: "completions", operation: "text_completion",
		outputLimitKeys: []string{"max_tokens"}},
	{body: true, api: provider.Embeddings, name: "embeddings", operation: "embeddings"},
	{body: true, api: provider.Messages, name: "messages", operation: "chat",
		outputLimitKeys: []string{"max_tokens"}},
	{body: true, api: provider.MessagesCountTokens, name: "messages_count_tokens", counts: true},
	{body: true, api: provider.Responses, name: "responses", operation: "chat",
		outputLimitKeys: []string{"max_output_tokens"}},
	{body: true, api: provider.ResponsesInputTokens, name: "responses_input_tokens", counts: true},
}

// The model endpoints, answered from the config. The model list and entry answer in
// Anthropic's shape when asked for it: Anthropic's SDKs send anthropic-version on
// every request, and OpenAI's never do.
var (
	endpointListModels = &endpoint{name: "list_models", path: "/v1/models", anthropicOnHeader: true}
	endpointGetModel   = &endpoint{name: "get_model", path: "/v1/models/{model}", anthropicOnHeader: true}
	endpointModelProps = &endpoint{name: "model_props", path: "/v1/models/{model}/props"}
)

// route is the endpoint's route as the client API documents it — never a request's
// path: {model} stays literal. It is the request duration's http.route.
func (e *endpoint) route() string {
	if e.body {
		return "/v1/" + e.api.Path()
	}
	return e.path
}

// answersAnthropic reports whether a request r to e is answered in Anthropic's shape —
// its errors (errorShape) and, on the model list and entry, the answer itself: a body
// endpoint in the shape of the API it speaks, a model endpoint as its row says.
func (e *endpoint) answersAnthropic(r *http.Request) bool {
	if e.body {
		return e.api.Format() == provider.FormatMessages
	}
	return e.anthropicOnHeader && r.Header.Get("Anthropic-Version") != ""
}

// request is the state of one client request as it moves through the pipeline. Each
// stage reads what earlier stages set and adds its own part.
type request struct {
	w        *statusWriter
	r        *http.Request
	endpoint *endpoint
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
	// serving is the model as routing sees it for a body request: only its
	// deployments whose backend serves the endpoint (servingDeployments).
	serving *config.Model
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
	// every attempt, the latest last — each with its deployment, its meter and the
	// backend's answer; what the model's queue did for the request over all its
	// attempts (queued at least once, the waits summed); why a retry got no slot, if
	// one did not ("queue_full", "queue_timeout", "no_deployment_left"), or why a
	// retry was not sent ("retry_budget").
	attempts     []*attempt
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

// stage is one pipeline step, run for the requests of its scope. A non-nil error ends
// the pipeline: it is written as the response and no later stage runs.
type stage struct {
	name  string
	run   func(ctx context.Context, rq *request) *apiError
	scope stageScope
}

// stageScope is the requests a stage runs for. Both kinds of request pass the one
// pipeline: the stages they share run in the same order for both.
type stageScope int

const (
	// everyRequest: the body endpoints and the model endpoints.
	everyRequest stageScope = iota
	// bodyRequests: the body endpoints only — the body, the routed attempts.
	bodyRequests
	// modelRequests: the model endpoints only, answered from the config.
	modelRequests
)

// covers reports whether a stage of scope s runs for a request to ep.
func (s stageScope) covers(ep *endpoint) bool {
	switch s {
	case bodyRequests:
		return ep.body
	case modelRequests:
		return !ep.body
	}
	return true
}

// finish runs the request's finishers. It is deferred by the request's handler, so it
// runs even when the handler cuts the connection by panicking.
func (rq *request) finish() {
	for i := len(rq.finishers) - 1; i >= 0; i-- {
		rq.finishers[i]()
	}
}

// newPipeline returns the stages every client request passes, in order, each with
// the requests it runs for (docs/specs/GATEWAY.md, Request pipeline). A new stage is
// a new entry here. Admission comes first: a draining gateway, or one with no config,
// refuses before reading anything. The per-key concurrency limit follows auth, before
// the body is read: a key's idle connections declaring bodies count against it. The
// model endpoints pass admission, auth, key concurrency and model access, then reach
// their terminal stage. The attempts stage is routing, accounting and provider run per
// attempt — retries go through all three again — and answers the body endpoints.
// Accounting opens each attempt's meter before its provider call because it must see
// the response as it is relayed, and settles however the request ends (a finisher).
// Limits run after model_params (they reserve the effective output limit) and before
// routing — once per client request, whatever the attempts; their finisher is
// registered before the attempt loop's, so it runs after settlement.
func newPipeline(drain *Drain, keys *keyInFlight, bodies *BodyBudget, providers *provider.Registry, limiter *limits.Limiter,
	router *routing.Router, missing *MissingEndpoints, recorder *accounting.Recorder, logger *slog.Logger) []stage {
	loop := &attempts{router: router, recorder: recorder, providers: providers, budget: newRetryBudget(time.Now),
		missing: missing, logger: logger}
	return []stage{
		{"admission", func(_ context.Context, rq *request) *apiError { return admit(drain, rq) }, everyRequest},
		{"auth", authenticateKey, everyRequest},
		{"key_concurrency", func(_ context.Context, rq *request) *apiError { return limitKeyConcurrency(rq, keys) }, everyRequest},
		{"inbound", func(ctx context.Context, rq *request) *apiError { return readInbound(ctx, rq, bodies) }, bodyRequests},
		{"model_access", authorizeModel, everyRequest},
		{"endpoint_support", findServingDeployments, bodyRequests},
		{"model_params", applyModelParams, bodyRequests},
		{"limits", func(_ context.Context, rq *request) *apiError { return checkLimits(rq, limiter) }, bodyRequests},
		{"attempts", loop.run, bodyRequests},
		{"models", answerModelEndpoint, modelRequests},
	}
}

// authenticateKey resolves the client's key before any body is read, so an
// unauthenticated client never makes the gateway buffer a request body.
func authenticateKey(_ context.Context, rq *request) *apiError {
	id, err := auth.Authenticate(rq.snapshot, rq.r.Header.Get("Authorization"), rq.r.Header.Get("X-Api-Key"), rq.start)
	if err != nil {
		rq.authFailure = err.Code
		rq.keyID = err.KeyID
		return authError(err)
	}
	rq.identity = id
	rq.keyID = id.KeyID
	return nil
}

// authorizeModel applies the one model-access check (auth.Identity.AuthorizeModel) to
// the model a request names, in its body or its path: every endpoint but the model
// list names one. Later stages may assume the model exists in the snapshot.
func authorizeModel(_ context.Context, rq *request) *apiError {
	if rq.endpoint == endpointListModels {
		return nil
	}
	if err := rq.identity.AuthorizeModel(rq.model); err != nil {
		return authError(err)
	}
	rq.modelAllowed = true
	return nil
}

// findServingDeployments finds the deployments of a body request's model that serve
// its endpoint. A model none of whose backends serves the endpoint can never answer:
// refused here, before limits spend anything on it (docs/specs/GATEWAY.md, Providers →
// Endpoint support).
func findServingDeployments(_ context.Context, rq *request) *apiError {
	if rq.serving = servingDeployments(rq.snapshot.Models[rq.model], rq.endpoint.api); rq.serving == nil {
		return errEndpointNotServed(rq.endpoint)
	}
	return nil
}
