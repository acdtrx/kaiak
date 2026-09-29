package e2e

// Routing reliability end to end: one public model with two deployments — backends
// "a" and "b", two fake backends serving the same model — through the built kaiak
// binary. Each scenario runs its own gateway and backends, so the round-robin tie
// order starts fresh: a model's first request goes to its first deployment ("a").

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"kaiak/internal/fakebackend"
)

// reliableModel is the model name on both backends.
const reliableModel = backendChatModel

// reliabilityConfig is a config with backends a and b and the public model "chat"
// deployed on both (a first); the circuit opens after 2 failures and probes every
// 100 ms. tune edits the document before it is written.
func reliabilityConfig(aURL, bURL, hash string, tune func(cfg map[string]any)) map[string]any {
	cfg := map[string]any{
		"format_version": 3,
		"global": map[string]any{
			"circuit": map[string]any{"failure_threshold": 2, "probe_interval_ms": 100},
		},
		"backends": map[string]any{
			"a": map[string]any{"type": "openai-compatible", "base_url": aURL + "/v1"},
			"b": map[string]any{"type": "openai-compatible", "base_url": bURL + "/v1"},
		},
		"models": map[string]any{"chat": reliableChatModel()},
		"groups": map[string]any{
			"t": map[string]any{},
			"w": map[string]any{"parent": "t", "allowed_models": []any{"*"}},
		},
		"keys": map[string]any{"k": map[string]any{"hash": hash, "group": "w"}},
	}
	if tune != nil {
		tune(cfg)
	}
	return cfg
}

// reliableChatModel is a chat model deployed on a, then b.
func reliableChatModel() map[string]any {
	return map[string]any{
		"deployments": []any{
			map[string]any{"backend": "a", "model": reliableModel},
			map[string]any{"backend": "b", "model": reliableModel},
		},
		"metadata": map[string]any{"context_length": 8192,
			"capabilities": map[string]any{"streaming": true, "tools": false, "vision": false, "reasoning": false}},
		"output_limit": map[string]any{"default": 64, "ceiling": 128},
	}
}

// startReliability writes the config and starts a gateway on it; it returns the
// gateway and the client key.
func startReliability(t *testing.T, aURL, bURL string, tune func(cfg map[string]any)) (*gateway, string) {
	t.Helper()
	dir := t.TempDir()
	key, hash := newKey()
	configFile := filepath.Join(dir, "config.json")
	writeJSON(t, configFile, reliabilityConfig(aURL, bURL, hash, tune))
	return startGateway(t, configFile, filepath.Join(dir, "data")), key
}

// tried is the log line's list of attempts for deployments on the given backends,
// each with its outcome: tried("a", "500", "b", "200").
func tried(pairs ...string) string {
	var parts []string
	for i := 0; i+1 < len(pairs); i += 2 {
		parts = append(parts, pairs[i]+"/"+reliableModel+":"+pairs[i+1])
	}
	return strings.Join(parts, ",")
}

// chatOK sends a non-streamed chat for model under id, requires 200 and returns the
// request's log line.
func chatOK(t *testing.T, g *gateway, key, id, model string) map[string]any {
	t.Helper()
	if r := g.post(t, "/v1/chat/completions", key, id, chatBody(model, false, nil)); r.StatusCode != http.StatusOK {
		t.Fatalf("%s: %d %s, want 200", id, r.StatusCode, r.body)
	}
	return g.settled(t, id)
}

// circuitSeries is the kaiak_circuit_open series of the model's deployment on backend.
func circuitSeries(backend string) string {
	return fmt.Sprintf(`kaiak_circuit_open{backend=%q,deployment_model=%q}`, backend, reliableModel)
}

