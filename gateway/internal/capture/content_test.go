package capture

import (
	"bytes"
	"encoding/json"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"kaiak/internal/provider"
	"kaiak/internal/telemetry/otlptrace"
)

// Helpers for every API's content tests: reading the attributes a mapping gives, and
// writing the inputs and expectations readably.

// events is an event stream of data, one event each, as servers write them.
func events(data ...string) string {
	var b strings.Builder
	for _, d := range data {
		b.WriteString("data: " + d + "\n\n")
	}
	return b.String()
}

// typedEvents is an event stream of data, one event each named by its data's type,
// as Messages and Responses servers write them.
func typedEvents(data ...string) string {
	var b strings.Builder
	for _, d := range data {
		var e struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal([]byte(d), &e) // a malformed event is written unnamed
		if e.Type != "" {
			b.WriteString("event: " + e.Type + "\n")
		}
		b.WriteString("data: " + d + "\n\n")
	}
	return b.String()
}

// apiContent is the content attributes of an exchange of api.
func apiContent(t *testing.T, api provider.Endpoint, request, response string, stream bool, status int) map[string]any {
	t.Helper()
	return plainAttributes(t, content(api, exchange{request: []byte(request), response: []byte(response), stream: stream,
		status: status}))
}

// contentCase is a request and its answer, whole and streamed — the same answer — and
// the content attributes both give.
type contentCase struct {
	name    string
	request string
	// answer is the answer as a JSON body; stream the same answer as an event stream.
	answer, stream string
	want           map[string]any
}

// expectCorpus checks that each case's request and answer give its attributes — the
// answer whole and streamed alike: every streamed answer assembles to its unstreamed
// twin.
func expectCorpus(t *testing.T, api provider.Endpoint, corpus []contentCase) {
	t.Helper()
	for _, c := range corpus {
		t.Run(c.name, func(t *testing.T) {
			whole := apiContent(t, api, c.request, c.answer, false, 200)
			expectAttributes(t, "whole", whole, c.want)
			streamed := apiContent(t, api, c.request, c.stream, true, 200)
			expectAttributes(t, "streamed", streamed, c.want)
			if !reflect.DeepEqual(whole, streamed) {
				t.Errorf("the stream assembles to\n%v\nits unstreamed twin gives\n%v", streamed, whole)
			}
		})
	}
}

// with is want with more attributes.
func with(want, more map[string]any) map[string]any {
	out := maps.Clone(want)
	maps.Copy(out, more)
	return out
}

// expectWellFormed checks what any input gives, however odd: every content attribute
// a non-empty JSON list — the conventions' — and one finish reason per output message
// when there are any.
func expectWellFormed(t *testing.T, attrs map[string]any) {
	t.Helper()
	for _, k := range jsonAttributes {
		v, ok := attrs[k].(string)
		if !ok {
			continue
		}
		var l []json.RawMessage
		if err := json.Unmarshal([]byte(v), &l); err != nil || len(l) == 0 {
			t.Fatalf("%s is not a JSON list (%v): %s", k, err, v)
		}
		if k == "gen_ai.output.messages" {
			if reasons, ok := attrs["gen_ai.response.finish_reasons"].([]string); ok && len(reasons) != len(l) {
				t.Fatalf("%d finish reasons for %d output messages", len(reasons), len(l))
			}
		}
	}
}

// plainAttributes is attrs by key, each value as Go holds it: a string, an int64, a
// float64, a bool or a []string. A key written twice fails the test.
func plainAttributes(t *testing.T, attrs []otlptrace.Attribute) map[string]any {
	t.Helper()
	out := map[string]any{}
	for _, a := range attrs {
		if _, dup := out[a.Key]; dup {
			t.Errorf("attribute %s written twice", a.Key)
		}
		v := a.Value
		switch {
		case v.StringValue != nil:
			out[a.Key] = *v.StringValue
		case v.IntValue != nil:
			n, err := strconv.ParseInt(*v.IntValue, 10, 64)
			if err != nil {
				t.Errorf("%s: intValue %q", a.Key, *v.IntValue)
			}
			out[a.Key] = n
		case v.DoubleValue != nil:
			out[a.Key] = float64(*v.DoubleValue)
		case v.BoolValue != nil:
			out[a.Key] = *v.BoolValue
		case v.ArrayValue != nil:
			l := []string{}
			for _, e := range v.ArrayValue.Values {
				l = append(l, *e.StringValue)
			}
			out[a.Key] = l
		}
	}
	return out
}

