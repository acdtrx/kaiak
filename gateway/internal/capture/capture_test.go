package capture

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"kaiak/internal/config"
	"kaiak/internal/provider"
	"kaiak/internal/telemetry/otlptrace"
)

// waitTimeout bounds every wait in these tests.
const waitTimeout = 5 * time.Second

// fakeExporter keeps the spans queued with it, releasing each when the test says
// its export ended. maxSpan is MaxSpanBytes; gate, when set, holds Enqueue — and so
// the build that calls it — until it is closed, after entered is sent the encoded
// span's length.
type fakeExporter struct {
	maxSpan int
	gate    chan struct{}
	entered chan int
	// releaseNow releases each span within Enqueue, as an exporter shut down does.
	releaseNow bool

	mu     sync.Mutex
	queued []queuedSpan
}

type queuedSpan struct {
	encoded []byte
	release func()
}

func (f *fakeExporter) MaxSpanBytes() int { return f.maxSpan }

func (f *fakeExporter) Enqueue(encoded []byte, release func()) {
	if f.gate != nil {
		f.entered <- len(encoded)
		<-f.gate
	}
	if f.releaseNow {
		release()
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queued = append(f.queued, queuedSpan{encoded: encoded, release: release})
}

// exportAll ends every queued span's export, releasing it, and returns the spans.
func (f *fakeExporter) exportAll() [][]byte {
	f.mu.Lock()
	queued := f.queued
	f.queued = nil
	f.mu.Unlock()
	var out [][]byte
	for _, q := range queued {
		out = append(out, q.encoded)
		q.release()
	}
	return out
}

func newFakeExporter() *fakeExporter { return &fakeExporter{maxSpan: 1 << 20} }

// newCaptures starts captures of memory bytes over exp, finished when the test ends;
// their log is discarded.
func newCaptures(t *testing.T, memory int64, exp Exporter) *Captures {
	t.Helper()
	return startCaptures(t, memory, exp, slog.New(slog.DiscardHandler))
}

// startCaptures starts captures of memory bytes over exp, logging to logger,
// finished when the test ends.
func startCaptures(t *testing.T, memory int64, exp Exporter, logger *slog.Logger) *Captures {
	t.Helper()
	cs := New(memory, exp, logger)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), waitTimeout)
		defer cancel()
		cs.Finish(ctx)
	})
	return cs
}

// finish builds every capture handed, within waitTimeout.
func finish(t *testing.T, cs *Captures) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), waitTimeout)
	defer cancel()
	cs.Finish(ctx)
}

// summary is a request's summary for these tests: routed and settled.
func summary(id string) Summary {
	now := time.Now()
	return Summary{Start: now.Add(-time.Second), End: now, RequestID: id, KeyID: "k-eval", Group: "eval",
		RootGroup: "research", Model: "qwen", Status: 200,
		Backend:         &config.Backend{ID: "gpu-1", Type: config.BackendVLLM, BaseURL: "http://10.0.0.7:8000/v1"},
		DeploymentModel: "Qwen/Qwen3-32B", Attempts: 1,
		Usage: &Usage{InputTokens: 12, CacheReadTokens: 4, OutputTokens: 7, CostUSD: 0.5}}
}

// decoded is an encoded span's attributes by key, each value in its OTLP JSON form.
func decoded(t *testing.T, encoded []byte) (name string, attrs map[string]map[string]any, status map[string]any) {
	t.Helper()
	var span struct {
		Name       string `json:"name"`
		Attributes []struct {
			Key   string         `json:"key"`
			Value map[string]any `json:"value"`
		} `json:"attributes"`
		Status map[string]any `json:"status"`
	}
	if err := json.Unmarshal(encoded, &span); err != nil {
		t.Fatalf("span %s: %v", encoded, err)
	}
	attrs = map[string]map[string]any{}
	for _, a := range span.Attributes {
		attrs[a.Key] = a.Value
	}
	return span.Name, attrs, span.Status
}