// sumMetric adds the values of every series of the metric name carrying all of the
// labels (each `name="value"`).
func sumMetric(t *testing.T, g *gateway, name string, labels ...string) float64 {
	t.Helper()
	status, body := g.get(t, "/metrics", "")
	if status != http.StatusOK {
		t.Fatalf("/metrics = %d", status)
	}
	var sum float64
	for line := range strings.SplitSeq(string(body), "\n") {
		series, value, ok := strings.Cut(line, " ")
		if !ok || !strings.HasPrefix(series, name+"{") {
			continue
		}
		inner := strings.TrimSuffix(strings.TrimPrefix(series, name+"{"), "}")
		have := make(map[string]bool)
		for l := range strings.SplitSeq(inner, ",") {
			have[l] = true
		}
		matches := true
		for _, l := range labels {
			matches = matches && have[l]
		}
		if !matches {
			continue
		}
		v, err := strconv.ParseFloat(value, 64)
		if err != nil {
			t.Fatalf("%s: %v", line, err)
		}
		sum += v
	}
	return sum
}

func TestRetryOnAnotherDeploymentAndCircuit(t *testing.T) {
	// a's port, closed: connections to it are refused until it listens again.
	down := fakebackend.New()
	aURL := down.URL()
	down.Close()
	b := fakebackend.New()
	defer b.Close()
	g, key := startReliability(t, aURL, b.URL(), nil)

	// Every request is served. Those whose first attempt went to a were retried on b,
	// until a's second failure opened its circuit.
	var n int
	for ; ; n++ {
		if n == 6 {
			t.Fatal("a's circuit did not open within 6 requests")
		}
		id := fmt.Sprintf("refused-%d", n)
		line := chatOK(t, g, key, id, "chat")
		switch line["attempts"] {
		case 1.0:
			if line["backend"] != "b" {
				t.Fatalf("%s: one attempt on %v, want b", id, line["backend"])
			}
		case 2.0:
			if line["backend"] != "b" || line["tried"] != tried("a", "upstream_unavailable", "b", "200") {
				t.Fatalf("%s: log line %v, want a refused then b", id, line)
			}
		default:
			t.Fatalf("%s: attempts %v", id, line["attempts"])
		}
		if v, ok := g.metricValue(t, circuitSeries("a")); ok && v == 1 {
			break
		}
	}
	opened := g.logs.wait(t, "a's circuit opening", msg("circuit opened", "backend", "a"))
	if opened["deployment_model"] != reliableModel || opened["failures"] != 2.0 {
		t.Errorf("circuit opened line %v, want deployment %s after 2 failures", opened, reliableModel)
	}
	if got := g.metric(t, circuitSeries("b")); got != 0 {
		t.Errorf("b's circuit = %v, want closed", got)
	}

	// With a's circuit open, requests go to b alone, at once.
	for i := range 2 {
		id := fmt.Sprintf("open-%d", i)
		if line := chatOK(t, g, key, id, "chat"); line["attempts"] != 1.0 || line["backend"] != "b" {
			t.Fatalf("%s with a's circuit open: log line %v, want one attempt on b", id, line)
		}
	}

	// a comes back on the same address: a probe makes its circuit half-open, and the
	// first request it takes (the trial) closes it.
	u, err := url.Parse(aURL)
	if err != nil {
		t.Fatal(err)
	}
	a, err := fakebackend.NewAt(u.Host)
	if err != nil {
		t.Fatalf("listening again on a's address: %v", err)
	}
	defer a.Close()
	a.SetModels(reliableModel)
	g.logs.wait(t, "a's circuit half-opening", msg("circuit half-open", "backend", "a", "trigger", "interval"))
	if got := g.metric(t, circuitSeries("a")); got != 0 {
		t.Errorf("a's kaiak_circuit_open after the probe = %v, want 0: half-open is not open", got)
	}
	halfOpen := fmt.Sprintf(`kaiak_circuit_half_open{backend="a",deployment_model=%q}`, reliableModel)
	if got := g.metric(t, halfOpen); got != 1 {
		t.Errorf("a's kaiak_circuit_half_open after the probe = %v, want 1 until the trial", got)
	}
	if len(a.ModelsRequests()) == 0 {
		t.Error("a's circuit half-opened without a models probe reaching it")
	}

	// Traffic returns to a: tied deployments take turns.
	for i := range 2 {
		id := fmt.Sprintf("back-%d", i)
		if line := chatOK(t, g, key, id, "chat"); line["attempts"] != 1.0 {
			t.Fatalf("%s: log line %v, want one attempt", id, line)
		}
	}
	if got := len(a.Requests()); got != 1 {
		t.Errorf("a served %d of the 2 requests after recovering, want 1", got)
	}
	g.logs.wait(t, "a's circuit closing", msg("circuit closed", "backend", "a", "trigger", "trial"))
	if got := g.metric(t, circuitSeries("a")) + g.metric(t, halfOpen); got != 0 {
		t.Errorf("a's circuit after the trial = %v, want closed", got)
	}
	if got := sumMetric(t, g, "kaiak_retries_total", `model="chat"`, `reason="unavailable"`); got < 1 {
		t.Errorf("retries for unavailable = %v, want ≥ 1", got)
	}
	g.stop(t)
}

