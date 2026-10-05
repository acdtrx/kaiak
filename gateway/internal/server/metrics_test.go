package server

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"kaiak/internal/config"
	"kaiak/internal/fakebackend"
	"kaiak/internal/metrics"
)

func (g *testGateway) metricsText() string {
	var buf bytes.Buffer
	g.metrics.WriteText(&buf)
	return buf.String()
}

func expectMetricLines(t *testing.T, text string, lines ...string) {
	t.Helper()
	for _, want := range lines {
		if !strings.Contains(text, want+"\n") {
			t.Errorf("metrics miss %q", want)
		}
	}
	if t.Failed() {
		t.Logf("metrics:\n%s", text)
	}
}

func TestOpsAndUsageMetricsMoveWithRequests(t *testing.T) {
	g := newTestGateway(t)
	withLimits(t, g, "", `[{ "type": "requests_per_minute", "value": 2 }]`)
	g.backend.SetReply(fakebackend.Reply{Usage: &fakebackend.Usage{
		PromptTokens: 100, CachedTokens: 40, CacheWriteTokens: 20, CompletionTokens: 10, ReasoningTokens: 4}})
	chat := func(key, model string) *httptest.ResponseRecorder {
		return do(t, g.h, call{method: "POST", path: "/v1/chat/completions", key: key,
			body: `{"model":"` + model + `","messages":[]}`})
	}

	if w := chat(workloadKey, "pair"); w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	expectError(t, chat(workloadKey, "down"), http.StatusBadGateway, "upstream_unavailable")
	expectError(t, chat(workloadKey, "pair"), http.StatusTooManyRequests, "rate_limit_exceeded")
	g.backend.SetReply(fakebackend.Reply{Status: http.StatusInternalServerError})
	if w := chat(userKey, "open"); w.Code != http.StatusInternalServerError {
		t.Fatalf("backend error status: %d", w.Code)
	}
	expectError(t, chat(userKey, "nope-model"), http.StatusNotFound, "model_not_found")
	expectError(t, do(t, g.h, call{method: "GET", path: "/metrics"}), http.StatusNotFound, "unknown_url")

	records := g.usage.all()
	if len(records) != 3 {
		t.Fatalf("%d usage records, want 3 (pair, down, open)", len(records))
	}
	pair := records[0]
	wl := `key_group="eval",root_group="research",key_id="k-eval",model="pair",status="complete"`
	usageLine := func(name, labels string, value int64) string {
		return name + "{" + labels + "} " + strconv.FormatInt(value, 10)
	}
	text := g.metricsText()
	expectMetricLines(t, text,
		`kaiak_request_duration_seconds_count{endpoint="chat_completions",model="pair",status_class="2xx"} 1`,
		`kaiak_request_duration_seconds_count{endpoint="chat_completions",model="pair",status_class="4xx"} 1`,
		`kaiak_request_duration_seconds_count{endpoint="chat_completions",model="down",status_class="5xx"} 1`,
		`kaiak_request_duration_seconds_count{endpoint="chat_completions",model="open",status_class="5xx"} 1`,
		// A model the caller may not use (or that does not exist) is never a label value.
		`kaiak_request_duration_seconds_count{endpoint="chat_completions",status_class="4xx"} 1`,
		`kaiak_request_duration_seconds_count{status_class="4xx"} 1`,
		`kaiak_errors_total{class="rate_limited"} 1`,
		`kaiak_errors_total{class="upstream_unavailable"} 1`,
		`kaiak_errors_total{class="upstream_error"} 1`,
		`kaiak_errors_total{class="not_found"} 2`,
		`kaiak_errors_total{class="auth"} 0`,
		// Usage metrics carry the record's own numbers.
		usageLine("kaiak_usage_records_total", wl, 1),
		usageLine("kaiak_usage_tokens_total", wl+`,unit="tokens_in"`, pair.Units[config.UnitTokensIn]),
		usageLine("kaiak_usage_tokens_total", wl+`,unit="tokens_cached"`, pair.Units[config.UnitTokensCached]),
		usageLine("kaiak_usage_tokens_total", wl+`,unit="tokens_cache_write"`, pair.Units[config.UnitTokensCacheWrite]),
		usageLine("kaiak_usage_tokens_total", wl+`,unit="tokens_out"`, pair.Units[config.UnitTokensOut]),
		usageLine("kaiak_usage_tokens_total", wl+`,unit="tokens_reasoning"`, pair.Units[config.UnitTokensReasoning]),
		`kaiak_usage_cost_usd_total{`+wl+`} `+strconv.FormatFloat(float64(pair.CostNanoUSD)/1e9, 'g', -1, 64),
		`kaiak_usage_records_total{key_group="eval",root_group="research",key_id="k-eval",model="down",status="partial"} 1`,
		`kaiak_usage_records_total{key_group="ann",root_group="users",key_id="k-ann",model="open",status="complete"} 1`,
		`kaiak_backend_in_flight_requests{backend="local"} 0`,
	)
	if pair.CostNanoUSD != 120_000 || pair.Units[config.UnitTokensIn] != 40 || pair.Units[config.UnitTokensCacheWrite] != 20 {
		t.Errorf("unexpected record %+v", pair)
	}
	if strings.Contains(text, "nope-model") {
		t.Error("an unknown model name became a label value")
	}
	for _, secret := range []string{workloadKey, userKey, localBackendKey, azureBackendKey} {
		if strings.Contains(text, secret) {
			t.Errorf("metrics carry a secret %q", secret)
		}
	}
}

