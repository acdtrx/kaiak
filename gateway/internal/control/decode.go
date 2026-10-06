package control

import (
	"bytes"
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	"kaiak/internal/accounting"
	"kaiak/internal/schemacheck"
)

// Each Decode function reads one message: syntax, then repeated object members (at any
// depth), then its schema walker, then its rules, then a strict typed decode (unknown
// fields are errors, so a schema field the Go types lack fails loudly instead of being
// dropped). A rejection is a *ValidationError listing the issues of the first stage
// that failed.

// DecodeConfigEvent reads a config event's data. The config inside is returned as
// sent, for config.Parse.
func DecodeConfigEvent(data []byte) (ConfigEvent, error) {
	return decode("config event", data, (*walker).configEvent, func(*ruleCheck, any) {},
		func(tree any, data []byte) (ConfigEvent, error) {
			var raw struct {
				Config json.RawMessage `json:"config"`
			}
			if err := json.Unmarshal(data, &raw); err != nil {
				return ConfigEvent{}, err
			}
			return ConfigEvent{ConfigHash: tree.(map[string]any)["config_hash"].(string), Config: raw.Config}, nil
		})
}

// DecodeTotals reads a totals event's data.
func DecodeTotals(data []byte) (Totals, error) {
	return decode("totals", data, (*walker).totals,
		func(r *ruleCheck, tree any) { r.totals(tree, "") }, decodeTyped[Totals])
}

// DecodeUsageRecord reads one usage record.
func DecodeUsageRecord(data []byte) (accounting.UsageRecord, error) {
	return decode("usage record", data, (*walker).usageRecord,
		func(r *ruleCheck, tree any) { r.usageRecord(tree, "") }, decodeTyped[accounting.UsageRecord])
}

// DecodeUsageBatch reads the body of POST /v1/usage.
func DecodeUsageBatch(data []byte) (UsageBatch, error) {
	return decode("usage batch", data, (*walker).usageBatch, (*ruleCheck).usageBatch, decodeTyped[UsageBatch])
}

// DecodeUsageAck reads the answer to POST /v1/usage.
func DecodeUsageAck(data []byte) (UsageAck, error) {
	return decode("usage ack", data, (*walker).usageAck, func(*ruleCheck, any) {}, decodeTyped[UsageAck])
}

// DecodeStatus reads the body of POST /v1/status.
func DecodeStatus(data []byte) (Status, error) {
	return decode("status", data, (*walker).status, (*ruleCheck).status, decodeTyped[Status])
}

func decode[T any](
	message string,
	data []byte,
	walk func(w *walker, v any, path string),
	rules func(r *ruleCheck, tree any),
	build func(tree any, data []byte) (T, error),
) (T, error) {
	var zero T
	reject := func(issues []Issue) (T, error) {
		return zero, &ValidationError{Message: message, Issues: issues}
	}
	tree, err := schemacheck.Decode(data)
	var duplicate *schemacheck.DuplicateMemberError
	switch {
	case errors.As(err, &duplicate):
		return reject([]Issue{{Code: CodeDuplicateMember, Path: duplicate.Path, Message: "appears more than once in its object"}})
	case err != nil:
		return reject([]Issue{{Code: CodeSyntax, Message: err.Error()}})
	}
	w := &walker{}
	walk(w, tree, "")
	if found := w.Issues(); len(found) > 0 {
		issues := make([]Issue, len(found))
		for i, issue := range found {
			issues[i] = Issue{Code: CodeSchema, Path: issue.Path, Message: issue.Message}
		}
		return reject(issues)
	}
	r := &ruleCheck{}
	rules(r, tree)
	if len(r.issues) > 0 {
		return reject(r.issues)
	}
	v, err := build(tree, data)
	if err != nil {
		// The walker accepted a message the Go types cannot hold: the two disagree.
		return reject([]Issue{{Code: CodeSchema, Message: err.Error()}})
	}
	return v, nil
}

// decodeTyped decodes a tree that passed its walker into T, strictly. Integer fields
// may be written with a fraction or exponent (4.0, 1e3) — JSON Schema counts those as
// integers — so they are rewritten as plain integers first; encoding/json would refuse
// them for an int64.
func decodeTyped[T any](tree any, _ []byte) (T, error) {
	var v T
	data, err := json.Marshal(plainIntegers(tree))
	if err != nil {
		return v, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	err = dec.Decode(&v)
	return v, err
}

func plainIntegers(v any) any {
	switch v := v.(type) {
	case map[string]any:
		for k, item := range v {
			v[k] = plainIntegers(item)
		}
	case []any:
		for i, item := range v {
			v[i] = plainIntegers(item)
		}
	case json.Number:
		if f := schemacheck.NumberValue(v); schemacheck.IsInteger(f) && strings.ContainsAny(string(v), ".eE") {
			return json.Number(strconv.FormatFloat(f, 'f', -1, 64))
		}
	}
	return v
}
