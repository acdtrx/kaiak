package metrics

// LogExportCounts are the log records OTLP export handled since the process started,
// by what became of them (docs/specs/GATEWAY.md, Observability → OTLP log export):
// accepted by the collector, in a batch given up, or never sent.
type LogExportCounts struct {
	Exported, Failed, Dropped uint64
}

// RegisterLogExport registers kaiak_log_export_records_total on reg, read from counts
// at each scrape. It is registered only with OTLP log export on, so the series are
// absent when it is off and at 0 from startup when it is on.
func RegisterLogExport(reg *Registry, counts func() LogExportCounts) {
	reg.CounterFunc("kaiak_log_export_records_total",
		"Log records exported over OTLP, by outcome (exported: accepted by the collector; failed: in a batch given up; dropped: never sent, a full queue or still queued at exit).",
		[]string{"outcome"},
		func(emit func(float64, ...string)) {
			c := counts()
			emit(float64(c.Exported), "exported")
			emit(float64(c.Failed), "failed")
			emit(float64(c.Dropped), "dropped")
		})
}
