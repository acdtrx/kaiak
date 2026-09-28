package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

// run is one kind's check sequence and its results.
type run struct {
	ctx    context.Context
	o      options
	gw     *gateway
	key    string
	client *http.Client

	passed, failed, skipped int
}

func newClient(timeout time.Duration) *http.Client { return &http.Client{Timeout: timeout} }

func (r *run) pass(name, detail string) {
	r.passed++
	fmt.Printf("  PASS  %-20s %s\n", name, detail)
}

func (r *run) fail(name, format string, args ...any) {
	r.failed++
	text := strings.TrimSpace(fmt.Sprintf(format, args...))
	fmt.Printf("  FAIL  %-20s %s\n", name, strings.ReplaceAll(text, "\n", "\n"+strings.Repeat(" ", 28)))
}

func (r *run) skip(name, why string) {
	r.skipped++
	fmt.Printf("  SKIP  %-20s %s\n", name, why)
}

func (r *run) report() error {
	fmt.Printf("  %d passed, %d failed, %d skipped\n", r.passed, r.failed, r.skipped)
	if r.failed > 0 {
		return errChecksFailed
	}
	return nil
}

// Request IDs of the requests whose log lines the usage checks read.
const (
	idChat        = "live-chat"
	idStream      = "live-stream"
	idStreamUsage = "live-stream-usage"
	idEmbed       = "live-embed"
)

func (r *run) checks() {
	r.checkAuth()
	r.checkModels()
	r.checkChat()
	r.checkUsageLog("usage-log/chat", idChat, true)
	r.checkStream("chat-stream", idStream, false)
	r.checkUsageLog("usage-log/stream", idStream, true)
	r.checkStream("chat-stream-usage", idStreamUsage, true)
	r.checkUsageLog("usage-log/stream-usg", idStreamUsage, true)
	if r.o.embeddingsModel == "" {
		r.skip("embeddings", "no -embeddings-model")
		r.skip("usage-log/embeddings", "no -embeddings-model")
	} else {
		r.checkEmbeddings()
		r.checkUsageLog("usage-log/embeddings", idEmbed, false)
	}
	r.checkCeiling()
	r.checkRateLimit()
	if r.o.twoBackends() {
		r.checkSpread()
		if r.o.maxInFlight > 0 {
			r.checkCapacity()
		} else {
			r.skip("capacity", "no -max-in-flight")
		}
		if r.o.checkFailover {
			r.checkFailover()
		} else {
			r.skip("failover", "no -check-failover")
		}
	}
	r.checkMetrics()
}

func (r *run) checkAuth() {
	resp, err := r.post("", "/v1/chat/completions", "", chatBody(modelChat, false, nil))
	switch {
	case err != nil:
		r.fail("auth-reject", "%v", err)
	case resp.status != http.StatusUnauthorized:
		r.fail("auth-reject", "no key: status %d, want 401\n%s", resp.status, resp.body)
	default:
		r.pass("auth-reject", "no key → 401")
	}
}

func (r *run) checkModels() {
	status, body, err := httpGet(r.ctx, r.client, r.gw.api+"/v1/models", r.key)
	if err != nil || status != http.StatusOK {
		r.fail("models", "GET /v1/models: %d %s %v", status, body, err)
		return
	}
	var list struct {
		Data []struct{ ID string }
	}
	if err := json.Unmarshal(body, &list); err != nil {
		r.fail("models", "not a model list: %s", body)
		return
	}
	var ids []string
	for _, m := range list.Data {
		ids = append(ids, m.ID)
	}
	want := []string{modelCapped, modelChat, modelRPM}
	if r.o.embeddingsModel != "" {
		want = append(want, modelEmbed)
		slices.Sort(want)
	}
	if !slices.Equal(ids, want) {
		r.fail("models", "lists %v, want %v", ids, want)
		return
	}
	r.pass("models", "lists "+strings.Join(ids, ", "))
}

// chatAnswer is the part of a chat completion the checks read.
type chatAnswer struct {
	Model   string
	Choices []struct {
		Message struct {
			Content          string
			ReasoningContent string `json:"reasoning_content"`
			Reasoning        string
		}
		FinishReason string `json:"finish_reason"`
	}
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	}
}

