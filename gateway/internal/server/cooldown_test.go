package server

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"kaiak/internal/fakebackend"
	"kaiak/internal/routing"
)

// The reviewers' scenario (D3; the independent audit's finding 3): deployment A
// answers 429 with Retry-After: 60 at once, B is healthy with one long stream
// holding a slot. Least-in-flight alone prefers A every time and the retry budget
// runs out on the spill-over; with the cooldown, A takes the first attempt only and
// every request is served by B.
func TestThrottledDeploymentCoolsDown(t *testing.T) {
	g, other := newRetryGateway(t, "local", "local-b", nil)
	g.backend.SetReply(fakebackend.Reply{Status: http.StatusTooManyRequests, Header: map[string]string{"Retry-After": "60"}})
	m := g.holder.Current().Models["retry"]
	held, _, err := g.router.Acquire(context.Background(), m,
		routing.Avoid{Refused: []routing.DeploymentID{{Backend: "local", Model: "first"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()

	served := 0
	for i := range 100 {
		if w := post(t, g, "r"+strconv.Itoa(i), `{"model":"retry"}`); w.Code == http.StatusOK {
			served++
		}
	}
	throttled, healthy := len(g.backend.Requests()), len(other.Requests())
	t.Logf("throttled backend calls=%d healthy backend calls=%d served=%d", throttled, healthy, served)
	if throttled != 1 || healthy != 100 || served != 100 {
		t.Errorf("throttled %d, healthy %d, served %d; want 1, 100, 100", throttled, healthy, served)
	}
	expectMetricLines(t, scrape(g),
		`kaiak_deployment_cooling_down{backend="local",deployment_model="first"} 1`,
		`kaiak_deployment_cooling_down{backend="local-b",deployment_model="second"} 0`)
}

// A single-deployment model keeps its deployment through the cooldown: the client
// gets the backend's 429 as it is, every time.
func TestThrottledSingleDeploymentIsStillUsed(t *testing.T) {
	g := newTestGateway(t)
	g.backend.SetReply(fakebackend.Reply{Status: http.StatusTooManyRequests, Header: map[string]string{"Retry-After": "60"}})
	for i := range 2 {
		w := post(t, g, "r"+strconv.Itoa(i), `{"model":"open"}`)
		if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") != "60" {
			t.Errorf("request %d: %d Retry-After %q, want the backend's 429", i, w.Code, w.Header().Get("Retry-After"))
		}
	}
	if n := len(g.backend.Requests()); n != 2 {
		t.Errorf("backend got %d requests, want 2", n)
	}
}

// Every deployment of the model cooling down: the model is still served by them,
// and the client gets the 429 honestly.
func TestAllDeploymentsThrottledAreStillUsed(t *testing.T) {
	g, other := newRetryGateway(t, "local", "local-b", nil)
	throttle := fakebackend.Reply{Status: http.StatusTooManyRequests, Header: map[string]string{"Retry-After": "60"}}
	g.backend.SetReply(throttle)
	other.SetReply(throttle)
	// The first request tries both (one 429 each, a failover); both cool down.
	if w := post(t, g, "first", `{"model":"retry"}`); w.Code != http.StatusTooManyRequests {
		t.Fatalf("status %d, want 429", w.Code)
	}
	if line := logLine(t, g, "first"); !strings.Contains(line, `"kaiak.attempts":2,`) {
		t.Fatalf("log line: %s", line)
	}
	if w := post(t, g, "second", `{"model":"retry"}`); w.Code != http.StatusTooManyRequests {
		t.Errorf("status %d, want the backend's 429", w.Code)
	}
	if n := len(g.backend.Requests()) + len(other.Requests()); n != 4 {
		t.Errorf("backends got %d requests, want 4", n)
	}
}

// A cooldown ends when its Retry-After-Ms has passed: the deployment takes first
// attempts again.
func TestCooldownEnds(t *testing.T) {
	g, other := newRetryGateway(t, "local", "local-b", nil)
	g.backend.QueueReplies(fakebackend.Reply{Status: http.StatusTooManyRequests, Header: map[string]string{"Retry-After-Ms": "50"}})
	m := g.holder.Current().Models["retry"]
	held, _, err := g.router.Acquire(context.Background(), m,
		routing.Avoid{Refused: []routing.DeploymentID{{Backend: "local", Model: "first"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	post(t, g, "throttled", `{"model":"retry"}`)
	post(t, g, "cooling", `{"model":"retry"}`)
	if line := logLine(t, g, "cooling"); !strings.Contains(line, `"kaiak.backend.id":"local-b"`) || !strings.Contains(line, `"kaiak.attempts":1,`) {
		t.Errorf("during the cooldown: %s, want one attempt on local-b", line)
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(g.router.CoolingDown()) > 0 {
		if time.Now().After(deadline) {
			t.Fatal("the cooldown did not end")
		}
		time.Sleep(5 * time.Millisecond)
	}
	post(t, g, "after", `{"model":"retry"}`)
	if line := logLine(t, g, "after"); !strings.Contains(line, `"kaiak.backend.id":"local"`) || !strings.Contains(line, `"kaiak.attempts":1,`) {
		t.Errorf("after the cooldown: %s, want one attempt on local (fewer in flight)", line)
	}
	if n := len(other.Requests()); n != 2 {
		t.Errorf("local-b got %d requests, want 2", n)
	}
}

func TestThrottleCooldown(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name   string
		header map[string]string
		want   time.Duration
	}{
		{"no header", nil, 5 * time.Second},
		{"seconds", map[string]string{"Retry-After": "7"}, 7 * time.Second},
		{"seconds past the ceiling", map[string]string{"Retry-After": "3600"}, 60 * time.Second},
		{"zero", map[string]string{"Retry-After": "0"}, 0},
		{"an HTTP date", map[string]string{"Retry-After": now.Add(12 * time.Second).Format(http.TimeFormat)}, 12 * time.Second},
		{"a date past", map[string]string{"Retry-After": now.Add(-time.Minute).Format(http.TimeFormat)}, 0},
		{"milliseconds win", map[string]string{"Retry-After": "7", "Retry-After-Ms": "6500"}, 6500 * time.Millisecond},
		{"fractional milliseconds", map[string]string{"Retry-After-Ms": "12.5"}, 12500 * time.Microsecond},
		{"milliseconds past the ceiling", map[string]string{"Retry-After-Ms": "1e12"}, 60 * time.Second},
		{"unreadable milliseconds fall back", map[string]string{"Retry-After": "3", "Retry-After-Ms": "soon"}, 3 * time.Second},
		{"negative milliseconds fall back", map[string]string{"Retry-After-Ms": "-5"}, 5 * time.Second},
		{"unreadable", map[string]string{"Retry-After": "later"}, 5 * time.Second},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := http.Header{}
			for k, v := range c.header {
				h.Set(k, v)
			}
			if got := throttleCooldown(h, now); got != c.want {
				t.Errorf("cooldown %s, want %s", got, c.want)
			}
		})
	}
}
