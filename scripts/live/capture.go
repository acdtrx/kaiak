package main

import (
	"bufio"
	"cmp"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"
)

// The capture checks (docs/specs/GATEWAY.md, Request capture). The capture key's
// group, live-capture, has capture on; every other group of the config has it off.
// Each chat API the kind serves is asked a question and a question for a tool it
// offers, each whole and streamed, under one Kaiak-Session-Id per run. The spans are
// read once the gateway has exited: its exit flush sends every capture it built, so
// no check waits on the trace export's one-second send.
const (
	groupCapture = "live-capture"
	keyCapture   = "k-live-capture"

	captureSystem     = "Answer in one short sentence."
	capturePrompt     = "What is a lighthouse for?"
	captureToolPrompt = "What is the weather in Oslo right now? Use the get_weather tool."
	captureTool       = "get_weather"
	captureToolText   = "The current weather in a city"
)

// captureToolSchema is the offered tool's parameters.
var captureToolSchema = map[string]any{"type": "object",
	"properties": map[string]any{"city": map[string]any{"type": "string"}}, "required": []any{"city"}}

// captureAPIs are the chat APIs — the endpoints the gateway captures — each with its
// path and the short name its checks carry.
var captureAPIs = []struct{ endpoint, path, tag string }{
	{epChat, "/v1/chat/completions", "chat"}, {epMessages, "/v1/messages", "msg"}, {epResponses, "/v1/responses", "resp"}}

// captureExchange is one capture check's request and what the live-test kit
// received for it.
type captureExchange struct {
	name, id     string
	endpoint     string
	tool, stream bool
	body         map[string]any
	// problem is why the request itself failed; "" when it was answered.
	problem string
	// answer is the answer the live-test kit received, as it reads it.
	answer answerOutput
}

// answerOutput is an answer read by the live-test kit into what its capture must
// carry (docs/specs/GATEWAY.md, Request capture → Content, Streams): its ID and
// model, its output messages in the GenAI conventions' format — decoded JSON, to
// compare with the span's — and their finish reasons, "" for one that gave none.
type answerOutput struct {
	id, model     string
	messages      []any
	finishReasons []string
}

// newSessionID is the run's Kaiak-Session-Id: a tool groups the run's captures by it.
func newSessionID(label string) string {
	raw := make([]byte, 4)
	_, _ = rand.Read(raw) // crypto/rand.Read never fails
	return "live-" + label + "-" + hex.EncodeToString(raw)
}

// sendCaptures sends the capture checks' requests with the capture key and reads
// their answers; checkCaptures reads their spans after the gateway's exit.
func (r *run) sendCaptures() {
	for _, api := range captureAPIs {
		if !r.o.serves(api.endpoint) {
			continue
		}
		for _, tool := range []bool{false, true} {
			for _, stream := range []bool{false, true} {
				suffix := api.tag
				if tool {
					suffix += "-tool"
				}
				if stream {
					suffix += "-stream"
				}
				c := &captureExchange{name: "capture/" + suffix, id: "live-capture-" + suffix, endpoint: api.endpoint,
					tool: tool, stream: stream, body: r.captureBody(api.endpoint, tool, stream)}
				c.problem = r.sendCapture(c, api.path)
				r.captures = append(r.captures, c)
			}
		}
	}
}

// captureBody is a capture check's request: a system prompt and a question — for the
// weather with the tool offered — with the API's -…-params added. It sets no output
// limit: the gateway fills the model's default, which the capture must not show.
func (r *run) captureBody(endpoint string, tool, stream bool) map[string]any {
	prompt := capturePrompt
	if tool {
		prompt = captureToolPrompt
	}
	user := []any{map[string]any{"role": "user", "content": prompt}}
	var body map[string]any
	var tools []any
	switch endpoint {
	case epChat:
		body = r.withChatParams(map[string]any{"model": modelChat,
			"messages": append([]any{map[string]any{"role": "system", "content": captureSystem}}, user...)})
		tools = []any{map[string]any{"type": "function", "function": map[string]any{"name": captureTool,
			"description": captureToolText, "parameters": captureToolSchema}}}
	case epMessages:
		body = withParams(map[string]any{"model": modelChat, "system": captureSystem, "messages": user},
			r.o.messagesParamsObj)
		tools = []any{map[string]any{"name": captureTool, "description": captureToolText, "input_schema": captureToolSchema}}
	default:
		body = withParams(map[string]any{"model": modelChat, "instructions": captureSystem, "input": prompt},
			r.o.responsesParamsObj)
		tools = []any{map[string]any{"type": "function", "name": captureTool, "description": captureToolText,
			"parameters": captureToolSchema}}
	}
	if tool {
		body["tools"] = tools
	}
	if stream {
		body["stream"] = true
	}
	return body
}