func (r *run) checkChat() {
	resp, err := r.post(r.key, "/v1/chat/completions", idChat, chatBody(modelChat, false, nil))
	if !r.ok("chat", resp, err, idChat) {
		return
	}
	var a chatAnswer
	if err := json.Unmarshal(resp.body, &a); err != nil || len(a.Choices) == 0 {
		r.fail("chat", "not a chat completion: %s", resp.body)
		return
	}
	msg := a.Choices[0].Message
	switch {
	case a.Model != modelChat:
		r.fail("chat", "answer names model %q, want the public name %q", a.Model, modelChat)
	case msg.Content == "" && (msg.ReasoningContent != "" || msg.Reasoning != ""):
		r.fail("chat", "only reasoning came back (finish_reason %q): raise -max-output, or switch thinking off with -chat-defaults", a.Choices[0].FinishReason)
	case msg.Content == "":
		r.fail("chat", "empty content: %s", resp.body)
	case a.Usage == nil:
		r.fail("chat", "no usage in the answer: %s", resp.body)
	default:
		r.pass("chat", fmt.Sprintf("%q (finish %s, %d+%d tokens)", clip(msg.Content), a.Choices[0].FinishReason,
			a.Usage.PromptTokens, a.Usage.CompletionTokens))
	}
}

// checkStream streams a chat completion. With includeUsage the client asks for the
// usage chunk and it must arrive last before [DONE]; without, the gateway asked for
// it on the client's behalf and must withhold it.
func (r *run) checkStream(name, id string, includeUsage bool) {
	extra := map[string]any{}
	if includeUsage {
		extra["stream_options"] = map[string]any{"include_usage": true}
	}
	req, err := r.request(r.key, "/v1/chat/completions", id, chatBody(modelChat, true, extra))
	if err != nil {
		r.fail(name, "%v", err)
		return
	}
	resp, err := r.client.Do(req)
	if err != nil {
		r.fail(name, "%v", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		body, _ := io.ReadAll(resp.Body)
		r.fail(name, "status %d, content type %q: %s%s", resp.StatusCode, resp.Header.Get("Content-Type"), body, r.upstreamHint(id))
		return
	}

	var (
		chunks, usageChunks int
		usageLast, done     bool
		afterDone           string
		content, reasoning  strings.Builder
		badModel            string
	)
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for sc.Scan() {
		data, ok := strings.CutPrefix(sc.Text(), "data:")
		if !ok {
			continue
		}
		data = strings.TrimPrefix(data, " ")
		if done {
			afterDone = data
			continue
		}
		if data == "[DONE]" {
			done = true
			continue
		}
		var c struct {
			Model   string
			Choices []struct {
				Delta struct {
					Content          string
					ReasoningContent string `json:"reasoning_content"`
					Reasoning        string
				}
			}
			Usage json.RawMessage
		}
		if err := json.Unmarshal([]byte(data), &c); err != nil {
			r.fail(name, "chunk is not JSON: %s", data)
			return
		}
		chunks++
		if c.Model != modelChat && badModel == "" {
			badModel = c.Model
		}
		isUsage := len(c.Choices) == 0 && len(c.Usage) > 0 && string(c.Usage) != "null"
		if isUsage {
			usageChunks++
		}
		usageLast = isUsage
		for _, ch := range c.Choices {
			content.WriteString(ch.Delta.Content)
			reasoning.WriteString(ch.Delta.ReasoningContent + ch.Delta.Reasoning)
		}
	}
	if err := sc.Err(); err != nil {
		r.fail(name, "stream broke off after %d chunks: %v", chunks, err)
		return
	}
	switch {
	case !done:
		r.fail(name, "stream ended without [DONE] after %d chunks", chunks)
	case afterDone != "":
		r.fail(name, "data after [DONE]: %s", afterDone)
	case badModel != "":
		r.fail(name, "a chunk names model %q, want %q", badModel, modelChat)
	case content.Len() == 0 && reasoning.Len() > 0:
		r.fail(name, "only reasoning streamed: raise -max-output, or switch thinking off with -chat-defaults")
	case content.Len() == 0:
		r.fail(name, "no content in %d chunks", chunks)
	case includeUsage && (usageChunks != 1 || !usageLast):
		r.fail(name, "asked for usage: %d usage chunks (want 1, last before [DONE]; last chunk usage: %v)", usageChunks, usageLast)
	case !includeUsage && usageChunks > 0:
		r.fail(name, "did not ask for usage, yet %d usage chunk(s) reached the client", usageChunks)
	default:
		detail := fmt.Sprintf("%d chunks, [DONE], %q", chunks, clip(content.String()))
		if includeUsage {
			detail = "usage chunk relayed, " + detail
		} else {
			detail = "no usage chunk, " + detail
		}
		r.pass(name, detail)
	}
}

func (r *run) checkEmbeddings() {
	resp, err := r.post(r.key, "/v1/embeddings", idEmbed, map[string]any{"model": modelEmbed, "input": []string{"The quick brown fox."}})
	if !r.ok("embeddings", resp, err, idEmbed) {
		return
	}
	var a struct {
		Model string
		Data  []struct{ Embedding []float64 }
		Usage *struct {
			PromptTokens int `json:"prompt_tokens"`
		}
	}
	switch err := json.Unmarshal(resp.body, &a); {
	case err != nil || len(a.Data) != 1:
		r.fail("embeddings", "not one embedding: %s", clip(string(resp.body)))
	case a.Model != modelEmbed:
		r.fail("embeddings", "answer names model %q, want %q", a.Model, modelEmbed)
	case len(a.Data[0].Embedding) == 0:
		r.fail("embeddings", "empty vector")
	case a.Usage == nil || a.Usage.PromptTokens == 0:
		r.fail("embeddings", "no prompt_tokens in usage")
	default:
		detail := fmt.Sprintf("%d dimensions, %d tokens", len(a.Data[0].Embedding), a.Usage.PromptTokens)
		if !r.o.embeddingsServer() {
			r.pass("embeddings", detail)
			return
		}
		line, err := r.gw.logs.wait(r.ctx, msg("request", "request_id", idEmbed))
		if err != nil || line["backend"] != backendEmbed {
			r.fail("embeddings", "served by backend %v, want %s (%v)", line["backend"], backendEmbed, err)
			return
		}
		r.pass("embeddings", detail+", served by "+backendEmbed)
	}
}

// checkCeiling asks for far more output than the capped model's ceiling allows — its
// whole context length; more is refused outright (400 invalid_value): the gateway
// must lower it, so the answer stops at the ceiling. vLLM is asked through
// max_tokens (the gateway lowers the client's own key); the cloud APIs through
// max_completion_tokens (their reasoning models refuse max_tokens).
func (r *run) checkCeiling() {
	limitKey := "max_completion_tokens"
	if r.o.kind == kindVLLM {
		limitKey = "max_tokens"
	}
	body := map[string]any{"model": modelCapped, limitKey: r.o.contextLength, "messages": []any{map[string]any{"role": "user",
		"content": "Write a long story, at least 600 words, about a lighthouse keeper."}}}
	resp, err := r.post(r.key, "/v1/chat/completions", "live-ceiling", body)
	if !r.ok("output-ceiling", resp, err, "live-ceiling") {
		return
	}
	var a chatAnswer
	switch err := json.Unmarshal(resp.body, &a); {
	case err != nil || len(a.Choices) == 0 || a.Usage == nil:
		r.fail("output-ceiling", "no choices or usage: %s", resp.body)
	case a.Usage.CompletionTokens > r.o.ceiling:
		r.fail("output-ceiling", "%d completion tokens, above the ceiling %d", a.Usage.CompletionTokens, r.o.ceiling)
	case a.Choices[0].FinishReason != "length":
		r.fail("output-ceiling", "finish_reason %q, want \"length\" (%d tokens)", a.Choices[0].FinishReason, a.Usage.CompletionTokens)
	default:
		r.pass("output-ceiling", fmt.Sprintf("%s %d → %d tokens, finish length", limitKey, r.o.contextLength, a.Usage.CompletionTokens))
	}
}

// checkRateLimit: live-rpm allows 1 request per minute, so the second is refused.
func (r *run) checkRateLimit() {
	resp, err := r.post(r.key, "/v1/chat/completions", "live-rpm-1", chatBody(modelRPM, false, nil))
	if !r.ok("rate-limit", resp, err, "live-rpm-1") {
		return
	}
	resp, err = r.post(r.key, "/v1/chat/completions", "live-rpm-2", chatBody(modelRPM, false, nil))
	if err != nil {
		r.fail("rate-limit", "%v", err)
		return
	}
	var e struct {
		Error struct{ Code, Type string }
	}
	_ = json.Unmarshal(resp.body, &e)
	h := resp.header
	switch {
	case resp.status != http.StatusTooManyRequests:
		r.fail("rate-limit", "second request: status %d, want 429\n%s", resp.status, resp.body)
	case e.Error.Code != "rate_limit_exceeded" || e.Error.Type != "requests":
		r.fail("rate-limit", "error %s/%s, want requests/rate_limit_exceeded", e.Error.Type, e.Error.Code)
	case h.Get("Retry-After") == "":
		r.fail("rate-limit", "no Retry-After")
	case h.Get("X-Ratelimit-Limit-Requests") != "1" || h.Get("X-Ratelimit-Remaining-Requests") != "0" || h.Get("X-Ratelimit-Reset-Requests") == "":
		r.fail("rate-limit", "x-ratelimit headers: limit %q remaining %q reset %q", h.Get("X-Ratelimit-Limit-Requests"),
			h.Get("X-Ratelimit-Remaining-Requests"), h.Get("X-Ratelimit-Reset-Requests"))
	default:
		r.pass("rate-limit", fmt.Sprintf("second request → 429, Retry-After %s, reset %s", h.Get("Retry-After"), h.Get("X-Ratelimit-Reset-Requests")))
	}
}

// checkUsageLog reads the request's log line: the backend's own token report
// (estimated=false) and, when prices are set, a cost.
func (r *run) checkUsageLog(name, id string, output bool) {
	line, err := r.gw.logs.wait(r.ctx, msg("request", "request_id", id))
	if err != nil {
		r.fail(name, "no log line for request %s: %v", id, err)
		return
	}
	num := func(k string) float64 { v, _ := line[k].(float64); return v }
	priced := r.o.priceIn != 0 || r.o.priceOut != 0
	switch {
	case line["status"] != 200.0:
		r.fail(name, "request %s ended with status %v", id, line["status"])
	case line["estimated"] != false:
		r.fail(name, "estimated=%v: the backend reported no usage, so the gateway estimated it", line["estimated"])
	case line["partial"] != false:
		r.fail(name, "partial=%v (relay_end %v)", line["partial"], line["relay_end"])
	case num("tokens_in")+num("tokens_cached") == 0:
		r.fail(name, "no input tokens")
	case output && num("tokens_out") == 0:
		r.fail(name, "no output tokens")
	case priced && num("cost_usd") <= 0:
		r.fail(name, "cost_usd %v with prices set", line["cost_usd"])
	default:
		r.pass(name, fmt.Sprintf("in %v, cached %v, out %v, reasoning %v, cost_usd %v", line["tokens_in"],
			line["tokens_cached"], line["tokens_out"], line["tokens_reasoning"], line["cost_usd"]))
	}
}

// checkMetrics reads the admin /metrics: the chat requests, their tokens and cost,
// the rate-limit refusal, and the upstream attempts that answered them. Usage series
// carry no backend label (the ops metrics do).
func (r *run) checkMetrics() {
	status, body, err := httpGet(r.ctx, r.client, r.gw.admin+"/metrics", "")
	if err != nil || status != http.StatusOK {
		r.fail("metrics", "GET /metrics: %d %v", status, err)
		return
	}
	text := string(body)
	chat := `model="` + modelChat + `"`
	records := sumSeries(text, "kaiak_usage_records_total{", chat)
	tokensOut := sumSeries(text, "kaiak_usage_tokens_total{", chat, `unit="tokens_out"`)
	cost := sumSeries(text, "kaiak_usage_cost_usd_total{", chat)
	refused := sumSeries(text, `kaiak_errors_total{class="rate_limited"}`)
	succeeded := sumSeries(text, "kaiak_upstream_attempts_total{", `outcome="success"`)
	timed := sumSeries(text, "kaiak_upstream_attempt_duration_seconds_count{")
	backendOnUsage := false
	for line := range strings.SplitSeq(text, "\n") {
		backendOnUsage = backendOnUsage || strings.HasPrefix(line, "kaiak_usage_") && strings.Contains(line, "backend=")
	}
	priced := r.o.priceIn != 0 || r.o.priceOut != 0
	switch {
	case records < 3:
		r.fail("metrics", "kaiak_usage_records_total for %s = %v, want ≥ 3", modelChat, records)
	case tokensOut <= 0:
		r.fail("metrics", "no tokens_out for %s", modelChat)
	case priced && cost <= 0:
		r.fail("metrics", "no cost for %s", modelChat)
	case refused < 1:
		r.fail("metrics", "kaiak_errors_total{class=\"rate_limited\"} = %v, want ≥ 1", refused)
	case succeeded < 1:
		r.fail("metrics", "kaiak_upstream_attempts_total{outcome=\"success\"} = %v, want ≥ 1", succeeded)
	case timed < succeeded:
		r.fail("metrics", "kaiak_upstream_attempt_duration_seconds_count = %v, want ≥ the %v successful attempts", timed, succeeded)
	case backendOnUsage:
		r.fail("metrics", "a usage series carries a backend label")
	default:
		r.pass("metrics", fmt.Sprintf("%s: %v usage records, %v tokens out, %v USD; %v rate-limited; %v successful upstream attempts",
			modelChat, records, tokensOut, cost, refused, succeeded))
	}
}

// sumSeries adds the values of every series starting with prefix and containing
// every one of labels.
func sumSeries(text, prefix string, labels ...string) float64 {
	var sum float64
	for line := range strings.SplitSeq(text, "\n") {
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		series, value, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		matches := true
		for _, l := range labels {
			matches = matches && strings.Contains(series, l)
		}
		if v, err := strconv.ParseFloat(value, 64); matches && err == nil {
			sum += v
		}
	}
	return sum
}

// ok reports whether a request answered 200, failing the check with the status,
// body and the gateway's upstream error otherwise.
func (r *run) ok(name string, resp *response, err error, id string) bool {
	switch {
	case err != nil:
		r.fail(name, "%v", err)
	case resp.status != http.StatusOK:
		r.fail(name, "status %d: %s%s", resp.status, clip(string(resp.body)), r.upstreamHint(id))
	default:
		return true
	}
	return false
}

// upstreamHint is the gateway's view of a failed request: its error code and the
// upstream error it logged (which names the backend address, never a credential).
func (r *run) upstreamHint(id string) string {
	line, err := r.gw.logs.wait(r.ctx, msg("request", "request_id", id))
	if err != nil {
		return ""
	}
	hint := fmt.Sprintf("\ngateway: error_code=%v", line["error_code"])
	if up, ok := line["upstream_error"]; ok {
		hint += fmt.Sprintf(" upstream_error=%v", up)
	}
	return hint
}

type response struct {
	status int
	header http.Header
	body   []byte
}

func (r *run) request(key, path, id string, body any) (*http.Request, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(r.ctx, http.MethodPost, r.gw.api+path, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	if id != "" {
		req.Header.Set("X-Request-Id", id)
	}
	return req, nil
}

func (r *run) post(key, path, id string, body any) (*response, error) {
	req, err := r.request(key, path, id, body)
	if err != nil {
		return nil, err
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return &response{status: resp.StatusCode, header: resp.Header, body: data}, nil
}

func httpGet(ctx context.Context, client *http.Client, url, key string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, nil, err
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	return resp.StatusCode, body, err
}

func chatBody(model string, stream bool, extra map[string]any) map[string]any {
	body := map[string]any{"model": model, "messages": []any{map[string]any{"role": "user",
		"content": "In one short sentence, what is a lighthouse for?"}}}
	if stream {
		body["stream"] = true
	}
	for k, v := range extra {
		body[k] = v
	}
	return body
}

// clip shortens text for a one-line report.
func clip(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > 60 {
		return string(r[:60]) + "…"
	}
	return s
}
