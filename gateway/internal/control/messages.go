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

// ConfigSnapshot is the answer to GET /v1/config and the data of a config event.
// Config is the config document as sent; config.Parse validates it, and a config it
// rejects is a config rejection (reported in status), not a malformed message.
type ConfigSnapshot struct {
	// ConfigEpoch is the control-plane store's epoch the version counts in: versions
	// compare only within one epoch.
	ConfigEpoch string          `json:"config_epoch"`
	Version     int64           `json:"version"`
	Config      json.RawMessage `json:"config"`
}

// Totals is the data of a totals event and part of every usage ack: the control
// plane's usage in its current window of every hour and month limit with usage, as
// one gateway gets it. A limit it does not list has used nothing in the control
// plane's current window. Each message is a consistent snapshot: its windows hold
// every batch the control plane had counted at its revision, CountedThrough among
// them, and no other.
type Totals struct {
	Revision Revision `json:"revision"`
	// ConfigEpoch and ConfigVersion identify the config the totals were computed
	// under: a gateway applies them only to that config.
	ConfigEpoch   string `json:"config_epoch"`
	ConfigVersion int64  `json:"config_version"`
	// LiveGateways is the live-gateway count; per-minute shares divide by it (at
	// least 1).
	LiveGateways int64 `json:"live_gateways"`
	// CountedThrough is the last batch the control plane has counted for the
	// recipient's instance (the stream's, or the acknowledged batch's); nil before
	// its first.
	CountedThrough *BatchPosition `json:"counted_through"`
	Windows        []TotalsWindow `json:"windows"`
}

// Revision orders totals messages within their config epoch: the store's totals
// sequence, which every control-plane process over the store shares and which grows
// with every change to the totals (Client.takeTotals).
type Revision int64

// BatchPosition is a batch within its instance's epochs.
type BatchPosition struct {
	Epoch    string `json:"epoch"`
	Sequence int64  `json:"sequence"`
}

// TotalsWindow is one limit's current window. The limit is identified as a config
// reload identifies it: its group (or global) and type.
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

// TotalsUpdate is what one totals message (a totals event, or the totals in a usage
// ack) gives the totals consumer. Both parts come in one call, so the consumer can
// adopt the totals and stop counting what they include in one step.
type TotalsUpdate struct {
	// Totals are the message's totals when they are newer than the last applied (by
	// revision); nil when they are not — the consumer keeps the totals it has.
	Totals *Totals
	// Counted is the newest usage generation (Client.Record) of the batches the
	// message shows counted: an acknowledged batch, and every queued batch at or below
	// counted_through in its epoch. The totals applied now or before include them,
	// since every message is a consistent snapshot. 0 when the message counts none.
	Counted uint64
}

// Resync is the data of a resync event: the gateway fetches the snapshot again.
type Resync struct{}

// BatchID identifies a usage batch; the control plane counts each one once.
type BatchID struct {
	Instance string `json:"instance"`
	// Epoch is random (128 bits, hex), created with a fresh spool.
	Epoch string `json:"epoch"`
	// Sequence increases by one per batch within the epoch, from 1.
	Sequence int64 `json:"sequence"`
}

// UsageBatch is the body of POST /v1/usage: 1 to 500 records of the batch's instance.
type UsageBatch struct {
	Batch   BatchID                  `json:"batch"`
	Records []accounting.UsageRecord `json:"records"`
}

// UsageAck is the answer to POST /v1/usage.
type UsageAck struct {
	Batch  BatchID `json:"batch"`
	Totals Totals  `json:"totals"`
}

// State is the gateway's serving state.
type State string

const (
	StateStarting State = "starting"
	StateReady    State = "ready"
	StateDraining State = "draining"
)

// Rejection is a config version the gateway rejected and its issue codes.
type Rejection struct {
	Version int64    `json:"version"`
	Codes   []string `json:"codes"`
}

// Status is the body of POST /v1/status.
type Status struct {
	Instance        string    `json:"instance"`
	ProtocolVersion int       `json:"protocol_version"`
	State           State     `json:"state"`
	StartedAt       time.Time `json:"started_at"`
	// AppliedConfigVersion is nil before the first config is applied, and while the
	// seed config (no version) is in force; AppliedConfigEpoch is the epoch it counts
	// in, nil exactly when it is.
	AppliedConfigVersion *int64  `json:"applied_config_version"`
	AppliedConfigEpoch   *string `json:"applied_config_epoch"`
	// LastRejection is the latest config received from the control plane when the
	// gateway rejected it; nil once a later one is applied, and before any rejection.
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
