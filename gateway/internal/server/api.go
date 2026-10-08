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
		if !st.scope.covers(ep) {
			continue
		}
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
