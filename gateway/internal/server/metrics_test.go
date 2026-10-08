package server

import (
	"bytes"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
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
	"time"

	"kaiak/internal/auth"
	"kaiak/internal/config"
	"kaiak/internal/fakebackend"
	"kaiak/internal/limits"
	"kaiak/internal/metrics"
	"kaiak/internal/provider"
	"kaiak/internal/telemetry/metric"
)

func (g *testGateway) metricsText() string {
	var buf bytes.Buffer
	metric.WritePrometheus(&buf, g.metrics.Collect())
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
	wl := `kaiak_key_group="eval",kaiak_key_root_group="research",kaiak_key_id="k-eval",gen_ai_request_model="pair",` +
		`gen_ai_operation_name="chat",kaiak_usage_status="complete"`
	tokens := wl + `,gen_ai_token_modality="unknown"`
	usageLine := func(name, labels string, value int64) string {
		return name + "{" + labels + "} " + strconv.FormatInt(value, 10)
	}
	text := g.metricsText()
	expectMetricLines(t, text,
		`http_server_request_duration_seconds_count{http_request_method="POST",url_scheme="http",http_route="/v1/chat/completions",http_response_status_code="200",gen_ai_request_model="pair"} 1`,
		`http_server_request_duration_seconds_count{http_request_method="POST",url_scheme="http",http_route="/v1/chat/completions",http_response_status_code="429",error_type="rate_limit_exceeded",gen_ai_request_model="pair"} 1`,
		`http_server_request_duration_seconds_count{http_request_method="POST",url_scheme="http",http_route="/v1/chat/completions",http_response_status_code="502",error_type="upstream_unavailable",gen_ai_request_model="down"} 1`,
		// A relayed backend error status: its class is the error type.
		`http_server_request_duration_seconds_count{http_request_method="POST",url_scheme="http",http_route="/v1/chat/completions",http_response_status_code="500",error_type="upstream_error",gen_ai_request_model="open"} 1`,
		// A model the caller may not use (or that does not exist) is never an attribute
		// value, nor is a path no route matched.
		`http_server_request_duration_seconds_count{http_request_method="POST",url_scheme="http",http_route="/v1/chat/completions",http_response_status_code="404",error_type="model_not_found"} 1`,
		`http_server_request_duration_seconds_count{http_request_method="GET",url_scheme="http",http_response_status_code="404",error_type="unknown_url"} 1`,
		`kaiak_errors_total{kaiak_error_class="rate_limited"} 1`,
		`kaiak_errors_total{kaiak_error_class="upstream_unavailable"} 1`,
		`kaiak_errors_total{kaiak_error_class="upstream_error"} 1`,
		`kaiak_errors_total{kaiak_error_class="not_found"} 2`,
		`kaiak_errors_total{kaiak_error_class="auth"} 0`,
		// Usage metrics carry the record's own numbers.
		usageLine("kaiak_usage_records_total", wl, 1),
		usageLine("gen_ai_client_inference_usage_input_tokens_total", tokens, pair.Units.Sum(config.InputUnits)),
		usageLine("gen_ai_client_inference_usage_cache_read_input_tokens_total", tokens, pair.Units[config.UnitTokensCached]),
		usageLine("gen_ai_client_inference_usage_cache_write_input_tokens_total", tokens, pair.Units[config.UnitTokensCacheWrite]),
		usageLine("gen_ai_client_inference_usage_output_tokens_total", tokens, pair.Units[config.UnitTokensOut]),
		usageLine("gen_ai_client_inference_usage_reasoning_output_tokens_total", tokens, pair.Units[config.UnitTokensReasoning]),
		`kaiak_usage_cost_usd_total{`+wl+`} `+strconv.FormatFloat(float64(pair.CostNanoUSD)/1e9, 'g', -1, 64),
		`kaiak_usage_records_total{kaiak_key_group="eval",kaiak_key_root_group="research",kaiak_key_id="k-eval",gen_ai_request_model="down",gen_ai_operation_name="chat",kaiak_usage_status="partial"} 1`,
		`kaiak_usage_records_total{kaiak_key_group="ann",kaiak_key_root_group="users",kaiak_key_id="k-ann",gen_ai_request_model="open",gen_ai_operation_name="chat",kaiak_usage_status="complete"} 1`,
		`kaiak_backend_active_requests{kaiak_backend_id="local"} 0`,
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

// The usage metrics count tokens as the request line does: input on the metric is
// the line's gen_ai.usage.input_tokens (cache reads and writes included), output its
// gen_ai.usage.output_tokens (reasoning included). The operation is the endpoint's,
// the provider the deployment's backend type's — absent for a self-hosted type.
func TestUsageMetricsCountAsTheRequestLine(t *testing.T) {
	g := newTestGateway(t)
	g.backend.SetReply(fakebackend.Reply{Usage: &fakebackend.Usage{
		PromptTokens: 100, CachedTokens: 40, CacheWriteTokens: 20, CompletionTokens: 10, ReasoningTokens: 4}})
	if w := post(t, g, "cached", `{"model":"on-azure","messages":[]}`); w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var line map[string]any
	if err := json.Unmarshal([]byte(logLine(t, g, "cached")), &line); err != nil {
		t.Fatal(err)
	}
	input, output := line["gen_ai.usage.input_tokens"], line["gen_ai.usage.output_tokens"]
	if input != float64(100) || output != float64(10) {
		t.Fatalf("log line input %v, output %v; want the backend's 100 and 10", input, output)
	}
	tokens := `{kaiak_key_group="eval",kaiak_key_root_group="research",kaiak_key_id="k-eval",gen_ai_request_model="on-azure",` +
		`gen_ai_operation_name="chat",gen_ai_provider_name="azure.ai.openai",kaiak_usage_status="complete",gen_ai_token_modality="unknown"} `
	expectMetricLines(t, g.metricsText(),
		`gen_ai_client_inference_usage_input_tokens_total`+tokens+strconv.FormatFloat(input.(float64), 'g', -1, 64),
		`gen_ai_client_inference_usage_cache_read_input_tokens_total`+tokens+`40`,
		`gen_ai_client_inference_usage_cache_write_input_tokens_total`+tokens+`20`,
		`gen_ai_client_inference_usage_output_tokens_total`+tokens+strconv.FormatFloat(output.(float64), 'g', -1, 64),
		`gen_ai_client_inference_usage_reasoning_output_tokens_total`+tokens+`4`)

	if w := do(t, g.h, call{method: "POST", path: "/v1/embeddings", key: userKey, body: `{"model":"open","input":"x"}`}); w.Code != http.StatusOK {
		t.Fatalf("embeddings: status %d: %s", w.Code, w.Body.String())
	}
	if w := do(t, g.h, call{method: "POST", path: "/v1/completions", key: userKey, body: `{"model":"open","prompt":"x"}`}); w.Code != http.StatusOK {
		t.Fatalf("completions: status %d: %s", w.Code, w.Body.String())
	}
	ann := `kaiak_usage_records_total{kaiak_key_group="ann",kaiak_key_root_group="users",kaiak_key_id="k-ann",gen_ai_request_model="open",`
	expectMetricLines(t, g.metricsText(),
		ann+`gen_ai_operation_name="embeddings",kaiak_usage_status="complete"} 1`,
		ann+`gen_ai_operation_name="text_completion",kaiak_usage_status="complete"} 1`)
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
		`kaiak_time_to_first_token_seconds_count{gen_ai_request_model="open",kaiak_backend_id="local"} 1`,
		`kaiak_output_token_rate_per_second_count{gen_ai_request_model="open",kaiak_backend_id="local"} 1`,
		`http_server_request_duration_seconds_count{http_request_method="POST",url_scheme="http",http_route="/v1/chat/completions",http_response_status_code="200",gen_ai_request_model="open"} 2`,
	)
}

// The request duration names the route as the client API documents it — never the
// path — and the method only as the HTTP convention knows it.
func TestRequestDurationRoutesAndMethods(t *testing.T) {
	g := newTestGateway(t)
	for _, c := range []call{
		{method: "GET", path: "/v1/models", key: userKey},
		{method: "GET", path: "/v1/models/open", key: userKey},
		{method: "GET", path: "/v1/models/open/props", key: userKey},
		{method: "POST", path: "/v1/embeddings", key: userKey, body: `{"model":"open","input":"x"}`},
		{method: "BREW", path: "/v1/chat/completions", key: userKey},
		{method: "GET", path: "/v1/elsewhere", key: userKey},
	} {
		do(t, g.h, c)
	}
	const scheme = `url_scheme="http"`
	text := g.metricsText()
	expectMetricLines(t, text,
		`http_server_request_duration_seconds_count{http_request_method="GET",`+scheme+`,http_route="/v1/models",http_response_status_code="200"} 1`,
		`http_server_request_duration_seconds_count{http_request_method="GET",`+scheme+`,http_route="/v1/models/{model}",http_response_status_code="200",gen_ai_request_model="open"} 1`,
		`http_server_request_duration_seconds_count{http_request_method="GET",`+scheme+`,http_route="/v1/models/{model}/props",http_response_status_code="200",gen_ai_request_model="open"} 1`,
		`http_server_request_duration_seconds_count{http_request_method="POST",`+scheme+`,http_route="/v1/embeddings",http_response_status_code="200",gen_ai_request_model="open"} 1`,
		// A refused method matched no route; the method as sent is never a value.
		`http_server_request_duration_seconds_count{http_request_method="_OTHER",`+scheme+`,http_response_status_code="405",error_type="method_not_allowed"} 1`,
		`http_server_request_duration_seconds_count{http_request_method="GET",`+scheme+`,http_response_status_code="404",error_type="unknown_url"} 1`,
	)
	for _, leak := range []string{"BREW", "/v1/elsewhere", "/v1/models/open"} {
		if strings.Contains(text, leak) {
			t.Errorf("%q, sent by the client, became an attribute value", leak)
		}
	}
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
		`kaiak_request_errors_total{kaiak_key_group="eval",kaiak_key_root_group="research",kaiak_key_id="k-eval",gen_ai_request_model="pair",error_type="rate_limit_exceeded"} 2`,
		`kaiak_request_errors_total{kaiak_key_group="ann",kaiak_key_root_group="users",kaiak_key_id="k-ann",error_type="model_not_found"} 1`,
		`kaiak_request_errors_total{error_type="invalid_api_key"} 1`,
		`kaiak_request_errors_total{error_type="unknown_url"} 1`,
	)
	if strings.Contains(text, `gen_ai_request_model="nope-model"`) {
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
		return replaceOnce(t, doc, `"max_request_body_bytes": 1024 }`,
			`"max_request_body_bytes": 1024, "metrics": { "key_id_label": false, "group_label": false } }`)
	}))
	g.backend.SetReply(fakebackend.Reply{Status: http.StatusInternalServerError})
	if w := do(t, g.h, call{method: "POST", path: "/v1/chat/completions", key: userKey, body: chatBody}); w.Code != http.StatusInternalServerError {
		t.Fatalf("status %d", w.Code)
	}
	expectMetricLines(t, g.metricsText(),
		`kaiak_request_errors_total{kaiak_key_root_group="users",gen_ai_request_model="open",error_type="upstream_error"} 1`)
}

