package metric

import (
	"bytes"
	"math"
	"slices"
	"strconv"
	"strings"
)

// The OpenTelemetry → Prometheus translation is Prometheus's own OTLP ingestion's
// default, UnderscoreEscapingWithSuffixes, as released (github.com/prometheus/
// otlptranslator v1.0.0, after OpenTelemetry's Prometheus compatibility
// specification), so a metric scraped and the same metric pushed to Prometheus over
// OTLP land as one series.

// unitWords are the Prometheus words of a unit's main part; a unit not here is
// written as itself, escaped.
var unitWords = map[string]string{
	"d": "days", "h": "hours", "min": "minutes", "s": "seconds", "ms": "milliseconds", "us": "microseconds",
	"ns": "nanoseconds",
	"By": "bytes", "KiBy": "kibibytes", "MiBy": "mebibytes", "GiBy": "gibibytes", "TiBy": "tibibytes",
	"KBy": "kilobytes", "MBy": "megabytes", "GBy": "gigabytes", "TBy": "terabytes",
	"m": "meters", "V": "volts", "A": "amperes", "J": "joules", "W": "watts", "g": "grams",
	"Cel": "celsius", "Hz": "hertz", "1": "", "%": "percent",
}

// perUnitWords are the words of a unit's part after '/'.
var perUnitWords = map[string]string{
	"s": "second", "m": "minute", "h": "hour", "d": "day", "w": "week", "mo": "month", "y": "year",
}

// prometheusName is the Prometheus name of a metric: the name's runs of characters
// outside [a-zA-Z0-9:] become one '_' (none at either end); the unit's word is
// appended unless the name already has it as a word — a unit in braces adds none, a
// unit x/y adds x's word and per_<y's word>; a counter ends in _total, and a gauge
// whose unit is 1 in _ratio (each moved to the end if the name has it elsewhere).
func prometheusName(name, unit string, kind Kind) string {
	words := strings.FieldsFunc(name, func(r rune) bool { return !prometheusNameChar(r) })
	main, per := unitSuffixes(unit)
	if slices.Contains(words, main) {
		main = ""
	}
	if per == "per_" {
		per = ""
	} else {
		per = strings.TrimSuffix(per, "_")
		if slices.Contains(words, per) {
			per = ""
		}
	}
	if per != "" {
		main = strings.TrimSuffix(main, "_")
	}
	if main != "" {
		words = append(words, main)
	}
	if per != "" {
		words = append(words, per)
	}
	switch {
	case kind == KindCounter:
		words = append(slices.DeleteFunc(words, func(w string) bool { return w == "total" }), "total")
	case kind == KindGauge && unit == "1":
		words = append(slices.DeleteFunc(words, func(w string) bool { return w == "ratio" }), "ratio")
	}
	return strings.Join(words, "_")
}

// unitSuffixes are the words a unit adds: its main part's and "per_" + its per
// part's, each escaped; a part in braces, or empty, adds none.
func unitSuffixes(unit string) (main, per string) {
	mainUnit, perUnit, hasPer := strings.Cut(unit, "/")
	if mainUnit = strings.TrimSpace(mainUnit); mainUnit != "" && !strings.ContainsAny(mainUnit, "{}") {
		main = mainUnit
		if word, ok := unitWords[mainUnit]; ok {
			main = word
		}
	}
	if perUnit = strings.TrimSpace(perUnit); hasPer && perUnit != "" && !strings.ContainsAny(perUnit, "{}") {
		word := perUnit
		if w, ok := perUnitWords[perUnit]; ok {
			word = w
		}
		if word != "" {
			per = "per_" + word
		}
	}
	return escapeUnit(main), escapeUnit(per)
}

// escapeUnit makes characters outside [a-zA-Z0-9:] '_', collapses runs of '_' and
// drops a leading one.
func escapeUnit(s string) string {
	var b strings.Builder
	underscore := false
	for _, r := range s {
		if !prometheusNameChar(r) {
			r = '_'
		}
		if r == '_' && underscore {
			continue
		}
		underscore = r == '_'
		b.WriteRune(r)
	}
	return strings.TrimPrefix(b.String(), "_")
}

func prometheusNameChar(r rune) bool {
	return 'a' <= r && r <= 'z' || 'A' <= r && r <= 'Z' || '0' <= r && r <= '9' || r == ':'
}

// prometheusLabel is the Prometheus label name of an attribute key: runs of
// characters outside [a-zA-Z0-9] become one '_'.
func prometheusLabel(key string) string {
	var b strings.Builder
	underscore := false
	for _, r := range key {
		if 'a' <= r && r <= 'z' || 'A' <= r && r <= 'Z' || '0' <= r && r <= '9' {
			b.WriteRune(r)
			underscore = false
			continue
		}
		if !underscore {
			b.WriteByte('_')
			underscore = true
		}
	}
	return b.String()
}

