package e2e

// Request capture through the built binary (docs/specs/GATEWAY.md, Request capture;
// Observability → OTLP trace export; Lifecycle → Draining). A gateway exporting
// traces to a fake collector captures the chat requests of the groups that capture,
// by each key's own group: one span per request, its attributes as its request line
// has them and its content as the client sent and received it — on each chat
// endpoint, streamed and not, tool calls included; refusals after the capture stage
// with error.type and no output, none before it; an upstream error; a stream the
// client left, with what it received; the session header kept, ignored or absent.
// The backend gets the same request whether it is captured or not. The capture
// budget drops captures, never requests; trace export needs its own switch; the
// drain delivers the captures of the requests it let finish, and of those it cut,
// before the process exits.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"kaiak/internal/fakebackend"
	"kaiak/internal/telemetry/fakeotlp"
)

// captureKeys are the capture scenario's client keys, one per group.
type captureKeys struct{ team, capChild, capRoot, plainChild, limited string }

// captureScenario is the capture scenario's config and its keys. Model cap, on the
// vLLM backend vl — chat completions, Messages and Responses — is priced and has an
// output limit whose default and ceiling the gateway applies; other is a model group
// cap-child may not use; chat-only lists chat completions alone; held runs on the
// backend capped, one request at a time, with no queue. Groups: team captures nothing
// and its child cap-child captures; cap-root captures and its child plain-child does
// not; limited, under team, captures, with one request a minute.
func captureScenario(backendURL, cappedURL string) (map[string]any, captureKeys) {
	var keys captureKeys
	var team, capChild, capRoot, plainChild, limited string
	keys.team, team = newKey()
	keys.capChild, capChild = newKey()
	keys.capRoot, capRoot = newKey()
	keys.plainChild, plainChild = newKey()
	keys.limited, limited = newKey()
	model := func(backend string, endpoints []any) map[string]any {
		return map[string]any{
			"deployments": []any{map[string]any{"backend": backend, "model": backendChatModel}},
			"endpoints":   endpoints,
			"metadata": map[string]any{"context_length": 8192,
				"capabilities": map[string]any{"streaming": true, "tools": true, "vision": true, "reasoning": true}},
			"output_limit": map[string]any{"default": 64, "ceiling": 128},
			"prices": []any{map[string]any{"effective_from": "2026-01-01", "tiers": []any{map[string]any{
				"above_input_tokens": 0, "usd_per_million": map[string]any{"tokens_in": 1, "tokens_out": 2}}}}},
		}
	}
	held := model("capped", everyEndpoint("vllm"))
	held["queue"] = map[string]any{"size": 0}
	return map[string]any{
		"protocol_version": 8,
		"global":           map[string]any{},
		"backends": map[string]any{
			"vl":     map[string]any{"type": "vllm", "base_url": backendURL + "/v1"},
			"capped": map[string]any{"type": "vllm", "base_url": cappedURL + "/v1", "max_in_flight": 1},
		},
		"models": map[string]any{
			"cap":       model("vl", everyEndpoint("vllm")),
			"other":     model("vl", everyEndpoint("vllm")),
			"chat-only": model("vl", []any{"chat_completions"}),
			"held":      held,
		},
		"groups": map[string]any{
			"team":        map[string]any{"capture": false},
			"cap-child":   map[string]any{"parent": "team", "capture": true, "allowed_models": []any{"cap", "held"}},
			"cap-root":    map[string]any{"capture": true},
			"plain-child": map[string]any{"parent": "cap-root", "capture": false},
			"limited": map[string]any{"parent": "team", "capture": true,
				"limits": []any{map[string]any{"type": "requests_per_minute", "value": 1}}},
		},
		"keys": map[string]any{
			"k-team":        map[string]any{"hash": team, "group": "team"},
			"k-cap-child":   map[string]any{"hash": capChild, "group": "cap-child"},
			"k-cap-root":    map[string]any{"hash": capRoot, "group": "cap-root"},
			"k-plain-child": map[string]any{"hash": plainChild, "group": "plain-child"},
			"k-limited":     map[string]any{"hash": limited, "group": "limited"},
		},
	}, keys
}

// startCapturing writes the capture scenario's config for the backends at backendURL
// and cappedURL and starts a gateway on it with env on top; it returns the gateway
// and the keys.
func startCapturing(t *testing.T, backendURL, cappedURL string, env ...string) (*gateway, captureKeys) {
	t.Helper()
	cfg, keys := captureScenario(backendURL, cappedURL)
	configFile := filepath.Join(t.TempDir(), "config.json")
	writeJSON(t, configFile, cfg)
	return startGatewayEnv(t, append(gatewayEnv(configFile), env...)), keys
}

// tracesTo is the environment that switches trace export on, to the collector.
func tracesTo(c *fakeotlp.Collector) string {
	return "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT=" + c.URL + "/v1/traces"
}

// postRaw posts body, raw JSON as the client writes it, to path with key, the request
// ID id and header (each may be empty), and reads the whole answer.
func (g *gateway) postRaw(t *testing.T, path, key, id string, header map[string]string, body string) *response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, g.api+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	if id != "" {
		req.Header.Set("X-Request-Id", id)
	}
	for name, value := range header {
		req.Header.Set(name, value)
	}
	return do(t, req)
}

// helloChat is a chat completion asking cap to say hello, streamed or not.
func helloChat(stream bool) string {
	return `{"model": "cap", "stream": ` + strconv.FormatBool(stream) +
		`, "messages": [{"role": "user", "content": "Say hello."}]}`
}

// helloInput is the input messages of helloChat as a span carries them; helloOutput
// the fake backend's default answer.
const (
	helloInput  = `[{"role":"user","parts":[{"type":"text","content":"Say hello."}]}]`
	helloOutput = `[{"role":"assistant","parts":[{"type":"text","content":"Hello from the fake"}]}]`
)

// answeredBy is the span attributes of a request answered by the backend at
// backendURL: its address and port.
func answeredBy(t *testing.T, backendURL string) map[string]any {
	t.Helper()
	u, err := url.Parse(backendURL)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.ParseInt(u.Port(), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]any{"server.address": u.Hostname(), "server.port": port}
}

