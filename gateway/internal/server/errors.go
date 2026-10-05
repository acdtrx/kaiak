package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"kaiak/internal/auth"
	"kaiak/internal/clip"
	"kaiak/internal/provider"
)

// OpenAI error types used by the gateway.
const (
	typeInvalidRequest = "invalid_request_error"
	typeServer         = "server_error"
)

// apiError is a client-facing failure, written in the OpenAI error shape
// (docs/specs/GATEWAY.md, Error responses).
type apiError struct {
	status  int
	errType string
	code    string
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

// writeError writes e as the whole response.
func writeError(w http.ResponseWriter, e *apiError) {
	var body errorBody
	body.Error.Message = e.message
	body.Error.Type = e.errType
	body.Error.Code = e.code
	if e.param != "" {
		body.Error.Param = &e.param
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
			code: "model_not_found", message: e.Message}
	case auth.CodeMissingKey:
		return &apiError{status: http.StatusUnauthorized, errType: typeInvalidRequest,
			code: "missing_api_key", message: e.Message}
	default:
		return &apiError{status: http.StatusUnauthorized, errType: typeInvalidRequest,
			code: "invalid_api_key", message: e.Message}
	}
}

func errUnknownURL(r *http.Request) *apiError {
	return &apiError{status: http.StatusNotFound, errType: typeInvalidRequest, code: "unknown_url",
		message: fmt.Sprintf("Invalid URL (%s %s)", clip.String(r.Method), clip.String(r.URL.Path))}
}

func errMethodNotAllowed(r *http.Request) *apiError {
	return &apiError{status: http.StatusMethodNotAllowed, errType: typeInvalidRequest,
		code: "method_not_allowed", message: fmt.Sprintf("Method %s is not allowed for %s", clip.String(r.Method), clip.String(r.URL.Path))}
}

func errBodyTooLarge(limit int64) *apiError {
	return &apiError{status: http.StatusRequestEntityTooLarge, errType: typeInvalidRequest,
		code: "request_too_large", message: fmt.Sprintf("The request body exceeds the limit of %d bytes.", limit)}
}

func errInvalidJSON() *apiError {
	return &apiError{status: http.StatusBadRequest, errType: typeInvalidRequest, code: "invalid_json",
		message: "The request body is not a JSON object."}
}

// errDuplicateMember refuses a body naming a member twice in the top-level object (or
// in stream_options, the owned object the provider edits); param names the member,
// clipped: the client chose it.
func errDuplicateMember(param string) *apiError {
	param = clip.String(param)
	return &apiError{status: http.StatusBadRequest, errType: typeInvalidRequest, code: "duplicate_member",
		param: param, message: fmt.Sprintf("'%s' appears more than once in the request body.", param)}
}

func errInvalidType(param, want string) *apiError {
	return &apiError{status: http.StatusBadRequest, errType: typeInvalidRequest, code: "invalid_type",
		param: param, message: fmt.Sprintf("Invalid type for '%s': expected %s.", param, want)}
}

// errNTooLarge refuses a request asking for more sequences per prompt than
// global.max_n allows; param is n or best_of.
func errNTooLarge(param string, maxN int64) *apiError {
	return &apiError{status: http.StatusBadRequest, errType: typeInvalidRequest, code: "n_too_large", param: param,
		message: fmt.Sprintf("'%s' is above the gateway's maximum of %d sequences per prompt.", param, maxN)}
}

// errTooManySequences refuses a chat or completions request asking the backend to
// generate more sequences than global.max_sequences_per_request allows; param is the
// prompt list, or n or best_of.
func errTooManySequences(param string, sequences, limit int64) *apiError {
	return &apiError{status: http.StatusBadRequest, errType: typeInvalidRequest, code: "invalid_value", param: param,
		message: fmt.Sprintf("The request asks for %d generated sequences (n or best_of per prompt, times the prompts); the gateway's maximum is %d per request.",
			sequences, limit)}
}

// errTooManyInputs refuses an embeddings request carrying more inputs than
// global.max_embedding_inputs allows.
func errTooManyInputs(inputs, limit int64) *apiError {
	return &apiError{status: http.StatusBadRequest, errType: typeInvalidRequest, code: "invalid_value", param: "input",
		message: fmt.Sprintf("'input' holds %d inputs; the gateway's maximum is %d per request.", inputs, limit)}
}

// errOutputLimitTooLarge refuses an output limit (param: max_tokens or
// max_completion_tokens) above the model's context length, in OpenAI's code and
// wording for the same refusal.
func errOutputLimitTooLarge(param string, value, contextLength int64) *apiError {
	return &apiError{status: http.StatusBadRequest, errType: typeInvalidRequest, code: "invalid_value", param: param,
		message: fmt.Sprintf("%s is too large: %d. This model supports at most %d tokens (its context length), whereas you provided %d.",
			param, value, contextLength, value)}
}

