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
	"kaiak/internal/state"
)

// This file is the control-plane client: the only code in the gateway that talks to
// a control plane (docs/ARCHITECTURE.md). It boots the config (snapshot, else the
// last-known-good copy, else the seed), then follows the config stream for as long as it runs,
// beside the usage sender (usage.go, spool.go) and the status reporter (status.go).
// Nothing here is on the request path: requests read the config Holder, which the
// client fills through the config Applier like any other source, and settled usage
// only joins a batch in memory (Record).

// TriggerControl, TriggerLastKnownGood and TriggerSeed name the client's config loads
// in the log and in kaiak_config_loads_total.
const (
	TriggerControl       = "control"
	TriggerLastKnownGood = "last-known-good"
	TriggerSeed          = "seed"
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

// The boot retries: while the control plane is unavailable, the snapshot is fetched
// again after a jittered delay, 250 ms doubling up to 2 s, until the boot wait ends.
// Shorter than the reconnect backoff: nothing serves yet, and a control plane
// restarting beside the gateway answers within seconds.
const (
	bootBackoffBase = 250 * time.Millisecond
	bootBackoffCap  = 2 * time.Second
)

// responseHeaderTimeout bounds the wait for any control-plane answer's headers once
// the request is sent: a backstop under each request's own bound (the stream's
// opening bound, the snapshot, status and usage timeouts).
const responseHeaderTimeout = 30 * time.Second

// defaultHTTPClient is the client for control-plane requests: no overall timeout
// (the stream is long-lived; each request is bounded by its context), and no answer
// may take longer than responseHeaderTimeout to start.
func defaultHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = responseHeaderTimeout
	return &http.Client{Transport: transport}
}

// snapshotTimeout bounds one GET /v1/config after boot, so a control plane that
// accepts the connection and never answers does not stall the client.
const snapshotTimeout = 30 * time.Second

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
	// Dir is the data directory: the usage spool and the last-known-good copy live
	// there. Nil keeps usage batches in memory and has no last-known-good copy.
	Dir    *state.Dir
	Logger *slog.Logger
	// HTTPClient carries every request; nil is defaultHTTPClient.
	HTTPClient *http.Client
	// BootWait bounds the snapshot fetches at boot — retried while the control plane
	// is unavailable — before falling back to the last-known-good or seed config.
	BootWait time.Duration
	// SeedConfig, when set, is the config document a boot applies when the control
	// plane is unavailable and there is no last-known-good copy
	// (KAIAK_SEED_CONFIG_FILE); SeedFile names its file in the log.
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
	// OnTotals receives what each totals message gives — every totals event and
	// every usage ack — one call at a time (never concurrently); nil drops them.
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

// Client fetches and follows the config from a control plane.
type Client struct {
	opts    Options
	base    string
	http    *http.Client
	logger  *slog.Logger
	backoff backoff
	usage   *usageSender
	status  *statusReporter
	// totalsMu makes OnTotals calls one at a time — the stream and the usage sender
	// deliver from different goroutines — and guards revision.
	totalsMu sync.Mutex
	// revision is the revision of the totals last applied; nil before any.
	revision *Revision
	// retired are the control-plane processes the totals moved away from, oldest
	// first, at most maxRetiredControlPlanes (CONTROL-PROTOCOL.md, Messages → Totals).
	retired []string

	// lastContact is when the control plane was last in contact (unix nanoseconds):
	// a snapshot fetched, bytes on the config stream, an ack. It starts when the
	// client is created, so a gateway that boots without reaching the control plane
	// (last-known-good or seed config) counts its outage from its start. streamOpen is set
	// while a config stream is open.
	lastContact atomic.Int64
	streamOpen  atomic.Bool

	mu sync.Mutex
	// applied is the version and epoch of the config in force, nil before one is
	// applied (and while the seed, which has none, is).
	applied *configPosition
	// position is the latest version taken from the control plane, applied or
	// rejected, with its epoch: the stream resumes after it, so a rejected config is
	// not replayed on every reconnect. Version 0 before any.
	position configPosition
	// rejection is the latest config received from the control plane when it was
	// rejected; nil before any, and once a later one is applied.
	rejection *Rejection
}

// configPosition is a config version and the control-plane store epoch it counts in
// (CONTROL-PROTOCOL.md, Config versions): versions of different epochs never compare.
type configPosition struct {
	epoch   string
	version int64
}