// A capture holds its copies in the budget while its request runs; handed on, it is
// built in the background, its charge moved to the encoded span, which goes back to
// the budget when its export ends.
func TestCaptureIsBuiltAndQueued(t *testing.T) {
	exp := newFakeExporter()
	cs := newCaptures(t, 1<<20, exp)
	body := []byte(`{"model":"qwen","messages":[]}`)
	c := cs.Start(provider.ChatCompletions, body, nil)
	if got := cs.InUse(); got != int64(len(body)) {
		t.Errorf("%d bytes held after the copy, want the body's %d", got, len(body))
	}
	body[0] = 'X' // the capture holds a copy: the request's own bytes may go
	c.Append([]byte("data: one\n\n"))
	c.Append([]byte("data: two\n\n"))
	if got := cs.InUse(); got != int64(len(body))+responseStep {
		t.Errorf("%d bytes held with the answer, want the body and one %d-byte buffer", got, responseStep)
	}
	if got := c.Hand(summary("r-1")); got != OutcomeQueued {
		t.Errorf("Hand: %q, want queued", got)
	}
	finish(t, cs)
	spans := exp.queued
	if len(spans) != 1 {
		t.Fatalf("%d spans queued, want 1", len(spans))
	}
	if got := cs.InUse(); got != int64(len(spans[0].encoded)) {
		t.Errorf("%d bytes held once queued, want the encoded span's %d", got, len(spans[0].encoded))
	}
	name, attrs, _ := decoded(t, spans[0].encoded)
	if name != "chat qwen" || attrs["kaiak.request.id"]["stringValue"] != "r-1" {
		t.Errorf("span %q with attributes %v", name, attrs)
	}
	exp.exportAll()
	if got := cs.InUse(); got != 0 {
		t.Errorf("%d bytes held once exported, want 0", got)
	}
	if d := cs.Drops(); d != (Drops{}) {
		t.Errorf("drops %+v, want none", d)
	}
}

// A copy of the request that does not fit drops the capture at once: it holds
// nothing, ignores what the client is written, and its request line says memory.
func TestCopyThatDoesNotFitDropsTheCapture(t *testing.T) {
	exp := newFakeExporter()
	cs := newCaptures(t, 10, exp)
	c := cs.Start(provider.Messages, []byte(`{"model":"claude","messages":[]}`), nil)
	c.Append([]byte("event: message_start\n\n"))
	if got := c.Hand(summary("r-1")); got != OutcomeMemory {
		t.Errorf("Hand: %q, want memory", got)
	}
	finish(t, cs)
	if len(exp.queued) != 0 || cs.InUse() != 0 || cs.Drops() != (Drops{Memory: 1}) {
		t.Errorf("%d spans queued, %d bytes held, drops %+v; want none, 0 and one memory", len(exp.queued), cs.InUse(),
			cs.Drops())
	}
}

// An answer that outgrows the budget mid-way drops the capture when its buffer
// cannot grow: everything it held goes back at once, and it is counted once.
func TestAnswerThatOutgrowsTheBudgetDropsTheCapture(t *testing.T) {
	body := []byte(`{"model":"qwen","stream":true}`)
	exp := newFakeExporter()
	// The body and the first buffer fit; its first doubling does not.
	cs := newCaptures(t, int64(len(body))+responseStep+100, exp)
	c := cs.Start(provider.ChatCompletions, body, nil)
	event := []byte(strings.Repeat("e", 1000))
	for range 4 {
		c.Append(event)
	}
	if c.dropped != "" {
		t.Fatal("dropped while the first buffer held the answer")
	}
	c.Append(event)
	c.Append(event)
	if c.dropped != OutcomeMemory || cs.InUse() != 0 {
		t.Errorf("dropped %q with %d bytes held, want memory and 0 held", c.dropped, cs.InUse())
	}
	if got := c.Hand(summary("r-1")); got != OutcomeMemory {
		t.Errorf("Hand: %q, want memory", got)
	}
	finish(t, cs)
	if len(exp.queued) != 0 || cs.Drops() != (Drops{Memory: 1}) {
		t.Errorf("%d spans queued, drops %+v; want none and one memory", len(exp.queued), cs.Drops())
	}
}

