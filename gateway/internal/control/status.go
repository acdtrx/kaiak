package control

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"sync"
	"time"
)

// Status reports (CONTROL-PROTOCOL.md, Gateway status): POST /v1/status when Run
// starts, whenever what the report says changes (state, applied config, rejection)
// or a config stream connects, and every StatusInterval. Routing changes (a queue
// starting or ending, a circuit opening or closing) are spaced by StatusMinGap: one
// inside the gap waits for its end, and that one report carries the latest state.
// A failed report is retried by the next trigger; nothing waits for one.

// DefaultStatusInterval: the control plane drops a gateway silent for 30 s.
const DefaultStatusInterval = 10 * time.Second

// DefaultStatusMinGap bounds the reports routing changes cause to one a second,
// however fast queues and circuits flip under load.
const DefaultStatusMinGap = time.Second

// statusTimeout bounds one POST /v1/status.
const statusTimeout = 10 * time.Second

// Status report triggers, as the log names them.
const (
	statusTriggerStart    = "start"
	statusTriggerInterval = "interval"
	statusTriggerChange   = "change"
	statusTriggerConnect  = "connect"
	statusTriggerServing  = "serving"
)

type statusReporter struct {
	c         *Client
	interval  time.Duration
	minGap    time.Duration
	startedAt time.Time
	serving   func() Serving
	now       func() time.Time
	after     func(time.Duration) <-chan time.Time
	// wake holds at most one pending report request: trigger names an immediate one,
	// servingChanged a routing change the minimum gap spaces.
	wake chan struct{}

	mu             sync.Mutex
	trigger        string
	servingChanged bool
	draining       bool
	// failing: the last report failed. Failures are logged at warning level when they
	// start, then at debug level until a report is delivered again.
	failing bool
}

func newStatusReporter(c *Client) *statusReporter {
	return &statusReporter{c: c, interval: c.opts.StatusInterval, minGap: c.opts.StatusMinGap,
		startedAt: c.opts.StartedAt.UTC(), serving: c.opts.Serving, now: c.opts.statusNow, after: c.opts.statusAfter,
		wake: make(chan struct{}, 1)}
}

// requestReport asks the reporter's loop for a report now.
func (r *statusReporter) requestReport(trigger string) {
	r.mu.Lock()
	r.trigger = trigger
	r.mu.Unlock()
	signal(r.wake)
}

// SetDraining marks the gateway draining: the next report, sent at once, says so.
func (c *Client) SetDraining() {
	c.status.mu.Lock()
	c.status.draining = true
	c.status.mu.Unlock()
	c.status.requestReport(statusTriggerChange)
}

// ServingChanged asks for a report because the routing state changed in a way
// worth telling before the next regular report (a model's queue starting or
// ending, a circuit opening or closing): at once when the last report is at least
// StatusMinGap old, else at the gap's end. It never blocks: a report already
// pending covers it.
func (c *Client) ServingChanged() {
	r := c.status
	r.mu.Lock()
	r.servingChanged = true
	r.mu.Unlock()
	signal(r.wake)
}

func (r *statusReporter) run(ctx context.Context) {
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	// last is when the latest report started; gapEnd is armed while a routing change
	// waits for the gap after it to pass. Any report carries the latest state, so
	// each one disarms it.
	var last time.Time
	var gapEnd <-chan time.Time
	report := func(trigger string) {
		last, gapEnd = r.now(), nil
		_ = r.c.ReportStatus(ctx, trigger) // failures are logged; the next trigger retries
	}
	report(statusTriggerStart)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			report(statusTriggerInterval)
		case <-gapEnd:
			report(statusTriggerServing)
		case <-r.wake:
			r.mu.Lock()
			trigger, serving := r.trigger, r.servingChanged
			r.trigger, r.servingChanged = "", false
			r.mu.Unlock()
			switch wait := last.Add(r.minGap).Sub(r.now()); {
			case trigger != "":
				report(trigger)
			case !serving:
			case wait <= 0:
				report(statusTriggerServing)
			case gapEnd == nil:
				gapEnd = r.after(wait)
			}
		}
	}
}

// ReportStatus sends one status report now; trigger names what asked for it. The
// result is logged and returned.
func (c *Client) ReportStatus(ctx context.Context, trigger string) error {
	r := c.status
	s := c.currentStatus()
	err := r.post(ctx, s)
	r.mu.Lock()
	wasFailing := r.failing
	r.failing = err != nil
	r.mu.Unlock()
	attrs := []any{"kaiak.trigger", trigger, "kaiak.status.state", s.State}
	switch {
	case err == nil && wasFailing:
		c.logger.Info("status report delivered again", attrs...)
	case err == nil:
		c.logger.Debug("status report delivered", attrs...)
	case ctx.Err() != nil:
	case !wasFailing || configProblem(err):
		c.logger.Warn("status report not delivered; retried with the next report", append(attrs, "exception.message", err)...)
	default:
		c.logger.Debug("status report not delivered", append(attrs, "exception.message", err)...)
	}
	return err
}

// currentStatus is the report as of now. Collections are non-nil: the schema refuses
// null.
func (c *Client) currentStatus() Status {
	r := c.status
	s := Status{Instance: c.opts.Instance, ProtocolVersion: ProtocolVersion, StartedAt: r.startedAt,
		Backends: map[string]BackendStatus{}, Models: map[string]ModelStatus{}}
	c.mu.Lock()
	// A config is in force from the control plane or the seed; the seed carries no
	// hash.
	loaded := c.opts.Applier.Loaded()
	if c.appliedHash != "" {
		hash := c.appliedHash
		s.AppliedConfigHash = &hash
	}
	if c.rejection != nil {
		codes := append([]string{}, c.rejection.Codes...)
		s.LastRejection = &Rejection{ConfigHash: c.rejection.ConfigHash, Codes: codes}
	}
	c.mu.Unlock()
	r.mu.Lock()
	draining := r.draining
	r.mu.Unlock()
	switch {
	case draining:
		s.State = StateDraining
	case loaded:
		s.State = StateReady
	default:
		s.State = StateStarting
	}
	if r.serving != nil {
		serving := r.serving()
		for id, b := range serving.Backends {
			if b.Deployments == nil {
				b.Deployments = map[string]DeploymentStatus{}
			}
			s.Backends[id] = b
		}
		maps.Copy(s.Models, serving.Models)
	}
	return s
}

// Serving is the routing state a status reports: backends (in flight, cap,
// deployments' circuits) and models (queued requests), as Status carries them.
type Serving struct {
	Backends map[string]BackendStatus
	Models   map[string]ModelStatus
}

func (r *statusReporter) post(ctx context.Context, s Status) error {
	body, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("encode status: %w", err)
	}
	reqCtx, cancel := context.WithTimeout(ctx, statusTimeout)
	defer cancel()
	resp, err := r.c.post(reqCtx, "/status", body, http.StatusNoContent)
	if err != nil {
		return fmt.Errorf("send status: %w", err)
	}
	return resp.Body.Close()
}
