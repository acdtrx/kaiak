package control

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"kaiak/internal/config"
	"kaiak/internal/logattr"
	"kaiak/internal/schemacheck"
)

// This file is the control-plane client: the only code in the gateway that talks to
// a control plane (docs/ARCHITECTURE.md). It boots the config (the stream's first
// config, else the seed), then follows the config stream for as long as it runs,
// beside the usage sender (usage.go, queue.go) and the status reporter (status.go).
// Nothing here is on the request path: requests read the config Holder, which the
// client fills through the config Applier like any other source, and settled usage
// only joins a batch in memory (Record).

// TriggerControl and TriggerSeed name the client's config loads in the log and in
// kaiak_config_loads_total.
const (
	TriggerControl = "control"
	TriggerSeed    = "seed"
)

// Defaults for Options left zero.
const (
	DefaultBootWait     = 60 * time.Second
	DefaultBackoffBase  = 500 * time.Millisecond
	DefaultBackoffCap   = 30 * time.Second
	DefaultHealthyAfter = 30 * time.Second
	// DefaultIdleTimeout: three missed 15 s heartbeats.
	DefaultIdleTimeout = 45 * time.Second
)

// The boot retries: while the control plane is unavailable, the stream is opened
// again after a jittered delay, 250 ms doubling up to 2 s, until the boot wait ends.
// Shorter than the reconnect backoff: nothing serves yet, and a control plane
// restarting beside the gateway answers within seconds.
const (
	bootBackoffBase = 250 * time.Millisecond
	bootBackoffCap  = 2 * time.Second
)

// responseHeaderTimeout bounds the wait for any control-plane answer's headers once
// the request is sent: a backstop under each request's own bound (the stream's
// opening bound, the status and usage timeouts).
const responseHeaderTimeout = 30 * time.Second

