package metrics

import (
	"runtime"

	"kaiak/internal/telemetry/metric"
)

// RegisterBuildInfo registers kaiak_build_info on reg: version, the build's version as
// main reports it, and the Go version.
func RegisterBuildInfo(reg *metric.Registry, version string) {
	reg.Gauge(metric.Definition{Name: "kaiak.build.info", Description: "Build information; the value is always 1.",
		Attributes: []string{"version", "go_version"}}).Set(1, version, runtime.Version())
}
