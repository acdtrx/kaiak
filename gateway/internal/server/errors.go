package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"kaiak/internal/auth"
	"kaiak/internal/clip"
	"kaiak/internal/metrics"
	"kaiak/internal/provider"
)

// OpenAI error types used by the gateway.
const (
	typeInvalidRequest = "invalid_request_error"
	typeServer         = "server_error"
)

// apiError is a client-facing failure, written in the error shape of the client API
// the request speaks (errorShape; docs/specs/GATEWAY.md, Client API → Errors).
type apiError struct {
	status  int
	errType string
	code    string
	// class is the error class the error metrics count the answer under
	// (docs/specs/GATEWAY.md, Observability: error classes).
	class   metrics.ErrorClass
	message string
	// param names the request parameter at fault; "" is written as null.
	param string
}

// errorBody is the OpenAI error envelope. OpenAI always sends param, null when no
// parameter is at fault.
type errorBody struct {
	Error struct {
		Message string  `json:"message"`
		Type    string  `json:"type"`
		Param   *string `json:"param"`
		Code    string  `json:"code"`
	} `json:"error"`
}

// errorShape is the error envelope of a client API.
type errorShape int

const (
	// shapeOpenAI: {"error": {"message", "type", "param", "code"}} — the OpenAI and
	// Responses endpoints.
	shapeOpenAI errorShape = iota
	// shapeAnthropic: {"type": "error", "error": {"type", "message", "code"}} — the
	// Messages endpoints and the Anthropic-shaped model list. Its type follows the
	// status, as Anthropic's do; code is kaiak's, a member Anthropic's SDKs ignore.
	shapeAnthropic
)

// anthropicErrorBody is Anthropic's error envelope, with kaiak's code beside its type.
type anthropicErrorBody struct {
	Type  string `json:"type"`
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
		Code    string `json:"code"`
	} `json:"error"`
}

// anthropicErrorType is the Anthropic error type of an answer with status
// (docs/specs/GATEWAY.md, Client API → Errors).
func anthropicErrorType(status int) string {
	switch {
	case status == http.StatusUnauthorized:
		return "authentication_error"
	case status == http.StatusForbidden:
		return "permission_error"
	case status == http.StatusNotFound:
		return "not_found_error"
	case status == http.StatusRequestEntityTooLarge:
		return "request_too_large"
	case status == http.StatusTooManyRequests:
		return "rate_limit_error"
	case status == http.StatusServiceUnavailable || status == provider.StatusOverloaded:
		return "overloaded_error"
	case status >= 500:
		return "api_error"
	}
	return typeInvalidRequest
}

// errorShape is the error shape of a request r to e: Anthropic's on the Messages
// endpoints, and on the model list and entry when the request carries an
// anthropic-version header (the Anthropic-shaped model list); OpenAI's everywhere
// else (answersAnthropic).
func (e *endpoint) errorShape(r *http.Request) errorShape {
	if e.answersAnthropic(r) {
		return shapeAnthropic
	}
	return shapeOpenAI
}

// errorShape is the error shape of the request's answers.
func (rq *request) errorShape() errorShape {
	return rq.endpoint.errorShape(rq.r)
}

// writeError writes e as the whole response, in shape.
func writeError(w http.ResponseWriter, e *apiError, shape errorShape) {
	var body any
	if shape == shapeAnthropic {
		var b anthropicErrorBody
		b.Type = "error"
		b.Error.Type = anthropicErrorType(e.status)
		b.Error.Message = e.message
		b.Error.Code = e.code
		body = b
	} else {
		var b errorBody
		b.Error.Message = e.message
		b.Error.Type = e.errType
		b.Error.Code = e.code
		if e.param != "" {
			b.Error.Param = &e.param
		}
		body = b
	}
	// Messages are plain text for API clients: < > & stay as written, not \u003c.
	var data bytes.Buffer
	enc := json.NewEncoder(&data)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(body) // strings only: encoding cannot fail
	h := w.Header()
	h.Set("Content-Type", "application/json")
	if e.status == http.StatusUnauthorized {
		h.Set("WWW-Authenticate", "Bearer")
	}
	w.WriteHeader(e.status)
	_, _ = w.Write(data.Bytes()) // a failed write means the client left; nothing to do
}

