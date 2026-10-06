package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
)

// Request IDs of the Messages requests whose log lines the checks read.
const (
	idMessages       = "live-messages"
	idMessagesStream = "live-messages-stream"
	idMessagesCount  = "live-messages-count"
	idMessagesTool   = "live-messages-tool"
	idMessagesPrice  = "live-messages-price"
	idMessagesCache1 = "live-messages-cache-1"
	idMessagesCache2 = "live-messages-cache-2"
)

// messagesChecks are the Messages checks, for the kinds that serve it.
func (r *run) messagesChecks() {
	r.checkMessages()
	r.checkUsageLog("usage-log/messages", idMessages, true)
	r.checkMessagesStream()
	r.checkUsageLog("usage-log/msg-stream", idMessagesStream, true)
	r.checkMessagesCache()
	r.checkCountTokens()
	r.checkAnthropicModels()
	r.checkMessagesErrors()
	r.checkMessagesHostedTool()
	r.checkPriceOptions()
}

// messagesBody is a Messages request to model with the -messages-params added, then
// extra. It sets no max_tokens: the gateway fills the model's output-limit default.
func (r *run) messagesBody(model string, stream bool, extra map[string]any) map[string]any {
	body := withParams(map[string]any{"model": model, "messages": []any{map[string]any{"role": "user",
		"content": "In one short sentence, what is a lighthouse for?"}}}, r.o.messagesParamsObj)
	if stream {
		body["stream"] = true
	}
	return withParams(body, extra)
}

// messageText is the text and thinking of a Messages answer's content blocks.
func messageText(content any) (text, thinking string) {
	blocks, _ := content.([]any)
	for _, b := range blocks {
		switch str(b, "type") {
		case "text":
			text += str(b, "text")
		case "thinking":
			thinking += str(b, "thinking")
		}
	}
	return text, thinking
}

// checkMessages sends a Messages request without max_tokens (the API requires it; the
// gateway sets the output-limit default) and reads the answer: the public model name,
// text, usage.
func (r *run) checkMessages() {
	resp, err := r.postAnthropic(r.key, "/v1/messages", idMessages, r.messagesBody(modelChat, false, nil))
	if !r.ok("messages", resp, err, idMessages) {
		return
	}
	var a map[string]any
	if err := json.Unmarshal(resp.body, &a); err != nil || str(a, "type") != "message" {
		r.fail("messages", "not a message: %s", clip(string(resp.body)))
		return
	}
	text, thinking := messageText(a["content"])
	switch {
	case str(a, "model") != modelChat:
		r.fail("messages", "answer names model %q, want the public name %q", str(a, "model"), modelChat)
	case text == "" && thinking != "":
		r.fail("messages", "only thinking came back (stop_reason %q): raise -max-output, or switch thinking off with -messages-params", str(a, "stop_reason"))
	case text == "":
		r.fail("messages", "no text: %s", clip(string(resp.body)))
	case field(a, "usage", "input_tokens") == nil:
		r.fail("messages", "no usage in the answer: %s", clip(string(resp.body)))
	default:
		r.pass("messages", fmt.Sprintf("%q (stop %s, %v+%v tokens), max_tokens set by the gateway", clip(text),
			str(a, "stop_reason"), num(a, "usage", "input_tokens"), num(a, "usage", "output_tokens")))
	}
}

