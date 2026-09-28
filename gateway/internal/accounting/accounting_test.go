package accounting

import (
	"bytes"
	"encoding/json"
	"log/slog"
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

func price(from string, usd map[config.Unit]float64) config.Price {
	return config.Price{EffectiveFrom: day(from), USDPerMillion: usd}
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
