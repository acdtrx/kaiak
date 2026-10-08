// Package metrics is the gateway's Prometheus surface: a small registry of counters,
// gauges and histograms, the text exposition writer the admin port serves, the ops
// metrics the request pipeline feeds, and the usage metrics accounting hands every
// settled record.
//
// Metrics are for dashboards: cheap, approximate, reset on restart. They are never the
// source of a usage record, and usage records are never rebuilt from them
// (docs/kaiak.md, principle 7).
package metrics

import (
	"fmt"
	"math"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
)

type kind int

const (
	kindCounter kind = iota
	kindGauge
	kindHistogram
)

func (k kind) String() string {
	switch k {
	case kindCounter:
		return "counter"
	case kindGauge:
		return "gauge"
	}
	return "histogram"
}

// Registry holds metric families and writes them in the text exposition format. The
// label names of a family are fixed when it is registered; every use must pass one
// value per label name. Registering an invalid or duplicate family, or using one with
// the wrong number of label values, is a programming error and panics.
//
// Series are created on first use — or ahead of it, at 0, by Add(0) or Prepare, so
// the first event shows as an increase — and live as long as the registry: counters
// never go back, as Prometheus expects of them.
type Registry struct {
	mu       sync.Mutex
	families map[string]*family
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{families: make(map[string]*family)}
}

type family struct {
	name   string
	help   string
	kind   kind
	labels []string
	// divisor: a counter counts integer sub-units and is written as count ÷ divisor
	// (1 for plain counters).
	divisor float64
	// buckets are a histogram's upper bounds, increasing; +Inf is implicit.
	buckets []float64
	// collect, when set, produces a gauge's or counter's samples at write time
	// instead of stored series; group, when set, produces them for the families
	// read together with it (GaugeFuncs).
	collect func(emit func(value float64, labelValues ...string))
	group   *gaugeGroup

	mu     sync.RWMutex
	series map[string]*series
}

type series struct {
	labelValues []string
	// count: a counter's value in sub-units.
	count atomic.Uint64
	// bits: a gauge's value, or a histogram's sum, as float64 bits.
	bits atomic.Uint64
	// buckets: a histogram's per-bucket (not cumulative) counts, +Inf last.
	buckets []atomic.Uint64
}

// CounterVec is a counter family.
type CounterVec struct{ f *family }

// GaugeVec is a gauge family whose values are set by its users.
type GaugeVec struct{ f *family }

// HistogramVec is a histogram family with fixed buckets.
type HistogramVec struct{ f *family }

// Counter registers a counter family. Its name should end in _total.
func (r *Registry) Counter(name, help string, labels ...string) *CounterVec {
	return &CounterVec{r.register(&family{name: name, help: help, kind: kindCounter, labels: labels, divisor: 1})}
}

// ScaledCounter registers a counter that counts whole sub-units (e.g. nano-dollars)
// and is written in base units, as count ÷ divisor: sums stay exact however many
// increments there are.
func (r *Registry) ScaledCounter(name, help string, divisor float64, labels ...string) *CounterVec {
	if !(divisor > 0) || math.IsInf(divisor, 0) {
		panic(fmt.Sprintf("metrics: %s: divisor must be positive and finite", name))
	}
	return &CounterVec{r.register(&family{name: name, help: help, kind: kindCounter, labels: labels, divisor: divisor})}
}

// Gauge registers a gauge family set by its users.
func (r *Registry) Gauge(name, help string, labels ...string) *GaugeVec {
	return &GaugeVec{r.register(&family{name: name, help: help, kind: kindGauge, labels: labels})}
}

// GaugeFunc registers a gauge read from live state at write time: collect calls emit
// once per series. It runs on the scraping goroutine and must not block.
func (r *Registry) GaugeFunc(name, help string, labels []string, collect func(emit func(value float64, labelValues ...string))) {
	r.register(&family{name: name, help: help, kind: kindGauge, labels: labels, collect: collect})
}

// GaugeDesc describes one gauge family of GaugeFuncs.
type GaugeDesc struct {
	Name, Help string
	Labels     []string
}

// gaugeGroup is gauge families read together: collect gets one emit per family, in
// the order of families.
type gaugeGroup struct {
	families []*family
	collect  func(emit []func(value float64, labelValues ...string))
}

