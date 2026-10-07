package limits

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"kaiak/internal/config"
	"kaiak/internal/state"
)

// The limiter at the target scale (docs/specs/GATEWAY.md, Limits): one group per key,
// 7 000 groups, every one counted for the hour and the month. The benchmarks measure
// what a totals push costs under the request lock, what a totals.json write costs, and
// the counters' heap.

const scaleGroups = 7000

func scaleLimiter(b *testing.B) (*Limiter, *clock) {
	b.Helper()
	clock := newClock("2026-10-07T12:30:00Z")
	snap := snapshot(&testing.T{}, limitsDoc{})
	for i := range scaleGroups {
		id := fmt.Sprintf("scale-%04d", i)
		snap.Groups[id] = &config.Group{ID: id}
	}
	l, _ := clock.shared(holderOf(snap))
	windows := make([]PushedWindow, 0, 2*scaleGroups)
	for i := range scaleGroups {
		id := fmt.Sprintf("scale-%04d", i)
		windows = append(windows, pushedWindow(id, config.LimitTokensPerHour, "2026-10-07T12:00:00Z", 100),
			pushedWindow(id, config.LimitUSDPerMonth, "2026-10-01T00:00:00Z", 100))
	}
	l.TakeTotals(Totals{Complete: true, LiveGateways: 3, Windows: windows}, 0)
	return l, clock
}

// BenchmarkTakeTotalsChangesOnly is a push listing one changed window, the ordinary
// push while a few keys are busy.
func BenchmarkTakeTotalsChangesOnly(b *testing.B) {
	l, _ := scaleLimiter(b)
	changed := []PushedWindow{pushedWindow("scale-0001", config.LimitTokensPerHour, "2026-10-07T12:00:00Z", 200)}
	b.ResetTimer()
	for i := range b.N {
		l.TakeTotals(Totals{LiveGateways: 3, Windows: changed}, uint64(i+1))
	}
}

// BenchmarkSaveShared is one totals.json write.
func BenchmarkSaveShared(b *testing.B) {
	l, _ := scaleLimiter(b)
	dir, err := state.Open(b.TempDir(), nil)
	if err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for range b.N {
		if _, err := l.SaveShared(dir); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	info, err := os.Stat(filepath.Join(dir.Path(), SharedFile))
	if err != nil {
		b.Fatal(err)
	}
	b.ReportMetric(float64(info.Size())/(1<<20), "MiB/file")
}

// BenchmarkCounterHeap reports the heap the counters of 7 000 groups take.
func BenchmarkCounterHeap(b *testing.B) {
	for range b.N {
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		l, _ := scaleLimiter(b)
		runtime.GC()
		runtime.ReadMemStats(&after)
		b.ReportMetric(float64(after.HeapAlloc-before.HeapAlloc)/(1<<20), "MiB")
		runtime.KeepAlive(l)
	}
}
