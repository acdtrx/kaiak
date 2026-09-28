package server

import (
	"sync"
	"time"
)

// Retry budget (docs/specs/GATEWAY.md, Routing and reliability: retry budget): per
// model, retries in the last retryBudgetWindow may reach retryBudgetRatio of the
// attempts sent in it, and at least retryBudgetMin, so a model with little traffic
// still retries while a fleet-wide failure does not multiply its load by the attempt
// count.
const (
	retryBudgetWindow = 10 * time.Second
	retryBudgetRatio  = 0.2
	retryBudgetMin    = 10
)

// retryBudgetBuckets is the window's number of one-second buckets.
const retryBudgetBuckets = int(retryBudgetWindow / time.Second)

// retryBudget counts attempts and retries per public model name over a sliding
// window of one-second buckets. Safe for concurrent use.
type retryBudget struct {
	now func() time.Time

	mu     sync.Mutex
	models map[string]*retryWindow
	// swept is when models idle for a whole window were last removed.
	swept time.Time
}

// retryWindow is one model's counts, bucket i holding second sec[i].
type retryWindow struct {
	sec      [retryBudgetBuckets]int64
	attempts [retryBudgetBuckets]int
	retries  [retryBudgetBuckets]int
}

func newRetryBudget(now func() time.Time) *retryBudget {
	return &retryBudget{now: now, models: make(map[string]*retryWindow)}
}

// attempt counts a request's first attempt sent for model.
func (b *retryBudget) attempt(model string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	w, i := b.bucket(model)
	w.attempts[i]++
}

// retry counts a retry sent for model: an attempt, and a retry. A retry is counted
// when it is sent, not when allowRetry approved it: one approved and then never sent
// (no slot, the client gone) spends nothing.
func (b *retryBudget) retry(model string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	w, i := b.bucket(model)
	w.attempts[i]++
	w.retries[i]++
}

// allowRetry reports whether model's budget has room for one more retry. It counts
// nothing: retry does, once the retry is sent. Retries approved at the same moment
// may pass the budget by the few in between.
func (b *retryBudget) allowRetry(model string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	w, _ := b.bucket(model)
	now := b.now().Unix()
	attempts, retries := 0, 0
	for j := range retryBudgetBuckets {
		if now-w.sec[j] < int64(retryBudgetBuckets) {
			attempts += w.attempts[j]
			retries += w.retries[j]
		}
	}
	return retries < max(retryBudgetMin, int(float64(attempts)*retryBudgetRatio))
}

// bucket returns model's window and the index of the current second's bucket,
// emptied when it held an older second. Callers hold b.mu.
func (b *retryBudget) bucket(model string) (*retryWindow, int) {
	now := b.now()
	if now.Sub(b.swept) >= retryBudgetWindow {
		b.sweep(now.Unix())
		b.swept = now
	}
	w := b.models[model]
	if w == nil {
		w = &retryWindow{}
		b.models[model] = w
	}
	sec := now.Unix()
	i := int(sec % int64(retryBudgetBuckets))
	if w.sec[i] != sec {
		w.sec[i], w.attempts[i], w.retries[i] = sec, 0, 0
	}
	return w, i
}

// sweep removes the models with nothing counted in the window ending at second now,
// so models dropped from the config leave nothing behind.
func (b *retryBudget) sweep(now int64) {
	for name, w := range b.models {
		idle := true
		for j := range retryBudgetBuckets {
			if now-w.sec[j] < int64(retryBudgetBuckets) && (w.attempts[j] > 0 || w.retries[j] > 0) {
				idle = false
				break
			}
		}
		if idle {
			delete(b.models, name)
		}
	}
}