func TestStreamMetricsTimeToFirstTokenAndDecodeRate(t *testing.T) {
	g := newTestGateway(t)
	w := do(t, g.h, call{method: "POST", path: "/v1/chat/completions", key: userKey,
		body: `{"model":"open","stream":true,"messages":[]}`})
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	non := do(t, g.h, call{method: "POST", path: "/v1/chat/completions", key: userKey, body: chatBody})
	if non.Code != http.StatusOK {
		t.Fatalf("status %d", non.Code)
	}
	// Only the stream is timed: a non-stream answer has no first token to see.
	expectMetricLines(t, g.metricsText(),
		`kaiak_time_to_first_token_seconds_count{model="open",backend="local"} 1`,
		`kaiak_output_tokens_per_second_count{model="open",backend="local"} 1`,
		`kaiak_request_duration_seconds_count{endpoint="chat_completions",model="open",status_class="2xx"} 2`,
	)
}

// Every request that ended in an error is counted by its key, model and the log
// line's error code — refusals before routing, routed failures and relayed backend
// errors alike; a model only once it passed the access check, no key labels without
// a valid key.
func TestRequestErrorsByKeyAndCode(t *testing.T) {
	g := newTestGateway(t)
	withLimits(t, g, "", `[{ "type": "requests_per_minute", "value": 1 }]`)
	chat := func(key, model string) *httptest.ResponseRecorder {
		return do(t, g.h, call{method: "POST", path: "/v1/chat/completions", key: key,
			body: `{"model":"` + model + `","messages":[]}`})
	}
	if w := chat(workloadKey, "pair"); w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	expectError(t, chat(workloadKey, "pair"), http.StatusTooManyRequests, "rate_limit_exceeded")
	expectError(t, chat(workloadKey, "pair"), http.StatusTooManyRequests, "rate_limit_exceeded")
	expectError(t, chat(userKey, "nope-model"), http.StatusNotFound, "model_not_found")
	expectError(t, chat("kaiak-not-a-key", "pair"), http.StatusUnauthorized, "invalid_api_key")
	expectError(t, do(t, g.h, call{method: "GET", path: "/nowhere"}), http.StatusNotFound, "unknown_url")

	text := g.metricsText()
	expectMetricLines(t, text,
		`kaiak_request_errors_total{key_group="eval",root_group="research",key_id="k-eval",model="pair",code="rate_limit_exceeded"} 2`,
		`kaiak_request_errors_total{key_group="ann",root_group="users",key_id="k-ann",code="model_not_found"} 1`,
		`kaiak_request_errors_total{code="invalid_api_key"} 1`,
		`kaiak_request_errors_total{code="unknown_url"} 1`,
	)
	if strings.Contains(text, `model="nope-model"`) {
		t.Errorf("a model name the config does not grant is a label:\n%s", text)
	}
	if n := strings.Count(text, "kaiak_request_errors_total{"); n != 4 {
		t.Errorf("%d request-error series, want 4 (the success counts none):\n%s", n, text)
	}
}

