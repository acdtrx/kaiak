// Package metrics is the gateway's metric families on a telemetry/metric registry:
// the ops metrics the request pipeline feeds, the usage metrics accounting hands
// every settled record, and the metrics of usage delivery, the control-plane
// connection, log export and the build (docs/specs/GATEWAY.md, Observability →
// Metric list).
//
// Metrics are for dashboards: cheap, approximate, reset on restart. They are never the
// source of a usage record, and usage records are never rebuilt from them
// (docs/kaiak.md, principle 7).
package metrics

import (
	"strconv"
	"time"

	"kaiak/internal/config"
	"kaiak/internal/limits"
	"kaiak/internal/routing"
	"kaiak/internal/telemetry/metric"
)

// ErrorClass groups the gateway's error codes, the kaiak.error.class of kaiak.errors
// (docs/specs/GATEWAY.md, Observability).
type ErrorClass string

const (
	ErrorAuth                ErrorClass = "auth"
	ErrorNotFound            ErrorClass = "not_found"
	ErrorInvalidRequest      ErrorClass = "invalid_request"
	ErrorRateLimited         ErrorClass = "rate_limited"
	ErrorQueueRejected       ErrorClass = "queue_rejected"
	ErrorBudgetExceeded      ErrorClass = "budget_exceeded"
	ErrorBudgetUnavailable   ErrorClass = "budget_unavailable"
	ErrorNoHealthyDeployment ErrorClass = "no_healthy_deployment"
	ErrorUpstreamUnavailable ErrorClass = "upstream_unavailable"
	ErrorUpstreamTimeout     ErrorClass = "upstream_timeout"
	ErrorUpstreamError       ErrorClass = "upstream_error"
	ErrorUpstreamRateLimited ErrorClass = "upstream_rate_limited"
	ErrorUpstreamClientError ErrorClass = "upstream_client_error"
	ErrorClientClosed        ErrorClass = "client_closed"
	ErrorShuttingDown        ErrorClass = "shutting_down"
	ErrorNotReady            ErrorClass = "not_ready"
	ErrorServerBusy          ErrorClass = "server_busy"
	ErrorInternal            ErrorClass = "internal"
)

var errorClasses = []ErrorClass{ErrorAuth, ErrorNotFound, ErrorInvalidRequest, ErrorRateLimited,
	ErrorQueueRejected, ErrorBudgetExceeded, ErrorBudgetUnavailable, ErrorNoHealthyDeployment, ErrorUpstreamUnavailable, ErrorUpstreamTimeout, ErrorUpstreamError,
	ErrorUpstreamRateLimited, ErrorUpstreamClientError, ErrorClientClosed, ErrorShuttingDown, ErrorNotReady, ErrorServerBusy,
	ErrorInternal}

// AttemptOutcome is what one upstream attempt came to, the kaiak.attempt.outcome of
// kaiak.upstream.attempts: the circuit breaker's classification
// (docs/specs/GATEWAY.md, Routing and reliability: outcome classes), named.
type AttemptOutcome string

const (
	// Success: a response relayed to its end, or until the client left.
	AttemptSuccess AttemptOutcome = "success"
	// Failures: they count toward the deployment's circuit.
	AttemptUnavailable  AttemptOutcome = "unavailable"   // connect error, lost before the first event
	AttemptTimeout      AttemptOutcome = "timeout"       // a stream's first-event timeout
	AttemptAuthFailed   AttemptOutcome = "auth_failed"   // the backend refused the gateway's credential
	AttemptModelMissing AttemptOutcome = "model_missing" // the backend does not serve the model
	AttemptPathMissing  AttemptOutcome = "path_missing"  // the backend's base_url leads to no endpoint
	AttemptServerError  AttemptOutcome = "server_error"  // a backend 5xx
	AttemptBrokeOff     AttemptOutcome = "broke_off"     // broken off, stalled or incomplete after the first event
	// Neutral: they leave the circuit as it is.
	AttemptEndpointMissing AttemptOutcome = "endpoint_missing" // the deployment's server does not serve an endpoint its type serves
	AttemptResponseTimeout AttemptOutcome = "response_timeout" // a non-stream response timeout
	AttemptRateLimited     AttemptOutcome = "rate_limited"     // a backend 429
	AttemptClientError     AttemptOutcome = "client_error"     // another backend 4xx, or the provider's refusal before sending
	AttemptCanceled        AttemptOutcome = "canceled"         // the client left, or the drain cut, before the first event
	AttemptInternal        AttemptOutcome = "internal"         // a gateway fault building the upstream request
)

