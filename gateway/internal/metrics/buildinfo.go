package metrics

import "runtime"

// RegisterBuildInfo registers kaiak_build_info on reg: version, the build's version as
// main reports it, and the Go version.
func RegisterBuildInfo(reg *Registry, version string) {
	reg.Gauge("kaiak_build_info", "Build information; the value is always 1.", "version", "go_version").
		Set(1, version, runtime.Version())
}
