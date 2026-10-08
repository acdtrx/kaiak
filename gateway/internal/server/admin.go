package server

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"io"
	"net/http"

	"kaiak/internal/config"
	"kaiak/internal/telemetry/metric"
)

// NewAdmin returns the admin handler: /healthz (the process is up), /readyz (it
// should receive traffic: a config is loaded and drain has not begun) and /metrics
// (a collect of reg in the Prometheus text format). It never serves the client API,
// and the API listener never serves these. With metricsToken set, /metrics answers only
// requests bearing it (Authorization: Bearer); the probes stay open, as kubelets
// send no credentials.
func NewAdmin(holder *config.Holder, drain *Drain, reg *metric.Registry, metricsToken string) http.Handler {
	mux := http.NewServeMux()
	var scrape http.Handler = scrapeHandler(reg)
	if metricsToken != "" {
		scrape = requireBearer(metricsToken, scrape)
	}
	mux.Handle("GET /metrics", scrape)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writePlain(w, http.StatusOK, "ok")
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if reason := notReadyReason(holder, drain); reason != "" {
			writePlain(w, http.StatusServiceUnavailable, reason)
			return
		}
		writePlain(w, http.StatusOK, "ready")
	})
	return mux
}

// scrapeHandler serves one collect of reg in the Prometheus text format.
func scrapeHandler(reg *metric.Registry) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		var buf bytes.Buffer
		metric.WritePrometheus(&buf, reg.Collect())
		w.Header().Set("Content-Type", metric.PrometheusContentType)
		_, _ = w.Write(buf.Bytes()) // a failed write means the scraper left
	})
}

// requireBearer passes to next only requests whose Authorization header is
// "Bearer <token>". The comparison is of SHA-256 digests in constant time, so
// neither the token's bytes nor its length leak through timing.
func requireBearer(token string, next http.Handler) http.Handler {
	want := sha256.Sum256([]byte("Bearer " + token))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := sha256.Sum256([]byte(r.Header.Get("Authorization")))
		if subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writePlain(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// notReadyReason is the readiness condition (docs/specs/GATEWAY.md, Lifecycle): ""
// when ready, else why not.
func notReadyReason(holder *config.Holder, drain *Drain) string {
	if drain.Draining() {
		return "draining"
	}
	if !holder.Loaded() {
		return "config not loaded"
	}
	return ""
}

func writePlain(w http.ResponseWriter, status int, text string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, text+"\n") // a failed write means the prober left
}
