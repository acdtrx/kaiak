package metric

import (
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func expectPanic(t *testing.T, what string, f func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Errorf("%s: no panic", what)
		}
	}()
	f()
}

func TestRegistrationAndUseAreValidated(t *testing.T) {
	reg := NewRegistry()
	c := reg.Counter(Definition{Name: "ok", Attributes: []string{"a", "b.c"}})
	def := func(name string, attrs ...string) Definition { return Definition{Name: name, Attributes: attrs} }
	expectPanic(t, "duplicate name", func() { reg.Gauge(def("ok")) })
	expectPanic(t, "duplicate Prometheus name", func() { reg.Counter(def("ok_total")) })
	expectPanic(t, "empty name", func() { reg.Counter(def("")) })
	expectPanic(t, "name starting with a digit", func() { reg.Counter(def("9lives")) })
	expectPanic(t, "name starting with _", func() { reg.Counter(def("_x")) })
	expectPanic(t, "name with a space", func() { reg.Counter(def("a b")) })
	expectPanic(t, "name with a colon", func() { reg.Counter(def("a:b")) })
	expectPanic(t, "name too long", func() { reg.Counter(def("a" + strings.Repeat("b", 255))) })
	reg.Counter(def("a" + strings.Repeat("b", 254)))
	reg.Counter(def("Mixed.Case_1-2/3"))
	expectPanic(t, "unit with a space", func() { reg.Gauge(Definition{Name: "u1", Unit: "k By"}) })
	expectPanic(t, "unit not ASCII", func() { reg.Gauge(Definition{Name: "u2", Unit: "µs"}) })
	expectPanic(t, "unit too long", func() { reg.Gauge(Definition{Name: "u3", Unit: strings.Repeat("x", 64)}) })
	expectPanic(t, "invalid attribute key", func() { reg.Counter(def("x", "bad-key")) })
	expectPanic(t, "attribute key starting with a digit", func() { reg.Counter(def("y", "1a")) })
	expectPanic(t, "duplicate attribute key", func() { reg.Counter(def("z", "a", "a")) })
	expectPanic(t, "attribute keys written as one label", func() { reg.Counter(def("w", "a.b", "a_b")) })
	expectPanic(t, "le on a histogram", func() { reg.Histogram(Definition{Name: "h1", Attributes: []string{"le"}}) })
	expectPanic(t, "unsorted buckets", func() { reg.Histogram(Definition{Name: "h2", Buckets: []float64{2, 1}}) })
	expectPanic(t, "buckets on a counter", func() { reg.Counter(Definition{Name: "h3", Buckets: []float64{1}}) })
	expectPanic(t, "zero divisor", func() { reg.ScaledCounter(def("s1"), 0) })
	expectPanic(t, "too few attribute values", func() { c.Inc("only-one") })
	expectPanic(t, "too many attribute values", func() { c.Inc("1", "2", "3") })
}

func TestCallbacksAreValidated(t *testing.T) {
	reg := NewRegistry()
	g := reg.ObservableGauge(Definition{Name: "g", Attributes: []string{"k"}})
	other := reg.ObservableCounter(Definition{Name: "other"})
	reg.Callback(func(o *Observer) { g.Observe(o, 1) }, g)
	expectPanic(t, "too few attribute values observed", func() { reg.Collect() })

	reg = NewRegistry()
	g = reg.ObservableGauge(Definition{Name: "g"})
	reg.Callback(func(*Observer) {}, g)
	expectPanic(t, "a second callback", func() { reg.Callback(func(*Observer) {}, g) })
	expectPanic(t, "another registry's instrument", func() { reg.Callback(func(*Observer) {}, other) })
	stray := reg.ObservableCounter(Definition{Name: "stray"})
	reg.Callback(func(o *Observer) { stray.Observe(o, 1) })
	expectPanic(t, "observed outside its callback", func() { reg.Collect() })
}

