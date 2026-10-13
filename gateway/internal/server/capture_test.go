package server

// Request capture through the pipeline (docs/specs/GATEWAY.md, Request capture): the
// requests the capture stage captures, the session header, the capture budget, the
// request line's kaiak.capture, each span's attributes but the content, and that
// capture changes nothing the client or the backend sees, nor when the client's
// response ends.

import (
	"bufio"
	"context"
	"io"
	"maps"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"kaiak/internal/capture"
	"kaiak/internal/fakebackend"
	"kaiak/internal/telemetry/fakeotlp"
)

// capturing is the test config edit that turns capture on for group eval, the
// workload key's; ann's, the user key's, stays off.
func capturing(t *testing.T) func(doc string) string {
	return func(doc string) string {
		return replaceOnce(t, doc, `"eval": { "parent": "research", "capture": false`,
			`"eval": { "parent": "research", "capture": true`)
	}
}

// newCapturingGateway is the test gateway as opts shape it, with trace export on,
// group eval capturing and model msg — on a vllm backend — served on the three chat
// endpoints.
func newCapturingGateway(t *testing.T, opts testOptions) *testGateway {
	t.Helper()
	opts.traces, opts.edit = true, capturing(t)
	g := buildTestGateway(t, opts)
	withMessagesModels(t, g, capturing(t))
	return g
}

// capturedSpans waits until no request is in flight — each has handed its capture
// to the builder — then builds every capture (the captures finish) and flushes the
// trace export: the spans the collector received, by kaiak.request.id. It ends the
// gateway's captures: a test calls it once, at its end.
func (g *testGateway) capturedSpans(t *testing.T) map[string]fakeotlp.Span {
	t.Helper()
	waitIdle(t, g)
	ctx, cancel := context.WithTimeout(context.Background(), waitTimeout)
	defer cancel()
	g.captures.Finish(ctx)
	if err := g.traces.ForceFlush(ctx); err != nil {
		t.Fatalf("trace export flush: %v", err)
	}
	spans := map[string]fakeotlp.Span{}
	for _, s := range g.collector.Spans() {
		id, _ := s.Attrs(t)["kaiak.request.id"].(string)
		if _, dup := spans[id]; dup {
			t.Errorf("two spans for request %s", id)
		}
		spans[id] = s
	}
	return spans
}

// expectAllGivenBack checks that the captures hold nothing and dropped nothing: every
// capture was built, exported and its bytes given back.
func expectAllGivenBack(t *testing.T, g *testGateway) {
	t.Helper()
	if held, drops := g.captures.InUse(), g.captures.Drops(); held != 0 || drops != (capture.Drops{}) {
		t.Errorf("captures hold %d bytes with drops %+v; want 0 and none", held, drops)
	}
}

