package otlpmetric

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"kaiak/internal/telemetry/fakeotlp"
	"kaiak/internal/telemetry/metric"
	"kaiak/internal/telemetry/otlp"
)

func envOf(vars map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		v, ok := vars[name]
		return v, ok
	}
}

// syncBuffer collects the problem reports, written from the sender goroutine.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

// reports decodes the `metric export failing` lines written so far.
func (s *syncBuffer) reports(t *testing.T) []map[string]any {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	var lines []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(s.b.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("report line %q: %v", line, err)
		}
		if m["msg"] != "metric export failing" || m["level"] != "WARN" {
			t.Fatalf("report line %q: want a WARN `metric export failing`", line)
		}
		lines = append(lines, m)
	}
	return lines
}

// fakeClock is the reports' clock.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// harness is an exporter of reg posting to a fake collector, with a manual tick and
// its reports in a buffer.
type harness struct {
	e       *Exporter
	col     *fakeotlp.Collector
	reg     *metric.Registry
	tick    chan time.Time
	reports *syncBuffer
	clock   *fakeClock
}

func newHarness(t *testing.T, col *fakeotlp.Collector, reg *metric.Registry, vars map[string]string, opts options) *harness {
	t.Helper()
	if vars == nil {
		vars = map[string]string{}
	}
	vars["OTEL_EXPORTER_OTLP_ENDPOINT"] = col.URL
	s, err := otlp.ReadSettings(otlp.Metrics, envOf(vars))
	if err != nil || s == nil {
		t.Fatalf("ReadSettings = %v, %v; want export on", s, err)
	}
	h := &harness{col: col, reg: reg, tick: make(chan time.Time), reports: &syncBuffer{},
		clock: &fakeClock{now: time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)}}
	if opts.tick == nil {
		opts.tick = h.tick
	}
	opts.now = h.clock.Now
	h.e = newExporter(s, otlp.Service{Version: "1.2.3", InstanceID: "gw-1"}, reg,
		slog.New(slog.NewJSONHandler(h.reports, nil)), opts)
	t.Cleanup(func() { h.e.Shutdown(context.Background()) })
	return h
}

// flush runs an export now and returns once it has ended.
func (h *harness) flush(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := h.e.ForceFlush(ctx); err != nil {
		t.Fatalf("ForceFlush: %v", err)
	}
}

// export runs an export now and decodes the requests it made.
func (h *harness) export(t *testing.T) exported {
	t.Helper()
	before := h.col.Requests()
	h.flush(t)
	var out exported
	for range h.col.Requests() - before {
		out.add(t, h.col.Next(t))
	}
	return out
}

func wantCounts(t *testing.T, e *Exporter, want Counts) {
	t.Helper()
	if got := e.Counts(); !reflect.DeepEqual(got, want) {
		t.Fatalf("counts %+v, want %+v", got, want)
	}
}

// The test side's reading of OTLP JSON metrics: what a backend makes of an export,
// in terms a registry's collect can be compared with.

type gotMetric struct {
	Name, Unit, Description string
	// Kind is the registry's kind as the export expresses it: a monotonic sum is a
	// counter, another sum an up-down counter.
	Kind        metric.Kind
	Temporality int
	Points      []gotPoint
}

type gotPoint struct {
	// Attributes are the point's attributes, an intValue as its decimal string; nil
	// when it carries none. IntAttributes are the keys sent as intValue, sorted.
	Attributes    map[string]string
	IntAttributes []string
	// Start and Time are Unix nanoseconds; 0 when absent.
	Start, Time int64
	// Int, Double: a sum's or gauge's value, IsInt saying which it was sent as.
	IsInt     bool
	Int       int64
	Double    float64
	Histogram *gotHistogram
}

type gotHistogram struct {
	BucketCounts []uint64
	Bounds       []float64
	Count        uint64
	Sum          float64
}

// exported is what one export delivered: its metrics by name, the points of a
// metric split across requests joined, and each request's metrics in order.
type exported struct {
	metrics  map[string]*gotMetric
	requests []fakeotlp.Received
}

func (x *exported) add(t *testing.T, r fakeotlp.Received) {
	t.Helper()
	if x.metrics == nil {
		x.metrics = map[string]*gotMetric{}
	}
	x.requests = append(x.requests, r)
	if len(r.Export.ResourceLogs) != 0 || len(r.Export.ResourceMetrics) != 1 || len(r.Export.ResourceMetrics[0].ScopeMetrics) != 1 {
		t.Fatalf("export %+v: want one resource with one scope of metrics", r.Export)
	}
	if name := r.Export.ResourceMetrics[0].ScopeMetrics[0].Scope.Name; name != "kaiak" {
		t.Fatalf("scope %q, want kaiak", name)
	}
	for _, m := range r.Metrics() {
		g := decodeMetric(t, m)
		if prev, ok := x.metrics[m.Name]; ok {
			g.Points = append(prev.Points, g.Points...)
			if prev.Kind != g.Kind || prev.Temporality != g.Temporality || prev.Unit != g.Unit {
				t.Fatalf("%s: parts disagree: %+v and %+v", m.Name, prev, g)
			}
		}
		x.metrics[m.Name] = &g
	}
}