// New returns a client; Boot and Run use it. It restores the usage spool from the data
// directory (or starts a fresh one, or none without a data directory), so Record can
// take records at once.
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
		logger:  opts.Logger.With("control_url", opts.URL.Redacted()),
		backoff: backoff{base: opts.BackoffBase, cap: opts.BackoffCap, random: opts.random},
	}
	c.lastContact.Store(time.Now().UnixNano())
	c.usage = newUsageSender(c)
	c.status = newStatusReporter(c)
	return c
}

// Contact reports whether a config stream is open — contact for as long as it stays
// open, since a silent one is closed after IdleTimeout — and when the control plane
// was last in contact: a snapshot fetched, bytes on the stream (heartbeats
// included), an ack. Before any, last is when the client was created.
func (c *Client) Contact() (connected bool, last time.Time) {
	return c.streamOpen.Load(), time.Unix(0, c.lastContact.Load())
}

func (c *Client) touch() { c.lastContact.Store(time.Now().UnixNano()) }

// AppliedVersion is the version of the config in force; false before one is applied.
func (c *Client) AppliedVersion() (int64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.applied == nil {
		return 0, false
	}
	return c.applied.version, true
}

// LastRejection is the status report's last_rejection: the latest config received
// from the control plane, when the gateway rejected it. A config applied from the
// control plane afterwards clears it, whatever the version numbers say — a restarted
// control plane counts from 1 again, so a rejected config can have a lower version
// than the one in force. False when there is none.
func (c *Client) LastRejection() (Rejection, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.rejection == nil {
		return Rejection{}, false
	}
	return Rejection{Version: c.rejection.Version, Codes: slices.Clone(c.rejection.Codes)}, true
}

// Boot gets the gateway its first config (docs/specs/GATEWAY.md, Control-plane mode →
// Boot): the snapshot, fetched again with backoff while the control plane is
// unavailable, until BootWait ends; failing that (unavailable through the wait, a
// config the gateway rejects, a refused token — the last two at once), the
// last-known-good copy when there is a data directory; failing that too, when the
// control plane is unavailable, the seed config (Options.SeedConfig). With none, Boot
// returns an error saying why: the process exits on it rather than run without a
// config. Cancelling ctx ends the boot with its error.
func (c *Client) Boot(ctx context.Context) error {
	snapshot, err := c.bootSnapshot(ctx)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		c.logFetchFailure("config snapshot not fetched at startup", err)
	} else if c.takeSnapshot(snapshot) {
		return nil
	}
	if c.bootFromLastKnownGood() {
		return nil
	}
	switch {
	case err == nil:
		r, _ := c.LastRejection()
		return fmt.Errorf("no config: the control plane's config version %d was rejected (codes %s; see the config rejected line) "+
			"and there is no last-known-good config: fix the published config", r.Version, strings.Join(r.Codes, ", "))
	case !unavailable(err):
		return fmt.Errorf("no config: %w, and there is no last-known-good config; the seed config serves only while the "+
			"control plane is unavailable", err)
	case c.opts.SeedConfig == nil:
		return fmt.Errorf("no config: control plane unavailable and no seed config (KAIAK_SEED_CONFIG_FILE) or last-known-good config: %w", err)
	case !c.bootFromSeed():
		return errors.New("no config: control plane unavailable and the seed config was rejected (see the config rejected line)")
	}
	return nil
}

// bootSnapshot fetches the snapshot within BootWait: an unavailable control plane (a
// restart, a rollout of the control plane beside the gateways) is asked again after a
// jittered delay until the wait ends; any other failure — the token refused, a
// snapshot the message rules refuse — returns at once, as waiting cannot fix it.
func (c *Client) bootSnapshot(ctx context.Context) (ConfigSnapshot, error) {
	bootCtx, cancel := context.WithTimeout(ctx, c.opts.BootWait)
	defer cancel()
	retry := backoff{base: bootBackoffBase, cap: bootBackoffCap, random: c.opts.random}
	for attempt := 1; ; attempt++ {
		snapshot, err := c.fetchSnapshot(bootCtx)
		if err == nil || !unavailable(err) || bootCtx.Err() != nil {
			return snapshot, err
		}
		level := slog.LevelDebug
		if attempt == 1 {
			level = slog.LevelWarn
			if errors.Is(err, errProtocolMismatch) {
				level = slog.LevelError
			}
		}
		c.logger.Log(ctx, level, "config snapshot not fetched at startup: retrying within the boot wait",
			"attempt", attempt, "boot_wait_ms", c.opts.BootWait.Milliseconds(), "error", err)
		if c.opts.wait(bootCtx, retry.next()) != nil {
			return snapshot, err // the wait is over
		}
	}
}