func TestKeyIDLabelSwitchedOff(t *testing.T) {
	g := newTestGateway(t)
	g.holder.Swap(testSnapshotWith(t, g.backend.URL(), func(doc string) string {
		return replaceOnce(t, doc, `"max_request_body_bytes": 1024 }`,
			`"max_request_body_bytes": 1024, "metrics": { "key_id_label": false } }`)
	}))
	if w := do(t, g.h, call{method: "POST", path: "/v1/chat/completions", key: userKey, body: chatBody}); w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	text := g.metricsText()
	expectMetricLines(t, text, `kaiak_usage_records_total{kaiak_key_group="ann",kaiak_key_root_group="users",gen_ai_request_model="open",gen_ai_operation_name="chat",kaiak_usage_status="complete"} 1`)
	if strings.Contains(text, "kaiak_key_id=") {
		t.Errorf("kaiak_key_id label present while switched off:\n%s", text)
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
		return replaceOnce(t, doc, `"max_request_body_bytes": 1024 }`,
			`"max_request_body_bytes": 1024, "metrics": { "group_label": false } }`)
	}))
	if w := do(t, g.h, call{method: "POST", path: "/v1/chat/completions", key: userKey, body: chatBody}); w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	text := g.metricsText()
	expectMetricLines(t, text, `kaiak_usage_records_total{kaiak_key_root_group="users",kaiak_key_id="k-ann",gen_ai_request_model="open",gen_ai_operation_name="chat",kaiak_usage_status="complete"} 1`)
	if strings.Contains(text, `kaiak_key_group=`) {
		t.Errorf("kaiak_key_group label present while switched off:\n%s", text)
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
	for _, family := range []string{"kaiak_build_info", "http_server_request_duration_seconds", "kaiak_time_to_first_token_seconds",
		"kaiak_output_token_rate_per_second", "kaiak_errors_total", "kaiak_backend_active_requests",
		"kaiak_config_loads_total", "kaiak_config_last_applied_timestamp_seconds", "kaiak_usage_records_total",
		"gen_ai_client_inference_usage_input_tokens_total", "kaiak_usage_cost_usd_total", "kaiak_backend_active_requests_limit", "kaiak_queue_size",
		"kaiak_queue_wait_duration_seconds", "kaiak_queue_rejections_total", "kaiak_retries_total", "kaiak_request_attempts",
		"kaiak_circuit_state", "kaiak_deployment_cooling_down", "kaiak_circuit_transitions_total", "kaiak_probes_total",
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
		`kaiak_usage_records_total{kaiak_key_group="ann",kaiak_key_root_group="users",kaiak_key_id="k-ann",gen_ai_request_model="open",gen_ai_operation_name="chat",kaiak_usage_status="complete"} 24`,
		`http_server_request_duration_seconds_count{http_request_method="POST",url_scheme="http",http_route="/v1/chat/completions",http_response_status_code="200",gen_ai_request_model="open"} 24`,
		`kaiak_time_to_first_token_seconds_count{gen_ai_request_model="open",kaiak_backend_id="local"} 12`,
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
		`kaiak_errors_total{kaiak_error_class="upstream_client_error"} 2`,
		`kaiak_errors_total{kaiak_error_class="upstream_rate_limited"} 1`,
		`kaiak_errors_total{kaiak_error_class="upstream_error"} 2`,
		// The gateway's own limits refused nothing.
		`kaiak_errors_total{kaiak_error_class="rate_limited"} 0`,
	)
}