// helloAnswered is the span attributes of helloChat answered by the fake backend at
// backendURL with its default answer, for a key whose top-level group is root.
func helloAnswered(t *testing.T, backendURL, root string) map[string]any {
	t.Helper()
	return merged(answeredBy(t, backendURL), map[string]any{"kaiak.key.root_group": root,
		"gen_ai.input.messages": helloInput, "gen_ai.output.messages": helloOutput, "gen_ai.response.id": "fake-1",
		"gen_ai.response.model": "cap", "gen_ai.response.finish_reasons": []any{"stop"}})
}

// merged is the attributes of every map in ms, a later map's winning.
func merged(ms ...map[string]any) map[string]any {
	out := map[string]any{}
	for _, m := range ms {
		maps.Copy(out, m)
	}
	return out
}

// requestSpans are the spans the collector received, by kaiak.request.id; a request
// with two spans fails the test.
func requestSpans(t *testing.T, c *fakeotlp.Collector) map[string]fakeotlp.Span {
	t.Helper()
	out := map[string]fakeotlp.Span{}
	for _, s := range c.Spans() {
		id, _ := s.Attrs(t)["kaiak.request.id"].(string)
		if _, dup := out[id]; dup {
			t.Errorf("two spans for request %q", id)
		}
		out[id] = s
	}
	return out
}

// waitSpans waits for the spans of the requests ids — the trace export sends what is
// queued every second — and returns every span received, by request ID.
func waitSpans(t *testing.T, c *fakeotlp.Collector, ids ...string) map[string]fakeotlp.Span {
	t.Helper()
	var spans map[string]fakeotlp.Span
	pollUntil(t, fmt.Sprintf("the spans of %q", ids), waitLimit, func() bool {
		spans = requestSpans(t, c)
		for _, id := range ids {
			if _, ok := spans[id]; !ok {
				return false
			}
		}
		return true
	})
	return spans
}

// spanChecks are the spans a subtest expects: each request's line and the attributes
// its span has beside the line's (expectSpan).
type spanChecks []struct{ line, want map[string]any }

func (s *spanChecks) add(line, want map[string]any) {
	*s = append(*s, struct{ line, want map[string]any }{line, want})
}

// check waits for every span s expects — the trace export sends every second, so a
// subtest waits once for all of its spans — checks each, and returns their request
// IDs.
func (s spanChecks) check(t *testing.T, c *fakeotlp.Collector) []string {
	t.Helper()
	var ids []string
	for _, e := range s {
		ids = append(ids, e.line["kaiak.request.id"].(string))
	}
	spans := waitSpans(t, c, ids...)
	for i, e := range s {
		expectSpan(t, spans[ids[i]], e.line, e.want)
	}
	return ids
}

// lineAndSpan are the attributes a span carries as its request line does, whenever
// the line has them (docs/specs/GATEWAY.md, Request capture → Attributes).
var lineAndSpan = []string{"kaiak.request.id", "kaiak.key.id", "kaiak.key.group", "gen_ai.operation.name",
	"gen_ai.request.model", "gen_ai.provider.name", "kaiak.backend.id", "kaiak.backend.type", "kaiak.deployment.model",
	"kaiak.attempts", "error.type", "kaiak.relay_end", "kaiak.time_to_first_token", "gen_ai.usage.input_tokens",
	"gen_ai.usage.cache_read.input_tokens", "gen_ai.usage.cache_write.input_tokens", "gen_ai.usage.output_tokens",
	"gen_ai.usage.reasoning.output_tokens", "kaiak.usage.cost_usd", "kaiak.usage.estimated", "kaiak.usage.partial",
	"kaiak.session_id.ignored"}

// expectSpan checks the span of the request whose line is line: named chat and the
// model asked for, kind CLIENT, a root span with IDs of OTLP's lengths, as long as the
// request at most; the attributes of lineAndSpan the line has, with its values —
// error.type the line's, else its kaiak.relay_end — and want's; no other attribute;
// an error status, with no message, when it has an error.type, else none.
func expectSpan(t *testing.T, span fakeotlp.Span, line, want map[string]any) {
	t.Helper()
	id := line["kaiak.request.id"]
	expected := maps.Clone(want)
	for _, k := range lineAndSpan {
		if v, ok := line[k]; ok {
			expected[k] = v
		}
	}
	if _, ok := expected["error.type"]; !ok && line["kaiak.relay_end"] != nil {
		expected["error.type"] = line["kaiak.relay_end"]
	}
	attrs := span.Attrs(t)
	if got, keys := slices.Sorted(maps.Keys(attrs)), slices.Sorted(maps.Keys(expected)); !slices.Equal(got, keys) {
		t.Errorf("%v: span attributes\n got %v\nwant %v", id, got, keys)
	}
	for k, w := range expected {
		if got := attrs[k]; !reflect.DeepEqual(plainNumber(got), plainNumber(w)) {
			t.Errorf("%v: %s =\n %v (%T)\nwant\n %v (%T)", id, k, got, got, w, w)
		}
	}
	if want := fmt.Sprint("chat ", line["gen_ai.request.model"]); span.Name != want || span.Kind != 3 ||
		len(span.TraceID) != 32 || len(span.SpanID) != 16 {
		t.Errorf("%v: span %q kind %d, IDs %q %q; want %q, kind 3 (CLIENT)", id, span.Name, span.Kind, span.TraceID,
			span.SpanID, want)
	}
	start, err1 := strconv.ParseInt(span.StartTimeUnixNano, 10, 64)
	end, err2 := strconv.ParseInt(span.EndTimeUnixNano, 10, 64)
	duration, _ := line["kaiak.request.duration"].(float64)
	if err1 != nil || err2 != nil || end < start || time.Duration(end-start) > time.Duration(duration*1e9)+2*time.Microsecond {
		t.Errorf("%v: span from %s to %s; want within the request's %v s", id, span.StartTimeUnixNano,
			span.EndTimeUnixNano, duration)
	}
	errorType, _ := expected["error.type"].(string)
	switch {
	case errorType == "" && span.Status != nil:
		t.Errorf("%v: status %+v, want none", id, span.Status)
	case errorType != "" && (span.Status == nil || span.Status.Code != 2 || span.Status.Message != ""):
		t.Errorf("%v: status %+v, want code 2 (ERROR) with no message: error.type is %s", id, span.Status, errorType)
	}
}