// unavailable reports whether a failed snapshot fetch means the control plane cannot
// give a config now: the connection failed or timed out, the body was cut off, the
// answer lacked this protocol version (a proxy answering for a control plane that is
// down, or a control plane of another version), or it answered 5xx (503
// config-unavailable: nothing published yet; 500: failing on its side). A 4xx (the
// token refused) or a snapshot the message rules refuse is an error for the operator
// to fix, not an outage.
func unavailable(err error) bool {
	if s, ok := errors.AsType[*statusError](err); ok {
		return s.status >= 500
	}
	_, invalid := errors.AsType[*ValidationError](err)
	return !invalid
}

// bootFromSeed applies the seed config through the shared apply path; it reports
// whether it was applied. The seed is never saved as the last-known-good copy and
// carries no control-plane version: the stream starts from the snapshot, whose config
// replaces it.
func (c *Client) bootFromSeed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, err := c.opts.Applier.Apply(TriggerSeed, c.opts.SeedConfig, "file", c.opts.SeedFile)
	return err == nil
}

// Run works with the control plane until ctx is cancelled, and returns once all of
// its goroutines have: it follows the config, sends usage batches and reports status.
// When ctx ends the filling usage batch is sealed into the spool.
func (c *Client) Run(ctx context.Context) {
	var work sync.WaitGroup
	work.Go(func() { c.usage.runSealer(ctx) })
	work.Go(func() { c.usage.runSender(ctx) })
	work.Go(func() { c.status.run(ctx) })
	c.followConfig(ctx)
	work.Wait()
	c.http.CloseIdleConnections()
}

// maxRetiredControlPlanes bounds the control-plane processes remembered as moved
// away from. One control plane restarting is the case the rule serves (a delayed
// answer from the process just replaced); 16 covers a run of restarts well past it.
const maxRetiredControlPlanes = 16

// takeTotals hands one totals message to the consumer, one call at a time: its totals
// when their revision is newer than the last applied and not from a control-plane
// process moved away from (CONTROL-PROTOCOL.md, Messages → Totals), and the usage it
// shows counted — whatever the revision or process, since the totals applied are at
// least as new and every message is a consistent snapshot. acked is the generation
// of the batch an ack acknowledges (0 for a totals event).
func (c *Client) takeTotals(t Totals, acked uint64) {
	c.totalsMu.Lock()
	defer c.totalsMu.Unlock()
	update := TotalsUpdate{Counted: acked}
	if t.CountedThrough != nil {
		update.Counted = max(update.Counted, c.usage.countedGeneration(*t.CountedThrough))
	}
	switch {
	case slices.Contains(c.retired, t.Revision.ControlPlane):
		c.logger.Info("totals ignored: from a control plane replaced since", "control_plane", t.Revision.ControlPlane,
			"current", c.revision.ControlPlane)
	case c.revision == nil || t.Revision.NewerThan(*c.revision):
		if c.revision != nil && t.Revision.ControlPlane != c.revision.ControlPlane {
			c.logger.Info("totals from another control plane: ordering restarts", "control_plane", t.Revision.ControlPlane,
				"previous", c.revision.ControlPlane)
			c.retired = append(c.retired, c.revision.ControlPlane)
			if len(c.retired) > maxRetiredControlPlanes {
				c.retired = slices.Delete(c.retired, 0, 1)
			}
		}
		c.revision = &t.Revision
		update.Totals = &t
	default:
		c.logger.Debug("totals ignored: not newer than the totals applied", "sequence", t.Revision.Sequence,
			"applied_sequence", c.revision.Sequence)
	}
	if c.opts.OnTotals != nil && (update.Totals != nil || update.Counted != 0) {
		c.opts.OnTotals(update)
	}
}

// followConfig follows the config until ctx is cancelled: the config stream from the
// latest version taken, the snapshot again when nothing was taken yet or the stream
// says resync, and a backoff delay before every reconnect.
func (c *Client) followConfig(ctx context.Context) {
	needSnapshot := c.currentPosition().version == 0
	for ctx.Err() == nil {
		if needSnapshot {
			fetchCtx, cancel := context.WithTimeout(ctx, snapshotTimeout)
			snapshot, err := c.fetchSnapshot(fetchCtx)
			cancel()
			if err != nil {
				if ctx.Err() == nil {
					c.logFetchFailure("config snapshot not fetched", err)
				}
				c.pause(ctx)
				continue
			}
			c.takeSnapshot(snapshot)
			needSnapshot = false
		}
		result := c.followStream(ctx)
		if ctx.Err() != nil {
			return
		}
		c.logStreamEnd(result)
		if result.lasted >= c.opts.HealthyAfter {
			c.backoff.reset()
		}
		needSnapshot = result.resync || positionRefused(result.err)
		c.pause(ctx)
	}
}

