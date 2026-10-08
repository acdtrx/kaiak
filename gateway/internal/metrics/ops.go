package metrics

import (
	"runtime"
	"runtime/debug"
	"slices"
	"time"

	"kaiak/internal/config"
	"kaiak/internal/routing"
)

// ErrorClass groups the gateway's error codes for kaiak_errors_total
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

// AttemptOutcome is what one upstream attempt came to, the outcome label of
// kaiak_upstream_attempts_total: the circuit breaker's classification
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
	AttemptEndpointMissing AttemptOutcome = "endpoint_missing" // the backend's server lacks an endpoint its type serves
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
// be retried (docs/specs/GATEWAY.md, Routing and reliability: retries): the reason
// label values of kaiak_retries_total.
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

// Queue rejection reasons, the reason label of kaiak_queue_rejections_total.
const (
	QueueFull    = "full"
	QueueTimeout = "timeout"
)

var queueReasons = []string{QueueFull, QueueTimeout}

// limitScopeKinds are the scope_kind label values of kaiak_limit_rejections_total:
// the kinds of scope a limit belongs to (limits.Scope). Its type label values are
// config.LimitTypes().
var limitScopeKinds = []string{"global", "group"}

// configTriggers and configResults are the label values of kaiak_config_loads_total:
// the file loader's triggers and the control client's (control.Trigger*).
var (
	configTriggers = []string{"startup", "sighup", "control", "seed"}
	configResults  = []string{"applied", "rejected"}
)

// Ops holds the gateway's operational metrics: how requests go, not whose usage they
// are. The request pipeline feeds it; label values are endpoint names, public model
// names, backend IDs and classes — never a key or any content.
type Ops struct {
	holder          *config.Holder
	requestDuration *HistogramVec
	firstToken      *HistogramVec
	tokenRate       *HistogramVec
	errors          *CounterVec
	requestErrors   *CounterVec
	limitRejections *CounterVec
	queueWait       *HistogramVec
	queueRejections *CounterVec
	retries         *CounterVec
	attempts        *HistogramVec
	upstream        *CounterVec
	upstreamTime    *HistogramVec
	configLoads     *CounterVec
	configApplied   *GaugeVec
	configSize      *GaugeVec
	configApply     *HistogramVec
	limitsSync      *HistogramVec
	refusedConns    *CounterVec
}

