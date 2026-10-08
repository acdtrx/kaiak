package main

import (
	"os"
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
	got := servingStatus(map[string]int{"vllm-a": 2, "retired": 1}, map[string]int{"qwen3-32b": 3, "retired-model": 1},
		map[routing.DeploymentID]routing.CircuitReport{
			{Backend: "vllm-b", Model: "BAAI/bge-m3"}:    {State: routing.CircuitOpen, OpenedAt: openedAt},
			{Backend: "vllm-b", Model: "Qwen/Qwen3-32B"}: {State: routing.CircuitHalfOpen, OpenedAt: openedAt},
		}, s)

	if len(got.Backends) != 4 || len(got.Models) != 5 {
		t.Fatalf("backends %v, models %v: want the config's 3 backends plus the retired one, and its 4 models plus the retired one", got.Backends, got.Models)
	}
	a := got.Backends["vllm-a"]
	if a.InFlight != 2 || a.MaxInFlight != 8 || len(a.Deployments) != 1 || a.Deployments["Qwen/Qwen3-32B"].Circuit != control.CircuitClosed {
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

	none := servingStatus(map[string]int{}, map[string]int{}, nil, nil)
	if none.Backends == nil || none.Models == nil || len(none.Backends)+len(none.Models) != 0 {
		t.Errorf("before a config: %+v, want empty collections", none)
	}
}