// positionRefused reports whether the control plane refused to open the stream from
// the position sent (400 since-invalid): resuming from it again would be refused
// again, so the client fetches the snapshot and resumes from its position.
func positionRefused(err error) bool {
	s, ok := errors.AsType[*statusError](err)
	return ok && s.status == http.StatusBadRequest && s.code == "since-invalid"
}

// pause waits the next backoff delay, or until ctx is cancelled.
func (c *Client) pause(ctx context.Context) {
	d := c.backoff.next()
	c.logger.Debug("control plane reconnect scheduled", "delay_ms", d.Milliseconds())
	_ = c.opts.wait(ctx, d) // cancelled: the loop sees ctx and stops
}

func (c *Client) currentPosition() configPosition {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.position
}

// takeSnapshot applies a snapshot whatever its version: at boot nothing is in force,
// and after a resync a lower version is the control plane's new count, or the same
// number in another epoch another config (CONTROL-PROTOCOL.md, Config versions). It
// reports whether the config was applied.
func (c *Client) takeSnapshot(s ConfigSnapshot) bool {
	return c.applyConfig(s)
}

// takeStreamConfig applies a config event unless its version was already taken in the
// same epoch: within an epoch the stream never takes the gateway back to an older
// version; a version of another epoch is another store's count, applied whatever its
// number.
func (c *Client) takeStreamConfig(s ConfigSnapshot) {
	position := c.currentPosition()
	if s.ConfigEpoch == position.epoch && s.Version <= position.version {
		c.logger.Debug("config event ignored: version already taken", "config_version", s.Version,
			"position", position.version)
		return
	}
	if s.ConfigEpoch != position.epoch {
		c.logger.Info("config event from another config epoch: applied whatever its version",
			"config_epoch", s.ConfigEpoch, "previous_epoch", position.epoch)
	}
	c.applyConfig(s)
}

// applyConfig runs the config through the shared apply path. Applied, it becomes the
// last-known-good copy and ends any rejection report; rejected, it is kept for the
// status report and the running config stays. Either way the stream resumes after its version. c.mu is held across
// the apply, so the applied version and rejection read by the status report always
// match the config in force.
func (c *Client) applyConfig(s ConfigSnapshot) bool {
	version := s.Version
	c.mu.Lock()
	_, err := c.opts.Applier.ApplyPublished(TriggerControl, s.Config, config.Version{Epoch: s.ConfigEpoch, Number: version},
		"config_version", version, "config_epoch", s.ConfigEpoch)
	c.position = configPosition{epoch: s.ConfigEpoch, version: version}
	if err != nil {
		c.rejection = &Rejection{Version: version, Codes: rejectionCodes(err)}
		c.mu.Unlock()
		c.status.requestReport(statusTriggerChange)
		return false
	}
	c.applied = &configPosition{epoch: s.ConfigEpoch, version: version}
	c.rejection = nil
	c.mu.Unlock()
	c.status.requestReport(statusTriggerChange)
	c.saveLastKnownGood(configPosition{epoch: s.ConfigEpoch, version: version}, s.Config)
	return true
}

// rejectionCodes are the issue codes of a config rejection, as the log shows them.
func rejectionCodes(err error) []string {
	var invalid *config.ValidationError
	if errors.As(err, &invalid) {
		return invalid.Codes()
	}
	return []string{config.CodeSchema}
}

func (c *Client) logFetchFailure(msg string, err error) {
	level := slog.LevelWarn
	if errors.Is(err, errProtocolMismatch) {
		level = slog.LevelError
	}
	c.logger.Log(context.Background(), level, msg, "error", err)
}

func (c *Client) logStreamEnd(r streamResult) {
	attrs := []any{"lasted_ms", r.lasted.Milliseconds()}
	switch {
	case r.resync:
		c.logger.Info("config stream ended: resync", attrs...)
	case r.err == nil:
		c.logger.Info("config stream ended by the control plane", attrs...)
	case errors.Is(r.err, errProtocolMismatch):
		c.logger.Error("config stream failed", append(attrs, "error", r.err)...)
	default:
		c.logger.Warn("config stream failed", append(attrs, "error", r.err)...)
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