// NewOps registers the ops metrics on reg. In-flight counts and queue depths are read
// from router at scrape time, with every backend and model of holder's live config
// present (0 when idle); backend caps are the shares routing enforces. Counters and
// histograms whose label values the config determines are created at 0 for holder's
// config, if it has one, and for every config applied since (ConfigLoaded).
func NewOps(reg *Registry, router *routing.Router, holder *config.Holder) *Ops {
	o := &Ops{
		holder: holder,
		requestDuration: reg.Histogram("kaiak_request_duration_seconds",
			"Time from request arrival to the end of its response, by endpoint, model and status class.",
			durationBuckets, "endpoint", "model", "status_class"),
		firstToken: reg.Histogram("kaiak_time_to_first_token_seconds",
			"Time from the send of a stream's answering attempt to its first event carrying generated content.",
			firstTokenBuckets, "model", "backend"),
		tokenRate: reg.Histogram("kaiak_output_tokens_per_second",
			"Decode speed of completed streams: output tokens after the first, per second after the first content event.",
			tokenRateBuckets, "model", "backend"),
		errors: reg.Counter("kaiak_errors_total",
			"Requests that ended in an error, by class.", "class"),
		requestErrors: reg.Counter("kaiak_request_errors_total",
			"Requests that ended in an error, refusals included, by key and error code.",
			"key_group", "root_group", "key_id", "model", "code"),
		limitRejections: reg.Counter("kaiak_limit_rejections_total",
			"Requests a limit refused (rate_limit_exceeded, budget_exceeded), by the kind of scope the limit belongs to and its type.",
			"scope_kind", "type"),
		queueWait: reg.Histogram("kaiak_queue_wait_seconds",
			"Time requests waited in their model's queue before getting a slot.",
			queueWaitBuckets, "model"),
		queueRejections: reg.Counter("kaiak_queue_rejections_total",
			"Requests refused by their model's queue, by reason (full, timeout).", "model", "reason"),
		retries: reg.Counter("kaiak_retries_total",
			"Retries: attempts sent after an earlier attempt of the same request failed, by the backend of that attempt and its failure (unavailable, timeout, server_error, rate_limited, auth_failed, model_missing, path_missing, endpoint_missing).",
			"model", "backend", "reason"),
		attempts: reg.Histogram("kaiak_request_attempts",
			"Attempts per routed request, the first included.", attemptBuckets, "model"),
		upstream: reg.Counter("kaiak_upstream_attempts_total",
			"Upstream attempts per deployment, by outcome: the circuit breaker's classification of each attempt.",
			"backend", "deployment_model", "outcome"),
		upstreamTime: reg.Histogram("kaiak_upstream_attempt_duration_seconds",
			"Duration of every upstream attempt, from its send to its end: the end of the relay, or its failure.",
			durationBuckets, "backend"),
		configLoads: reg.Counter("kaiak_config_loads_total",
			"Config loads by trigger (startup, sighup, control, seed) and result (applied, rejected).", "trigger", "result"),
		configApplied: reg.Gauge("kaiak_config_last_applied_timestamp_seconds",
			"Unix time the running config was applied."),
		configSize: reg.Gauge("kaiak_config_size_bytes",
			"Size of the running config document, in bytes."),
		configApply: reg.Histogram("kaiak_config_apply_duration_seconds",
			"Time a config load took to validate the document and build its snapshot, to the swap or the rejection, by trigger and result.",
			configWorkBuckets, "trigger", "result"),
		limitsSync: reg.Histogram("kaiak_limits_sync_duration_seconds",
			"Time the limiter took to match its counters to a newly applied config; the request that finds the new config waits for it, and every other request waits behind it.",
			configWorkBuckets),
		refusedConns: reg.Counter("kaiak_connections_refused_total",
			"API connections closed at accept because KAIAK_MAX_CONNECTIONS were open."),
	}
	o.refusedConns.Add(0)
	for _, c := range errorClasses {
		o.errors.Add(0, string(c))
	}
	for _, kind := range limitScopeKinds {
		for _, typ := range config.LimitTypes() {
			o.limitRejections.Add(0, kind, string(typ))
		}
	}
	for _, trigger := range configTriggers {
		for _, result := range configResults {
			o.configLoads.Add(0, trigger, result)
			o.configApply.Prepare(trigger, result)
		}
	}
	o.limitsSync.Prepare()
	if s := holder.Current(); s != nil {
		o.prepareSeries(s)
	}
	reg.GaugeFunc("kaiak_backend_in_flight_requests",
		"Requests in flight per backend, from routing until the response is over.",
		[]string{"backend"}, func(emit func(float64, ...string)) {
			counts := router.InFlightByBackend()
			if s := holder.Current(); s != nil {
				for id := range s.Backends {
					if _, ok := counts[id]; !ok {
						counts[id] = 0
					}
				}
			}
			for id, n := range counts {
				emit(float64(n), id)
			}
		})
	reg.GaugeFunc("kaiak_backend_max_in_flight",
		"The cap on requests in flight per backend this gateway enforces: its share of max_in_flight among the live gateways; backends without a cap are absent.",
		[]string{"backend"}, func(emit func(float64, ...string)) {
			for id, n := range router.MaxInFlightByBackend() {
				emit(float64(n), id)
			}
		})
	reg.GaugeFunc("kaiak_queued_requests",
		"Requests waiting in each model's queue for a slot.",
		[]string{"model"}, func(emit func(float64, ...string)) {
			counts := router.QueuedByModel()
			if s := holder.Current(); s != nil {
				for name := range s.Models {
					if _, ok := counts[name]; !ok {
						counts[name] = 0
					}
				}
			}
			for name, n := range counts {
				emit(float64(n), name)
			}
		})
	circuitGauge := func(state routing.CircuitState) func(emit func(float64, ...string)) {
		return func(emit func(float64, ...string)) {
			// One sample per distinct deployment: public models may share one.
			values := make(map[routing.DeploymentID]float64)
			if s := holder.Current(); s != nil {
				for _, m := range s.Models {
					for _, d := range m.Deployments {
						values[routing.DeploymentID{Backend: d.Backend.ID, Model: d.Model}] = 0
					}
				}
			}
			for id, c := range router.Circuits() {
				if c.State == state {
					values[id] = 1
				}
			}
			for id, v := range values {
				emit(v, id.Backend, id.Model)
			}
		}
	}
	reg.GaugeFunc("kaiak_circuit_open",
		"1 while the deployment's circuit breaker is open (waiting for a probe), else 0; half-open counts 0.",
		[]string{"backend", "deployment_model"}, circuitGauge(routing.CircuitOpen))
	reg.GaugeFunc("kaiak_circuit_half_open",
		"1 while the deployment's circuit breaker is half-open (a probe succeeded; the next request is its trial), else 0.",
		[]string{"backend", "deployment_model"}, circuitGauge(routing.CircuitHalfOpen))
	reg.GaugeFunc("kaiak_deployment_cooling_down",
		"1 while the deployment cools down after a 429 (routing sends it no request while another deployment of the model is eligible), else 0.",
		[]string{"backend", "deployment_model"}, func(emit func(float64, ...string)) {
			// One sample per distinct deployment: public models may share one.
			values := make(map[routing.DeploymentID]float64)
			if s := holder.Current(); s != nil {
				for _, m := range s.Models {
					for _, d := range m.Deployments {
						values[routing.DeploymentID{Backend: d.Backend.ID, Model: d.Model}] = 0
					}
				}
			}
			for id := range router.CoolingDown() {
				values[id] = 1
			}
			for id, v := range values {
				emit(v, id.Backend, id.Model)
			}
		})
	registerBuildInfo(reg)
	return o
}