func decodeMetric(t *testing.T, m fakeotlp.Metric) gotMetric {
	t.Helper()
	g := gotMetric{Name: m.Name, Unit: m.Unit, Description: m.Description}
	set := 0
	var numbers []fakeotlp.NumberDataPoint
	if m.Sum != nil {
		set++
		g.Kind, g.Temporality, numbers = metric.KindUpDownCounter, m.Sum.AggregationTemporality, m.Sum.DataPoints
		if m.Sum.IsMonotonic {
			g.Kind = metric.KindCounter
		}
	}
	if m.Gauge != nil {
		set++
		g.Kind, numbers = metric.KindGauge, m.Gauge.DataPoints
	}
	if m.Histogram != nil {
		set++
		g.Kind, g.Temporality = metric.KindHistogram, m.Histogram.AggregationTemporality
		for _, p := range m.Histogram.DataPoints {
			gp := gotPoint{Start: nanos(t, p.StartTimeUnixNano), Time: nanos(t, p.TimeUnixNano),
				Histogram: &gotHistogram{Bounds: p.ExplicitBounds, Count: uint64s(t, p.Count)[0]}}
			gp.Attributes, gp.IntAttributes = attributes(t, p.Attributes)
			if p.Sum == nil {
				t.Fatalf("%s: a histogram point without its sum", m.Name)
			}
			gp.Histogram.Sum = *p.Sum
			gp.Histogram.BucketCounts = uint64s(t, p.BucketCounts...)
			if len(gp.Histogram.BucketCounts) != len(gp.Histogram.Bounds)+1 {
				t.Fatalf("%s: %d buckets for %d bounds", m.Name, len(gp.Histogram.BucketCounts), len(gp.Histogram.Bounds))
			}
			g.Points = append(g.Points, gp)
		}
	}
	if set != 1 {
		t.Fatalf("%s: %d of sum, gauge and histogram set, want 1", m.Name, set)
	}
	for _, p := range numbers {
		gp := gotPoint{Start: nanos(t, p.StartTimeUnixNano), Time: nanos(t, p.TimeUnixNano)}
		gp.Attributes, gp.IntAttributes = attributes(t, p.Attributes)
		switch {
		case p.AsInt != nil && p.AsDouble == nil:
			gp.IsInt = true
			v, err := strconv.ParseInt(*p.AsInt, 10, 64)
			if err != nil {
				t.Fatalf("%s: asInt %q: %v", m.Name, *p.AsInt, err)
			}
			gp.Int = v
		case p.AsDouble != nil && p.AsInt == nil:
			gp.Double = *p.AsDouble
		default:
			t.Fatalf("%s: a point with both or neither of asInt and asDouble", m.Name)
		}
		g.Points = append(g.Points, gp)
	}
	return g
}

// attributes reads string and int attributes: their values (an int as its decimal
// string) and the keys sent as ints, sorted.
func attributes(t *testing.T, kvs []fakeotlp.KeyValue) (values map[string]string, ints []string) {
	t.Helper()
	if len(kvs) == 0 {
		return nil, nil
	}
	values = map[string]string{}
	for _, kv := range kvs {
		switch v := kv.Value; {
		case v.StringValue != nil && v.IntValue == nil:
			values[kv.Key] = *v.StringValue
		case v.IntValue != nil && v.StringValue == nil:
			n, err := strconv.ParseInt(*v.IntValue, 10, 64)
			if err != nil {
				t.Fatalf("attribute %s: intValue %q: %v", kv.Key, *v.IntValue, err)
			}
			values[kv.Key] = strconv.FormatInt(n, 10)
			ints = append(ints, kv.Key)
		default:
			t.Fatalf("attribute %s is neither a string nor an int", kv.Key)
		}
	}
	slices.Sort(ints)
	return values, ints
}

func nanos(t *testing.T, s string) int64 {
	t.Helper()
	if s == "" {
		return 0
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		t.Fatalf("timestamp %q: %v", s, err)
	}
	return n
}

func uint64s(t *testing.T, ss ...string) []uint64 {
	t.Helper()
	out := make([]uint64, len(ss))
	for i, s := range ss {
		n, err := strconv.ParseUint(s, 10, 64)
		if err != nil {
			t.Fatalf("count %q: %v", s, err)
		}
		out[i] = n
	}
	return out
}

// want is what a collect should read as once exported: every family with series,
// each point starting at its series' start, every value as collected, at the given
// temporality per family (the values compared are the collect's, so a delta
// export's are compared after its first export).
func want(snap metric.Snapshot, temporality func(metric.Family) int) map[string]*gotMetric {
	out := map[string]*gotMetric{}
	for _, f := range snap.Families {
		if len(f.Points) == 0 {
			continue
		}
		g := &gotMetric{Name: f.Name, Unit: f.Unit, Description: f.Description, Kind: f.Kind, Temporality: temporality(f)}
		for _, p := range f.Points {
			gp := gotPoint{Time: snap.Time.UnixNano()}
			for i, v := range p.Attributes {
				if v != "" {
					if gp.Attributes == nil {
						gp.Attributes = map[string]string{}
					}
					gp.Attributes[f.Attributes[i]] = v
					if f.AttributeTypes[f.Attributes[i]] == metric.IntAttribute {
						gp.IntAttributes = append(gp.IntAttributes, f.Attributes[i])
					}
				}
			}
			slices.Sort(gp.IntAttributes)
			if f.Kind != metric.KindGauge {
				gp.Start = p.StartTime.UnixNano()
			}
			switch {
			case f.Kind == metric.KindHistogram:
				gp.Histogram = &gotHistogram{BucketCounts: p.Histogram.BucketCounts, Bounds: f.Buckets,
					Count: p.Histogram.Count, Sum: p.Histogram.Sum}
				if gp.Histogram.Bounds == nil {
					gp.Histogram.Bounds = []float64{}
				}
			case f.Number == metric.Int:
				gp.IsInt, gp.Int = true, p.Int
			default:
				gp.Double = p.Double
			}
			g.Points = append(g.Points, gp)
		}
		out[f.Name] = g
	}
	return out
}