// defaultHTTPClient is the client for control-plane requests: no overall timeout
// (the stream is long-lived; each request is bounded by its context), no answer may
// take longer than responseHeaderTimeout to start, and redirects are not followed —
// Go resends Authorization to a redirect target with the same host name whatever its
// port or scheme, so following one could hand the token to another service, or send
// it in clear after an https→http redirect.
func defaultHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = responseHeaderTimeout
	return &http.Client{Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// IsInstanceID reports whether id has the instance ID shape (CONTROL-PROTOCOL.md,
// Messages): a letter or digit, then up to 252 letters, digits, '.', '_' or '-'.
func IsInstanceID(id string) bool { return instancePattern.MatchString(id) }

// Options configure a Client.
type Options struct {
	// URL is the control plane's base URL; endpoints are under URL/v1/.
	URL *url.URL
	// Token is the bearer token; it is sent in the Authorization header and never
	// logged.
	Token string
	// Instance is the gateway's instance ID (Kaiak-Instance header).
	Instance string
	Applier  *config.Applier
	Logger   *slog.Logger
	// HTTPClient carries every request; nil is defaultHTTPClient.
	HTTPClient *http.Client
	// BootWait bounds the wait for the stream's first config at boot — the stream
	// opened again while the control plane is unavailable — before falling back to the
	// seed config.
	BootWait time.Duration
	// SeedConfig, when set, is the config document a boot applies when the control
	// plane is unavailable (KAIAK_SEED_CONFIG_FILE); SeedFile names its file in the
	// log.
	SeedConfig []byte
	SeedFile   string
	// BackoffBase and BackoffCap shape the reconnect delay: exponential from base,
	// capped, full jitter.
	BackoffBase time.Duration
	BackoffCap  time.Duration
	// HealthyAfter: a stream that stayed open this long resets the backoff.
	HealthyAfter time.Duration
	// IdleTimeout: a stream silent this long (no event, no heartbeat) is treated as
	// dead and reconnected.
	IdleTimeout time.Duration
	// OnTotals receives what each totals event gives, one call at a time, in the order
	// the stream delivered them; nil drops them.
	OnTotals func(TotalsUpdate)
	// BatchInterval seals the filling usage batch this often; BatchMaxRecords seals it
	// once it holds that many records (at most MaxBatchRecords).
	BatchInterval   time.Duration
	BatchMaxRecords int
	// StatusInterval is the status report cadence.
	StatusInterval time.Duration
	// StatusMinGap is the least time between a report and one a routing change
	// (ServingChanged) causes.
	StatusMinGap time.Duration
	// StartedAt is when the gateway process started (status started_at); zero is the
	// time New runs.
	StartedAt time.Time
	// Serving returns the routing state status reports (backends, models); nil
	// reports none.
	Serving func() Serving
	// Observer is told how usage delivery goes (metrics); nil tells no one.
	Observer UsageObserver
	// UsageMemoryBytes bounds the encoded usage records held in memory waiting for
	// delivery (KAIAK_USAGE_MEMORY_BYTES); 0 is DefaultUsageMemoryBytes.
	UsageMemoryBytes int64

	// wait and random are the backoff's timer and jitter source; statusNow and
	// statusAfter the status reporter's clock and gap timer. Tests replace them.
	wait        func(ctx context.Context, d time.Duration) error
	random      func() float64
	statusNow   func() time.Time
	statusAfter func(time.Duration) <-chan time.Time
}

// Client follows the config and totals from a control plane's stream, and sends it
// usage and status.
type Client struct {
	opts    Options
	base    string
	http    *http.Client
	logger  *slog.Logger
	backoff backoff
	usage   *usageSender
	status  *statusReporter
	// lastContact is when the control plane was last in contact (unix nanoseconds):
	// bytes on the config stream — the only channel that brings totals, so an ack is
	// not contact (docs/specs/GATEWAY.md, Limits → Outage refusal). It starts when the
	// client is created, so a gateway that boots without reaching the control plane
	// (the seed config) counts its outage from its start. streamOpen is
	// set while a config stream is open.
	lastContact atomic.Int64
	streamOpen  atomic.Bool

	mu sync.Mutex
	// appliedHash is the hash of the config in force from the control plane; "" before
	// one is applied, and while the seed, which has none, is.
	appliedHash string
	// rejection is the latest config received from the control plane when it was
	// rejected; nil before any, once a later one is applied, and once the config
	// received is the one running. A config event with its hash is skipped, so the
	// same rejected config is not checked again on every reconnect.
	rejection *Rejection
}

// New returns a client; Boot and Run use it. It starts the process's usage epoch, so
// Record can take records at once.
func New(opts Options) *Client {
	if opts.HTTPClient == nil {
		opts.HTTPClient = defaultHTTPClient()
	}
	if opts.BootWait == 0 {
		opts.BootWait = DefaultBootWait
	}
	if opts.BackoffBase == 0 {
		opts.BackoffBase = DefaultBackoffBase
	}
	if opts.BackoffCap == 0 {
		opts.BackoffCap = DefaultBackoffCap
	}
	if opts.HealthyAfter == 0 {
		opts.HealthyAfter = DefaultHealthyAfter
	}
	if opts.IdleTimeout == 0 {
		opts.IdleTimeout = DefaultIdleTimeout
	}
	if opts.wait == nil {
		opts.wait = waitTimer
	}
	if opts.random == nil {
		opts.random = rand.Float64
	}
	if opts.BatchInterval == 0 {
		opts.BatchInterval = DefaultBatchInterval
	}
	if opts.BatchMaxRecords <= 0 || opts.BatchMaxRecords > MaxBatchRecords {
		opts.BatchMaxRecords = MaxBatchRecords
	}
	if opts.StatusInterval == 0 {
		opts.StatusInterval = DefaultStatusInterval
	}
	if opts.StatusMinGap == 0 {
		opts.StatusMinGap = DefaultStatusMinGap
	}
	if opts.statusNow == nil {
		opts.statusNow = time.Now
	}
	if opts.statusAfter == nil {
		opts.statusAfter = time.After
	}
	if opts.UsageMemoryBytes <= 0 {
		opts.UsageMemoryBytes = DefaultUsageMemoryBytes
	}
	if opts.StartedAt.IsZero() {
		opts.StartedAt = time.Now()
	}
	base := *opts.URL
	base.Path = trimSlash(base.Path) + "/v1"
	c := &Client{
		opts:    opts,
		base:    base.String(),
		http:    opts.HTTPClient,
		logger:  opts.Logger.With("kaiak.control.url", opts.URL.Redacted()),
		backoff: backoff{base: opts.BackoffBase, cap: opts.BackoffCap, random: opts.random},
	}
	c.lastContact.Store(time.Now().UnixNano())
	c.usage = newUsageSender(c)
	c.status = newStatusReporter(c)
	return c
}

// Contact reports whether a config stream is open — contact for as long as it stays
// open, since a silent one is closed after IdleTimeout — and when the control plane
// was last in contact: bytes on the stream, heartbeats included. Before any, last is
// when the client was created.
func (c *Client) Contact() (connected bool, last time.Time) {
	return c.streamOpen.Load(), time.Unix(0, c.lastContact.Load())
}

func (c *Client) touch() { c.lastContact.Store(time.Now().UnixNano()) }

// AppliedConfigHash is the hash of the config in force from the control plane; false
// before one is applied, and while the seed is.
func (c *Client) AppliedConfigHash() (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.appliedHash, c.appliedHash != ""
}

// LastRejection is the status report's last_rejection: the latest config received
// from the control plane, when the gateway rejected it. A config applied from the
// control plane afterwards clears it, as does receiving the running config again.
// False when there is none.
func (c *Client) LastRejection() (Rejection, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.rejection == nil {
		return Rejection{}, false
	}
	return Rejection{ConfigHash: c.rejection.ConfigHash, Codes: slices.Clone(c.rejection.Codes)}, true
}

// errNoConfigYet: the stream was open when the boot wait ended, but the control plane
// had no config published to send.
var errNoConfigYet = errors.New("the control plane sent no config within the boot wait: nothing published yet")

// Boot gets the gateway its first config (docs/specs/GATEWAY.md, Control-plane mode →
// Boot): the stream's first config event, the stream opened again with backoff while
// the control plane is unavailable, until BootWait ends; failing that (unavailable
// through the wait, a config the gateway rejects, a refused token — the last two at
// once), the seed config (Options.SeedConfig) when the control plane is unavailable.
// With none, Boot returns an error saying why: the process exits on it rather than run
// without a config. Cancelling ctx ends the boot with its error.
func (c *Client) Boot(ctx context.Context) error {
	first, err := c.bootConfig(ctx)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		c.logFetchFailure("config not received at startup", err)
	} else if c.applyConfig(first) {
		return nil
	}
	switch {
	case err == nil:
		r, _ := c.LastRejection()
		return fmt.Errorf("no config: the control plane's config was rejected (codes %s; see the config rejected line): "+
			"fix the published config", strings.Join(r.Codes, ", "))
	case !unavailable(err):
		return fmt.Errorf("no config: %w; the seed config serves only while the control plane is unavailable", err)
	case c.opts.SeedConfig == nil:
		return fmt.Errorf("no config: control plane unavailable and no seed config (KAIAK_SEED_CONFIG_FILE): %w", err)
	case !c.bootFromSeed():
		return errors.New("no config: control plane unavailable and the seed config was rejected (see the config rejected line)")
	}
	return nil
}

