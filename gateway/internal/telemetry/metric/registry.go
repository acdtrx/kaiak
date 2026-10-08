// Package metric holds metrics in OpenTelemetry's data model: a registry of
// instruments — counters, up-down counters, gauges and histograms with explicit
// bounds, recorded as events happen or read at collect — a collect step that turns
// them into a snapshot of the data model, and the Prometheus text writer over a
// snapshot, which names each metric as Prometheus's own OTLP ingestion would.
//
// Code records through its instruments and never knows about export: every reader
// (a scrape, an OTLP exporter) collects on its own and keeps its own state.
// Recording is atomic operations on an existing series; a collect never blocks a
// recording for more than a map copy.
package metric

import (
	"fmt"
	"math"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Kind is an instrument's kind in the data model.
type Kind int

const (
	// KindCounter only grows: a monotonic sum.
	KindCounter Kind = iota
	// KindUpDownCounter goes both ways and still means something summed over its
	// series: a non-monotonic sum.
	KindUpDownCounter
	// KindGauge is a value whose sum means nothing.
	KindGauge
	// KindHistogram counts observations in explicit buckets, with their sum.
	KindHistogram
)

// Number is how a sum's or a gauge's values are carried: as integers or doubles.
type Number int

const (
	Int Number = iota
	Double
)

// Definition is a metric as the data model defines it. Name follows OpenTelemetry's
// instrument name syntax (a letter, then letters, digits, '_', '.', '-' or '/', at
// most 255); Unit is UCUM-style printable ASCII, at most 63, or empty; Attributes are
// the attribute keys every point carries a value for (a letter, then letters, digits,
// '_' or '.'), in the order recordings pass the values; Buckets are a histogram's
// explicit bounds, increasing and finite (the last bucket, to +Inf, is implicit), and
// only a histogram's.
type Definition struct {
	Name        string
	Unit        string
	Description string
	Attributes  []string
	Buckets     []float64
}

// Registry holds instruments and collects them. An instrument's definition is fixed
// when it is registered; every recording passes one value per attribute key, "" for
// an attribute the point does not carry. Registering an invalid definition or one
// whose name — OpenTelemetry's or its Prometheus translation — is taken, or recording
// with the wrong number of attribute values, is a programming error and panics.
//
// A recorded series is created on first use — or ahead of it, at 0, by Add(0) or
// Prepare, so the first event shows as an increase — and lives as long as the
// registry: counters never go back.
type Registry struct {
	start time.Time

	mu        sync.Mutex
	families  map[string]*family
	promNames map[string]bool
	callbacks []*callback
}

// NewRegistry returns an empty registry; its creation is the start time of the
// series read at collect.
func NewRegistry() *Registry {
	return &Registry{start: time.Now(), families: make(map[string]*family), promNames: make(map[string]bool)}
}

type family struct {
	def    Definition
	kind   Kind
	number Number
	// divisor: a counter counts integer sub-units and is collected as count ÷
	// divisor (0 for a plain counter, collected as an integer).
	divisor float64
	// cb is the callback that reads an instrument read at collect; observed marks
	// such an instrument.
	observed bool
	cb       *callback

	mu     sync.RWMutex
	series map[string]*series
}

type series struct {
	attrs []string
	start time.Time
	// count: a counter's value (in sub-units for a scaled one).
	count atomic.Uint64
	// level: an up-down counter's value.
	level atomic.Int64
	// bits: a gauge's value, or a histogram's sum, as float64 bits.
	bits atomic.Uint64
	// buckets: a histogram's per-bucket (not cumulative) counts, +Inf last.
	buckets []atomic.Uint64
}

// Counter is a monotonic counter recorded as events happen.
type Counter struct{ f *family }

// UpDownCounter is a counter that goes both ways, recorded as changes happen.
type UpDownCounter struct{ f *family }

// Gauge is a gauge set by its users.
type Gauge struct{ f *family }

// Histogram is a histogram with explicit bounds.
type Histogram struct{ f *family }

// Counter registers a counter collected as an integer.
func (r *Registry) Counter(d Definition) *Counter {
	return &Counter{r.register(d, KindCounter, Int, false)}
}

// ScaledCounter registers a counter that counts whole sub-units (e.g. nano-dollars)
// and is collected in base units, as a double count ÷ divisor: sums stay exact
// however many increments there are.
func (r *Registry) ScaledCounter(d Definition, divisor float64) *Counter {
	if !(divisor > 0) || math.IsInf(divisor, 0) {
		panic(fmt.Sprintf("metric: %s: divisor must be positive and finite", d.Name))
	}
	f := r.register(d, KindCounter, Double, false)
	f.divisor = divisor
	return &Counter{f}
}

// UpDownCounter registers an up-down counter.
func (r *Registry) UpDownCounter(d Definition) *UpDownCounter {
	return &UpDownCounter{r.register(d, KindUpDownCounter, Int, false)}
}

// Gauge registers a gauge set by its users.
func (r *Registry) Gauge(d Definition) *Gauge {
	return &Gauge{r.register(d, KindGauge, Double, false)}
}

// Histogram registers a histogram with d's buckets.
func (r *Registry) Histogram(d Definition) *Histogram {
	return &Histogram{r.register(d, KindHistogram, Double, false)}
}

func (r *Registry) register(d Definition, kind Kind, number Number, observed bool) *family {
	if !validName(d.Name) {
		panic(fmt.Sprintf("metric: invalid metric name %q", d.Name))
	}
	if !validUnit(d.Unit) {
		panic(fmt.Sprintf("metric: %s: invalid unit %q", d.Name, d.Unit))
	}
	labels := make([]string, 0, len(d.Attributes)+1)
	for _, key := range d.Attributes {
		label := prometheusLabel(key)
		if !validAttributeKey(key) || slices.Contains(labels, label) {
			panic(fmt.Sprintf("metric: %s: invalid or duplicate attribute key %q", d.Name, key))
		}
		labels = append(labels, label)
	}
	if kind != KindHistogram && d.Buckets != nil {
		panic(fmt.Sprintf("metric: %s: buckets on a %s", d.Name, kind))
	}
	if kind == KindHistogram {
		if slices.Contains(labels, "le") {
			panic(fmt.Sprintf("metric: %s: a histogram cannot have an attribute written as le", d.Name))
		}
		for i, b := range d.Buckets {
			if math.IsNaN(b) || math.IsInf(b, 0) || (i > 0 && b <= d.Buckets[i-1]) {
				panic(fmt.Sprintf("metric: %s: buckets must be finite and increasing", d.Name))
			}
		}
	}
	d.Attributes = slices.Clone(d.Attributes)
	d.Buckets = slices.Clone(d.Buckets)
	f := &family{def: d, kind: kind, number: number, observed: observed, series: make(map[string]*series)}
	promName := prometheusName(d.Name, d.Unit, kind)
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.families[d.Name]; dup {
		panic(fmt.Sprintf("metric: %s registered twice", d.Name))
	}
	if r.promNames[promName] {
		panic(fmt.Sprintf("metric: %s: its Prometheus name %s is taken", d.Name, promName))
	}
	r.families[d.Name] = f
	r.promNames[promName] = true
	return f
}

// Add adds n to the series with the given attribute values.
func (c *Counter) Add(n uint64, attrs ...string) {
	c.f.get(attrs).count.Add(n)
}

// Inc adds one to the series with the given attribute values.
func (c *Counter) Inc(attrs ...string) {
	c.f.get(attrs).count.Add(1)
}

// Add adds delta, which may be negative, to the series with the given attribute
// values.
func (c *UpDownCounter) Add(delta int64, attrs ...string) {
	c.f.get(attrs).level.Add(delta)
}

// Set sets the series with the given attribute values to v.
func (g *Gauge) Set(v float64, attrs ...string) {
	g.f.get(attrs).bits.Store(math.Float64bits(v))
}

// Prepare creates the series with the given attribute values, with no observations,
// if it does not exist yet.
func (h *Histogram) Prepare(attrs ...string) {
	h.f.get(attrs)
}

// Observe records one observation of v in the series with the given attribute
// values.
func (h *Histogram) Observe(v float64, attrs ...string) {
	s := h.f.get(attrs)
	i, _ := slices.BinarySearch(h.f.def.Buckets, v) // first bound >= v: a bound is inclusive
	s.buckets[i].Add(1)
	for {
		old := s.bits.Load()
		if s.bits.CompareAndSwap(old, math.Float64bits(math.Float64frombits(old)+v)) {
			return
		}
	}
}

// get returns the series for attrs, creating it on first use. The common case is a
// map lookup under a read lock.
func (f *family) get(attrs []string) *series {
	if len(attrs) != len(f.def.Attributes) {
		panic(fmt.Sprintf("metric: %s: %d attribute values for attributes %v", f.def.Name, len(attrs), f.def.Attributes))
	}
	key := strings.Join(attrs, "\xff")
	f.mu.RLock()
	s, ok := f.series[key]
	f.mu.RUnlock()
	if ok {
		return s
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if s, ok := f.series[key]; ok {
		return s
	}
	s = &series{attrs: slices.Clone(attrs), start: time.Now()}
	if f.kind == KindHistogram {
		s.buckets = make([]atomic.Uint64, len(f.def.Buckets)+1)
	}
	f.series[key] = s
	return s
}

func (f *family) checkAttrs(attrs []string) {
	if len(attrs) != len(f.def.Attributes) {
		panic(fmt.Sprintf("metric: %s: %d attribute values for attributes %v", f.def.Name, len(attrs), f.def.Attributes))
	}
}

func (k Kind) String() string {
	switch k {
	case KindCounter:
		return "counter"
	case KindUpDownCounter:
		return "up-down counter"
	case KindGauge:
		return "gauge"
	}
	return "histogram"
}

// validName is OpenTelemetry's instrument name syntax.
func validName(name string) bool {
	if name == "" || len(name) > 255 {
		return false
	}
	for i, c := range name {
		switch {
		case 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z':
		case i == 0:
			return false
		case '0' <= c && c <= '9' || c == '_' || c == '.' || c == '-' || c == '/':
		default:
			return false
		}
	}
	return true
}

// validUnit is OpenTelemetry's unit syntax, printable ASCII without spaces.
func validUnit(unit string) bool {
	if len(unit) > 63 {
		return false
	}
	for i := range len(unit) {
		if unit[i] < '!' || unit[i] > '~' {
			return false
		}
	}
	return true
}

// validAttributeKey is the semantic conventions' attribute key form.
func validAttributeKey(key string) bool {
	if key == "" {
		return false
	}
	for i, c := range key {
		switch {
		case 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z':
		case i == 0:
			return false
		case '0' <= c && c <= '9' || c == '_' || c == '.':
		default:
			return false
		}
	}
	return true
}