// sendCapture sends one capture check's request and reads its answer into c.answer;
// it returns why the request failed, "" when it was answered.
func (r *run) sendCapture(c *captureExchange, path string) string {
	var req *http.Request
	var err error
	if c.endpoint == epMessages {
		req, err = r.anthropicRequest(r.captureKey, path, c.id, c.body)
	} else {
		req, err = r.request(r.captureKey, path, c.id, c.body)
	}
	if err != nil {
		return err.Error()
	}
	req.Header.Set("Kaiak-Session-Id", r.session)
	if c.stream {
		resp, problem := r.openTypedStream(req, c.id)
		if problem != "" {
			return problem
		}
		defer resp.Body.Close()
		events, err := readEventData(resp.Body)
		if err == nil {
			c.answer, err = streamOutput(c.endpoint, events)
		}
		if err != nil {
			return err.Error()
		}
		return ""
	}
	resp, err := r.do(req)
	switch {
	case err != nil:
		return err.Error()
	case resp.status != http.StatusOK:
		return fmt.Sprintf("status %d: %s%s", resp.status, clip(string(resp.body)), r.upstreamHint(c.id))
	}
	var top map[string]any
	if err := json.Unmarshal(resp.body, &top); err != nil {
		return "the answer is not a JSON object: " + clip(string(resp.body))
	}
	switch c.endpoint {
	case epChat:
		c.answer = chatOutput(top)
	case epMessages:
		c.answer = messagesOutput(top)
	default:
		c.answer = responseOutput(top)
	}
	return ""
}

// readEventData reads an event stream to its end: each event's data, in order.
func readEventData(body io.Reader) ([]string, error) {
	var events []string
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for sc.Scan() {
		if data, ok := strings.CutPrefix(sc.Text(), "data:"); ok {
			events = append(events, strings.TrimPrefix(data, " "))
		}
	}
	if err := sc.Err(); err != nil {
		return events, fmt.Errorf("stream broke off after %d events: %w", len(events), err)
	}
	return events, nil
}

// streamOutput reads a streamed answer of the API into what the same answer
// unstreamed gives.
func streamOutput(endpoint string, events []string) (answerOutput, error) {
	decoded := make([]map[string]any, 0, len(events))
	for _, data := range events {
		if strings.TrimSpace(data) == "[DONE]" && endpoint == epChat {
			return chatStreamOutput(decoded), nil
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			return answerOutput{}, fmt.Errorf("event data is not a JSON object: %s", clip(data))
		}
		decoded = append(decoded, ev)
	}
	switch endpoint {
	case epChat:
		return answerOutput{}, errors.New("the stream ended without [DONE]")
	case epMessages:
		return messagesStreamOutput(decoded)
	}
	return responsesStreamOutput(decoded)
}

// The GenAI conventions' parts and messages, as decoded JSON.

// appendTextPart appends text as a part of kind — text or reasoning — unless it is
// empty: an empty text is no part.
func appendTextPart(parts []any, kind, text string) []any {
	if text == "" {
		return parts
	}
	return append(parts, map[string]any{"type": kind, "content": text})
}

// toolCallPart is a call of a tool: its ID when it has one, and its arguments when
// it has some (has).
func toolCallPart(id, name string, arguments any, has bool) map[string]any {
	p := map[string]any{"type": "tool_call", "name": name}
	if id != "" {
		p["id"] = id
	}
	if has {
		p["arguments"] = arguments
	}
	return p
}

// toolArguments is a call's arguments member as a capture carries it: a string as
// stringArguments reads it, any other value itself; none when absent or null.
func toolArguments(v any) (any, bool) {
	if v == nil {
		return nil, false
	}
	if s, ok := v.(string); ok {
		return stringArguments(s), true
	}
	return v, true
}

// stringArguments is arguments given as a string: the JSON it holds when it is
// valid JSON, else the string itself.
func stringArguments(s string) any {
	var v any
	if json.Unmarshal([]byte(s), &v) == nil {
		return v
	}
	return s
}