// A request body larger than the exporter can send in a span drops the capture at
// once, before anything is copied: nothing is held, what the client is written is
// ignored, nothing is built, and it is counted — and its request line says —
// too_large. A body of the limit's own size is copied.
func TestBodyOverTheExportLimitIsNotCopied(t *testing.T) {
	body := []byte(`{"model":"qwen","messages":[{"role":"user","content":"` + strings.Repeat("x", 64) + `"}]}`)
	exp := newFakeExporter()
	exp.maxSpan = len(body) - 1
	cs := newCaptures(t, 1<<20, exp)
	cs.readContent = func(provider.Endpoint, exchange) []otlptrace.Attribute {
		t.Error("a capture whose body is over the export limit was built")
		return nil
	}
	c := cs.Start(provider.ChatCompletions, body, nil)
	c.Append([]byte("data: one\n\n"))
	if got := cs.InUse(); got != 0 {
		t.Errorf("%d bytes held, want none: nothing is copied", got)
	}
	if got := c.Hand(summary("r-1")); got != OutcomeTooLarge {
		t.Errorf("Hand: %q, want too_large", got)
	}
	finish(t, cs)
	if len(exp.queued) != 0 || cs.InUse() != 0 || cs.Drops() != (Drops{TooLarge: 1}) {
		t.Errorf("%d spans queued, %d bytes held, drops %+v; want none, 0 and one too_large", len(exp.queued), cs.InUse(),
			cs.Drops())
	}

	atLimit := newFakeExporter()
	atLimit.maxSpan = len(body)
	cs = newCaptures(t, 1<<20, atLimit)
	cs.Start(provider.ChatCompletions, body, nil)
	if got := cs.InUse(); got != int64(len(body)) {
		t.Errorf("%d bytes held for a body at the limit, want its %d", got, len(body))
	}
}

// A span the exporter cannot send is dropped as too_large; one whose encoding does
// not fit in what the budget has left, as memory. Either way its bytes go back.
func TestBuildDrops(t *testing.T) {
	t.Run("too large", func(t *testing.T) {
		exp := newFakeExporter()
		exp.maxSpan = 100
		cs := newCaptures(t, 1<<20, exp)
		cs.Start(provider.Responses, []byte(`{"model":"qwen","input":"hi"}`), nil).Hand(summary("r-1"))
		finish(t, cs)
		if len(exp.queued) != 0 || cs.InUse() != 0 || cs.Drops() != (Drops{TooLarge: 1}) {
			t.Errorf("%d spans queued, %d bytes held, drops %+v; want none, 0 and one too_large", len(exp.queued),
				cs.InUse(), cs.Drops())
		}
	})
	t.Run("encoded span over the budget", func(t *testing.T) {
		exp := newFakeExporter()
		// The copy of the request fits; the span, with every attribute, does not.
		cs := newCaptures(t, 300, exp)
		c := cs.Start(provider.ChatCompletions, []byte(`{"model":"qwen"}`), nil)
		if got := c.Hand(summary("r-1")); got != OutcomeQueued {
			t.Fatalf("Hand: %q, want queued: the copies fit", got)
		}
		finish(t, cs)
		if len(exp.queued) != 0 || cs.InUse() != 0 || cs.Drops() != (Drops{Memory: 1}) {
			t.Errorf("%d spans queued, %d bytes held, drops %+v; want none, 0 and one memory", len(exp.queued),
				cs.InUse(), cs.Drops())
		}
	})
}

// The budget is never locked while a span is queued: an exporter that releases
// within Enqueue (one shut down) gives the bytes back at once.
func TestReleaseWithinEnqueue(t *testing.T) {
	exp := newFakeExporter()
	exp.releaseNow = true
	cs := newCaptures(t, 1<<20, exp)
	cs.Start(provider.ChatCompletions, []byte(`{"model":"qwen"}`), nil).Hand(summary("r-1"))
	finish(t, cs)
	if cs.InUse() != 0 {
		t.Errorf("%d bytes held, want 0", cs.InUse())
	}
}

