package metrics

import (
	"sync/atomic"
	"time"

	"kaiak/internal/control"
	"kaiak/internal/telemetry/metric"
)

// UsageDelivery holds the metrics of usage delivery to the control plane
// (control-plane mode only): batch sends by result, the queue's depth and the last
// acknowledgement. The control client reports to it.
type UsageDelivery struct {
	sends   *metric.Counter
	lastAck *metric.Gauge
	dropped *metric.Counter
	// The queue's depth: sealed batches, the records in them, and the encoded bytes
	// of the queued records.
	batches, records, bytes queueDepth
}

// queueDepth is one depth of the queue: an up-down counter moved by the change
// from the depth last reported.
type queueDepth struct {
	counter *metric.UpDownCounter
	last    atomic.Int64
}

// report moves the counter to v. The swap makes the changes add up to the last
// value reported, whatever the order of concurrent reports.
func (q *queueDepth) report(v int64) {
	q.counter.Add(v - q.last.Swap(v))
}

// NewUsageDelivery registers the usage delivery metrics on reg.
func NewUsageDelivery(reg *metric.Registry) *UsageDelivery {
	d := &UsageDelivery{
		sends: reg.Counter(metric.Definition{Name: "kaiak.usage.batch.sends", Unit: "{batch}",
			Description: "Usage batches sent to the control plane, by the result of the send.", Attributes: []string{"kaiak.usage.batch.result"}}),
		lastAck: reg.Gauge(metric.Definition{Name: "kaiak.usage.last_ack_timestamp", Unit: "s",
			Description: "Unix time the control plane last acknowledged a usage batch."}),
		dropped: reg.Counter(metric.Definition{Name: "kaiak.usage.dropped_records", Unit: "{record}",
			Description: "Usage records dropped before reaching the control plane, by the reason they were dropped.",
			Attributes:  []string{"kaiak.usage.drop_reason"}}),
	}
	d.batches.counter = reg.UpDownCounter(metric.Definition{Name: "kaiak.usage.queue.batches", Unit: "{batch}",
		Description: "Sealed usage batches not yet acknowledged by the control plane (in memory)."})
	d.records.counter = reg.UpDownCounter(metric.Definition{Name: "kaiak.usage.queue.records", Unit: "{record}",
		Description: "Usage records in the sealed batches not yet acknowledged."})
	d.bytes.counter = reg.UpDownCounter(metric.Definition{Name: "kaiak.usage.queue.size", Unit: "By",
		Description: "Encoded bytes of the unacknowledged usage records held in memory, bounded by KAIAK_USAGE_MEMORY_BYTES (every queued record)."})
	for _, r := range control.BatchResults {
		d.sends.Add(0, string(r))
	}
	for _, r := range control.DropReasons {
		d.dropped.Add(0, string(r))
	}
	d.batches.counter.Add(0)
	d.records.counter.Add(0)
	d.bytes.counter.Add(0)
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
	d.batches.report(int64(batches))
	d.records.report(int64(records))
	d.bytes.report(queuedBytes)
}

// UsageRecordsDropped counts records dropped before reaching the control plane.
func (d *UsageDelivery) UsageRecordsDropped(reason control.DropReason, records int) {
	d.dropped.Add(uint64(max(records, 0)), string(reason))
}