// bootConfig waits for the stream's first config event within BootWait: an
// unavailable control plane (a restart, a rollout of the control plane beside the
// gateways) is asked again after a jittered delay until the wait ends; any other
// failure — the token refused, a config event the message rules refuse — returns at
// once, as waiting cannot fix it. The stream is closed once the config arrives: Run
// opens its own, which sends the same config (skipped by its hash) and then totals.
func (c *Client) bootConfig(ctx context.Context) (ConfigEvent, error) {
	bootCtx, cancel := context.WithTimeout(ctx, c.opts.BootWait)
	defer cancel()
	retry := backoff{base: bootBackoffBase, cap: bootBackoffCap, random: c.opts.random}
	for attempt := 1; ; attempt++ {
		result := c.followStream(bootCtx, true)
		if result.first != nil {
			return *result.first, nil
		}
		err := result.err
		switch {
		case err == nil:
			err = errors.New("the config stream ended before a config")
		case result.lasted > 0 && bootCtx.Err() != nil && ctx.Err() == nil:
			err = errNoConfigYet
		}
		if !unavailable(err) || bootCtx.Err() != nil {
			return ConfigEvent{}, err
		}
		level := slog.LevelDebug
		if attempt == 1 {
			level = slog.LevelWarn
			if errors.Is(err, errProtocolMismatch) {
				level = slog.LevelError
			}
		}
		c.logger.Log(ctx, level, "config not received at startup: retrying within the boot wait",
			"kaiak.control.attempt", attempt, logattr.Seconds("kaiak.control.boot_wait", c.opts.BootWait), "exception.message", err)
		if c.opts.wait(bootCtx, retry.next()) != nil {
			return ConfigEvent{}, err // the wait is over
		}
	}
}

// unavailable reports whether a failed boot attempt means the control plane cannot
// give a config now: the connection failed or timed out, the stream was cut off or
// stayed silent, the answer lacked this protocol version (a proxy answering for a
// control plane that is down, or a control plane of another version), it answered 5xx
// (failing on its side), or it had nothing published yet. A 4xx (the token refused)
// or a config event the message rules refuse is an error for the operator to fix, not
// an outage.
func unavailable(err error) bool {
	if s, ok := errors.AsType[*statusError](err); ok {
		return s.status >= 500
	}
	_, invalid := errors.AsType[*schemacheck.ValidationError](err)
	return !invalid
}

// bootFromSeed applies the seed config through the shared apply path; it reports
// whether it was applied. The seed has no config hash: the stream's config replaces
// it.
func (c *Client) bootFromSeed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, err := c.opts.Applier.Apply(TriggerSeed, c.opts.SeedConfig, "file.path", c.opts.SeedFile)
	return err == nil
}

// Run works with the control plane until ctx is cancelled, and returns once all of
// its goroutines have: it follows the config, sends usage batches and reports status.
// Usage not delivered when ctx ends is not sent: FlushUsage, called before, delivers it.
func (c *Client) Run(ctx context.Context) {
	var work sync.WaitGroup
	work.Go(func() { c.usage.runSealer(ctx) })
	work.Go(func() { c.usage.runSender(ctx) })
	work.Go(func() { c.status.run(ctx) })
	c.followConfig(ctx)
	work.Wait()
	c.http.CloseIdleConnections()
}

