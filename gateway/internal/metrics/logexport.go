package metrics

import (
	"kaiak/internal/telemetry/metric"
	"kaiak/internal/telemetry/otlplog"
)

// The log export's components, as the SDK's own metrics name them: the convention's
// well-known types, each the process's one instance (docs/specs/GATEWAY.md,
// Observability → Exporters' own counts).
const (
	logProcessorType = "batching_log_processor"
	logExporterType  = "otlp_http_json_log_exporter"
)

// RegisterLogExport registers the log export's own counts on reg, read from counts at
// each collect: otel.sdk.processor.log.processed — the records its queue handed to
// the exporter, or dropped (queue_full, shutdown) — and otel.sdk.exporter.log.exported
// — the records the collector accepted, or that failed, by error.type. It is
// registered only with OTLP log export on, so the series are absent when it is off;
// the accepted and queue series are at 0 from startup when it is on, a failure's
// series from its first failure.
func RegisterLogExport(reg *metric.Registry, counts func() otlplog.Counts) {
	component := []string{"otel.component.type", "otel.component.name", "error.type"}
	processed := reg.ObservableCounter(metric.Definition{Name: "otel.sdk.processor.log.processed", Unit: "{log_record}",
		Description: "Log records the OTLP export's queue is done with: handed to the exporter, or dropped (error.type queue_full: refused by the full queue; shutdown: still queued when the final flush ended).",
		Attributes:  component})
	exported := reg.ObservableCounter(metric.Definition{Name: "otel.sdk.exporter.log.exported", Unit: "{log_record}",
		Description: "Log records the OTLP exporter sent: accepted by the collector, or failed (error.type says how).",
		Attributes:  component})
	reg.Callback(func(o *metric.Observer) {
		c := counts()
		processor := []string{logProcessorType, logProcessorType + "/0"}
		processed.Observe(o, c.Handed, append(processor, "")...)
		processed.Observe(o, c.QueueFull, append(processor, "queue_full")...)
		processed.Observe(o, c.Shutdown, append(processor, "shutdown")...)
		exporter := []string{logExporterType, logExporterType + "/0"}
		exported.Observe(o, c.Exported, append(exporter, "")...)
		for errorType, n := range c.Failed {
			exported.Observe(o, n, append(exporter, errorType)...)
		}
	}, processed, exported)
}