var attemptOutcomes = []AttemptOutcome{AttemptSuccess, AttemptUnavailable, AttemptTimeout, AttemptAuthFailed,
	AttemptModelMissing, AttemptPathMissing, AttemptServerError, AttemptBrokeOff, AttemptEndpointMissing,
	AttemptResponseTimeout, AttemptRateLimited, AttemptClientError, AttemptCanceled, AttemptInternal}

// RetryableOutcomes are the outcomes after which an attempt that relayed nothing may
// be retried (docs/specs/GATEWAY.md, Routing and reliability: retries): the
// kaiak.attempt.outcome values of kaiak.retries.
var RetryableOutcomes = []AttemptOutcome{AttemptUnavailable, AttemptTimeout, AttemptServerError, AttemptRateLimited,
	AttemptAuthFailed, AttemptModelMissing, AttemptPathMissing, AttemptEndpointMissing}

// Bucket bounds. Durations span quick refusals to a long generation (longer ones,
// up to the default 30 min response timeout, fall in +Inf); decode rates span a busy CPU backend to a
// fast GPU.
var (
	durationBuckets   = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300}
	firstTokenBuckets = []float64{0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 30, 60, 120, 300}
	tokenRateBuckets  = []float64{1, 2, 5, 10, 20, 50, 100, 200, 500, 1000}
	// Queue waits span a slot freeing at once to the default queue timeout (30 s) and
	// beyond (a model may set a longer one).
	queueWaitBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120}
	// Attempts per request: one bucket per possible count (max_attempts is at most
	// config.MaxAttemptsCeiling).
	attemptBuckets = []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	// Config work — an apply, a limiter sync — spans a small config's fraction of a
	// millisecond to a large one's seconds.
	configWorkBuckets = []float64{0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30}
)

// QueueReason is why a model's queue refused a request: the error code answered, the
// error.type of kaiak.queue.rejections. The server names it from the refusal; it
// lives here, as ErrorClass does, because the server imports metrics.
type QueueReason string

const (
	QueueFull    QueueReason = "queue_full"    // the queue was full
	QueueTimeout QueueReason = "queue_timeout" // the wait for a slot timed out
)

var queueReasons = []QueueReason{QueueFull, QueueTimeout}

// Attribute keys several families share (docs/specs/GATEWAY.md, Observability →
// Metric list).
const (
	attrModel           = "gen_ai.request.model"
	attrBackend         = "kaiak.backend.id"
	attrDeploymentModel = "kaiak.deployment.model"
	attrOutcome         = "kaiak.attempt.outcome"
	attrCircuitState    = "kaiak.circuit.state"
)

// urlScheme is every request's url.scheme: the API listener speaks plain HTTP; TLS,
// where there is any, ends before it.
const urlScheme = "http"

// Ops holds the gateway's operational metrics: how requests go, not whose usage they
// are. The request pipeline feeds it; attribute values are routes, public model
// names, backend IDs and fixed vocabularies — never a key or any content.
type Ops struct {
	holder          *config.Holder
	circuits        *Circuits
	requestDuration *metric.Histogram
	firstToken      *metric.Histogram
	tokenRate       *metric.Histogram
	errors          *metric.Counter
	requestErrors   *metric.Counter
	limitRejections *metric.Counter
	queueWait       *metric.Histogram
	queueRejections *metric.Counter
	retries         *metric.Counter
	attempts        *metric.Histogram
	upstream        *metric.Counter
	upstreamTime    *metric.Histogram
	configLoads     *metric.Counter
	configApplied   *metric.Gauge
	configSize      *metric.Gauge
	configApply     *metric.Histogram
	limitsSync      *metric.Histogram
	refusedConns    *metric.Counter
}

