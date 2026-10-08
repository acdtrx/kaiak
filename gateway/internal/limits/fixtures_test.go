package limits

import (
	"bytes"
	"encoding/json"
	"maps"
	"path/filepath"
	"slices"
	"strconv"
	"testing"

	"kaiak/internal/accounting"
	"kaiak/internal/config"
	"kaiak/internal/fixturetest"
)

// countedFixture is a file of protocol/fixtures/usage/: a usage record's units and
// cost, and what it counts toward each token and cost limit type, in the type's unit
// as a decimal string (docs/specs/CONTROL-PROTOCOL.md, Usage intake → Counted
// toward). kaiak-control's suite checks the control plane counts the same.
type countedFixture struct {
	Reason      string                      `json:"reason"`
	Units       map[config.Unit]int64       `json:"units"`
	CostNanoUSD int64                       `json:"cost_nano_usd"`
	Expected    map[config.LimitType]string `json:"expected"`
}

func TestCountedUnitsFixtures(t *testing.T) {
	// Every limit type a record counts toward; a request limit counts the request.
	var types []config.LimitType
	for _, typ := range config.LimitTypes() {
		if typ.Measure() != config.MeasureRequests {
			types = append(types, typ)
		}
	}
	dir := fixturetest.Dir("usage")
	for _, file := range fixturetest.Files(t, dir) {
		t.Run(file, func(t *testing.T) {
			dec := json.NewDecoder(bytes.NewReader(fixturetest.Read(t, filepath.Join(dir, file))))
			dec.DisallowUnknownFields()
			var fixture countedFixture
			if err := dec.Decode(&fixture); err != nil {
				t.Fatal(err)
			}
			if got := slices.Sorted(maps.Keys(fixture.Units)); !slices.Equal(got, slices.Sorted(slices.Values(config.TokenUnits))) {
				t.Fatalf("units %v, want every token unit %v", got, config.TokenUnits)
			}
			if got := slices.Sorted(maps.Keys(fixture.Expected)); !slices.Equal(got, slices.Sorted(slices.Values(types))) {
				t.Fatalf("expected types %v, want %v", got, types)
			}
			rec := accounting.UsageRecord{Units: accounting.Units(fixture.Units), CostNanoUSD: fixture.CostNanoUSD}
			for _, typ := range types {
				want, err := strconv.ParseInt(fixture.Expected[typ], 10, 64)
				if err != nil {
					t.Fatal(err)
				}
				if got := amountOf(typ.Measure(), rec); got != want {
					t.Errorf("%s: counted %d, want %d (%s)", typ, got, want, fixture.Reason)
				}
			}
		})
	}
}