// plainNumber is v with an integer as a float64, as a JSON log line decodes it.
func plainNumber(v any) any {
	switch n := v.(type) {
	case int64:
		return float64(n)
	case int:
		return float64(n)
	}
	return v
}

// spanProcessedSeries is the captures' otel.sdk.processor.span.processed series of
// errorType: the captures handed to the exporter for "".
func spanProcessedSeries(errorType string) string {
	return logExportSeries("otel_sdk_processor_span_processed_total", "capture_span_processor", errorType)
}

// spanExportedSeries is the trace exporter's otel.sdk.exporter.span.exported series
// of errorType: the spans the collector accepted for "".
func spanExportedSeries(errorType string) string {
	return logExportSeries("otel_sdk_exporter_span_exported_total", "otlp_http_json_span_exporter", errorType)
}

// spanDrops are the error types the capture counts dropped captures by.
var spanDrops = []string{"memory", "too_large", "shutdown", "_OTHER"}

// readEventData reads one event of a stream and returns its data.
func readEventData(t *testing.T, r *bufio.Reader) string {
	t.Helper()
	var data string
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("reading an event: %v", err)
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" && data != "" {
			return data
		}
		if d, ok := strings.CutPrefix(line, "data: "); ok {
			data = d
		}
	}
}

// The capture scenario on one gateway exporting traces: every request whose key's
// group captures, from the capture stage on, makes exactly one span, its attributes
// and content as the spec has them; no other request makes one; the counts follow.
func TestCaptureEndToEnd(t *testing.T) {
	backend, capped := fakebackend.New(), fakebackend.New()
	defer backend.Close()
	defer capped.Close()
	collector := fakeotlp.New(t, nil)
	g, keys := startCapturing(t, backend.URL(), capped.URL(), tracesTo(collector))
	starting := g.logs.wait(t, "kaiak starting", msg("kaiak starting"))
	if got, want := starting["kaiak.trace_export.endpoint"], collectorHost(t, collector); got != want {
		t.Errorf("kaiak.trace_export.endpoint %v, want %s", got, want)
	}
	// captured lists the requests the collector must hold a span for at the end, and
	// no other.
	var captured []string

	t.Run("each chat endpoint, streamed and not: the request as sent, the answer as received", func(t *testing.T) {
		// The requests are the spec's examples (Request capture → Examples) — the
		// Responses one with its tool declared — each answered with text, then with a
		// tool call; streamed, an answer gives the output messages it gives whole.
		chat := func(stream bool) string {
			return `{"model": "cap", "stream": ` + strconv.FormatBool(stream) + `, "temperature": 0.2,
				"messages": [
					{"role": "system", "content": "Answer briefly."},
					{"role": "user", "content": [
						{"type": "text", "text": "What is in this image?"},
						{"type": "image_url", "image_url": {"url": "data:image/png;base64,iVBORw0K"}}]}],
				"tools": [{"type": "function", "function": {"name": "lookup",
					"description": "Look a word up", "parameters": {"type": "object"}}}]}`
		}
		chatSent := map[string]any{"gen_ai.request.temperature": 0.2,
			"gen_ai.input.messages": `[{"role":"system","parts":[{"type":"text","content":"Answer briefly."}]},` +
				`{"role":"user","parts":[{"type":"text","content":"What is in this image?"},` +
				`{"type":"blob","modality":"image","mime_type":"image/png","content":"iVBORw0K"}]}]`,
			"gen_ai.tool.definitions": `[{"type":"function","name":"lookup","description":"Look a word up",` +
				`"parameters":{"type":"object"}}]`}
		messages := func(stream bool) string {
			return `{"model": "cap", "max_tokens": 1024, "stream": ` + strconv.FormatBool(stream) + `, "system": "Be terse.",
				"tools": [{"name": "get_weather", "description": "Weather for a city",
					"input_schema": {"type": "object", "properties": {"city": {"type": "string"}}}}],
				"messages": [
					{"role": "user", "content": "Weather in Oslo?"},
					{"role": "assistant", "content": [
						{"type": "tool_use", "id": "toolu_1", "name": "get_weather", "input": {"city": "Oslo"}}]},
					{"role": "user", "content": [
						{"type": "tool_result", "tool_use_id": "toolu_1", "content": "4 °C, rain"}]}]}`
		}
		messagesSent := map[string]any{"gen_ai.request.max_tokens": int64(1024),
			"gen_ai.system_instructions": `[{"type":"text","content":"Be terse."}]`,
			"gen_ai.input.messages": `[{"role":"user","parts":[{"type":"text","content":"Weather in Oslo?"}]},` +
				`{"role":"assistant","parts":[{"type":"tool_call","id":"toolu_1","name":"get_weather","arguments":{"city":"Oslo"}}]},` +
				`{"role":"user","parts":[{"type":"tool_call_response","id":"toolu_1","response":"4 °C, rain"}]}]`,
			"gen_ai.tool.definitions": `[{"type":"function","name":"get_weather","description":"Weather for a city",` +
				`"parameters":{"type":"object","properties":{"city":{"type":"string"}}}}]`}
		responses := func(stream bool) string {
			return `{"model": "cap", "stream": ` + strconv.FormatBool(stream) + `, "instructions": "Answer in French.",
				"max_output_tokens": 500,
				"tools": [{"type": "function", "name": "zoom", "description": "Zoom in", "parameters": {"type": "object"}}],
				"input": [
					{"role": "user", "content": [
						{"type": "input_text", "text": "Describe it."},
						{"type": "input_image", "image_url": "https://example.com/cat.png"}]},
					{"type": "function_call", "call_id": "call_1", "name": "zoom", "arguments": "{\"level\": 2}"},
					{"type": "function_call_output", "call_id": "call_1", "output": "zoomed"}]}`
		}
		responsesSent := map[string]any{"gen_ai.request.max_tokens": int64(500),
			"gen_ai.system_instructions": `[{"type":"text","content":"Answer in French."}]`,
			"gen_ai.input.messages": `[{"role":"user","parts":[{"type":"text","content":"Describe it."},` +
				`{"type":"uri","modality":"image","uri":"https://example.com/cat.png"}]},` +
				`{"role":"assistant","parts":[{"type":"tool_call","id":"call_1","name":"zoom","arguments":{"level":2}}]},` +
				`{"role":"tool","parts":[{"type":"tool_call_response","id":"call_1","response":"zoomed"}]}]`,
			"gen_ai.tool.definitions": `[{"type":"function","name":"zoom","description":"Zoom in","parameters":{"type":"object"}}]`}
		var checks spanChecks
		for _, c := range []struct {
			name, path string
			body       func(stream bool) string
			sent       map[string]any
			reply      fakebackend.Reply
			// output is the answer's output messages; finish its finish reason; id its ID.
			output, finish, id string
		}{
			{"chat-text", "/v1/chat/completions", chat, chatSent,
				fakebackend.Reply{Reasoning: []string{"A cat ", "on a mat."}, Chunks: []string{"A cat."}},
				`[{"role":"assistant","parts":[{"type":"reasoning","content":"A cat on a mat."},{"type":"text","content":"A cat."}]}]`,
				"stop", "fake-1"},
			{"chat-tool", "/v1/chat/completions", chat, chatSent,
				fakebackend.Reply{Chunks: []string{"Looking it up."}, ToolCalls: []fakebackend.ToolCall{
					{ID: "call_7", Name: "lookup", Arguments: []string{`{"word":`, `"cat"}`}}}},
				`[{"role":"assistant","parts":[{"type":"text","content":"Looking it up."},` +
					`{"type":"tool_call","id":"call_7","name":"lookup","arguments":{"word":"cat"}}]}]`,
				"tool_calls", "fake-1"},
			{"messages-text", "/v1/messages", messages, messagesSent,
				fakebackend.Reply{Chunks: []string{"4 °C and rain."}},
				`[{"role":"assistant","parts":[{"type":"text","content":"4 °C and rain."}]}]`, "end_turn", "msg_fake_1"},
			{"messages-tool", "/v1/messages", messages, messagesSent,
				fakebackend.Reply{Reasoning: []string{"Oslo ", "again."}, Chunks: []string{"Checking."},
					ToolCalls: []fakebackend.ToolCall{{ID: "toolu_2", Name: "get_weather", Arguments: []string{`{"city":`, `"Bergen"}`}}}},
				`[{"role":"assistant","parts":[{"type":"reasoning","content":"Oslo again."},{"type":"text","content":"Checking."},` +
					`{"type":"tool_call","id":"toolu_2","name":"get_weather","arguments":{"city":"Bergen"}}]}]`,
				"tool_use", "msg_fake_1"},
			{"responses-text", "/v1/responses", responses, responsesSent,
				fakebackend.Reply{Reasoning: []string{"Looked ", "closer."}, Chunks: []string{"Un chat."}},
				`[{"role":"assistant","parts":[{"type":"reasoning","content":"Looked closer."},{"type":"text","content":"Un chat."}]}]`,
				"completed", "resp_fake_1"},
			{"responses-tool", "/v1/responses", responses, responsesSent,
				fakebackend.Reply{Chunks: []string{"Zooming."}, ToolCalls: []fakebackend.ToolCall{
					{ID: "call_2", Name: "zoom", Arguments: []string{`{"level":`, `3}`}}}},
				`[{"role":"assistant","parts":[{"type":"text","content":"Zooming."},` +
					`{"type":"tool_call","id":"call_2","name":"zoom","arguments":{"level":3}}]}]`,
				"completed", "resp_fake_1"},
		} {
			for _, stream := range []bool{false, true} {
				id := c.name + map[bool]string{false: "-whole", true: "-streamed"}[stream]
				backend.SetReply(c.reply)
				if r := g.postRaw(t, c.path, keys.capChild, id, nil, c.body(stream)); r.StatusCode != http.StatusOK {
					t.Fatalf("%s: %d %s", id, r.StatusCode, r.body)
				}
				if c.name == "messages-text" {
					// The span holds the client's output limit; the backend got it lowered
					// to the model's ceiling.
					if sent := lastRequest(t, backend); fmt.Sprint(sent["max_tokens"]) != "128" {
						t.Errorf("%s: the backend got max_tokens %v, want the ceiling 128", id, sent["max_tokens"])
					}
				}
				line := g.settled(t, id)
				if line["kaiak.capture"] != "queued" || line["gen_ai.provider.name"] != "vllm" || line["kaiak.backend.id"] != "vl" ||
					line["kaiak.deployment.model"] != backendChatModel || line["gen_ai.usage.input_tokens"] != 7.0 {
					t.Errorf("%s: request line %v", id, line)
				}
				want := merged(answeredBy(t, backend.URL()), c.sent, map[string]any{"kaiak.key.root_group": "team",
					"gen_ai.output.messages": c.output, "gen_ai.response.finish_reasons": []any{c.finish},
					"gen_ai.response.id": c.id, "gen_ai.response.model": "cap"})
				if stream {
					want["gen_ai.request.stream"] = true
				}
				checks.add(line, want)
			}
		}
		backend.SetReply(fakebackend.Reply{})
		captured = append(captured, checks.check(t, collector)...)
	})

	t.Run("the key's own group decides, a parent's flag reaches no child", func(t *testing.T) {
		var checks spanChecks
		for _, c := range []struct {
			name, key, root string
			captures        bool
		}{
			{"team", keys.team, "team", false},
			{"cap-child", keys.capChild, "team", true},
			{"cap-root", keys.capRoot, "cap-root", true},
			{"plain-child", keys.plainChild, "cap-root", false},
		} {
			id := "group-" + c.name
			if r := g.postRaw(t, "/v1/chat/completions", c.key, id, nil, helloChat(false)); r.StatusCode != http.StatusOK {
				t.Fatalf("%s: %d %s", id, r.StatusCode, r.body)
			}
			line := g.settled(t, id)
			if outcome, ok := line["kaiak.capture"]; ok != c.captures || (ok && outcome != "queued") {
				t.Errorf("%s: kaiak.capture %v; want queued when the group captures (%v), else none", id, outcome, c.captures)
			}
			if c.captures {
				checks.add(line, helloAnswered(t, backend.URL(), c.root))
			}
		}
		captured = append(captured, checks.check(t, collector)...)
	})

	t.Run("refusals after the capture stage are captured without output, before it not", func(t *testing.T) {
		// A stream of a group that does not capture holds the held model's one slot.
		pace := make(chan struct{})
		capped.SetReply(fakebackend.Reply{Pace: pace})
		holding := openStream(t, g, keys.team, "holding", chatBody("held", true, nil))
		limitedFirst := g.postRaw(t, "/v1/chat/completions", keys.limited, "limited-served", nil, helloChat(false))
		if limitedFirst.StatusCode != http.StatusOK {
			t.Fatalf("limited-served: %d %s", limitedFirst.StatusCode, limitedFirst.body)
		}
		var checks spanChecks
		checks.add(g.settled(t, "limited-served"), helloAnswered(t, backend.URL(), "team"))
		for _, c := range []struct {
			id, path, key, body string
			status              int
			code, root          string
			sent                map[string]any
		}{
			{"refused-model", "/v1/chat/completions", keys.capChild, `{"model": "other", "messages": [{"role": "user", "content": "Say hello."}]}`,
				http.StatusNotFound, "model_not_found", "team", nil},
			{"refused-endpoint", "/v1/messages", keys.capRoot,
				`{"model": "chat-only", "max_tokens": 16, "messages": [{"role": "user", "content": "Say hello."}]}`,
				http.StatusBadRequest, "endpoint_not_served", "cap-root", map[string]any{"gen_ai.request.max_tokens": int64(16)}},
			{"refused-limit", "/v1/chat/completions", keys.limited, helloChat(false),
				http.StatusTooManyRequests, "rate_limit_exceeded", "team", nil},
			{"refused-queue", "/v1/chat/completions", keys.capChild,
				`{"model": "held", "messages": [{"role": "user", "content": "Say hello."}]}`,
				http.StatusTooManyRequests, "queue_full", "team", nil},
		} {
			r := g.postRaw(t, c.path, c.key, c.id, nil, c.body)
			if r.StatusCode != c.status || !strings.Contains(string(r.body), `"`+c.code+`"`) {
				t.Fatalf("%s: %d %s, want %d %s", c.id, r.StatusCode, r.body, c.status, c.code)
			}
			line := g.settled(t, c.id)
			if line["kaiak.capture"] != "queued" || line["error.type"] != c.code {
				t.Errorf("%s: request line %v, want kaiak.capture queued and error.type %s", c.id, line, c.code)
			}
			checks.add(line, merged(c.sent, map[string]any{"kaiak.key.root_group": c.root, "gen_ai.input.messages": helloInput}))
		}
		close(pace)
		finishStream(t, holding)
		capped.SetReply(fakebackend.Reply{})

		// The inbound stage refuses a body that is not JSON: no content, no capture.
		r := g.postRaw(t, "/v1/chat/completions", keys.capChild, "refused-json", nil, `{"model":`)
		if r.StatusCode != http.StatusBadRequest || r.errorCode(t) != "invalid_json" {
			t.Fatalf("refused-json: %d %s, want 400 invalid_json", r.StatusCode, r.body)
		}
		if line := g.settled(t, "refused-json"); line["kaiak.capture"] != nil {
			t.Errorf("refused-json: kaiak.capture %v, want none: refused before the capture stage", line["kaiak.capture"])
		}
		captured = append(captured, checks.check(t, collector)...)
	})

	t.Run("an upstream error is captured without output", func(t *testing.T) {
		backend.SetReply(fakebackend.Reply{Status: http.StatusInternalServerError})
		r := g.postRaw(t, "/v1/chat/completions", keys.capChild, "upstream-error", nil, helloChat(false))
		backend.SetReply(fakebackend.Reply{})
		if r.StatusCode != http.StatusInternalServerError || r.errorCode(t) != "upstream_error" {
			t.Fatalf("upstream-error: %d %s, want 500 upstream_error", r.StatusCode, r.body)
		}
		line := g.settled(t, "upstream-error")
		if line["kaiak.capture"] != "queued" || line["error.type"] != "upstream_error" {
			t.Errorf("request line %v", line)
		}
		expectSpan(t, waitSpans(t, collector, "upstream-error")["upstream-error"], line, merged(answeredBy(t, backend.URL()),
			map[string]any{"kaiak.key.root_group": "team", "gen_ai.input.messages": helloInput}))
		captured = append(captured, "upstream-error")
	})

	t.Run("a client that leaves mid-stream: the output it received", func(t *testing.T) {
		pace := make(chan struct{})
		backend.SetReply(fakebackend.Reply{Pace: pace})
		req, err := http.NewRequest(http.MethodPost, g.api+"/v1/chat/completions", strings.NewReader(helloChat(true)))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+keys.capChild)
		req.Header.Set("X-Request-Id", "client-left")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		stream := bufio.NewReader(resp.Body)
		readEventData(t, stream)
		pace <- struct{}{} // the second event, then the backend waits
		readEventData(t, stream)
		resp.Body.Close()
		line := g.settled(t, "client-left")
		backend.SetReply(fakebackend.Reply{})
		if line["kaiak.capture"] != "queued" || line["kaiak.relay_end"] != "client_closed" {
			t.Errorf("request line %v, want kaiak.relay_end client_closed", line)
		}
		expectSpan(t, waitSpans(t, collector, "client-left")["client-left"], line, merged(answeredBy(t, backend.URL()),
			map[string]any{"kaiak.key.root_group": "team", "gen_ai.input.messages": helloInput, "gen_ai.request.stream": true,
				"gen_ai.response.id": "fake-1", "gen_ai.response.model": "cap", "gen_ai.response.finish_reasons": []any{"error"},
				"gen_ai.output.messages": `[{"role":"assistant","parts":[{"type":"text","content":"Hello from"}]}]`}))
		captured = append(captured, "client-left")
	})

	t.Run("the session header: kept, a malformed one ignored, absent", func(t *testing.T) {
		long := "conv-" + strings.Repeat("x", 252) // 257 characters
		var checks spanChecks
		for _, c := range []struct {
			id string
			// header is the request's Kaiak-Session-Id; nil sends none.
			header           map[string]string
			session, ignored string
		}{
			{"session-absent", nil, "", ""},
			{"session-kept", map[string]string{"Kaiak-Session-Id": "conv-7f3a-e2e"}, "conv-7f3a-e2e", ""},
			{"session-spaces", map[string]string{"Kaiak-Session-Id": "conv 7f3a e2e"}, "", "invalid_characters"},
			{"session-too-long", map[string]string{"Kaiak-Session-Id": long}, "", "too_long"},
			{"session-empty", map[string]string{"Kaiak-Session-Id": ""}, "", "empty"},
		} {
			if r := g.postRaw(t, "/v1/chat/completions", keys.capChild, c.id, c.header, helloChat(false)); r.StatusCode != http.StatusOK {
				t.Fatalf("%s: %d %s, want the request served", c.id, r.StatusCode, r.body)
			}
			line := g.settled(t, c.id)
			if got, _ := line["kaiak.session_id.ignored"].(string); got != c.ignored || line["kaiak.capture"] != "queued" {
				t.Errorf("%s: request line %v, want kaiak.session_id.ignored %q", c.id, line, c.ignored)
			}
			want := helloAnswered(t, backend.URL(), "team")
			if c.session != "" {
				want["gen_ai.conversation.id"] = c.session
			}
			checks.add(line, want)
		}
		captured = append(captured, checks.check(t, collector)...)
		if text := g.logs.text(); strings.Contains(text, "conv-7f3a-e2e") || strings.Contains(text, "conv 7f3a e2e") ||
			strings.Contains(text, long) {
			t.Error("the log carries a session ID")
		}
		for _, r := range backend.Requests() {
			if v := r.Header.Values("Kaiak-Session-Id"); v != nil {
				t.Errorf("the backend received Kaiak-Session-Id %q", v)
			}
		}
	})

	t.Run("the backend receives the same request captured or not", func(t *testing.T) {
		for i, c := range []struct{ path, body string }{
			{"/v1/chat/completions", `{"model":"cap","stream":true,"messages":[{"role":"user","content":"<hi> & bye"}]}`},
			{"/v1/chat/completions", `{"model":"cap","messages":[],"temperature":0.2,"x_unknown":{"a":1}}`},
			{"/v1/messages", `{"model":"cap","max_tokens":16,"system":"terse","messages":[{"role":"user","content":"hi"}]}`},
			{"/v1/responses", `{"model":"cap","input":"hi","stream":true}`},
		} {
			// The same request ID both times: the backend is sent it.
			id := fmt.Sprintf("same-%d", i)
			var sent []*fakebackend.Request
			for _, key := range []string{keys.capChild, keys.team} {
				before := len(backend.Requests())
				r := g.postRaw(t, c.path, key, id, map[string]string{"Kaiak-Session-Id": "conv-same"}, c.body)
				if r.StatusCode != http.StatusOK {
					t.Fatalf("%s: %d %s", id, r.StatusCode, r.body)
				}
				sent = append(sent, backend.Requests()[before:]...)
			}
			if on := g.logs.wait(t, id+" captured", msg("request", "kaiak.request.id", id, "kaiak.key.id", "k-cap-child")); on["kaiak.capture"] != "queued" {
				t.Errorf("%s: kaiak.capture %v with the capturing key", id, on["kaiak.capture"])
			}
			if off := g.logs.wait(t, id+" not captured", msg("request", "kaiak.request.id", id, "kaiak.key.id", "k-team")); off["kaiak.capture"] != nil {
				t.Errorf("%s: kaiak.capture %v with the other key", id, off["kaiak.capture"])
			}
			if len(sent) != 2 {
				t.Fatalf("%s: the backend received %d requests, want 2", id, len(sent))
			}
			if on, off := sent[0], sent[1]; string(on.Body) != string(off.Body) || on.Path != off.Path ||
				!maps.EqualFunc(on.Header, off.Header, slices.Equal) {
				t.Errorf("%s: captured, the backend received\n%s %s %v\nnot captured\n%s %s %v", id, on.Path, on.Body, on.Header,
					off.Path, off.Body, off.Header)
			}
			captured = append(captured, id)
		}
	})

	// Every capture handed over is exported: the counts say so, and nothing was
	// dropped.
	waitSpans(t, collector, captured...)
	g.waitMetric(t, "the spans exported", spanExportedSeries(""), func(v float64) bool { return v == float64(len(captured)) })
	if v := g.metric(t, spanProcessedSeries("")); v != float64(len(captured)) {
		t.Errorf("%s = %v, want %d", spanProcessedSeries(""), v, len(captured))
	}
	for _, drop := range spanDrops {
		if v, ok := g.metricValue(t, spanProcessedSeries(drop)); !ok || v != 0 {
			t.Errorf("%s = %v (exposed %v), want 0", spanProcessedSeries(drop), v, ok)
		}
	}
	g.stop(t)

	if got, want := slices.Sorted(maps.Keys(requestSpans(t, collector))), slices.Sorted(slices.Values(captured)); !slices.Equal(got, want) {
		t.Errorf("spans for\n %q\nwant\n %q", got, want)
	}
	for _, r := range collector.Received() {
		if r.Path != "/v1/traces" || len(r.Export.ResourceSpans) != 1 || len(r.Export.ResourceSpans[0].ScopeSpans) != 1 {
			t.Errorf("an export to %s with %d resources, want traces under one resource and one scope", r.Path,
				len(r.Export.ResourceSpans))
			continue
		}
		resource := resourceOf(t, r.Export.ResourceSpans[0].Resource)
		if resource["service.name"] != "kaiak" || resource["service.instance.id"] != "e2e" || resource["service.version"] == nil {
			t.Errorf("resource %v", resource)
		}
		if scope := r.Export.ResourceSpans[0].ScopeSpans[0].Scope.Name; scope != "kaiak" {
			t.Errorf("scope %q, want kaiak", scope)
		}
	}
	text := g.logs.text()
	for _, content := range []string{"Answer briefly", "What is in this image", "iVBORw0K", "Look a word up", "A cat on a mat",
		"Looking it up", "Be terse", "Weather in Oslo", "4 °C", "Bergen", "Answer in French", "Describe it", "example.com",
		"zoomed", "Un chat", "Say hello", "Hello from the fake", "<hi> & bye"} {
		if strings.Contains(text, content) {
			t.Errorf("the log carries content: %q", content)
		}
	}
}

