package metrics

import (
	"kaiak/internal/telemetry/metric"
	"kaiak/internal/telemetry/otlptrace"
)

// The trace export's components, as the SDK's own metrics name them, each the
// process's one instance (docs/specs/GATEWAY.md, Observability → Exporters' own
// counts): kaiak's own processor type — the capture stage, its builder and the
// export's queue, which neither of the convention's span processors describes — and
// the convention's exporter type.
const (
	spanProcessorType = "capture_span_processor"
	spanExporterType  = "otlp_http_json_span_exporter"
)

// errorTypeOther is the error.type of a capture whose build failed: the
// convention's fallback for an error with no class of its own.
const errorTypeOther = "_OTHER"

// CaptureDrops are the request captures dropped before the trace export's queue, by
// why: Memory — the capture budget could not hold one; TooLarge — its request's body
// or its span exceeds the export limit, or its request holds more array elements and
// object members than a build reads; Shutdown — handed to the builder and not built
// when the exit's bound ended; Failed — its build failed, a gateway fault
// (docs/specs/GATEWAY.md, Request capture → Capture never gets in the way of
// serving, Memory, The export limit, The request's values; Observability → OTLP
// trace export: at exit).
type CaptureDrops struct {
	Memory, TooLarge, Shutdown, Failed uint64
}

// RegisterTraceExport registers the trace export's own counts on reg, read from
// counts and drops at each collect: otel.sdk.processor.span.processed — the captures
// handed to the exporter, or dropped (memory and too_large before the queue, from
// drops; shutdown from both: not built, or still queued, when the final work at
// exit ended; _OTHER, the convention's fallback, from drops: a build that failed) —
// and otel.sdk.exporter.span.exported — the spans the collector
// accepted, or that failed, by error.type. It is registered only with OTLP trace
// export on, so the series are absent when it is off; the accepted and processor
// series are at 0 from startup when it is on, a failure's series from its first
// failure.
func RegisterTraceExport(reg *metric.Registry, counts func() otlptrace.Counts, drops func() CaptureDrops) {
	component := []string{"otel.component.type", "otel.component.name", "error.type"}
	processed := reg.ObservableCounter(metric.Definition{Name: "otel.sdk.processor.span.processed", Unit: "{span}",
		Description: "Request captures the capture stage, its builder and the OTLP trace export's queue are done with: handed to the exporter, or dropped (error.type memory: the capture budget could not hold it; too_large: its request's body or its span exceeds the export limit, or its request holds more than 250,000 array elements and object members; shutdown: not built, or still queued, when the final work at exit ended; _OTHER: its build failed, a gateway fault reported by a capture build failed line).",
		Attributes:  component})
	exported := reg.ObservableCounter(metric.Definition{Name: "otel.sdk.exporter.span.exported", Unit: "{span}",
		Description: "Spans the OTLP trace exporter sent: accepted by the collector, or failed (error.type says how).",
		Attributes:  component})
	reg.Callback(func(o *metric.Observer) {
		c, d := counts(), drops()
		processor := []string{spanProcessorType, spanProcessorType + "/0"}
		processed.Observe(o, c.Handed, append(processor, "")...)
		processed.Observe(o, d.Memory, append(processor, "memory")...)
		processed.Observe(o, d.TooLarge, append(processor, "too_large")...)
		processed.Observe(o, c.Shutdown+d.Shutdown, append(processor, "shutdown")...)
		processed.Observe(o, d.Failed, append(processor, errorTypeOther)...)
		exporter := []string{spanExporterType, spanExporterType + "/0"}
		exported.Observe(o, c.Exported, append(exporter, "")...)
		for errorType, n := range c.Failed {
			exported.Observe(o, n, append(exporter, errorType)...)
		}
	}, processed, exported)
}