// NewOps registers the ops metrics on reg. The routing metrics — in-flight counts,
// backend caps, queue depths, circuit and cooldown states — are read at each collect
// from one routing.Serving against holder's live config, so every backend, model and
// deployment of it is present (0 when idle); backend caps are the shares routing
// enforces. Counters and histograms whose attribute values the config determines,
// circuits' among them, are created at 0 for holder's config, if it has one, and for
// every config applied since (ConfigLoaded).
func NewOps(reg *metric.Registry, router *routing.Router, circuits *Circuits, holder *config.Holder) *Ops {
	o := &Ops{
		holder:   holder,
		circuits: circuits,
		requestDuration: reg.Histogram(metric.Definition{Name: "http.server.request.duration", Unit: "s",
			Description: "Time from request arrival to the end of its response, by method, route, status, error and model.",
			Attributes: []string{"http.request.method", "url.scheme", "http.route", "http.response.status_code",
				"error.type", attrModel},
			AttributeTypes: map[string]metric.AttributeType{"http.response.status_code": metric.IntAttribute},
			Buckets:        durationBuckets}),
		firstToken: reg.Histogram(metric.Definition{Name: "kaiak.time_to_first_token", Unit: "s",
			Description: "Time from the send of a stream's answering attempt to its first event carrying generated content.",
			Attributes:  []string{attrModel, attrBackend}, Buckets: firstTokenBuckets}),
		tokenRate: reg.Histogram(metric.Definition{Name: "kaiak.output_token_rate", Unit: "{token}/s",
			Description: "Decode speed of completed streams: output tokens after the first, per second after the first content event.",
			Attributes:  []string{attrModel, attrBackend}, Buckets: tokenRateBuckets}),
		errors: reg.Counter(metric.Definition{Name: "kaiak.errors", Unit: "{request}",
			Description: "Requests that ended in an error, by class.", Attributes: []string{"kaiak.error.class"}}),
		requestErrors: reg.Counter(metric.Definition{Name: "kaiak.request.errors", Unit: "{request}",
			Description: "Requests that ended in an error, refusals included, by key, model and error type.",
			Attributes:  append(append([]string(nil), keyAttributes...), attrModel, "error.type")}),
		limitRejections: reg.Counter(metric.Definition{Name: "kaiak.limit.rejections", Unit: "{request}",
			Description: "Requests a limit refused (rate_limit_exceeded, budget_exceeded), by the kind of scope the limit belongs to and its type.",
			Attributes:  []string{"kaiak.limit.scope", "kaiak.limit.type"}}),
		queueWait: reg.Histogram(metric.Definition{Name: "kaiak.queue.wait_duration", Unit: "s",
			Description: "Time requests waited in their model's queue before getting a slot.",
			Attributes:  []string{attrModel}, Buckets: queueWaitBuckets}),
		queueRejections: reg.Counter(metric.Definition{Name: "kaiak.queue.rejections", Unit: "{request}",
			Description: "Requests refused by their model's queue, by the error code answered.",
			Attributes:  []string{attrModel, "error.type"}}),
		retries: reg.Counter(metric.Definition{Name: "kaiak.retries", Unit: "{attempt}",
			Description: "Retries: attempts sent after an earlier attempt of the same request failed, by the backend of that attempt and the outcome that made it retryable.",
			Attributes:  []string{attrModel, attrBackend, attrOutcome}}),
		attempts: reg.Histogram(metric.Definition{Name: "kaiak.request.attempts", Unit: "{attempt}",
			Description: "Attempts per routed request, the first included.",
			Attributes:  []string{attrModel}, Buckets: attemptBuckets}),
		upstream: reg.Counter(metric.Definition{Name: "kaiak.upstream.attempts", Unit: "{attempt}",
			Description: "Upstream attempts per deployment, by outcome: the circuit breaker's classification of each attempt.",
			Attributes:  []string{attrBackend, attrDeploymentModel, attrOutcome}}),
		upstreamTime: reg.Histogram(metric.Definition{Name: "kaiak.upstream.attempt.duration", Unit: "s",
			Description: "Duration of every upstream attempt, from its send to its end: the end of the relay, or its failure.",
			Attributes:  []string{attrBackend}, Buckets: durationBuckets}),
		configLoads: reg.Counter(metric.Definition{Name: "kaiak.config.loads", Unit: "{load}",
			Description: "Config loads, by what asked for the load and whether it was applied.",
			Attributes:  []string{"kaiak.trigger", "kaiak.config.result"}}),
		configApplied: reg.Gauge(metric.Definition{Name: "kaiak.config.last_applied_timestamp", Unit: "s",
			Description: "Unix time the running config was applied."}),
		configSize: reg.Gauge(metric.Definition{Name: "kaiak.config.size", Unit: "By",
			Description: "Size of the running config document, in bytes."}),
		configApply: reg.Histogram(metric.Definition{Name: "kaiak.config.apply.duration", Unit: "s",
			Description: "Time a config load took to validate the document and build its snapshot, to the swap or the rejection, by trigger and result.",
			Attributes:  []string{"kaiak.trigger", "kaiak.config.result"}, Buckets: configWorkBuckets}),
		limitsSync: reg.Histogram(metric.Definition{Name: "kaiak.limits.sync.duration", Unit: "s",
			Description: "Time the limiter took to match its counters to a newly applied config; the request that finds the new config waits for it, and every other request waits behind it.",
			Buckets:     configWorkBuckets}),
		refusedConns: reg.Counter(metric.Definition{Name: "kaiak.connections.refused", Unit: "{connection}",
			Description: "API connections closed at accept because KAIAK_MAX_CONNECTIONS were open."}),
	}
	o.refusedConns.Add(0)
	for _, c := range errorClasses {
		o.errors.Add(0, string(c))
	}
	for _, scope := range limits.Scopes {
		for _, typ := range config.LimitTypes() {
			o.limitRejections.Add(0, string(scope), string(typ))
		}
	}
	for _, trigger := range config.Triggers {
		for _, result := range config.LoadResults {
			o.configLoads.Add(0, string(trigger), string(result))
			o.configApply.Prepare(string(trigger), string(result))
		}
	}
	o.limitsSync.Prepare()
	if s := holder.Current(); s != nil {
		o.prepareSeries(s)
	}
	o.registerRouting(reg, router)
	return o
}

