// Package provider is the only code that talks to model backends. A provider takes a
// routed client request, sends it upstream and hands back the response as a sequence
// of events in the client's wire format, which the pipeline relays while observers
// (accounting) read the same events.
package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"sync"
	"time"

	"kaiak/internal/config"
)

// Provider sends one client request to one backend.
//
// The interface must fit a translating provider (Bedrock: another request shape,
// binary stream framing, request signing) without pipeline changes. It does so by
// keeping every backend-specific concern behind Send and Response: the pipeline hands
// over the client's request as received plus the routing decision, and gets back
// status, headers and events already in the client's format (the endpoint's: OpenAI,
// Messages or Responses JSON and SSE). A translating provider converts on both sides;
// today's modules pass every request through in its own format with the owned edits
// only (docs/specs/GATEWAY.md, Client API → Client APIs).
type Provider interface {
	// Send sends req upstream and waits for the first event of the response (or for
	// the whole response to end, when it has no body), so a failure before anything
	// reaches the client is still an error the pipeline can answer with — except an
	// error answer (4xx, 5xx) whose body breaks before its first byte: its status
	// came with the headers, so it is returned as the response, the break its first
	// Next (docs/specs/GATEWAY.md, Providers: upstream failures). Errors are
	// *Error, or the context's error when ctx ended first. Cancelling ctx cancels the
	// upstream request at any point, including while the caller reads the Response.
	Send(ctx context.Context, req *Request) (Response, error)
}

// Endpoint is the client API operation a request calls.
type Endpoint int

const (
	ChatCompletions Endpoint = iota
	Completions
	Embeddings
	Messages
	MessagesCountTokens
	Responses
	ResponsesInputTokens
)

// Format is a client API format: the request and response shapes an endpoint speaks
// (docs/specs/GATEWAY.md, Client API → Client APIs).
type Format int

const (
	// FormatOpenAI: chat completions, completions and embeddings.
	FormatOpenAI Format = iota
	// FormatMessages: Anthropic's Messages API and its token counting.
	FormatMessages
	// FormatResponses: OpenAI's Responses API and its token counting.
	FormatResponses
)

// endpoints holds each Endpoint's path below the API's version prefix
// (docs/specs/GATEWAY.md, Base URLs) and the format it speaks.
var endpoints = [...]struct {
	path   string
	format Format
}{
	ChatCompletions:      {"chat/completions", FormatOpenAI},
	Completions:          {"completions", FormatOpenAI},
	Embeddings:           {"embeddings", FormatOpenAI},
	Messages:             {"messages", FormatMessages},
	MessagesCountTokens:  {"messages/count_tokens", FormatMessages},
	Responses:            {"responses", FormatResponses},
	ResponsesInputTokens: {"responses/input_tokens", FormatResponses},
}

// Path is the endpoint's path below the API's version prefix: a backend URL ends with
// it, and the client's route is /v1/ and it.
func (e Endpoint) Path() string { return endpoints[e].path }

// Format is the format the endpoint speaks.
func (e Endpoint) Format() Format { return endpoints[e].format }

// Request is one client request as the pipeline hands it to a provider: already
// authenticated, parsed and routed.
type Request struct {
	Endpoint Endpoint
	// Deployment is where routing sent the request: the backend and the model name
	// on it.
	Deployment config.Deployment
	// Body is the client's request body exactly as received (the endpoint's format, a
	// JSON object).
	Body []byte
	// Stream and IncludeUsage are the client's stream and
	// stream_options.include_usage.
	Stream       bool
	IncludeUsage bool
	// RequestID is forwarded to the backend as x-request-id.
	RequestID string
	// PublicModel is the model name the client asked for. The response carries it in
	// place of the backend's model name.
	PublicModel string
	// Sent, when set, is called once the request has been written to the backend in
	// full — from the transport's goroutine, so it must be safe for concurrent use.
	// From then on the backend has the prompt and may process and bill it, whatever
	// happens before its answer (docs/specs/GATEWAY.md, Accounting).
	Sent func()
	// Params are top-level request parameters the pipeline sets (the output limit), in
	// order. Each value is encoded JSON; it replaces every
	// occurrence of its key or, when the key is absent, is added. Keys never repeat
	// and never name a member the provider edits itself (model, stream_options).
	Params []Param
}

// Param is one top-level request parameter set by the pipeline.
type Param struct {
	Key   string
	Value []byte
}