// time checks that every point of an export carries one time, and returns it.
func (x exported) time(t *testing.T) int64 {
	t.Helper()
	var at int64
	for _, m := range x.metrics {
		for _, p := range m.Points {
			if at == 0 {
				at = p.Time
			}
			if p.Time != at || at == 0 {
				t.Fatalf("%s: point time %d, want every point at the collect's time %d", m.Name, p.Time, at)
			}
		}
	}
	return at
}

// withTime sets every wanted point's time to at.
func withTime(w map[string]*gotMetric, at int64) map[string]*gotMetric {
	for _, m := range w {
		for i := range m.Points {
			m.Points[i].Time = at
		}
	}
	return w
}

func compare(t *testing.T, got, want map[string]*gotMetric) {
	t.Helper()
	for name, w := range want {
		g, ok := got[name]
		if !ok {
			t.Errorf("%s: not exported", name)
			continue
		}
		if !reflect.DeepEqual(g, w) {
			t.Errorf("%s:\n got %s\nwant %s", name, dump(g), dump(w))
		}
	}
	for name := range got {
		if _, ok := want[name]; !ok {
			t.Errorf("%s: exported, not in the registry's collect", name)
		}
	}
}

func dump(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// everyKind registers one instrument of each kind and number, recorded and read at
// collect, with points that carry attributes, an empty one among them.
func everyKind() *metric.Registry {
	reg := metric.NewRegistry()
	requests := reg.Counter(metric.Definition{Name: "test.requests", Unit: "{request}", Description: "Requests served.",
		Attributes:     []string{"http.route", "http.response.status_code", "error.type"},
		AttributeTypes: map[string]metric.AttributeType{"http.response.status_code": metric.IntAttribute}})
	requests.Add(3, "/a", "200", "")
	requests.Inc("/a", "504", "timeout")
	requests.Inc("/b", "", "client_closed")
	cost := reg.ScaledCounter(metric.Definition{Name: "test.cost", Unit: "{USD}", Description: "Cost."}, 1e9)
	cost.Add(1_500_000_000)
	queue := reg.UpDownCounter(metric.Definition{Name: "test.queue.size", Unit: "{request}", Description: "Queued.",
		Attributes: []string{"model"}})
	queue.Add(5, "m1")
	queue.Add(-2, "m1")
	queue.Add(0, "m2")
	reg.Gauge(metric.Definition{Name: "test.config.size", Unit: "By", Description: "Config size."}).Set(1024.5)
	duration := reg.Histogram(metric.Definition{Name: "test.duration", Unit: "s", Description: "Durations.",
		Attributes: []string{"model"}, Buckets: []float64{0.1, 1}})
	duration.Observe(0.05, "m1")
	duration.Observe(0.5, "m1")
	duration.Observe(5, "m1")
	duration.Prepare("m2")
	reg.Histogram(metric.Definition{Name: "test.unused", Unit: "s", Description: "Never observed.", Buckets: []float64{1}})
	seen := reg.ObservableCounter(metric.Definition{Name: "test.seen", Unit: "{item}", Description: "Seen elsewhere.",
		Attributes: []string{"error.type"}})
	active := reg.ObservableUpDownCounter(metric.Definition{Name: "test.active", Unit: "{request}", Description: "Active."})
	up := reg.ObservableGauge(metric.Definition{Name: "test.up", Description: "Up."})
	reg.Callback(func(o *metric.Observer) {
		seen.Observe(o, 7, "")
		seen.Observe(o, 2, "503")
		active.Observe(o, -1)
		up.Observe(o, 1)
	}, seen, active, up)
	return reg
}

// Decoding an export yields every family of the registry that has series — its
// name, unit, description, kind and attributes, each point's start and value as
// collected — under the resource and scope every signal shares.
func TestExportRoundTripsEveryFamily(t *testing.T) {
	col := fakeotlp.New(t, nil)
	reg := everyKind()
	h := newHarness(t, col, reg, map[string]string{"OTEL_RESOURCE_ATTRIBUTES": "deployment.environment.name=test"}, options{})
	x := h.export(t)
	if len(x.requests) != 1 {
		t.Fatalf("%d requests, want 1", len(x.requests))
	}
	r := x.requests[0]
	if ct := r.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type %q", ct)
	}
	if ua := r.Header.Get("User-Agent"); ua != "kaiak/1.2.3" {
		t.Errorf("User-Agent %q", ua)
	}
	res, ints := attributes(t, r.Export.ResourceMetrics[0].Resource.Attributes)
	if ints != nil || !reflect.DeepEqual(res, map[string]string{"service.name": "kaiak", "service.version": "1.2.3",
		"service.instance.id": "gw-1", "deployment.environment.name": "test"}) {
		t.Errorf("resource %v", res)
	}
	at := x.time(t)
	compare(t, x.metrics, withTime(want(reg.Collect(), func(f metric.Family) int {
		if f.Kind == metric.KindGauge {
			return 0
		}
		return 2
	}), at))
	if _, ok := x.metrics["test.unused"]; ok {
		t.Error("a family with no series was exported")
	}
	if g := x.metrics["test.cost"]; g.Points[0].IsInt || g.Points[0].Double != 1.5 {
		t.Errorf("cost %+v, want asDouble 1.5", g.Points[0])
	}
	if g := x.metrics["test.requests"]; !g.Points[0].IsInt || g.Points[0].Attributes["error.type"] != "" || len(g.Points[0].Attributes) != 2 {
		t.Errorf("requests %+v, want asInt and the empty attribute left out", g.Points[0])
	}
}