// Instruments of one callback are read once per collect, each written in its place
// by name.
func TestCallbackRunsOncePerCollect(t *testing.T) {
	reg := NewRegistry()
	reg.Gauge(Definition{Name: "m.middle", Description: "Between."}).Set(5)
	reads := 0
	last := reg.ObservableGauge(Definition{Name: "z.last", Description: "Last.", Attributes: []string{"k"}})
	first := reg.ObservableUpDownCounter(Definition{Name: "a.first", Description: "First."})
	reg.Callback(func(o *Observer) {
		reads++
		last.Observe(o, float64(reads), "x")
		first.Observe(o, int64(reads))
	}, last, first)
	want := `# HELP a_first First.
# TYPE a_first gauge
a_first 1
# HELP m_middle Between.
# TYPE m_middle gauge
m_middle 5
# HELP z_last Last.
# TYPE z_last gauge
z_last{k="x"} 1
`
	if got := text(reg); got != want || reads != 1 {
		t.Errorf("%d reads, got:\n%s\nwant:\n%s", reads, got, want)
	}
	if got := text(reg); !strings.Contains(got, "a_first 2\n") || !strings.Contains(got, `z_last{k="x"} 2`+"\n") || reads != 2 {
		t.Errorf("second collect, %d reads:\n%s", reads, got)
	}
}

// A collect in the data model: every kind with its number, start times — a recorded
// series' own creation, stable across collects; the registry's for one read at
// collect; none on a gauge — and a histogram's per-bucket counts.
func TestCollect(t *testing.T) {
	before := time.Now()
	reg := NewRegistry()
	counter := reg.Counter(Definition{Name: "c", Unit: "{request}", Description: "C.", Attributes: []string{"k"}})
	counter.Add(2, "early")
	cost := reg.ScaledCounter(Definition{Name: "cost", Unit: "{USD}"}, 1e9)
	cost.Add(250_000_000)
	level := reg.UpDownCounter(Definition{Name: "level", Unit: "By"})
	level.Add(-3)
	gauge := reg.Gauge(Definition{Name: "g", Unit: "s"})
	gauge.Set(1.25)
	hist := reg.Histogram(Definition{Name: "h", Unit: "s", Buckets: []float64{1, 2}})
	hist.Observe(0.5)
	hist.Observe(1.5)
	hist.Observe(5)
	hist.Observe(0.25)
	observed := reg.ObservableCounter(Definition{Name: "observed", Attributes: []string{"k"}})
	reg.Callback(func(o *Observer) { observed.Observe(o, 7, "late") }, observed)
	created := time.Now()
	counter.Inc("late")

	snap := reg.Collect()
	if snap.Time.Before(created) {
		t.Errorf("collect time %v before the last recording", snap.Time)
	}
	byName := map[string]Family{}
	var names []string
	for _, f := range snap.Families {
		byName[f.Name] = f
		names = append(names, f.Name)
	}
	if want := []string{"c", "cost", "g", "h", "level", "observed"}; !slices.Equal(names, want) {
		t.Fatalf("families %v, want %v", names, want)
	}
	check := func(name string, kind Kind, number Number, observed bool) Family {
		t.Helper()
		f := byName[name]
		if f.Kind != kind || f.Number != number || f.Observed != observed {
			t.Errorf("%s: kind %s number %d observed %v", name, f.Kind, f.Number, f.Observed)
		}
		return f
	}

	c := check("c", KindCounter, Int, false)
	if c.Unit != "{request}" || c.Description != "C." || !slices.Equal(c.Attributes, []string{"k"}) {
		t.Errorf("c's definition: %+v", c.Definition)
	}
	if len(c.Points) != 2 || c.Points[0].Attributes[0] != "early" || c.Points[0].Int != 2 || c.Points[1].Int != 1 {
		t.Fatalf("c's points: %+v", c.Points)
	}
	early, late := c.Points[0].StartTime, c.Points[1].StartTime
	if early.Before(before) || early.After(created) || late.Before(created) {
		t.Errorf("start times: registry from %v, early %v, late series created after %v at %v", before, early, created, late)
	}
	if again := reg.Collect(); !again.Families[0].Points[0].StartTime.Equal(early) {
		t.Error("a series' start time moved between collects")
	}
	if f := check("cost", KindCounter, Double, false); f.Divisor != 1e9 || f.Points[0].Double != 0.25 || f.Points[0].SubUnits != 250_000_000 {
		t.Errorf("cost: divisor %v, %+v", f.Divisor, f.Points[0])
	}
	if f := byName["c"]; f.Divisor != 0 || f.Points[0].SubUnits != 0 {
		t.Errorf("c: a plain counter with divisor %v, sub-units %d", f.Divisor, f.Points[0].SubUnits)
	}
	if p := check("cost", KindCounter, Double, false).Points[0]; p.Double != 0.25 {
		t.Errorf("cost: %+v", p)
	}
	if p := check("level", KindUpDownCounter, Int, false).Points[0]; p.Int != -3 || p.StartTime.IsZero() {
		t.Errorf("level: %+v", p)
	}
	if p := check("g", KindGauge, Double, false).Points[0]; p.Double != 1.25 || !p.StartTime.IsZero() {
		t.Errorf("gauge: %+v", p)
	}
	h := check("h", KindHistogram, Double, false)
	if p := h.Points[0].Histogram; !slices.Equal(p.BucketCounts, []uint64{2, 1, 1}) || p.Count != 4 || p.Sum != 7.25 ||
		!slices.Equal(h.Buckets, []float64{1, 2}) {
		t.Errorf("histogram: %+v, bounds %v", p, h.Buckets)
	}
	if p := check("observed", KindCounter, Int, true).Points[0]; p.Int != 7 || !p.StartTime.Equal(reg.start) {
		t.Errorf("observed: %+v, registry start %v", p, reg.start)
	}
}