// checkMessagesStream streams a Messages answer: message_start first, naming the
// public model, text deltas, message_stop last, no error event and no [DONE].
func (r *run) checkMessagesStream() {
	const name = "messages-stream"
	req, err := r.anthropicRequest(r.key, "/v1/messages", idMessagesStream, r.messagesBody(modelChat, true, nil))
	if err != nil {
		r.fail(name, "%v", err)
		return
	}
	resp, problem := r.openTypedStream(req, idMessagesStream)
	if problem != "" {
		r.fail(name, "%s", problem)
		return
	}
	defer resp.Body.Close()
	events, err := readTypedEvents(resp.Body)
	if err != nil {
		r.fail(name, "%v", err)
		return
	}
	var text, thinking strings.Builder
	for _, ev := range events {
		if ev.Type == "content_block_delta" {
			text.WriteString(str(ev.Data, "delta", "text"))
			thinking.WriteString(str(ev.Data, "delta", "thinking"))
		}
	}
	errIndex := slices.IndexFunc(events, func(ev typedEvent) bool { return ev.Type == "error" })
	switch {
	case len(events) == 0:
		r.fail(name, "no events")
	case events[0].Type != "message_start":
		r.fail(name, "first event %q, want message_start", events[0].Type)
	case str(events[0].Data, "message", "model") != modelChat:
		r.fail(name, "message_start names model %q, want %q", str(events[0].Data, "message", "model"), modelChat)
	case errIndex >= 0:
		r.fail(name, "error event: %s", clip(events[errIndex].Raw))
	case events[len(events)-1].Type != "message_stop":
		r.fail(name, "last event %q, want message_stop", events[len(events)-1].Type)
	case text.Len() == 0 && thinking.Len() > 0:
		r.fail(name, "only thinking streamed: raise -max-output, or switch thinking off with -messages-params")
	case text.Len() == 0:
		r.fail(name, "no text in %d events", len(events))
	default:
		r.pass(name, fmt.Sprintf("%d events, message_start … message_stop, %q", len(events), clip(text.String())))
	}
}

// cachedPromptText is a system prompt long enough for any model's prompt cache: about
// 8000 tokens, above the largest minimum Anthropic sets for a cached prefix.
var cachedPromptText = strings.Repeat("The lighthouse keeper writes the weather into the logbook every hour, "+
	"trims the wick, polishes the lens and watches the sea for ships. ", 320)

// checkMessagesCache sends the same long system prompt twice, marked for the prompt
// cache: the second request's log line must report input read from the cache. A
// self-hosted server reports cache reads only with its prefix cache on, so there a
// missing read is a skip; Anthropic's API caches every marked prefix this long.
func (r *run) checkMessagesCache() {
	const name = "messages-cache"
	body := func() map[string]any {
		return r.messagesBody(modelChat, false, map[string]any{"max_tokens": 16, "system": []any{map[string]any{
			"type": "text", "text": cachedPromptText, "cache_control": map[string]any{"type": "ephemeral"}}}})
	}
	var lines []map[string]any
	for _, id := range []string{idMessagesCache1, idMessagesCache2} {
		resp, err := r.postAnthropic(r.key, "/v1/messages", id, body())
		if !r.ok(name, resp, err, id) {
			return
		}
		line, err := r.gw.logs.wait(r.ctx, msg("request", "kaiak.request.id", id))
		if err != nil {
			r.fail(name, "no log line for request %s: %v", id, err)
			return
		}
		lines = append(lines, line)
	}
	written, read := lines[0]["gen_ai.usage.cache_write.input_tokens"], lines[1]["gen_ai.usage.cache_read.input_tokens"]
	switch {
	case num(read) > 0:
		r.pass(name, fmt.Sprintf("first request wrote %v, second read %v of %v input tokens", written, read,
			lines[1]["gen_ai.usage.input_tokens"]))
	case anthropicKind(r.o.kind):
		r.fail(name, "the second request read nothing from the cache (first wrote %v)", written)
	default:
		r.skip(name, "the server reported no cache read: its prefix cache is off or does not report it")
	}
}

// checkCountTokens counts a Messages request's input: an answer with input_tokens,
// and no usage record (nothing is billed).
func (r *run) checkCountTokens() {
	const name = "messages-count"
	body := r.messagesBody(modelChat, false, nil)
	resp, err := r.postAnthropic(r.key, "/v1/messages/count_tokens", idMessagesCount, body)
	if !r.ok(name, resp, err, idMessagesCount) {
		return
	}
	var a map[string]any
	_ = json.Unmarshal(resp.body, &a)
	if num(a, "input_tokens") <= 0 {
		r.fail(name, "no input_tokens: %s", clip(string(resp.body)))
		return
	}
	if problem := r.noUsageRecord(idMessagesCount); problem != "" {
		r.fail(name, "%s", problem)
		return
	}
	r.pass(name, fmt.Sprintf("%v input tokens, no usage record", a["input_tokens"]))
}

