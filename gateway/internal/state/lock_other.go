//go:build !unix

package state

import "os"

// lockFile is a no-op where the standard library offers no advisory file lock: the
// lock file is created, but a second gateway on the directory is not detected
// (docs/specs/GATEWAY.md, Configuration sources: data directory). Supported
// deployments run on Unix.
func lockFile(*os.File) error { return nil }