// An attribute typed int goes out as an intValue (a decimal string), as the log
// export writes the same attribute; the others as stringValue; an empty one not at
// all.
func TestIntAttributeIsIntValue(t *testing.T) {
	col := fakeotlp.New(t, nil)
	h := newHarness(t, col, everyKind(), nil, options{})
	h.flush(t)
	var points []fakeotlp.NumberDataPoint
	for _, m := range col.Next(t).Metrics() {
		if m.Name == "test.requests" {
			points = m.Sum.DataPoints
		}
	}
	if len(points) != 3 {
		t.Fatalf("test.requests: %d points, want 3", len(points))
	}
	type typed struct{ str, int *string }
	byKey := func(p fakeotlp.NumberDataPoint) map[string]typed {
		out := map[string]typed{}
		for _, kv := range p.Attributes {
			out[kv.Key] = typed{kv.Value.StringValue, kv.Value.IntValue}
		}
		return out
	}
	for i, wantCode := range []string{"200", "504"} {
		code := byKey(points[i])["http.response.status_code"]
		if code.str != nil || code.int == nil || *code.int != wantCode {
			t.Errorf("point %d: status code %+v, want intValue %q", i, code, wantCode)
		}
		if route := byKey(points[i])["http.route"]; route.str == nil || route.int != nil || *route.str != "/a" {
			t.Errorf("point %d: route %+v, want stringValue /a", i, route)
		}
	}
	if _, ok := byKey(points[2])["http.response.status_code"]; ok {
		t.Errorf("point 2 carries the empty status code: %+v", points[2].Attributes)
	}
}

// The path is the metrics signal's.
func TestExportGoesToTheMetricsPath(t *testing.T) {
	paths := make(chan string, 1)
	col := fakeotlp.New(t, func(w http.ResponseWriter, r *http.Request, _ int) { paths <- r.URL.Path })
	h := newHarness(t, col, everyKind(), nil, options{})
	h.flush(t)
	if p := <-paths; p != "/v1/metrics" {
		t.Fatalf("posted to %s, want /v1/metrics", p)
	}
}

// Each preference maps each kind as the specification defines it.
func TestTemporalityByKind(t *testing.T) {
	families := []string{"test.requests", "test.cost", "test.seen", "test.queue.size", "test.active", "test.duration", "test.config.size", "test.up"}
	for _, tc := range []struct {
		pref string
		want []int // in the order of families
	}{
		{"cumulative", []int{2, 2, 2, 2, 2, 2, 0, 0}},
		{"delta", []int{1, 1, 1, 2, 2, 1, 0, 0}},
		{"lowmemory", []int{1, 1, 2, 2, 2, 1, 0, 0}},
	} {
		t.Run(tc.pref, func(t *testing.T) {
			col := fakeotlp.New(t, nil)
			h := newHarness(t, col, everyKind(), map[string]string{"OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE": tc.pref}, options{})
			x := h.export(t)
			for i, name := range families {
				if got := x.metrics[name].Temporality; got != tc.want[i] {
					t.Errorf("%s: temporality %d, want %d", name, got, tc.want[i])
				}
			}
		})
	}
}

// A cumulative stream starts every point at its series' start, the same at every
// export: the process's start for a series read at collect, its creation for a
// recorded one, a late series' included.
func TestCumulativeStartTimes(t *testing.T) {
	col := fakeotlp.New(t, nil)
	reg := everyKind()
	h := newHarness(t, col, reg, nil, options{})
	first := h.export(t)
	time.Sleep(2 * time.Millisecond)
	late := reg.Counter(metric.Definition{Name: "test.late", Unit: "{item}"})
	late.Inc()
	second := h.export(t)
	for name, m := range first.metrics {
		for i, p := range m.Points {
			if q := second.metrics[name].Points[i]; q.Start != p.Start {
				t.Errorf("%s: start moved from %d to %d", name, p.Start, q.Start)
			}
		}
	}
	firstAt := first.time(t)
	if s := second.metrics["test.late"].Points[0].Start; s <= firstAt {
		t.Errorf("late series starts at %d, want its creation, after the first export (%d)", s, firstAt)
	}
	if s, r := first.metrics["test.seen"].Points[0].Start, first.metrics["test.requests"].Points[0].Start; s > r {
		t.Errorf("a series read at collect starts at %d, after a recorded one's creation (%d)", s, r)
	}
}

