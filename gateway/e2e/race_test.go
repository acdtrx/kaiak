//go:build race

package e2e

// raceEnabled: the test binary runs under the race detector, so the kaiak binary it
// drives is built with it too.
const raceEnabled = true
