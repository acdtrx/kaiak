package metrics

import (
	"time"

	"kaiak/internal/control"
)

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
			"Usage batches sent to the control plane, by the result of the send.", "result"),
		lastAck: reg.Gauge("kaiak_usage_last_ack_timestamp_seconds",
			"Unix time the control plane last acknowledged a usage batch."),
		batchesQueued: reg.Gauge("kaiak_usage_queue_batches",
			"Sealed usage batches not yet acknowledged by the control plane (in memory)."),
		recordsQueued: reg.Gauge("kaiak_usage_queue_records",
			"Usage records in the sealed batches not yet acknowledged."),
		queuedBytes: reg.Gauge("kaiak_usage_queued_bytes",
			"Encoded bytes of the unacknowledged usage records held in memory, bounded by KAIAK_USAGE_MEMORY_BYTES (every queued record)."),
		dropped: reg.Counter("kaiak_usage_dropped_records_total",
			"Usage records dropped before reaching the control plane, by the reason they were dropped.", "reason"),
	}
	for _, r := range control.BatchResults {
		d.sends.Add(0, string(r))
	}
	for _, r := range control.DropReasons {
		d.dropped.Add(0, string(r))
	}
	d.batchesQueued.Set(0)
	d.recordsQueued.Set(0)
	d.queuedBytes.Set(0)
	return d
}

// UsageBatchSent counts one batch send and its result; an acknowledgement also sets
// the last-ack time.
func (d *UsageDelivery) UsageBatchSent(result control.BatchResult, at time.Time) {
	d.sends.Inc(string(result))
	if result == control.BatchAcked {
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
func (d *UsageDelivery) UsageRecordsDropped(reason control.DropReason, records int) {
	d.dropped.Add(uint64(max(records, 0)), string(reason))
}