// Response is a backend's answer, in the client's format. The caller reads events
// until Next returns an error and always calls Close.
type Response interface {
	// Status is the HTTP status the client receives.
	Status() int
	// Header holds the headers the client receives from the backend's response
	// (docs/specs/GATEWAY.md, Providers: headers).
	Header() http.Header
	// Stream reports whether the body is a server-sent event stream, whose events the
	// caller must flush to the client one by one.
	Stream() bool
	// Next returns the next event; io.EOF after the last one. Any other error means
	// the response broke off (backend failure, or ctx cancelled): what was relayed so
	// far is all there is.
	Next() (Event, error)
	// Close ends the response and cancels the upstream request if it is still
	// running. It may be called more than once.
	Close()
}

// Event is one piece of a response body.
type Event struct {
	// Data is written to the client as is: one whole server-sent event, blank line
	// included, for a stream; a piece of the body otherwise. It carries the client's
	// model name (Request.PublicModel).
	Data []byte
	// Payload is a stream event's data field (a JSON chunk, or "[DONE]") as the
	// backend sent it; nil for body pieces and for blocks carrying no data (comments,
	// keep-alives).
	Payload []byte
	// Hidden marks an event the gateway asked the backend for that the client did
	// not request (the usage chunk): observers see it, the client must not.
	Hidden bool
}

// Code classifies a failure to get a response from the backend.
type Code string

const (
	// CodeUnavailable: no response — the connection failed (refused, DNS, connect
	// timeout, TLS), or broke before the first event of a response that was not an
	// error answer.
	CodeUnavailable Code = "upstream_unavailable"
	// CodeTimeout: a streaming request got no first event within the backend's
	// first-event timeout.
	CodeTimeout Code = "upstream_timeout"
	// CodeResponseTimeout: a non-streaming request got no response within the
	// backend's response timeout. Unlike CodeTimeout the backend is taken to be
	// working (a long generation), so it is neither retried nor a circuit failure;
	// the client's answer is the same upstream_timeout.
	CodeResponseTimeout Code = "upstream_response_timeout"
	// CodeAuthFailed: the backend refused the gateway's own credential (401 or 403).
	CodeAuthFailed Code = "upstream_auth_failed"
	// CodeModelMissing: the backend answered 404 saying the deployment's model does
	// not exist there — the host serves another model (docs/specs/GATEWAY.md,
	// Providers: wrong model on a host).
	CodeModelMissing Code = "upstream_model_missing"
	// CodePathMissing: the backend answered 404 the way its server answers a path it
	// does not have, on one of its type's core endpoints — the backend's base_url is
	// wrong (docs/specs/GATEWAY.md, Providers: wrong path to a host).
	CodePathMissing Code = "upstream_path_missing"
	// CodeEndpointMissing: the same answer on an endpoint beyond the type's core ones
	// — the server's version predates the endpoint, which its other endpoints do not
	// (docs/specs/GATEWAY.md, Providers: an endpoint missing from a server).
	CodeEndpointMissing Code = "upstream_endpoint_missing"
	// CodeErrorEvent: a successful stream's first event was an error event (a
	// Messages error event, a Responses error or response.failed) — the backend gave
	// up before anything reached the client; Error.Event says what it named
	// (docs/specs/GATEWAY.md, Providers: error events).
	CodeErrorEvent Code = "upstream_error_event"
)

// Error is a failure before any part of the response reached the client. Its message
// is for logs: it may name backend addresses, never credentials.
type Error struct {
	Code Code
	Err  error
	// Event is the error event of a CodeErrorEvent.
	Event *ErrorEvent
}

// ErrorEventKind is what an error event in a successful stream says about its
// cause, read from its error code or type as the matching HTTP status would be
// (docs/specs/GATEWAY.md, Providers: error events).
type ErrorEventKind int

const (
	// ErrorEventFailure: the backend failing (a server error, a code unknown or
	// missing) — as a 5xx.
	ErrorEventFailure ErrorEventKind = iota + 1
	// ErrorEventBusy: the backend overloaded or rate-limiting — as a 429 or 529.
	ErrorEventBusy
	// ErrorEventCaller: the request's own fault — an invalid prompt or image, a
	// policy — as a 4xx.
	ErrorEventCaller
)

// StatusOverloaded is Anthropic's status for an overloaded API (overloaded_error).
const StatusOverloaded = 529

// BusyStatus reports whether a backend status says busy rather than broken, as
// ErrorEventBusy does of an error event: a 429, or Anthropic's 529 (docs/specs/GATEWAY.md,
// Routing and reliability: outcome classes).
func BusyStatus(status int) bool {
	return status == http.StatusTooManyRequests || status == StatusOverloaded
}

// ErrorEvent is an error event a successful stream carried: its kind, and its code
// as the backend sent it (raw JSON, for the caller to take only as an identifier).
type ErrorEvent struct {
	Kind ErrorEventKind
	Code json.RawMessage
}

