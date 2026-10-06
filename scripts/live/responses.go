package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
)

// Request IDs of the Responses requests whose log lines the checks read.
const (
	idResponses       = "live-responses"
	idResponsesStream = "live-responses-stream"
	idResponsesCount  = "live-responses-count"
	idResponsesState  = "live-responses-stateful"
	idResponsesTool   = "live-responses-tool"
	idResponsesTier   = "live-responses-tier"
)

// responsesChecks are the Responses checks, for the kinds that serve it.
func (r *run) responsesChecks() {
	r.checkResponses()
	r.checkUsageLog("usage-log/responses", idResponses, true)
	r.checkResponsesStream()
	r.checkUsageLog("usage-log/resp-stream", idResponsesStream, true)
	if r.o.serves(epInputTokens) {
		r.checkInputTokens()
	}
	r.checkResponsesStateful()
	r.checkResponsesHostedTool()
	if r.o.kind == kindOpenAI || r.o.kind == kindAzure {
		r.checkServiceTier()
	}
}

// responsesBody is a Responses request to model with the -responses-params added,
// then extra. It sets no max_output_tokens: the gateway fills the model's
// output-limit default.
func (r *run) responsesBody(model string, stream bool, extra map[string]any) map[string]any {
	body := withParams(map[string]any{"model": model, "input": "In one short sentence, what is a lighthouse for?"},
		r.o.responsesParamsObj)
	if stream {
		body["stream"] = true
	}
	return withParams(body, extra)
}

// outputText is the text of a Responses answer's message items, and whether it
// carried a reasoning item.
func outputText(output any) (text string, reasoning bool) {
	items, _ := output.([]any)
	for _, item := range items {
		switch str(item, "type") {
		case "message":
			parts, _ := field(item, "content").([]any)
			for _, p := range parts {
				if str(p, "type") == "output_text" {
					text += str(p, "text")
				}
			}
		case "reasoning":
			reasoning = true
		}
	}
	return text, reasoning
}

// checkResponses sends a Responses request asking to be stored: the gateway sends it
// with store false. OpenAI and Azure report back what they were sent; the self-hosted
// servers' answers carry no store.
func (r *run) checkResponses() {
	const name = "responses"
	resp, err := r.post(r.key, "/v1/responses", idResponses, r.responsesBody(modelChat, false, map[string]any{"store": true}))
	if !r.ok(name, resp, err, idResponses) {
		return
	}
	var a map[string]any
	if err := json.Unmarshal(resp.body, &a); err != nil || str(a, "object") != "response" {
		r.fail(name, "not a response: %s", clip(string(resp.body)))
		return
	}
	text, reasoning := outputText(a["output"])
	store, reported := a["store"]
	cloud := r.o.kind == kindOpenAI || r.o.kind == kindAzure
	switch {
	case str(a, "model") != modelChat:
		r.fail(name, "answer names model %q, want the public name %q", str(a, "model"), modelChat)
	case str(a, "status") == "incomplete" && text == "" && reasoning:
		r.fail(name, "only reasoning came back: raise -max-output, or lower the effort with -responses-params")
	case text == "":
		r.fail(name, "no output text (status %q): %s", str(a, "status"), clip(string(resp.body)))
	case field(a, "usage", "input_tokens") == nil:
		r.fail(name, "no usage in the answer: %s", clip(string(resp.body)))
	case reported && store != false:
		r.fail(name, "the answer's store is %v: the gateway must send store false", store)
	case cloud && !reported:
		r.fail(name, "%s's answer carries no store", r.o.kind)
	default:
		detail := fmt.Sprintf("%q (status %s, %v+%v tokens)", clip(text), str(a, "status"),
			num(a, "usage", "input_tokens"), num(a, "usage", "output_tokens"))
		if reported {
			detail += ", sent with store false"
		}
		r.pass(name, detail)
	}
}

// checkResponsesStream streams a Responses answer: response.created first, text
// deltas, response.completed last, every model the events name the public one, no
// error event and no [DONE].
func (r *run) checkResponsesStream() {
	const name = "responses-stream"
	req, err := r.request(r.key, "/v1/responses", idResponsesStream, r.responsesBody(modelChat, true, nil))
	if err != nil {
		r.fail(name, "%v", err)
		return
	}
	resp, problem := r.openTypedStream(req, idResponsesStream)
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
	var text strings.Builder
	badModel := ""
	for _, ev := range events {
		if ev.Type == "response.output_text.delta" {
			text.WriteString(str(ev.Data, "delta"))
		}
		if m := str(ev.Data, "response", "model"); m != "" && m != modelChat && badModel == "" {
			badModel = m
		}
	}
	failure := slices.IndexFunc(events, func(ev typedEvent) bool {
		return ev.Type == "error" || ev.Type == "response.failed"
	})
	switch {
	case len(events) == 0:
		r.fail(name, "no events")
	case events[0].Type != "response.created":
		r.fail(name, "first event %q, want response.created", events[0].Type)
	case failure >= 0:
		r.fail(name, "%s event: %s", events[failure].Type, clip(events[failure].Raw))
	case badModel != "":
		r.fail(name, "an event names model %q, want %q", badModel, modelChat)
	case events[len(events)-1].Type == "response.incomplete":
		r.fail(name, "response.incomplete (%s): raise -max-output, or lower the effort with -responses-params",
			str(events[len(events)-1].Data, "response", "incomplete_details", "reason"))
	case events[len(events)-1].Type != "response.completed":
		r.fail(name, "last event %q, want response.completed", events[len(events)-1].Type)
	case str(events[len(events)-1].Data, "response", "model") != modelChat:
		r.fail(name, "response.completed names model %q, want %q", str(events[len(events)-1].Data, "response", "model"), modelChat)
	case text.Len() == 0:
		r.fail(name, "no output text in %d events", len(events))
	default:
		r.pass(name, fmt.Sprintf("%d events, response.created … response.completed, %q", len(events), clip(text.String())))
	}
}

