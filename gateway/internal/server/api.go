// Package server holds the gateway's two HTTP surfaces: the API listener, where every
// client request runs the request pipeline, and the admin listener (health, readiness
// and metrics).
package server

import (
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"kaiak/internal/accounting"
	"kaiak/internal/clip"
	"kaiak/internal/config"
	"kaiak/internal/limits"
	"kaiak/internal/logattr"
	"kaiak/internal/metrics"
	"kaiak/internal/provider"
	"kaiak/internal/routing"
)

// API is the client-facing handler. Every request to a client API endpoint runs the
// same pipeline; anything else is refused before it.
type API struct {
	holder *config.Holder
	logger *slog.Logger
	ops    *metrics.Ops
	drain  *Drain
	// keys counts each key's requests in flight (the per-key concurrency limit).
	keys   *keyInFlight
	stages []stage
	mux    *http.ServeMux
}

// NewAPI returns the client API handler, admitting requests and counting them in
// flight through drain, holding request bodies within bodies, checking limits through limiter, choosing deployments through
// router (leaving out those on backends missing remembers lacking the endpoint),
// sending requests upstream through providers and settling their usage through
// recorder; every request is observed in ops. The
// holder must hold a snapshot before the handler serves: the API listener starts only
// once a config is loaded.
func NewAPI(holder *config.Holder, drain *Drain, bodies *BodyBudget, providers *provider.Registry,
	limiter *limits.Limiter, router *routing.Router, missing *MissingEndpoints, recorder *accounting.Recorder,
	ops *metrics.Ops, logger *slog.Logger) *API {
	keys := newKeyInFlight()
	a := &API{holder: holder, logger: logger, ops: ops, drain: drain, keys: keys,
		stages: newPipeline(drain, keys, bodies, providers, limiter, router, missing, recorder, logger), mux: http.NewServeMux()}

	for _, ep := range bodyEndpoints {
		a.mux.HandleFunc("POST "+ep.path(), func(w http.ResponseWriter, r *http.Request) {
			a.serve(w, r, ep, "")
		})
	}
	a.mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, r *http.Request) {
		a.serve(w, r, endpointListModels, "")
	})
	a.mux.HandleFunc("GET /v1/models/{rest...}", a.serveModelPath)

	// Method-less patterns catch the other methods on known paths, answered in the
	// path's error shape; "/" catches unknown paths. ServeMux's own 404 and 405
	// answers are plain text, so they never reach clients.
	refuseMethod := func(path, allow string, ep endpoint) {
		a.mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Allow", allow)
			a.refuse(w, r, errMethodNotAllowed(r), errorShapeOf(ep, r))
		})
	}
	refuseMethod("/v1/models", "GET, HEAD", endpointListModels)
	refuseMethod("/v1/models/{rest...}", "GET, HEAD", endpointGetModel)
	for _, ep := range bodyEndpoints {
		refuseMethod(ep.path(), http.MethodPost, ep)
	}
	a.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		a.refuse(w, r, errUnknownURL(r), shapeOpenAI)
	})
	return a
}

// ServeHTTP counts the request in flight until everything it does is over. Once the
// drain has begun every response closes its connection, so keep-alive clients
// reconnect — to another instance once this one is out of rotation.
func (a *API) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.drain.enter()
	defer a.drain.exit()
	if a.drain.Draining() {
		w.Header().Set("Connection", "close")
	}
	a.mux.ServeHTTP(w, r)
}

// serveModelPath routes /v1/models/{id} and /v1/models/{id}/props. Model names may
// contain "/", so the id is the rest of the path; a trailing "/props" always selects
// the props endpoint.
func (a *API) serveModelPath(w http.ResponseWriter, r *http.Request) {
	rest := r.PathValue("rest")
	if id, ok := strings.CutSuffix(rest, "/props"); ok && id != "" {
		a.serve(w, r, endpointModelProps, id)
		return
	}
	if rest == "" {
		a.refuse(w, r, errUnknownURL(r), shapeOpenAI)
		return
	}
	a.serve(w, r, endpointGetModel, rest)
}

// serve runs the pipeline for one request to ep. pathModel is the model named by the
// route ("" for endpoints that take it from the body or name none).
func (a *API) serve(w http.ResponseWriter, r *http.Request, ep endpoint, pathModel string) {
	rq := a.begin(w, r)
	defer rq.finish()
	// The first finisher runs last: the ops metrics and the log line carry what
	// settlement found.
	rq.finishers = append(rq.finishers, func() {
		a.observeRequest(rq)
		a.logRequest(rq)
	})
	rq.endpoint = ep
	rq.model = pathModel
	for _, st := range a.stages {
		if err := st.run(r.Context(), rq); err != nil {
			rq.failure = err
			writeError(rq.w, err, rq.errorShape())
			break
		}
	}
	if rq.abort {
		// net/http cuts the connection without logging for this sentinel panic.
		panic(http.ErrAbortHandler)
	}
}