// checkAnthropicModels reads the model list as Anthropic's SDKs ask for it: the same
// models, in Anthropic's shape.
func (r *run) checkAnthropicModels() {
	const name = "messages-models"
	req, err := http.NewRequestWithContext(r.ctx, http.MethodGet, r.gw.api+"/v1/models", nil)
	if err != nil {
		r.fail(name, "%v", err)
		return
	}
	req.Header.Set("X-Api-Key", r.key)
	req.Header.Set("Anthropic-Version", anthropicVersion)
	resp, err := r.do(req)
	if err != nil || resp.status != http.StatusOK {
		r.fail(name, "GET /v1/models: %v %v", resp, err)
		return
	}
	var list struct {
		Data []struct {
			Type, ID string
		}
		HasMore bool   `json:"has_more"`
		FirstID string `json:"first_id"`
	}
	if err := json.Unmarshal(resp.body, &list); err != nil {
		r.fail(name, "not a model list: %s", clip(string(resp.body)))
		return
	}
	var ids []string
	for _, m := range list.Data {
		if m.Type != "model" {
			r.fail(name, "entry %s has type %q, want model", m.ID, m.Type)
			return
		}
		ids = append(ids, m.ID)
	}
	// The models whose deployments serve Messages: the chat models, and the embeddings
	// model when it shares their backend.
	want := []string{modelCapped, modelChat, modelRPM}
	if r.o.embeddingsModel != "" && !r.o.embeddingsServer() {
		want = append(want, modelEmbed)
		slices.Sort(want)
	}
	switch {
	case !slices.Equal(ids, want):
		r.fail(name, "lists %v, want %v", ids, want)
	case list.HasMore || list.FirstID != want[0]:
		r.fail(name, "has_more %v, first_id %q: want one page from %s", list.HasMore, list.FirstID, want[0])
	default:
		r.pass(name, "Anthropic's shape: "+strings.Join(ids, ", "))
	}
}

// checkMessagesErrors: a wrong key and an unknown model are answered in Anthropic's
// error shape, with kaiak's code.
func (r *run) checkMessagesErrors() {
	const name = "messages-errors"
	resp, err := r.postAnthropic("kaiak-not-a-key", "/v1/messages", "", r.messagesBody(modelChat, false, nil))
	if err != nil {
		r.fail(name, "%v", err)
		return
	}
	if problem := anthropicError(resp, http.StatusUnauthorized, "authentication_error", "invalid_api_key"); problem != "" {
		r.fail(name, "wrong key: %s", problem)
		return
	}
	resp, err = r.postAnthropic(r.key, "/v1/messages", "", r.messagesBody("live-no-such-model", false, nil))
	if err != nil {
		r.fail(name, "%v", err)
		return
	}
	if problem := anthropicError(resp, http.StatusNotFound, "not_found_error", "model_not_found"); problem != "" {
		r.fail(name, "unknown model: %s", problem)
		return
	}
	r.pass(name, "wrong key → 401 authentication_error, unknown model → 404 not_found_error")
}

// checkMessagesHostedTool: a tool the backend would run (Anthropic's web search) is
// refused before routing.
func (r *run) checkMessagesHostedTool() {
	const name = "messages-hosted-tool"
	body := r.messagesBody(modelChat, false, map[string]any{"tools": []any{map[string]any{
		"type": "web_search_20250305", "name": "web_search", "max_uses": 1}}})
	resp, err := r.postAnthropic(r.key, "/v1/messages", idMessagesTool, body)
	if err != nil {
		r.fail(name, "%v", err)
		return
	}
	if problem := anthropicError(resp, http.StatusBadRequest, "invalid_request_error", "hosted_tool_unsupported"); problem != "" {
		r.fail(name, "%s", problem)
		return
	}
	if sent, err := r.notSent(idMessagesTool); err != nil || !sent {
		r.fail(name, "refused, but the log line shows it routed to a backend (%v)", err)
		return
	}
	r.pass(name, "web_search_20250305 → 400 hosted_tool_unsupported, never sent")
}