// outputMessage is an output message: role, and parts — an empty list for none.
func outputMessage(role string, parts []any) map[string]any {
	if parts == nil {
		parts = []any{}
	}
	return map[string]any{"role": role, "parts": parts}
}

// integral reads a JSON number that is an integer.
func integral(v any) (float64, bool) {
	n, ok := v.(float64)
	return n, ok && n == math.Trunc(n)
}

// chatReasoning is a chat message's or delta's reasoning: reasoning_content when it
// has text, else reasoning.
func chatReasoning(m any) string {
	return cmp.Or(str(m, "reasoning_content"), str(m, "reasoning"))
}

// chatOutput reads a chat completion: each choice one output message, in index
// order — its reasoning, its text and refusal, its tool calls.
func chatOutput(top map[string]any) answerOutput {
	out := answerOutput{id: str(top, "id"), model: str(top, "model")}
	type choice struct {
		index   float64
		message map[string]any
		finish  string
	}
	var choices []choice
	list, _ := top["choices"].([]any)
	for _, raw := range list {
		index, ok := integral(field(raw, "index"))
		m, isMessage := field(raw, "message").(map[string]any)
		if !ok || !isMessage {
			continue
		}
		choices = append(choices, choice{index, outputMessage(cmp.Or(str(m, "role"), "assistant"), chatParts(m)),
			str(raw, "finish_reason")})
	}
	slices.SortStableFunc(choices, func(a, b choice) int { return cmp.Compare(a.index, b.index) })
	for _, c := range choices {
		out.messages = append(out.messages, c.message)
		out.finishReasons = append(out.finishReasons, c.finish)
	}
	return out
}

// chatParts are a chat answer message's parts.
func chatParts(m map[string]any) []any {
	parts := appendTextPart(nil, "reasoning", chatReasoning(m))
	switch content := m["content"].(type) {
	case string:
		parts = appendTextPart(parts, "text", content)
	case []any:
		for _, p := range content {
			switch typ := str(p, "type"); typ {
			case "":
			case "text", "refusal":
				parts = appendTextPart(parts, "text", str(p, typ))
			default:
				parts = append(parts, p)
			}
		}
	}
	parts = appendTextPart(parts, "text", str(m, "refusal"))
	calls, _ := m["tool_calls"].([]any)
	for _, raw := range calls {
		call, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if fn, ok := call["function"].(map[string]any); ok {
			if name, ok := fn["name"].(string); ok {
				arguments, has := toolArguments(fn["arguments"])
				parts = append(parts, toolCallPart(str(call, "id"), name, arguments, has))
			}
			continue
		}
		if str(call, "type") != "" {
			parts = append(parts, call)
		}
	}
	return parts
}

// chatStreamOutput reads a chat completion stream up to [DONE]: the chunks gathered
// by choice index, each delta's text concatenated, tool calls by their own index with
// the ID and name of the fragments that first carry them.
func chatStreamOutput(chunks []map[string]any) answerOutput {
	type call struct {
		id, name          string
		function, hasArgs bool
		arguments         strings.Builder
	}
	type choice struct {
		role, finish                string
		reasoning, content, refusal strings.Builder
		calls                       map[float64]*call
	}
	var out answerOutput
	choices := map[float64]*choice{}
	for _, chunk := range chunks {
		out.id = cmp.Or(out.id, str(chunk, "id"))
		out.model = cmp.Or(out.model, str(chunk, "model"))
		list, _ := chunk["choices"].([]any)
		for _, raw := range list {
			index, ok := integral(field(raw, "index"))
			if !ok {
				continue
			}
			c := choices[index]
			if c == nil {
				c = &choice{calls: map[float64]*call{}}
				choices[index] = c
			}
			c.finish = cmp.Or(str(raw, "finish_reason"), c.finish)
			delta, ok := field(raw, "delta").(map[string]any)
			if !ok {
				continue
			}
			c.role = cmp.Or(c.role, str(delta, "role"))
			c.reasoning.WriteString(chatReasoning(delta))
			c.content.WriteString(str(delta, "content"))
			c.refusal.WriteString(str(delta, "refusal"))
			fragments, _ := delta["tool_calls"].([]any)
			for _, f := range fragments {
				i, ok := integral(field(f, "index"))
				if !ok {
					continue
				}
				tc := c.calls[i]
				if tc == nil {
					tc = &call{}
					c.calls[i] = tc
				}
				tc.id = cmp.Or(tc.id, str(f, "id"))
				fn, ok := field(f, "function").(map[string]any)
				if !ok {
					continue
				}
				tc.function = true
				tc.name = cmp.Or(tc.name, str(fn, "name"))
				if s, ok := fn["arguments"].(string); ok {
					tc.arguments.WriteString(s)
					tc.hasArgs = true
				}
			}
		}
	}
	for _, index := range slices.Sorted(maps.Keys(choices)) {
		c := choices[index]
		parts := appendTextPart(nil, "reasoning", c.reasoning.String())
		parts = appendTextPart(parts, "text", c.content.String())
		parts = appendTextPart(parts, "text", c.refusal.String())
		for _, i := range slices.Sorted(maps.Keys(c.calls)) {
			if tc := c.calls[i]; tc.function {
				var arguments any
				if tc.hasArgs {
					arguments = stringArguments(tc.arguments.String())
				}
				parts = append(parts, toolCallPart(tc.id, tc.name, arguments, tc.hasArgs))
			}
		}
		out.messages = append(out.messages, outputMessage(cmp.Or(c.role, "assistant"), parts))
		out.finishReasons = append(out.finishReasons, c.finish)
	}
	return out
}

