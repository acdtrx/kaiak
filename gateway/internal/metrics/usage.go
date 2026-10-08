package metrics

import (
	"kaiak/internal/accounting"
	"kaiak/internal/config"
	"kaiak/internal/telemetry/metric"
)

// usageLabels label every usage metric: the key's group, its top-level group, key ID,
// model and status. One fixed label set whatever the tree's depth: intermediate
// levels are not labels. The key's group is key_group, not group: Prometheus scrape
// configs commonly set a target label named group, which would rename this one to
// exported_group. An empty value (key_group or key ID switched off) is absent from
// the series. No backend: it would multiply every group's series by the backends
// serving each model; the ops metrics carry the backend. A group's labels never
// become metric labels.
var usageLabels = append(append([]string(nil), keyLabelNames...), "model", "status")

// keyLabelNames are the labels naming a request's key, first on every per-key metric:
// the usage metrics and kaiak_request_errors_total.
var keyLabelNames = []string{"key_group", "root_group", "key_id"}

// keyLabels are the keyLabelNames values for the key keyID whose group's path
// (top-level group first, the key's group last) is path: key_group the key's group,
// root_group its top-level group, key_id the key ID. Without a key (path empty) all
// three are empty. With the live config's key_id_label (group_label) off, key_id
// (key_group) is empty, so new series carry no such label; root_group stays.
func keyLabels(holder *config.Holder, path []string, keyID string) []string {
	var group, root string
	if n := len(path); n > 0 {
		group, root = path[n-1], path[0]
	}
	if snap := holder.Current(); snap != nil {
		if !snap.KeyIDLabel {
			keyID = ""
		}
		if !snap.GroupLabel {
			group = ""
		}
	}
	return []string{group, root, keyID}
}

// UsageMetrics turns settled usage records into usage metrics (accounting.Metrics):
// the record — settled once, by accounting — is its only input, and nothing reads the
// metrics back into a record (docs/kaiak.md, principle 7).
type UsageMetrics struct {
	holder  *config.Holder
	records *metric.Counter
	tokens  *metric.Counter
	cost    *metric.Counter
	clamped *metric.Counter
}

// NewUsageMetrics registers the usage metrics on reg. The key_id and key_group labels
// follow the live config's global.metrics.key_id_label and group_label at each record.
func NewUsageMetrics(reg *metric.Registry, holder *config.Holder) *UsageMetrics {
	s := &UsageMetrics{
		holder: holder,
		records: reg.Counter(metric.Definition{Name: "kaiak.usage.records", Unit: "{record}",
			Description: "Usage records settled: one per routed request, plus one per retried attempt whose request reached the backend and got no answer.",
			Attributes:  usageLabels}),
		tokens: reg.Counter(metric.Definition{Name: "kaiak.usage.tokens", Unit: "{token}",
			Description: "Tokens by usage unit, as settled by accounting.",
			Attributes:  append(append([]string(nil), usageLabels...), "unit")}),
		cost: reg.ScaledCounter(metric.Definition{Name: "kaiak.usage.cost_usd", Unit: "{USD}",
			Description: "Estimated cost in US dollars, from the model's price table.", Attributes: usageLabels}, 1e9),
		clamped: reg.Counter(metric.Definition{Name: "kaiak.usage.clamped_records", Unit: "{record}",
			Description: "Usage records whose units or cost exceeded 2^53-1 (the protocol's bound) and were clamped to it."}),
	}
	s.clamped.Add(0)
	return s
}

// RecordClamped counts one record clamped to the protocol's bound.
func (s *UsageMetrics) RecordClamped() {
	s.clamped.Inc()
}

// Record updates the counters for one record. It only touches memory, so it never
// blocks the request goroutine it runs on.
//
// The key labels come from the record's key ID and group path (keyLabels); series
// already written with a label a switch now leaves out stay until restart (counters
// never go back). status is "partial" when the response stopped early (or never
// came), else "complete".
func (s *UsageMetrics) Record(r accounting.UsageRecord) {
	status := "complete"
	if r.Partial {
		status = "partial"
	}
	labels := append(keyLabels(s.holder, r.Groups, r.KeyID), r.Model, status)
	s.records.Inc(labels...)
	s.cost.Add(uint64(max(r.CostNanoUSD, 0)), labels...)
	for unit, n := range r.Units {
		if n < 0 {
			continue
		}
		s.tokens.Add(uint64(n), append(labels, string(unit))...)
	}
}
