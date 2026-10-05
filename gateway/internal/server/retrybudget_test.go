package server

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"kaiak/internal/fakebackend"
)

func TestRetryBudgetWindow(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	b := newRetryBudget(func() time.Time { return now })
	// Low traffic: the minimum allows retries even with few attempts.
	b.attempt("m")
	for i := range retryBudgetMin {
		if !b.allowRetry("m") {
			t.Fatalf("retry %d denied under the minimum", i+1)
		}
		b.retry("m")
	}
	if b.allowRetry("m") {
		t.Fatal("retry allowed past the minimum with few attempts")
	}
	// Models have budgets of their own.
	b.attempt("other")
	if !b.allowRetry("other") {
		t.Error("another model's retry denied")
	}
	// Busy: 20% of the window's attempts, retries included.
	for range 99 {
		b.attempt("m")
	}
	allowed := 0
	for b.allowRetry("m") {
		b.retry("m")
		allowed++
	}
	// 100 first attempts and r retries: allowed while r < int(20% of (100 + r)), up to 24.
	if total := retryBudgetMin + allowed; total != 24 {
		t.Errorf("%d retries allowed over 100 first attempts, want 24", total)
	}
	// The window slides: past it, the minimum again.
	now = now.Add(retryBudgetWindow)
	if !b.allowRetry("m") {
		t.Error("retry denied once the window slid past the spent budget")
	}
}

// A retry counts against the budget when it is sent, not when it is approved: one
// approved and then never sent (no slot, the client gone) spends nothing (N-P8).
func TestRetryBudgetCountsSentRetries(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	b := newRetryBudget(func() time.Time { return now })
	b.attempt("m")
	for i := range 2 * retryBudgetMin {
		if !b.allowRetry("m") {
			t.Fatalf("approval %d denied: approvals that were never sent spent the budget", i+1)
		}
	}
	for range retryBudgetMin {
		b.retry("m")
	}
	if b.allowRetry("m") {
		t.Error("retry allowed once the sent retries reached the minimum")
	}
}

// Past the model's retry budget a failed attempt is not retried: the request ends
// with its answer, and the log line says why (L1).
func TestRetryBudgetStopsRetries(t *testing.T) {
	g, other := newRetryGateway(t, "local", "local-b",
		withGlobal(`"circuit": { "failure_threshold": 1000, "probe_interval_ms": 3600000 }`))
	g.backend.SetReply(fakebackend.Reply{Status: http.StatusInternalServerError})
	other.SetReply(fakebackend.Reply{Status: http.StatusInternalServerError})
	sent := func() int { return len(g.backend.Requests()) + len(other.Requests()) }
	// Every attempt fails: 2 attempts (a failover to the other deployment) per
	// request until the minimum of retries is spent.
	for i := range retryBudgetMin {
		if w := post(t, g, "spend", `{"model":"retry"}`); w.Code != http.StatusInternalServerError {
			t.Fatalf("request %d: status %d", i, w.Code)
		}
	}
	if n := sent(); n != 2*retryBudgetMin {
		t.Fatalf("%d attempts spending the budget, want %d", n, 2*retryBudgetMin)
	}
	before := sent()
	w := post(t, g, "over", `{"model":"retry"}`)
	if w.Code != http.StatusInternalServerError || sent() != before+1 {
		t.Fatalf("over the budget: status %d, %d attempts; want the first attempt's 500 alone", w.Code, sent()-before)
	}
	if line := logLine(t, g, "over"); !strings.Contains(line, `"kaiak.attempts":1,"kaiak.retry_refused":"retry_budget"`) {
		t.Errorf("log line: %s", line)
	}
}
