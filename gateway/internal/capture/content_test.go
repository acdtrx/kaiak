package capture

import (
	"bytes"
	"encoding/json"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

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