// authError maps an auth refusal to its client answer. Every key failure is a 401
// with invalid_api_key except a missing key; unknown and not-allowed models share one
// 404.
func authError(e *auth.Error) *apiError {
	switch e.Code {
	case auth.CodeModelNotFound:
		return &apiError{status: http.StatusNotFound, errType: typeInvalidRequest,
			code: "model_not_found", class: metrics.ErrorNotFound, message: e.Message}
	case auth.CodeMissingKey:
		return &apiError{status: http.StatusUnauthorized, errType: typeInvalidRequest,
			code: "missing_api_key", class: metrics.ErrorAuth, message: e.Message}
	default:
		return &apiError{status: http.StatusUnauthorized, errType: typeInvalidRequest,
			code: "invalid_api_key", class: metrics.ErrorAuth, message: e.Message}
	}
}

func errUnknownURL(r *http.Request) *apiError {
	return &apiError{status: http.StatusNotFound, errType: typeInvalidRequest, code: "unknown_url", class: metrics.ErrorNotFound,
		message: fmt.Sprintf("Invalid URL (%s %s)", clip.String(r.Method), clip.String(r.URL.Path))}
}

func errMethodNotAllowed(r *http.Request) *apiError {
	return &apiError{status: http.StatusMethodNotAllowed, errType: typeInvalidRequest,
		code: "method_not_allowed", class: metrics.ErrorInvalidRequest, message: fmt.Sprintf("Method %s is not allowed for %s", clip.String(r.Method), clip.String(r.URL.Path))}
}

func errBodyTooLarge(limit int64) *apiError {
	return &apiError{status: http.StatusRequestEntityTooLarge, errType: typeInvalidRequest,
		code: "request_too_large", class: metrics.ErrorInvalidRequest, message: fmt.Sprintf("The request body exceeds the limit of %d bytes.", limit)}
}

func errInvalidJSON() *apiError {
	return &apiError{status: http.StatusBadRequest, errType: typeInvalidRequest, code: "invalid_json", class: metrics.ErrorInvalidRequest,
		message: "The request body is not a JSON object."}
}

// errDuplicateMember refuses a body naming a member twice in the top-level object (or
// in stream_options, the owned object the provider edits); param names the member,
// clipped: the client chose it.
func errDuplicateMember(param string) *apiError {
	param = clip.String(param)
	return &apiError{status: http.StatusBadRequest, errType: typeInvalidRequest, code: "duplicate_member", class: metrics.ErrorInvalidRequest,
		param: param, message: fmt.Sprintf("'%s' appears more than once in the request body.", param)}
}

func errInvalidType(param, want string) *apiError {
	return &apiError{status: http.StatusBadRequest, errType: typeInvalidRequest, code: "invalid_type", class: metrics.ErrorInvalidRequest,
		param: param, message: fmt.Sprintf("Invalid type for '%s': expected %s.", param, want)}
}

// errNTooLarge refuses a request asking for more sequences per prompt than
// global.max_n allows; param is n or best_of.
func errNTooLarge(param string, maxN int64) *apiError {
	return &apiError{status: http.StatusBadRequest, errType: typeInvalidRequest, code: "n_too_large", class: metrics.ErrorInvalidRequest, param: param,
		message: fmt.Sprintf("'%s' is above the gateway's maximum of %d sequences per prompt.", param, maxN)}
}

// errTooManySequences refuses a chat or completions request asking the backend to
// generate more sequences than global.max_sequences_per_request allows; param is the
// prompt list, or n or best_of.
func errTooManySequences(param string, sequences, limit int64) *apiError {
	return &apiError{status: http.StatusBadRequest, errType: typeInvalidRequest, code: "invalid_value", class: metrics.ErrorInvalidRequest, param: param,
		message: fmt.Sprintf("The request asks for %d generated sequences (n or best_of per prompt, times the prompts); the gateway's maximum is %d per request.",
			sequences, limit)}
}