// ErrorEventEnd is the error a Response's Next returns once a stream that carried an
// error event after its first event has ended: an ErrIncomplete, carrying the event.
type ErrorEventEnd struct {
	Event   *ErrorEvent
	backend string
}

func (e *ErrorEventEnd) Error() string {
	return fmt.Sprintf("backend %s: the stream carried an error event: %v", e.backend, ErrIncomplete)
}
func (e *ErrorEventEnd) Unwrap() error { return ErrIncomplete }

func (e *Error) Error() string { return fmt.Sprintf("%s: %v", e.Code, e.Err) }
func (e *Error) Unwrap() error { return e.Err }

// RefusalError is a provider's refusal of a request as the caller's mistake, made
// before anything was sent (docs/specs/GATEWAY.md, Providers: a provider's refusal):
// the pipeline answers it as a 400 with Code and Param, never retries it and never
// counts it toward the circuit. Message is safe to show the client: it names
// parameters, never values the client wrote.
type RefusalError struct {
	Code    string
	Param   string
	Message string
}

func (e *RefusalError) Error() string { return e.Code + ": " + e.Message }

// Registry hands out the provider for a backend and owns the backends' HTTP
// transports, so connections are pooled across requests and config reloads.
type Registry struct {
	lookupEnv func(string) (string, bool)

	mu         sync.Mutex
	transports map[string]*backendTransport
}

// backendTransport is one backend's connection pool. The connect timeout is a
// property of the dialer, so a reload that changes it gets a new pool.
type backendTransport struct {
	connectTimeout time.Duration
	client         *http.Client
}

// NewRegistry returns a registry reading backend credentials through lookupEnv (the
// variable a backend's api_key_env names).
func NewRegistry(lookupEnv func(string) (string, bool)) *Registry {
	return &Registry{lookupEnv: lookupEnv, transports: make(map[string]*backendTransport)}
}

// backendModule is one backend type's provider: what that type does differently
// lives in its module, which builds on the wire core (send.go) for what the types
// share.
type backendModule interface {
	Provider
	// probe checks whether the backend answers (Registry.Probe).
	probe(ctx context.Context) (serves func(model string) bool, err error)
}

// backendKind is one backend type: the endpoints its server serves, its GenAI
// provider name, and how its module is built over a backend's connection pool and
// credential.
type backendKind struct {
	// serves are the endpoints the type's server serves (docs/specs/GATEWAY.md,
	// Providers → Endpoint support).
	serves []Endpoint
	// providerName is gen_ai.provider.name for the type: the GenAI convention's
	// well-known value where one fits, else "" — the self-hosted types have none, and
	// kaiak.backend.type names every type. Claude in Foundry is Anthropic's service and
	// API on Azure, and no Azure value names it, so it is anthropic too.
	providerName string
	build        func(b *config.Backend, client *http.Client, credential string) backendModule
}

// kinds holds every backend type the config schema admits: the one place a type is
// named.
var kinds = map[config.BackendType]backendKind{
	config.BackendOpenAI:           {serves: openAIEndpoints, providerName: "openai", build: newOpenAI},
	config.BackendAzureOpenAI:      {serves: azureOpenAIEndpoints, providerName: "azure.ai.openai", build: newAzureOpenAI},
	config.BackendVLLM:             {serves: vLLMEndpoints, build: newVLLM},
	config.BackendLlamaServer:      {serves: llamaServerEndpoints, build: newLlamaServer},
	config.BackendOpenAICompatible: {serves: openAICompatibleEndpoints, build: newOpenAICompatible},
	config.BackendAnthropic:        {serves: anthropicEndpoints, providerName: "anthropic", build: newAnthropic},
	config.BackendAzureAnthropic:   {serves: azureAnthropicEndpoints, providerName: "anthropic", build: newAzureAnthropic},
}

// kindOf is type t's kind. The config schema admits only the types in kinds, so
// another is a gateway fault.
func kindOf(t config.BackendType) backendKind {
	k, ok := kinds[t]
	if !ok {
		panic(fmt.Sprintf("provider: no module for backend type %q", t))
	}
	return k
}

// Serves reports whether backends of type t serve endpoint e: routing sends a
// request only to deployments whose backend does (docs/specs/GATEWAY.md, Providers →
// Endpoint support).
func Serves(t config.BackendType, e Endpoint) bool {
	return slices.Contains(kindOf(t).serves, e)
}

// ProviderName is gen_ai.provider.name for backends of type t; "" for a type with no
// well-known value.
func ProviderName(t config.BackendType) string {
	return kindOf(t).providerName
}

// For returns the provider for backend b: its type's module.
func (r *Registry) For(b *config.Backend) Provider {
	return r.module(b)
}

// module returns backend b's module, over b's connection pool and with its credential.
func (r *Registry) module(b *config.Backend) backendModule {
	return kindOf(b.Type).build(b, r.client(b), r.credential(b))
}

