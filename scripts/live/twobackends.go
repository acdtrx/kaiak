package main

// The checks of a two-backend run (-base-url-2): both backends serve the same model,
// so every chat model has two deployments.

import (
	"fmt"
	"strings"
	"time"
)

// servedBy sends a short chat to the capped model (answers stay cheap), requires 200
// and returns the request's log line. A failure is reported under name.
func (r *run) servedBy(name, id string) (map[string]any, bool) {
	resp, err := r.post(r.key, "/v1/chat/completions", id, chatBody(modelCapped, false, nil))
	if !r.ok(name, resp, err, id) {
		return nil, false
	}
	line, err := r.gw.logs.wait(r.ctx, msg("request", "kaiak.request.id", id))
	if err != nil {
		r.fail(name, "no log line for request %s: %v", id, err)
		return nil, false
	}
	return line, true
}

// attempts is the log line's attempt count (0 when absent).
func attempts(line map[string]any) int {
	n, _ := line["kaiak.attempts"].(float64)
	return int(n)
}

// checkSpread: requests one after another go to both backends — tied deployments
// take turns.
func (r *run) checkSpread() {
	const name, n = "spread", 6
	served := map[string]int{}
	for i := range n {
		line, ok := r.servedBy(name, fmt.Sprintf("live-spread-%d", i))
		if !ok {
			return
		}
		served[fmt.Sprint(line["kaiak.backend.id"])]++
	}
	detail := fmt.Sprintf("%d requests: %s served %d, %s served %d", n, backendFirst, served[backendFirst],
		backendSecond, served[backendSecond])
	if served[backendFirst] == 0 || served[backendSecond] == 0 {
		r.fail(name, "%s; want both", detail)
		return
	}
	r.pass(name, detail)
}

// checkCapacity sends twice the two backends' slots and two more at once: every
// request is answered (those over the cap wait in the gateway's queue), and the
// metrics show each backend's cap.
func (r *run) checkCapacity() {
	const name = "capacity"
	n := 2*r.o.maxInFlight + 2
	type result struct {
		resp *response
		err  error
	}
	results := make([]chan result, n)
	for i := range n {
		results[i] = make(chan result, 1)
		go func() {
			resp, err := r.post(r.key, "/v1/chat/completions", fmt.Sprintf("live-capacity-%d", i), chatBody(modelCapped, false, nil))
			results[i] <- result{resp, err}
		}()
	}
	failed := false
	for i, ch := range results {
		res := <-ch
		if !failed && !r.ok(name, res.resp, res.err, fmt.Sprintf("live-capacity-%d", i)) {
			failed = true
		}
	}
	if failed {
		return
	}
	queued := 0
	served := map[string]int{}
	for i := range n {
		line, err := r.gw.logs.wait(r.ctx, msg("request", "kaiak.request.id", fmt.Sprintf("live-capacity-%d", i)))
		if err != nil {
			r.fail(name, "no log line for request live-capacity-%d: %v", i, err)
			return
		}
		if _, ok := line["kaiak.queue.wait_duration"]; ok {
			queued++
		}
		served[fmt.Sprint(line["kaiak.backend.id"])]++
	}
	text, ok := r.metricsText(name)
	if !ok {
		return
	}
	for _, b := range []string{backendFirst, backendSecond} {
		if got := sumSeries(text, `kaiak_backend_max_in_flight{backend="`+b+`"}`); got != float64(r.o.maxInFlight) {
			r.fail(name, "kaiak_backend_max_in_flight for %s = %v, want %d", b, got, r.o.maxInFlight)
			return
		}
	}
	r.pass(name, fmt.Sprintf("%d at once over 2×%d slots: all 200, %d queued; %s served %d, %s served %d", n,
		r.o.maxInFlight, queued, backendFirst, served[backendFirst], backendSecond, served[backendSecond]))
}