// errTooManyInputs refuses an embeddings request carrying more inputs than
// global.max_embedding_inputs allows.
func errTooManyInputs(inputs, limit int64) *apiError {
	return &apiError{status: http.StatusBadRequest, errType: typeInvalidRequest, code: "invalid_value", class: metrics.ErrorInvalidRequest, param: "input",
		message: fmt.Sprintf("'input' holds %d inputs; the gateway's maximum is %d per request.", inputs, limit)}
}

// errTooManyDocuments refuses a rerank request carrying more documents than
// global.max_rerank_documents allows.
func errTooManyDocuments(documents, limit int64) *apiError {
	return &apiError{status: http.StatusBadRequest, errType: typeInvalidRequest, code: "invalid_value", class: metrics.ErrorInvalidRequest, param: "documents",
		message: fmt.Sprintf("'documents' holds %d documents; the gateway's maximum is %d per request.", documents, limit)}
}

// errOutputLimitTooLarge refuses an output limit (param: max_tokens or
// max_completion_tokens) above the model's context length, in OpenAI's code and
// wording for the same refusal.
func errOutputLimitTooLarge(param string, value, contextLength int64) *apiError {
	return &apiError{status: http.StatusBadRequest, errType: typeInvalidRequest, code: "invalid_value", class: metrics.ErrorInvalidRequest, param: param,
		message: fmt.Sprintf("%s is too large: %d. This model supports at most %d tokens (its context length), whereas you provided %d.",
			param, value, contextLength, value)}
}

// errOutputLimitBelowThinking refuses a Messages request whose output limit the
// gateway would set to the model's ceiling or default (which), n, at or below the
// request's thinking budget.
func errOutputLimitBelowThinking(param string, n int64, which string, budget int64) *apiError {
	return &apiError{status: http.StatusBadRequest, errType: typeInvalidRequest, code: "invalid_value", class: metrics.ErrorInvalidRequest, param: param,
		message: fmt.Sprintf("%s would be set to %d, this model's output-limit %s, which is not above thinking.budget_tokens (%d): "+
			"send a smaller thinking budget, or ask the operator to raise the model's output-limit %s.", param, n, which, budget, which)}
}

// errOutputLimitNegative refuses an output limit (param: max_tokens or
// max_completion_tokens) below 0, in OpenAI's wording for an integer below its
// minimum.
func errOutputLimitNegative(param string, value int64) *apiError {
	return &apiError{status: http.StatusBadRequest, errType: typeInvalidRequest, code: "invalid_value", class: metrics.ErrorInvalidRequest, param: param,
		message: fmt.Sprintf("Invalid '%s': integer below minimum value. Expected a value >= 0, but got %d instead.", param, value)}
}

func errMissingModel() *apiError {
	return &apiError{status: http.StatusBadRequest, errType: typeInvalidRequest,
		code: "missing_required_parameter", class: metrics.ErrorInvalidRequest, param: "model", message: "Missing required parameter: 'model'."}
}

func errReadBody() *apiError {
	return &apiError{status: http.StatusBadRequest, errType: typeInvalidRequest, code: "invalid_body", class: metrics.ErrorInvalidRequest,
		message: "The request body could not be read."}
}

// statusClientClosed is the status the request metric records when the client left
// before any answer was written (nginx's convention). The client never receives it,
// and the request line carries no status for it.
const statusClientClosed = 499

// codeClientClosed is the error code of a client that left before any answer.
const codeClientClosed = "client_closed"

func errClientClosed() *apiError {
	return &apiError{status: statusClientClosed, errType: typeInvalidRequest, code: codeClientClosed, class: metrics.ErrorClientClosed,
		message: "The client closed the request."}
}

