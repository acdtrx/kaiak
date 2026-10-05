// Package logattr writes log attribute values in the units the gateway's log
// vocabulary fixes (docs/specs/GATEWAY.md, Observability: Logs → Units): a duration
// is seconds as a double, never a Go duration string or milliseconds.
package logattr

import (
	"log/slog"
	"time"
)

// Seconds is a duration attribute in seconds to the millisecond — every
// operational event's.
func Seconds(key string, d time.Duration) slog.Attr {
	return slog.Float64(key, float64(d.Milliseconds())/1e3)
}

// SecondsMicro is a duration attribute in seconds to the microsecond — the request
// line's, and a config load's.
func SecondsMicro(key string, d time.Duration) slog.Attr {
	return slog.Float64(key, float64(d.Microseconds())/1e6)
}
