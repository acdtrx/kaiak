package accounting

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"math"
	"strings"
	"testing"
	"time"

	"kaiak/internal/config"
)

func day(s string) time.Time {
	t, err := time.Parse(time.DateOnly, s)
	if err != nil {
		panic(err)
	}
	return t
}

// price is a one-tier entry: usd applies to every input size.
func price(from string, usd map[config.Unit]float64) config.Price {
	return tiered(from, config.PriceTier{AboveInputTokens: 0, USDPerMillion: usd})
}

func tiered(from string, tiers ...config.PriceTier) config.Price {
	return config.Price{EffectiveFrom: day(from), Tiers: tiers}
}

func tier(above int64, usd map[config.Unit]float64) config.PriceTier {
	return config.PriceTier{AboveInputTokens: above, USDPerMillion: usd}
}

var priceHistory = []config.Price{
	price("2025-01-01", map[config.Unit]float64{config.UnitTokensIn: 2, config.UnitTokensOut: 8}),
	price("2025-06-01", map[config.Unit]float64{config.UnitTokensIn: 1, config.UnitTokensCached: 0.25, config.UnitTokensOut: 4}),
}

func TestPriceInForce(t *testing.T) {
	cases := []struct {
		at   time.Time
		want int // index in priceHistory; -1 = none
	}{
		{day("2024-12-31").Add(23*time.Hour + 59*time.Minute), -1},
		{day("2025-01-01"), 0},
		{day("2025-05-31").Add(23 * time.Hour), 0},
		{day("2025-06-01"), 1},
		{day("2030-01-01"), 1},
		// The request's UTC date decides, whatever its zone.
		{time.Date(2025, 6, 1, 1, 0, 0, 0, time.FixedZone("CEST", 2*3600)), 0},
	}
	for _, c := range cases {
		got, ok := PriceAt(priceHistory, c.at)
		switch {
		case c.want < 0 && ok:
			t.Errorf("%v: entry %v, want none", c.at, got.EffectiveFrom)
		case c.want >= 0 && (!ok || !got.EffectiveFrom.Equal(priceHistory[c.want].EffectiveFrom)):
			t.Errorf("%v: entry %v %v, want %v", c.at, got.EffectiveFrom, ok, priceHistory[c.want].EffectiveFrom)
		}
	}
}

func TestCost(t *testing.T) {
	u := tokenUnits(1000, 4000, 500, 300)
	cases := []struct {
		name   string
		prices []config.Price
		at     time.Time
		want   int64
	}{
		// Cached input at the input price: (1000+4000)×2 + 500×8 = 14000 µ$.
		{"cached falls back to the input price", priceHistory, day("2025-03-01"), 14_000_000},
		// 1000×1 + 4000×0.25 + 500×4 = 4000 µ$; reasoning is inside tokens_out.
		{"cached priced on its own", priceHistory, day("2025-07-01"), 4_000_000},
		{"before the first entry", priceHistory, day("2024-01-01"), 0},
		{"unpriced model", nil, day("2025-07-01"), 0},
		{"unpriced input: cached falls back to 0 too", []config.Price{
			price("2025-01-01", map[config.Unit]float64{config.UnitTokensOut: 1})}, day("2025-07-01"), 500_000},
		// (1000+4000) × 0.00000003 µ$ = 0.15 nano-dollars.
		{"sub-nano costs round per record", []config.Price{
			price("2025-01-01", map[config.Unit]float64{config.UnitTokensIn: 0.00000003})}, day("2025-07-01"), 0},
	}
	for _, c := range cases {
		if got := Cost(c.prices, c.at, u); got != c.want {
			t.Errorf("%s: cost %d nano-USD, want %d", c.name, got, c.want)
		}
	}
}

// Qwen-style brackets: 0–32k, 32k–128k, 128k+.
var brackets = tiered("2025-01-01",
	tier(0, map[config.Unit]float64{config.UnitTokensIn: 1}),
	tier(32_000, map[config.Unit]float64{config.UnitTokensIn: 2}),
	tier(128_000, map[config.Unit]float64{config.UnitTokensIn: 3}),
)