// Finish builds every capture handed before it; when its bound ends first, a build
// in progress completes and the captures not yet built are dropped as shutdown,
// their bytes given back. A capture handed after Finish is dropped the same way.
func TestFinish(t *testing.T) {
	t.Run("builds the backlog", func(t *testing.T) {
		exp := newFakeExporter()
		cs := newCaptures(t, 1<<20, exp)
		for _, id := range []string{"r-1", "r-2", "r-3"} {
			cs.Start(provider.ChatCompletions, []byte(`{"model":"qwen"}`), nil).Hand(summary(id))
		}
		finish(t, cs)
		var ids []any
		for _, encoded := range exp.exportAll() {
			_, attrs, _ := decoded(t, encoded)
			ids = append(ids, attrs["kaiak.request.id"]["stringValue"])
		}
		if !slices.Equal(ids, []any{"r-1", "r-2", "r-3"}) || cs.InUse() != 0 {
			t.Errorf("built %v with %d bytes held, want r-1, r-2, r-3 in order and 0", ids, cs.InUse())
		}
	})
	t.Run("bound ends with the builder blocked", func(t *testing.T) {
		exp := newFakeExporter()
		exp.gate, exp.entered = make(chan struct{}), make(chan int, 1)
		cs := newCaptures(t, 1<<20, exp)
		body := []byte(`{"model":"qwen"}`)
		cs.Start(provider.ChatCompletions, body, nil).Hand(summary("r-1"))
		var building int
		select {
		case building = <-exp.entered:
		case <-time.After(waitTimeout):
			t.Fatal("the builder never started")
		}
		cs.Start(provider.ChatCompletions, body, nil).Hand(summary("r-2"))
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		finished := make(chan struct{})
		go func() {
			cs.Finish(ctx)
			close(finished)
		}()
		<-ctx.Done()
		// Finish waits for the build in progress, and has dropped the backlog by then.
		deadline := time.After(waitTimeout)
		for cs.Drops().Shutdown != 1 {
			select {
			case <-deadline:
				t.Fatalf("drops %+v, want r-2 dropped at the bound", cs.Drops())
			case <-time.After(time.Millisecond):
			}
		}
		if got := cs.InUse(); got != int64(building) {
			t.Errorf("%d bytes held at the bound, want r-1's encoded span's %d alone", got, building)
		}
		select {
		case <-finished:
			t.Fatal("Finish returned with a build in progress")
		default:
		}
		close(exp.gate)
		<-finished
		spans := exp.exportAll()
		if len(spans) != 1 || cs.InUse() != 0 {
			t.Fatalf("%d spans built, %d bytes held; want r-1's and 0", len(spans), cs.InUse())
		}
		cs.Start(provider.ChatCompletions, body, nil).Hand(summary("r-3"))
		if d := cs.Drops(); d != (Drops{Shutdown: 2}) || cs.InUse() != 0 {
			t.Errorf("drops %+v with %d bytes held after a late hand, want two shutdown and 0", d, cs.InUse())
		}
	})
}