// The capture stage runs on the chat endpoints alone, for a key whose own group
// captures, once the inbound stage has accepted the body: other groups, other
// endpoints and requests refused before it have no capture and no kaiak.capture.
// With trace export off a capturing group's requests say export_off, and nothing
// else is done — not even reading the session header.
func TestCaptureStageDecides(t *testing.T) {
	g := newCapturingGateway(t, testOptions{})
	for _, c := range []struct {
		id, path, key, body string
		// status is the answer's; outcome the line's kaiak.capture ("" absent).
		status  int
		outcome string
	}{
		{"chat", "/v1/chat/completions", workloadKey, `{"model":"msg","messages":[]}`, 200, "queued"},
		{"messages", "/v1/messages", workloadKey, `{"model":"msg","max_tokens":8,"messages":[]}`, 200, "queued"},
		{"responses", "/v1/responses", workloadKey, `{"model":"msg","input":"hi"}`, 200, "queued"},
		{"refused after the stage", "/v1/chat/completions", workloadKey, `{"model":"nope","messages":[]}`, 404, "queued"},
		{"group not capturing", "/v1/chat/completions", userKey, `{"model":"open","messages":[]}`, 200, ""},
		{"completions", "/v1/completions", workloadKey, `{"model":"msg","prompt":"hi"}`, 200, ""},
		{"embeddings", "/v1/embeddings", workloadKey, `{"model":"msg","input":"hi"}`, 200, ""},
		{"count tokens", "/v1/messages/count_tokens", workloadKey, `{"model":"msg","messages":[]}`, 200, ""},
		{"refused by the inbound stage", "/v1/chat/completions", workloadKey, `{"model":`, 400, ""},
	} {
		w := do(t, g.h, call{method: "POST", path: c.path, key: c.key, body: c.body,
			header: map[string]string{"X-Request-Id": strings.ReplaceAll(c.id, " ", "-")}})
		if w.Code != c.status {
			t.Errorf("%s: status %d, want %d: %s", c.id, w.Code, c.status, w.Body.String())
		}
		fields := logFields(t, g, strings.ReplaceAll(c.id, " ", "-"))
		if got, _ := fields["kaiak.capture"].(string); got != c.outcome {
			t.Errorf("%s: kaiak.capture %q, want %q", c.id, got, c.outcome)
		}
	}
	spans := g.capturedSpans(t)
	if got := slices.Sorted(maps.Keys(spans)); !slices.Equal(got, []string{"chat", "messages", "refused-after-the-stage", "responses"}) {
		t.Errorf("spans for %v, want the captured requests'", got)
	}
	expectAllGivenBack(t, g)

	off := buildTestGateway(t, testOptions{edit: capturing(t)})
	do(t, off.h, call{method: "POST", path: "/v1/chat/completions", key: workloadKey, body: `{"model":"open","messages":[]}`,
		header: map[string]string{"X-Request-Id": "off", "Kaiak-Session-Id": "two words"}})
	fields := logFields(t, off, "off")
	expectFields(t, fields, map[string]any{"kaiak.capture": "export_off"}, "kaiak.session_id.ignored")
}

// Kaiak-Session-Id is optional: a valid one is the span's gen_ai.conversation.id; a
// malformed one is ignored — the request served, the span and the line naming why,
// never the value. It never reaches the backend.
func TestCaptureSessionHeader(t *testing.T) {
	g := newCapturingGateway(t, testOptions{})
	long := strings.Repeat("s", 256)
	cases := []struct {
		id string
		// value is the header's; nil sends none.
		value   *string
		session string
		ignored string
	}{
		{id: "none"},
		{id: "valid", value: ptr("conv-7f3a"), session: "conv-7f3a"},
		{id: "at-the-limit", value: ptr(long), session: long},
		{id: "too-long", value: ptr(long + "s"), ignored: "too_long"},
		{id: "bad-characters", value: ptr("two words"), ignored: "invalid_characters"},
		{id: "not-ascii", value: ptr("café-1"), ignored: "invalid_characters"},
		{id: "empty", value: ptr(""), ignored: "empty"},
	}
	for _, c := range cases {
		header := map[string]string{"X-Request-Id": c.id}
		if c.value != nil {
			header["Kaiak-Session-Id"] = *c.value
		}
		w := do(t, g.h, call{method: "POST", path: "/v1/chat/completions", key: workloadKey,
			body: `{"model":"open","messages":[]}`, header: header})
		if w.Code != http.StatusOK {
			t.Errorf("%s: status %d, want 200: %s", c.id, w.Code, w.Body.String())
		}
		line := logLine(t, g, c.id)
		if c.value != nil && *c.value != "" && strings.Contains(line, *c.value) {
			t.Errorf("%s: the line carries the session ID: %s", c.id, line)
		}
		fields := logFields(t, g, c.id)
		if got, _ := fields["kaiak.session_id.ignored"].(string); got != c.ignored {
			t.Errorf("%s: kaiak.session_id.ignored %q on the line, want %q", c.id, got, c.ignored)
		}
	}
	for _, r := range g.backend.Requests() {
		if v := r.Header.Values("Kaiak-Session-Id"); v != nil {
			t.Errorf("the backend received Kaiak-Session-Id %q", v)
		}
	}
	spans := g.capturedSpans(t)
	for _, c := range cases {
		attrs := spans[c.id].Attrs(t)
		session, _ := attrs["gen_ai.conversation.id"].(string)
		ignored, _ := attrs["kaiak.session_id.ignored"].(string)
		if session != c.session || ignored != c.ignored {
			t.Errorf("%s: span session %q, ignored %q; want %q, %q", c.id, session, ignored, c.session, c.ignored)
		}
	}
}

