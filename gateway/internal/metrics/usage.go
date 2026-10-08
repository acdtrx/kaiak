package metrics

import (
	"kaiak/internal/accounting"
	"kaiak/internal/config"
	"kaiak/internal/provider"
	"kaiak/internal/telemetry/metric"
)

// usageLabels label every usage metric: the key's group, its top-level group, key ID,
// the public model, the operation, the provider and the status. One fixed label set
// whatever the tree's depth: intermediate levels are not labels. An empty value (the
// key's group or key ID switched off, no provider) is absent from the series. No
// backend: it would multiply every group's series by the backends serving each model;
// the ops metrics carry the backend. A group's labels never become metric labels.
var usageLabels = append(append([]string(nil), keyAttributes...),
	attrModel, "gen_ai.operation.name", "gen_ai.provider.name", "kaiak.usage.status")

// tokenLabels label the token counters: the usage labels and gen_ai.token.modality,
// the GenAI convention's required attribute.
var tokenLabels = append(append([]string(nil), usageLabels...), "gen_ai.token.modality")

// tokenModality is every token count's gen_ai.token.modality: no backend reports
// tokens by modality.
const tokenModality = "unknown"

// keyAttributes are the attributes naming a request's key, first on every per-key
// metric: the usage metrics and kaiak.request.errors. The key's group is
// kaiak.key.group, never a bare group: Prometheus scrape configs commonly set a
// target label named group, which would rename this one to exported_group.
var keyAttributes = []string{"kaiak.key.group", "kaiak.key.root_group", "kaiak.key.id"}

// keyLabels are the keyAttributes values for the key keyID whose group's path
// (top-level group first, the key's group last) is path, under the live config snap:
// the key's group, its top-level group, the key ID. Without a key (path empty) all
// three are empty. With snap's key_id_label (group_label) off, the key ID (the key's
// group) is empty, so new series carry no such attribute; the top-level group stays.
func keyLabels(snap *config.Snapshot, path []string, keyID string) []string {
	var group, root string
	if n := len(path); n > 0 {
		group, root = path[n-1], path[0]
	}
	if snap != nil {
		if !snap.KeyIDLabel {
			keyID = ""
		}
		if !snap.GroupLabel {
			group = ""
		}
	}
	return []string{group, root, keyID}
}

// providerName is gen_ai.provider.name for a record run on backend: its type's in the
// live config snap (provider.ProviderName, the request line's), "" for a self-hosted
// type or a backend snap no longer has.
func providerName(snap *config.Snapshot, backend string) string {
	if snap == nil {
		return ""
	}
	b, ok := snap.Backends[backend]
	if !ok {
		return ""
	}
	return provider.ProviderName(b.Type)
}

// UsageMetrics turns settled usage records into usage metrics (accounting.Metrics):
// the record — settled once, by accounting — is its only input, and nothing reads the
// metrics back into a record (docs/kaiak.md, principle 7).
type UsageMetrics struct {
	holder     *config.Holder
	records    *metric.Counter
	cost       *metric.Counter
	clamped    *metric.Counter
	input      *metric.Counter
	cacheRead  *metric.Counter
	cacheWrite *metric.Counter
	output     *metric.Counter
	reasoning  *metric.Counter
}

// NewUsageMetrics registers the usage metrics on reg. The kaiak.key.id and
// kaiak.key.group attributes follow the live config's global.metrics.key_id_label
// and group_label at each record.
func NewUsageMetrics(reg *metric.Registry, holder *config.Holder) *UsageMetrics {
	tokens := func(name, description string) *metric.Counter {
		return reg.Counter(metric.Definition{Name: name, Unit: "{token}", Description: description,
			Attributes: tokenLabels})
	}
	s := &UsageMetrics{
		holder: holder,
		records: reg.Counter(metric.Definition{Name: "kaiak.usage.records", Unit: "{record}",
			Description: "Usage records settled: one per routed request, plus one per retried attempt whose request reached the backend and got no answer.",
			Attributes:  usageLabels}),
		cost: reg.ScaledCounter(metric.Definition{Name: "kaiak.usage.cost_usd", Unit: "{USD}",
			Description: "Estimated cost in US dollars, from the model's price table.", Attributes: usageLabels}, 1e9),
		clamped: reg.Counter(metric.Definition{Name: "kaiak.usage.clamped_records", Unit: "{record}",
			Description: "Usage records whose units or cost exceeded 2^53-1 (the protocol's bound) and were clamped to it."}),
		input: tokens("gen_ai.client.inference.usage.input_tokens",
			"All input tokens: those read from and written to the cache included."),
		cacheRead: tokens("gen_ai.client.inference.usage.cache_read.input_tokens",
			"Input tokens read from the cache: part of the input count."),
		cacheWrite: tokens("gen_ai.client.inference.usage.cache_write.input_tokens",
			"Input tokens written to the cache: part of the input count."),
		output: tokens("gen_ai.client.inference.usage.output_tokens",
			"All output tokens: reasoning included."),
		reasoning: tokens("gen_ai.client.inference.usage.reasoning.output_tokens",
			"Reasoning output tokens: part of the output count."),
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
// The key labels come from the record's key ID and group path (keyLabels), the
// provider from its deployment's backend in the live config (providerName); series
// already written with a label a switch now leaves out stay until restart (counters
// never go back). The status is "partial" when the response stopped early (or never
// came), else "complete". Input counts as the request line's gen_ai.usage.input_tokens
// does: config.InputUnits, the input neither read from nor written to the cache plus
// its two cache parts.
func (s *UsageMetrics) Record(r accounting.UsageRecord) {
	snap := s.holder.Current()
	status := "complete"
	if r.Partial {
		status = "partial"
	}
	labels := append(keyLabels(snap, r.Groups, r.KeyID), r.Model, r.Operation,
		providerName(snap, r.Deployment.Backend), status)
	s.records.Inc(labels...)
	s.cost.Add(amount(r.CostNanoUSD), labels...)
	tokens := append(labels, tokenModality)
	s.input.Add(amount(r.Units.Sum(config.InputUnits)), tokens...)
	s.cacheRead.Add(amount(r.Units[config.UnitTokensCached]), tokens...)
	s.cacheWrite.Add(amount(r.Units[config.UnitTokensCacheWrite]), tokens...)
	s.output.Add(amount(r.Units[config.UnitTokensOut]), tokens...)
	s.reasoning.Add(amount(r.Units[config.UnitTokensReasoning]), tokens...)
}

// amount is a record's amount as a count: amounts are never negative, and one that
// were would count nothing rather than wrap.
func amount(n int64) uint64 {
	return uint64(max(n, 0))
}