// messagesOutput reads a Messages answer: one output message of its content blocks,
// its stop_reason the finish reason.
func messagesOutput(top map[string]any) answerOutput {
	out := answerOutput{id: str(top, "id"), model: str(top, "model")}
	blocks, ok := top["content"].([]any)
	if !ok {
		return out
	}
	var parts []any
	for _, raw := range blocks {
		b, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		switch str(b, "type") {
		case "":
		case "text":
			parts = appendTextPart(parts, "text", str(b, "text"))
		case "thinking":
			parts = appendTextPart(parts, "reasoning", str(b, "thinking"))
		case "tool_use":
			if name, ok := b["name"].(string); ok {
				arguments, has := toolArguments(b["input"])
				parts = append(parts, toolCallPart(str(b, "id"), name, arguments, has))
			}
		default:
			parts = append(parts, b)
		}
	}
	out.messages = []any{outputMessage("assistant", parts)}
	out.finishReasons = []string{str(top, "stop_reason")}
	return out
}

// messagesStreamOutput reads a Messages stream from message_start to message_stop:
// content blocks by index, each built by the deltas of its kind until its stop; a
// tool call's input its fragments joined, or with none the input its start gave.
func messagesStreamOutput(events []map[string]any) (answerOutput, error) {
	type block struct {
		typ      string
		start    map[string]any
		id, name string
		hasName  bool
		input    any
		content  strings.Builder
		stopped  bool
	}
	var out answerOutput
	blocks := map[float64]*block{}
	started, ended := false, false
	stopReason := ""
	for _, ev := range events {
		typ := str(ev, "type")
		if !started {
			if typ == "message_start" {
				started = true
				out.id, out.model = str(ev, "message", "id"), str(ev, "message", "model")
			}
			continue
		}
		if typ == "message_stop" {
			ended = true
			break
		}
		if typ == "message_delta" {
			stopReason = cmp.Or(str(ev, "delta", "stop_reason"), stopReason)
			continue
		}
		index, ok := integral(ev["index"])
		if !ok {
			continue
		}
		b := blocks[index]
		switch {
		case typ == "content_block_start" && b == nil:
			start, ok := ev["content_block"].(map[string]any)
			if !ok || str(start, "type") == "" {
				continue
			}
			b = &block{start: start}
			switch t := str(start, "type"); t {
			case "text", "thinking":
				b.typ = t
				b.content.WriteString(str(start, t))
			case "tool_use":
				b.typ = t
				b.id = str(start, "id")
				b.name, b.hasName = start["name"].(string)
				b.input = start["input"]
			}
			blocks[index] = b
		case b == nil || b.stopped:
		case typ == "content_block_delta":
			switch kind := str(ev, "delta", "type"); {
			case kind == "text_delta" && b.typ == "text":
				b.content.WriteString(str(ev, "delta", "text"))
			case kind == "thinking_delta" && b.typ == "thinking":
				b.content.WriteString(str(ev, "delta", "thinking"))
			case kind == "input_json_delta" && b.typ == "tool_use":
				b.content.WriteString(str(ev, "delta", "partial_json"))
			}
		case typ == "content_block_stop":
			b.stopped = true
		}
	}
	switch {
	case !started:
		return out, errors.New("the stream has no message_start")
	case !ended:
		return out, errors.New("the stream ended without message_stop")
	}
	var parts []any
	for _, index := range slices.Sorted(maps.Keys(blocks)) {
		b := blocks[index]
		switch b.typ {
		case "text":
			parts = appendTextPart(parts, "text", b.content.String())
		case "thinking":
			parts = appendTextPart(parts, "reasoning", b.content.String())
		case "tool_use":
			if !b.hasName {
				continue
			}
			arguments, has := toolArguments(b.input)
			if b.content.Len() > 0 {
				arguments, has = stringArguments(b.content.String()), true
			}
			parts = append(parts, toolCallPart(b.id, b.name, arguments, has))
		default:
			parts = append(parts, b.start)
		}
	}
	out.messages = []any{outputMessage("assistant", parts)}
	out.finishReasons = []string{stopReason}
	return out, nil
}