// The drain: the captures of the requests finished before SIGTERM, of a stream the
// drain lets finish and of one it cuts at its timeout — with what the client
// received — are at the collector before the process exits, and the final metric
// export, which follows the trace flush, counts every one handed over and exported.
func TestCaptureDrain(t *testing.T) {
	backend := fakebackend.New()
	defer backend.Close()
	collector := fakeotlp.New(t, nil)
	// A drain short enough to end before the trace export's first periodic send, a
	// second after its start: what reaches the collector, the final flush sends.
	g, keys := startCapturing(t, backend.URL(), backend.URL(), tracesTo(collector),
		"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT="+collector.URL+"/v1/metrics", "KAIAK_DRAIN_TIMEOUT_MS=500")

	finished := []struct{ id, path, body string }{
		{"drain-chat", "/v1/chat/completions", helloChat(false)},
		{"drain-messages", "/v1/messages", `{"model":"cap","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"Say hello."}]}`},
		{"drain-responses", "/v1/responses", `{"model":"cap","input":"Say hello."}`},
	}
	for _, c := range finished {
		if r := g.postRaw(t, c.path, keys.capChild, c.id, nil, c.body); r.StatusCode != http.StatusOK {
			t.Fatalf("%s: %d %s", c.id, r.StatusCode, r.body)
		}
		if line := g.settled(t, c.id); line["kaiak.capture"] != "queued" {
			t.Fatalf("%s: request line %v", c.id, line)
		}
	}
	// In flight at SIGTERM: a stream the drain lets finish, one it cuts.
	pace := make(chan struct{})
	backend.QueueReplies(fakebackend.Reply{Pace: pace},
		fakebackend.Reply{Fault: &fakebackend.StreamFault{At: 1, Kind: fakebackend.Hang}})
	finishing := openStream(t, g, keys.capChild, "drain-finishing", chatBody("cap", true, nil))
	cut := openStream(t, g, keys.capChild, "drain-cut", chatBody("cap", true, nil))

	g.signal(t, syscall.SIGTERM)
	g.logs.wait(t, "the drain refusing new requests", msg("draining: refusing new requests"))
	close(pace)
	finishStream(t, finishing)
	_, _ = io.ReadAll(cut.Body) // ends when the drain cuts it
	g.waitExit(t)
	if drained := g.logs.wait(t, "the drain end", msg("drained")); drained["kaiak.drain.cut_off"] != 1.0 {
		t.Errorf("drained line %v, want one request cut off", drained)
	}

	spans := requestSpans(t, collector)
	want := []string{"drain-chat", "drain-messages", "drain-responses", "drain-finishing", "drain-cut"}
	if got := slices.Sorted(maps.Keys(spans)); !slices.Equal(got, slices.Sorted(slices.Values(want))) {
		t.Fatalf("spans at exit for %q, want %q", got, want)
	}
	answered := merged(answeredBy(t, backend.URL()), map[string]any{"kaiak.key.root_group": "team",
		"gen_ai.input.messages": helloInput, "gen_ai.request.stream": true, "gen_ai.response.id": "fake-1",
		"gen_ai.response.model": "cap"})
	expectSpan(t, spans["drain-finishing"], g.settled(t, "drain-finishing"), merged(answered,
		map[string]any{"gen_ai.output.messages": helloOutput, "gen_ai.response.finish_reasons": []any{"stop"}}))
	cutLine := g.settled(t, "drain-cut")
	if cutLine["kaiak.relay_end"] != "shutdown" {
		t.Errorf("drain-cut: request line %v, want kaiak.relay_end shutdown", cutLine)
	}
	expectSpan(t, spans["drain-cut"], cutLine, merged(answered,
		map[string]any{"gen_ai.output.messages": `[{"role":"assistant","parts":[{"type":"text","content":"Hello"}]}]`,
			"gen_ai.response.finish_reasons": []any{"error"}}))

	// The final metric export comes after the trace flush: it counts every span.
	received := collector.Received()
	final, lastTraces := -1, -1
	for i, r := range received {
		switch {
		case len(r.Export.ResourceMetrics) > 0:
			final = i
		case len(r.Export.ResourceSpans) > 0:
			lastTraces = i
		}
	}
	if final < 0 || lastTraces < 0 || lastTraces > final {
		t.Fatalf("the last trace export at %d, the final metric export at %d: want traces first", lastTraces, final)
	}
	exposition, _ := pushedExposition(t, received[final])
	for series, want := range map[string]float64{
		spanProcessedSeries(""): 5, spanExportedSeries(""): 5, spanProcessedSeries("shutdown"): 0,
		spanProcessedSeries("memory"): 0,
	} {
		if got, ok := exposition.samples[sortedSeries(t, series)]; !ok || got != want {
			t.Errorf("final metric export: %s = %v (exported %v), want %v", series, got, ok, want)
		}
	}
}