// The span of each outcome from the capture stage on carries its attributes
// (docs/specs/GATEWAY.md, Request capture → Attributes): a success, a stream, a
// refusal after the stage, an upstream error and a stream cut short — the answer's
// content when the client received one, all of it or what arrived.
func TestCaptureSpanAttributes(t *testing.T) {
	// The attributes every span has; a routed request's; a settled one's; an answered
	// one's; one whose stream was cut short — no finish reason.
	always := []string{"gen_ai.operation.name", "gen_ai.request.model", "kaiak.request.id", "kaiak.key.id",
		"kaiak.key.group", "kaiak.key.root_group"}
	routed := []string{"gen_ai.provider.name", "server.address", "server.port", "kaiak.backend.id", "kaiak.backend.type",
		"kaiak.deployment.model", "kaiak.attempts"}
	settled := []string{"gen_ai.usage.input_tokens", "gen_ai.usage.cache_read.input_tokens",
		"gen_ai.usage.cache_write.input_tokens", "gen_ai.usage.output_tokens", "gen_ai.usage.reasoning.output_tokens",
		"kaiak.usage.cost_usd", "kaiak.usage.estimated", "kaiak.usage.partial"}
	answered := []string{"gen_ai.response.id", "gen_ai.response.model", "gen_ai.response.finish_reasons",
		"gen_ai.output.messages"}
	cutShort := []string{"gen_ai.response.id", "gen_ai.response.model", "gen_ai.output.messages"}
	keys := func(groups ...[]string) []string { return slices.Concat(groups...) }

	g := newCapturingGateway(t, testOptions{})
	url := serveGateway(t, g)
	post := func(id, body string, reply fakebackend.Reply) {
		t.Helper()
		g.backend.SetReply(reply)
		req, _ := http.NewRequest(http.MethodPost, url+"/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+workloadKey)
		req.Header.Set("X-Request-Id", id)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.ReadAll(resp.Body) // a stream cut short ends in an error
		resp.Body.Close()
		waitIdle(t, g)
	}
	start := time.Now()
	post("success", `{"model":"open","messages":[]}`, fakebackend.Reply{})
	post("stream", `{"model":"open","stream":true,"messages":[]}`, fakebackend.Reply{})
	post("refused", `{"model":"nope","messages":[]}`, fakebackend.Reply{})
	post("upstream-error", `{"model":"open","messages":[]}`, fakebackend.Reply{Status: http.StatusInternalServerError})
	post("cut", `{"model":"open","stream":true,"messages":[]}`,
		fakebackend.Reply{Fault: &fakebackend.StreamFault{At: 2, Kind: fakebackend.Cut}})
	end := time.Now()
	spans := g.capturedSpans(t)

	port, _ := strconv.ParseInt(g.backend.URL()[strings.LastIndexByte(g.backend.URL(), ':')+1:], 10, 64)
	for _, c := range []struct {
		id     string
		want   []string
		values map[string]any
		// errorType is the span's error.type and status message; "" for no status.
		errorType string
	}{
		{id: "success", want: keys(always, routed, settled, answered), values: map[string]any{
			"gen_ai.operation.name": "chat", "gen_ai.provider.name": "openai-compatible", "gen_ai.request.model": "open",
			"server.address": "127.0.0.1", "server.port": port, "kaiak.request.id": "success", "kaiak.key.id": "k-eval",
			"kaiak.key.group": "eval", "kaiak.key.root_group": "research", "kaiak.backend.id": "local",
			"kaiak.backend.type": "openai-compatible", "kaiak.deployment.model": "open", "kaiak.attempts": int64(1),
			"gen_ai.usage.input_tokens": int64(7), "gen_ai.usage.output_tokens": int64(4), "kaiak.usage.cost_usd": float64(0),
			"kaiak.usage.estimated": false, "kaiak.usage.partial": false, "gen_ai.response.id": "fake-1",
			"gen_ai.response.model":  "open",
			"gen_ai.output.messages": `[{"role":"assistant","parts":[{"type":"text","content":"Hello from the fake"}]}]`}},
		{id: "stream", want: keys(always, routed, settled, answered,
			[]string{"gen_ai.request.stream", "kaiak.time_to_first_token"}),
			values: map[string]any{"gen_ai.request.stream": true, "kaiak.usage.partial": false,
				"gen_ai.output.messages": `[{"role":"assistant","parts":[{"type":"text","content":"Hello from the fake"}]}]`}},
		{id: "refused", want: keys(always, []string{"error.type"}), errorType: "model_not_found",
			values: map[string]any{"gen_ai.request.model": "nope"}},
		{id: "upstream-error", want: keys(always, routed, settled, []string{"error.type"}), errorType: "upstream_error"},
		{id: "cut", want: keys(always, routed, settled, cutShort, []string{"gen_ai.request.stream",
			"kaiak.time_to_first_token", "error.type", "kaiak.relay_end"}), errorType: "upstream_failed",
			values: map[string]any{"kaiak.relay_end": "upstream_failed", "kaiak.usage.partial": true,
				"gen_ai.output.messages": `[{"role":"assistant","parts":[{"type":"text","content":"Hello from"}]}]`}},
	} {
		span, ok := spans[c.id]
		if !ok {
			t.Errorf("%s: no span", c.id)
			continue
		}
		attrs := span.Attrs(t)
		if got, want := slices.Sorted(maps.Keys(attrs)), slices.Sorted(slices.Values(c.want)); !slices.Equal(got, want) {
			t.Errorf("%s: attributes\n got %v\nwant %v", c.id, got, want)
		}
		for k, v := range c.values {
			if attrs[k] != v {
				t.Errorf("%s: %s = %v (%T), want %v (%T)", c.id, k, attrs[k], attrs[k], v, v)
			}
		}
		if reasons, ok := attrs["gen_ai.response.finish_reasons"]; ok && !reflect.DeepEqual(reasons, []any{"stop"}) {
			t.Errorf("%s: finish reasons %v, want stop", c.id, reasons)
		}
		wantName := "chat open"
		if c.id == "refused" {
			wantName = "chat nope"
		}
		if span.Name != wantName || span.Kind != 3 || len(span.TraceID) != 32 || len(span.SpanID) != 16 {
			t.Errorf("%s: span %q kind %d, IDs %q %q", c.id, span.Name, span.Kind, span.TraceID, span.SpanID)
		}
		from, _ := strconv.ParseInt(span.StartTimeUnixNano, 10, 64)
		to, _ := strconv.ParseInt(span.EndTimeUnixNano, 10, 64)
		if from < start.UnixNano() || to < from || to > end.UnixNano() {
			t.Errorf("%s: span from %d to %d, want within the request (%d to %d)", c.id, from, to, start.UnixNano(),
				end.UnixNano())
		}
		switch {
		case c.errorType == "" && span.Status != nil:
			t.Errorf("%s: status %+v, want none", c.id, span.Status)
		case c.errorType != "" && (span.Status == nil || span.Status.Code != 2 || span.Status.Message != c.errorType ||
			attrs["error.type"] != c.errorType):
			t.Errorf("%s: status %+v, error.type %v; want an error status and error.type %s", c.id, span.Status,
				attrs["error.type"], c.errorType)
		}
	}
	expectAllGivenBack(t, g)
}

// A captured chat request's span carries its content (docs/specs/GATEWAY.md, Request
// capture → Chat Completions): the request as the client sent it — never the output
// limit the gateway injects — and the answer as the client received it, the public
// model name in it; streamed, the answer assembles to what it gives unstreamed, and a
// stream cut short keeps what arrived. None of the content reaches the request line.
func TestCaptureChatCompletionsContent(t *testing.T) {
	g := newCapturingGateway(t, testOptions{})
	url := serveGateway(t, g)
	reply := fakebackend.Reply{Reasoning: []string{"A cat ", "on a mat."}, Chunks: []string{"A ", "cat."}}
	cut := reply
	cut.Fault = &fakebackend.StreamFault{At: 1, Kind: fakebackend.Cut}
	body := func(stream bool) string {
		return `{"model": "msg", "stream": ` + strconv.FormatBool(stream) + `, "temperature": 0.2,
			"messages": [
				{"role": "system", "content": "Answer briefly."},
				{"role": "user", "content": [
					{"type": "text", "text": "What is in this image?"},
					{"type": "image_url", "image_url": {"url": "data:image/png;base64,iVBORw0K"}}]}],
			"tools": [{"type": "function", "function": {"name": "lookup",
				"description": "Look a word up", "parameters": {"type": "object"}}}]}`
	}
	for _, c := range []struct {
		id     string
		stream bool
		reply  fakebackend.Reply
	}{{"whole", false, reply}, {"streamed", true, reply}, {"cut", true, cut}} {
		g.backend.SetReply(c.reply)
		req, _ := http.NewRequest(http.MethodPost, url+"/v1/chat/completions", strings.NewReader(body(c.stream)))
		req.Header.Set("Authorization", "Bearer "+workloadKey)
		req.Header.Set("X-Request-Id", c.id)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.ReadAll(resp.Body) // a stream cut short ends in an error
		resp.Body.Close()
		waitIdle(t, g)
	}
	if line := g.logText(); strings.Contains(line, "What is in this image") || strings.Contains(line, "on a mat") {
		t.Errorf("the log carries content:\n%s", line)
	}
	spans := g.capturedSpans(t)

	input := `[{"role":"system","parts":[{"type":"text","content":"Answer briefly."}]},` +
		`{"role":"user","parts":[{"type":"text","content":"What is in this image?"},` +
		`{"type":"blob","modality":"image","mime_type":"image/png","content":"iVBORw0K"}]}]`
	tools := `[{"type":"function","name":"lookup","description":"Look a word up","parameters":{"type":"object"}}]`
	for _, c := range []struct {
		id     string
		output string
		// finished: the answer gave its finish reason.
		finished bool
	}{
		{"whole", `[{"role":"assistant","parts":[{"type":"reasoning","content":"A cat on a mat."},{"type":"text","content":"A cat."}]}]`, true},
		{"streamed", `[{"role":"assistant","parts":[{"type":"reasoning","content":"A cat on a mat."},{"type":"text","content":"A cat."}]}]`, true},
		{"cut", `[{"role":"assistant","parts":[{"type":"reasoning","content":"A cat on a mat."},{"type":"text","content":"A "}]}]`, false},
	} {
		attrs := spans[c.id].Attrs(t)
		for k, want := range map[string]any{"gen_ai.request.temperature": 0.2, "gen_ai.input.messages": input,
			"gen_ai.tool.definitions": tools, "gen_ai.output.messages": c.output, "gen_ai.response.id": "fake-1",
			"gen_ai.response.model": "msg", "kaiak.deployment.model": "msg-back", "gen_ai.provider.name": "vllm"} {
			if got := attrs[k]; got != want {
				t.Errorf("%s: %s =\n %v\nwant\n %v", c.id, k, got, want)
			}
		}
		if _, ok := attrs["gen_ai.request.max_tokens"]; ok {
			t.Errorf("%s: gen_ai.request.max_tokens %v, want none: the client set no output limit", c.id,
				attrs["gen_ai.request.max_tokens"])
		}
		reasons, ok := attrs["gen_ai.response.finish_reasons"]
		if ok != c.finished || (ok && !reflect.DeepEqual(reasons, []any{"stop"})) {
			t.Errorf("%s: finish reasons %v, want stop: %v", c.id, reasons, c.finished)
		}
	}
	expectAllGivenBack(t, g)
}

// A capture the budget cannot hold is dropped, never the request: a body over the
// budget is not copied, and an answer that outgrows it mid-stream stops being
// copied while the client receives all of it. The line says memory, the counts
// count it, and nothing stays held.
func TestCaptureBudget(t *testing.T) {
	body := `{"model":"open","stream":true,"messages":[{"role":"user","content":"` + strings.Repeat("x", 300) + `"}]}`
	chunks := slices.Repeat([]string{"word "}, 100)
	for _, c := range []struct {
		name   string
		memory int64
	}{
		// The body does not fit.
		{"the copy", int64(len(body)) - 1},
		// The body and the answer's first 4 KiB buffer fit; its first doubling does not.
		{"the answer", int64(len(body)) + 4<<10 + 1<<10},
	} {
		t.Run(c.name, func(t *testing.T) {
			g := newCapturingGateway(t, testOptions{captureMemory: c.memory})
			g.backend.SetReply(fakebackend.Reply{Chunks: chunks})
			url := serveGateway(t, g)
			req, _ := http.NewRequest(http.MethodPost, url+"/v1/chat/completions", strings.NewReader(body))
			req.Header.Set("Authorization", "Bearer "+workloadKey)
			req.Header.Set("X-Request-Id", "budget")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			answer, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil || resp.StatusCode != http.StatusOK || strings.Count(string(answer), `"content":"word "`) != len(chunks) ||
				!strings.Contains(string(answer), "data: [DONE]") {
				t.Fatalf("status %d, %v: the client did not receive the whole stream:\n%s", resp.StatusCode, err, answer)
			}
			waitIdle(t, g)
			if held := g.captures.InUse(); held != 0 {
				t.Errorf("%d bytes held once dropped, want 0", held)
			}
			expectFields(t, logFields(t, g, "budget"), map[string]any{"kaiak.capture": "memory"})
			if spans := g.capturedSpans(t); len(spans) != 0 {
				t.Errorf("%d spans, want none", len(spans))
			}
			if drops := g.captures.Drops(); drops != (capture.Drops{Memory: 1}) {
				t.Errorf("drops %+v, want one memory", drops)
			}
		})
	}
}

// However a captured request ends, its capture is built and exported and every byte
// it held goes back to the budget: a success, a refusal, an upstream error, a client
// that left, a stream cut short (the handler panics to cut the connection) and the
// drain's cut.
func TestCaptureGivesEverythingBack(t *testing.T) {
	const chat = `{"model":"open","messages":[]}`
	const stream = `{"model":"open","stream":true,"messages":[]}`
	for _, c := range []struct {
		name string
		run  func(t *testing.T, g *testGateway)
		// errorType is the span's.
		errorType string
	}{
		{"success", func(t *testing.T, g *testGateway) {
			do(t, g.h, call{method: "POST", path: "/v1/chat/completions", key: workloadKey, body: chat})
		}, ""},
		{"refusal", func(t *testing.T, g *testGateway) {
			do(t, g.h, call{method: "POST", path: "/v1/chat/completions", key: workloadKey, body: `{"model":"nope"}`})
		}, "model_not_found"},
		{"upstream error", func(t *testing.T, g *testGateway) {
			g.backend.SetReply(fakebackend.Reply{Status: http.StatusBadGateway})
			do(t, g.h, call{method: "POST", path: "/v1/chat/completions", key: workloadKey, body: chat})
		}, "upstream_error"},
		{"client gone", func(t *testing.T, g *testGateway) {
			g.backend.SetReply(fakebackend.Reply{Before: fakebackend.StallFirstByte})
			ctx, leave := context.WithCancel(context.Background())
			req, _ := http.NewRequestWithContext(ctx, http.MethodPost, serveGateway(t, g)+"/v1/chat/completions",
				strings.NewReader(stream))
			req.Header.Set("Authorization", "Bearer "+workloadKey)
			go func() {
				<-g.backend.Arrivals()
				leave()
			}()
			if resp, err := http.DefaultClient.Do(req); err == nil {
				resp.Body.Close()
			}
		}, "client_closed"},
		{"cut stream", func(t *testing.T, g *testGateway) {
			g.backend.SetReply(fakebackend.Reply{Fault: &fakebackend.StreamFault{At: 2, Kind: fakebackend.Cut}})
			resp := postStreamAs(t, serveGateway(t, g), workloadKey, stream)
			_, _ = io.ReadAll(resp.Body)
			resp.Body.Close()
		}, "upstream_failed"},
		{"drain cut", func(t *testing.T, g *testGateway) {
			g.backend.SetReply(fakebackend.Reply{Fault: &fakebackend.StreamFault{At: 1, Kind: fakebackend.Hang}})
			d := newDrainable(t, g)
			resp, _, err := d.post(t, newClient(t), workloadKey, stream)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			readEvent(t, bufio.NewReader(resp.Body))
			hurry := make(chan struct{})
			close(hurry)
			g.drain.Run(d.l, DrainTimes{Grace: time.Hour, Timeout: time.Hour}, hurry, d.logger)
		}, "shutdown"},
	} {
		t.Run(c.name, func(t *testing.T) {
			g := newCapturingGateway(t, testOptions{})
			c.run(t, g)
			spans := g.capturedSpans(t)
			if len(spans) != 1 {
				t.Fatalf("%d spans, want 1", len(spans))
			}
			for _, span := range spans {
				if got, _ := span.Attrs(t)["error.type"].(string); got != c.errorType {
					t.Errorf("error.type %q, want %q", got, c.errorType)
				}
			}
			expectAllGivenBack(t, g)
			if counts := g.traces.Counts(); counts.Exported != 1 {
				t.Errorf("trace export counts %+v, want the span exported", counts)
			}
		})
	}
}

// postStreamAs posts body to url's chat completions with key.
func postStreamAs(t *testing.T, url, key, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// gatedExporter holds each build at the exporter until gate is closed, telling
// entered first: the builder stands still.
type gatedExporter struct {
	capture.Exporter
	gate, entered chan struct{}
}

func (e gatedExporter) MaxSpanBytes() int {
	e.entered <- struct{}{}
	<-e.gate
	return e.Exporter.MaxSpanBytes()
}

// The span is built after the client's response has ended, never before: with the
// builder held still, the client reads the whole response — streamed or not — to
// its end; then the build runs.
func TestCaptureDoesNotDelayTheResponse(t *testing.T) {
	for _, stream := range []bool{true, false} {
		t.Run("stream "+strconv.FormatBool(stream), func(t *testing.T) {
			gate, entered := make(chan struct{}), make(chan struct{}, 1)
			opened := false
			open := func() {
				if !opened {
					opened = true
					close(gate)
				}
			}
			g := newCapturingGateway(t, testOptions{spanExporter: func(e capture.Exporter) capture.Exporter {
				return gatedExporter{Exporter: e, gate: gate, entered: entered}
			}})
			url := serveGateway(t, g)
			// Cleanups run last first: the gate opens before the server closes — it
			// waits for its handlers — and before the captures finish.
			t.Cleanup(open)
			read := make(chan error, 1)
			go func() {
				resp := postStreamAs(t, url, workloadKey, `{"model":"open","stream":`+strconv.FormatBool(stream)+`,"messages":[]}`)
				_, err := io.ReadAll(resp.Body)
				resp.Body.Close()
				read <- err
			}()
			select {
			case err := <-read:
				if err != nil {
					t.Fatalf("reading the response: %v", err)
				}
			case <-time.After(waitTimeout):
				t.Fatal("the response's end waited for the capture's build")
			}
			select {
			case <-entered:
			case <-time.After(waitTimeout):
				t.Fatal("the capture was never built")
			}
			open()
			if spans := g.capturedSpans(t); len(spans) != 1 {
				t.Errorf("%d spans, want 1", len(spans))
			}
			expectAllGivenBack(t, g)
		})
	}
}

// The backend receives the same request, byte for byte, whether the request is
// captured or not: capture copies, and changes nothing it passes on.
func TestCaptureSendsTheBackendTheSameRequest(t *testing.T) {
	g := newCapturingGateway(t, testOptions{})
	requests := []call{
		{method: "POST", path: "/v1/chat/completions", body: `{"model":"msg","stream":true,"messages":[{"role":"user","content":"<hi> & bye"}]}`},
		{method: "POST", path: "/v1/chat/completions", body: `{"model":"msg","messages":[],"temperature":0.2,"x_unknown":{"a":1}}`},
		{method: "POST", path: "/v1/messages", body: `{"model":"msg","system":"terse","messages":[{"role":"user","content":"hi"}]}`},
		{method: "POST", path: "/v1/responses", body: `{"model":"msg","input":"hi","stream":true}`},
	}
	// Each request is sent twice under the same ID, so that what the backend receives
	// can be the same; the line of the latest is the last with that ID.
	captured := func(id string) bool {
		var line string
		for l := range strings.SplitSeq(g.logText(), "\n") {
			if strings.Contains(l, `"msg":"request"`) && strings.Contains(l, `"kaiak.request.id":"`+id+`"`) {
				line = l
			}
		}
		return strings.Contains(line, `"kaiak.capture":"queued"`)
	}
	send := func(capturing bool) []*fakebackend.Request {
		t.Helper()
		before := len(g.backend.Requests())
		for i, c := range requests {
			id := "same-" + strconv.Itoa(i)
			c.key = workloadKey
			c.header = map[string]string{"X-Request-Id": id, "Kaiak-Session-Id": "conv-1"}
			if w := do(t, g.h, c); w.Code != http.StatusOK || captured(id) != capturing {
				t.Fatalf("%s: status %d, captured %v; want 200 and %v", c.path, w.Code, captured(id), capturing)
			}
		}
		return g.backend.Requests()[before:]
	}
	on := send(true)
	withMessagesModels(t, g, nil)
	off := send(false)
	if len(on) != len(requests) || len(off) != len(requests) {
		t.Fatalf("the backend received %d and %d requests, want %d each", len(on), len(off), len(requests))
	}
	for i := range requests {
		if string(on[i].Body) != string(off[i].Body) || !maps.EqualFunc(on[i].Header, off[i].Header, slices.Equal) ||
			on[i].Path != off[i].Path {
			t.Errorf("%s: captured, the backend received\n%s %v\nnot captured\n%s %v", requests[i].path, on[i].Body,
				on[i].Header, off[i].Body, off[i].Header)
		}
	}
}

// faultingExporter panics with marker in the first build that reaches it — the
// builder's goroutine — and serves the builds after it.
type faultingExporter struct {
	capture.Exporter
	marker  string
	faulted bool
}

func (e *faultingExporter) MaxSpanBytes() int {
	if !e.faulted {
		e.faulted = true
		panic(e.marker)
	}
	return e.Exporter.MaxSpanBytes()
}

// A fault in building a capture never reaches serving: the request is answered, the
// next captured request's span is built and exported, and the faulted capture is
// dropped — counted as a failed build, its bytes given back — with one error line
// naming the request and the builder's stack, never the fault's value.
func TestCaptureBuildFaultNeverReachesServing(t *testing.T) {
	const marker = "PANIC-MARKER-5eed"
	g := newCapturingGateway(t, testOptions{spanExporter: func(e capture.Exporter) capture.Exporter {
		return &faultingExporter{Exporter: e, marker: marker}
	}})
	for _, id := range []string{"faulty", "fine"} {
		w := do(t, g.h, call{method: "POST", path: "/v1/chat/completions", key: workloadKey,
			body: `{"model":"open","messages":[]}`, header: map[string]string{"X-Request-Id": id}})
		if w.Code != http.StatusOK {
			t.Fatalf("%s: status %d, want 200: %s", id, w.Code, w.Body.String())
		}
		expectFields(t, logFields(t, g, id), map[string]any{"kaiak.capture": "queued"})
	}
	spans := g.capturedSpans(t)
	if got := slices.Sorted(maps.Keys(spans)); !slices.Equal(got, []string{"fine"}) {
		t.Errorf("spans for %v, want fine's alone", got)
	}
	if held, drops := g.captures.InUse(), g.captures.Drops(); held != 0 || drops != (capture.Drops{Failed: 1}) {
		t.Errorf("captures hold %d bytes with drops %+v; want 0 and one failed build", held, drops)
	}
	var failed []string
	for line := range strings.SplitSeq(g.logText(), "\n") {
		if strings.Contains(line, `"msg":"capture build failed"`) {
			failed = append(failed, line)
		}
	}
	if len(failed) != 1 || !strings.Contains(failed[0], `"level":"ERROR"`) ||
		!strings.Contains(failed[0], `"kaiak.request.id":"faulty"`) || !strings.Contains(failed[0], `"exception.stacktrace":"goroutine `) {
		t.Errorf("failure lines %q, want one error line naming faulty with a stack", failed)
	}
	if strings.Contains(g.logText(), marker) {
		t.Errorf("the log carries the fault's value:\n%s", g.logText())
	}
}
