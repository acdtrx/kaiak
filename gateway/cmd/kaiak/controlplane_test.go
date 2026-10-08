package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"kaiak/internal/config"
	"kaiak/internal/control"
	"kaiak/internal/fixturetest"
	"kaiak/internal/routing"
)

func TestServingStatusCoversTheAppliedConfig(t *testing.T) {
	data, err := os.ReadFile(fixturetest.Dir("config", "valid", "full.json"))
	if err != nil {
		t.Fatal(err)
	}
	s, err := config.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	openedAt := time.Date(2026, 9, 24, 12, 0, 0, 0, time.FixedZone("CEST", 2*60*60))
	// What an idle router serves against s, with live state set by hand: routing's
	// own tests cover how it comes about.
	serving := routing.New(routing.Options{}).Serving(s)
	a := serving.Backends["vllm-a"]
	a.InFlight = 2
	serving.Backends["vllm-a"] = a
	serving.Backends["retired"] = routing.BackendServing{InFlight: 1}
	serving.Models["qwen3-32b"] = routing.ModelServing{Queued: 3}
	serving.Models["retired-model"] = routing.ModelServing{Queued: 1}
	serving.Deployments[routing.DeploymentID{Backend: "vllm-b", Model: "BAAI/bge-m3"}] = routing.DeploymentServing{
		Circuit: routing.CircuitOpen, OpenedAt: openedAt}
	serving.Deployments[routing.DeploymentID{Backend: "vllm-b", Model: "Qwen/Qwen3-32B"}] = routing.DeploymentServing{
		Circuit: routing.CircuitHalfOpen, OpenedAt: openedAt}
	got := servingStatus(serving)

	if len(got.Backends) != 4 || len(got.Models) != 5 {
		t.Fatalf("backends %v, models %v: want the config's 3 backends plus the retired one, and its 4 models plus the retired one", got.Backends, got.Models)
	}
	if a := got.Backends["vllm-a"]; a.InFlight != 2 || a.MaxInFlight != 8 || len(a.Deployments) != 1 || a.Deployments["Qwen/Qwen3-32B"].Circuit != control.CircuitClosed {
		t.Errorf("vllm-a = %+v", a)
	}
	b := got.Backends["vllm-b"]
	if b.InFlight != 0 || b.MaxInFlight != 0 || len(b.Deployments) != 2 {
		t.Errorf("vllm-b = %+v, want idle, no cap, two deployments", b)
	}
	if d := b.Deployments["BAAI/bge-m3"]; d.Circuit != control.CircuitOpen || d.OpenedAt == nil ||
		!d.OpenedAt.Equal(openedAt) || d.OpenedAt.Location() != time.UTC {
		t.Errorf("vllm-b/BAAI/bge-m3 = %+v, want open since %s, in UTC", d, openedAt)
	}
	if d := b.Deployments["Qwen/Qwen3-32B"]; d.Circuit != control.CircuitHalfOpen || d.OpenedAt == nil || !d.OpenedAt.Equal(openedAt) {
		t.Errorf("vllm-b/Qwen/Qwen3-32B = %+v, want half-open since %s", d, openedAt)
	}
	if r := got.Backends["retired"]; r.InFlight != 1 || r.Deployments == nil || len(r.Deployments) != 0 {
		t.Errorf("retired = %+v, want its in-flight count and no deployments", r)
	}
	if m, ok := got.Models["qwen3-32b"]; !ok || m.Queued != 3 {
		t.Errorf("qwen3-32b = %+v, %v; want 3 queued", m, ok)
	}
	if m := got.Models["retired-model"]; m.Queued != 1 {
		t.Errorf("retired-model = %+v, want its 1 queued", m)
	}
	for name, m := range got.Models {
		if name != "qwen3-32b" && name != "retired-model" && m.Queued != 0 {
			t.Errorf("%s = %+v, want 0 queued", name, m)
		}
	}

	none := servingStatus(routing.New(routing.Options{}).Serving(nil))
	if none.Backends == nil || none.Models == nil || len(none.Backends)+len(none.Models) != 0 {
		t.Errorf("before a config: %+v, want empty collections", none)
	}
}

// A second stop signal that comes after the drain's deadline, while the usage flush
// is ending, still skips the final draining status (docs/specs/GATEWAY.md,
// Lifecycle → Draining): the hurry is cancelled as the flush logs its end, with the
// deadline already past.
func TestHurryAfterTheDrainDeadlineSkipsTheFinalStatus(t *testing.T) {
	hurry, cancel := context.WithCancel(context.Background())
	defer cancel()
	var flushed atomic.Bool
	logger := slog.New(onMessage{msg: "usage flushed", do: func() {
		flushed.Store(true)
		cancel()
	}})
	u, err := url.Parse("http://control.invalid")
	if err != nil {
		t.Fatal(err)
	}
	var statusRequests atomic.Int64
	client := control.New(control.Options{
		URL: u, Token: "test-token", Instance: "test-instance", Logger: logger,
		HTTPClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.URL.Path != "/v1/status" {
				t.Errorf("unexpected request: %s", req.URL.Path)
			}
			statusRequests.Add(1)
			return &http.Response{StatusCode: http.StatusNoContent, Header: http.Header{},
				Body: io.NopCloser(strings.NewReader(""))}, nil
		})},
	})
	client.SetDraining()
	cp := &controlPlane{client: client}
	cp.finish(hurry, time.Now().Add(-time.Second))
	if !flushed.Load() || hurry.Err() == nil {
		t.Fatal("the drain was not hurried at the end of its usage flush")
	}
	if n := statusRequests.Load(); n != 0 {
		t.Errorf("%d final status reports after the hurry, want none", n)
	}
}

// onMessage is a log handler that runs do when a record with message msg is logged,
// and drops every record.
type onMessage struct {
	msg string
	do  func()
}

func (h onMessage) Enabled(context.Context, slog.Level) bool { return true }
func (h onMessage) Handle(_ context.Context, r slog.Record) error {
	if r.Message == h.msg {
		h.do()
	}
	return nil
}
func (h onMessage) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h onMessage) WithGroup(string) slog.Handler      { return h }

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
