package control

// Runs the shared message fixtures in protocol/fixtures/messages/<kind>/.
// kaiak-control's suite runs the same files: a valid fixture passes both, an invalid
// one fails both, and a semantic fixture fails with the rule code its cases.json
// entry names. For a config event, the config inside is part of the message: it must
// pass config.Parse too, and its rejection codes count as the message's.

import (
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	"kaiak/internal/accounting"
	"kaiak/internal/config"
	"kaiak/internal/fixturetest"
)

// fixturesDir is protocol/fixtures/messages.
var fixturesDir = fixturetest.Dir("messages")

// decoder decodes one kind of message and returns the value (for round trips) or the
// rejection.
type decoder func(data []byte) (value any, err error)

func decodeWith[T any](decode func([]byte) (T, error)) decoder {
	return func(data []byte) (any, error) { return decode(data) }
}

// decoders maps each fixture directory to its message's decoder.
var decoders = map[string]decoder{
	"config-event": func(data []byte) (any, error) {
		event, err := DecodeConfigEvent(data)
		if err != nil {
			return nil, err
		}
		if _, err := config.Parse(event.Config); err != nil {
			return nil, err
		}
		return event, nil
	},
	"status":       decodeWith(DecodeStatus),
	"totals":       decodeWith(DecodeTotals),
	"usage-ack":    decodeWith(DecodeUsageAck),
	"usage-batch":  decodeWith(DecodeUsageBatch),
	"usage-record": decodeWith(DecodeUsageRecord),
}

// rejection is decode as a fixture decoder: only the rejection counts.
func (decode decoder) rejection(data []byte) error {
	_, err := decode(data)
	return err
}

func TestEveryFixtureKindHasADecoder(t *testing.T) {
	entries, err := os.ReadDir(fixturesDir)
	if err != nil {
		t.Fatal(err)
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, e.Name())
		}
	}
	slices.Sort(dirs)
	if kinds := slices.Sorted(maps.Keys(decoders)); !slices.Equal(dirs, kinds) {
		t.Errorf("fixture directories %v, decoders %v", dirs, kinds)
	}
}

func TestValidMessageFixtures(t *testing.T) {
	for kind, decode := range decoders {
		t.Run(kind, func(t *testing.T) {
			fixturetest.RunValid(t, filepath.Join(fixturesDir, kind, "valid"), decode.rejection)
		})
	}
}

func TestInvalidMessageFixtures(t *testing.T) {
	for kind, decode := range decoders {
		t.Run(kind, func(t *testing.T) {
			fixturetest.RunInvalid(t, filepath.Join(fixturesDir, kind, "invalid"), decode.rejection, CodeSchema)
		})
	}
}

// Every valid fixture decodes into the Go types and encodes back to the same JSON
// value: the types carry every field, with the protocol's names and encodings.
func TestValidMessageFixturesRoundTrip(t *testing.T) {
	for kind, decode := range decoders {
		dir := filepath.Join(fixturesDir, kind, "valid")
		for _, file := range fixturetest.Files(t, dir) {
			t.Run(kind+"/"+file, func(t *testing.T) {
				data := fixturetest.Read(t, filepath.Join(dir, file))
				value, err := decode(data)
				if err != nil {
					t.Fatalf("rejected: %v", err)
				}
				encoded, err := json.Marshal(value)
				if err != nil {
					t.Fatal(err)
				}
				var want, got any
				if err := json.Unmarshal(data, &want); err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(encoded, &got); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(got, want) {
					t.Errorf("round trip changed the message:\n got:  %s\n want: %s", encoded, data)
				}
			})
		}
	}
}