// responseOutput reads a response object: one output message of its output items,
// its finish reason the incomplete_details.reason of an incomplete response, error
// for a failed one, else its status.
func responseOutput(response map[string]any) answerOutput {
	out := answerOutput{id: str(response, "id"), model: str(response, "model")}
	items, ok := response["output"].([]any)
	if !ok {
		return out
	}
	var parts []any
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		switch str(item, "type") {
		case "":
		case "message":
			if s, ok := item["content"].(string); ok {
				parts = appendTextPart(parts, "text", s)
				continue
			}
			content, _ := item["content"].([]any)
			for _, p := range content {
				switch typ := str(p, "type"); typ {
				case "":
				case "output_text", "input_text":
					parts = appendTextPart(parts, "text", str(p, "text"))
				case "refusal":
					parts = appendTextPart(parts, "text", str(p, "refusal"))
				default:
					parts = append(parts, p)
				}
			}
		case "function_call":
			if name, ok := item["name"].(string); ok {
				arguments, has := toolArguments(item["arguments"])
				parts = append(parts, toolCallPart(str(item, "call_id"), name, arguments, has))
			}
		case "reasoning":
			for _, list := range []struct{ member, kind string }{{"summary", "summary_text"}, {"content", "reasoning_text"}} {
				entries, _ := item[list.member].([]any)
				for _, e := range entries {
					switch typ := str(e, "type"); typ {
					case "":
					case list.kind:
						parts = appendTextPart(parts, "reasoning", str(e, "text"))
					default:
						parts = append(parts, e)
					}
				}
			}
		default:
			parts = append(parts, item)
		}
	}
	reason := str(response, "status")
	switch reason {
	case "incomplete":
		reason = cmp.Or(str(response, "incomplete_details", "reason"), reason)
	case "failed":
		reason = "error"
	}
	out.messages = []any{outputMessage("assistant", parts)}
	out.finishReasons = []string{reason}
	return out
}

// responsesStreamOutput reads a Responses stream: its terminal event's response is
// the answer — where the item events differ from it (vLLM names a function call by
// another call_id there), the terminal event is what the client keeps.
func responsesStreamOutput(events []map[string]any) (answerOutput, error) {
	for _, ev := range events {
		switch str(ev, "type") {
		case "response.completed", "response.incomplete", "response.failed":
			if response, ok := ev["response"].(map[string]any); ok {
				return responseOutput(response), nil
			}
		}
	}
	return answerOutput{}, errors.New("the stream ended without response.completed, response.incomplete or response.failed")
}

// wantRequest is the request content a capture check's span must carry: the system
// instructions (Messages and Responses; Chat Completions has them in its messages),
// the input messages, and the tool offered.
func (c *captureExchange) wantRequest() (system, input, tools []any) {
	prompt := capturePrompt
	if c.tool {
		prompt = captureToolPrompt
	}
	text := func(s string) []any { return []any{map[string]any{"type": "text", "content": s}} }
	input = []any{map[string]any{"role": "user", "parts": text(prompt)}}
	if c.endpoint == epChat {
		input = append([]any{map[string]any{"role": "system", "parts": text(captureSystem)}}, input...)
	} else {
		system = text(captureSystem)
	}
	if c.tool {
		tools = []any{map[string]any{"type": "function", "name": captureTool, "description": captureToolText,
			"parameters": captureToolSchema}}
	}
	return system, input, tools
}