func TestTierFor(t *testing.T) {
	cases := []struct {
		input int64
		want  int64 // the chosen tier's threshold
	}{
		{0, 0},
		{1, 0},
		{32_000, 0}, // exactly at a threshold: the tier below
		{32_001, 32_000},
		{128_000, 32_000},
		{128_001, 128_000},
		{math.MaxInt64, 128_000},
	}
	for _, c := range cases {
		if got := tierFor(brackets, c.input); got.AboveInputTokens != c.want {
			t.Errorf("input %d: tier above %d, want above %d", c.input, got.AboveInputTokens, c.want)
		}
	}
	one := price("2025-01-01", map[config.Unit]float64{config.UnitTokensIn: 1})
	for _, input := range []int64{0, 1, math.MaxInt64} {
		if got := tierFor(one, input); got.AboveInputTokens != 0 {
			t.Errorf("one tier, input %d: tier above %d, want the only one", input, got.AboveInputTokens)
		}
	}
}

func TestInputSize(t *testing.T) {
	if got := inputSize(tokenUnits(1000, 4000, 500, 300)); got != 5000 {
		t.Errorf("input size %d, want tokens_in + tokens_cached = 5000", got)
	}
	if got := inputSize(Units{}); got != 0 {
		t.Errorf("no units: input size %d, want 0", got)
	}
	// Units are clamped to 2^53 − 1 only after pricing: the sum must not wrap.
	if got := inputSize(tokenUnits(math.MaxInt64, math.MaxInt64, 0, 0)); got != math.MaxInt64 {
		t.Errorf("huge units: input size %d, want %d", got, int64(math.MaxInt64))
	}
}

func TestCostPicksTheTierByInputSize(t *testing.T) {
	// OpenAI-style long context: above 272k input tokens, about 2× input and 1.5×
	// output, cached input included.
	longContext := []config.Price{tiered("2025-01-01",
		tier(0, map[config.Unit]float64{config.UnitTokensIn: 4, config.UnitTokensCached: 0.4, config.UnitTokensOut: 20}),
		tier(272_000, map[config.Unit]float64{config.UnitTokensIn: 8, config.UnitTokensCached: 0.8, config.UnitTokensOut: 30}),
	)}
	// Upper tier without a tokens_cached price: cached input at that tier's tokens_in.
	cachedUnpriced := []config.Price{tiered("2025-01-01",
		tier(0, map[config.Unit]float64{config.UnitTokensIn: 4, config.UnitTokensCached: 0.4, config.UnitTokensOut: 20}),
		tier(272_000, map[config.Unit]float64{config.UnitTokensIn: 8, config.UnitTokensOut: 30}),
	)}
	at := day("2025-07-01")
	cases := []struct {
		name   string
		prices []config.Price
		units  Units
		want   int64
	}{
		// 1000 × 20 = 20000 µ$.
		{"input 0: the first tier", longContext, tokenUnits(0, 0, 1000, 0), 20_000_000},
		// 272000 × 4 + 1000 × 20 = 1108000 µ$.
		{"exactly at the threshold: the lower tier", longContext, tokenUnits(272_000, 0, 1000, 0), 1_108_000_000},
		// 272001 × 8 + 1000 × 30 = 2206008 µ$: the whole record at the upper tier.
		{"one token above: the upper tier", longContext, tokenUnits(272_001, 0, 1000, 0), 2_206_008_000},
		// 72001 + 200000 cached = 272001 input: 72001 × 8 + 200000 × 0.8 + 1000 × 30
		// = 766008 µ$.
		{"cached input counts toward the size", longContext, tokenUnits(72_001, 200_000, 1000, 0), 766_008_000},
		// The same input all uncached but below: 72001 × 4 + 1000 × 20 = 308004 µ$.
		{"below the threshold without the cached input", longContext, tokenUnits(72_001, 0, 1000, 0), 308_004_000},
		// 72001 × 8 + 200000 × 8 + 1000 × 30 = 2206008 µ$.
		{"upper tier without a cached price: its tokens_in price", cachedUnpriced,
			tokenUnits(72_001, 200_000, 1000, 0), 2_206_008_000},
		// Qwen-style brackets, tokens_in only: 32000 × 1, 32001 × 2, 128001 × 3.
		{"brackets: first", []config.Price{brackets}, tokenUnits(32_000, 0, 0, 0), 32_000_000},
		{"brackets: second", []config.Price{brackets}, tokenUnits(32_001, 0, 0, 0), 64_002_000},
		{"brackets: third", []config.Price{brackets}, tokenUnits(128_001, 0, 0, 0), 384_003_000},
	}
	for _, c := range cases {
		if got := Cost(c.prices, at, c.units); got != c.want {
			t.Errorf("%s: cost %d nano-USD, want %d", c.name, got, c.want)
		}
	}
}

