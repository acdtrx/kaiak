package metrics

import (
	"bytes"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
)

// ContentType is the Prometheus text exposition format, version 0.0.4.
const ContentType = "text/plain; version=0.0.4; charset=utf-8"

// Handler serves the registry in the text exposition format.
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		var buf bytes.Buffer
		r.WriteText(&buf)
		w.Header().Set("Content-Type", ContentType)
		_, _ = w.Write(buf.Bytes()) // a failed write means the scraper left
	})
}

// WriteText writes every family in the text exposition format: families by name,
// series by label values, so the output is deterministic. A label whose value is
// empty is left out of the series, which Prometheus reads as the same thing.
func (r *Registry) WriteText(w *bytes.Buffer) {
	r.mu.Lock()
	families := make([]*family, 0, len(r.families))
	for _, f := range r.families {
		families = append(families, f)
	}
	r.mu.Unlock()
	slices.SortFunc(families, func(a, b *family) int { return strings.Compare(a.name, b.name) })
	for _, f := range families {
		f.writeText(w)
	}
}

// sample is one series' labels at write time.
type sample struct {
	labelValues []string
	s           *series
	value       float64 // collected gauges only
}

func (f *family) writeText(w *bytes.Buffer) {
	w.WriteString("# HELP ")
	w.WriteString(f.name)
	w.WriteByte(' ')
	writeEscaped(w, f.help, false)
	w.WriteString("\n# TYPE ")
	w.WriteString(f.name)
	w.WriteByte(' ')
	w.WriteString(f.kind.String())
	w.WriteByte('\n')

	var samples []sample
	if f.collect != nil {
		f.collect(func(value float64, labelValues ...string) {
			if len(labelValues) != len(f.labels) {
				panic("metrics: " + f.name + ": wrong number of label values emitted")
			}
			samples = append(samples, sample{labelValues: slices.Clone(labelValues), value: value})
		})
	} else {
		f.mu.RLock()
		for _, s := range f.series {
			samples = append(samples, sample{labelValues: s.labelValues, s: s})
		}
		f.mu.RUnlock()
	}
	slices.SortFunc(samples, func(a, b sample) int { return slices.Compare(a.labelValues, b.labelValues) })

	for _, smp := range samples {
		switch {
		case f.collect != nil:
			f.writeLine(w, "", smp.labelValues, "", "", formatFloat(smp.value))
		case f.kind == kindCounter:
			n := smp.s.count.Load()
			value := strconv.FormatUint(n, 10)
			if f.divisor != 1 {
				value = formatFloat(float64(n) / f.divisor)
			}
			f.writeLine(w, "", smp.labelValues, "", "", value)
		case f.kind == kindGauge:
			f.writeLine(w, "", smp.labelValues, "", "", formatFloat(math.Float64frombits(smp.s.bits.Load())))
		default:
			f.writeHistogram(w, smp)
		}
	}
}

// writeHistogram writes one histogram series: cumulative buckets, then sum and count.
// The count is the +Inf bucket, read from the same bucket loads, so the two always
// agree even while observations land.
func (f *family) writeHistogram(w *bytes.Buffer, smp sample) {
	var cumulative uint64
	for i := range smp.s.buckets {
		cumulative += smp.s.buckets[i].Load()
		le := "+Inf"
		if i < len(f.buckets) {
			le = formatFloat(f.buckets[i])
		}
		f.writeLine(w, "_bucket", smp.labelValues, "le", le, strconv.FormatUint(cumulative, 10))
	}
	f.writeLine(w, "_sum", smp.labelValues, "", "", formatFloat(math.Float64frombits(smp.s.bits.Load())))
	f.writeLine(w, "_count", smp.labelValues, "", "", strconv.FormatUint(cumulative, 10))
}

// writeLine writes one sample line: name+suffix, the non-empty labels plus an
// optional extra one (le), and the value.
func (f *family) writeLine(w *bytes.Buffer, suffix string, labelValues []string, extraName, extraValue, value string) {
	w.WriteString(f.name)
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
	for i, v := range labelValues {
		if v != "" {
			label(f.labels[i], v)
		}
	}
	if extraName != "" {
		label(extraName, extraValue)
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
	s = strings.ToValidUTF8(s, "\uFFFD")
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
