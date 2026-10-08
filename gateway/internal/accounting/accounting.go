// Package accounting settles every routed request into one usage record: units from
// the backend's usage report (or estimated and flagged), cost from the model's price
// table, flags for estimated and partial usage. Each record goes to the control-plane
// sender (Batcher, in control-plane mode), then to the usage metrics (Metrics). Local
// limits read the request's own record instead.
package accounting

import (
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"math"
	"slices"
	"time"

	"kaiak/internal/config"
)

// UsageRecord is one request's settled usage (docs/specs/CONTROL-PROTOCOL.md, Usage
// records). Field names are the protocol's, so the control-plane sender can send it as
// it is. It never holds a key, a credential or any request or response content.
type UsageRecord struct {
	// RecordID is random (128 bits, hex): the control plane de-duplicates by it.
	RecordID  string `json:"record_id"`
	RequestID string `json:"request_id"`
	// GatewayInstance is the gateway's instance ID.
	GatewayInstance string `json:"gateway_instance"`
	KeyID           string `json:"key_id"`
	// Groups is the key's group path in the request's config snapshot, top-level
	// first, the key's own group last: the control plane counts the record toward
	// them and global without re-deriving the path. Shared with the snapshot: do not
	// modify.
	Groups     []string   `json:"groups"`
	Model      string     `json:"model"`
	Deployment Deployment `json:"deployment"`
	Units      Units      `json:"units"`
	// CostNanoUSD is the cost in billionths of a US dollar.
	CostNanoUSD int64 `json:"cost_nano_usd"`
	Estimated   bool  `json:"estimated"`
	Partial     bool  `json:"partial"`
	// GatewayTime is when the gateway settled the record (UTC): the control plane
	// counts the record in its window when that is the current or previous one, and
	// local limits keep it by the same rule.
	GatewayTime time.Time `json:"gateway_time"`
	// Generation is the usage generation of the batch the control-plane sender took
	// the record into (Batcher); 0 without one (file mode). Never sent: local limits
	// tag the record's usage with it, so they stop counting it the moment the control
	// plane shows its batch counted.
	Generation uint64 `json:"-"`
	// Operation is the request's gen_ai.operation.name, from its endpoint: chat,
	// text_completion or embeddings. Never sent: the protocol has no field for it; the
	// usage metrics label the record with it.
	Operation string `json:"-"`
}

// Deployment is where the request ran: the backend ID and the model name on it.
type Deployment struct {
	Backend string `json:"backend"`
	Model   string `json:"model"`
}

// Batcher takes a settled record into the control plane's filling usage batch and
// returns that batch's usage generation. Taking the record and reading the generation
// are one step under the batcher's lock, so the generation is exactly the batch the
// record is sealed in. Record is called on the request's goroutine as the request
// finishes, so it never blocks.
type Batcher interface {
	Record(UsageRecord) (generation uint64)
}

// Metrics turns settled records into usage metrics. Its methods are called on the
// request's goroutine as the request finishes, so they never block: they update
// memory only. Record shares the record's Units map and must not modify it.
type Metrics interface {
	// Record counts one settled record.
	Record(UsageRecord)
	// RecordClamped counts one record whose units or cost were clamped to the
	// protocol's bound.
	RecordClamped()
}

// RecorderOptions configure a Recorder.
type RecorderOptions struct {
	// Instance is the gateway's instance ID, stamped on every record.
	Instance string
	// Batcher, in control-plane mode, takes every record into a usage batch before
	// the metrics see it, so the record they see carries its generation; nil in file
	// mode.
	Batcher Batcher
	// Metrics counts every record, and every record clamped to the protocol's bound.
	Metrics Metrics
	Logger  *slog.Logger
}

// Recorder turns settled requests into usage records and hands them to its batcher,
// then its metrics.
type Recorder struct {
	opts RecorderOptions
	now  func() time.Time
}

// NewRecorder returns a recorder with opts.
func NewRecorder(opts RecorderOptions) *Recorder {
	return &Recorder{opts: opts, now: time.Now}
}

// Request is what a record says about the request itself.
type Request struct {
	RequestID string
	KeyID     string
	// Groups is the key's group path, top-level first (config.Group.PathIDs).
	Groups     []string
	Model      *config.Model
	Deployment config.Deployment
	// Operation is the endpoint's gen_ai.operation.name (UsageRecord.Operation).
	Operation string
	// Start is when the request arrived; it picks the price entry in force.
	Start time.Time
}