// An estimated record picks its tier by its estimated input, the figure limits
// reserved.
func TestEstimatedRecordPicksTheTierByItsEstimatedInput(t *testing.T) {
	model := &config.Model{Name: "chat", Prices: []config.Price{brackets}}
	r := NewRecorder(RecorderOptions{Instance: "gw-1", Sink: &recordingSink{}})
	for _, c := range []struct {
		input int64
		want  int64
	}{{32_000, 32_000_000}, {32_001, 64_002_000}} {
		meter := NewMeter(0, c.input)
		meter.Sent() // sent, never answered: the input is estimated
		rec := r.Settle(Request{Model: model, Deployment: config.Deployment{Backend: &config.Backend{}},
			Start: day("2025-07-01")}, meter, false)
		if !rec.Estimated || rec.CostNanoUSD != c.want {
			t.Errorf("estimated input %d: estimated %v, cost %d nano-USD, want estimated at %d",
				c.input, rec.Estimated, rec.CostNanoUSD, c.want)
		}
	}
}

// A one-tier entry prices exactly as a flat price: the units × prices sum, rounded
// once, at every input size.
func TestOneTierPricesAsTheFlatArithmetic(t *testing.T) {
	usd := map[config.Unit]float64{config.UnitTokensIn: 1.25, config.UnitTokensCached: 0.125, config.UnitTokensOut: 10}
	prices := []config.Price{price("2025-01-01", usd)}
	for _, u := range []Units{
		tokenUnits(0, 0, 0, 0), tokenUnits(7, 3, 11, 2), tokenUnits(1_000_000, 250_000, 40_000, 0),
		tokenUnits(900_000_000, 0, 1, 0),
	} {
		flat := int64(math.Round(1000 * (float64(u[config.UnitTokensIn])*1.25 +
			float64(u[config.UnitTokensCached])*0.125 + float64(u[config.UnitTokensOut])*10)))
		if got := Cost(prices, day("2025-07-01"), u); got != flat {
			t.Errorf("units %v: cost %d nano-USD, want %d", u, got, flat)
		}
	}
}

func TestCostsAddUpWithoutDrift(t *testing.T) {
	prices := []config.Price{price("2025-01-01", map[config.Unit]float64{config.UnitTokensIn: 0.1, config.UnitTokensOut: 0.3})}
	one := Cost(prices, day("2025-07-01"), tokenUnits(7, 0, 3, 0)) // 0.7 + 0.9 = 1.6 µ$
	if one != 1600 {
		t.Fatalf("cost %d, want 1600", one)
	}
	var total int64
	for range 1_000_000 {
		total += Cost(prices, day("2025-07-01"), tokenUnits(7, 0, 3, 0))
	}
	if total != 1_600_000_000 {
		t.Errorf("a million records total %d nano-USD, want exactly 1600000000", total)
	}
}

type recordingSink struct{ records []UsageRecord }

func (s *recordingSink) Record(r UsageRecord) { s.records = append(s.records, r) }