func TestConcurrentRecordingAndCollects(t *testing.T) {
	reg := NewRegistry()
	attrs := []string{"worker"}
	c := reg.Counter(Definition{Name: "c", Attributes: attrs})
	h := reg.Histogram(Definition{Name: "h", Unit: "s", Attributes: attrs, Buckets: []float64{1, 10}})
	g := reg.Gauge(Definition{Name: "g", Attributes: attrs})
	u := reg.UpDownCounter(Definition{Name: "u", Attributes: attrs})
	const workers, perWorker = 8, 1000
	var wg sync.WaitGroup
	stop := make(chan struct{})
	var collects sync.WaitGroup
	collects.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
				_ = text(reg)
			}
		}
	})
	for i := range workers {
		label := fmt.Sprint(i % 2) // two workers share each series
		wg.Go(func() {
			for range perWorker {
				c.Inc(label)
				h.Observe(2, label)
				g.Set(1, label)
				u.Add(1, label)
				u.Add(-1, label)
			}
		})
	}
	wg.Wait()
	close(stop)
	collects.Wait()
	out := text(reg)
	for _, want := range []string{
		`c_total{worker="0"} 4000`, `c_total{worker="1"} 4000`,
		`h_seconds_bucket{worker="0",le="1"} 0`, `h_seconds_bucket{worker="0",le="10"} 4000`,
		`h_seconds_sum{worker="1"} 8000`, `h_seconds_count{worker="1"} 4000`, `u{worker="1"} 0`,
	} {
		if !strings.Contains(out, want+"\n") {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func BenchmarkCounterInc(b *testing.B) {
	c := NewRegistry().Counter(Definition{Name: "c", Attributes: []string{"model", "backend", "outcome"}})
	c.Inc("m", "b", "success")
	b.ReportAllocs()
	for b.Loop() {
		c.Inc("m", "b", "success")
	}
}

func BenchmarkCounterIncNoAttributes(b *testing.B) {
	c := NewRegistry().Counter(Definition{Name: "c"})
	b.ReportAllocs()
	for b.Loop() {
		c.Inc()
	}
}

var benchBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300}

func BenchmarkHistogramObserve(b *testing.B) {
	h := NewRegistry().Histogram(Definition{Name: "h", Unit: "s", Attributes: []string{"model", "backend"}, Buckets: benchBuckets})
	h.Observe(0.3, "m", "b")
	b.ReportAllocs()
	for b.Loop() {
		h.Observe(0.3, "m", "b")
	}
}

func BenchmarkHistogramObserveParallel(b *testing.B) {
	h := NewRegistry().Histogram(Definition{Name: "h", Unit: "s", Attributes: []string{"model", "backend"}, Buckets: benchBuckets})
	h.Observe(0.3, "m", "b")
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			h.Observe(0.3, "m", "b")
		}
	})
}