// checkPriceOptions: on the Anthropic kinds a request for a priced option (fast mode)
// is refused before it is sent; the self-hosted kinds price nothing and pass it.
func (r *run) checkPriceOptions() {
	const name = "price-options"
	body := r.messagesBody(modelChat, false, map[string]any{"max_tokens": 16, "speed": "fast"})
	resp, err := r.postAnthropic(r.key, "/v1/messages", idMessagesPrice, body)
	if err != nil {
		r.fail(name, "%v", err)
		return
	}
	if !anthropicKind(r.o.kind) {
		if !r.ok(name, resp, err, idMessagesPrice) {
			return
		}
		r.pass(name, `speed "fast" passed through: `+r.o.kind+" prices nothing")
		return
	}
	if problem := anthropicError(resp, http.StatusBadRequest, "invalid_request_error", "price_option_unsupported"); problem != "" {
		r.fail(name, "%s", problem)
		return
	}
	detail := `speed "fast" → 400 price_option_unsupported`
	if r.o.kind == kindAnthropic {
		detail += `; service_tier "standard_only" accepted (the messages check)`
	}
	r.pass(name, detail)
}

// checkMessagesCeiling is the output-ceiling check through Messages, for the kinds
// serving no chat completions: max_tokens far above the capped model's ceiling is
// lowered to it, so the answer stops at max_tokens.
func (r *run) checkMessagesCeiling() {
	const name = "output-ceiling"
	body := withParams(map[string]any{"model": modelCapped, "max_tokens": r.o.contextLength, "messages": []any{
		map[string]any{"role": "user", "content": "Write a long story, at least 600 words, about a lighthouse keeper."}}},
		r.o.messagesParamsObj)
	resp, err := r.postAnthropic(r.key, "/v1/messages", "live-ceiling", body)
	if !r.ok(name, resp, err, "live-ceiling") {
		return
	}
	var a map[string]any
	_ = json.Unmarshal(resp.body, &a)
	out := num(a, "usage", "output_tokens")
	switch {
	case field(a, "usage") == nil:
		r.fail(name, "no usage: %s", clip(string(resp.body)))
	case out > float64(r.o.ceiling):
		r.fail(name, "%v output tokens, above the ceiling %d", out, r.o.ceiling)
	case str(a, "stop_reason") != "max_tokens":
		r.fail(name, "stop_reason %q, want max_tokens (%v tokens)", str(a, "stop_reason"), out)
	default:
		r.pass(name, fmt.Sprintf("max_tokens %d → %v tokens, stop max_tokens", r.o.contextLength, out))
	}
}

// checkMessagesRateLimit is the rate-limit check through Messages: the second request
// with the metered key is refused in Anthropic's shape, with the rate-limit headers.
func (r *run) checkMessagesRateLimit() {
	const name = "rate-limit"
	resp, err := r.postAnthropic(r.meteredKey, "/v1/messages", "live-rpm-1", r.messagesBody(modelRPM, false, nil))
	if !r.ok(name, resp, err, "live-rpm-1") {
		return
	}
	resp, err = r.postAnthropic(r.meteredKey, "/v1/messages", "live-rpm-2", r.messagesBody(modelRPM, false, nil))
	if err != nil {
		r.fail(name, "%v", err)
		return
	}
	if problem := anthropicError(resp, http.StatusTooManyRequests, "rate_limit_error", "rate_limit_exceeded"); problem != "" {
		r.fail(name, "second request: %s", problem)
		return
	}
	h := resp.header
	if h.Get("Retry-After") == "" || h.Get("X-Ratelimit-Limit-Requests") != "1" {
		r.fail(name, "Retry-After %q, x-ratelimit-limit-requests %q", h.Get("Retry-After"), h.Get("X-Ratelimit-Limit-Requests"))
		return
	}
	r.pass(name, "second request → 429 rate_limit_error, Retry-After "+h.Get("Retry-After"))
}
