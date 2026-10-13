package otlptrace

import (
	"crypto/rand"
	"time"

	"kaiak/internal/telemetry/otlp"
)

// Span is one span as the gateway makes it (docs/specs/GATEWAY.md, Request capture
// → The span): the root of a trace of its own — no parent — with no events and no
// links.
type Span struct {
	TraceID TraceID
	SpanID  SpanID
	Name    string
	Kind    SpanKind
	// Start and End are when the operation began and ended.
	Start, End time.Time
	Attributes []Attribute
	// Status is the operation's status; the zero Status is unset.
	Status Status
}

// TraceID is a trace's ID: 16 bytes.
type TraceID [16]byte

// SpanID is a span's ID: 8 bytes.
type SpanID [8]byte

// NewTraceID is a random trace ID, as W3C Trace Context asks of a trace's root.
func NewTraceID() TraceID {
	var id TraceID
	_, _ = rand.Read(id[:]) // crypto/rand.Read never returns an error
	return id
}

// NewSpanID is a random span ID.
func NewSpanID() SpanID {
	var id SpanID
	_, _ = rand.Read(id[:]) // crypto/rand.Read never returns an error
	return id
}

// SpanKind is OTLP's SpanKind, an integer on the wire.
type SpanKind int

// KindClient: the span describes a request to a remote service.
const KindClient SpanKind = 3

// StatusCode is OTLP's Status.StatusCode, an integer on the wire.
type StatusCode int

const (
	// StatusUnset is the default: the span carries no status.
	StatusUnset StatusCode = 0
	// StatusError is an operation that ended in an error.
	StatusError StatusCode = 2
)

// Status is a span's status: its code, and for an error a message.
type Status struct {
	Code    StatusCode
	Message string
}

// Attribute is one of a span's attributes, OTLP's KeyValue. String, Int, Float64,
// Bool and Strings make one of each type a span carries.
type Attribute = otlp.KeyValue

// String is a string attribute.
func String(key, value string) Attribute {
	return Attribute{Key: key, Value: otlp.StringValue(value)}
}

// Int is an integer attribute: an intValue, a decimal string on the wire.
func Int(key string, value int64) Attribute {
	return Attribute{Key: key, Value: otlp.IntValue(value)}
}

// Float64 is a floating-point attribute.
func Float64(key string, value float64) Attribute {
	return Attribute{Key: key, Value: otlp.DoubleValue(value)}
}

// Bool is a boolean attribute.
func Bool(key string, value bool) Attribute {
	return Attribute{Key: key, Value: otlp.BoolValue(value)}
}

// Strings is a string-list attribute: an arrayValue of stringValues.
func Strings(key string, values []string) Attribute {
	return Attribute{Key: key, Value: otlp.StringsValue(values)}
}
