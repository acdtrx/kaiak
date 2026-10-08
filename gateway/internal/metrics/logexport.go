package metrics

import "kaiak/internal/telemetry/metric"

// RegisterLogExport registers kaiak_log_export_records_total on reg, read from counts
// at each collect: the log records OTLP export handled since the process started, by
// what became of them (docs/specs/GATEWAY.md, Observability → OTLP log export) —
// accepted by the collector, in a batch given up, or never sent. It is registered only
// with OTLP log export on, so the series are absent when it is off and at 0 from
// startup when it is on.
func RegisterLogExport(reg *metric.Registry, counts func() (exported, failed, dropped uint64)) {
	records := reg.ObservableCounter(metric.Definition{Name: "kaiak.log_export.records", Unit: "{log_record}",
		Description: "Log records exported over OTLP, by outcome (exported: accepted by the collector; failed: in a batch given up; dropped: never sent, a full queue or still queued at exit).",
		Attributes:  []string{"outcome"}})
	reg.Callback(func(o *metric.Observer) {
		exported, failed, dropped := counts()
		records.Observe(o, exported, "exported")
		records.Observe(o, failed, "failed")
		records.Observe(o, dropped, "dropped")
	}, records)
}
