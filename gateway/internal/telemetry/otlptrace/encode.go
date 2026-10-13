package otlptrace

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"

	"kaiak/internal/telemetry/otlp"
)

// The OTLP/HTTP JSON encoding of ExportTraceServiceRequest (opentelemetry-proto:
// collector/trace/v1, trace/v1; docs/specification.md, JSON Protobuf Encoding), on
// the messages every signal shares (otlp): lowerCamelCase keys, trace and span IDs
// as lowercase hex (not base64), 64-bit integers as decimal strings, enums as
// integers. Only the fields the gateway writes are here. A request is assembled
// from spans encoded one by one: the caller encodes each to learn its size, and the
// exporter puts the bytes in the request's envelope — nothing is encoded twice.

type span struct {
	TraceID           string          `json:"traceId"`
	SpanID            string          `json:"spanId"`
	Name              string          `json:"name"`
	Kind              SpanKind        `json:"kind"`
	StartTimeUnixNano string          `json:"startTimeUnixNano"`
	EndTimeUnixNano   string          `json:"endTimeUnixNano"`
	Attributes        []otlp.KeyValue `json:"attributes,omitempty"`
	Status            *status         `json:"status,omitempty"`
}

type status struct {
	Code    StatusCode `json:"code"`
	Message string     `json:"message,omitempty"`
}

// Encode is s as an OTLP/HTTP JSON Span: the bytes Enqueue takes, whose length the
// caller holds in its budget and checks against MaxSpanBytes. The status is written
// only when it is set (docs/specs/GATEWAY.md, Observability → OTLP trace export:
// span mapping).
func Encode(s Span) ([]byte, error) {
	out := span{
		TraceID:           hex.EncodeToString(s.TraceID[:]),
		SpanID:            hex.EncodeToString(s.SpanID[:]),
		Name:              s.Name,
		Kind:              s.Kind,
		StartTimeUnixNano: otlp.UnixNano(s.Start),
		EndTimeUnixNano:   otlp.UnixNano(s.End),
		Attributes:        s.Attributes,
	}
	if s.Status.Code != StatusUnset {
		out.Status = &status{Code: s.Status.Code, Message: s.Status.Message}
	}
	return marshal(out)
}

// marshal is v's JSON without HTML escaping: a span's content is the bulk of its
// bytes, and writing each <, > and & as a six-byte escape would only grow it for a
// reader that takes JSON, not HTML.
func marshal(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	// The value without the newline Encode ends it with, in a slice of its own size:
	// the buffer grew past it, and a queued span holds what its budget counts.
	return bytes.Clone(bytes.TrimSuffix(b.Bytes(), []byte{'\n'})), nil
}

// envelope is an ExportTraceServiceRequest's JSON before and after its spans, which
// go between them joined by commas: one resource, res, with one scope.
func envelope(res otlp.Resource) (open, closing []byte) {
	open = slices.Concat([]byte(`{"resourceSpans":[{"resource":`), mustMarshal(res),
		[]byte(`,"scopeSpans":[{"scope":`), mustMarshal(otlp.InstrumentationScope{Name: otlp.ScopeName}),
		[]byte(`,"spans":[`))
	return open, []byte(`]}]}]}`)
}

// mustMarshal is the JSON of v, a value of strings alone, whose encoding cannot
// fail.
func mustMarshal(v any) []byte {
	b, err := marshal(v)
	if err != nil {
		panic(fmt.Sprintf("otlptrace: encode %T: %v", v, err))
	}
	return b
}
