package control

import (
	"regexp"
	"slices"

	"kaiak/internal/config"
	"kaiak/internal/schemacheck"
)

// Structural validation: the checks protocol/schema/*.schema.json express for each
// message, as schemacheck walkers. The shared fixtures in protocol/fixtures/messages
// keep these walkers and the schemas in agreement.

var (
	instancePattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,252}$`)
	hex32Pattern      = regexp.MustCompile(`^[0-9a-f]{32}$`)
	configHashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
	requestIDPattern  = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)
	amountPattern     = regexp.MustCompile(`^(0|[1-9][0-9]{0,17})$`)
	hourStartPattern  = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:00:00Z$`)
	monthStartPattern = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-01T00:00:00Z$`)
)

// MaxBatchRecords is the most records one usage batch may hold.
const MaxBatchRecords = 500

var windowTypes = countedLimitTypes()

// countedLimitTypes is the totals' window types: the limit types the control plane
// counts, in order.
func countedLimitTypes() []string {
	var names []string
	for _, t := range config.LimitTypes() {
		if t.Counted() {
			names = append(names, string(t))
		}
	}
	return names
}

const (
	publicModelNameWhat = `a model name not ending in "/props"`
	timestampWhat       = "an RFC 3339 UTC timestamp (Z suffix)"
)

type walker struct {
	schemacheck.Checker
}

func (w *walker) id() func(v any, path string) {
	return w.StringWhere(config.IsID, "an ID")
}

func (w *walker) instance() func(v any, path string) {
	return w.StringMatching(instancePattern, "an instance ID: a letter or digit, then letters, digits, '.', '_' or '-'")
}

func (w *walker) timestamp() func(v any, path string) {
	return w.StringWhere(config.IsTimestamp, timestampWhat)
}

func (w *walker) count() func(v any, path string) {
	return w.IntegerBetween(0, schemacheck.MaxSafeInteger)
}

func (w *walker) positive() func(v any, path string) {
	return w.IntegerBetween(1, schemacheck.MaxSafeInteger)
}

func (w *walker) configHash() func(v any, path string) {
	return w.StringMatching(configHashPattern, "64 lowercase hex digits")
}

func (w *walker) hex32() func(v any, path string) {
	return w.StringMatching(hex32Pattern, "32 lowercase hex digits")
}

func (w *walker) usageRecord(v any, path string) {
	units := make(map[string]schemacheck.Field, len(config.TokenUnits))
	for _, unit := range config.TokenUnits {
		units[string(unit)] = schemacheck.Field{Required: true, Check: w.count()}
	}
	w.Object(v, path, map[string]schemacheck.Field{
		"record_id":        {Required: true, Check: w.hex32()},
		"request_id":       {Required: true, Check: w.StringMatching(requestIDPattern, "a request ID: 1 to 128 letters, digits, '.', '_', ':' or '-'")},
		"gateway_instance": {Required: true, Check: w.instance()},
		"key_id":           {Required: true, Check: w.id()},
		"groups":           {Required: true, Check: w.ArrayOf(1, config.MaxGroupDepth, true, w.id())},
		"model":            {Required: true, Check: w.StringWhere(config.IsPublicModelName, publicModelNameWhat)},
		"deployment": {Required: true, Check: func(v any, path string) {
			w.Object(v, path, map[string]schemacheck.Field{
				"backend": {Required: true, Check: w.id()},
				"model":   {Required: true, Check: w.StringWhere(config.IsBackendModelName, "a backend model name")},
			})
		}},
		"units": {Required: true, Check: func(v any, path string) {
			w.Object(v, path, units)
		}},
		"cost_nano_usd": {Required: true, Check: w.count()},
		"estimated":     {Required: true, Check: w.Boolean},
		"partial":       {Required: true, Check: w.Boolean},
		"gateway_time":  {Required: true, Check: w.timestamp()},
	})
}

// configEvent checks the envelope; the config inside is config.Parse's to check.
func (w *walker) configEvent(v any, path string) {
	w.Object(v, path, map[string]schemacheck.Field{
		"config_hash": {Required: true, Check: w.configHash()},
		"config": {Required: true, Check: func(v any, path string) {
			if _, ok := v.(map[string]any); !ok {
				w.Fail(path, "must be an object")
			}
		}},
	})
}

func (w *walker) totals(v any, path string) {
	w.Object(v, path, map[string]schemacheck.Field{
		"live_gateways": {Required: true, Check: w.count()},
		"counted_through": {Required: true, Check: w.ArrayOf(0, 0, false, func(v any, path string) {
			w.Object(v, path, map[string]schemacheck.Field{
				"epoch":    {Required: true, Check: w.hex32()},
				"sequence": {Required: true, Check: w.positive()},
			})
		})},
		"windows": {Required: true, Check: w.ArrayOf(0, 0, false, w.totalsWindow)},
	})
}

func (w *walker) totalsWindow(v any, path string) {
	m := w.Object(v, path, map[string]schemacheck.Field{
		"group":        {Check: w.id()},
		"type":         {Required: true, Check: w.Enum(windowTypes)},
		"window_start": {Required: true, Check: w.timestamp()},
		"used":         {Required: true, Check: w.StringMatching(amountPattern, "a string of at most 18 decimal digits, no leading zeros")},
	})
	if m == nil {
		return
	}
	start, isString := m["window_start"].(string)
	typ, _ := m["type"].(string)
	if !isString || !slices.Contains(windowTypes, typ) {
		return
	}
	switch window := config.LimitType(typ).Window(); window {
	case config.WindowHour:
		if !hourStartPattern.MatchString(start) {
			w.Fail(schemacheck.Pointer(path, "window_start"), "must be the top of an hour")
		}
	case config.WindowMonth:
		if !monthStartPattern.MatchString(start) {
			w.Fail(schemacheck.Pointer(path, "window_start"), "must be the first of a month at midnight")
		}
	default:
		panic("control: no window start shape for " + string(window))
	}
}

func (w *walker) batchID(v any, path string) {
	w.Object(v, path, map[string]schemacheck.Field{
		"instance": {Required: true, Check: w.instance()},
		"epoch":    {Required: true, Check: w.hex32()},
		"sequence": {Required: true, Check: w.positive()},
	})
}

func (w *walker) usageAck(v any, path string) {
	w.Object(v, path, map[string]schemacheck.Field{
		"batch": {Required: true, Check: w.batchID},
	})
}