// refuse answers a request that matches no client API endpoint, in shape.
func (a *API) refuse(w http.ResponseWriter, r *http.Request, err *apiError, shape errorShape) {
	rq := a.begin(w, r)
	rq.failure = err
	writeError(rq.w, err, shape)
	a.observeRequest(rq)
	a.logRequest(rq)
}

// begin sets up the per-request state every answer needs: start time, request ID
// (echoed on the response), the config snapshot.
func (a *API) begin(w http.ResponseWriter, r *http.Request) *request {
	rq := &request{
		w:        &statusWriter{ResponseWriter: w},
		r:        r,
		id:       requestID(r.Header.Get("X-Request-Id")),
		start:    time.Now(),
		snapshot: a.holder.Current(),
		ops:      a.ops,
	}
	w.Header().Set("X-Request-Id", rq.id)
	return rq
}

// logRequest writes the one log line per request, in the log vocabulary
// (docs/specs/GATEWAY.md, Observability: Logs → the request line). It never carries
// the key, a header value or any request or response content; strings the client
// controls (method, path, model) are clipped.
func (a *API) logRequest(rq *request) {
	attrs := []slog.Attr{slog.String("kaiak.request.id", rq.id)}
	attrs = append(attrs, methodAttrs(rq.r.Method)...)
	attrs = append(attrs, slog.String("url.path", clip.String(rq.r.URL.Path)))
	// No status for a client that left before any answer: none was sent. Its 499 is
	// the request metric's alone (docs/specs/GATEWAY.md, Observability → Logs).
	if rq.failure == nil || rq.failure.code != codeClientClosed {
		attrs = append(attrs, slog.Int("http.response.status_code", rq.w.status))
	}
	attrs = append(attrs, logattr.SecondsMicro("kaiak.request.duration", time.Since(rq.start)))
	if rq.keyID != "" {
		attrs = append(attrs, slog.String("kaiak.key.id", rq.keyID))
	}
	// The key's group: its config ID, as on the usage metrics; never its labels.
	if g := rq.identity.Group; g != nil {
		attrs = append(attrs, slog.String("kaiak.key.group", g.ID))
	}
	if rq.model != "" {
		attrs = append(attrs, slog.String("gen_ai.request.model", clip.String(rq.model)))
	}
	if rq.modelAllowed && rq.endpoint.takesBody() {
		attrs = append(attrs, slog.Bool("gen_ai.request.stream", rq.inbound.Stream))
	}
	if op := rq.endpoint.operationName(); op != "" {
		attrs = append(attrs, slog.String("gen_ai.operation.name", op))
	}
	if b := rq.deployment.Backend; b != nil {
		attrs = append(attrs, slog.String("kaiak.backend.id", b.ID),
			slog.String("kaiak.backend.type", string(b.Type)))
		if name := providerName(b.Type); name != "" {
			attrs = append(attrs, slog.String("gen_ai.provider.name", name))
		}
		attrs = append(attrs, slog.String("kaiak.deployment.model", rq.deployment.Model),
			slog.Int("kaiak.attempts", len(rq.attempts)))
	}
	if len(rq.attempts) > 1 {
		attrs = append(attrs, slog.String("kaiak.tried", triedAttempts(rq)))
	}
	if rq.retryRefusal != "" {
		attrs = append(attrs, slog.String("kaiak.retry_refused", rq.retryRefusal))
	}
	if rq.queueWait.Queued {
		attrs = append(attrs, logattr.SecondsMicro("kaiak.queue.wait_duration", rq.queueWait.Duration))
	}
	if code := errorCode(rq); code != "" {
		attrs = append(attrs, slog.String("error.type", code))
	}
	if rej := rq.rejection; rej != nil {
		attrs = append(attrs, limitAttrs(rej)...)
	}
	if !rq.firstContent.IsZero() {
		attrs = append(attrs, logattr.SecondsMicro("kaiak.time_to_first_token", rq.ttft))
	}
	if rq.relayEnd != "" {
		attrs = append(attrs, slog.String("kaiak.relay_end", rq.relayEnd))
	}
	if rq.upstreamErr != nil {
		attrs = append(attrs, slog.String("kaiak.upstream.error.message", rq.upstreamErr.Error()))
	}
	if rq.upstreamErrorCode != "" {
		attrs = append(attrs, slog.String("kaiak.upstream.error.code", rq.upstreamErrorCode))
	}
	if rq.upstreamErrorType != "" {
		attrs = append(attrs, slog.String("kaiak.upstream.error.type", rq.upstreamErrorType))
	}
	if rq.authFailure != "" {
		attrs = append(attrs, slog.String("kaiak.auth.failure", string(rq.authFailure)))
	}
	if u := rq.usage; u != nil {
		// Units and cost are the request's: every record's (one per attempt that
		// has usage); the flags are the answering attempt's.
		units := make(accounting.Units)
		var cost int64
		for _, rec := range rq.records {
			for unit, n := range rec.Units {
				units[unit] += n
			}
			cost += rec.CostNanoUSD
		}
		// gen_ai.usage.input_tokens is all input, as the GenAI convention means it:
		// the input neither read from nor written to the cache, plus its two cache
		// parts.
		cached, cacheWrite := units[config.UnitTokensCached], units[config.UnitTokensCacheWrite]
		attrs = append(attrs,
			slog.Int64("gen_ai.usage.input_tokens", units[config.UnitTokensIn]+cached+cacheWrite),
			slog.Int64("gen_ai.usage.cache_read.input_tokens", cached),
			slog.Int64("gen_ai.usage.cache_write.input_tokens", cacheWrite),
			slog.Int64("gen_ai.usage.output_tokens", units[config.UnitTokensOut]),
			slog.Int64("gen_ai.usage.reasoning.output_tokens", units[config.UnitTokensReasoning]),
			slog.Float64("kaiak.usage.cost_usd", float64(cost)/1e9),
			slog.Bool("kaiak.usage.estimated", u.Estimated),
			slog.Bool("kaiak.usage.partial", u.Partial))
	}
	a.logger.LogAttrs(rq.r.Context(), slog.LevelInfo, "request", attrs...)
}