// credential is the gateway's credential for b; "" when it has none. A reserved
// KAIAK_ or OTEL_ name gives none either: the config schema refuses one, and this is
// the last point before its value would leave for the backend's URL, which the config
// author chooses.
func (r *Registry) credential(b *config.Backend) string {
	if b.APIKeyEnv == "" || config.IsReservedEnvName(b.APIKeyEnv) {
		return ""
	}
	// Presence was checked when the config loaded (api-key-env-unset).
	credential, _ := r.lookupEnv(b.APIKeyEnv)
	return credential
}

// Probe checks whether backend b answers (docs/specs/GATEWAY.md, Routing and
// reliability): its module asks for its models list with the gateway's credential,
// over b's connection pool, bounded by b's connect timeout plus probeReadTimeout. A
// 2xx answer succeeds; serves reports whether a backend-side model name is served
// there, as far as the backend's list says, and is nil when the backend cannot tell
// (its type has no list naming what requests carry): every name counts as served. The
// error may name the backend's address, never the credential nor text the backend
// sent.
func (r *Registry) Probe(ctx context.Context, b *config.Backend) (serves func(model string) bool, err error) {
	return r.module(b).probe(ctx)
}

// client returns b's HTTP client, creating its pool on first use or when the connect
// timeout changed. A replaced pool's idle connections are closed; its in-flight
// requests finish on their connections.
func (r *Registry) client(b *config.Backend) *http.Client {
	r.mu.Lock()
	defer r.mu.Unlock()
	if t, ok := r.transports[b.ID]; ok {
		if t.connectTimeout == b.ConnectTimeout {
			return t.client
		}
		t.client.CloseIdleConnections()
	}
	t := &backendTransport{connectTimeout: b.ConnectTimeout, client: newClient(b.ConnectTimeout)}
	r.transports[b.ID] = t
	return t.client
}

// Retain drops the pools of backends not in backends (the applied config's), closing
// their idle connections; requests still running on one finish on their connections.
func (r *Registry) Retain(backends map[string]*config.Backend) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, t := range r.transports {
		if _, ok := backends[id]; !ok {
			t.client.CloseIdleConnections()
			delete(r.transports, id)
		}
	}
}

// Connection pool sizing: many concurrent streams to one backend are the normal case
// (a vLLM server batches dozens), so idle connections are kept per host well above
// net/http's default of 2, which would close and reopen connections under load.
const (
	maxIdleConnsPerHost = 256
	idleConnTimeout     = 90 * time.Second
	tcpKeepAlive        = 30 * time.Second
)

// newClient builds a backend's HTTP client. There is no overall timeout: a stream runs
// as long as the backend sends. The connect timeout bounds TCP connect and the TLS
// handshake; the first-event, response and stall timeouts are applied per request by
// Send and the Response. Compression is
// off so SSE bytes flow through undecoded and unbuffered. Redirects are not followed:
// a backend that answers with one is relayed as is.
func newClient(connectTimeout time.Duration) *http.Client {
	dialer := &net.Dialer{Timeout: connectTimeout, KeepAlive: tcpKeepAlive}
	return &http.Client{
		Transport: &http.Transport{
			Proxy:               http.ProxyFromEnvironment,
			DialContext:         dialer.DialContext,
			TLSHandshakeTimeout: connectTimeout,
			ForceAttemptHTTP2:   true,
			MaxIdleConnsPerHost: maxIdleConnsPerHost,
			IdleConnTimeout:     idleConnTimeout,
			DisableCompression:  true,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// Cancellation causes of the upstream request's timers.
var (
	errFirstEventTimeout = errors.New("no first event within the first-event timeout")
	errResponseTimeout   = errors.New("no complete response within the response timeout")
	errStalled           = errors.New("no event within the stall timeout")
)

// Errors a Response's Next returns, wrapped, when the response broke off after the
// first event for a reason of the gateway's own reading (docs/specs/GATEWAY.md,
// Providers: upstream failures and complete responses). Any other error is the
// connection's, named by its class (netfail), or the event stream's (sse).
var (
	// ErrStalled: a stream was silent for the backend's stall timeout.
	ErrStalled = errors.New("stream stalled")
	// ErrResponseTimeout: a non-streaming response did not end within the backend's
	// response timeout.
	ErrResponseTimeout = errors.New("response timeout")
	// ErrIncomplete: a successful response ended before it was complete — a stream
	// short of its format's end (an OpenAI stream without [DONE] or every choice's
	// finish_reason, a Messages stream without message_stop) or broken off by an error
	// event, a JSON body cut short.
	ErrIncomplete = errors.New("response ended incomplete")
)