// A relayed backend error status counts under its class, as the log line's
// error.type names it; the label switches apply as for the usage metrics.
func TestRequestErrorsFollowTheLabelSwitches(t *testing.T) {
	g := newTestGateway(t)
	g.holder.Swap(testSnapshotWith(t, g.backend.URL(), func(doc string) string {
		return strings.Replace(doc, `"max_request_body_bytes": 1024 }`,
			`"max_request_body_bytes": 1024, "metrics": { "key_id_label": false, "group_label": false } }`, 1)
	}))
	g.backend.SetReply(fakebackend.Reply{Status: http.StatusInternalServerError})
	if w := do(t, g.h, call{method: "POST", path: "/v1/chat/completions", key: userKey, body: chatBody}); w.Code != http.StatusInternalServerError {
		t.Fatalf("status %d", w.Code)
	}
	expectMetricLines(t, g.metricsText(),
		`kaiak_request_errors_total{root_group="users",model="open",code="upstream_error"} 1`)
}

func TestKeyIDLabelSwitchedOff(t *testing.T) {
	g := newTestGateway(t)
	g.holder.Swap(testSnapshotWith(t, g.backend.URL(), func(doc string) string {
		return strings.Replace(doc, `"max_request_body_bytes": 1024 }`,
			`"max_request_body_bytes": 1024, "metrics": { "key_id_label": false } }`, 1)
	}))
	if w := do(t, g.h, call{method: "POST", path: "/v1/chat/completions", key: userKey, body: chatBody}); w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	text := g.metricsText()
	expectMetricLines(t, text, `kaiak_usage_records_total{key_group="ann",root_group="users",model="open",status="complete"} 1`)
	if strings.Contains(text, "key_id=") {
		t.Errorf("key_id label present while switched off:\n%s", text)
	}
	if r := onlyRecord(t, g); r.KeyID != "k-ann" {
		t.Errorf("the usage record lost its key ID: %+v", r)
	}
}

// group_label off: the key's group leaves the usage series, its top-level group stays;
// the record keeps the whole path.
func TestGroupLabelSwitchedOff(t *testing.T) {
	g := newTestGateway(t)
	g.holder.Swap(testSnapshotWith(t, g.backend.URL(), func(doc string) string {
		return strings.Replace(doc, `"max_request_body_bytes": 1024 }`,
			`"max_request_body_bytes": 1024, "metrics": { "group_label": false } }`, 1)
	}))
	if w := do(t, g.h, call{method: "POST", path: "/v1/chat/completions", key: userKey, body: chatBody}); w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	text := g.metricsText()
	expectMetricLines(t, text, `kaiak_usage_records_total{root_group="users",key_id="k-ann",model="open",status="complete"} 1`)
	if strings.Contains(text, `key_group=`) {
		t.Errorf("key_group label present while switched off:\n%s", text)
	}
	if r := onlyRecord(t, g); !slices.Equal(r.Groups, []string{"users", "ann"}) {
		t.Errorf("the usage record lost its groups: %+v", r)
	}
}