// outputLimit is the output limit the request sets in its API's member, nil for none
// — a capture carries the client's own, never the default the gateway fills.
func (c *captureExchange) outputLimit() any {
	members := map[string][]string{epChat: {"max_completion_tokens", "max_tokens"}, epMessages: {"max_tokens"},
		epResponses: {"max_output_tokens"}}[c.endpoint]
	for _, m := range members {
		if v := c.body[m]; v != nil {
			return v
		}
	}
	return nil
}

// lineAttributes are the attributes a span shares with its request's log line, by the
// same name and value.
var lineAttributes = []string{"gen_ai.provider.name", "kaiak.backend.id", "kaiak.backend.type",
	"kaiak.deployment.model", "kaiak.attempts", "gen_ai.usage.input_tokens", "gen_ai.usage.cache_read.input_tokens",
	"gen_ai.usage.cache_write.input_tokens", "gen_ai.usage.output_tokens", "gen_ai.usage.reasoning.output_tokens",
	"kaiak.usage.cost_usd", "kaiak.usage.estimated", "kaiak.usage.partial", "kaiak.time_to_first_token",
	"kaiak.relay_end"}

// checkCaptures holds each capture check's span to the answer the live-test kit
// received and to the request's log line. It runs after the gateway's exit.
func (r *run) checkCaptures() {
	for _, c := range r.captures {
		if c.problem != "" {
			r.fail(c.name, "%s", c.problem)
			continue
		}
		line, err := r.gw.logs.wait(r.ctx, msg("request", "kaiak.request.id", c.id))
		if err != nil {
			r.fail(c.name, "no log line for request %s: %v", c.id, err)
			continue
		}
		sp, n := r.traces.spanOf(c.id)
		switch {
		case line["kaiak.capture"] != "queued":
			r.fail(c.name, "the request line says kaiak.capture=%v, want queued", line["kaiak.capture"])
		case n != 1:
			r.fail(c.name, "%d spans for request %s after the gateway's exit, want 1", n, c.id)
		default:
			if problem := r.captureProblem(c, sp, line); problem != "" {
				r.fail(c.name, "%s", problem)
				continue
			}
			r.pass(c.name, fmt.Sprintf("%s; finish %s; in %v out %v as logged", outputSummary(c.answer.messages),
				strings.Join(c.answer.finishReasons, ","), line["gen_ai.usage.input_tokens"], line["gen_ai.usage.output_tokens"]))
		}
	}
}

