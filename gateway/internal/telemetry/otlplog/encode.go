package otlplog

import (
	"encoding/json"
	"log/slog"

	"kaiak/internal/telemetry/otlp"
)

// The OTLP/HTTP JSON encoding of ExportLogsServiceRequest (opentelemetry-proto:
// collector/logs/v1, logs/v1; docs/specification.md, JSON Protobuf Encoding), on
// the messages every signal shares (otlp): lowerCamelCase keys, 64-bit integers as
// decimal strings, enums as integers. Only the fields the gateway writes are here.

type exportRequest struct {
	ResourceLogs []resourceLogs `json:"resourceLogs"`
}

type resourceLogs struct {
	Resource  otlp.Resource `json:"resource"`
	ScopeLogs []scopeLogs   `json:"scopeLogs"`
}

type scopeLogs struct {
	Scope      otlp.InstrumentationScope `json:"scope"`
	LogRecords []logRecord               `json:"logRecords"`
}

type logRecord struct {
	TimeUnixNano         string          `json:"timeUnixNano,omitempty"`
	ObservedTimeUnixNano string          `json:"observedTimeUnixNano,omitempty"`
	SeverityNumber       int             `json:"severityNumber"`
	SeverityText         string          `json:"severityText"`
	Body                 otlp.AnyValue   `json:"body"`
	Attributes           []otlp.KeyValue `json:"attributes,omitempty"`
}

// encodeBatch encodes records as one ExportLogsServiceRequest under res.
func encodeBatch(res otlp.Resource, records []record) ([]byte, error) {
	logs := make([]logRecord, len(records))
	for i, r := range records {
		logs[i] = encodeRecord(r)
	}
	return json.Marshal(exportRequest{ResourceLogs: []resourceLogs{{
		Resource:  res,
		ScopeLogs: []scopeLogs{{Scope: otlp.InstrumentationScope{Name: otlp.ScopeName}, LogRecords: logs}},
	}}})
}

// encodeRecord maps a record: its time is both the time and the observed time, the
// level gives the severity, the message is the body (docs/specs/GATEWAY.md,
// Observability → OTLP log export: record mapping).
func encodeRecord(r record) logRecord {
	l := logRecord{
		SeverityNumber: severityNumber(r.level),
		SeverityText:   r.level.String(),
		Body:           otlp.StringValue(r.message),
	}
	if !r.time.IsZero() {
		l.TimeUnixNano = otlp.UnixNano(r.time)
		l.ObservedTimeUnixNano = l.TimeUnixNano
	}
	if len(r.attrs) > 0 {
		l.Attributes = make([]otlp.KeyValue, len(r.attrs))
		for i, a := range r.attrs {
			l.Attributes[i] = otlp.KeyValue{Key: a.key, Value: encodeValue(a.value)}
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

func encodeValue(v value) otlp.AnyValue {
	switch v.kind {
	case kindInt:
		return otlp.IntValue(v.integer)
	case kindDouble:
		return otlp.DoubleValue(v.double)
	case kindBool:
		return otlp.BoolValue(v.boolean)
	case kindStrings:
		return otlp.StringsValue(v.strs)
	}
	return otlp.StringValue(v.str)
}