// methodAttrs are the request method's log fields: http.request.method — a method
// the HTTP semantic convention names, else _OTHER — and, for _OTHER,
// http.request.method_original, the method as sent (clipped).
func methodAttrs(method string) []slog.Attr {
	switch method {
	case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodHead,
		http.MethodOptions, http.MethodPatch, http.MethodConnect, http.MethodTrace, "QUERY":
		return []slog.Attr{slog.String("http.request.method", method)}
	}
	return []slog.Attr{slog.String("http.request.method", "_OTHER"),
		slog.String("http.request.method_original", clip.String(method))}
}

// providerName is gen_ai.provider.name for a backend type: the GenAI convention's
// well-known value where one fits, else "" — the self-hosted types have none, and
// kaiak.backend.type names every type. Claude in Foundry is Anthropic's service and
// API on Azure, and no Azure value names it, so it is anthropic too.
func providerName(t config.BackendType) string {
	switch t {
	case config.BackendOpenAI:
		return "openai"
	case config.BackendAzureOpenAI:
		return "azure.ai.openai"
	case config.BackendAnthropic, config.BackendAzureAnthropic:
		return "anthropic"
	}
	return ""
}

// limitAttrs are a limit refusal's log fields: the limit's kind of scope (global or
// group), its group's ID (absent for a global limit), its type, the value enforced (a
// per-minute limit's share among the live gateways) and the value configured, what the
// window had used (unknown for a budget refused as unavailable) and, for a token
// limit, what the request asked for. Counts are in the limit's unit; USD limits are in
// dollars, as kaiak.usage.cost_usd (limits.LogValue).
func limitAttrs(rej *limits.Rejection) []slog.Attr {
	value := func(key string, v int64) slog.Attr { return slog.Any(key, limits.LogValue(rej.Measure, v)) }
	attrs := []slog.Attr{slog.String("kaiak.limit.scope", string(rej.Scope))}
	if rej.Group != "" {
		attrs = append(attrs, slog.String("kaiak.limit.group", rej.Group))
	}
	attrs = append(attrs, slog.String("kaiak.limit.type", string(rej.Type)), value("kaiak.limit.enforced", rej.Limit),
		value("kaiak.limit.configured", rej.Max))
	if rej.Unavailable {
		return attrs
	}
	attrs = append(attrs, value("kaiak.limit.used", rej.Used))
	if rej.Measure == limits.MeasureTokens {
		attrs = append(attrs, slog.Int64("kaiak.limit.requested", rej.Requested))
	}
	return attrs
}

// triedAttempts lists a request's attempts for the log line, in order, as
// backend/deployment_model:outcome — the backend's status or the gateway's error
// code (e.g. "down/m:upstream_unavailable,local/m:200").
func triedAttempts(rq *request) string {
	var b strings.Builder
	for i, at := range rq.attempts {
		if i > 0 {
			b.WriteByte(',')
		}
		outcome := at.outcome
		if outcome == "" {
			// The answering attempt: set only for attempts a retry was decided for.
			outcome = attemptOutcome(rq, rq.failure)
		}
		b.WriteString(at.deployment.Backend.ID + "/" + at.deployment.Model + ":" + outcome)
	}
	return b.String()
}

// Request IDs: a client's x-request-id is kept when it is 1 to maxRequestIDLength
// characters of [A-Za-z0-9._:-]; anything else is replaced by a generated ID.
const maxRequestIDLength = 128

func requestID(fromClient string) string {
	if validRequestID(fromClient) {
		return fromClient
	}
	var b [16]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never returns an error
	return hex.EncodeToString(b[:])
}

func validRequestID(id string) bool {
	if id == "" || len(id) > maxRequestIDLength {
		return false
	}
	for _, c := range []byte(id) {
		switch {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9':
		case c == '.', c == '_', c == ':', c == '-':
		default:
			return false
		}
	}
	return true
}

// statusWriter records the status code written, for the log line. Unwrap lets
// http.ResponseController reach the underlying writer (flushing streams).
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (s *statusWriter) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Write(p []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	return s.ResponseWriter.Write(p)
}

func (s *statusWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }
