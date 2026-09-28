// Package clip bounds strings a client controls (a request path, a method, a model
// name) before they are written to a log line or echoed in an error message, so a
// client cannot make the gateway write megabytes per request (docs/specs/GATEWAY.md,
// Observability: logs).
package clip

import "unicode/utf8"

// Max is the most bytes of a client's string kept.
const Max = 256

// Marker ends a clipped string, so a reader knows it was cut.
const Marker = "…"

// String returns s when it is at most Max bytes, else its first Max bytes — cut back
// to the start of a UTF-8 character, so the result stays valid text — followed by
// Marker.
func String(s string) string {
	if len(s) <= Max {
		return s
	}
	cut := Max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + Marker
}
