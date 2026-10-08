package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"kaiak/internal/metrics"
)

// With a metrics token set, /metrics answers only a request bearing it; the
// probes stay open.
func TestMetricsTokenGuardsMetricsOnly(t *testing.T) {
	g := newTestGateway(t)
	const token = "scrape-secret"
	h := NewAdmin(g.holder, NewDrain(), g.metrics, token)
	get := func(path, authorization string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", path, nil)
		if authorization != "" {
			r.Header.Set("Authorization", authorization)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	for _, auth := range []string{"", "Bearer wrong", "Bearer " + token + "x", "Basic " + token, token} {
		w := get("/metrics", auth)
		if w.Code != http.StatusUnauthorized || w.Header().Get("WWW-Authenticate") != "Bearer" {
			t.Errorf("Authorization %q: status %d, WWW-Authenticate %q; want 401 Bearer", auth, w.Code, w.Header().Get("WWW-Authenticate"))
		}
		if strings.Contains(w.Body.String(), "# TYPE") || strings.Contains(w.Body.String(), token) {
			t.Errorf("Authorization %q: body %q", auth, w.Body.String())
		}
	}
	if w := get("/metrics", "Bearer "+token); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "# TYPE kaiak_build_info") {
		t.Errorf("with the token: status %d", w.Code)
	}
	for _, probe := range []string{"/healthz", "/readyz"} {
		if w := get(probe, ""); w.Code != http.StatusOK {
			t.Errorf("%s without a token: status %d, want 200", probe, w.Code)
		}
	}
	// No token set: /metrics is open.
	open := NewAdmin(g.holder, NewDrain(), metrics.NewRegistry(), "")
	w := httptest.NewRecorder()
	open.ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	if w.Code != http.StatusOK {
		t.Errorf("no token set: status %d, want 200", w.Code)
	}
}