func TestMetricsServedOnAdminOnlyAndWellFormed(t *testing.T) {
	g := newTestGateway(t)
	api := httptest.NewServer(g.h)
	defer api.Close()
	admin := httptest.NewServer(NewAdmin(g.holder, NewDrain(), g.metrics, ""))
	defer admin.Close()

	post := func(key, body string) {
		t.Helper()
		req, _ := http.NewRequest("POST", api.URL+"/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	post(workloadKey, `{"model":"pair","messages":[]}`)
	settledRecord(t, g)
	post(userKey, `{"model":"open","stream":true,"messages":[]}`)
	settledRecord(t, g)
	post("wrong", chatBody)

	resp, err := http.Get(api.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("API port serves /metrics: %d", resp.StatusCode)
	}

	resp, err = http.Get(admin.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin /metrics: %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/plain; version=0.0.4; charset=utf-8" {
		t.Errorf("Content-Type %q", ct)
	}
	validateExposition(t, string(body))
	for _, family := range []string{"kaiak_build_info", "kaiak_request_duration_seconds", "kaiak_time_to_first_token_seconds",
		"kaiak_output_tokens_per_second", "kaiak_errors_total", "kaiak_backend_in_flight_requests",
		"kaiak_config_loads_total", "kaiak_config_last_applied_timestamp_seconds", "kaiak_usage_records_total",
		"kaiak_usage_tokens_total", "kaiak_usage_cost_usd_total", "kaiak_backend_max_in_flight", "kaiak_queued_requests",
		"kaiak_queue_wait_seconds", "kaiak_queue_rejections_total", "kaiak_retries_total", "kaiak_request_attempts",
		"kaiak_circuit_open", "kaiak_circuit_half_open", "kaiak_circuit_transitions_total", "kaiak_probes_total",
		"kaiak_upstream_attempts_total", "kaiak_upstream_attempt_duration_seconds"} {
		if !strings.Contains(string(body), "# TYPE "+family+" ") {
			t.Errorf("family %s missing", family)
		}
	}
	// Usage metrics count records, not requests (a retried request can have several).
	if strings.Contains(string(body), "kaiak_usage_requests_total") {
		t.Error("kaiak_usage_requests_total is exposed; usage records are kaiak_usage_records_total")
	}
}

func TestMetricsUnderConcurrentRequests(t *testing.T) {
	g := newTestGateway(t)
	const n = 24
	// All n from one key at once: above the default per-key limit of 16.
	withKeyLimit(t, g, n, nil)
	stop := make(chan struct{})
	var scraper sync.WaitGroup
	scraper.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
				_ = g.metricsText()
			}
		}
	})
	var wg sync.WaitGroup
	for i := range n {
		body := chatBody
		if i%2 == 1 {
			body = `{"model":"open","stream":true,"messages":[]}`
		}
		wg.Go(func() {
			if w := do(t, g.h, call{method: "POST", path: "/v1/chat/completions", key: userKey, body: body}); w.Code != http.StatusOK {
				t.Errorf("status %d", w.Code)
			}
		})
	}
	wg.Wait()
	close(stop)
	scraper.Wait()
	text := g.metricsText()
	validateExposition(t, text)
	expectMetricLines(t, text,
		`kaiak_usage_records_total{key_group="ann",root_group="users",key_id="k-ann",model="open",status="complete"} 24`,
		`kaiak_request_duration_seconds_count{endpoint="chat_completions",model="open",status_class="2xx"} 24`,
		`kaiak_time_to_first_token_seconds_count{model="open",backend="local"} 12`,
	)
}

// validateExposition checks text against the text exposition format 0.0.4: HELP and
// TYPE before a family's samples, each family once, sample names belonging to the
// current family, well-formed labels with only the format's escapes, parseable
// values, no duplicate series, and histograms with cumulative buckets whose +Inf
// bucket equals the count.
func validateExposition(t *testing.T, text string) {
	t.Helper()
	if !strings.HasSuffix(text, "\n") {
		t.Fatal("exposition does not end with a newline")
	}
	var family, kind string
	seenFamily := map[string]bool{}
	seenSeries := map[string]bool{}
	type histSeries struct {
		last  float64
		inf   float64
		count float64
	}
	hist := map[string]*histSeries{}
	for i, line := range strings.Split(strings.TrimSuffix(text, "\n"), "\n") {
		fail := func(msg string) { t.Errorf("line %d %q: %s", i+1, line, msg) }
		if rest, ok := strings.CutPrefix(line, "# HELP "); ok {
			name, _, _ := strings.Cut(rest, " ")
			if !validName(name) || seenFamily[name] {
				fail("bad or repeated HELP")
			}
			seenFamily[name] = true
			family, kind = name, ""
			continue
		}
		if rest, ok := strings.CutPrefix(line, "# TYPE "); ok {
			name, typ, _ := strings.Cut(rest, " ")
			if name != family || kind != "" {
				fail("TYPE without its HELP")
			}
			switch typ {
			case "counter", "gauge", "histogram":
			default:
				fail("unknown type")
			}
			kind = typ
			continue
		}
		if line == "" || line[0] == '#' {
			fail("unexpected line")
			continue
		}
		name, labels, value, ok := parseSample(line)
		if !ok {
			fail("malformed sample")
			continue
		}
		suffix, belongs := strings.CutPrefix(name, family)
		if !belongs || kind == "" || (kind == "histogram" && suffix != "_bucket" && suffix != "_sum" && suffix != "_count") ||
			(kind != "histogram" && suffix != "") {
			fail("sample outside its family")
		}
		if seenSeries[name+"{"+strings.Join(labels, ",")+"}"] {
			fail("duplicate series")
		}
		seenSeries[name+"{"+strings.Join(labels, ",")+"}"] = true
		if kind != "histogram" || suffix == "_sum" {
			continue
		}
		var le string
		var rest []string
		for _, l := range labels {
			if v, ok := strings.CutPrefix(l, `le=`); ok {
				le = v
			} else {
				rest = append(rest, l)
			}
		}
		key := family + "{" + strings.Join(rest, ",") + "}"
		h := hist[key]
		if h == nil {
			h = &histSeries{}
			hist[key] = h
		}
		switch suffix {
		case "_bucket":
			if le == "" {
				fail("bucket without le")
			}
			if value < h.last {
				fail("buckets not cumulative")
			}
			h.last = value
			if le == `"+Inf"` {
				h.inf = value
			}
		case "_count":
			if le != "" {
				fail("count with le")
			}
			if value != h.inf {
				fail("count differs from the +Inf bucket")
			}
			h.count = value
		}
	}
}

