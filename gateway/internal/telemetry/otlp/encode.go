package otlp

import (
	"encoding/json"
	"math"
	"strconv"
	"time"
)

// The OTLP/HTTP JSON encoding's messages every signal shares (opentelemetry-proto:
// common/v1, resource/v1; docs/specification.md, JSON Protobuf Encoding):
// lowerCamelCase keys, 64-bit integers as decimal strings, enums as integers. Only
// the fields the gateway writes are here; each signal's exporter wraps them in its
// own export request.

// Resource is OTLP's Resource: the attributes that describe the process.
type Resource struct {
	Attributes []KeyValue `json:"attributes"`
}

// InstrumentationScope is OTLP's InstrumentationScope.
type InstrumentationScope struct {
	Name string `json:"name"`
}

// ScopeName is the one instrumentation scope's name, every signal's.
const ScopeName = "kaiak"

// KeyValue is one attribute.
type KeyValue struct {
	Key   string   `json:"key"`
	Value AnyValue `json:"value"`
}

// AnyValue is AnyValue's one-of: exactly one field is set.
type AnyValue struct {
	StringValue *string     `json:"stringValue,omitempty"`
	BoolValue   *bool       `json:"boolValue,omitempty"`
	IntValue    *string     `json:"intValue,omitempty"`
	DoubleValue *Double     `json:"doubleValue,omitempty"`
	ArrayValue  *ArrayValue `json:"arrayValue,omitempty"`
}

// ArrayValue is an AnyValue's array.
type ArrayValue struct {
	Values []AnyValue `json:"values"`
}

// Double is a JSON double as the protobuf JSON mapping writes it: a number, or
// "NaN", "Infinity", "-Infinity", which a JSON number cannot be.
type Double float64

func (d Double) MarshalJSON() ([]byte, error) {
	f := float64(d)
	switch {
	case math.IsNaN(f):
		return []byte(`"NaN"`), nil
	case math.IsInf(f, 1):
		return []byte(`"Infinity"`), nil
	case math.IsInf(f, -1):
		return []byte(`"-Infinity"`), nil
	}
	return json.Marshal(f)
}

// StringValue is s as an AnyValue.
func StringValue(s string) AnyValue {
	return AnyValue{StringValue: &s}
}

// IntValue is v as an AnyValue: a decimal string, as OTLP JSON writes 64-bit
// integers.
func IntValue(v int64) AnyValue {
	s := strconv.FormatInt(v, 10)
	return AnyValue{IntValue: &s}
}

// DoubleValue is f as an AnyValue.
func DoubleValue(f float64) AnyValue {
	d := Double(f)
	return AnyValue{DoubleValue: &d}
}

// BoolValue is b as an AnyValue.
func BoolValue(b bool) AnyValue {
	return AnyValue{BoolValue: &b}
}

// StringsValue is strs as an AnyValue: an array of strings.
func StringsValue(strs []string) AnyValue {
	values := make([]AnyValue, len(strs))
	for i, s := range strs {
		values[i] = StringValue(s)
	}
	return AnyValue{ArrayValue: &ArrayValue{Values: values}}
}

// UnixNano is t as a fixed64 timestamp field writes it: nanoseconds since the Unix
// epoch, a decimal string.
func UnixNano(t time.Time) string {
	return strconv.FormatInt(t.UnixNano(), 10)
}

// Service is what the gateway itself puts in the resource; both win over
// OTEL_RESOURCE_ATTRIBUTES.
type Service struct {
	// Version is the build version: service.version, and the User-Agent's.
	Version string
	// InstanceID is the gateway's instance ID: service.instance.id.
	InstanceID string
}

// newResource builds the resource: service.name, service.version and
// service.instance.id, then the other OTEL_RESOURCE_ATTRIBUTES in their order.
func newResource(s *Settings, svc Service) Resource {
	kvs := []KeyValue{
		{Key: "service.name", Value: StringValue(s.serviceName)},
		{Key: "service.version", Value: StringValue(svc.Version)},
		{Key: "service.instance.id", Value: StringValue(svc.InstanceID)},
	}
	for _, a := range s.resource {
		kvs = append(kvs, KeyValue{Key: a.key, Value: StringValue(a.value)})
	}
	return Resource{Attributes: kvs}
}
