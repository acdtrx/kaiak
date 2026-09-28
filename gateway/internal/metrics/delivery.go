package metrics

import (
	"slices"
	"time"
)

// Batch send results (docs/specs/GATEWAY.md, Observability): the control plane
// acknowledged the batch, refused it (set aside), or the send failed and is retried.
var batchResults = []string{"acked", "rejected", "failed"}

// dropReasons are why usage records are dropped before reaching the control plane: a
// record failing the protocol's checks, set aside alone; the oldest sealed records
// while the spool cannot be written, past the in-memory bound; the oldest queued
// records with no data directory, past the same bound.
var dropReasons = []string{"invalid", "spool_unwritable", "memory_bound"}

// UsageDelivery holds the metrics of usage delivery to the control plane
// (control-plane mode only): batch sends by result, the spool's depth and the last
// acknowledgement. The control client reports to it.
type UsageDelivery struct {
	sends        *CounterVec
	lastAck      *GaugeVec
	spoolBatches *GaugeVec
	spoolRecords *GaugeVec
	queuedBytes  *GaugeVec
	dropped      *CounterVec
}

// NewUsageDelivery registers the usage delivery metrics on reg.
func NewUsageDelivery(reg *Registry) *UsageDelivery {
	d := &UsageDelivery{
		sends: reg.Counter("kaiak_usage_batch_sends_total",
			"Usage batches sent to the control plane, by result (acked, rejected, failed: retried).", "result"),
		lastAck: reg.Gauge("kaiak_usage_last_ack_timestamp_seconds",
			"Unix time the control plane last acknowledged a usage batch."),
		spoolBatches: reg.Gauge("kaiak_usage_spool_batches",
			"Sealed usage batches not yet acknowledged by the control plane."),
		spoolRecords: reg.Gauge("kaiak_usage_spool_records",
			"Usage records in the sealed batches not yet acknowledged."),
		queuedBytes: reg.Gauge("kaiak_usage_queued_bytes",
			"Encoded bytes of the unacknowledged usage records held in memory, bounded by KAIAK_USAGE_MEMORY_BYTES (every queued record with no data directory; with one, the sealed records the spool could not write)."),
		dropped: reg.Counter("kaiak_usage_dropped_records_total",
			"Usage records dropped before reaching the control plane, by reason (invalid: set aside alone; spool_unwritable: over the in-memory bound while the spool cannot be written; memory_bound: over the in-memory bound with no data directory).", "reason"),
	}
	for _, r := range batchResults {
		d.sends.Add(0, r)
	}
	for _, r := range dropReasons {
		d.dropped.Add(0, r)
	}
	d.spoolBatches.Set(0)
	d.spoolRecords.Set(0)
	d.queuedBytes.Set(0)
	return d
}

// UsageBatchSent counts one batch send and its result; an acknowledgement also sets
// the last-ack time.
func (d *UsageDelivery) UsageBatchSent(result string, at time.Time) {
	if !slices.Contains(batchResults, result) {
		panic("metrics: unknown batch result " + result)
	}
	d.sends.Inc(result)
	if result == "acked" {
		d.lastAck.Set(float64(at.UnixMilli()) / 1000)
	}
}

// UsageSpoolDepth sets the spool's depth: sealed batches not yet acknowledged, the
// records in them, and the encoded bytes of those held in memory.
func (d *UsageDelivery) UsageSpoolDepth(batches, records int, memoryBytes int64) {
	d.spoolBatches.Set(float64(batches))
	d.spoolRecords.Set(float64(records))
	d.queuedBytes.Set(float64(memoryBytes))
}

// UsageRecordsDropped counts records dropped before reaching the control plane.
func (d *UsageDelivery) UsageRecordsDropped(reason string, records int) {
	if !slices.Contains(dropReasons, reason) {
		panic("metrics: unknown drop reason " + reason)
	}
	d.dropped.Add(uint64(max(records, 0)), reason)
}
