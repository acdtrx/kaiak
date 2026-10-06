package metrics

import "time"

// ControlState is the control-plane connection as the metrics read it
// (control-plane mode only).
type ControlState interface {
	// Contact reports whether a config stream is open and when the control plane was
	// last in contact.
	Contact() (connected bool, last time.Time)
	// Outage reports whether the control plane has been out of reach past the outage
	// grace, so money-limited models are refused.
	Outage() bool
	// TotalsAppliedAt is when the control plane's totals were last applied; false
	// before any.
	TotalsAppliedAt() (time.Time, bool)
}

// RegisterControlState registers the control-plane connection gauges on reg, read
// from s at each scrape.
func RegisterControlState(reg *Registry, s ControlState) {
	reg.GaugeFunc("kaiak_control_connected",
		"1 while a config stream to the control plane is open, else 0.", nil,
		func(emit func(float64, ...string)) {
			connected, _ := s.Contact()
			emit(boolValue(connected))
		})
	reg.GaugeFunc("kaiak_control_last_contact_timestamp_seconds",
		"Unix time of the last contact with the control plane: stream bytes (heartbeats included) or an ack; the process start before any.", nil,
		func(emit func(float64, ...string)) {
			_, last := s.Contact()
			emit(float64(last.UnixMilli()) / 1000)
		})
	reg.GaugeFunc("kaiak_control_totals_applied_timestamp_seconds",
		"Unix time the control plane's usage totals were last applied (a totals event); absent before any.", nil,
		func(emit func(float64, ...string)) {
			if at, ok := s.TotalsAppliedAt(); ok {
				emit(float64(at.UnixMilli()) / 1000)
			}
		})
	reg.GaugeFunc("kaiak_control_outage",
		"1 while the control plane has been out of reach past the outage grace (money-limited models refused), else 0.", nil,
		func(emit func(float64, ...string)) { emit(boolValue(s.Outage())) })
}

func boolValue(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