// A delta stream sends every series every time, an unchanged one as 0; each delta
// starts at the previous export, or at the series' start for its first; histograms
// go by bucket, count and sum.
func TestDeltaStream(t *testing.T) {
	col := fakeotlp.New(t, nil)
	reg := metric.NewRegistry()
	c := reg.Counter(metric.Definition{Name: "test.c", Attributes: []string{"k"}})
	cost := reg.ScaledCounter(metric.Definition{Name: "test.cost"}, 1e9)
	hist := reg.Histogram(metric.Definition{Name: "test.h", Buckets: []float64{1, 10}})
	updown := reg.UpDownCounter(metric.Definition{Name: "test.ud"})
	c.Add(0, "zero")
	c.Add(5, "a")
	cost.Add(250_000_000)
	hist.Observe(0.5)
	hist.Observe(5)
	updown.Add(4)
	h := newHarness(t, col, reg, map[string]string{"OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE": "delta"}, options{})

	x1 := h.export(t)
	t1 := x1.time(t)
	pts := x1.metrics["test.c"].Points
	if len(pts) != 2 || pts[0].Int != 5 || pts[1].Int != 0 {
		t.Fatalf("first export %s, want a=5 and zero=0", dump(pts))
	}
	snap := reg.Collect()
	if want := snap.Families[0].Points[0].StartTime.UnixNano(); pts[0].Start != want || x1.metrics["test.c"].Temporality != 1 {
		t.Fatalf("first delta starts at %d, want the series' start %d, delta", pts[0].Start, want)
	}

	c.Add(3, "a")
	cost.Add(500_000_000)
	hist.Observe(50)
	hist.Observe(0.25)
	updown.Add(-1)
	x2 := h.export(t)
	t2 := x2.time(t)
	pts = x2.metrics["test.c"].Points
	if len(pts) != 2 || pts[0].Int != 3 || pts[1].Int != 0 || pts[0].Start != t1 || pts[1].Start != t1 {
		t.Fatalf("second export %s, want a=3, zero=0, both from the first export (%d)", dump(pts), t1)
	}
	if p := x2.metrics["test.cost"].Points[0]; p.Double != 0.5 || p.IsInt {
		t.Fatalf("cost delta %+v, want 0.5", p)
	}
	if hp := x2.metrics["test.h"].Points[0]; !reflect.DeepEqual(hp.Histogram,
		&gotHistogram{BucketCounts: []uint64{1, 0, 1}, Bounds: []float64{1, 10}, Count: 2, Sum: 50.25}) || hp.Start != t1 {
		t.Fatalf("histogram delta %s", dump(hp))
	}
	if p := x2.metrics["test.ud"].Points[0]; p.Int != 3 || x2.metrics["test.ud"].Temporality != 2 {
		t.Fatalf("up-down counter %+v, want cumulative 3", p)
	}

	time.Sleep(2 * time.Millisecond)
	c.Inc("late")
	x3 := h.export(t)
	pts = x3.metrics["test.c"].Points
	if len(pts) != 3 || pts[0].Int != 0 || pts[0].Start != t2 || pts[1].Int != 1 || pts[1].Start <= t2 || pts[2].Int != 0 {
		t.Fatalf("third export %s, want a=0 from %d, late=1 from its creation, zero=0", dump(pts), t2)
	}
	if hp := x3.metrics["test.h"].Points[0]; !reflect.DeepEqual(hp.Histogram.BucketCounts, []uint64{0, 0, 0}) || hp.Histogram.Count != 0 || hp.Histogram.Sum != 0 {
		t.Fatalf("unchanged histogram %s, want zeros", dump(hp))
	}
}

// A delta export that fails is not sent again: its points are counted failed and
// its deltas lost, so what was delivered sums to the cumulative value less the
// failed export's change; the next delta starts at the failed export.
func TestDeltaLostWhenAnExportFails(t *testing.T) {
	col := fakeotlp.New(t, fakeotlp.AnswerStatus(func(n int, _ *http.Request) int {
		if n == 1 {
			return http.StatusBadRequest
		}
		return http.StatusOK
	}))
	reg := metric.NewRegistry()
	c := reg.Counter(metric.Definition{Name: "test.c"})
	h := newHarness(t, col, reg, map[string]string{"OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE": "delta"}, options{})

	c.Add(5)
	delivered := h.export(t).metrics["test.c"].Points[0].Int
	c.Add(7)
	failedExport := h.export(t)
	lost := failedExport.metrics["test.c"].Points[0].Int
	tFailed := failedExport.time(t)
	c.Add(2)
	third := h.export(t).metrics["test.c"].Points[0]
	delivered += third.Int

	if lost != 7 || delivered != 7 || delivered+lost != 14 {
		t.Fatalf("delivered %d, lost %d; want 7 delivered (5 + 2) and the failed export's 7 lost, of 14", delivered, lost)
	}
	if third.Start != tFailed {
		t.Fatalf("delta after the failure starts at %d, want the failed export's time %d", third.Start, tFailed)
	}
	wantCounts(t, h.e, Counts{Exported: 2, Failed: map[string]uint64{"400": 1}})
}

// A cumulative stream carries the value of a failed export into the next one.
func TestCumulativeCarriesAFailedExport(t *testing.T) {
	col := fakeotlp.New(t, fakeotlp.AnswerStatus(func(n int, _ *http.Request) int {
		if n == 0 {
			return http.StatusBadRequest
		}
		return http.StatusOK
	}))
	reg := metric.NewRegistry()
	c := reg.Counter(metric.Definition{Name: "test.c"})
	h := newHarness(t, col, reg, nil, options{})
	c.Add(5)
	h.export(t)
	c.Add(2)
	if v := h.export(t).metrics["test.c"].Points[0].Int; v != 7 {
		t.Fatalf("after a failed export %d, want the cumulative 7", v)
	}
}