// checkFailover: with the second backend stopped, every request is still answered
// (retried on the first) until the second's circuit opens and it is left out; once
// it is back, a probe makes its circuit half-open, the first request it takes (the
// trial) closes it, and requests reach it again. In a live run the user stops and
// starts the backend when asked.
func (r *run) checkFailover() {
	const name = "failover"
	if r.o.stopSecond != nil {
		if err := r.o.stopSecond(); err != nil {
			r.fail(name, "stopping the second backend: %v", err)
			return
		}
	} else {
		r.ask(fmt.Sprintf("Stop the second backend (%s) now, e.g. Ctrl-C its server process. "+
			"A request goes every %s; waiting up to %s for its circuit to open.", r.o.baseURL2, r.o.failoverPace, r.o.failoverWait))
	}

	start := time.Now()
	deadline := time.NewTimer(r.o.failoverWait)
	defer deadline.Stop()
	pace := time.NewTicker(r.o.failoverPace)
	defer pace.Stop()
	opened := msg("circuit opened", "kaiak.backend.id", backendSecond)
	served, retried := 0, 0
	for i := 0; ; i++ {
		if _, ok := r.gw.logs.find(opened); ok {
			break
		}
		line, ok := r.servedBy(name, fmt.Sprintf("live-failover-%d", i))
		if !ok {
			return
		}
		served++
		if attempts(line) > 1 {
			retried++
		}
		select {
		case <-pace.C:
		case <-deadline.C:
			r.fail(name, "%s's circuit did not open within %s (%d requests served, %d retried): was the backend stopped?",
				backendSecond, r.o.failoverWait, served, retried)
			return
		case <-r.ctx.Done():
			r.fail(name, "%v", r.ctx.Err())
			return
		}
	}
	openedAfter := time.Since(start).Round(100 * time.Millisecond)
	text, ok := r.metricsText(name)
	if !ok {
		return
	}
	if got := sumSeries(text, "kaiak_circuit_open{", `backend="`+backendSecond+`"`); got != 1 {
		r.fail(name, "kaiak_circuit_open for %s = %v after the circuit opened, want 1", backendSecond, got)
		return
	}
	for i := range 2 {
		line, ok := r.servedBy(name, fmt.Sprintf("live-failover-open-%d", i))
		if !ok {
			return
		}
		if line["kaiak.backend.id"] != backendFirst || attempts(line) != 1 {
			r.fail(name, "with %s's circuit open a request went to %v in %d attempts, want %s in 1",
				backendSecond, line["kaiak.backend.id"], attempts(line), backendFirst)
			return
		}
	}

	if r.o.startSecond != nil {
		if err := r.o.startSecond(); err != nil {
			r.fail(name, "starting the second backend again: %v", err)
			return
		}
	} else {
		r.ask(fmt.Sprintf("Start the second backend (%s) again. Waiting up to %s for a probe to half-open its circuit.",
			r.o.baseURL2, r.o.failoverWait))
	}
	start = time.Now()
	if _, err := r.gw.logs.waitWithin(r.ctx, r.o.failoverWait, msg("circuit half-open", "kaiak.backend.id", backendSecond)); err != nil {
		r.fail(name, "%s's circuit did not go half-open within %s: %v", backendSecond, r.o.failoverWait, err)
		return
	}
	halfOpenAfter := time.Since(start).Round(100 * time.Millisecond)
	// Tied deployments take turns: within a few requests one reaches the second
	// again — the trial, whose success closes the circuit.
	for i := range 4 {
		line, ok := r.servedBy(name, fmt.Sprintf("live-failover-back-%d", i))
		if !ok {
			return
		}
		if line["kaiak.backend.id"] != backendSecond {
			continue
		}
		if _, ok := r.gw.logs.find(msg("circuit closed", "kaiak.backend.id", backendSecond, "kaiak.trigger", "trial")); !ok {
			r.fail(name, "%s served a request after its probe, but its circuit did not close", backendSecond)
			return
		}
		r.pass(name, fmt.Sprintf("%d served while %s was down (%d retried); circuit opened after %s, half-open by a probe %s after the restart began, closed by its trial, traffic back",
			served, backendSecond, retried, openedAfter, halfOpenAfter))
		return
	}
	r.fail(name, "no request reached %s after its circuit went half-open", backendSecond)
}

// ask prints an instruction for the person running the check.
func (r *run) ask(text string) {
	fmt.Printf("  >>>>  %s\n", strings.TrimSpace(text))
}

// metricsText reads the admin /metrics, failing the check name when it cannot.
func (r *run) metricsText(name string) (string, bool) {
	status, body, err := httpGet(r.ctx, r.client, r.gw.admin+"/metrics", "")
	if err != nil || status != 200 {
		r.fail(name, "GET /metrics: %d %v", status, err)
		return "", false
	}
	return string(body), true
}
