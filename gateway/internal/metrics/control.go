package metrics

import (
	"time"

	"kaiak/internal/telemetry/metric"
)

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
	// ConfigRejected reports whether the latest config received from the control
	// plane was rejected, so the gateway runs another.
	ConfigRejected() bool
}

// RegisterControlState registers the control-plane connection gauges on reg, read
// together from s at each collect.
func RegisterControlState(reg *metric.Registry, s ControlState) {
	connected := reg.ObservableGauge(metric.Definition{Name: "kaiak.control.connected",
		Description: "1 while a config stream to the control plane is open, else 0."})
	lastContact := reg.ObservableGauge(metric.Definition{Name: "kaiak.control.last_contact_timestamp", Unit: "s",
		Description: "Unix time of the last contact with the control plane: stream bytes, heartbeats included; the process start before any."})
	totalsApplied := reg.ObservableGauge(metric.Definition{Name: "kaiak.control.totals_applied_timestamp", Unit: "s",
		Description: "Unix time the control plane's usage totals were last applied (a totals event); absent before any."})
	outage := reg.ObservableGauge(metric.Definition{Name: "kaiak.control.outage",
		Description: "1 while the control plane has been out of reach past the outage grace (money-limited models refused), else 0."})
	configRejected := reg.ObservableGauge(metric.Definition{Name: "kaiak.control.config_rejected",
		Description: "1 while the latest config received from the control plane was rejected (the status report's last_rejection is set) and an earlier config stays in force, else 0."})
	reg.Callback(func(o *metric.Observer) {
		isConnected, last := s.Contact()
		connected.Observe(o, boolValue(isConnected))
		lastContact.Observe(o, float64(last.UnixMilli())/1000)
		if at, ok := s.TotalsAppliedAt(); ok {
			totalsApplied.Observe(o, float64(at.UnixMilli())/1000)
		}
		outage.Observe(o, boolValue(s.Outage()))
		configRejected.Observe(o, boolValue(s.ConfigRejected()))
	}, connected, lastContact, totalsApplied, outage, configRejected)
}

func boolValue(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
