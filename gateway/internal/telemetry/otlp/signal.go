package otlp

import "fmt"

// Signal is one OpenTelemetry signal: what its variables, its endpoint's path and
// its export response are named after.
type Signal string

// The signals the gateway exports.
const (
	Logs    Signal = "logs"
	Metrics Signal = "metrics"
)

// signalInfo is what tells one signal's export apart from another's.
type signalInfo struct {
	variables signalVariables
	// path is joined to the general endpoint's path.
	path string
	// response is the export response's message name, rejected its partial-success
	// count's member, and items what that member counts — the words of an outcome.
	response, rejected, items string
}

// signalVariables are the signal's own OTEL_* variables.
type signalVariables struct {
	exporter, endpoint, headers, timeout, protocol string
	// periodic is nil for a signal not exported on an interval.
	periodic *periodicVariables
}

// periodicVariables are the variables of a signal exported on an interval: the
// time between exports, the bound on one export, and the temporality preference.
type periodicVariables struct {
	interval, timeout, temporality string
}

var signals = map[Signal]signalInfo{
	Logs: {
		variables: signalVariables{
			exporter: "OTEL_LOGS_EXPORTER",
			endpoint: "OTEL_EXPORTER_OTLP_LOGS_ENDPOINT",
			headers:  "OTEL_EXPORTER_OTLP_LOGS_HEADERS",
			timeout:  "OTEL_EXPORTER_OTLP_LOGS_TIMEOUT",
			protocol: "OTEL_EXPORTER_OTLP_LOGS_PROTOCOL",
		},
		path:     "v1/logs",
		response: "ExportLogsServiceResponse",
		rejected: "rejectedLogRecords",
		items:    "records",
	},
	Metrics: {
		variables: signalVariables{
			exporter: "OTEL_METRICS_EXPORTER",
			endpoint: "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT",
			headers:  "OTEL_EXPORTER_OTLP_METRICS_HEADERS",
			timeout:  "OTEL_EXPORTER_OTLP_METRICS_TIMEOUT",
			protocol: "OTEL_EXPORTER_OTLP_METRICS_PROTOCOL",
			periodic: &periodicVariables{
				interval:    "OTEL_METRIC_EXPORT_INTERVAL",
				timeout:     "OTEL_METRIC_EXPORT_TIMEOUT",
				temporality: "OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE",
			},
		},
		path:     "v1/metrics",
		response: "ExportMetricsServiceResponse",
		rejected: "rejectedDataPoints",
		items:    "data points",
	},
}

// info is sig's description. A signal outside the list is a programming error, not
// a setting: it panics.
func (sig Signal) info() signalInfo {
	info, ok := signals[sig]
	if !ok {
		panic(fmt.Sprintf("otlp: unknown signal %q", string(sig)))
	}
	return info
}
