package control

import (
	"encoding/json"
	"time"

	"kaiak/internal/accounting"
	"kaiak/internal/config"
)

// The messages, following protocol/schema/. Collections a message requires (Windows,
// Records, Backends, Deployments, Models, Codes) must be non-nil when encoded: nil encodes as null, which
// the schemas refuse.

// ConfigEvent is the data of a config event: the control plane's current config.
// Config is the config document as sent; config.Parse validates it, and a config it
// rejects is a config rejection (reported in status), not a malformed message.
type ConfigEvent struct {
	// ConfigHash identifies the config's content (the SHA-256 of Config as sent): the
	// gateway skips a config it already runs or last rejected, and reports which it
	// applied or rejected. It carries no order.
	ConfigHash string          `json:"config_hash"`
	Config     json.RawMessage `json:"config"`
}

// Totals is the data of a totals event: the control plane's usage in its current
// window, for every scope and type with usage, whatever the config, as one gateway
// gets it. The first totals on a stream list every such window (a scope not listed has
// used nothing in the control plane's current window); each later one lists only the
// windows that changed since the stream's last totals (a scope not listed keeps its
// value). Each message is a consistent snapshot: its windows hold every batch the
// control plane had counted when it read them, the CountedThrough batches among them,
// and no other.
type Totals struct {
	// LiveGateways is the live-gateway count; per-minute shares divide by it (at
	// least 1).
	LiveGateways int64 `json:"live_gateways"`
	// CountedThrough is, for the stream's instance, the last batch the control plane
	// has counted in each epoch it still keeps a cursor for, one per epoch; empty
	// before its first.
	CountedThrough []BatchPosition `json:"counted_through"`
	Windows        []TotalsWindow  `json:"windows"`
}

// BatchPosition is a batch within its instance's epochs.
type BatchPosition struct {
	Epoch    string `json:"epoch"`
	Sequence int64  `json:"sequence"`
}

// TotalsWindow is one scope's current window for one type: its group (or global) and
// type, whether or not the scope has a limit of that type.
type TotalsWindow struct {
	// Group is the group the limit belongs to; "" for a global limit.
	Group string           `json:"group,omitempty"`
	Type  config.LimitType `json:"type"`
	// WindowStart is the top of the hour (tokens_per_hour) or the first of the month
	// (usd_per_month), UTC, by the control plane's clock.
	WindowStart time.Time `json:"window_start"`
	// Used counts tokens (tokens_in + tokens_cache_write + tokens_out; cache reads do
	// not count) or nano-USD. It travels as a string of digits: a JavaScript number is exact only
	// up to 2^53.
	Used int64 `json:"used,string"`
}

// TotalsUpdate is what one totals event gives the totals consumer. Both parts come in
// one call, so the consumer can adopt the totals and stop counting what they include
// in one step.
type TotalsUpdate struct {
	Totals Totals
	// Complete: the stream's first totals since it connected, listing every window
	// with usage; the others list only the windows that changed.
	Complete bool
	// Counted is the newest usage generation (Client.Record) of the batches held that
	// the message's counted_through covers — each at or below its epoch's entry: the
	// totals include them, since every message is a consistent snapshot. 0 when the
	// message covers none.
	Counted uint64
}

// BatchID identifies a usage batch; the control plane counts each one once.
type BatchID struct {
	Instance string `json:"instance"`
	// Epoch is random (128 bits, hex), created with every gateway process.
	Epoch string `json:"epoch"`
	// Sequence increases by one per batch within the epoch, from 1.
	Sequence int64 `json:"sequence"`
}

// UsageBatch is the body of POST /v1/usage: 1 to 500 records of the batch's instance.
type UsageBatch struct {
	Batch   BatchID                  `json:"batch"`
	Records []accounting.UsageRecord `json:"records"`
}

// UsageAck is the answer to POST /v1/usage: the batch is counted, now or before, and
// is not sent again. It carries no totals: they come on the stream.
type UsageAck struct {
	Batch BatchID `json:"batch"`
}

// State is the gateway's serving state.
type State string

const (
	StateReady    State = "ready"
	StateDraining State = "draining"
)

// Rejection is a config the gateway rejected, by its hash, and its issue codes.
type Rejection struct {
	ConfigHash string   `json:"config_hash"`
	Codes      []string `json:"codes"`
}

// Status is the body of POST /v1/status.
type Status struct {
	Instance        string    `json:"instance"`
	ProtocolVersion int       `json:"protocol_version"`
	State           State     `json:"state"`
	StartedAt       time.Time `json:"started_at"`
	// AppliedConfigHash is the hash of the config in force; nil before the first
	// config from the control plane is applied, and while the seed config is in force.
	AppliedConfigHash *string `json:"applied_config_hash"`
	// LastRejection is the latest config received from the control plane when the
	// gateway rejected it; nil once a later one is applied or the running one is
	// received again, and before any rejection.
	LastRejection *Rejection `json:"last_rejection"`
	// Backends by backend ID: every backend of the applied config, and any backend a
	// reload dropped while requests on it still run.
	Backends map[string]BackendStatus `json:"backends"`
	// Models by public model name: every model of the applied config.
	Models map[string]ModelStatus `json:"models"`
}

// BackendStatus is one backend's line in the status.
type BackendStatus struct {
	InFlight int64 `json:"in_flight"`
	// MaxInFlight is the backend's cap; 0 = no cap (left out of the message).
	MaxInFlight int64 `json:"max_in_flight,omitempty"`
	// Deployments by the model name on the backend: the applied config's deployments
	// on this backend.
	Deployments map[string]DeploymentStatus `json:"deployments"`
}

// Circuit is a deployment's circuit-breaker state.
type Circuit string

const (
	CircuitClosed Circuit = "closed"
	CircuitOpen   Circuit = "open"
	// CircuitHalfOpen: a probe succeeded since the circuit opened; the next request
	// is its trial.
	CircuitHalfOpen Circuit = "half_open"
)

// DeploymentStatus is one deployment's circuit.
type DeploymentStatus struct {
	Circuit Circuit `json:"circuit"`
	// OpenedAt is when the circuit opened, in UTC: set exactly when it is open or
	// half-open.
	OpenedAt *time.Time `json:"opened_at,omitempty"`
}

// ModelStatus is one model's line in the status.
type ModelStatus struct {
	Queued int64 `json:"queued"`
}