// registerRouting registers the routing up-down counters, read together at collect
// from one routing.Serving against the live config.
func (o *Ops) registerRouting(reg *metric.Registry, router *routing.Router) {
	backend, model := []string{attrBackend}, []string{attrModel}
	deployment := []string{attrBackend, attrDeploymentModel}
	active := reg.ObservableUpDownCounter(metric.Definition{Name: "kaiak.backend.active_requests", Unit: "{request}",
		Description: "Requests in flight per backend, from routing until the response is over.", Attributes: backend})
	activeLimit := reg.ObservableUpDownCounter(metric.Definition{Name: "kaiak.backend.active_requests_limit", Unit: "{request}",
		Description: "The cap on requests in flight per backend this gateway enforces: its share of max_in_flight among the live gateways; backends without a cap are absent.",
		Attributes:  backend})
	queued := reg.ObservableUpDownCounter(metric.Definition{Name: "kaiak.queue.size", Unit: "{request}",
		Description: "Requests waiting in each model's queue for a slot.", Attributes: model})
	circuit := reg.ObservableUpDownCounter(metric.Definition{Name: "kaiak.circuit.state", Unit: "{deployment}",
		Description: "1 on the deployment's current circuit-breaker state (closed; open, waiting for a probe; half_open, the next request is a probe's trial), 0 on the other two.",
		Attributes:  append(append([]string(nil), deployment...), attrCircuitState)})
	cooling := reg.ObservableUpDownCounter(metric.Definition{Name: "kaiak.deployment.cooling_down", Unit: "{deployment}",
		Description: "1 while the deployment cools down after a 429 (routing sends it no request while another deployment of the model is eligible), else 0.",
		Attributes:  deployment})
	reg.Callback(func(obs *metric.Observer) {
		serving := router.Serving(o.holder.Current())
		for id, b := range serving.Backends {
			active.Observe(obs, int64(b.InFlight), id)
			if b.Share > 0 {
				activeLimit.Observe(obs, int64(b.Share), id)
			}
		}
		for name, m := range serving.Models {
			queued.Observe(obs, int64(m.Queued), name)
		}
		for id, d := range serving.Deployments {
			for _, state := range circuitStates {
				circuit.Observe(obs, oneIf(d.Circuit == state), id.Backend, id.Model, string(state))
			}
			cooling.Observe(obs, oneIf(d.CoolingDown()), id.Backend, id.Model)
		}
	}, active, activeLimit, queued, circuit, cooling)
}

