package metrics

import (
	"kaiak/internal/accounting"
	"kaiak/internal/config"
)

// usageLabels label every usage metric: the key's group, its top-level group, key ID,
// model and status. One fixed label set whatever the tree's depth: intermediate
// levels are not labels. The key's group is key_group, not group: Prometheus scrape
// configs commonly set a target label named group, which would rename this one to
// exported_group. An empty value (key_group or key ID switched off) is absent from
// the series. No backend: it would multiply every group's series by the backends
// serving each model; the ops metrics carry the backend. A group's labels never
// become metric labels.
var usageLabels = []string{"key_group", "root_group", "key_id", "model", "status"}

// UsageSink turns settled usage records into usage metrics. It is one sink on
// accounting's fan-out: the record — settled once, by accounting — is its only input,
// and nothing reads the metrics back into a record (docs/kaiak.md, principle 7).
type UsageSink struct {
	holder  *config.Holder
	records *CounterVec
	tokens  *CounterVec
	cost    *CounterVec
	clamped *CounterVec
}

// NewUsageSink registers the usage metrics on reg. The key_id and key_group labels
// follow the live config's global.metrics.key_id_label and group_label at each record.
func NewUsageSink(reg *Registry, holder *config.Holder) *UsageSink {
	s := &UsageSink{
		holder: holder,
		records: reg.Counter("kaiak_usage_records_total",
			"Usage records settled: one per routed request, plus one per retried attempt whose request reached the backend and got no answer.", usageLabels...),
		tokens: reg.Counter("kaiak_usage_tokens_total",
			"Tokens by usage unit, as settled by accounting.", append(append([]string(nil), usageLabels...), "unit")...),
		cost: reg.ScaledCounter("kaiak_usage_cost_usd_total",
			"Estimated cost in US dollars, from the model's price table.", 1e9, usageLabels...),
		clamped: reg.Counter("kaiak_usage_clamped_records_total",
			"Usage records whose units or cost exceeded 2^53-1 (the protocol's bound) and were clamped to it."),
	}
	s.clamped.Add(0)
	return s
}

// RecordClamped counts one record clamped to the protocol's bound
// (accounting.RecorderOptions.OutOfRange).
func (s *UsageSink) RecordClamped() {
	s.clamped.Inc()
}

// Record updates the counters for one record. It only touches memory, so it never
// blocks the request goroutine it runs on.
//
// status is "partial" when the response stopped early (or never came), else
// "complete". key_group is the key's group, root_group its top-level group (the
// record's path, last and first). With key_id_label (group_label) off the key ID
// (key_group) is left empty, so new series carry no such label — root_group stays; series already
// written with one stay until restart (counters never go back).
func (s *UsageSink) Record(r accounting.UsageRecord) {
	keyID := r.KeyID
	var group, root string
	if n := len(r.Groups); n > 0 {
		group, root = r.Groups[n-1], r.Groups[0]
	}
	if snap := s.holder.Current(); snap != nil {
		if !snap.KeyIDLabel {
			keyID = ""
		}
		if !snap.GroupLabel {
			group = ""
		}
	}
	status := "complete"
	if r.Partial {
		status = "partial"
	}
	labels := []string{group, root, keyID, r.Model, status}
	s.records.Inc(labels...)
	s.cost.Add(uint64(max(r.CostNanoUSD, 0)), labels...)
	for unit, n := range r.Units {
		if n < 0 {
			continue
		}
		s.tokens.Add(uint64(n), append(labels, string(unit))...)
	}
}