func TestRetryOnBackendError(t *testing.T) {
	a, b := fakebackend.New(), fakebackend.New()
	defer a.Close()
	defer b.Close()
	g, key := startReliability(t, a.URL(), b.URL(), nil)

	a.QueueReplies(fakebackend.Reply{Status: http.StatusInternalServerError})
	line := chatOK(t, g, key, "err-500", "chat")
	if line["attempts"] != 2.0 || line["backend"] != "b" || line["tried"] != tried("a", "500", "b", "200") {
		t.Fatalf("log line %v, want a's 500 then b", line)
	}
	// a is healthy: its turn comes next, and one failure below the threshold left
	// its circuit closed.
	line = chatOK(t, g, key, "err-after", "chat")
	if line["attempts"] != 1.0 || line["backend"] != "a" {
		t.Fatalf("after the 500: log line %v, want one attempt on a", line)
	}
	if got := g.metric(t, circuitSeries("a")); got != 0 {
		t.Errorf("a's circuit = %v, want closed", got)
	}
	g.stop(t)
}

// Retries are failover only, and a 429 cools its deployment down (D3, D4): a's 429
// fails over to b, whose 500 answers (no deployment left — a is never retried);
// while a cools down, requests go to b alone.
func TestThrottledDeploymentFailsOverAndCoolsDown(t *testing.T) {
	a, b := fakebackend.New(), fakebackend.New()
	defer a.Close()
	defer b.Close()
	g, key := startReliability(t, a.URL(), b.URL(), nil)

	a.QueueReplies(fakebackend.Reply{Status: http.StatusTooManyRequests, Header: map[string]string{"Retry-After": "60"}})
	b.QueueReplies(fakebackend.Reply{Status: http.StatusInternalServerError})
	r := g.post(t, "/v1/chat/completions", key, "busy", chatBody("chat", false, nil))
	if r.StatusCode != http.StatusInternalServerError || r.errorCode(t) != "upstream_error" {
		t.Fatalf("busy: %d %s, want b's 500 answered upstream_error", r.StatusCode, r.body)
	}
	line := g.settled(t, "busy")
	if line["attempts"] != 2.0 || line["tried"] != tried("a", "429", "b", "500") || line["retry_refused"] != "no_deployment_left" {
		t.Fatalf("log line %v, want a's 429, then b's 500 with no deployment left", line)
	}
	if got := g.metric(t, fmt.Sprintf(`kaiak_deployment_cooling_down{backend="a",deployment_model=%q}`, reliableModel)); got != 1 {
		t.Errorf("a cooling down = %v, want 1", got)
	}
	for i := range 2 {
		id := fmt.Sprintf("cooling-%d", i)
		if line := chatOK(t, g, key, id, "chat"); line["attempts"] != 1.0 || line["backend"] != "b" {
			t.Fatalf("%s while a cools down: log line %v, want one attempt on b", id, line)
		}
	}
	if got := len(a.Requests()); got != 1 {
		t.Errorf("a got %d requests, want 1", got)
	}
	if got := sumMetric(t, g, "kaiak_retries_total", `model="chat"`, `reason="rate_limited"`); got != 1 {
		t.Errorf("retries for rate_limited = %v, want 1", got)
	}
	g.stop(t)
}

