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
// router, sending requests upstream through providers and settling their usage
// through recorder; every request is observed in ops. The
// holder must hold a snapshot before the handler serves: the API listener starts only
// once a config is loaded.
func NewAPI(holder *config.Holder, drain *Drain, bodies *BodyBudget, providers *provider.Registry,
	limiter *limits.Limiter, router *routing.Router, recorder *accounting.Recorder, ops *metrics.Ops,
	logger *slog.Logger) *API {
	keys := newKeyInFlight()
	a := &API{holder: holder, logger: logger, ops: ops, drain: drain, keys: keys,
		stages: newPipeline(drain, keys, bodies, providers, limiter, router, recorder), mux: http.NewServeMux()}

	for _, ep := range []endpoint{endpointChatCompletions, endpointCompletions, endpointEmbeddings} {
		a.mux.HandleFunc("POST "+ep.path(), func(w http.ResponseWriter, r *http.Request) {
			a.serve(w, r, ep, "")
		})
	}
	a.mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, r *http.Request) {
		a.serve(w, r, endpointListModels, "")
	})
	a.mux.HandleFunc("GET /v1/models/{rest...}", a.serveModelPath)

	// Method-less patterns catch the other methods on known paths; "/" catches
	// unknown paths. ServeMux's own 404 and 405 answers are plain text, so they never
	// reach clients.
	for _, path := range []string{endpointChatCompletions.path(), endpointCompletions.path(),
		endpointEmbeddings.path(), "/v1/models", "/v1/models/{rest...}"} {
		allow := http.MethodPost
		if strings.HasPrefix(path, "/v1/models") {
			allow = "GET, HEAD"
		}
		a.mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Allow", allow)
			a.refuse(w, r, errMethodNotAllowed(r))
		})
	}
	a.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		a.refuse(w, r, errUnknownURL(r))
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
		a.refuse(w, r, errUnknownURL(r))
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
			writeError(rq.w, err)
			break
		}
	}
	if rq.abort {
		// net/http cuts the connection without logging for this sentinel panic.
		panic(http.ErrAbortHandler)
	}
}

// refuse answers a request that matches no client API endpoint.
func (a *API) refuse(w http.ResponseWriter, r *http.Request, err *apiError) {
	rq := a.begin(w, r)
	rq.failure = err
	writeError(rq.w, err)
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

// logRequest writes the one log line per request. It never carries the key, a
// header value or any request or response content; strings the client controls
// (method, path, model) are clipped.
func (a *API) logRequest(rq *request) {
	attrs := []slog.Attr{
		slog.String("request_id", rq.id),
		slog.String("method", clip.String(rq.r.Method)),
		slog.String("path", clip.String(rq.r.URL.Path)),
		slog.Int("status", rq.w.status),
		slog.Float64("latency_ms", float64(time.Since(rq.start).Microseconds())/1000),
	}
	if rq.keyID != "" {
		attrs = append(attrs, slog.String("key_id", rq.keyID))
	}
	// The key's group: its config ID, as on the usage metrics; never its labels.
	if g := rq.identity.Group; g != nil {
		attrs = append(attrs, slog.String("group", g.ID))
	}
	if rq.model != "" {
		attrs = append(attrs, slog.String("model", clip.String(rq.model)))
	}
	if rq.modelAllowed && rq.endpoint.takesBody() {
		attrs = append(attrs, slog.Bool("stream", rq.inbound.Stream))
	}
	if rq.deployment.Backend != nil {
		attrs = append(attrs, slog.String("backend", rq.deployment.Backend.ID),
			slog.String("deployment_model", rq.deployment.Model),
			slog.Int("attempts", len(rq.attempts)))
	}
	if len(rq.attempts) > 1 {
		attrs = append(attrs, slog.String("tried", triedAttempts(rq)))
	}
	if rq.retryRefusal != "" {
		attrs = append(attrs, slog.String("retry_refused", rq.retryRefusal))
	}
	if rq.queueWait.Queued {
		attrs = append(attrs, slog.Float64("queue_wait_ms", float64(rq.queueWait.Duration.Microseconds())/1000))
	}
	if code := errorCode(rq); code != "" {
		attrs = append(attrs, slog.String("error_code", code))
	}
	if rej := rq.rejection; rej != nil {
		attrs = append(attrs, limitAttrs(rej)...)
	}
	if !rq.firstContent.IsZero() {
		attrs = append(attrs, slog.Float64("ttft_ms", float64(rq.ttft.Microseconds())/1000))
	}
	if rq.relayEnd != "" {
		attrs = append(attrs, slog.String("relay_end", rq.relayEnd))
	}
	if rq.upstreamErr != nil {
		attrs = append(attrs, slog.String("upstream_error", rq.upstreamErr.Error()))
	}
	if rq.upstreamErrorCode != "" {
		attrs = append(attrs, slog.String("upstream_error_code", rq.upstreamErrorCode))
	}
	if rq.upstreamErrorType != "" {
		attrs = append(attrs, slog.String("upstream_error_type", rq.upstreamErrorType))
	}
	if rq.authFailure != "" {
		attrs = append(attrs, slog.String("auth_failure", string(rq.authFailure)))
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
		attrs = append(attrs,
			slog.Int64("tokens_in", units[config.UnitTokensIn]),
			slog.Int64("tokens_cached", units[config.UnitTokensCached]),
			slog.Int64("tokens_cache_write", units[config.UnitTokensCacheWrite]),
			slog.Int64("tokens_out", units[config.UnitTokensOut]),
			slog.Int64("tokens_reasoning", units[config.UnitTokensReasoning]),
			slog.Float64("cost_usd", float64(cost)/1e9),
			slog.Bool("estimated", u.Estimated),
			slog.Bool("partial", u.Partial))
	}
	a.logger.LogAttrs(rq.r.Context(), slog.LevelInfo, "request", attrs...)
}

// limitAttrs are a limit refusal's log fields: the limit's kind of scope (global or
// group), its group's ID ("global" for a global limit), its type, the value enforced (a per-minute limit's
// share among the live gateways) and the value configured, what the window had used
// (unknown for a budget refused as unavailable) and, for a token limit, what the
// request asked for. Counts are in the limit's unit; USD limits are in dollars, as
// cost_usd.
func limitAttrs(rej *limits.Rejection) []slog.Attr {
	id := rej.ID
	if rej.Scope == limits.ScopeGlobal {
		id = "global"
	}
	value := func(key string, v int64) slog.Attr {
		if rej.Measure == limits.MeasureCost {
			return slog.Float64(key, float64(v)/1e9)
		}
		return slog.Int64(key, v)
	}
	attrs := []slog.Attr{slog.String("limit_scope", string(rej.Scope)), slog.String("limit_id", id),
		slog.String("limit_type", string(rej.Type)), value("limit", rej.Limit), value("limit_configured", rej.Max)}
	if rej.Unavailable {
		return attrs
	}
	attrs = append(attrs, value("used", rej.Used))
	if rej.Measure == limits.MeasureTokens {
		attrs = append(attrs, slog.Int64("requested", rej.Requested))
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