// checkInputTokens counts a Responses request's input: an answer with input_tokens,
// and no usage record.
func (r *run) checkInputTokens() {
	const name = "responses-count"
	resp, err := r.post(r.key, "/v1/responses/input_tokens", idResponsesCount, r.responsesBody(modelChat, false, nil))
	if !r.ok(name, resp, err, idResponsesCount) {
		return
	}
	var a map[string]any
	_ = json.Unmarshal(resp.body, &a)
	if num(a, "input_tokens") <= 0 {
		r.fail(name, "no input_tokens: %s", clip(string(resp.body)))
		return
	}
	if problem := r.noUsageRecord(idResponsesCount); problem != "" {
		r.fail(name, "%s", problem)
		return
	}
	r.pass(name, fmt.Sprintf("%v input tokens, no usage record", a["input_tokens"]))
}

// checkResponsesStateful: a follow-up naming a stored response is refused before
// routing — the gateway serves Responses stateless.
func (r *run) checkResponsesStateful() {
	const name = "responses-stateful"
	body := r.responsesBody(modelChat, false, map[string]any{"previous_response_id": "resp_live_earlier"})
	resp, err := r.post(r.key, "/v1/responses", idResponsesState, body)
	if err != nil {
		r.fail(name, "%v", err)
		return
	}
	if problem := openAIError(resp, http.StatusBadRequest, "stateful_responses_unsupported", "previous_response_id"); problem != "" {
		r.fail(name, "%s", problem)
		return
	}
	if sent, err := r.notSent(idResponsesState); err != nil || !sent {
		r.fail(name, "refused, but the log line shows it routed to a backend (%v)", err)
		return
	}
	r.pass(name, "previous_response_id → 400 stateful_responses_unsupported, never sent")
}

// checkResponsesHostedTool: a tool the backend would run (OpenAI's web search) is
// refused before routing.
func (r *run) checkResponsesHostedTool() {
	const name = "responses-hosted-tool"
	body := r.responsesBody(modelChat, false, map[string]any{"tools": []any{map[string]any{"type": "web_search"}}})
	resp, err := r.post(r.key, "/v1/responses", idResponsesTool, body)
	if err != nil {
		r.fail(name, "%v", err)
		return
	}
	if problem := openAIError(resp, http.StatusBadRequest, "hosted_tool_unsupported"); problem != "" {
		r.fail(name, "%s", problem)
		return
	}
	if sent, err := r.notSent(idResponsesTool); err != nil || !sent {
		r.fail(name, "refused, but the log line shows it routed to a backend (%v)", err)
		return
	}
	r.pass(name, "web_search → 400 hosted_tool_unsupported, never sent")
}

// checkServiceTier asks OpenAI or Azure for priority processing: the gateway sends
// the default tier, which the answer reports when the backend reports its tier.
func (r *run) checkServiceTier() {
	const name = "service-tier"
	body := r.responsesBody(modelChat, false, map[string]any{"service_tier": "priority", "max_output_tokens": 16})
	resp, err := r.post(r.key, "/v1/responses", idResponsesTier, body)
	if !r.ok(name, resp, err, idResponsesTier) {
		return
	}
	var a map[string]any
	_ = json.Unmarshal(resp.body, &a)
	tier, reported := a["service_tier"]
	switch {
	case !reported:
		r.skip(name, "the answer does not report its service tier")
	case tier != "default":
		r.fail(name, "asked for priority, ran on %v: the gateway must send the default tier", tier)
	default:
		r.pass(name, "asked for priority, ran on default")
	}
}

// checkNotServed: the APIs the kind's backend type does not serve are refused with
// endpoint_not_served, each in its API's error shape.
func (r *run) checkNotServed() {
	const name = "endpoint-not-served"
	openAI := func(path string, body map[string]any) func() string {
		return func() string {
			resp, err := r.post(r.key, path, "", body)
			if err != nil {
				return err.Error()
			}
			return openAIError(resp, http.StatusBadRequest, "endpoint_not_served")
		}
	}
	anthropic := func(path string, body map[string]any) func() string {
		return func() string {
			resp, err := r.postAnthropic(r.key, path, "", body)
			if err != nil {
				return err.Error()
			}
			return anthropicError(resp, http.StatusBadRequest, "invalid_request_error", "endpoint_not_served")
		}
	}
	type refusal struct {
		path  string
		check func() string
	}
	var want []refusal
	if !r.o.serves(epChat) {
		want = append(want, refusal{"/v1/chat/completions", openAI("/v1/chat/completions", r.chatBody(modelChat, false, nil))})
	}
	if !r.o.serves(epMessages) {
		want = append(want, refusal{"/v1/messages", anthropic("/v1/messages", r.messagesBody(modelChat, false, nil))})
	}
	if !r.o.serves(epResponses) {
		want = append(want, refusal{"/v1/responses", openAI("/v1/responses", r.responsesBody(modelChat, false, nil))})
	} else if !r.o.serves(epInputTokens) {
		want = append(want, refusal{"/v1/responses/input_tokens",
			openAI("/v1/responses/input_tokens", r.responsesBody(modelChat, false, nil))})
	}
	if len(want) == 0 {
		r.skip(name, r.o.kind+" serves every endpoint the kit checks")
		return
	}
	var refused []string
	for _, w := range want {
		if problem := w.check(); problem != "" {
			r.fail(name, "%s: %s", w.path, problem)
			return
		}
		refused = append(refused, w.path)
	}
	r.pass(name, strings.Join(refused, ", ")+" → 400 endpoint_not_served")
}