func TestFirstEventTimeoutRetried(t *testing.T) {
	a, b := fakebackend.New(), fakebackend.New()
	defer a.Close()
	defer b.Close()
	g, key := startReliability(t, a.URL(), b.URL(), func(cfg map[string]any) {
		cfg["backends"].(map[string]any)["a"].(map[string]any)["first_event_timeout_ms"] = 300
	})

	a.QueueReplies(fakebackend.Reply{StallBeforeFirstByte: true})
	finishStream(t, openStream(t, g, key, "stalled", chatBody("chat", true, nil)))
	line := g.settled(t, "stalled")
	if line["attempts"] != 2.0 || line["backend"] != "b" || line["tried"] != tried("a", "upstream_timeout", "b", "200") {
		t.Fatalf("log line %v, want a's first-event timeout then b", line)
	}
	// The line sums the request's records: a's estimated input, b's 7 reported.
	if in, _ := line["tokens_in"].(float64); in <= 7 || line["tokens_out"] != 4.0 {
		t.Errorf("log line tokens in %v out %v, want a's estimate plus b's 7 in, b's 4 out", line["tokens_in"], line["tokens_out"])
	}
	select {
	case <-a.Requests()[0].Canceled():
	case <-time.After(waitLimit):
		t.Error("the timed-out attempt was not cancelled upstream")
	}
	// One client request, two usage records: a's partial estimate and b's answer.
	records := func(labels ...string) float64 {
		return sumMetric(t, g, "kaiak_usage_records_total", append([]string{`model="chat"`}, labels...)...)
	}
	if got := records(); got != 2 {
		t.Errorf("usage records = %v, want 2", got)
	}
	// Usage metrics carry no backend: a's record is the partial one, b's the
	// complete one; the attempts metric shows each on its backend.
	if got := records(`status="partial"`); got != 1 {
		t.Errorf("partial usage records = %v, want the timed-out attempt's", got)
	}
	if got := records(`status="complete"`); got != 1 {
		t.Errorf("complete usage records = %v, want the answer's", got)
	}
	for backend, outcome := range map[string]string{"a": "timeout", "b": "success"} {
		if got := sumMetric(t, g, "kaiak_upstream_attempts_total", `backend="`+backend+`"`, `outcome="`+outcome+`"`); got != 1 {
			t.Errorf("%s's %s attempts = %v, want 1", backend, outcome, got)
		}
	}
	if got := sumMetric(t, g, "kaiak_request_duration_seconds_count", `model="chat"`); got != 1 {
		t.Errorf("client requests = %v, want 1", got)
	}
	g.stop(t)
}

// A non-stream request past its response timeout is answered 504 at once: the
// backend was working on a long answer, so no other deployment is tried and the
// circuit is not told of a failure.
func TestResponseTimeoutNotRetried(t *testing.T) {
	a, b := fakebackend.New(), fakebackend.New()
	defer a.Close()
	defer b.Close()
	g, key := startReliability(t, a.URL(), b.URL(), func(cfg map[string]any) {
		for _, id := range []string{"a", "b"} {
			cfg["backends"].(map[string]any)[id].(map[string]any)["response_timeout_ms"] = 300
		}
	})

	a.SetReply(fakebackend.Reply{StallBeforeFirstByte: true})
	b.SetReply(fakebackend.Reply{StallBeforeFirstByte: true})
	r := g.post(t, "/v1/chat/completions", key, "long", chatBody("chat", false, nil))
	if r.StatusCode != http.StatusGatewayTimeout || !strings.Contains(string(r.body), `"upstream_timeout"`) {
		t.Fatalf("answer %d %s, want 504 upstream_timeout", r.StatusCode, r.body)
	}
	line := g.settled(t, "long")
	if line["attempts"] != 1.0 || line["estimated"] != true || line["partial"] != true {
		t.Errorf("log line %v, want one attempt billed its estimated input", line)
	}
	if got := len(a.Requests()) + len(b.Requests()); got != 1 {
		t.Errorf("backends got %d requests, want 1", got)
	}
	g.stop(t)
}

