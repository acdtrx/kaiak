package metrics

import (
	"runtime"

	"kaiak/internal/telemetry/metric"
)

// RegisterBuildInfo registers kaiak.build.info on reg: service.version, the build's
// version as main reports it, and process.runtime.version, the Go version.
func RegisterBuildInfo(reg *metric.Registry, version string) {
	reg.Gauge(metric.Definition{Name: "kaiak.build.info", Description: "Build information; the value is always 1.",
		Attributes: []string{"service.version", "process.runtime.version"}}).Set(1, version, runtime.Version())
}