// Circuits counts circuit-breaker events, fed by routing (a routing.Observer).
type Circuits struct {
	transitions *CounterVec
	probes      *CounterVec
}

// NewCircuits registers the circuit-breaker counters on reg.
func NewCircuits(reg *Registry) *Circuits {
	return &Circuits{
		transitions: reg.Counter("kaiak_circuit_transitions_total",
			"Circuit-breaker transitions per deployment, by the state entered (open, half_open, closed).", "backend", "deployment_model", "to"),
		probes: reg.Counter("kaiak_probes_total",
			"Probes of backends with open circuits, by result (success, failure).", "backend", "result"),
	}
}

// CircuitChanged counts one deployment's circuit transition.
func (c *Circuits) CircuitChanged(d routing.DeploymentID, to routing.CircuitState) {
	c.transitions.Inc(d.Backend, d.Model, string(to))
}

// PrepareSeries creates at 0, for every deployment of s, its circuit transitions
// by state entered and its backend's probes by result, so the first opening shows
// as an increase. Series of deployments a later config drops stay until restart.
func (c *Circuits) PrepareSeries(s *config.Snapshot) {
	for _, m := range s.Models {
		for _, d := range m.Deployments {
			for _, to := range []routing.CircuitState{routing.CircuitOpen, routing.CircuitHalfOpen, routing.CircuitClosed} {
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

// version is the release the binary was built as, set only at link time by the image
// build (-ldflags "-X kaiak/internal/metrics.version=<git describe>"). It is a build
// constant: nothing assigns it at run time.
var version string

// registerBuildInfo registers kaiak_build_info: the build's version and the Go version.
func registerBuildInfo(reg *Registry) {
	reg.Gauge("kaiak_build_info", "Build information; the value is always 1.", "version", "go_version").
		Set(1, Version(), runtime.Version())
}

// Version is the build version kaiak_build_info reports.
func Version() string {
	info, ok := debug.ReadBuildInfo()
	return buildVersion(version, info, ok)
}

// buildVersion picks the version to report: the one stamped at link time, else the
// module version Go recorded (a VCS pseudo-version for `go build` in a checkout),
// else "(devel)" (`go run`, or a build with no version control).
func buildVersion(stamped string, info *debug.BuildInfo, ok bool) string {
	if stamped != "" {
		return stamped
	}
	if ok && info.Main.Version != "" {
		return info.Main.Version
	}
	return "(devel)"
}

// ObserveRequest records one finished client request. endpoint and model are "" when
// unknown (an unknown path, a model the caller may not use): only names the config
// declares become label values, so clients cannot mint series.
func (o *Ops) ObserveRequest(endpoint, model string, status int, d time.Duration) {
	o.requestDuration.Observe(d.Seconds(), endpoint, model, statusClass(status))
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
// a valid key), labelled as the usage metrics are — key_group, root_group, key_id,
// following the live config's key_id_label and group_label — model only once it
// passed the access check, and code the request log line's error.type, or the
// kaiak.relay_end of a response broken off after it started. Series exist only once
// counted: their number follows the keys that met an error, not the config.
func (o *Ops) CountRequestError(group *config.Group, keyID, model, code string) {
	var keyGroup, root string
	if group != nil {
		keyGroup, root = group.ID, group.PathIDs[0]
	}
	if snap := o.holder.Current(); snap != nil {
		if !snap.KeyIDLabel {
			keyID = ""
		}
		if !snap.GroupLabel {
			keyGroup = ""
		}
	}
	o.requestErrors.Inc(keyGroup, root, keyID, model, code)
}

// CountError counts one request that ended in an error of class c.
func (o *Ops) CountError(c ErrorClass) {
	if !slices.Contains(errorClasses, c) {
		panic("metrics: unknown error class " + string(c))
	}
	o.errors.Inc(string(c))
}

// CountLimitRejection counts one request refused by a limit of type typ belonging to
// a scope of kind scopeKind (global, group).
func (o *Ops) CountLimitRejection(scopeKind string, typ config.LimitType) {
	if !slices.Contains(limitScopeKinds, scopeKind) || !slices.Contains(config.LimitTypes(), typ) {
		panic("metrics: unknown limit " + scopeKind + " " + string(typ))
	}
	o.limitRejections.Inc(scopeKind, string(typ))
}

// ObserveQueueWait records how long a request waited in model's queue before it got
// a slot.
func (o *Ops) ObserveQueueWait(model string, d time.Duration) {
	o.queueWait.Observe(d.Seconds(), model)
}

// CountQueueRejection counts one request model's queue refused, for reason QueueFull
// or QueueTimeout.
func (o *Ops) CountQueueRejection(model, reason string) {
	o.queueRejections.Inc(model, reason)
}

// CountRetry counts one retry of a request to model, after an attempt on backend
// that came to outcome, one of RetryableOutcomes.
func (o *Ops) CountRetry(model, backend string, outcome AttemptOutcome) {
	if !slices.Contains(RetryableOutcomes, outcome) {
		panic("metrics: not a retryable outcome " + string(outcome))
	}
	o.retries.Inc(model, backend, string(outcome))
}

// ObserveUpstreamAttempt records one upstream attempt on a deployment (backend and
// the model name there): its outcome and how long it took.
func (o *Ops) ObserveUpstreamAttempt(backend, deploymentModel string, outcome AttemptOutcome, d time.Duration) {
	if !slices.Contains(attemptOutcomes, outcome) {
		panic("metrics: unknown attempt outcome " + string(outcome))
	}
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
	result := "rejected"
	if l.Applied() {
		result = "applied"
		o.configApplied.Set(float64(l.At.UnixMilli()) / 1000)
		o.configSize.Set(float64(l.Bytes))
		o.prepareSeries(l.Snapshot)
	}
	if l.Document {
		o.configApply.Observe(l.Duration.Seconds(), l.Trigger, result)
	}
	o.configLoads.Inc(l.Trigger, result)
}

// ObserveLimitsSync records one limiter sync to a new config (a limits sync
// observer).
func (o *Ops) ObserveLimitsSync(d time.Duration) {
	o.limitsSync.Observe(d.Seconds())
}

// prepareSeries creates at 0 every ops series whose label values s determines
// (docs/specs/GATEWAY.md, Observability: series at 0): per model its queue
// rejections, queue wait and attempts; per deployment its upstream attempts by
// outcome and its backend's attempt duration; per model and backend of its
// deployments the retries by reason, time to first token and decode rate. Series of
// deployments a later config drops stay until restart. The request duration is left
// out: its status class comes from traffic.
func (o *Ops) prepareSeries(s *config.Snapshot) {
	for name, m := range s.Models {
		for _, reason := range queueReasons {
			o.queueRejections.Add(0, name, reason)
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

func statusClass(status int) string {
	if status < 100 || status > 599 {
		return ""
	}
	return string(rune('0'+status/100)) + "xx"
}