// A capture budget too small for any capture: every capture is dropped — a request
// body the budget cannot copy, an answer it cannot hold — and every request is served
// whole; the request lines and the counts say memory; nothing reaches the collector.
func TestCaptureBudget(t *testing.T) {
	backend := fakebackend.New()
	defer backend.Close()
	collector := fakeotlp.New(t, nil)
	// The answer's copy starts at 4 KiB: no answer fits.
	g, keys := startCapturing(t, backend.URL(), backend.URL(), tracesTo(collector), "KAIAK_CAPTURE_MEMORY_BYTES=2048")

	big := `{"model":"cap","messages":[{"role":"user","content":"` + strings.Repeat("word ", 600) + `"}]}`
	r := g.postRaw(t, "/v1/chat/completions", keys.capChild, "budget-copy", nil, big)
	if r.StatusCode != http.StatusOK || !strings.Contains(string(r.body), "Hello from the fake") {
		t.Fatalf("budget-copy: %d %s, want the whole answer", r.StatusCode, r.body)
	}
	r = g.postRaw(t, "/v1/chat/completions", keys.capChild, "budget-answer", nil, helloChat(true))
	var text strings.Builder
	evs := events(r.body)
	for _, ev := range evs {
		var chunk struct {
			Choices []struct{ Delta struct{ Content string } }
		}
		if json.Unmarshal([]byte(ev), &chunk) == nil && len(chunk.Choices) > 0 {
			text.WriteString(chunk.Choices[0].Delta.Content)
		}
	}
	if r.StatusCode != http.StatusOK || text.String() != "Hello from the fake" || len(evs) == 0 || evs[len(evs)-1] != "[DONE]" {
		t.Fatalf("budget-answer: %d %q %q, want the whole stream", r.StatusCode, text.String(), evs)
	}
	if r := g.postRaw(t, "/v1/chat/completions", keys.team, "budget-not-captured", nil, helloChat(false)); r.StatusCode != http.StatusOK {
		t.Fatalf("budget-not-captured: %d %s", r.StatusCode, r.body)
	}
	for id, want := range map[string]any{"budget-copy": "memory", "budget-answer": "memory", "budget-not-captured": nil} {
		if line := g.settled(t, id); line["kaiak.capture"] != want {
			t.Errorf("%s: kaiak.capture %v, want %v", id, line["kaiak.capture"], want)
		}
	}
	for series, want := range map[string]float64{spanProcessedSeries("memory"): 2, spanProcessedSeries(""): 0} {
		if got := g.metric(t, series); got != want {
			t.Errorf("%s = %v, want %v", series, got, want)
		}
	}
	g.stop(t)
	if n := collector.Requests(); n != 0 {
		t.Errorf("the collector received %d exports, want none", n)
	}
}