// A scaled counter's deltas are computed on its whole sub-units and divided once:
// each delta is exactly its change in sub-units ÷ the divisor, and the deltas'
// sub-units sum to the cumulative count, however many tiny increments there were.
func TestScaledCounterDeltasAreExact(t *testing.T) {
	col := fakeotlp.New(t, nil)
	reg := metric.NewRegistry()
	cost := reg.ScaledCounter(metric.Definition{Name: "test.cost", Unit: "{USD}"}, 1e9)
	h := newHarness(t, col, reg, map[string]string{"OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE": "delta"}, options{})

	cost.Add(123_456_789_012) // a large running total, so double subtraction would round
	var total, sum uint64 = 123_456_789_012, 0
	first := h.export(t).metrics["test.cost"].Points[0]
	if first.Double != float64(total)/1e9 {
		t.Fatalf("first delta %v, want %v", first.Double, float64(total)/1e9)
	}
	sum += total
	for export := range 20 {
		var change uint64
		for i := range 1000 {
			n := uint64(1 + (export*1000+i)%7) // nano-dollars at a time
			cost.Add(n)
			change += n
		}
		total += change
		got := h.export(t).metrics["test.cost"].Points[0]
		if got.IsInt || got.Double != float64(change)/1e9 {
			t.Fatalf("export %d: delta %v, want exactly %d sub-units ÷ 1e9 = %v", export+2, got.Double, change, float64(change)/1e9)
		}
		sum += change
	}
	if snapTotal := reg.Collect().Families[0].Points[0].SubUnits; sum != total || snapTotal != total {
		t.Fatalf("deltas sum to %d sub-units, cumulative %d (collected %d)", sum, total, snapTotal)
	}
}

// Under lowmemory a counter read at collect stays cumulative, a recorded one goes
// delta.
func TestLowMemoryKeepsObservedCountersCumulative(t *testing.T) {
	col := fakeotlp.New(t, nil)
	reg := metric.NewRegistry()
	recorded := reg.Counter(metric.Definition{Name: "test.recorded"})
	observed := reg.ObservableCounter(metric.Definition{Name: "test.observed"})
	var n uint64
	reg.Callback(func(o *metric.Observer) { observed.Observe(o, n) }, observed)
	h := newHarness(t, col, reg, map[string]string{"OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE": "lowmemory"}, options{})
	recorded.Add(4)
	n = 4
	h.export(t)
	recorded.Add(1)
	n = 5
	x := h.export(t)
	if r, o := x.metrics["test.recorded"].Points[0].Int, x.metrics["test.observed"].Points[0].Int; r != 1 || o != 5 {
		t.Fatalf("recorded %d, observed %d; want the recorded delta 1 and the observed cumulative 5", r, o)
	}
}

// A value below the previous one is a series that started over: its whole value is
// the change. By construction the registry's counters never go back; the rule
// keeps a delta from wrapping round should one ever do.
func TestSubtractStartedOver(t *testing.T) {
	p := subtract(&metric.Family{Kind: metric.KindCounter}, metric.Point{Int: 2}, metric.Point{Int: 9})
	if p.Int != 2 {
		t.Fatalf("counter that went back: %d, want 2", p.Int)
	}
	h := subtract(&metric.Family{Kind: metric.KindHistogram},
		metric.Point{Histogram: metric.HistogramValue{BucketCounts: []uint64{1, 0}, Count: 1, Sum: 1}},
		metric.Point{Histogram: metric.HistogramValue{BucketCounts: []uint64{3, 2}, Count: 5, Sum: 9}})
	if h.Histogram.Count != 1 || h.Histogram.Sum != 1 || !slices.Equal(h.Histogram.BucketCounts, []uint64{1, 0}) {
		t.Fatalf("histogram that went back: %+v", h.Histogram)
	}
}

// The exporter's own counts, read at collect on the registry it exports, show an
// export's outcome in the next export.
func TestOwnCountsShowInTheNextExport(t *testing.T) {
	col := fakeotlp.New(t, nil)
	reg := metric.NewRegistry()
	reg.Counter(metric.Definition{Name: "test.c"}).Inc()
	h := newHarness(t, col, reg, nil, options{})
	exportedPoints := reg.ObservableCounter(metric.Definition{Name: "otel.sdk.exporter.metric_data_point.exported",
		Unit: "{data_point}", Attributes: []string{"error.type"}})
	reg.Callback(func(o *metric.Observer) {
		c := h.e.Counts()
		exportedPoints.Observe(o, c.Exported, "")
		for t, n := range c.Failed {
			exportedPoints.Observe(o, n, t)
		}
	}, exportedPoints)

	first := h.export(t).metrics["otel.sdk.exporter.metric_data_point.exported"].Points
	if len(first) != 1 || first[0].Int != 0 {
		t.Fatalf("first export's own count %s, want 0: its own outcome is not known yet", dump(first))
	}
	second := h.export(t).metrics["otel.sdk.exporter.metric_data_point.exported"].Points
	if len(second) != 1 || second[0].Int != 2 {
		t.Fatalf("second export's own count %s, want the first export's 2 points", dump(second))
	}
	wantCounts(t, h.e, Counts{Exported: 4})
}

// A partial success counts its rejected points failed, the rest exported, and is
// reported without the collector's text.
func TestPartialSuccess(t *testing.T) {
	col := fakeotlp.New(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		_, _ = w.Write([]byte(`{"partialSuccess":{"rejectedDataPoints":"3","errorMessage":"secret-bearing text"}}`))
	})
	h := newHarness(t, col, everyKind(), nil, options{})
	x := h.export(t)
	total := uint64(0)
	for _, m := range x.metrics {
		total += uint64(len(m.Points))
	}
	wantCounts(t, h.e, Counts{Exported: total - 3, Failed: map[string]uint64{"rejected": 3}})
	r := h.reports.reports(t)
	if len(r) != 1 || r[0]["kaiak.metric_export.failed"] != float64(3) || r[0]["exception.message"] != "collector rejected 3 data points" ||
		r[0]["http.response.status_code"] != float64(200) {
		t.Fatalf("reports %v, want one: 3 failed, status 200, the gateway's words", r)
	}
}

