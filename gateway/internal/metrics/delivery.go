package metrics

import (
	"slices"
	"time"
)

// Batch send results (docs/specs/GATEWAY.md, Observability): the control plane
// acknowledged the batch, refused it (dropped), or the send failed and is retried.
var batchResults = []string{"acked", "rejected", "failed"}

// dropReasons are why usage records are dropped before reaching the control plane: a
// record failing the protocol's checks, dropped alone; the oldest queued records,
// past the in-memory bound.
var dropReasons = []string{"invalid", "memory_bound"}

// UsageDelivery holds the metrics of usage delivery to the control plane
// (control-plane mode only): batch sends by result, the queue's depth and the last
// acknowledgement. The control client reports to it.
type UsageDelivery struct {
	sends         *CounterVec
	lastAck       *GaugeVec
	batchesQueued *GaugeVec
	recordsQueued *GaugeVec
	queuedBytes   *GaugeVec
	dropped       *CounterVec
}

// NewUsageDelivery registers the usage delivery metrics on reg.
func NewUsageDelivery(reg *Registry) *UsageDelivery {
	d := &UsageDelivery{
		sends: reg.Counter("kaiak_usage_batch_sends_total",
			"Usage batches sent to the control plane, by result (acked, rejected, failed: retried).", "result"),
		lastAck: reg.Gauge("kaiak_usage_last_ack_timestamp_seconds",
			"Unix time the control plane last acknowledged a usage batch."),
		batchesQueued: reg.Gauge("kaiak_usage_queue_batches",
			"Sealed usage batches not yet acknowledged by the control plane (in memory)."),
		recordsQueued: reg.Gauge("kaiak_usage_queue_records",
			"Usage records in the sealed batches not yet acknowledged."),
		queuedBytes: reg.Gauge("kaiak_usage_queued_bytes",
			"Encoded bytes of the unacknowledged usage records held in memory, bounded by KAIAK_USAGE_MEMORY_BYTES (every queued record)."),
		dropped: reg.Counter("kaiak_usage_dropped_records_total",
			"Usage records dropped before reaching the control plane, by reason (invalid: failed the record checks, dropped alone; memory_bound: over the in-memory bound).", "reason"),
	}
	for _, r := range batchResults {
		d.sends.Add(0, r)
	}
	for _, r := range dropReasons {
		d.dropped.Add(0, r)
	}
	d.batchesQueued.Set(0)
	d.recordsQueued.Set(0)
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

// UsageQueueDepth sets the queue's depth: sealed batches not yet acknowledged, the
// records in them, and the encoded bytes of the queued ones.
func (d *UsageDelivery) UsageQueueDepth(batches, records int, queuedBytes int64) {
	d.batchesQueued.Set(float64(batches))
	d.recordsQueued.Set(float64(records))
	d.queuedBytes.Set(float64(queuedBytes))
}

// UsageRecordsDropped counts records dropped before reaching the control plane.
func (d *UsageDelivery) UsageRecordsDropped(reason string, records int) {
	if !slices.Contains(dropReasons, reason) {
		panic("metrics: unknown drop reason " + reason)
	}
	d.dropped.Add(uint64(max(records, 0)), reason)
}