// The span carries every attribute but the content from the summary, under the
// convention's names and the request line's (docs/specs/GATEWAY.md, Request capture
// → Attributes); an error makes its status an error, with no message: error.type
// says which.
func TestSpanAttributes(t *testing.T) {
	str := func(s string) map[string]any { return map[string]any{"stringValue": s} }
	integer := func(s string) map[string]any { return map[string]any{"intValue": s} }
	boolean := func(b bool) map[string]any { return map[string]any{"boolValue": b} }
	double := func(f float64) map[string]any { return map[string]any{"doubleValue": f} }
	for _, tc := range []struct {
		name    string
		summary func() Summary
		session []string
		want    map[string]map[string]any
		status  map[string]any
	}{
		{name: "routed and settled", summary: func() Summary { return summary("r-1") }, session: []string{"conv-1"},
			want: map[string]map[string]any{
				"gen_ai.operation.name": str("chat"), "gen_ai.provider.name": str("vllm"),
				"gen_ai.request.model": str("qwen"), "gen_ai.usage.input_tokens": integer("12"),
				"gen_ai.usage.cache_read.input_tokens": integer("4"), "gen_ai.usage.cache_write.input_tokens": integer("0"),
				"gen_ai.usage.output_tokens": integer("7"), "gen_ai.usage.reasoning.output_tokens": integer("0"),
				"gen_ai.conversation.id": str("conv-1"), "server.address": str("10.0.0.7"), "server.port": integer("8000"),
				"kaiak.request.id": str("r-1"), "kaiak.key.id": str("k-eval"), "kaiak.key.group": str("eval"),
				"kaiak.key.root_group": str("research"), "kaiak.backend.id": str("gpu-1"), "kaiak.backend.type": str("vllm"),
				"kaiak.deployment.model": str("Qwen/Qwen3-32B"), "kaiak.attempts": integer("1"),
				"kaiak.usage.cost_usd": double(0.5), "kaiak.usage.estimated": boolean(false),
				"kaiak.usage.partial": boolean(false)}},
		{name: "refused before routing", summary: func() Summary {
			s := summary("r-2")
			s.Backend, s.DeploymentModel, s.Attempts, s.Usage, s.Status = nil, "", 0, nil, 404
			s.ErrorType, s.Model = "model_not_found", "nope"
			return s
		}, session: []string{"has space"},
			want: map[string]map[string]any{
				"gen_ai.operation.name": str("chat"), "gen_ai.request.model": str("nope"),
				"error.type": str("model_not_found"), "kaiak.request.id": str("r-2"), "kaiak.key.id": str("k-eval"),
				"kaiak.key.group": str("eval"), "kaiak.key.root_group": str("research"),
				"kaiak.session_id.ignored": str("invalid_characters")},
			status: map[string]any{"code": float64(2)}},
		{name: "a stream cut short", summary: func() Summary {
			s := summary("r-3")
			s.Stream, s.RelayEnd, s.ContentStreamed, s.TimeToFirstToken = true, "upstream_failed", true, 1234567*time.Microsecond
			s.Backend = &config.Backend{ID: "cloud", Type: config.BackendAnthropic, BaseURL: "https://api.anthropic.com"}
			s.Usage.Partial = true
			return s
		}, want: map[string]map[string]any{
			"gen_ai.operation.name": str("chat"), "gen_ai.provider.name": str("anthropic"),
			"gen_ai.request.model": str("qwen"), "gen_ai.request.stream": boolean(true),
			"gen_ai.usage.input_tokens": integer("12"), "gen_ai.usage.cache_read.input_tokens": integer("4"),
			"gen_ai.usage.cache_write.input_tokens": integer("0"), "gen_ai.usage.output_tokens": integer("7"),
			"gen_ai.usage.reasoning.output_tokens": integer("0"), "server.address": str("api.anthropic.com"),
			"server.port": integer("443"), "error.type": str("upstream_failed"), "kaiak.request.id": str("r-3"),
			"kaiak.key.id": str("k-eval"), "kaiak.key.group": str("eval"), "kaiak.key.root_group": str("research"),
			"kaiak.backend.id": str("cloud"), "kaiak.backend.type": str("anthropic"),
			"kaiak.deployment.model": str("Qwen/Qwen3-32B"), "kaiak.attempts": integer("1"),
			"kaiak.time_to_first_token": double(1.234567), "kaiak.relay_end": str("upstream_failed"),
			"kaiak.usage.cost_usd": double(0.5), "kaiak.usage.estimated": boolean(false),
			"kaiak.usage.partial": boolean(true)},
			status: map[string]any{"code": float64(2)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exp := newFakeExporter()
			cs := newCaptures(t, 1<<20, exp)
			s := tc.summary()
			cs.Start(provider.ChatCompletions, []byte(`{}`), tc.session).Hand(s)
			finish(t, cs)
			spans := exp.exportAll()
			if len(spans) != 1 {
				t.Fatalf("%d spans, want 1", len(spans))
			}
			name, attrs, status := decoded(t, spans[0])
			if want := "chat " + s.Model; name != want {
				t.Errorf("name %q, want %q", name, want)
			}
			for k, v := range tc.want {
				if got, ok := attrs[k]; !ok || !mapsEqual(got, v) {
					t.Errorf("%s = %v (present %v), want %v", k, got, ok, v)
				}
			}
			for k, v := range attrs {
				if _, ok := tc.want[k]; !ok {
					t.Errorf("%s = %v, want it absent", k, v)
				}
			}
			if !mapsEqual(status, tc.status) {
				t.Errorf("status %v, want %v", status, tc.status)
			}
		})
	}
}