// PrometheusContentType is the Prometheus text exposition format, version 0.0.4.
const PrometheusContentType = "text/plain; version=0.0.4; charset=utf-8"

// WritePrometheus writes s in the Prometheus text exposition format under the
// Prometheus names: families by name, series by attribute values, so the output is
// deterministic. A counter is a Prometheus counter; an up-down counter and a gauge
// are Prometheus gauges. An attribute whose value is empty is left out of the
// series, which Prometheus reads as the same thing.
func WritePrometheus(w *bytes.Buffer, s Snapshot) {
	type named struct {
		name string
		f    *Family
	}
	families := make([]named, len(s.Families))
	for i := range s.Families {
		f := &s.Families[i]
		families[i] = named{prometheusName(f.Name, f.Unit, f.Kind), f}
	}
	slices.SortFunc(families, func(a, b named) int { return strings.Compare(a.name, b.name) })
	for _, n := range families {
		writeFamily(w, n.name, n.f)
	}
}

func writeFamily(w *bytes.Buffer, name string, f *Family) {
	typ := "gauge"
	switch f.Kind {
	case KindCounter:
		typ = "counter"
	case KindHistogram:
		typ = "histogram"
	}
	w.WriteString("# HELP ")
	w.WriteString(name)
	w.WriteByte(' ')
	writeEscaped(w, f.Description, false)
	w.WriteString("\n# TYPE ")
	w.WriteString(name)
	w.WriteByte(' ')
	w.WriteString(typ)
	w.WriteByte('\n')

	labels := make([]string, len(f.Attributes))
	for i, key := range f.Attributes {
		labels[i] = prometheusLabel(key)
	}
	line := sampleLine{w: w, name: name, labels: labels}
	for _, p := range f.Points {
		switch {
		case f.Kind == KindHistogram:
			line.histogram(p, f.Buckets)
		case f.Number == Int:
			line.write("", p.Attributes, "", strconv.FormatInt(p.Int, 10))
		default:
			line.write("", p.Attributes, "", formatFloat(p.Double))
		}
	}
}

type sampleLine struct {
	w      *bytes.Buffer
	name   string
	labels []string
}

// histogram writes one histogram point: cumulative buckets, then sum and count.
func (l sampleLine) histogram(p Point, bounds []float64) {
	var cumulative uint64
	for i, n := range p.Histogram.BucketCounts {
		cumulative += n
		le := "+Inf"
		if i < len(bounds) {
			le = formatFloat(bounds[i])
		}
		l.write("_bucket", p.Attributes, le, strconv.FormatUint(cumulative, 10))
	}
	l.write("_sum", p.Attributes, "", formatFloat(p.Histogram.Sum))
	l.write("_count", p.Attributes, "", strconv.FormatUint(p.Histogram.Count, 10))
}

// write writes one sample line: name+suffix, the labels with a non-empty value plus
// le when given, and the value.
func (l sampleLine) write(suffix string, values []string, le, value string) {
	w := l.w
	w.WriteString(l.name)
	w.WriteString(suffix)
	first := true
	label := func(name, value string) {
		if first {
			w.WriteByte('{')
			first = false
		} else {
			w.WriteByte(',')
		}
		w.WriteString(name)
		w.WriteString(`="`)
		writeEscaped(w, value, true)
		w.WriteByte('"')
	}
	for i, v := range values {
		if v != "" {
			label(l.labels[i], v)
		}
	}
	if le != "" {
		label("le", le)
	}
	if !first {
		w.WriteByte('}')
	}
	w.WriteByte(' ')
	w.WriteString(value)
	w.WriteByte('\n')
}

// Escapers for the format: backslash and newline always, double quote in label
// values too. Replacers are immutable and safe for concurrent use.
var (
	helpEscaper  = strings.NewReplacer(`\`, `\\`, "\n", `\n`)
	labelEscaper = strings.NewReplacer(`\`, `\\`, "\n", `\n`, `"`, `\"`)
)

// writeEscaped writes s escaped for a label value (quote) or a HELP text. Invalid
// UTF-8 is replaced, so the output always parses.
func writeEscaped(w *bytes.Buffer, s string, quote bool) {
	s = strings.ToValidUTF8(s, "�")
	if quote {
		_, _ = labelEscaper.WriteString(w, s)
		return
	}
	_, _ = helpEscaper.WriteString(w, s)
}

func formatFloat(v float64) string {
	switch {
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	case math.IsNaN(v):
		return "NaN"
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}