// upstreamAnswer is the answer to a failure to get a response from the backend
// (failureRules): status, the provider's code, its error class and the gateway's
// message. Messages never name the backend: its address is not the client's business.
func upstreamAnswer(status int, code provider.Code, class metrics.ErrorClass, message string) func(*provider.Error) *apiError {
	return func(*provider.Error) *apiError {
		return &apiError{status: status, errType: typeServer, code: string(code), class: class, message: message}
	}
}

// errUpstreamFault answers a backend 5xx: the backend's status, the gateway's own
// message (the backend's text is logged, not relayed).
func errUpstreamFault(status int) *apiError {
	return &apiError{status: status, errType: typeServer, code: "upstream_error", class: metrics.ErrorUpstreamError,
		message: "The model backend failed to process the request."}
}

// errUpstreamOverloaded answers a backend busy rather than broken: Anthropic's 529
// (overloaded_error) under its status, or a stream whose first event said the backend
// was overloaded or rate-limiting, under 503. Its own code, so clients and the
// error metrics tell a busy backend from a failing one.
func errUpstreamOverloaded(status int) *apiError {
	return &apiError{status: status, errType: typeServer, code: "upstream_overloaded", class: metrics.ErrorUpstreamRateLimited,
		message: "The model backend is overloaded; retry later."}
}

// errUpstreamRefused answers a stream whose first event said the request itself was at
// fault (an invalid prompt or image, a policy): a 400 naming the backend's error code
// when it is a plain identifier (backendIdentifier), never its message.
func errUpstreamRefused(code string) *apiError {
	message := "The model backend refused the request."
	if code != "" {
		message = fmt.Sprintf("The model backend refused the request (%s).", code)
	}
	return &apiError{status: http.StatusBadRequest, errType: typeInvalidRequest, code: "upstream_refused", class: metrics.ErrorUpstreamClientError, message: message}
}

// errRefused answers a provider's refusal of the request before sending: the caller's
// mistake, a 400 naming the parameter at fault.
func errRefused(r *provider.RefusalError) *apiError {
	return &apiError{status: http.StatusBadRequest, errType: typeInvalidRequest, code: r.Code, class: metrics.ErrorInvalidRequest, param: r.Param,
		message: r.Message}
}

// errEndpointNotServed answers a request for a model none of whose deployments is on
// a backend serving the endpoint (docs/specs/GATEWAY.md, Providers → Endpoint
// support). The model passed the access check, so naming the endpoint leaks nothing.
func errEndpointNotServed(ep *endpoint) *apiError {
	return &apiError{status: http.StatusBadRequest, errType: typeInvalidRequest, code: "endpoint_not_served", class: metrics.ErrorInvalidRequest,
		message: fmt.Sprintf("The model is not served on %s: none of its backends serves this API.", ep.route())}
}

func errInternal() *apiError {
	return &apiError{status: http.StatusInternalServerError, errType: typeServer, code: "internal_error", class: metrics.ErrorInternal,
		message: "The gateway failed to process the request."}
}

// errQueueFull answers a request that found no free slot on its model's backends and
// no room in the model's queue (or a queue of size 0). Platform-side — capacity, not
// the caller's rate — hence server_error; the caller adds Retry-After: 1, as a slot
// may free any moment.
func errQueueFull() *apiError {
	return &apiError{status: http.StatusTooManyRequests, errType: typeServer, code: "queue_full", class: metrics.ErrorQueueRejected,
		message: "The model's backends are at capacity and its queue is full; retry the request."}
}

// errConcurrencyLimited answers a request of a key that already has limit requests
// in flight on this gateway (global.max_concurrent_requests_per_key). The caller's
// own doing, like a rate limit: OpenAI's 429 with type "requests"; the caller adds
// Retry-After: 1, as a slot frees when any of the key's requests ends.
func errConcurrencyLimited(limit int64) *apiError {
	return &apiError{status: http.StatusTooManyRequests, errType: "requests", code: "concurrency_limit_exceeded", class: metrics.ErrorRateLimited,
		message: fmt.Sprintf("Concurrency limit reached: this key has %d requests in flight, the most allowed per gateway; retry once one of them completes.", limit)}
}