// A record as accounting builds it encodes to a valid usage record and a valid batch.
func TestAccountingRecordEncodesToAValidMessage(t *testing.T) {
	records := []accounting.UsageRecord{
		{
			RecordID: "0123456789abcdef0123456789abcdef", RequestID: "req-1", GatewayInstance: "gw-1",
			KeyID: "k-eval-ci", Groups: []string{"research", "rag", "eval-pipeline"},
			Model: "qwen3-32b", Deployment: accounting.Deployment{Backend: "vllm-a", Model: "Qwen/Qwen3-32B"},
			Units: accounting.Units{config.UnitTokensIn: 812, config.UnitTokensCached: 0,
				config.UnitTokensCacheWrite: 0, config.UnitTokensOut: 240, config.UnitTokensReasoning: 96},
			CostNanoUSD: 1234, GatewayTime: time.Date(2026, 9, 24, 10, 0, 0, 123456789, time.UTC),
		},
		{
			RecordID: "fedcba9876543210fedcba9876543210", RequestID: "req-2", GatewayInstance: "gw-1",
			KeyID: "k-alice", Groups: []string{"alice"},
			Model: "gpt-4.1-mini", Deployment: accounting.Deployment{Backend: "azure", Model: "gpt-4.1-mini"},
			Units: accounting.Units{config.UnitTokensIn: 0, config.UnitTokensCached: 0,
				config.UnitTokensCacheWrite: 0, config.UnitTokensOut: 0, config.UnitTokensReasoning: 0},
			Partial: true, GatewayTime: time.Date(2026, 9, 24, 10, 0, 1, 0, time.UTC),
		},
	}
	for _, rec := range records {
		data, err := json.Marshal(rec)
		if err != nil {
			t.Fatal(err)
		}
		got, err := DecodeUsageRecord(data)
		if err != nil {
			t.Fatalf("%s: %v", data, err)
		}
		if !reflect.DeepEqual(got, rec) {
			t.Errorf("decoded %+v, want %+v", got, rec)
		}
	}
	batch := UsageBatch{Batch: BatchID{Instance: "gw-1", Epoch: "5d41402abc4b2a76b9719d911017c592", Sequence: 1}, Records: records}
	data, err := json.Marshal(batch)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeUsageBatch(data); err != nil {
		t.Errorf("batch rejected: %v", err)
	}
}

// A totals amount past 2^53 decodes exactly.
func TestTotalsAmountBeyondSafeInteger(t *testing.T) {
	totals, err := DecodeTotals(fixturetest.Read(t, filepath.Join(fixturesDir, "totals", "valid", "windows.json")))
	if err != nil {
		t.Fatal(err)
	}
	if got := totals.Windows[0].Used; got != 123456789012345678 {
		t.Errorf("Used = %d, want 123456789012345678", got)
	}
}

// Integer fields written with a fraction or exponent are integers, as the schemas say.
func TestIntegerSpellings(t *testing.T) {
	status, err := DecodeStatus([]byte(`{"instance":"gw-1","protocol_version":5.0,"state":"ready",
		"started_at":"2026-09-24T10:00:00Z","applied_config_hash":null,"last_rejection":null,
		"backends":{"b":{"in_flight":2.0,"max_in_flight":4e0,"deployments":{}}},"models":{"m":{"queued":1.0}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if status.Backends["b"].InFlight != 2 || status.Backends["b"].MaxInFlight != 4 || status.Models["m"].Queued != 1 {
		t.Errorf("status = %+v", status)
	}
}

func TestSyntaxError(t *testing.T) {
	_, err := DecodeUsageAck([]byte(`{"batch":`))
	var invalid *ValidationError
	if !errors.As(err, &invalid) || !slices.Equal(invalid.Codes(), []string{CodeSyntax}) {
		t.Errorf("got %v, want a %s rejection", err, CodeSyntax)
	}
}

// Runs the message fixtures of protocol/fixtures/duplicate-members (the config ones run
// in the config package): each is refused with duplicate-member at its path, before
// any decoder reads it. Every file there must have an entry naming a known kind.
func TestDuplicateMemberFixtures(t *testing.T) {
	cases := fixturetest.DuplicateCases(t)
	for _, file := range slices.Sorted(maps.Keys(cases)) {
		c := cases[file]
		if c.Kind == "config" {
			continue
		}
		decode, known := decoders[c.Kind]
		if !known {
			t.Errorf("%s: unknown kind %q", file, c.Kind)
			continue
		}
		t.Run(file, func(t *testing.T) {
			_, err := decode(fixturetest.Read(t, filepath.Join(fixturetest.Dir("duplicate-members"), file)))
			var invalid *ValidationError
			if !errors.As(err, &invalid) || !slices.Equal(invalid.Codes(), []string{CodeDuplicateMember}) {
				t.Fatalf("want a %s rejection (%s), got %v", CodeDuplicateMember, c.Reason, err)
			}
			if invalid.Issues[0].Path != c.Path {
				t.Errorf("path %q, want %q", invalid.Issues[0].Path, c.Path)
			}
		})
	}
}