// compact is the JSON s without insignificant whitespace — what the content
// attributes carry — so that a test can write its expectation readably.
func compact(t *testing.T, s string) string {
	t.Helper()
	var b bytes.Buffer
	if err := json.Compact(&b, []byte(s)); err != nil {
		t.Fatalf("expected JSON %s: %v", s, err)
	}
	return b.String()
}

// The content attributes, holding JSON strings.
var jsonAttributes = []string{"gen_ai.system_instructions", "gen_ai.input.messages", "gen_ai.tool.definitions",
	"gen_ai.output.messages"}

// expectAttributes checks that got holds exactly want: the same keys, each the same
// value — a JSON attribute's expectation compacted first.
func expectAttributes(t *testing.T, label string, got, want map[string]any) {
	t.Helper()
	for k, w := range want {
		if s, ok := w.(string); ok && slices.Contains(jsonAttributes, k) {
			w = compact(t, s)
		}
		if g, ok := got[k]; !ok || !reflect.DeepEqual(g, w) {
			t.Errorf("%s: %s =\n %#v\nwant\n %#v", label, k, g, w)
		}
	}
	for k, g := range got {
		if _, ok := want[k]; !ok {
			t.Errorf("%s: %s = %#v, want it absent", label, k, g)
		}
	}
}

// expectSpecExample builds a capture of api — request, and answer as a JSON body —
// through Captures into a span and checks its name (chat model), the attributes the
// spec's example shows, exactly, and that it has those, the answer's ID and model and
// the summary's attributes, nothing more.
func expectSpecExample(t *testing.T, api provider.Endpoint, request, answer, model string,
	shown map[string]map[string]any, id string) {
	t.Helper()
	exp := newFakeExporter()
	cs := newCaptures(t, 1<<20, exp)
	c := cs.Start(api, []byte(request), nil)
	c.Append([]byte(answer))
	s := summary("r-example")
	s.Model = model
	c.Hand(s)
	finish(t, cs)
	spans := exp.exportAll()
	if len(spans) != 1 {
		t.Fatalf("%d spans, want 1", len(spans))
	}
	name, attrs, _ := decoded(t, spans[0])
	if name != "chat "+model {
		t.Errorf("name %q, want chat %s", name, model)
	}
	for k, want := range shown {
		if got := attrs[k]; !reflect.DeepEqual(got, want) {
			t.Errorf("%s = %v, want %v", k, got, want)
		}
	}
	if got, want := slices.Sorted(maps.Keys(attrs)), slices.Sorted(slices.Values(slices.Concat(slices.Collect(maps.Keys(shown)),
		[]string{"gen_ai.response.id", "gen_ai.response.model", "gen_ai.operation.name", "gen_ai.provider.name",
			"gen_ai.request.model", "gen_ai.usage.input_tokens", "gen_ai.usage.cache_read.input_tokens",
			"gen_ai.usage.cache_write.input_tokens", "gen_ai.usage.output_tokens", "gen_ai.usage.reasoning.output_tokens",
			"server.address", "server.port", "kaiak.request.id", "kaiak.key.id", "kaiak.key.group", "kaiak.key.root_group",
			"kaiak.backend.id", "kaiak.backend.type", "kaiak.deployment.model", "kaiak.attempts", "kaiak.usage.cost_usd",
			"kaiak.usage.estimated", "kaiak.usage.partial"}))); !slices.Equal(got, want) {
		t.Errorf("attributes\n got %v\nwant %v", got, want)
	}
	if attrs["gen_ai.response.id"]["stringValue"] != id || attrs["gen_ai.response.model"]["stringValue"] != model {
		t.Errorf("response ID %v, model %v; want %s, %s", attrs["gen_ai.response.id"], attrs["gen_ai.response.model"], id, model)
	}
}
