package config

import (
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"kaiak/internal/schemacheck"
)

// Structural validation: the checks protocol/schema/config.schema.json expresses, as
// a schemacheck walker. The shared fixtures in protocol/fixtures/config keep this
// walker and the schema in agreement.

var (
	idPattern              = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._@-]{0,127}$`)
	modelNamePattern       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,255}$`)
	backendModelPattern    = regexp.MustCompile(`^[!-~]{1,512}$`)
	datePattern            = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}$`)
	timestampPattern       = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]{1,9})?Z$`)
	baseURLPattern         = regexp.MustCompile(`^https?://[^/?#@` + schemacheck.JSWhitespace + `]+(/[^?#` + schemacheck.JSWhitespace + `]*[^/?#` + schemacheck.JSWhitespace + `])?$`)
	envNamePattern         = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	keyHashPattern         = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	reasoningEffortPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)
	labelKeyPattern        = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,62}$`)
)

// FormatVersion is the config document format this gateway reads; any other is
// rejected, never migrated.
const FormatVersion = 5

// maxPriceTiers bounds a price entry's tiers.
const maxPriceTiers = 8

// Bounds on a group's labels.
const (
	maxLabels          = 16
	maxLabelValueChars = 256
)

// reservedEnvPrefixes may not start an api_key_env name, mirroring the schema's
// ^(KAIAK|OTEL)_: the gateway's own settings and tokens (KAIAK_CONTROL_TOKEN,
// KAIAK_METRICS_TOKEN) and its log export's headers and endpoints (OTEL_EXPORTER_OTLP_*,
// which can carry the collector's credentials) live there, and a backend credential
// is sent to the backend's URL — which the config author chooses. The whole OTEL_
// prefix, not a list of the variables read today.
var reservedEnvPrefixes = []string{"KAIAK_", "OTEL_"}

// IsReservedEnvName reports whether name is one of the gateway's own variables, which
// no backend credential may name: the schema refuses it, and the provider reading the
// credential refuses it again.
func IsReservedEnvName(name string) bool {
	for _, prefix := range reservedEnvPrefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// reservedModelSuffix may not end a public model name: GET /v1/models/{id}/props would
// take such a model's own path for its props endpoint.
const reservedModelSuffix = "/props"

const publicModelNameWhat = `a model name not ending in "` + reservedModelSuffix + `"`

// IsID checks the shape of a backend, group or key ID. It,
// IsBackendModelName, IsPublicModelName and IsTimestamp are the config schema's shared
// $defs, which the protocol message schemas refer to.
func IsID(s string) bool { return idPattern.MatchString(s) }

// IsBackendModelName checks a model name on a backend (a deployment's model), in the
// backend's own naming: 1 to 512 printable ASCII characters, no spaces — llama-server's
// path-style ids included.
func IsBackendModelName(s string) bool { return backendModelPattern.MatchString(s) }

// backendModelNameWhat describes a backend model name in schema messages.
const backendModelNameWhat = "a backend model name: 1 to 512 printable ASCII characters, no spaces"

// IsTimestamp checks the shape of an RFC 3339 UTC timestamp (Z suffix); whether the
// instant exists is schemacheck.IsRealTimestamp.
func IsTimestamp(s string) bool { return timestampPattern.MatchString(s) }

// IsPublicModelName checks a public model name: a model name not ending in the
// reserved suffix.
func IsPublicModelName(s string) bool {
	return modelNamePattern.MatchString(s) && !strings.HasSuffix(s, reservedModelSuffix)
}

var (
	backendTypes = []string{
		string(BackendOpenAICompatible), string(BackendOpenAI), string(BackendAzureOpenAI), string(BackendVLLM),
		string(BackendLlamaServer), string(BackendAnthropic), string(BackendAzureAnthropic),
	}
	// Backend types whose API answers nothing without a key: api_key_env is required.
	keyedBackendTypes = []string{
		string(BackendOpenAI), string(BackendAzureOpenAI), string(BackendAnthropic), string(BackendAzureAnthropic),
	}
	limitTypeNames = limitTypeEnum()
)

// limitTypeEnum is the limit type names: every limit type, in order.
func limitTypeEnum() []string {
	var names []string
	for _, t := range LimitTypes() {
		names = append(names, string(t))
	}
	return names
}

// checkSchema validates the generic JSON tree against the config schema.
func checkSchema(tree any) []Issue {
	c := &schemaCheck{}
	c.document(tree)
	issues := make([]Issue, 0, len(c.Issues()))
	for _, issue := range c.Issues() {
		issues = append(issues, Issue{Code: CodeSchema, Path: issue.Path, Message: issue.Message})
	}
	return issues
}

type schemaCheck struct {
	schemacheck.Checker
}

func (c *schemaCheck) document(v any) {
	c.Object(v, "", map[string]schemacheck.Field{
		"format_version": {Required: true, Check: c.Const(FormatVersion)},
		"global":         {Required: true, Check: c.global},
		"backends":       {Required: true, Check: c.CollectionOf(idPattern.MatchString, "an ID", c.backend)},
		"models":         {Required: true, Check: c.CollectionOf(IsPublicModelName, publicModelNameWhat, c.model)},
		"groups":         {Check: c.CollectionOf(idPattern.MatchString, "an ID", c.group)},
		"keys":           {Required: true, Check: c.CollectionOf(idPattern.MatchString, "an ID", c.key)},
	})
}

func (c *schemaCheck) global(v any, path string) {
	c.Object(v, path, map[string]schemacheck.Field{
		"limits":                          {Check: c.limits},
		"max_request_body_bytes":          {Check: c.IntegerAtLeast(1)},
		"control_outage_grace_ms":         {Check: c.IntegerAtLeast(0)},
		"max_n":                           {Check: c.IntegerAtLeast(1)},
		"max_sequences_per_request":       {Check: c.IntegerAtLeast(1)},
		"max_embedding_inputs":            {Check: c.IntegerAtLeast(1)},
		"max_concurrent_requests_per_key": {Check: c.IntegerAtLeast(1)},
		"queue":                           {Check: func(v any, path string) { c.queueSettings(v, path) }},
		"retries":                         {Check: func(v any, path string) { c.retrySettings(v, path) }},
		"circuit": {Check: func(v any, path string) {
			c.Object(v, path, map[string]schemacheck.Field{
				"failure_threshold": {Check: c.IntegerAtLeast(1)},
				"probe_interval_ms": {Check: c.IntegerAtLeast(MinProbeIntervalMS)},
			})
		}},
		"metrics": {Check: func(v any, path string) {
			c.Object(v, path, map[string]schemacheck.Field{
				"key_id_label": {Check: c.Boolean},
				"group_label":  {Check: c.Boolean},
			})
		}},
	})
}

func (c *schemaCheck) backend(v any, path string) {
	m := c.Object(v, path, map[string]schemacheck.Field{
		"type":                   {Required: true, Check: c.Enum(backendTypes)},
		"base_url":               {Required: true, Check: c.StringMatching(baseURLPattern, "an http(s) URL with no credentials, trailing slash, query or fragment")},
		"api_key_env":            {Check: c.apiKeyEnv},
		"connect_timeout_ms":     {Check: c.IntegerAtLeast(1)},
		"first_event_timeout_ms": {Check: c.IntegerAtLeast(1)},
		"response_timeout_ms":    {Check: c.IntegerAtLeast(1)},
		"stall_timeout_ms":       {Check: c.IntegerAtLeast(1)},
		"max_in_flight":          {Check: c.IntegerAtLeast(1)},
	})
	if m == nil {
		return
	}
	// The cloud APIs answer nothing without a key.
	if t, _ := m["type"].(string); slices.Contains(keyedBackendTypes, t) {
		if _, ok := m["api_key_env"]; !ok {
			c.Fail(path, t+" backends need api_key_env")
		}
	}
}

func (c *schemaCheck) apiKeyEnv(v any, path string) {
	c.StringMatching(envNamePattern, "an environment variable name")(v, path)
	if s, ok := v.(string); ok && IsReservedEnvName(s) {
		c.Fail(path, "must not start with KAIAK_ or OTEL_: those variables hold the gateway's own settings and tokens "+
			"and its log export's headers and endpoints")
	}
}

func (c *schemaCheck) model(v any, path string) {
	c.Object(v, path, map[string]schemacheck.Field{
		"deployments": {Required: true, Check: c.ArrayOf(1, 0, true, func(v any, path string) {
			c.Object(v, path, map[string]schemacheck.Field{
				"backend": {Required: true, Check: c.StringMatching(idPattern, "an ID")},
				"model":   {Required: true, Check: c.StringMatching(backendModelPattern, backendModelNameWhat)},
			})
		})},
		"metadata": {Required: true, Check: func(v any, path string) {
			c.Object(v, path, map[string]schemacheck.Field{
				"context_length": {Required: true, Check: c.IntegerAtLeast(1)},
				"capabilities": {Required: true, Check: func(v any, path string) {
					c.Object(v, path, map[string]schemacheck.Field{
						"streaming": {Required: true, Check: c.Boolean},
						"tools":     {Required: true, Check: c.Boolean},
						"vision":    {Required: true, Check: c.Boolean},
						"reasoning": {Required: true, Check: c.Boolean},
					})
				}},
				"reasoning_efforts": {Check: c.ArrayOf(0, 0, true, c.StringMatching(reasoningEffortPattern, "a lowercase word"))},
			})
		}},
		"output_limit": {Check: func(v any, path string) {
			c.Object(v, path, map[string]schemacheck.Field{
				"default": {Required: true, Check: c.IntegerAtLeast(1)},
				"ceiling": {Required: true, Check: c.IntegerAtLeast(1)},
			})
		}},
		"queue": {Check: func(v any, path string) {
			if m := c.queueSettings(v, path); m != nil && len(m) == 0 {
				c.Fail(path, "must set at least one field")
			}
		}},
		"retries": {Check: func(v any, path string) {
			if m := c.retrySettings(v, path); m != nil {
				if _, ok := m["max_attempts"]; !ok {
					c.Fail(path, "must set max_attempts")
				}
			}
		}},
		"prices": {Check: c.ArrayOf(0, 0, false, func(v any, path string) {
			c.Object(v, path, map[string]schemacheck.Field{
				"effective_from": {Required: true, Check: c.StringMatching(datePattern, "a date, YYYY-MM-DD")},
				"tiers": {Required: true, Check: c.ArrayOf(1, maxPriceTiers, false, func(v any, path string) {
					c.Object(v, path, map[string]schemacheck.Field{
						"above_input_tokens": {Required: true, Check: c.IntegerAtLeast(0)},
						"usd_per_million":    {Required: true, Check: c.usdPerMillion},
					})
				})},
			})
		})},
	})
}

// queueSettings checks global.queue or a model's queue override; it returns the object,
// or nil when v is not one.
func (c *schemaCheck) queueSettings(v any, path string) map[string]any {
	return c.Object(v, path, map[string]schemacheck.Field{
		"size":       {Check: c.IntegerAtLeast(0)},
		"timeout_ms": {Check: c.IntegerAtLeast(1)},
	})
}

// retrySettings checks global.retries or a model's retries override; it returns the
// object, or nil when v is not one.
func (c *schemaCheck) retrySettings(v any, path string) map[string]any {
	return c.Object(v, path, map[string]schemacheck.Field{
		"max_attempts": {Check: c.IntegerBetween(1, MaxAttemptsCeiling)},
	})
}

func (c *schemaCheck) usdPerMillion(v any, path string) {
	fields := make(map[string]schemacheck.Field, len(PricedUnits))
	for _, unit := range PricedUnits {
		fields[string(unit)] = schemacheck.Field{Check: c.NumberAtLeast(0)}
	}
	m := c.Object(v, path, fields)
	if m != nil && len(m) == 0 {
		c.Fail(path, "must name at least one unit")
	}
}

func (c *schemaCheck) group(v any, path string) {
	c.Object(v, path, map[string]schemacheck.Field{
		"parent":         {Check: c.StringMatching(idPattern, "an ID")},
		"labels":         {Check: c.labels},
		"allowed_models": {Check: c.allowedModels},
		"limits":         {Check: c.limits},
		"child_defaults": {Check: func(v any, path string) {
			c.Object(v, path, map[string]schemacheck.Field{
				"allowed_models": {Check: c.allowedModels},
				"limits":         {Check: c.limits},
			})
		}},
	})
}

// labels checks a group's labels: shape only, the gateway gives them no meaning.
func (c *schemaCheck) labels(v any, path string) {
	m, ok := v.(map[string]any)
	if !ok {
		c.Fail(path, "must be an object")
		return
	}
	if len(m) > maxLabels {
		c.Fail(path, fmt.Sprintf("must hold at most %d labels", maxLabels))
	}
	for _, name := range slices.Sorted(maps.Keys(m)) {
		p := schemacheck.Pointer(path, name)
		if !labelKeyPattern.MatchString(name) {
			c.Fail(p, "must be a label key: a lowercase letter, then up to 62 lowercase letters, digits, _, . or -")
		}
		if s, ok := m[name].(string); !ok || !isLabelValue(s) {
			c.Fail(p, fmt.Sprintf("must be a string of 1 to %d characters with no control characters", maxLabelValueChars))
		}
	}
}

// isLabelValue: 1 to maxLabelValueChars code points (as the schema's u-flag pattern
// counts them), none of them a C0 or C1 control character or DEL.
func isLabelValue(s string) bool {
	n := 0
	for _, r := range s {
		if r <= 0x1F || (r >= 0x7F && r <= 0x9F) {
			return false
		}
		n++
	}
	return n >= 1 && n <= maxLabelValueChars
}

func (c *schemaCheck) key(v any, path string) {
	c.Object(v, path, map[string]schemacheck.Field{
		"hash":       {Required: true, Check: c.StringMatching(keyHashPattern, `"sha256:" and 64 lowercase hex digits`)},
		"group":      {Required: true, Check: c.StringMatching(idPattern, "an ID")},
		"expires_at": {Check: c.StringMatching(timestampPattern, "an RFC 3339 UTC timestamp (Z suffix)")},
		"disabled":   {Check: c.Boolean},
	})
}

func (c *schemaCheck) allowedModels(v any, path string) {
	c.ArrayOf(0, 0, true, func(v any, path string) {
		s, ok := v.(string)
		if !ok || (s != allModels && !IsPublicModelName(s)) {
			c.Fail(path, "must be "+publicModelNameWhat+` or "*"`)
		}
	})(v, path)
}

func (c *schemaCheck) limits(v any, path string) {
	c.ArrayOf(0, 0, false, c.limit)(v, path)
}

func (c *schemaCheck) limit(v any, path string) {
	m := c.Object(v, path, map[string]schemacheck.Field{
		"type":  {Required: true, Check: c.Enum(limitTypeNames)},
		"value": {Required: true, Check: c.NumberBetween(0, schemacheck.MaxSafeInteger)},
	})
	if m == nil {
		return
	}
	limitType, _ := m["type"].(string)
	value, isNumber := m["value"].(json.Number)
	// A value counting requests or tokens must be an integer.
	if isNumber && slices.Contains(limitTypeNames, limitType) && LimitType(limitType).Measure() != MeasureCost &&
		!schemacheck.IsInteger(schemacheck.NumberValue(value)) {
		c.Fail(schemacheck.Pointer(path, "value"), "must be an integer for "+limitType)
	}
}
