package metric

import (
	"fmt"
	"math"
	"slices"
	"strings"
	"time"
)

// ObservableCounter is a monotonic counter read at collect from a count kept
// elsewhere, which must never go back.
type ObservableCounter struct{ f *family }

// ObservableUpDownCounter is an up-down counter read at collect from live state.
type ObservableUpDownCounter struct{ f *family }

// ObservableGauge is a gauge read at collect from live state.
type ObservableGauge struct{ f *family }

// Observable is an instrument read at collect, observed by one callback
// (Registry.Callback).
type Observable interface {
	observedFamily() *family
}

func (c *ObservableCounter) observedFamily() *family       { return c.f }
func (c *ObservableUpDownCounter) observedFamily() *family { return c.f }
func (g *ObservableGauge) observedFamily() *family         { return g.f }

// ObservableCounter registers a counter read at collect, collected as an integer.
func (r *Registry) ObservableCounter(d Definition) *ObservableCounter {
	return &ObservableCounter{r.register(d, KindCounter, Int, true)}
}

// ObservableUpDownCounter registers an up-down counter read at collect.
func (r *Registry) ObservableUpDownCounter(d Definition) *ObservableUpDownCounter {
	return &ObservableUpDownCounter{r.register(d, KindUpDownCounter, Int, true)}
}

// ObservableGauge registers a gauge read at collect.
func (r *Registry) ObservableGauge(d Definition) *ObservableGauge {
	return &ObservableGauge{r.register(d, KindGauge, Double, true)}
}

type callback struct {
	observe func(*Observer)
}

// Callback registers observe as the one reader of instruments: each collect runs it
// once, and it observes each series of those instruments once — so the instruments
// of one callback agree with each other in a collect. It runs on the collecting
// goroutine, possibly in several collects at once, and must not block. An
// instrument with a callback already, or observed by a callback that does not read
// it, is a programming error and panics.
func (r *Registry) Callback(observe func(*Observer), instruments ...Observable) {
	cb := &callback{observe: observe}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, inst := range instruments {
		f := inst.observedFamily()
		if r.families[f.def.Name] != f {
			panic(fmt.Sprintf("metric: %s: not an instrument of this registry", f.def.Name))
		}
		if f.cb != nil {
			panic(fmt.Sprintf("metric: %s: read by two callbacks", f.def.Name))
		}
		f.cb = cb
	}
	r.callbacks = append(r.callbacks, cb)
}

// Observer takes the observations of one callback run.
type Observer struct {
	cb     *callback
	start  time.Time
	points map[*family][]Point
}

// Observe records v for the series with the given attribute values.
func (c *ObservableCounter) Observe(o *Observer, v uint64, attrs ...string) {
	o.add(c.f, Point{Int: int64(v)}, attrs)
}

// Observe records v for the series with the given attribute values.
func (c *ObservableUpDownCounter) Observe(o *Observer, v int64, attrs ...string) {
	o.add(c.f, Point{Int: v}, attrs)
}

// Observe records v for the series with the given attribute values.
func (g *ObservableGauge) Observe(o *Observer, v float64, attrs ...string) {
	o.add(g.f, Point{Double: v}, attrs)
}

func (o *Observer) add(f *family, p Point, attrs []string) {
	if f.cb != o.cb {
		panic(fmt.Sprintf("metric: %s: observed outside its callback", f.def.Name))
	}
	f.checkAttrs(attrs)
	p.Attributes = slices.Clone(attrs)
	if f.kind != KindGauge {
		p.StartTime = o.start
	}
	o.points[f] = append(o.points[f], p)
}

// Snapshot is one collect of a registry in the data model.
type Snapshot struct {
	// Time is when the collect ran: taken once every series is read, so no point
	// in it starts after it — a series created during the collect included.
	Time time.Time
	// Families are every registered instrument, by name.
	Families []Family
}