// errServerBusy answers a request that found the gateway's body budget spent: the
// bodies of the requests in flight fill the memory set aside for them. The caller
// adds Retry-After: 1, as bodies are released all the time.
func errServerBusy() *apiError {
	return &apiError{status: http.StatusServiceUnavailable, errType: typeServer, code: "server_busy", class: metrics.ErrorServerBusy,
		message: "The gateway is holding as many request bodies as its memory allows; retry the request."}
}

// errNoHealthyDeployment answers a request for a model whose every deployment has
// its circuit open: the backends are failing, the gateway is probing them.
func errNoHealthyDeployment() *apiError {
	return &apiError{status: http.StatusServiceUnavailable, errType: typeServer, code: "no_healthy_deployment", class: metrics.ErrorNoHealthyDeployment,
		message: "No backend serving the model is healthy right now; retry the request later."}
}

// errQueueTimeout answers a request that waited its model's queue timeout without
// getting a slot. No Retry-After: the gateway has no estimate better than the wait
// that just ran out.
func errQueueTimeout(timeout time.Duration) *apiError {
	return &apiError{status: http.StatusTooManyRequests, errType: typeServer, code: "queue_timeout", class: metrics.ErrorQueueRejected,
		message: fmt.Sprintf("The model's backends stayed at capacity for the queue timeout of %s; retry the request.", timeout)}
}

// errHostedTool refuses a tool the backend would run itself (docs/specs/GATEWAY.md,
// Client API → hosted tools are refused); param names its type's position, and the
// message names the type, clipped: the client wrote it.
func errHostedTool(param, toolType string) *apiError {
	return &apiError{status: http.StatusBadRequest, errType: typeInvalidRequest, code: "hosted_tool_unsupported", class: metrics.ErrorInvalidRequest, param: param,
		message: fmt.Sprintf("'%s' is %q, a tool the backend runs itself; the gateway serves only tools the client runs.",
			param, clip.String(toolType))}
}

// errStatefulResponses refuses a Responses request relying on state kept between
// requests (docs/specs/GATEWAY.md, Client API → Responses is stateless); param names
// the field.
func errStatefulResponses(param string) *apiError {
	return &apiError{status: http.StatusBadRequest, errType: typeInvalidRequest, code: "stateful_responses_unsupported", class: metrics.ErrorInvalidRequest, param: param,
		message: fmt.Sprintf("'%s' relies on state kept between requests; the gateway serves Responses stateless: send the whole conversation in input.", param)}
}

// errStoredObjectResponses refuses a Responses request referring to an object stored
// at the backend — an item, a file (docs/specs/GATEWAY.md, Client API → Responses is
// stateless): the gateway stores nothing and has no upload endpoint, so such an ID
// can only name another application's object in the shared provider account.
func errStoredObjectResponses(param string) *apiError {
	return &apiError{status: http.StatusBadRequest, errType: typeInvalidRequest, code: "stateful_responses_unsupported", class: metrics.ErrorInvalidRequest, param: param,
		message: fmt.Sprintf("'%s' refers to an object stored at the backend; the gateway serves Responses stateless: send the content itself.", param)}
}

// errStoredObject refuses a Messages request referring to a file stored at the
// backend (docs/specs/GATEWAY.md, Client API → stored objects), for the same reason.
func errStoredObject(param string) *apiError {
	return &apiError{status: http.StatusBadRequest, errType: typeInvalidRequest, code: "stored_object_unsupported", class: metrics.ErrorInvalidRequest, param: param,
		message: fmt.Sprintf("'%s' refers to a file stored at the backend; the gateway serves no stored files: send the content itself.", param)}
}

// errHostedMember refuses a request member that hands the backend tools to run itself
// (Messages mcp_servers and container).
func errHostedMember(param string) *apiError {
	return &apiError{status: http.StatusBadRequest, errType: typeInvalidRequest, code: "hosted_tool_unsupported", class: metrics.ErrorInvalidRequest, param: param,
		message: fmt.Sprintf("'%s' asks the backend to run tools itself; the gateway serves only tools the client runs.", param)}
}