// captureProblem is what is wrong with a capture check's span, "" when nothing is.
func (r *run) captureProblem(c *captureExchange, sp span, line map[string]any) string {
	switch {
	case sp.name != "chat "+modelChat:
		return fmt.Sprintf("span name %q, want %q", sp.name, "chat "+modelChat)
	case sp.kind != 3:
		return fmt.Sprintf("span kind %d, want 3 (CLIENT)", sp.kind)
	case sp.parentSpanID != "":
		return "the span has a parent: a capture is a trace of its own"
	case sp.status != nil && sp.status.Code != 0:
		return fmt.Sprintf("span status %d %q: the request succeeded", sp.status.Code, sp.status.Message)
	}
	backendURL := map[string]string{backendFirst: r.o.baseURL, backendSecond: r.o.baseURL2}[str(line, "kaiak.backend.id")]
	address, port := serverAddress(backendURL)
	type expect struct {
		key  string
		want any // nil: absent
	}
	expects := []expect{{"gen_ai.operation.name", "chat"}, {"gen_ai.request.model", modelChat},
		{"kaiak.request.id", c.id}, {"kaiak.key.id", keyCapture}, {"kaiak.key.group", groupCapture},
		{"kaiak.key.root_group", groupCapture}, {"gen_ai.conversation.id", r.session}, {"kaiak.session_id.ignored", nil},
		{"server.address", address}, {"server.port", port}, {"error.type", cmp.Or(line["error.type"], line["kaiak.relay_end"])}}
	if c.stream {
		expects = append(expects, expect{"gen_ai.request.stream", true})
	} else {
		expects = append(expects, expect{"gen_ai.request.stream", nil})
	}
	expects = append(expects, expect{"gen_ai.request.max_tokens", c.outputLimit()})
	for _, key := range lineAttributes {
		expects = append(expects, expect{key, line[key]})
	}
	expects = append(expects, expect{"gen_ai.response.id", nonEmpty(c.answer.id)},
		expect{"gen_ai.response.model", nonEmpty(c.answer.model)})
	// One finish reason per generation, error for one that gave none.
	if len(c.answer.finishReasons) > 0 {
		reasons := []any{}
		for _, s := range c.answer.finishReasons {
			reasons = append(reasons, cmp.Or(s, "error"))
		}
		expects = append(expects, expect{"gen_ai.response.finish_reasons", reasons})
	} else {
		expects = append(expects, expect{"gen_ai.response.finish_reasons", nil})
	}
	for _, e := range expects {
		got, present := sp.attrs[e.key]
		switch {
		case e.want == nil && present:
			return fmt.Sprintf("%s is %v, want none", e.key, got)
		case e.want != nil && !present:
			return fmt.Sprintf("no %s, want %v", e.key, e.want)
		case e.want != nil && !sameValue(got, e.want):
			return fmt.Sprintf("%s is %v, want %v", e.key, got, e.want)
		}
	}

	system, input, tools := c.wantRequest()
	var output []any
	if len(c.answer.messages) > 0 {
		output = c.answer.messages
	}
	for _, e := range []struct {
		key, what string
		want      []any // nil: absent
	}{{"gen_ai.system_instructions", "the system prompt sent", system}, {"gen_ai.input.messages", "the messages sent", input},
		{"gen_ai.tool.definitions", "the tool offered", tools}, {"gen_ai.output.messages", "the answer received", output}} {
		raw, present := sp.attrs[e.key]
		s, isString := raw.(string)
		var got any
		switch {
		case e.want == nil && present:
			return fmt.Sprintf("%s is %s, want none", e.key, clipJSON(raw))
		case e.want == nil:
			continue
		case !present:
			return fmt.Sprintf("no %s, want %s: %s", e.key, e.what, clipJSON(e.want))
		case !isString || json.Unmarshal([]byte(s), &got) != nil:
			return fmt.Sprintf("%s is not a JSON string: %v", e.key, raw)
		}
		if want := normalized(e.want); !reflect.DeepEqual(got, want) {
			return fmt.Sprintf("%s differs from %s: %s\nspan: %s\nwant: %s", e.key, e.what, firstDifference(got, want, ""),
				clipJSON(got), clipJSON(want))
		}
	}
	return ""
}

// checkCapturedOnly: after the gateway's exit, the receiver holds no span but the
// capture checks' — none for the requests of the groups that do not capture, whose
// log lines carry no kaiak.capture — took every export, and the gateway reported no
// failing trace export.
func (r *run) checkCapturedOnly() {
	const name = "capture/groups"
	ids := map[string]bool{}
	for _, c := range r.captures {
		ids[c.id] = true
	}
	var others []string
	for _, sp := range r.traces.received() {
		if id, _ := sp.attrs["kaiak.request.id"].(string); !ids[id] {
			others = append(others, fmt.Sprintf("%v (group %v)", sp.attrs["kaiak.request.id"], sp.attrs["kaiak.key.group"]))
		}
	}
	notCaptured, marked := 0, ""
	for _, line := range r.gw.logs.all(msg("request")) {
		if group, ok := line["kaiak.key.group"]; ok && group != groupCapture {
			notCaptured++
			if _, ok := line["kaiak.capture"]; ok && marked == "" {
				marked = fmt.Sprint(line["kaiak.request.id"])
			}
		}
	}
	_, failing := r.gw.logs.find(msg("trace export failing"))
	switch refused := r.traces.refusals(); {
	case len(refused) > 0:
		r.fail(name, "the receiver refused %d export(s): %s", len(refused), strings.Join(refused, "; "))
	case failing:
		r.fail(name, "the gateway logged trace export failing (see its log)")
	case len(others) > 0:
		r.fail(name, "spans for requests outside %s: %s", groupCapture, strings.Join(others, ", "))
	case marked != "":
		r.fail(name, "request %s of a group that does not capture carries kaiak.capture", marked)
	default:
		r.pass(name, fmt.Sprintf("after the exit flush, %d spans, the capture checks'; none for the %d requests of the groups that do not capture",
			len(r.captures), notCaptured))
	}
}