// stalledCollector answers no request until released or the request is cut.
func stalledCollector(t *testing.T) (*fakeotlp.Collector, func()) {
	release := make(chan struct{})
	var once sync.Once
	col := fakeotlp.New(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	releaseAll := func() { once.Do(func() { close(release) }) }
	t.Cleanup(releaseAll)
	return col, releaseAll
}

// One export is bounded by the shorter of the export timeout and the request
// timeout, retries included; its points then fail as a timeout.
func TestStalledCollectorIsBoundedByTheExportTimeout(t *testing.T) {
	for _, tc := range []struct {
		name string
		vars map[string]string
	}{
		{"export timeout shorter", map[string]string{"OTEL_METRIC_EXPORT_TIMEOUT": "200", "OTEL_EXPORTER_OTLP_METRICS_TIMEOUT": "20000"}},
		{"request timeout shorter", map[string]string{"OTEL_METRIC_EXPORT_TIMEOUT": "20000", "OTEL_EXPORTER_OTLP_METRICS_TIMEOUT": "200"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			col, _ := stalledCollector(t)
			reg := metric.NewRegistry()
			reg.Counter(metric.Definition{Name: "test.c"}).Inc()
			h := newHarness(t, col, reg, tc.vars, options{})
			start := time.Now()
			h.flush(t)
			if d := time.Since(start); d > 5*time.Second {
				t.Fatalf("export took %s, want about 200ms", d)
			}
			wantCounts(t, h.e, Counts{Failed: map[string]uint64{"timeout": 1}})
			if r := h.reports.reports(t); len(r) != 1 || r[0]["kaiak.metric_export.failed"] != float64(1) {
				t.Fatalf("reports %v, want one with the 1 point failed", r)
			}
		})
	}
}

// A tick due while an export runs is skipped, never queued behind it.
func TestTickDuringAnExportIsSkipped(t *testing.T) {
	col, release := stalledCollector(t)
	reg := metric.NewRegistry()
	reg.Counter(metric.Definition{Name: "test.c"}).Inc()
	h := newHarness(t, col, reg, nil, options{})
	h.tick <- time.Now()
	col.Next(t) // the export is in flight
	due := time.Now()
	release()
	h.tick <- due        // taken once the export has ended: skipped
	h.tick <- time.Now() // due after it: exported
	h.flush(t)
	if n := col.Requests(); n != 3 {
		t.Fatalf("%d exports, want 3: the tick due during the first export skipped", n)
	}
}

// Without a test tick the exporter runs on the settings' interval.
func TestExportsOnTheInterval(t *testing.T) {
	col := fakeotlp.New(t, nil)
	reg := metric.NewRegistry()
	reg.Counter(metric.Definition{Name: "test.c"}).Inc()
	s, err := otlp.ReadSettings(otlp.Metrics, envOf(map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": col.URL, "OTEL_METRIC_EXPORT_INTERVAL": "20"}))
	if err != nil {
		t.Fatal(err)
	}
	e := New(s, otlp.Service{Version: "1"}, reg, slog.New(slog.DiscardHandler))
	defer e.Shutdown(context.Background())
	col.Next(t)
	col.Next(t)
}

// An export too large for one request goes as several, each within the bound:
// whole metrics where they fit, a larger metric's points split; every point
// arrives once, and a request that fails fails its own points only.
func TestSizeSplit(t *testing.T) {
	const limit = 2048
	var mu sync.Mutex
	var sizes []int64
	col := fakeotlp.New(t, func(w http.ResponseWriter, r *http.Request, n int) {
		mu.Lock()
		sizes = append(sizes, r.ContentLength)
		mu.Unlock()
		if n == 1 {
			w.WriteHeader(http.StatusBadRequest)
		}
	})
	reg := metric.NewRegistry()
	for i := range 12 {
		reg.Counter(metric.Definition{Name: fmt.Sprintf("test.small%02d", i), Unit: "{item}", Description: "A small one."}).Inc()
	}
	large := reg.Counter(metric.Definition{Name: "test.large", Unit: "{item}", Description: "Many series.", Attributes: []string{"k"}})
	for i := range 100 {
		large.Add(uint64(i), fmt.Sprintf("key-%03d", i))
	}
	h := newHarness(t, col, reg, nil, options{maxRequestSize: limit})
	x := h.export(t)

	if len(x.requests) < 3 {
		t.Fatalf("%d requests, want the export split", len(x.requests))
	}
	mu.Lock()
	for _, s := range sizes {
		if s <= 0 || s > limit {
			t.Errorf("a request of %d bytes, want at most %d", s, limit)
		}
	}
	mu.Unlock()
	failedPoints := 0
	largeParts := 0
	for i, r := range x.requests {
		for _, m := range r.Metrics() {
			if m.Name == "test.large" {
				largeParts++
			} else if len(m.Sum.DataPoints) != 1 {
				t.Errorf("%s split", m.Name)
			}
			if i == 1 {
				failedPoints += len(m.Sum.DataPoints)
			}
		}
	}
	if largeParts < 2 {
		t.Errorf("the large metric in %d requests, want split", largeParts)
	}
	compare(t, x.metrics, withTime(want(reg.Collect(), func(metric.Family) int { return 2 }), x.time(t)))
	wantCounts(t, h.e, Counts{Exported: uint64(112 - failedPoints), Failed: map[string]uint64{"400": uint64(failedPoints)}})
}

// A collect with no series sends nothing.
func TestNothingToSend(t *testing.T) {
	col := fakeotlp.New(t, nil)
	reg := metric.NewRegistry()
	reg.Histogram(metric.Definition{Name: "test.h", Buckets: []float64{1}})
	h := newHarness(t, col, reg, nil, options{})
	h.flush(t)
	if n := col.Requests(); n != 0 {
		t.Fatalf("%d requests, want none", n)
	}
	wantCounts(t, h.e, Counts{})
}

func TestProblemReportsAreRateLimited(t *testing.T) {
	col := fakeotlp.New(t, func(w http.ResponseWriter, _ *http.Request, n int) {
		if n == 3 {
			w.Header().Set("Retry-After", "60")
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusBadRequest)
	})
	reg := metric.NewRegistry()
	reg.Counter(metric.Definition{Name: "test.c"}).Inc()
	h := newHarness(t, col, reg, nil, options{})

	h.flush(t)
	if r := h.reports.reports(t); len(r) != 1 || r[0]["kaiak.metric_export.failed"] != float64(1) ||
		r[0]["http.response.status_code"] != float64(400) || r[0]["exception.message"] != "collector answered 400 Bad Request" {
		t.Fatalf("reports %v, want one at the first failure", r)
	}
	h.clock.Advance(30 * time.Second)
	h.flush(t)
	h.clock.Advance(29 * time.Second)
	h.flush(t)
	if r := h.reports.reports(t); len(r) != 1 {
		t.Fatalf("reports %v, want still one within the minute", r)
	}
	h.clock.Advance(time.Second)
	h.flush(t)
	r := h.reports.reports(t)
	if len(r) != 2 || r[1]["kaiak.metric_export.failed"] != float64(3) || r[1]["http.response.status_code"] != float64(503) ||
		!strings.HasPrefix(r[1]["exception.message"].(string), "collector answered 503") {
		t.Fatalf("reports %v, want a second one a minute after the first: 3 failed, the last status and error", r)
	}
	if _, ok := r[0]["kaiak.log_export.failed"]; ok {
		t.Fatal("a metric report carries the log export's field")
	}
}

// The collector's own text never reaches a report.
func TestCollectorTextIsNeverReported(t *testing.T) {
	col := fakeotlp.New(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 16, "message": "invalid credential: " + r.Header.Get("Authorization")})
	})
	reg := metric.NewRegistry()
	reg.Counter(metric.Definition{Name: "test.c"}).Inc()
	h := newHarness(t, col, reg, map[string]string{"OTEL_EXPORTER_OTLP_HEADERS": "authorization=Bearer%20test-secret"}, options{})
	h.flush(t)
	r := h.reports.reports(t)
	if len(r) != 1 || r[0]["exception.message"] != "collector answered 401 Unauthorized" {
		t.Fatalf("reports %v, want the gateway's own words", r)
	}
}