func TestRecorderSettlesOneRecordToEverySink(t *testing.T) {
	a, b := &recordingSink{}, &recordingSink{}
	r := NewRecorder(RecorderOptions{Instance: "gw-1", Sink: Fanout{a, b}})
	r.now = func() time.Time { return time.Date(2025, 7, 1, 12, 0, 0, 5, time.FixedZone("X", 3600)) }
	model := &config.Model{Name: "chat", Prices: priceHistory}
	meter := bodyMeter(0, 40, 200, `{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":2}}`)
	rec := r.Settle(Request{
		RequestID:  "req-1",
		KeyID:      "k-1",
		Groups:     []string{"users", "ann"},
		Model:      model,
		Deployment: config.Deployment{Backend: &config.Backend{ID: "vllm"}, Model: "org/chat"},
		Start:      day("2025-07-01"),
	}, meter, true)

	if len(a.records) != 1 || len(b.records) != 1 || a.records[0].RecordID != rec.RecordID {
		t.Fatalf("sinks got %d and %d records", len(a.records), len(b.records))
	}
	data, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"record_id":"` + rec.RecordID + `","request_id":"req-1","gateway_instance":"gw-1","key_id":"k-1",` +
		`"groups":["users","ann"],"model":"chat","deployment":{"backend":"vllm","model":"org/chat"},` +
		`"units":{"tokens_cached":0,"tokens_in":10,"tokens_out":2,"tokens_reasoning":0},"cost_nano_usd":18000,` +
		`"estimated":false,"partial":false,"gateway_time":"2025-07-01T11:00:00.000000005Z"}`
	if string(data) != want {
		t.Errorf("record\n%s\nwant\n%s", data, want)
	}
	if other := r.Settle(Request{Model: model, Deployment: config.Deployment{Backend: &config.Backend{}}}, NewMeter(0, 0), false); other.RecordID == rec.RecordID {
		t.Error("record IDs repeat")
	}
}

// H5: a backend reporting more than 2^53 − 1 tokens would make the record — and its
// whole batch — unacceptable to the control plane. Settlement clamps every unit and
// the cost to the bound, logs it with the request ID and tells the metric.
func TestSettlementClampsUsageToTheProtocolBound(t *testing.T) {
	var logs bytes.Buffer
	clamped := 0
	sink := &recordingSink{}
	r := NewRecorder(RecorderOptions{Instance: "gw-1", Sink: sink,
		Logger: slog.New(slog.NewTextHandler(&logs, nil)), OutOfRange: func() { clamped++ }})
	model := &config.Model{Name: "chat", Prices: priceHistory}
	meter := bodyMeter(0, 40, 200, `{"choices":[],"usage":{"prompt_tokens":9007199254740992,"completion_tokens":3}}`)
	rec := r.Settle(Request{RequestID: "req-huge", Model: model,
		Deployment: config.Deployment{Backend: &config.Backend{ID: "vllm"}}, Start: day("2025-07-01")}, meter, true)

	if rec.Units[config.UnitTokensIn] != MaxAmount || rec.Units[config.UnitTokensOut] != 3 || rec.CostNanoUSD != MaxAmount {
		t.Errorf("units %v, cost %d; want tokens_in and the cost clamped to %d", rec.Units, rec.CostNanoUSD, int64(MaxAmount))
	}
	if len(sink.records) != 1 || sink.records[0].Units[config.UnitTokensIn] != MaxAmount {
		t.Errorf("the sink got %+v, want the clamped record", sink.records)
	}
	if clamped != 1 {
		t.Errorf("OutOfRange told %d times, want 1", clamped)
	}
	if out := logs.String(); !strings.Contains(out, "level=WARN") || !strings.Contains(out, "request_id=req-huge") ||
		!strings.Contains(out, "clamped=\"[tokens_in cost_nano_usd]\"") {
		t.Errorf("clamp not logged as a warning with the request ID and what was clamped:\n%s", out)
	}

	// Within the bound nothing changes and nothing is told.
	r.Settle(Request{Model: model, Deployment: config.Deployment{Backend: &config.Backend{}}, Start: day("2025-07-01")},
		bodyMeter(0, 40, 200, `{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":3}}`), true)
	if clamped != 1 {
		t.Errorf("OutOfRange told %d times, want still 1", clamped)
	}
}
