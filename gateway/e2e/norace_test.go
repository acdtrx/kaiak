//go:build !race

package e2e

// raceEnabled: the test binary runs without the race detector, so the kaiak binary
// it drives is built without it too.
const raceEnabled = false