// checkForward: with -traces-forward, every export the receiver took was forwarded
// and answered 2xx.
func (r *run) checkForward() {
	if r.o.tracesForward == "" {
		return
	}
	const name = "capture/forward"
	exports, spans, failures := r.traces.forwardResults()
	switch {
	case len(failures) > 0:
		r.fail(name, "%d of %d exports not forwarded to %s; the first: %s", len(failures), len(failures)+exports,
			r.o.tracesForward, failures[0])
	case exports == 0:
		r.fail(name, "nothing forwarded to %s", r.o.tracesForward)
	default:
		r.pass(name, fmt.Sprintf("%d spans forwarded to %s in %d export request(s), each answered 2xx", spans,
			r.o.tracesForward, exports))
	}
}

// outputSummary is a one-line view of output messages: each part's type, a text's
// first words, a tool call's name and arguments.
func outputSummary(messages []any) string {
	var items []string
	for _, m := range messages {
		parts, _ := field(m, "parts").([]any)
		for _, p := range parts {
			switch typ := str(p, "type"); typ {
			case "text":
				items = append(items, fmt.Sprintf("text %q", clip(str(p, "content"))))
			case "tool_call":
				arguments, _ := json.Marshal(field(p, "arguments"))
				items = append(items, "tool_call "+str(p, "name")+" "+clip(string(arguments)))
			default:
				items = append(items, typ)
			}
		}
	}
	if len(items) == 0 {
		return "no output"
	}
	return strings.Join(items, " + ")
}

// serverAddress is a backend's server.address and server.port as a capture carries
// them: its base URL's host, and its port — the URL's, else 443 for https and 80 for
// http.
func serverAddress(baseURL string) (string, int64) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return "", 0
	}
	if p, err := strconv.ParseInt(u.Port(), 10, 64); err == nil {
		return u.Hostname(), p
	}
	if u.Scheme == "https" {
		return u.Hostname(), 443
	}
	return u.Hostname(), 80
}

// sameValue compares an attribute's value with what is wanted: numbers by value —
// an intValue reads as an int64, the log line's numbers as float64s — anything else
// exactly.
func sameValue(got, want any) bool {
	g, gok := number(got)
	w, wok := number(want)
	if gok && wok {
		return g == w || math.Abs(g-w) <= 1e-9*math.Max(math.Abs(g), math.Abs(w))
	}
	return reflect.DeepEqual(got, want)
}

func number(v any) (float64, bool) {
	switch n := v.(type) {
	case int64:
		return float64(n), true
	case float64:
		return n, true
	}
	return 0, false
}

// nonEmpty is s, or nil (absent) when it is empty.
func nonEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// normalized is v as JSON decodes it.
func normalized(v any) any {
	data, _ := json.Marshal(v) // decoded JSON and this file's values always encode
	var out any
	_ = json.Unmarshal(data, &out)
	return out
}

// firstDifference names where decoded JSON got first differs from want: the path,
// what is there and what is wanted.
func firstDifference(got, want any, path string) string {
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			break
		}
		for _, k := range slices.Sorted(maps.Keys(w)) {
			if _, ok := g[k]; !ok {
				return fmt.Sprintf("%s.%s is missing, want %s", path, k, clipJSON(w[k]))
			}
			if d := firstDifference(g[k], w[k], path+"."+k); d != "" {
				return d
			}
		}
		for _, k := range slices.Sorted(maps.Keys(g)) {
			if _, ok := w[k]; !ok {
				return fmt.Sprintf("%s.%s is %s, want none", path, k, clipJSON(g[k]))
			}
		}
		return ""
	case []any:
		g, ok := got.([]any)
		if !ok {
			break
		}
		for i := range min(len(g), len(w)) {
			if d := firstDifference(g[i], w[i], fmt.Sprintf("%s[%d]", path, i)); d != "" {
				return d
			}
		}
		if len(g) != len(w) {
			return fmt.Sprintf("%s has %d elements, want %d", cmp.Or(path, "the list"), len(g), len(w))
		}
		return ""
	}
	if reflect.DeepEqual(got, want) {
		return ""
	}
	return fmt.Sprintf("%s is %s, want %s", cmp.Or(path, "the value"), clipJSON(got), clipJSON(want))
}

// clipJSON is v as JSON, shortened for a report.
func clipJSON(v any) string {
	data, _ := json.Marshal(v)
	if s := []rune(string(data)); len(s) > 600 {
		return string(s[:600]) + "…"
	}
	return string(data)
}
