package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"kaiak/internal/config"
)

// checkWait bounds every wait on a model check the test expects to happen.
const checkWait = 5 * time.Second

type logBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	return len(p), nil
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}

// checkModel is a model on backends, one deployment each, named name@<backend ID>
// there.
func checkModel(name string, backends ...*config.Backend) *config.Model {
	m := &config.Model{Name: name}
	for _, b := range backends {
		m.Deployments = append(m.Deployments, config.Deployment{Backend: b, Model: name + "@" + b.ID})
	}
	return m
}

// checkSnapshot is a config of models over their backends.
func checkSnapshot(models ...*config.Model) *config.Snapshot {
	s := &config.Snapshot{Backends: map[string]*config.Backend{}, Models: map[string]*config.Model{}}
	for _, m := range models {
		s.Models[m.Name] = m
		for _, d := range m.Deployments {
			s.Backends[d.Backend.ID] = d.Backend
		}
	}
	return s
}

// At config apply every backend is probed once, in the background, and each
// deployment whose model it does not list is warned about.
func TestModelCheckWarnsPerMissingModel(t *testing.T) {
	var logs logBuffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	var mu sync.Mutex
	probed := map[string]int{}
	done := make(chan struct{}, 4)
	c := newModelChecker(func(_ context.Context, b *config.Backend) (func(string) bool, error) {
		defer func() { done <- struct{}{} }()
		mu.Lock()
		probed[b.ID]++
		mu.Unlock()
		switch b.ID {
		case "down":
			return nil, errors.New("connection refused")
		case "nopath":
			return nil, fmt.Errorf("probe: %w", &pathMissingError{backend: b.ID, url: b.BaseURL + "/models", hint: "base_url should end in /v1"})
		}
		return func(model string) bool { return model != "wrong@x" }, nil
	}, logger)
	x, y, down := &config.Backend{ID: "x"}, &config.Backend{ID: "y"}, &config.Backend{ID: "down"}
	nopath := &config.Backend{ID: "nopath", BaseURL: "http://vllm:8000"}
	ctx, stop := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		c.Run(ctx)
		close(stopped)
	}()
	c.Check(checkSnapshot(checkModel("right", x, y), checkModel("wrong", x), checkModel("other", down), checkModel("lost", nopath)))
	for range 4 {
		select {
		case <-done:
		case <-time.After(checkWait):
			t.Fatal("model check did not probe every backend")
		}
	}
	stop()
	<-stopped
	out := logs.String()
	if !strings.Contains(out, `msg="the backend does not list the deployment's model" kaiak.backend.id=x kaiak.deployment.model=wrong@x`) ||
		strings.Count(out, "does not list") != 1 {
		t.Errorf("log:\n%s\nwant one warning, for wrong@x", out)
	}
	if !strings.Contains(out, `level=WARN msg="model check skipped: the backend did not answer" kaiak.backend.id=down exception.message="connection refused"`) {
		t.Errorf("log:\n%s\nwant the unreachable backend named", out)
	}
	// A models list answering 404: the backend is up and its base_url likely wrong.
	if !strings.Contains(out, `level=WARN msg="the backend has no models list at its base_url" kaiak.backend.id=nopath kaiak.backend.base_url=http://vllm:8000 kaiak.backend.base_url_hint="base_url should end in /v1"`) ||
		strings.Contains(out, "skipped: the backend did not answer\" kaiak.backend.id=nopath") {
		t.Errorf("log:\n%s\nwant a warning naming nopath's base_url, with the hint", out)
	}
	if probed["x"] != 1 || probed["y"] != 1 || probed["down"] != 1 || probed["nopath"] != 1 {
		t.Errorf("probes %v, want one per backend", probed)
	}
}

// A backend whose probe answers but cannot tell which models it serves (the Azure
// types) gets an info line saying the check is not available for it, and no warning
// about its models; an azure-openai backend whose models list is not at its base_url
// is still warned about.
func TestModelCheckSaysWhenTheBackendCannotTell(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/openai/v1/models" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"gpt-4o"}]}`)
	}))
	defer srv.Close()
	backend := func(id string, typ config.BackendType, baseURL string) *config.Backend {
		return &config.Backend{ID: id, Type: typ, BaseURL: baseURL, ConnectTimeout: time.Second}
	}
	azure := backend("azure", config.BackendAzureOpenAI, srv.URL)
	wrongPath := backend("wrongpath", config.BackendAzureOpenAI, srv.URL+"/v1")
	foundry := backend("foundry", config.BackendAzureAnthropic, srv.URL)
	var logs logBuffer
	c := NewModelChecker(NewRegistry(func(string) (string, bool) { return "", false }), slog.New(slog.NewTextHandler(&logs, nil)))
	c.check(context.Background(), checkSnapshot(checkModel("a", azure, wrongPath), checkModel("b", foundry)))

	out := logs.String()
	for _, want := range []string{
		`level=INFO msg="model check not available for this backend type" kaiak.backend.id=azure kaiak.backend.type=azure-openai`,
		`level=INFO msg="model check not available for this backend type" kaiak.backend.id=foundry kaiak.backend.type=azure-anthropic`,
		`level=WARN msg="the backend has no models list at its base_url" kaiak.backend.id=wrongpath kaiak.backend.base_url=` +
			srv.URL + `/v1 kaiak.backend.base_url_hint="` + azurePathHint + `"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log:\n%s\nwant %s", out, want)
		}
	}
	if strings.Count(out, "level=WARN") != 1 || strings.Count(out, "not available") != 2 {
		t.Errorf("log:\n%s\nwant two info lines and the one warning, for wrongpath", out)
	}
}
