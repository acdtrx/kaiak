package control

import (
	"encoding/json"

	"kaiak/internal/accounting"
	"kaiak/internal/schemacheck"
)

// Each Decode function reads one message: schemacheck.Validate (syntax, repeated
// object members at any depth, the message's schema walker), then its rules, then the
// strict typed decode (schemacheck.DecodeTyped). A rejection is a
// *schemacheck.ValidationError listing the issues of the first stage that failed.

// DecodeConfigEvent reads a config event's data. The config inside is returned as
// sent, for config.Parse.
func DecodeConfigEvent(data []byte) (ConfigEvent, error) {
	tree, err := validate("config event", data, (*walker).configEvent)
	if err != nil {
		return ConfigEvent{}, err
	}
	var raw struct {
		Config json.RawMessage `json:"config"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return ConfigEvent{}, err // unreachable: Validate has read data as one JSON object
	}
	return ConfigEvent{ConfigHash: tree.(map[string]any)["config_hash"].(string), Config: raw.Config}, nil
}

// DecodeTotals reads a totals event's data.
func DecodeTotals(data []byte) (Totals, error) {
	return decode[Totals]("totals", data, (*walker).totals, func(r *ruleCheck, tree any) { r.totals(tree, "") })
}

// DecodeUsageRecord reads one usage record.
func DecodeUsageRecord(data []byte) (accounting.UsageRecord, error) {
	return decode[accounting.UsageRecord]("usage record", data, (*walker).usageRecord,
		func(r *ruleCheck, tree any) { r.usageRecord(tree, "") })
}

// DecodeUsageAck reads the answer to POST /v1/usage.
func DecodeUsageAck(data []byte) (UsageAck, error) {
	return decode[UsageAck]("usage ack", data, (*walker).usageAck, func(*ruleCheck, any) {})
}

// decode reads the message subject into T: validate, then rules, then the typed decode.
func decode[T any](
	subject string,
	data []byte,
	walk func(w *walker, v any, path string),
	rules func(r *ruleCheck, tree any),
) (T, error) {
	var zero T
	tree, err := validate(subject, data, walk)
	if err != nil {
		return zero, err
	}
	r := &ruleCheck{}
	rules(r, tree)
	if len(r.issues) > 0 {
		return zero, &schemacheck.ValidationError{Subject: subject, Issues: r.issues}
	}
	return schemacheck.DecodeTyped[T](subject, tree)
}

// validate reads the message subject's generic tree, checked by its walker.
func validate(subject string, data []byte, walk func(w *walker, v any, path string)) (any, error) {
	return schemacheck.Validate(subject, data, func(tree any) []schemacheck.Issue {
		w := &walker{}
		walk(w, tree, "")
		return w.Issues()
	})
}