// Family is one instrument and its points. Its Definition shares the registry's
// slices and map: read-only.
type Family struct {
	Definition
	Kind Kind
	// Number is how a sum's or gauge's points carry their value.
	Number Number
	// Observed: read at collect, not recorded as events happen.
	Observed bool
	// Divisor of a scaled counter (Registry.ScaledCounter): its points' Double is
	// SubUnits ÷ Divisor. 0 for every other family.
	Divisor float64
	// Points are the series, by attribute values.
	Points []Point
}

// Point is one series' value at the collect.
type Point struct {
	// Attributes are the values of the definition's attribute keys, in order; "" is
	// an attribute the point does not carry.
	Attributes []string
	// StartTime of a sum or histogram: a recorded series' creation; the registry's
	// creation for a series read at collect, whose value is kept since before any
	// collect saw it. Zero for a gauge.
	StartTime time.Time
	// Int or Double is the value of a sum or gauge, as the family's Number says.
	Int    int64
	Double float64
	// SubUnits is a scaled counter's exact count of whole sub-units, which Double
	// divides: a reader that subtracts values subtracts these, so sums stay exact.
	SubUnits uint64
	// Histogram is a histogram's value.
	Histogram HistogramValue
}

// HistogramValue is a histogram point's buckets, count and sum.
type HistogramValue struct {
	// BucketCounts are per bucket (not cumulative), one more than the bounds: the
	// last is the bucket to +Inf.
	BucketCounts []uint64
	// Count is the sum of BucketCounts.
	Count uint64
	Sum   float64
}

// Collect reads every instrument: callbacks run once each, recorded series are read
// as they stand.
func (r *Registry) Collect() Snapshot {
	r.mu.Lock()
	families := make([]*family, 0, len(r.families))
	for _, f := range r.families {
		families = append(families, f)
	}
	callbacks := slices.Clone(r.callbacks)
	r.mu.Unlock()

	o := &Observer{start: r.start, points: make(map[*family][]Point)}
	for _, cb := range callbacks {
		o.cb = cb
		cb.observe(o)
	}

	snap := Snapshot{Families: make([]Family, 0, len(families))}
	for _, f := range families {
		fam := Family{Definition: f.def, Kind: f.kind, Number: f.number, Observed: f.observed, Divisor: f.divisor}
		if f.observed {
			fam.Points = o.points[f]
		} else {
			fam.Points = f.collectSeries()
		}
		slices.SortFunc(fam.Points, func(a, b Point) int { return slices.Compare(a.Attributes, b.Attributes) })
		snap.Families = append(snap.Families, fam)
	}
	snap.Time = time.Now()
	slices.SortFunc(snap.Families, func(a, b Family) int { return strings.Compare(a.Name, b.Name) })
	return snap
}

// collectSeries reads a recorded family's series. A histogram's count is the sum of
// the same bucket loads, so the two always agree even while observations land.
func (f *family) collectSeries() []Point {
	f.mu.RLock()
	all := make([]*series, 0, len(f.series))
	for _, s := range f.series {
		all = append(all, s)
	}
	f.mu.RUnlock()
	points := make([]Point, 0, len(all))
	for _, s := range all {
		p := Point{Attributes: s.attrs}
		if f.kind != KindGauge {
			p.StartTime = s.start
		}
		switch f.kind {
		case KindCounter:
			n := s.count.Load()
			if f.divisor > 0 {
				p.SubUnits = n
				p.Double = float64(n) / f.divisor
			} else {
				p.Int = int64(n)
			}
		case KindUpDownCounter:
			p.Int = s.level.Load()
		case KindGauge:
			p.Double = math.Float64frombits(s.bits.Load())
		case KindHistogram:
			h := HistogramValue{BucketCounts: make([]uint64, len(s.buckets))}
			for i := range s.buckets {
				h.BucketCounts[i] = s.buckets[i].Load()
				h.Count += h.BucketCounts[i]
			}
			h.Sum = math.Float64frombits(s.bits.Load())
			p.Histogram = h
		}
		points = append(points, p)
	}
	return points
}