func mapsEqual(a, b map[string]any) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// server.port is the base URL's port, else the scheme's.
func TestServerAddress(t *testing.T) {
	for _, tc := range []struct {
		url  string
		host string
		port int64
	}{
		{"http://vllm:8000/v1", "vllm", 8000},
		{"https://res.openai.azure.com", "res.openai.azure.com", 443},
		{"http://llama.internal/v1", "llama.internal", 80},
		{"http://[fd00::7]:9000/v1", "fd00::7", 9000},
	} {
		host, port, ok := serverAddress(tc.url)
		if !ok || host != tc.host || port != tc.port {
			t.Errorf("%s: %s %d %v, want %s %d", tc.url, host, port, ok, tc.host, tc.port)
		}
	}
}

// A session ID is 1 to 256 bytes of visible ASCII, read from the first header; any
// other value is ignored with the first reason that applies — empty, too long,
// invalid characters — and none sent is neither.
func TestSession(t *testing.T) {
	long := strings.Repeat("s", maxSessionID)
	for _, tc := range []struct {
		name    string
		values  []string
		id      string
		ignored string
	}{
		{"none sent", nil, "", ""},
		{"valid", []string{"conv-7f3a:2"}, "conv-7f3a:2", ""},
		{"every visible character", []string{"!~\"#$%&'()*+,-./09:;<=>?@AZ[\\]^_`az{|}"}, "!~\"#$%&'()*+,-./09:;<=>?@AZ[\\]^_`az{|}", ""},
		{"at the length limit", []string{long}, long, ""},
		{"empty", []string{""}, "", "empty"},
		{"too long", []string{long + "s"}, "", "too_long"},
		{"too long before invalid", []string{long + " "}, "", "too_long"},
		{"a space", []string{"two words"}, "", "invalid_characters"},
		{"a tab", []string{"a\tb"}, "", "invalid_characters"},
		{"not ASCII", []string{"café"}, "", "invalid_characters"},
		{"a control character", []string{"a\x7fb"}, "", "invalid_characters"},
		{"the first header decides", []string{"bad value", "good"}, "", "invalid_characters"},
		{"the first header is valid", []string{"good", "bad value"}, "good", ""},
	} {
		id, ignored := session(tc.values)
		if id != tc.id || ignored != tc.ignored {
			t.Errorf("%s: %q, %q; want %q, %q", tc.name, id, ignored, tc.id, tc.ignored)
		}
	}
}

// faultyExporter faults within the first build that reaches it — within Enqueue,
// once it has released the span — and serves the builds after it as fakeExporter
// does. marker is the panic's value.
type faultyExporter struct {
	*fakeExporter
	marker  string
	faulted bool
}

func (f *faultyExporter) Enqueue(encoded []byte, release func()) {
	if !f.faulted {
		f.faulted = true
		release()
		panic(f.marker)
	}
	f.fakeExporter.Enqueue(encoded, release)
}