func TestEveryErrorCodeHasItsClass(t *testing.T) {
	// The Client API error table (docs/specs/GATEWAY.md), code by code, with the class
	// Observability → error classes gives it.
	want := map[string]metrics.ErrorClass{
		"missing_api_key": metrics.ErrorAuth, "invalid_api_key": metrics.ErrorAuth,
		"model_not_found": metrics.ErrorNotFound, "unknown_url": metrics.ErrorNotFound,
		"invalid_json": metrics.ErrorInvalidRequest, "duplicate_member": metrics.ErrorInvalidRequest,
		"invalid_body":               metrics.ErrorInvalidRequest,
		"missing_required_parameter": metrics.ErrorInvalidRequest, "invalid_type": metrics.ErrorInvalidRequest,
		"invalid_value": metrics.ErrorInvalidRequest, "n_too_large": metrics.ErrorInvalidRequest,
		"request_too_large": metrics.ErrorInvalidRequest, "method_not_allowed": metrics.ErrorInvalidRequest,
		"stateful_responses_unsupported": metrics.ErrorInvalidRequest, "hosted_tool_unsupported": metrics.ErrorInvalidRequest,
		"price_option_unsupported": metrics.ErrorInvalidRequest, "endpoint_not_served": metrics.ErrorInvalidRequest,
		"stored_object_unsupported": metrics.ErrorInvalidRequest,
		"upstream_endpoint_missing": metrics.ErrorUpstreamError,
		"upstream_overloaded":       metrics.ErrorUpstreamRateLimited, "upstream_refused": metrics.ErrorUpstreamClientError,
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
	// Every answer the gateway builds carries its code's class.
	answered := map[string]bool{}
	for _, a := range errorAnswers() {
		class, ok := want[a.err.code]
		switch {
		case !ok:
			t.Errorf("%s answers %s, which is not in the Client API table", a.constructor, a.err.code)
		case a.err.class != class:
			t.Errorf("%s answers %s with class %q, want %s", a.constructor, a.err.code, a.err.class, class)
		}
		answered[a.err.code] = true
	}
	// The map above covers the table exactly, and every code in it is answered.
	table := clientAPIErrorCodes(t)
	for _, code := range table {
		if _, ok := want[code]; !ok {
			t.Errorf("%s is in the Client API table but not checked here", code)
		}
		if !answered[code] {
			t.Errorf("%s is in the Client API table but no answer in errorAnswers has it", code)
		}
	}
	for code := range want {
		if !slices.Contains(table, code) {
			t.Errorf("%s is checked here but not in the Client API table", code)
		}
	}
	// errorAnswers lists every function that builds an answer.
	listed := map[string]bool{}
	for _, a := range errorAnswers() {
		listed[a.constructor] = true
	}
	for _, name := range apiErrorConstructors(t) {
		if !listed[name] {
			t.Errorf("%s builds an apiError but errorAnswers does not list its answers", name)
		}
	}
}

// errorAnswers are the gateway's error answers, built as the pipeline builds them: each
// constructor once per code it can answer with.
func errorAnswers() []struct {
	constructor string
	err         *apiError
} {
	type answer = struct {
		constructor string
		err         *apiError
	}
	r := httptest.NewRequest(http.MethodGet, "/v1/unknown", nil)
	answers := []answer{
		{"authError", authError(&auth.Error{Code: auth.CodeModelNotFound})},
		{"authError", authError(&auth.Error{Code: auth.CodeMissingKey})},
		{"authError", authError(&auth.Error{Code: auth.CodeUnknownKey})},
		{"errUnknownURL", errUnknownURL(r)},
		{"errMethodNotAllowed", errMethodNotAllowed(r)},
		{"errBodyTooLarge", errBodyTooLarge(1)},
		{"errInvalidJSON", errInvalidJSON()},
		{"errDuplicateMember", errDuplicateMember("model")},
		{"errInvalidType", errInvalidType("stream", "a boolean")},
		{"errNTooLarge", errNTooLarge("n", 1)},
		{"errTooManySequences", errTooManySequences("n", 2, 1)},
		{"errTooManyInputs", errTooManyInputs(2, 1)},
		{"errTooManyDocuments", errTooManyDocuments(2, 1)},
		{"errOutputLimitTooLarge", errOutputLimitTooLarge("max_tokens", 2, 1)},
		{"errOutputLimitBelowThinking", errOutputLimitBelowThinking("max_tokens", 1, "default", 2)},
		{"errOutputLimitNegative", errOutputLimitNegative("max_tokens", -1)},
		{"errMissingModel", errMissingModel()},
		{"errReadBody", errReadBody()},
		{"errClientClosed", errClientClosed()},
		{"errUpstreamFault", errUpstreamFault(http.StatusInternalServerError)},
		{"errUpstreamOverloaded", errUpstreamOverloaded(provider.StatusOverloaded)},
		{"errUpstreamRefused", errUpstreamRefused("invalid_prompt")},
		// Every provider refusal is the caller's mistake, whatever its code.
		{"errRefused", errRefused(&provider.RefusalError{Code: "price_option_unsupported"})},
		{"errRefused", errRefused(&provider.RefusalError{Code: "duplicate_member"})},
		{"errEndpointNotServed", errEndpointNotServed(bodyEndpoint(provider.ChatCompletions))},
		{"errInternal", errInternal()},
		{"errQueueFull", errQueueFull()},
		{"errConcurrencyLimited", errConcurrencyLimited(1)},
		{"errServerBusy", errServerBusy()},
		{"errNoHealthyDeployment", errNoHealthyDeployment()},
		{"errQueueTimeout", errQueueTimeout(time.Second)},
		{"errHostedTool", errHostedTool("tools[0].type", "web_search")},
		{"errStatefulResponses", errStatefulResponses("conversation")},
		{"errStoredObjectResponses", errStoredObjectResponses("input[0].file_id")},
		{"errStoredObject", errStoredObject("messages[0].content[0].source.file_id")},
		{"errHostedMember", errHostedMember("mcp_servers")},
		{"errShuttingDown", errShuttingDown()},
		{"errConfigNotLoaded", errConfigNotLoaded()},
		{"errLimited", errLimited(&limits.Rejection{Type: config.LimitRequestsPerMinute, Measure: config.MeasureRequests})},
		{"errLimited", errLimited(&limits.Rejection{Type: config.LimitTokensPerMinute, Measure: config.MeasureTokens})},
		{"errLimited", errLimited(&limits.Rejection{Type: config.LimitUSDPerMonth, Measure: config.MeasureCost})},
		{"errBudgetUnavailable", errBudgetUnavailable(&limits.Rejection{})},
	}
	// The answers to a failure to get a response, by provider code and by the kind of a
	// first event that was an error event.
	for code, rule := range failureRules {
		answers = append(answers, answer{"upstreamAnswer", rule.answer(&provider.Error{Code: code})})
	}
	for kind, rule := range errorEventRules {
		answers = append(answers, answer{"errorEventRules", rule.answer(&provider.Error{Code: provider.CodeErrorEvent,
			Event: &provider.ErrorEvent{Kind: kind, Code: []byte(`"invalid_prompt"`)}})})
	}
	return answers
}

// apiErrorConstructors are the functions of the package that build an apiError.
func apiErrorConstructors(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		// A function, or a package-level variable (a rule table's answer), is named by
		// its declaration.
		for _, decl := range f.Decls {
			name := ""
			switch d := decl.(type) {
			case *ast.FuncDecl:
				name = d.Name.Name
			case *ast.GenDecl:
				if spec, ok := d.Specs[0].(*ast.ValueSpec); ok && d.Tok == token.VAR {
					name = spec.Names[0].Name
				}
			}
			if name == "" {
				continue
			}
			ast.Inspect(decl, func(n ast.Node) bool {
				if lit, ok := n.(*ast.CompositeLit); ok {
					if id, ok := lit.Type.(*ast.Ident); ok && id.Name == "apiError" {
						names = append(names, name)
						return false
					}
				}
				return true
			})
		}
	}
	if len(names) < 30 {
		t.Fatalf("found %d functions building an apiError: %v", len(names), names)
	}
	return names
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