// errOutputLimitNegative refuses an output limit (param: max_tokens or
// max_completion_tokens) below 0, in OpenAI's wording for an integer below its
// minimum.
func errOutputLimitNegative(param string, value int64) *apiError {
	return &apiError{status: http.StatusBadRequest, errType: typeInvalidRequest, code: "invalid_value", param: param,
		message: fmt.Sprintf("Invalid '%s': integer below minimum value. Expected a value >= 0, but got %d instead.", param, value)}
}

func errMissingModel() *apiError {
	return &apiError{status: http.StatusBadRequest, errType: typeInvalidRequest,
		code: "missing_required_parameter", param: "model", message: "Missing required parameter: 'model'."}
}

func errReadBody() *apiError {
	return &apiError{status: http.StatusBadRequest, errType: typeInvalidRequest, code: "invalid_body",
		message: "The request body could not be read."}
}

// statusClientClosed is the status the request metric records when the client left
// before any answer was written (nginx's convention). The client never receives it,
// and the request line carries no status for it.
const statusClientClosed = 499

// codeClientClosed is the error code of a client that left before any answer.
const codeClientClosed = "client_closed"

func errClientClosed() *apiError {
	return &apiError{status: statusClientClosed, errType: typeInvalidRequest, code: codeClientClosed,
		message: "The client closed the request."}
}

// errUpstream answers a failure to get a response from the backend. Messages never
// name the backend: its address is not the client's business.
func errUpstream(code provider.Code) *apiError {
	switch code {
	case provider.CodeTimeout, provider.CodeResponseTimeout:
		return &apiError{status: http.StatusGatewayTimeout, errType: typeServer, code: string(provider.CodeTimeout),
			message: "The model backend did not respond in time."}
	case provider.CodeAuthFailed:
		return &apiError{status: http.StatusBadGateway, errType: typeServer, code: string(code),
			message: "The model backend refused the gateway's credentials."}
	case provider.CodeModelMissing:
		return &apiError{status: http.StatusBadGateway, errType: typeServer, code: string(code),
			message: "The model backend does not serve the model."}
	case provider.CodePathMissing:
		return &apiError{status: http.StatusBadGateway, errType: typeServer, code: string(code),
			message: "The model backend's address is misconfigured."}
	}
	return &apiError{status: http.StatusBadGateway, errType: typeServer, code: string(provider.CodeUnavailable),
		message: "The model backend could not be reached."}
}

// errUpstreamFault answers a backend 5xx: the backend's status, the gateway's own
// message (the backend's text is logged, not relayed).
func errUpstreamFault(status int) *apiError {
	return &apiError{status: status, errType: typeServer, code: "upstream_error",
		message: "The model backend failed to process the request."}
}

func errInternal() *apiError {
	return &apiError{status: http.StatusInternalServerError, errType: typeServer, code: "internal_error",
		message: "The gateway failed to process the request."}
}

// errQueueFull answers a request that found no free slot on its model's backends and
// no room in the model's queue (or a queue of size 0). Platform-side — capacity, not
// the caller's rate — hence server_error; the caller adds Retry-After: 1, as a slot
// may free any moment.
func errQueueFull() *apiError {
	return &apiError{status: http.StatusTooManyRequests, errType: typeServer, code: "queue_full",
		message: "The model's backends are at capacity and its queue is full; retry the request."}
}

// errConcurrencyLimited answers a request of a key that already has limit requests
// in flight on this gateway (global.max_concurrent_requests_per_key). The caller's
// own doing, like a rate limit: OpenAI's 429 with type "requests"; the caller adds
// Retry-After: 1, as a slot frees when any of the key's requests ends.
func errConcurrencyLimited(limit int64) *apiError {
	return &apiError{status: http.StatusTooManyRequests, errType: "requests", code: "concurrency_limit_exceeded",
		message: fmt.Sprintf("Concurrency limit reached: this key has %d requests in flight, the most allowed per gateway; retry once one of them completes.", limit)}
}

// errServerBusy answers a request that found the gateway's body budget spent: the
// bodies of the requests in flight fill the memory set aside for them. The caller
// adds Retry-After: 1, as bodies are released all the time.
func errServerBusy() *apiError {
	return &apiError{status: http.StatusServiceUnavailable, errType: typeServer, code: "server_busy",
		message: "The gateway is holding as many request bodies as its memory allows; retry the request."}
}

// errNoHealthyDeployment answers a request for a model whose every deployment has
// its circuit open: the backends are failing, the gateway is probing them.
func errNoHealthyDeployment() *apiError {
	return &apiError{status: http.StatusServiceUnavailable, errType: typeServer, code: "no_healthy_deployment",
		message: "No backend serving the model is healthy right now; retry the request later."}
}

// errQueueTimeout answers a request that waited its model's queue timeout without
// getting a slot. No Retry-After: the gateway has no estimate better than the wait
// that just ran out.
func errQueueTimeout(timeout time.Duration) *apiError {
	return &apiError{status: http.StatusTooManyRequests, errType: typeServer, code: "queue_timeout",
		message: fmt.Sprintf("The model's backends stayed at capacity for the queue timeout of %s; retry the request.", timeout)}
}
