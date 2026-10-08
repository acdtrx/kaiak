package otlpmetric

import (
	"strings"
	"time"

	"kaiak/internal/telemetry/metric"
	"kaiak/internal/telemetry/otlp"
)

// aggregationTemporality is OTLP's AggregationTemporality, as its JSON writes it: an
// integer. A gauge has none.
type aggregationTemporality int

const (
	temporalityNone       aggregationTemporality = 0
	temporalityDelta      aggregationTemporality = 1
	temporalityCumulative aggregationTemporality = 2
)

// temporalityOf is the temporality a family is exported with under the preference
// pref, as the OpenTelemetry specification defines the three preferences
// (docs/specs/GATEWAY.md, Observability → OTLP metric export: temporality):
// up-down counters are cumulative under every one; delta has counters and
// histograms delta; lowmemory only those recorded as events happen, so a counter
// read at collect stays cumulative.
func temporalityOf(pref otlp.Temporality, f *metric.Family) aggregationTemporality {
	switch f.Kind {
	case metric.KindGauge:
		return temporalityNone
	case metric.KindCounter:
		if pref == otlp.Delta || pref == otlp.LowMemory && !f.Observed {
			return temporalityDelta
		}
	case metric.KindHistogram:
		if pref != otlp.Cumulative {
			return temporalityDelta
		}
	}
	return temporalityCumulative
}

// stream is one family as an export sends it: its points under its temporality.
type stream struct {
	family      *metric.Family
	temporality aggregationTemporality
	points      []metric.Point
}

// reader is the exporter's view of the registry across collects: its temporality
// preference, and for the delta streams the last value collected of each series.
// It belongs to the exporter alone — a scrape of the same registry never sees it.
type reader struct {
	pref otlp.Temporality
	// last is, by family name and series, the series' cumulative value at the last
	// collect that carried it, and that collect's time. A series kept here lives as
	// long as the registry keeps it (recorded series live as long as the registry),
	// and one read at collect that went away and came back continues from its last
	// value instead of counting its whole value twice.
	last map[string]map[string]lastPoint
}

type lastPoint struct {
	at    time.Time
	point metric.Point
}

func newReader(pref otlp.Temporality) *reader {
	return &reader{pref: pref, last: make(map[string]map[string]lastPoint)}
}

// streams applies the temporality to a collect. A delta stream holds every series,
// an unchanged one as 0, so a series created at 0 stays in the stream; each delta
// starts at the previous collect that carried its series, or at the series' start
// for its first. The state moves on at every collect, whatever becomes of the
// export: a delta not delivered is lost, as the specification's delta streams lose
// it. A cumulative stream is the collect as it stands, each point starting at its
// series' start. Families with no series are left out.
func (r *reader) streams(snap metric.Snapshot) []stream {
	out := make([]stream, 0, len(snap.Families))
	for i := range snap.Families {
		f := &snap.Families[i]
		if len(f.Points) == 0 {
			continue
		}
		t := temporalityOf(r.pref, f)
		points := f.Points
		if t == temporalityDelta {
			points = r.deltas(snap.Time, f)
		}
		out = append(out, stream{family: f, temporality: t, points: points})
	}
	return out
}

// deltas turns a family's cumulative points into the change since the last collect.
func (r *reader) deltas(now time.Time, f *metric.Family) []metric.Point {
	last := r.last[f.Name]
	if last == nil {
		last = make(map[string]lastPoint, len(f.Points))
		r.last[f.Name] = last
	}
	out := make([]metric.Point, len(f.Points))
	for i, p := range f.Points {
		key := strings.Join(p.Attributes, "\xff")
		prev, seen := last[key]
		d := p
		if seen {
			d.StartTime = prev.at
			d = subtract(f, d, prev.point)
		}
		out[i] = d
		last[key] = lastPoint{at: now, point: p}
	}
	return out
}

// subtract is cur less prev. A scaled counter subtracts its whole sub-units and
// divides the change once, so the deltas of a cost sum exactly to its cumulative
// value. A value below the previous one means the series started over (a counter
// never goes back while the registry lives): the whole of cur is the change then.
func subtract(f *metric.Family, cur, prev metric.Point) metric.Point {
	switch f.Kind {
	case metric.KindCounter:
		switch {
		case f.Divisor > 0:
			if cur.SubUnits >= prev.SubUnits {
				cur.SubUnits -= prev.SubUnits
				cur.Double = float64(cur.SubUnits) / f.Divisor
			}
		case cur.Int >= prev.Int:
			cur.Int -= prev.Int
		}
	case metric.KindHistogram:
		h, ph := cur.Histogram, prev.Histogram
		if h.Count < ph.Count || len(h.BucketCounts) != len(ph.BucketCounts) {
			return cur
		}
		d := metric.HistogramValue{BucketCounts: make([]uint64, len(h.BucketCounts)), Count: h.Count - ph.Count, Sum: h.Sum - ph.Sum}
		for i := range h.BucketCounts {
			d.BucketCounts[i] = h.BucketCounts[i] - ph.BucketCounts[i]
		}
		cur.Histogram = d
	}
	return cur
}
