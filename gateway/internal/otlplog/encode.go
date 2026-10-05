package otlplog

import (
	"encoding/json"
	"log/slog"
	"math"
	"strconv"
	"time"
)

// The OTLP/HTTP JSON encoding of ExportLogsServiceRequest (opentelemetry-proto:
// collector/logs/v1, logs/v1, common/v1, resource/v1; docs/specification.md, JSON
// Protobuf Encoding): lowerCamelCase keys, 64-bit integers as decimal strings,
// enums as integers. Only the fields the gateway writes are here.

type exportRequest struct {
	ResourceLogs []resourceLogs `json:"resourceLogs"`
}

type resourceLogs struct {
	Resource  resource    `json:"resource"`
	ScopeLogs []scopeLogs `json:"scopeLogs"`
}

type resource struct {
	Attributes []keyValue `json:"attributes"`
}

type scopeLogs struct {
	Scope      scope       `json:"scope"`
	LogRecords []logRecord `json:"logRecords"`
}

type scope struct {
	Name string `json:"name"`
}

type logRecord struct {
	TimeUnixNano         string     `json:"timeUnixNano,omitempty"`
	ObservedTimeUnixNano string     `json:"observedTimeUnixNano,omitempty"`
	SeverityNumber       int        `json:"severityNumber"`
	SeverityText         string     `json:"severityText"`
	Body                 anyValue   `json:"body"`
	Attributes           []keyValue `json:"attributes,omitempty"`
}

type keyValue struct {
	Key   string   `json:"key"`
	Value anyValue `json:"value"`
}

// anyValue is AnyValue's one-of: exactly one field is set.
type anyValue struct {
	StringValue *string     `json:"stringValue,omitempty"`
	BoolValue   *bool       `json:"boolValue,omitempty"`
	IntValue    *string     `json:"intValue,omitempty"`
	DoubleValue *double     `json:"doubleValue,omitempty"`
	ArrayValue  *arrayValue `json:"arrayValue,omitempty"`
}

type arrayValue struct {
	Values []anyValue `json:"values"`
}

// double is a JSON double as the protobuf JSON mapping writes it: a number, or
// "NaN", "Infinity", "-Infinity", which a JSON number cannot be.
type double float64

func (d double) MarshalJSON() ([]byte, error) {
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

// scopeName is the one instrumentation scope's name.
const scopeName = "kaiak"

// encodeBatch encodes records as one ExportLogsServiceRequest under res.
func encodeBatch(res []keyValue, records []record) ([]byte, error) {
	logs := make([]logRecord, len(records))
	for i, r := range records {
		logs[i] = encodeRecord(r)
	}
	return json.Marshal(exportRequest{ResourceLogs: []resourceLogs{{
		Resource:  resource{Attributes: res},
		ScopeLogs: []scopeLogs{{Scope: scope{Name: scopeName}, LogRecords: logs}},
	}}})
}

// encodeRecord maps a record: its time is both the time and the observed time, the
// level gives the severity, the message is the body (docs/specs/GATEWAY.md,
// Observability → OTLP log export: record mapping).
func encodeRecord(r record) logRecord {
	l := logRecord{
		SeverityNumber: severityNumber(r.level),
		SeverityText:   r.level.String(),
		Body:           stringValue(r.message),
	}
	if !r.time.IsZero() {
		l.TimeUnixNano = unixNano(r.time)
		l.ObservedTimeUnixNano = l.TimeUnixNano
	}
	if len(r.attrs) > 0 {
		l.Attributes = make([]keyValue, len(r.attrs))
		for i, a := range r.attrs {
			l.Attributes[i] = keyValue{Key: a.key, Value: encodeValue(a.value)}
		}
	}
	return l
}

// severityNumber maps an slog level to OTLP's SeverityNumber: DEBUG 5, INFO 9,
// WARN 13, ERROR 17, the levels between them to the numbers between, clamped to
// TRACE (1) and FATAL4 (24).
func severityNumber(l slog.Level) int {
	return min(max(int(l)+9, 1), 24)
}

func unixNano(t time.Time) string {
	return strconv.FormatInt(t.UnixNano(), 10)
}

func encodeValue(v value) anyValue {
	switch v.kind {
	case kindInt:
		s := strconv.FormatInt(v.integer, 10)
		return anyValue{IntValue: &s}
	case kindDouble:
		d := double(v.double)
		return anyValue{DoubleValue: &d}
	case kindBool:
		b := v.boolean
		return anyValue{BoolValue: &b}
	case kindStrings:
		values := make([]anyValue, len(v.strs))
		for i, s := range v.strs {
			values[i] = stringValue(s)
		}
		return anyValue{ArrayValue: &arrayValue{Values: values}}
	}
	return stringValue(v.str)
}

func stringValue(s string) anyValue {
	return anyValue{StringValue: &s}
}

// resourceAttributes builds the resource: service.name, service.version and
// service.instance.id, then the other OTEL_RESOURCE_ATTRIBUTES in their order.
func resourceAttributes(s *Settings, res Resource) []keyValue {
	kvs := []keyValue{
		{Key: "service.name", Value: stringValue(s.serviceName)},
		{Key: "service.version", Value: stringValue(res.ServiceVersion)},
		{Key: "service.instance.id", Value: stringValue(res.InstanceID)},
	}
	for _, a := range s.resource {
		kvs = append(kvs, keyValue{Key: a.key, Value: stringValue(a.value)})
	}
	return kvs
}