// Groups capture while this gateway's trace export is off — nothing set, or only the
// general OTLP endpoint, which sends logs and metrics: the applied config is warned
// about, the requests are served and say export_off, the session header is not even
// read, and no span is made or sent. OTEL_TRACES_EXPORTER=otlp beside the general
// endpoint is the switch that sends them, to its /v1/traces.
func TestCaptureTraceExportOff(t *testing.T) {
	backend := fakebackend.New()
	defer backend.Close()
	const warning = "groups capture requests but trace export is off: nothing is captured"
	// run starts a gateway with env, sends a captured request with a malformed
	// session header, stops the gateway and returns it with its request line.
	run := func(t *testing.T, env ...string) (*gateway, map[string]any) {
		t.Helper()
		g, keys := startCapturing(t, backend.URL(), backend.URL(), env...)
		r := g.postRaw(t, "/v1/chat/completions", keys.capChild, "off", map[string]string{"Kaiak-Session-Id": "two words"},
			helloChat(false))
		if r.StatusCode != http.StatusOK {
			t.Fatalf("%d %s", r.StatusCode, r.body)
		}
		line := g.settled(t, "off")
		// The trace export's counts exist only with the export on.
		if _, exposed := g.metricValue(t, spanProcessedSeries("")); exposed != (line["kaiak.capture"] != "export_off") {
			t.Errorf("the trace export's counts exposed %v with kaiak.capture %v", exposed, line["kaiak.capture"])
		}
		g.stop(t)
		return g, line
	}
	expectOff := func(t *testing.T, g *gateway, line map[string]any) {
		t.Helper()
		if line["kaiak.capture"] != "export_off" || line["kaiak.session_id.ignored"] != nil {
			t.Errorf("request line %v, want kaiak.capture export_off and the session header not read", line)
		}
		// cap-child, cap-root and limited capture.
		if warned := g.logs.wait(t, "the capture warning", msg(warning)); warned["level"] != "WARN" ||
			warned["kaiak.config.capture_groups"] != 3.0 {
			t.Errorf("warning %v, want WARN naming 3 groups", warned)
		}
		if starting := g.logs.wait(t, "kaiak starting", msg("kaiak starting")); starting["kaiak.trace_export.endpoint"] != nil {
			t.Errorf("kaiak starting names a trace export endpoint: %v", starting)
		}
	}

	t.Run("no export", func(t *testing.T) {
		g, line := run(t)
		expectOff(t, g, line)
	})

	t.Run("the general endpoint alone: logs and metrics, no spans", func(t *testing.T) {
		collector := fakeotlp.New(t, nil)
		g, line := run(t, "OTEL_EXPORTER_OTLP_ENDPOINT="+collector.URL)
		expectOff(t, g, line)
		paths := map[string]int{}
		for _, r := range collector.Received() {
			paths[r.Path]++
		}
		if paths["/v1/logs"] == 0 || paths["/v1/metrics"] == 0 || paths["/v1/traces"] != 0 || len(collector.Spans()) != 0 {
			t.Errorf("exports by path %v, %d spans; want logs and metrics, no traces", paths, len(collector.Spans()))
		}
	})

	t.Run("OTEL_TRACES_EXPORTER=otlp sends them to the general endpoint", func(t *testing.T) {
		collector := fakeotlp.New(t, nil)
		g, line := run(t, "OTEL_EXPORTER_OTLP_ENDPOINT="+collector.URL, "OTEL_TRACES_EXPORTER=otlp",
			"OTEL_LOGS_EXPORTER=none", "OTEL_METRICS_EXPORTER=none")
		if line["kaiak.capture"] != "queued" || line["kaiak.session_id.ignored"] != "invalid_characters" {
			t.Errorf("request line %v, want queued with the session header ignored", line)
		}
		if _, warned := g.logs.find(msg(warning)); warned {
			t.Error("warned of capture with trace export on")
		}
		if spans := requestSpans(t, collector); len(spans) != 1 || spans["off"].Name != "chat cap" {
			t.Errorf("spans %v, want the request's", spans)
		}
		for _, r := range collector.Received() {
			if r.Path != "/v1/traces" {
				t.Errorf("an export to %s, want /v1/traces alone", r.Path)
			}
		}
	})
}
