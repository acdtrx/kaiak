package metrics

import (
	"kaiak/internal/telemetry/metric"
	"kaiak/internal/telemetry/otlpmetric"
)

// metricExporterType is the metric exporter's component type, as the SDK's own
// metrics name it: the convention's well-known value, the process's one instance
// (docs/specs/GATEWAY.md, Observability → Exporters' own counts).
const metricExporterType = "otlp_http_json_metric_exporter"

// RegisterMetricExport registers the metric export's own count on reg, the registry
// the exporter reads, read from counts at each collect:
// otel.sdk.exporter.metric_data_point.exported — the data points the collector
// accepted, or that failed, by error.type. An export's points are counted once it
// ends, so they show in the next export and the next scrape. It is registered only
// with OTLP metric export on, so the series are absent when it is off; the accepted
// series is at 0 from startup when it is on, a failure's series from its first
// failure.
func RegisterMetricExport(reg *metric.Registry, counts func() otlpmetric.Counts) {
	exported := reg.ObservableCounter(metric.Definition{Name: "otel.sdk.exporter.metric_data_point.exported", Unit: "{data_point}",
		Description: "Data points the OTLP metric exporter sent: accepted by the collector, or failed (error.type says how).",
		Attributes:  []string{"otel.component.type", "otel.component.name", "error.type"}})
	reg.Callback(func(o *metric.Observer) {
		c := counts()
		exporter := []string{metricExporterType, metricExporterType + "/0"}
		exported.Observe(o, c.Exported, append(exporter, "")...)
		for errorType, n := range c.Failed {
			exported.Observe(o, n, append(exporter, errorType)...)
		}
	}, exported)
}