// takeTotals hands one totals event to the consumer with the usage it shows counted:
// every totals event is applied, in the order the stream delivered it — the control
// plane keeps its stream in order (CONTROL-PROTOCOL.md, Config stream → Order).
// complete: the stream's first totals since it connected.
func (c *Client) takeTotals(t Totals, complete bool) {
	update := TotalsUpdate{Totals: t, Complete: complete, Counted: c.usage.countedGeneration(t.CountedThrough)}
	if c.opts.OnTotals != nil {
		c.opts.OnTotals(update)
	}
}

// followConfig follows the config until ctx is cancelled: the config stream, with a
// backoff delay before every reconnect.
func (c *Client) followConfig(ctx context.Context) {
	for ctx.Err() == nil {
		result := c.followStream(ctx, false)
		if ctx.Err() != nil {
			return
		}
		c.logStreamEnd(result)
		if result.lasted >= c.opts.HealthyAfter {
			c.backoff.reset()
		}
		c.pause(ctx)
	}
}

// pause waits the next backoff delay, or until ctx is cancelled.
func (c *Client) pause(ctx context.Context) {
	d := c.backoff.next()
	c.logger.Debug("control plane reconnect scheduled", logattr.Seconds("kaiak.control.delay", d))
	_ = c.opts.wait(ctx, d) // cancelled: the loop sees ctx and stops
}

// takeConfig applies a config event — whatever config it replaces: the control plane
// is the authority on which config is current (CONTROL-PROTOCOL.md, Current config) —
// unless its hash is the config the gateway runs or the one it last rejected. The
// running config received again is the latest config received, and it is not
// rejected: a rejection still reported is cleared, and the status says so.
func (c *Client) takeConfig(e ConfigEvent) {
	c.mu.Lock()
	running := e.ConfigHash == c.appliedHash
	cleared := running && c.rejection != nil
	if cleared {
		c.rejection = nil
	}
	rejected := c.rejection != nil && e.ConfigHash == c.rejection.ConfigHash
	c.mu.Unlock()
	if cleared {
		c.logger.Info("config rejection cleared: the control plane sent the running config", "kaiak.config.hash", e.ConfigHash)
		c.status.requestReport(statusTriggerChange)
	}
	if running || rejected {
		c.logger.Debug("config event skipped: the config already applied or rejected", "kaiak.config.hash", e.ConfigHash)
		return
	}
	c.applyConfig(e)
}

// applyConfig runs the config through the shared apply path. Applied, it ends any
// rejection report; rejected, it is kept for the status report and the running config
// stays. c.mu is held across the apply, so the
// applied hash and rejection read by the status report always match the config in
// force.
func (c *Client) applyConfig(e ConfigEvent) bool {
	c.mu.Lock()
	_, err := c.opts.Applier.Apply(TriggerControl, e.Config, "kaiak.config.hash", e.ConfigHash)
	if err != nil {
		c.rejection = &Rejection{ConfigHash: e.ConfigHash, Codes: rejectionCodes(err)}
		c.mu.Unlock()
		c.status.requestReport(statusTriggerChange)
		return false
	}
	c.appliedHash = e.ConfigHash
	c.rejection = nil
	c.mu.Unlock()
	c.status.requestReport(statusTriggerChange)
	return true
}

// rejectionCodes are the issue codes of a config rejection, as the log shows them.
func rejectionCodes(err error) []string {
	if invalid, ok := errors.AsType[*schemacheck.ValidationError](err); ok {
		return invalid.Codes()
	}
	return []string{schemacheck.CodeSchema}
}

func (c *Client) logFetchFailure(msg string, err error) {
	level := slog.LevelWarn
	if errors.Is(err, errProtocolMismatch) {
		level = slog.LevelError
	}
	c.logger.Log(context.Background(), level, msg, "exception.message", err)
}

func (c *Client) logStreamEnd(r streamResult) {
	attrs := []any{logattr.Seconds("kaiak.lasted", r.lasted)}
	switch {
	case r.err == nil:
		c.logger.Info("config stream ended by the control plane", attrs...)
	case errors.Is(r.err, errProtocolMismatch), errors.Is(r.err, errMalformedTotals):
		c.logger.Error("config stream failed", append(attrs, "exception.message", r.err)...)
	default:
		c.logger.Warn("config stream failed", append(attrs, "exception.message", r.err)...)
	}
}

func waitTimer(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func trimSlash(p string) string {
	for len(p) > 0 && p[len(p)-1] == '/' {
		p = p[:len(p)-1]
	}
	return p
}