// Settle settles a request that is over: its units from meter, its cost, and the
// record, which it passes to the batcher (which tags it with its generation), then the
// metrics, and returns. complete reports whether the response ran to its end.
func (r *Recorder) Settle(rq Request, meter *Meter, complete bool) UsageRecord {
	units, flags := meter.Settle(complete)
	rec := UsageRecord{
		RecordID:        newRecordID(),
		RequestID:       rq.RequestID,
		GatewayInstance: r.opts.Instance,
		KeyID:           rq.KeyID,
		Groups:          rq.Groups,
		Model:           rq.Model.Name,
		Deployment:      Deployment{Backend: rq.Deployment.Backend.ID, Model: rq.Deployment.Model},
		Units:           units,
		CostNanoUSD:     Cost(rq.Model.Prices, rq.Start, units),
		Estimated:       flags.Estimated,
		Partial:         flags.Partial,
		GatewayTime:     r.now().UTC(),
		Operation:       rq.Operation,
	}
	if clamped := clampToProtocol(&rec); len(clamped) > 0 {
		if r.opts.Logger != nil {
			r.opts.Logger.Warn("usage out of the protocol's range: clamped to 2^53-1", "kaiak.request.id", rec.RequestID,
				"kaiak.usage.record_id", rec.RecordID, "kaiak.backend.id", rec.Deployment.Backend, "kaiak.usage.clamped", clamped)
		}
		r.opts.Metrics.RecordClamped()
	}
	if r.opts.Batcher != nil {
		rec.Generation = r.opts.Batcher.Record(rec)
	}
	r.opts.Metrics.Record(rec)
	return rec
}

// MaxAmount is the largest unit amount or cost a usage record carries: 2^53 − 1, the
// protocol's integer bound (CONTROL-PROTOCOL.md, Messages → Integers).
const MaxAmount = 1<<53 - 1

// clampToProtocol lowers every unit amount and the cost above MaxAmount to it and
// names what it lowered. A backend reporting more tokens than that is broken or
// hostile; the clamped record still counts (at an amount above any limit) instead of
// making its whole batch unacceptable to the control plane.
func clampToProtocol(rec *UsageRecord) []string {
	var clamped []string
	for unit, n := range rec.Units {
		if n > MaxAmount {
			rec.Units[unit] = MaxAmount
			clamped = append(clamped, string(unit))
		}
	}
	slices.Sort(clamped)
	if rec.CostNanoUSD > MaxAmount {
		rec.CostNanoUSD = MaxAmount
		clamped = append(clamped, "cost_nano_usd")
	}
	return clamped
}

func newRecordID() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never returns an error
	return hex.EncodeToString(b[:])
}

// PriceAt returns the price entry in force at t: the latest whose effective date (UTC
// midnight) is on or before t. prices is in increasing date order.
func PriceAt(prices []config.Price, t time.Time) (config.Price, bool) {
	for i := len(prices) - 1; i >= 0; i-- {
		if !prices[i].EffectiveFrom.After(t) {
			return prices[i], true
		}
	}
	return config.Price{}, false
}

// tierFor returns the tier of price that prices a record whose input size is input:
// the last tier whose AboveInputTokens is strictly less than input, else the first
// (docs/specs/CONTROL-PROTOCOL.md, Config → Tiered prices). price has at least one
// tier, the first at 0, thresholds strictly increasing.
func tierFor(price config.Price, input int64) config.PriceTier {
	for i := len(price.Tiers) - 1; i > 0; i-- {
		if price.Tiers[i].AboveInputTokens < input {
			return price.Tiers[i]
		}
	}
	return price.Tiers[0]
}

// Cost prices units under the entry in force at t, in nano-USD
// (docs/specs/CONTROL-PROTOCOL.md, Units and price units): at the one tier the
// record's input size picks, the whole record — input, input read from and written to
// the cache, and output — at that tier's prices. The priced units are disjoint, so
// the cost is a sum over config.PricedUnits; tokens_cached or tokens_cache_write
// without a price of its own is charged at the tier's tokens_in price
// (config.PriceFallback); tokens_reasoning is inside tokens_out and never priced. An unpriced model, or a date before its first entry, costs 0.
//
// Each record's cost is rounded to a whole nano-dollar once, here: records are summed
// as integers downstream, so totals over any number of records carry no floating-point
// drift, and the rounding error is at most half a nano-dollar per record.
func Cost(prices []config.Price, t time.Time, units Units) int64 {
	price, ok := PriceAt(prices, t)
	if !ok {
		return 0
	}
	// The input size picks the tier: InputUnits, saturating (units are clamped to the
	// protocol's bound only after pricing).
	tier := tierFor(price, units.Sum(config.InputUnits))
	// USD per million tokens × tokens = micro-dollars; × 1000 = nano-dollars.
	var micro float64
	for _, unit := range config.PricedUnits {
		micro += float64(units[unit]) * unitPrice(tier, unit)
	}
	nano := 1000 * micro
	if nano >= math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(math.Round(nano))
}

// unitPrice is the tier's price for unit: its own price when the tier names one, else
// the price of its config.PriceFallback unit, else 0.
func unitPrice(tier config.PriceTier, unit config.Unit) float64 {
	if price, ok := tier.USDPerMillion[unit]; ok {
		return price
	}
	if fallback, ok := config.PriceFallback[unit]; ok {
		return tier.USDPerMillion[fallback]
	}
	return 0
}