// circuitStates are a circuit breaker's states, the kaiak.circuit.state values.
var circuitStates = []routing.CircuitState{routing.CircuitClosed, routing.CircuitOpen, routing.CircuitHalfOpen}

func oneIf(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// Circuits counts circuit-breaker events, fed by routing (a routing.Observer).
type Circuits struct {
	transitions *metric.Counter
	probes      *metric.Counter
}

// NewCircuits registers the circuit-breaker counters on reg. Their series per config
// are created by the Ops given them (NewOps).
func NewCircuits(reg *metric.Registry) *Circuits {
	return &Circuits{
		transitions: reg.Counter(metric.Definition{Name: "kaiak.circuit.transitions", Unit: "{transition}",
			Description: "Circuit-breaker transitions per deployment, by the state entered (open, half_open, closed).",
			Attributes:  []string{attrBackend, attrDeploymentModel, attrCircuitState}}),
		probes: reg.Counter(metric.Definition{Name: "kaiak.probes", Unit: "{probe}",
			Description: "Probes of backends with open circuits, by result (success, failure).",
			Attributes:  []string{attrBackend, "kaiak.probe.result"}}),
	}
}

// CircuitChanged counts one deployment's circuit transition.
func (c *Circuits) CircuitChanged(d routing.DeploymentID, to routing.CircuitState) {
	c.transitions.Inc(d.Backend, d.Model, string(to))
}

// prepareSeries creates at 0, for every deployment of s, its circuit transitions
// by state entered and its backend's probes by result, so the first opening shows
// as an increase. Series of deployments a later config drops stay until restart.
func (c *Circuits) prepareSeries(s *config.Snapshot) {
	for _, m := range s.Models {
		for _, d := range m.Deployments {
			for _, to := range circuitStates {
				c.transitions.Add(0, d.Backend.ID, d.Model, string(to))
			}
			c.probes.Add(0, d.Backend.ID, "success")
			c.probes.Add(0, d.Backend.ID, "failure")
		}
	}
}

// Probed counts one probe of backend.
func (c *Circuits) Probed(backend string, ok bool) {
	result := "failure"
	if ok {
		result = "success"
	}
	c.probes.Inc(backend, result)
}

// ObserveRequest records one finished client request (docs/specs/GATEWAY.md,
// Observability → the request duration's attributes): method is a method the HTTP
// convention knows or _OTHER; route the route as the client API documents it, ""
// for a path no route matched; status the status answered, 0 when none was sent
// (the client left first); errorType the request's error code, or how a response
// broken off after it started ended, "" without an error; model "" until the model
// passed the access check. Only fixed vocabularies and names the config declares
// become attribute values, so clients cannot mint series.
func (o *Ops) ObserveRequest(method, route string, status int, errorType, model string, d time.Duration) {
	code := ""
	if status != 0 {
		code = strconv.Itoa(status)
	}
	o.requestDuration.Observe(d.Seconds(), method, urlScheme, route, code, errorType, model)
}

// ObserveTimeToFirstToken records the time from the send of a streamed request's
// answering attempt to its first generated content.
func (o *Ops) ObserveTimeToFirstToken(model, backend string, d time.Duration) {
	o.firstToken.Observe(d.Seconds(), model, backend)
}

// ObserveOutputRate records one completed stream's decode speed in tokens per second.
func (o *Ops) ObserveOutputRate(model, backend string, tokensPerSecond float64) {
	o.tokenRate.Observe(tokensPerSecond, model, backend)
}

// CountRequestError counts one request that ended in an error by who sent it and how
// it ended: group and keyID are the request's key (group nil and keyID empty without
// a valid key), attributed as the usage metrics are (keyLabels), model only once it
// passed the access check, and code the request log line's error.type, or the
// kaiak.relay_end of a response broken off after it started. Series exist only once
// counted: their number follows the keys that met an error, not the config.
func (o *Ops) CountRequestError(group *config.Group, keyID, model, code string) {
	var path []string
	if group != nil {
		path = group.PathIDs
	}
	o.requestErrors.Inc(append(keyLabels(o.holder.Current(), path, keyID), model, code)...)
}

// CountError counts one request that ended in an error of class c.
func (o *Ops) CountError(c ErrorClass) {
	o.errors.Inc(string(c))
}

// CountLimitRejection counts one request refused by a limit of type typ belonging to
// a scope of kind scope.
func (o *Ops) CountLimitRejection(scope limits.Scope, typ config.LimitType) {
	o.limitRejections.Inc(string(scope), string(typ))
}

// ObserveQueueWait records how long a request waited in model's queue before it got
// a slot.
func (o *Ops) ObserveQueueWait(model string, d time.Duration) {
	o.queueWait.Observe(d.Seconds(), model)
}

// CountQueueRejection counts one request model's queue refused, for reason.
func (o *Ops) CountQueueRejection(model string, reason QueueReason) {
	o.queueRejections.Inc(model, string(reason))
}

// CountRetry counts one retry of a request to model, after an attempt on backend
// that came to outcome, one of RetryableOutcomes.
func (o *Ops) CountRetry(model, backend string, outcome AttemptOutcome) {
	o.retries.Inc(model, backend, string(outcome))
}

// ObserveUpstreamAttempt records one upstream attempt on a deployment (backend and
// the model name there): its outcome and how long it took.
func (o *Ops) ObserveUpstreamAttempt(backend, deploymentModel string, outcome AttemptOutcome, d time.Duration) {
	o.upstream.Inc(backend, deploymentModel, string(outcome))
	o.upstreamTime.Observe(d.Seconds(), backend)
}

// ObserveAttempts records how many attempts a routed request to model made.
func (o *Ops) ObserveAttempts(model string, n int) {
	o.attempts.Observe(float64(n), model)
}

// ConnectionRefused counts one API connection closed at accept: the listener had
// KAIAK_MAX_CONNECTIONS open.
func (o *Ops) ConnectionRefused() { o.refusedConns.Inc() }

// ConfigLoaded records one config load (a config.Applier's onLoad): its trigger and
// result, its duration when there was a document, and for an applied config when and
// its size. An applied config's series are created at 0.
func (o *Ops) ConfigLoaded(l config.Load) {
	if l.Applied() {
		o.configApplied.Set(float64(l.At.UnixMilli()) / 1000)
		o.configSize.Set(float64(l.Bytes))
		o.prepareSeries(l.Snapshot)
	}
	trigger, result := string(l.Trigger), string(l.Result())
	if l.Document {
		o.configApply.Observe(l.Duration.Seconds(), trigger, result)
	}
	o.configLoads.Inc(trigger, result)
}

// ObserveLimitsSync records one limiter sync to a new config (a limits sync
// observer).
func (o *Ops) ObserveLimitsSync(d time.Duration) {
	o.limitsSync.Observe(d.Seconds())
}

// prepareSeries creates at 0 every ops series whose attribute values s determines
// (docs/specs/GATEWAY.md, Observability: series at 0): per model its queue
// rejections, queue wait and attempts; per deployment its upstream attempts by
// outcome and its backend's attempt duration, and the circuit counters; per model
// and backend of its deployments the retries by outcome, time to first token and
// decode rate. Series of deployments a later config drops stay until restart. The
// request duration is left out: its status and error type come from the answer.
func (o *Ops) prepareSeries(s *config.Snapshot) {
	o.circuits.prepareSeries(s)
	for name, m := range s.Models {
		for _, reason := range queueReasons {
			o.queueRejections.Add(0, name, string(reason))
		}
		o.queueWait.Prepare(name)
		o.attempts.Prepare(name)
		for _, d := range m.Deployments {
			for _, outcome := range attemptOutcomes {
				o.upstream.Add(0, d.Backend.ID, d.Model, string(outcome))
			}
			o.upstreamTime.Prepare(d.Backend.ID)
			for _, outcome := range RetryableOutcomes {
				o.retries.Add(0, name, d.Backend.ID, string(outcome))
			}
			o.firstToken.Prepare(name, d.Backend.ID)
			o.tokenRate.Prepare(name, d.Backend.ID)
		}
	}
}