// GaugeFuncs registers gauge families read together from live state at write time:
// collect runs once per write and gets one emit per family, in the order of gauges,
// each called once per series of its family — so the families of one write agree
// with each other. It runs on the scraping goroutine and must not block.
func (r *Registry) GaugeFuncs(gauges []GaugeDesc, collect func(emit []func(value float64, labelValues ...string))) {
	g := &gaugeGroup{collect: collect}
	for _, d := range gauges {
		g.families = append(g.families, r.register(&family{name: d.Name, help: d.Help, kind: kindGauge, labels: d.Labels, group: g}))
	}
}

// CounterFunc registers a counter read from a count kept elsewhere at write time:
// collect calls emit once per series. It runs on the scraping goroutine and must not
// block; the counts it reads must never go back.
func (r *Registry) CounterFunc(name, help string, labels []string, collect func(emit func(value float64, labelValues ...string))) {
	r.register(&family{name: name, help: help, kind: kindCounter, labels: labels, divisor: 1, collect: collect})
}

// Histogram registers a histogram family with the given bucket upper bounds
// (increasing, finite; +Inf is added).
func (r *Registry) Histogram(name, help string, buckets []float64, labels ...string) *HistogramVec {
	for i, b := range buckets {
		if math.IsNaN(b) || math.IsInf(b, 0) || (i > 0 && b <= buckets[i-1]) {
			panic(fmt.Sprintf("metrics: %s: buckets must be finite and increasing", name))
		}
	}
	if slices.Contains(labels, "le") {
		panic(fmt.Sprintf("metrics: %s: a histogram cannot have an le label", name))
	}
	return &HistogramVec{r.register(&family{name: name, help: help, kind: kindHistogram, labels: labels,
		buckets: slices.Clone(buckets)})}
}

func (r *Registry) register(f *family) *family {
	if !validMetricName(f.name) {
		panic(fmt.Sprintf("metrics: invalid metric name %q", f.name))
	}
	for i, l := range f.labels {
		if !validLabelName(l) || slices.Contains(f.labels[:i], l) {
			panic(fmt.Sprintf("metrics: %s: invalid or duplicate label name %q", f.name, l))
		}
	}
	f.labels = slices.Clone(f.labels)
	f.series = make(map[string]*series)
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.families[f.name]; dup {
		panic(fmt.Sprintf("metrics: %s registered twice", f.name))
	}
	r.families[f.name] = f
	return f
}

// Add adds n to the series with the given label values.
func (c *CounterVec) Add(n uint64, labelValues ...string) {
	c.f.get(labelValues).count.Add(n)
}

// Inc adds one to the series with the given label values.
func (c *CounterVec) Inc(labelValues ...string) {
	c.f.get(labelValues).count.Add(1)
}

// Set sets the series with the given label values to v.
func (g *GaugeVec) Set(v float64, labelValues ...string) {
	g.f.get(labelValues).bits.Store(math.Float64bits(v))
}

// Prepare creates the series with the given label values, with no observations, if
// it does not exist yet.
func (h *HistogramVec) Prepare(labelValues ...string) {
	h.f.get(labelValues)
}

// Observe records one observation of v in the series with the given label values.
func (h *HistogramVec) Observe(v float64, labelValues ...string) {
	s := h.f.get(labelValues)
	i, _ := slices.BinarySearch(h.f.buckets, v) // first bound >= v: le is inclusive
	s.buckets[i].Add(1)
	for {
		old := s.bits.Load()
		if s.bits.CompareAndSwap(old, math.Float64bits(math.Float64frombits(old)+v)) {
			return
		}
	}
}

// get returns the series for labelValues, creating it on first use. The common case
// is a map lookup under a read lock.
func (f *family) get(labelValues []string) *series {
	if len(labelValues) != len(f.labels) {
		panic(fmt.Sprintf("metrics: %s: %d label values for labels %v", f.name, len(labelValues), f.labels))
	}
	key := strings.Join(labelValues, "\xff")
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
	s = &series{labelValues: slices.Clone(labelValues)}
	if f.kind == kindHistogram {
		s.buckets = make([]atomic.Uint64, len(f.buckets)+1)
	}
	f.series[key] = s
	return s
}

func validMetricName(name string) bool {
	if name == "" {
		return false
	}
	for i, c := range name {
		switch {
		case c == '_' || c == ':' || 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z':
		case '0' <= c && c <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

func validLabelName(name string) bool {
	if name == "" || strings.HasPrefix(name, "__") {
		return false
	}
	for i, c := range name {
		switch {
		case c == '_' || 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z':
		case '0' <= c && c <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}