func TestQueueOnCappedBackends(t *testing.T) {
	a, b := fakebackend.New(), fakebackend.New()
	defer a.Close()
	defer b.Close()
	// One slot per backend; each model's queue holds one request. "chat" waits up to
	// 10 s (served when a slot frees), "chat-short" 300 ms (runs out).
	g, key := startReliability(t, a.URL(), b.URL(), func(cfg map[string]any) {
		for _, id := range []string{"a", "b"} {
			cfg["backends"].(map[string]any)[id].(map[string]any)["max_in_flight"] = 1
		}
		cfg["global"].(map[string]any)["queue"] = map[string]any{"size": 1, "timeout_ms": 10000}
		short := reliableChatModel()
		short["queue"] = map[string]any{"timeout_ms": 300}
		cfg["models"].(map[string]any)["chat-short"] = short
	})
	queued := func(model string) string { return fmt.Sprintf(`kaiak_queued_requests{model=%q}`, model) }

	// Two held streams take both slots.
	paceA, paceB := make(chan struct{}), make(chan struct{})
	a.SetReply(fakebackend.Reply{Pace: paceA})
	b.SetReply(fakebackend.Reply{Pace: paceB})
	streamA := openStream(t, g, key, "held-a", chatBody("chat", true, nil))
	streamB := openStream(t, g, key, "held-b", chatBody("chat", true, nil))

	t.Run("a request queues, the full queue refuses, a wait runs out", func(t *testing.T) {
		waiting := postAsync(g, key, "queued", "chat")
		g.waitMetric(t, "the request queued", queued("chat"), func(v float64) bool { return v == 1 })

		r := g.post(t, "/v1/chat/completions", key, "full", chatBody("chat", false, nil))
		if r.StatusCode != http.StatusTooManyRequests || r.errorCode(t) != "queue_full" || r.Header.Get("Retry-After") != "1" {
			t.Fatalf("with the queue full: %d %s (Retry-After %q), want 429 queue_full", r.StatusCode, r.body, r.Header.Get("Retry-After"))
		}
		r = g.post(t, "/v1/chat/completions", key, "timeout", chatBody("chat-short", false, nil))
		if r.StatusCode != http.StatusTooManyRequests || r.errorCode(t) != "queue_timeout" || r.Header.Get("Retry-After") != "" {
			t.Fatalf("past the queue timeout: %d %s, want 429 queue_timeout without Retry-After", r.StatusCode, r.body)
		}
		if line := g.settled(t, "timeout"); line["queue_wait_ms"] == nil {
			t.Errorf("timed-out request's log line has no queue_wait_ms: %v", line)
		}

		// a's stream ends: its slot goes to the waiting request.
		close(paceA)
		finishStream(t, streamA)
		res := await(t, waiting)
		if res.err != nil || res.status != http.StatusOK {
			t.Fatalf("queued request: %d %s %v, want 200 once a slot freed", res.status, res.body, res.err)
		}
		if line := g.settled(t, "queued"); line["backend"] != "a" || line["queue_wait_ms"] == nil {
			t.Errorf("queued request's log line %v, want served by a with queue_wait_ms", line)
		}
		for series, want := range map[string]float64{
			`kaiak_queue_rejections_total{model="chat",reason="full"}`:          1,
			`kaiak_queue_rejections_total{model="chat-short",reason="timeout"}`: 1,
			queued("chat"): 0,
		} {
			if got := g.metric(t, series); got != want {
				t.Errorf("%s = %v, want %v", series, got, want)
			}
		}
	})

	t.Run("the drain serves a queued request when a slot frees", func(t *testing.T) {
		paceA2 := make(chan struct{})
		a.SetReply(fakebackend.Reply{Pace: paceA2})
		streamA2 := openStream(t, g, key, "held-a2", chatBody("chat", true, nil))
		waiting := postAsync(g, key, "drain-queued", "chat")
		g.waitMetric(t, "the request queued", queued("chat"), func(v float64) bool { return v == 1 })

		g.signal(t, syscall.SIGTERM)
		g.logs.wait(t, "the drain refusing new requests", msg("draining: refusing new requests"))
		close(paceA2)
		finishStream(t, streamA2)
		res := await(t, waiting)
		if res.err != nil || res.status != http.StatusOK {
			t.Fatalf("request queued into the drain: %d %s %v, want 200", res.status, res.body, res.err)
		}
		close(paceB)
		finishStream(t, streamB)
		g.waitExit(t)
		if line := g.settled(t, "drain-queued"); line["queue_wait_ms"] == nil || line["backend"] != "a" {
			t.Errorf("drain-queued log line %v, want served by a after a wait", line)
		}
		g.logs.wait(t, "the drain end", func(e map[string]any) bool { return e["msg"] == "drained" && e["cut_off"] == nil })
	})
}

