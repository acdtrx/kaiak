package control

import (
	"fmt"
	"maps"
	"regexp"
	"slices"

	"kaiak/internal/config"
	"kaiak/internal/schemacheck"
)

// The messages the gateway only sends — the usage batch and the status — are checked
// here, in the tests, not in the binary: their walkers, rules and decoders are the
// oracle the shared fixtures hold to protocol/schema, and the tests hold what the
// gateway builds to them (docs/specs/GATEWAY.md, Control-plane mode → Messages the
// gateway only sends).

var (
	states      = []string{string(StateStarting), string(StateReady), string(StateDraining)}
	circuits    = []string{string(CircuitClosed), string(CircuitOpen), string(CircuitHalfOpen)}
	codePattern = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
)

// DecodeUsageBatch reads the body of POST /v1/usage.
func DecodeUsageBatch(data []byte) (UsageBatch, error) {
	return decode("usage batch", data, (*walker).usageBatch, (*ruleCheck).usageBatch, decodeTyped[UsageBatch])
}

// DecodeStatus reads the body of POST /v1/status.
func DecodeStatus(data []byte) (Status, error) {
	return decode("status", data, (*walker).status, (*ruleCheck).status, decodeTyped[Status])
}

func (w *walker) usageBatch(v any, path string) {
	w.Object(v, path, map[string]schemacheck.Field{
		"batch":   {Required: true, Check: w.batchID},
		"records": {Required: true, Check: w.ArrayOf(1, MaxBatchRecords, false, w.usageRecord)},
	})
}

func (w *walker) status(v any, path string) {
	w.Object(v, path, map[string]schemacheck.Field{
		"instance":            {Required: true, Check: w.instance()},
		"protocol_version":    {Required: true, Check: w.Const(ProtocolVersion)},
		"state":               {Required: true, Check: w.Enum(states)},
		"started_at":          {Required: true, Check: w.timestamp()},
		"applied_config_hash": {Required: true, Check: schemacheck.Nullable(w.configHash())},
		"last_rejection": {Required: true, Check: schemacheck.Nullable(func(v any, path string) {
			w.Object(v, path, map[string]schemacheck.Field{
				"config_hash": {Required: true, Check: w.configHash()},
				"codes": {Required: true, Check: w.ArrayOf(1, 0, true,
					w.StringMatching(codePattern, "a lowercase code, words joined by '-'"))},
			})
		})},
		"backends": {Required: true, Check: w.CollectionOf(config.IsID, "a backend ID", w.backendStatus)},
		"models": {Required: true, Check: w.CollectionOf(config.IsPublicModelName, publicModelNameWhat,
			func(v any, path string) {
				w.Object(v, path, map[string]schemacheck.Field{
					"queued": {Required: true, Check: w.count()},
				})
			})},
	})
}

func (w *walker) backendStatus(v any, path string) {
	w.Object(v, path, map[string]schemacheck.Field{
		"in_flight":     {Required: true, Check: w.count()},
		"max_in_flight": {Check: w.IntegerBetween(1, schemacheck.MaxSafeInteger)},
		"deployments":   {Required: true, Check: w.CollectionOf(config.IsBackendModelName, "a backend model name", w.deploymentStatus)},
	})
}

func (w *walker) deploymentStatus(v any, path string) {
	m := w.Object(v, path, map[string]schemacheck.Field{
		"circuit":   {Required: true, Check: w.Enum(circuits)},
		"opened_at": {Check: w.timestamp()},
	})
	if m == nil {
		return
	}
	_, hasOpenedAt := m["opened_at"]
	circuit, _ := m["circuit"].(string)
	notClosed := circuit == string(CircuitOpen) || circuit == string(CircuitHalfOpen)
	switch {
	case notClosed && !hasOpenedAt:
		w.Fail(path, "an open or half-open circuit needs opened_at")
	case !notClosed && hasOpenedAt:
		w.Fail(schemacheck.Pointer(path, "opened_at"), "is set only while the circuit is open or half-open")
	}
}

func (r *ruleCheck) usageBatch(tree any) {
	m := tree.(map[string]any)
	instance := m["batch"].(map[string]any)["instance"].(string)
	seen := map[string]int{}
	for i, item := range m["records"].([]any) {
		record := item.(map[string]any)
		recordPath := schemacheck.Pointer("/records", i)
		r.usageRecord(record, recordPath)
		if got := record["gateway_instance"].(string); got != instance {
			r.report(CodeRecordInstanceMismatch, schemacheck.Pointer(recordPath, "gateway_instance"),
				fmt.Sprintf("%q is not the batch's instance %q", got, instance))
		}
		id := record["record_id"].(string)
		if first, dup := seen[id]; dup {
			r.report(CodeRecordIDDuplicate, schemacheck.Pointer(recordPath, "record_id"),
				"same record ID as "+schemacheck.Pointer("/records", first))
			continue
		}
		seen[id] = i
	}
}

func (r *ruleCheck) status(tree any) {
	m := tree.(map[string]any)
	r.timestamp(m["started_at"], "/started_at")
	backends := m["backends"].(map[string]any)
	for _, id := range slices.Sorted(maps.Keys(backends)) {
		deployments := backends[id].(map[string]any)["deployments"].(map[string]any)
		for _, model := range slices.Sorted(maps.Keys(deployments)) {
			if openedAt, ok := deployments[model].(map[string]any)["opened_at"]; ok {
				r.timestamp(openedAt, schemacheck.Pointer("/backends", id, "deployments", model, "opened_at"))
			}
		}
	}
}