// ForceFlush's context bounds the export itself, not only the wait: a flush past
// its deadline returns, its export ends with it — the points count as a timeout —
// and the next export runs at once rather than behind the stalled one.
func TestForceFlushEndsWithItsContext(t *testing.T) {
	col := fakeotlp.New(t, func(w http.ResponseWriter, r *http.Request, n int) {
		if n == 0 {
			<-r.Context().Done()
		}
	})
	reg := metric.NewRegistry()
	reg.Counter(metric.Definition{Name: "test.c"}).Inc()
	h := newHarness(t, col, reg, map[string]string{"OTEL_METRIC_EXPORT_TIMEOUT": "20000", "OTEL_EXPORTER_OTLP_METRICS_TIMEOUT": "20000"}, options{})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := h.e.ForceFlush(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ForceFlush = %v, want the deadline", err)
	}
	h.flush(t)
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("the second flush ended %s after the first began: the first export outlived its context", d)
	}
	wantCounts(t, h.e, Counts{Exported: 1, Failed: map[string]uint64{"timeout": 1}})
}

// Shutdown cuts the export in flight — its points count as failed — and its
// report is never held back, within the minute of the previous one.
func TestShutdownCutsTheExportAndReports(t *testing.T) {
	col := fakeotlp.New(t, func(w http.ResponseWriter, r *http.Request, n int) {
		if n == 0 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		<-r.Context().Done()
	})
	reg := metric.NewRegistry()
	reg.Counter(metric.Definition{Name: "test.c"}).Inc()
	h := newHarness(t, col, reg, nil, options{})
	h.flush(t)
	h.tick <- time.Now()
	col.Next(t)
	col.Next(t) // the second export is in flight
	h.e.Shutdown(context.Background())
	h.e.Shutdown(context.Background())
	wantCounts(t, h.e, Counts{Failed: map[string]uint64{"400": 1, "timeout": 1}})
	r := h.reports.reports(t)
	if len(r) != 2 || r[1]["kaiak.metric_export.failed"] != float64(1) {
		t.Fatalf("reports %v, want the cut export's own line at shutdown", r)
	}
	if err := h.e.ForceFlush(context.Background()); !errors.Is(err, errShutdown) {
		t.Fatalf("ForceFlush after Shutdown = %v, want errShutdown", err)
	}
	if n := col.Requests(); n != 2 {
		t.Fatalf("%d requests, want nothing after Shutdown", n)
	}
}