// openStream starts a streamed chat and returns once its first event arrived — the
// request holds its slot; the body stays open for finishStream.
func openStream(t *testing.T, g *gateway, key, id string, body map[string]any) *http.Response {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, g.api+"/v1/chat/completions", bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("X-Request-Id", id)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("stream %s: %d %s", id, resp.StatusCode, body)
	}
	br := bufio.NewReader(resp.Body)
	if first, err := br.ReadString('\n'); err != nil || !strings.HasPrefix(first, "data: ") {
		t.Fatalf("stream %s first event: %q %v", id, first, err)
	}
	resp.Body = struct {
		io.Reader
		io.Closer
	}{br, resp.Body}
	return resp
}

// finishStream reads a stream to its end, requires [DONE] and returns its event
// payloads.
func finishStream(t *testing.T, resp *http.Response) []string {
	t.Helper()
	rest, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("stream broke off: %v", err)
	}
	evs := events(rest)
	if len(evs) == 0 || evs[len(evs)-1] != "[DONE]" {
		t.Fatalf("stream did not end with [DONE]: %q", rest)
	}
	return evs
}

// asyncResult is a request sent in the background.
type asyncResult struct {
	status int
	body   []byte
	err    error
}

// postAsync sends a non-streamed chat in the background (no testing.T there): the
// result arrives on the channel.
func postAsync(g *gateway, key, id, model string) <-chan asyncResult {
	out := make(chan asyncResult, 1)
	go func() {
		data, _ := json.Marshal(chatBody(model, false, nil)) // a map of plain values always encodes
		req, err := http.NewRequest(http.MethodPost, g.api+"/v1/chat/completions", bytes.NewReader(data))
		if err != nil {
			out <- asyncResult{err: err}
			return
		}
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("X-Request-Id", id)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			out <- asyncResult{err: err}
			return
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		out <- asyncResult{status: resp.StatusCode, body: body, err: err}
	}()
	return out
}

// await returns the background request's result, waiting up to waitLimit.
func await(t *testing.T, ch <-chan asyncResult) asyncResult {
	t.Helper()
	select {
	case res := <-ch:
		return res
	case <-time.After(waitLimit):
		t.Fatalf("the background request did not end within %s", waitLimit)
		return asyncResult{}
	}
}