// A fault in building one capture — a panic while its content is read, before its
// charge moves to the encoded span, or once the exporter has taken and released the
// span — drops that capture alone: its bytes go back once, it is counted as a
// failed build, and one error line names its request and the builder's stack, never
// the fault's value, which can quote what the client sent. The builder goes on: the
// next capture is built and queued.
func TestBuildFaultDropsOneCapture(t *testing.T) {
	const marker = "PANIC-MARKER-c0ffee"
	for _, at := range []string{"content", "enqueue"} {
		t.Run(at, func(t *testing.T) {
			fake := newFakeExporter()
			var exp Exporter = fake
			if at == "enqueue" {
				exp = &faultyExporter{fakeExporter: fake, marker: marker}
			}
			var logs bytes.Buffer
			cs := startCaptures(t, 1<<20, exp, slog.New(slog.NewJSONHandler(&logs, nil)))
			if at == "content" {
				cs.readContent = func(api provider.Endpoint, x exchange) []otlptrace.Attribute {
					if bytes.Contains(x.request, []byte(marker)) {
						panic(marker + " in " + string(x.request))
					}
					return content(api, x)
				}
			}
			c := cs.Start(provider.ChatCompletions, []byte(`{"model":"`+marker+`"}`), nil)
			c.Append([]byte("data: answer\n\n"))
			c.Hand(summary("r-fault"))
			cs.Start(provider.ChatCompletions, []byte(`{"model":"qwen"}`), nil).Hand(summary("r-next"))
			finish(t, cs)

			var ids []any
			for _, encoded := range fake.exportAll() {
				_, attrs, _ := decoded(t, encoded)
				ids = append(ids, attrs["kaiak.request.id"]["stringValue"])
			}
			if !slices.Equal(ids, []any{"r-next"}) {
				t.Errorf("queued %v, want r-next alone", ids)
			}
			if d := cs.Drops(); d != (Drops{Failed: 1}) || cs.InUse() != 0 {
				t.Errorf("drops %+v with %d bytes held, want one failed build and 0", d, cs.InUse())
			}
			var lines []map[string]any
			for line := range strings.Lines(logs.String()) {
				var l map[string]any
				if err := json.Unmarshal([]byte(line), &l); err != nil {
					t.Fatalf("log line %q: %v", line, err)
				}
				lines = append(lines, l)
			}
			if len(lines) != 1 || lines[0]["level"] != "ERROR" || lines[0]["msg"] != "capture build failed" ||
				lines[0]["kaiak.request.id"] != "r-fault" ||
				!strings.Contains(lines[0]["exception.stacktrace"].(string), "capture.(*Captures).build") {
				t.Errorf("log %s, want one error line naming r-fault with the builder's stack", logs.String())
			}
			if strings.Contains(logs.String(), marker) {
				t.Errorf("the log carries the fault's value: %s", logs.String())
			}
		})
	}
}

// A capture's content is read knowing whether its request failed — its span has an
// error.type: a stream cut before its content arrived gives no output message, none
// made up, and the finish reason error; the same stream of a request that did not
// fail gives its message, empty.
func TestFailedCaptureMakesUpNoOutput(t *testing.T) {
	stream := typedEvents(`{"type":"message_start","message":{"id":"msg","model":"claude","content":[]}}`)
	for _, c := range []struct {
		name, relayEnd string
		output         bool
	}{{"failed", "client_closed", false}, {"not failed", "", true}} {
		exp := newFakeExporter()
		cs := newCaptures(t, 1<<20, exp)
		capture := cs.Start(provider.Messages, []byte(`{"model":"claude","stream":true}`), nil)
		capture.Append([]byte(stream))
		s := summary("r-1")
		s.Stream, s.RelayEnd = true, c.relayEnd
		capture.Hand(s)
		finish(t, cs)
		spans := exp.exportAll()
		if len(spans) != 1 {
			t.Fatalf("%s: %d spans, want 1", c.name, len(spans))
		}
		_, attrs, _ := decoded(t, spans[0])
		if _, has := attrs["gen_ai.output.messages"]; has != c.output {
			t.Errorf("%s: output messages %v, want them %v", c.name, attrs["gen_ai.output.messages"], c.output)
		}
		if got, want := attrs["gen_ai.response.finish_reasons"], map[string]any{"arrayValue": map[string]any{
			"values": []any{map[string]any{"stringValue": "error"}}}}; !reflect.DeepEqual(got, want) {
			t.Errorf("%s: finish reasons %v, want error", c.name, got)
		}
	}
}