// parseSample splits `name{l="v",…} value` into its parts; labels come back as
// `name="escaped"` strings.
func parseSample(line string) (name string, labels []string, value float64, ok bool) {
	i := strings.IndexAny(line, "{ ")
	if i <= 0 || !validName(line[:i]) {
		return "", nil, 0, false
	}
	name, rest := line[:i], line[i:]
	if rest[0] == '{' {
		rest = rest[1:]
		for {
			eq := strings.Index(rest, `="`)
			if eq <= 0 || !validName(rest[:eq]) || strings.Contains(rest[:eq], ":") {
				return "", nil, 0, false
			}
			j := eq + 2
			for ; j < len(rest) && rest[j] != '"'; j++ {
				if rest[j] == '\\' {
					j++
					if j >= len(rest) || !strings.ContainsRune(`\"n`, rune(rest[j])) {
						return "", nil, 0, false
					}
				}
				if rest[j] == '\n' {
					return "", nil, 0, false
				}
			}
			if j >= len(rest) {
				return "", nil, 0, false
			}
			labels = append(labels, rest[:j+1])
			rest = rest[j+1:]
			if strings.HasPrefix(rest, ",") {
				rest = rest[1:]
				continue
			}
			if !strings.HasPrefix(rest, "}") {
				return "", nil, 0, false
			}
			rest = rest[1:]
			break
		}
	}
	v, found := strings.CutPrefix(rest, " ")
	if !found || v == "" {
		return "", nil, 0, false
	}
	value, err := strconv.ParseFloat(v, 64)
	return name, labels, value, err == nil
}

func validName(s string) bool {
	if s == "" {
		return false
	}
	for i, c := range s {
		if !(c == '_' || c == ':' || 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || (i > 0 && '0' <= c && c <= '9')) {
			return false
		}
	}
	return true
}

func TestExpositionValidatorRejectsMalformedSamples(t *testing.T) {
	for _, line := range []string{
		`x{a="1"} 1`, `x 1.5e-05`, `x{a="q\"\\\n"} +Inf`, `x_total{a="1",b="2"} 0`,
	} {
		if _, _, _, ok := parseSample(line); !ok {
			t.Errorf("valid sample refused: %q", line)
		}
	}
	for _, line := range []string{
		`x{a="1"}1`, `x{a="1} 1`, `x{a=1} 1`, `x{a="\t"} 1`, `x{a="1",} 1`, `9x 1`, `x one`, `x{a="1"} `,
	} {
		if _, _, _, ok := parseSample(line); ok {
			t.Errorf("malformed sample accepted: %q", line)
		}
	}
}

