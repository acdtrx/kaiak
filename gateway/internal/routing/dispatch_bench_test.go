package routing

import (
	"container/list"
	"fmt"
	"testing"
	"time"

	"kaiak/internal/config"
)

// BenchmarkDispatchFullScan measures the dispatcher's round that finds no free slot —
// the round every release ends with — at 5 models × 20 deployments (20 shared
// backends, all full) with 500 first-attempt requests waiting per model.
func BenchmarkDispatchFullScan(b *testing.B) {
	const models, deployments, waiting = 5, 20, 500
	backends := make([]*config.Backend, deployments)
	for i := range backends {
		backends[i] = backend(fmt.Sprintf("b%d", i), 1)
	}
	var ms []*config.Model
	for i := range models {
		ms = append(ms, queuedModel(fmt.Sprintf("m%d", i), waiting, time.Hour, backends...))
	}
	r := New(Options{})
	r.Configure(circuitSnapshot(5, time.Hour, ms...))
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, bk := range backends {
		r.backendLoad[bk.ID] = 1
	}
	for _, m := range ms {
		for range waiting {
			r.arrivals++
			w := &waiter{model: m, arrival: r.arrivals, granted: make(chan grant, 1)}
			q := r.queues[m.Name]
			if q == nil {
				r.queues[m.Name] = list.New()
				q = r.queues[m.Name]
			}
			w.elem = q.PushBack(w)
		}
	}
	b.ResetTimer()
	for b.Loop() {
		if r.dispatch() {
			b.Fatal("a queue emptied with every backend full")
		}
	}
}
