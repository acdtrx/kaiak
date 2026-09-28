package control

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"kaiak/internal/schemacheck"
)

// Message rules the schemas cannot express (docs/specs/CONTROL-PROTOCOL.md,
// Messages). They read the generic tree of a message that passed its walker, so every
// shape they assert is already guaranteed; they run before the typed decode, which
// could not hold an instant that does not exist.

type ruleCheck struct {
	issues []Issue
}

func (r *ruleCheck) report(code, path, message string) {
	r.issues = append(r.issues, Issue{Code: code, Path: path, Message: message})
}

func (r *ruleCheck) timestamp(value any, path string) {
	if s, _ := value.(string); !schemacheck.IsRealTimestamp(s) {
		r.report(CodeTimestampInvalid, path, fmt.Sprintf("%q is not a real instant", s))
	}
}

func (r *ruleCheck) usageRecord(tree any, path string) {
	r.timestamp(tree.(map[string]any)["gateway_time"], schemacheck.Pointer(path, "gateway_time"))
}

func (r *ruleCheck) totals(tree any, path string) {
	seen := map[string]int{}
	for i, item := range tree.(map[string]any)["windows"].([]any) {
		window := item.(map[string]any)
		windowPath := schemacheck.Pointer(path, "windows", i)
		r.timestamp(window["window_start"], schemacheck.Pointer(windowPath, "window_start"))
		identity := windowIdentity(window)
		if first, dup := seen[identity]; dup {
			r.report(CodeTotalsWindowDuplicate, windowPath, "same limit as "+schemacheck.Pointer(path, "windows", first))
			continue
		}
		seen[identity] = i
	}
}

// windowIdentity: a window belongs to one limit — group (absent: global), type and
// model set, order ignored; "all models" (no models) is its own set.
func windowIdentity(window map[string]any) string {
	models := "*"
	if list, ok := window["models"].([]any); ok {
		names := make([]string, len(list))
		for i, name := range list {
			names[i] = name.(string)
		}
		slices.Sort(names)
		models = strings.Join(names, "\n")
	}
	group, _ := window["group"].(string)
	return strings.Join([]string{group, window["type"].(string), models}, "\n")
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

func (r *ruleCheck) usageAck(tree any) {
	r.totals(tree.(map[string]any)["totals"], "/totals")
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