func TestRelayedBackendErrorsAreClassedByWhoseProblemTheyAre(t *testing.T) {
	g := newTestGateway(t)
	for _, status := range []int{http.StatusBadRequest, http.StatusNotFound, http.StatusTooManyRequests,
		http.StatusInternalServerError, http.StatusServiceUnavailable} {
		g.backend.SetReply(fakebackend.Reply{Status: status})
		w := do(t, g.h, call{method: "POST", path: "/v1/chat/completions", key: userKey,
			body: `{"model":"open","messages":[]}`})
		if w.Code != status {
			t.Fatalf("backend %d relayed as %d", status, w.Code)
		}
	}
	expectMetricLines(t, g.metricsText(),
		`kaiak_errors_total{class="upstream_client_error"} 2`,
		`kaiak_errors_total{class="upstream_rate_limited"} 1`,
		`kaiak_errors_total{class="upstream_error"} 2`,
		// The gateway's own limits refused nothing.
		`kaiak_errors_total{class="rate_limited"} 0`,
	)
}

func TestEveryErrorCodeHasItsClass(t *testing.T) {
	// The Client API error table (docs/specs/GATEWAY.md), code by code.
	want := map[string]metrics.ErrorClass{
		"missing_api_key": metrics.ErrorAuth, "invalid_api_key": metrics.ErrorAuth,
		"model_not_found": metrics.ErrorNotFound, "unknown_url": metrics.ErrorNotFound,
		"invalid_json": metrics.ErrorInvalidRequest, "duplicate_member": metrics.ErrorInvalidRequest,
		"invalid_body":               metrics.ErrorInvalidRequest,
		"missing_required_parameter": metrics.ErrorInvalidRequest, "invalid_type": metrics.ErrorInvalidRequest,
		"invalid_value": metrics.ErrorInvalidRequest, "n_too_large": metrics.ErrorInvalidRequest,
		"request_too_large": metrics.ErrorInvalidRequest, "method_not_allowed": metrics.ErrorInvalidRequest,
		"upstream_unavailable": metrics.ErrorUpstreamUnavailable, "upstream_auth_failed": metrics.ErrorUpstreamError,
		"upstream_model_missing": metrics.ErrorUpstreamError, "upstream_path_missing": metrics.ErrorUpstreamError,
		"upstream_error":   metrics.ErrorUpstreamError,
		"upstream_timeout": metrics.ErrorUpstreamTimeout, "internal_error": metrics.ErrorInternal,
		"client_closed": metrics.ErrorClientClosed, "rate_limit_exceeded": metrics.ErrorRateLimited,
		"concurrency_limit_exceeded": metrics.ErrorRateLimited,
		"budget_exceeded":            metrics.ErrorBudgetExceeded, "budget_unavailable": metrics.ErrorBudgetUnavailable,
		"queue_full": metrics.ErrorQueueRejected, "queue_timeout": metrics.ErrorQueueRejected,
		"no_healthy_deployment": metrics.ErrorNoHealthyDeployment, "server_busy": metrics.ErrorServerBusy,
		"server_shutting_down": metrics.ErrorShuttingDown, "config_not_loaded": metrics.ErrorNotReady,
	}
	for code, class := range want {
		if got := errorCodeClass(code); got != class {
			t.Errorf("%s: class %s, want %s", code, got, class)
		}
	}
	// The map above covers the table exactly: a code added to the table without a
	// class fails here, not in production as class="internal".
	for _, code := range clientAPIErrorCodes(t) {
		if _, ok := want[code]; !ok {
			t.Errorf("%s is in the Client API table but not checked here", code)
		}
	}
}

// clientAPIErrorCodes reads the code column of the Client API error table in
// docs/specs/GATEWAY.md.
func clientAPIErrorCodes(t *testing.T) []string {
	t.Helper()
	doc, err := os.ReadFile(filepath.Join("..", "..", "..", "docs", "specs", "GATEWAY.md"))
	if err != nil {
		t.Fatal(err)
	}
	var codes []string
	inTable := false
	for line := range strings.Lines(string(doc)) {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "| Situation | Status |") {
			inTable = true
			continue
		}
		if !inTable {
			continue
		}
		if !strings.HasPrefix(line, "|") {
			break
		}
		cells := strings.Split(strings.Trim(line, "|"), "|")
		if code := strings.Trim(strings.TrimSpace(cells[len(cells)-1]), "`"); code != "" && !strings.HasPrefix(code, "---") {
			codes = append(codes, code)
		}
	}
	if len(codes) < 20 {
		t.Fatalf("read %d codes from the Client API table: %v", len(codes), codes)
	}
	return codes
}
