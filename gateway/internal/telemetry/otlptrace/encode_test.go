package otlptrace

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"kaiak/internal/telemetry/fakeotlp"
	"kaiak/internal/telemetry/otlp"
)

var testStart = time.Date(2026, 10, 13, 12, 0, 0, 123456789, time.UTC)

// A span on the wire follows OTLP JSON's rules: IDs as lowercase hex, kind and the
// status code as integers, times and intValues as decimal strings, every attribute
// type the span carries, the status only with an error — and no HTML escaping.
func TestEncodeSpan(t *testing.T) {
	got, err := Encode(Span{
		TraceID: TraceID{0x4b, 0xf9, 0x2f, 0x35, 0x77, 0xb3, 0x4d, 0xa6, 0xa3, 0xce, 0x92, 0x9d, 0x0e, 0x0e, 0x47, 0x36},
		SpanID:  SpanID{0x00, 0xf0, 0x67, 0xaa, 0x0b, 0xa9, 0x02, 0xb7},
		Name:    "chat gpt-4o",
		Kind:    KindClient,
		Start:   testStart,
		End:     testStart.Add(1500 * time.Millisecond),
		Attributes: []Attribute{
			String("gen_ai.operation.name", "chat"),
			Int("gen_ai.usage.input_tokens", 1<<53+1),
			Float64("kaiak.usage.cost_usd", 0.00125),
			Bool("kaiak.usage.estimated", false),
			Strings("gen_ai.response.finish_reasons", []string{"stop", "length"}),
			Strings("kaiak.empty", []string{}),
			String("gen_ai.input.messages", `[{"role":"user","parts":[{"type":"text","content":"is 1 < 2 && 3 > 2?"}]}]`),
		},
		Status: Status{Code: StatusError},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"traceId":"4bf92f3577b34da6a3ce929d0e0e4736","spanId":"00f067aa0ba902b7","name":"chat gpt-4o","kind":3,` +
		`"startTimeUnixNano":"1791892800123456789","endTimeUnixNano":"1791892801623456789","attributes":[` +
		`{"key":"gen_ai.operation.name","value":{"stringValue":"chat"}},` +
		`{"key":"gen_ai.usage.input_tokens","value":{"intValue":"9007199254740993"}},` +
		`{"key":"kaiak.usage.cost_usd","value":{"doubleValue":0.00125}},` +
		`{"key":"kaiak.usage.estimated","value":{"boolValue":false}},` +
		`{"key":"gen_ai.response.finish_reasons","value":{"arrayValue":{"values":[{"stringValue":"stop"},{"stringValue":"length"}]}}},` +
		`{"key":"kaiak.empty","value":{"arrayValue":{"values":[]}}},` +
		`{"key":"gen_ai.input.messages","value":{"stringValue":"[{\"role\":\"user\",\"parts\":[{\"type\":\"text\",\"content\":\"is 1 < 2 && 3 > 2?\"}]}]"}}],` +
		`"status":{"code":2}}`
	if string(got) != want {
		t.Fatalf("encoded\n%s\nwant\n%s", got, want)
	}

	// The collector's own types read it, refusing any member they do not know.
	var s fakeotlp.Span
	dec := json.NewDecoder(bytes.NewReader(got))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if attrs := s.Attrs(t); attrs["gen_ai.usage.input_tokens"] != int64(1<<53+1) || attrs["kaiak.usage.estimated"] != false {
		t.Fatalf("attributes %v", attrs)
	}
}

// An encoded span sits in a slice of its own size: a queued span holds what the
// caller's budget counts, not the encoder's grown buffer.
func TestEncodedSpanHoldsItsOwnSize(t *testing.T) {
	got, err := Encode(Span{Name: "chat m", Kind: KindClient, Start: testStart, End: testStart,
		Attributes: []Attribute{String("gen_ai.input.messages", strings.Repeat("x", 1<<20))}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) < 1<<20 || cap(got) > len(got)+8<<10 {
		t.Fatalf("encoded span of %d bytes held in %d", len(got), cap(got))
	}
}

// A span that did not end in an error carries no status: unset, as the
// conventions' error rules have it; one with no attributes carries no list.
func TestEncodeSpanWithoutErrorHasNoStatus(t *testing.T) {
	got, err := Encode(Span{TraceID: TraceID{1}, SpanID: SpanID{2}, Name: "chat m", Kind: KindClient,
		Start: testStart, End: testStart})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"traceId":"01000000000000000000000000000000","spanId":"0200000000000000","name":"chat m","kind":3,` +
		`"startTimeUnixNano":"1791892800123456789","endTimeUnixNano":"1791892800123456789"}`
	if string(got) != want {
		t.Fatalf("encoded\n%s\nwant\n%s", got, want)
	}
}

// The envelope is the resource the client carries — the log export's — and one
// scope named kaiak, around the spans.
func TestEnvelope(t *testing.T) {
	s, err := otlp.ReadSettings(otlp.Traces, envOf(map[string]string{
		"OTEL_TRACES_EXPORTER":     "otlp",
		"OTEL_RESOURCE_ATTRIBUTES": "deployment.environment.name=prod",
	}))
	if err != nil {
		t.Fatal(err)
	}
	open, closing := envelope(otlp.NewClient(s, otlp.Service{Version: "1.2.3", InstanceID: "gw-1"}).Resource())
	wantOpen := `{"resourceSpans":[{"resource":{"attributes":[` +
		`{"key":"service.name","value":{"stringValue":"kaiak"}},` +
		`{"key":"service.version","value":{"stringValue":"1.2.3"}},` +
		`{"key":"service.instance.id","value":{"stringValue":"gw-1"}},` +
		`{"key":"deployment.environment.name","value":{"stringValue":"prod"}}]},` +
		`"scopeSpans":[{"scope":{"name":"kaiak"},"spans":[`
	if string(open) != wantOpen || string(closing) != `]}]}]}` {
		t.Fatalf("envelope\n%s … %s\nwant\n%s … ]}]}]}", open, closing, wantOpen)
	}
}

func TestNewIDsAreRandom(t *testing.T) {
	traces, spans := map[TraceID]bool{}, map[SpanID]bool{}
	for range 1000 {
		tid, sid := NewTraceID(), NewSpanID()
		if tid == (TraceID{}) || sid == (SpanID{}) || traces[tid] || spans[sid] {
			t.Fatalf("IDs %x, %x: want random, never zero or repeated", tid, sid)
		}
		traces[tid], spans[sid] = true, true
	}
}
