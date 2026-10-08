package server

import (
	"net/http"
	"slices"
	"testing"
	"time"

	"kaiak/internal/fakebackend"
	"kaiak/internal/provider"
)

// rememberedBackends lists the backends m remembers lacking an endpoint, sorted.
func rememberedBackends(m *MissingEndpoints) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var backends []string
	for key := range m.until {
		backends = append(backends, key.backend)
	}
	slices.Sort(backends)
	return backends
}

// An applied config that no longer has a backend forgets what the missing-endpoint
// memory holds for it: no request visits a removed backend's entry again, so its
// expiry would never remove it. A kept backend's entry
// stays.
func TestAppliedConfigForgetsARemovedBackendsMissingEndpoints(t *testing.T) {
	g := newTestGateway(t)
	withResponsesModels(t, g)
	g.backend.SetReply(fakebackend.Reply{Status: http.StatusNotFound,
		Body: `{"error":{"message":"File Not Found","type":"not_found_error","code":404}}`})
	w := do(t, g.h, call{method: "POST", path: "/v1/responses", key: workloadKey, body: responsesBody})
	expectError(t, w, http.StatusBadGateway, "upstream_endpoint_missing")
	g.missing.remember("local", provider.Responses, time.Now(), time.Hour)
	if got := rememberedBackends(g.missing); !slices.Equal(got, []string{"local", "ls"}) {
		t.Fatalf("remembered %v, want local and ls", got)
	}

	g.apply(t, nil) // the test config: no backend ls
	if got := rememberedBackends(g.missing); !slices.Equal(got, []string{"local"}) {
		t.Errorf("remembered %v after the reload, want local alone", got)
	}
}
