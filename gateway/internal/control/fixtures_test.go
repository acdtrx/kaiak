package control

// Runs the shared message fixtures in protocol/fixtures/messages/<kind>/.
// kaiak-control's suite runs the same files: a valid fixture passes both, an invalid
// one fails both, and a semantic fixture fails with the rule code its cases.json
// entry names. For a config snapshot, the config inside is part of the message: it
// must pass config.Parse too, and its rejection codes count as the message's.

import (
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"kaiak/internal/accounting"
	"kaiak/internal/config"
)

const (
	fixturesDir = "../../../protocol/fixtures/messages"
	casesFile   = "cases.json"
)

// decoder decodes one kind of message and returns the value (for round trips) and the
// codes of any rejection.
type decoder func(data []byte) (value any, codes []string)

func codesOf(err error) []string {
	if err == nil {
		return nil
	}
	var invalid *ValidationError
	if errors.As(err, &invalid) {
		return invalid.Codes()
	}
	return []string{"not a *ValidationError: " + err.Error()}
}

func decodeWith[T any](decode func([]byte) (T, error)) decoder {
	return func(data []byte) (any, []string) {
		v, err := decode(data)
		return v, codesOf(err)
	}
}

// decoders maps each fixture directory to its message's decoder.
var decoders = map[string]decoder{
	"config-snapshot": func(data []byte) (any, []string) {
		snapshot, err := DecodeConfigSnapshot(data)
		if err != nil {
			return nil, codesOf(err)
		}
		if _, err := config.Parse(snapshot.Config); err != nil {
			var invalid *config.ValidationError
			if !errors.As(err, &invalid) {
				return nil, []string{"not a *config.ValidationError: " + err.Error()}
			}
			return nil, invalid.Codes()
		}
		return snapshot, nil
	},
	"resync":       decodeWith(DecodeResync),
	"status":       decodeWith(DecodeStatus),
	"totals":       decodeWith(DecodeTotals),
	"usage-ack":    decodeWith(DecodeUsageAck),
	"usage-batch":  decodeWith(DecodeUsageBatch),
	"usage-record": decodeWith(DecodeUsageRecord),
}

type invalidCase struct {
	Kind   string  `json:"kind"`
	Code   *string `json:"code"`
	Reason string  `json:"reason"`
}

func fixtureFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var files []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") && e.Name() != casesFile {
			files = append(files, e.Name())
		}
	}
	slices.Sort(files)
	if len(files) == 0 {
		t.Fatalf("no fixtures in %s", dir)
	}
	return files
}

func readCases(t *testing.T, dir string) map[string]invalidCase {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, casesFile))
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	var cases map[string]invalidCase
	if err := dec.Decode(&cases); err != nil {
		t.Fatalf("%s: %v", casesFile, err)
	}
	for file, c := range cases {
		switch {
		case c.Kind != "schema" && c.Kind != "semantic":
			t.Errorf("%s: kind %q is not schema or semantic", file, c.Kind)
		case c.Kind == "semantic" && c.Code == nil:
			t.Errorf("%s: semantic case names no code", file)
		case c.Kind == "schema" && c.Code != nil:
			t.Errorf("%s: schema case names a code", file)
		}
		if c.Reason == "" {
			t.Errorf("%s: no reason", file)
		}
	}
	return cases
}

func readFixture(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestEveryFixtureKindHasADecoder(t *testing.T) {
	entries, err := os.ReadDir(fixturesDir)
	if err != nil {
		t.Fatal(err)
	}
	var dirs, kinds []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, e.Name())
		}
	}
	for kind := range decoders {
		kinds = append(kinds, kind)
	}
	slices.Sort(dirs)
	slices.Sort(kinds)
	if !slices.Equal(dirs, kinds) {
		t.Errorf("fixture directories %v, decoders %v", dirs, kinds)
	}
}

func TestValidMessageFixtures(t *testing.T) {
	for kind, decode := range decoders {
		dir := filepath.Join(fixturesDir, kind, "valid")
		for _, file := range fixtureFiles(t, dir) {
			t.Run(kind+"/"+file, func(t *testing.T) {
				if _, codes := decode(readFixture(t, filepath.Join(dir, file))); codes != nil {
					t.Fatalf("rejected: %v", codes)
				}
			})
		}
	}
}

func TestInvalidMessageFixtures(t *testing.T) {
	for kind, decode := range decoders {
		dir := filepath.Join(fixturesDir, kind, "invalid")
		files := fixtureFiles(t, dir)
		cases := readCases(t, dir)
		caseFiles := make([]string, 0, len(cases))
		for file := range cases {
			caseFiles = append(caseFiles, file)
		}
		slices.Sort(caseFiles)
		if !slices.Equal(caseFiles, files) {
			t.Errorf("%s: %s entries and fixture files differ:\n entries: %v\n files:   %v", kind, casesFile, caseFiles, files)
		}
		for _, file := range files {
			expected, ok := cases[file]
			if !ok {
				continue
			}
			t.Run(kind+"/"+file, func(t *testing.T) {
				_, codes := decode(readFixture(t, filepath.Join(dir, file)))
				want := CodeSchema
				if expected.Kind == "semantic" {
					want = *expected.Code
				}
				// A semantic fixture breaks exactly one rule, so no other code may appear.
				if !slices.Equal(codes, []string{want}) {
					t.Errorf("codes %v, want [%s] (%s)", codes, want, expected.Reason)
				}
			})
		}
	}
}

// Every valid fixture decodes into the Go types and encodes back to the same JSON
// value: the types carry every field, with the protocol's names and encodings.
func TestValidMessageFixturesRoundTrip(t *testing.T) {
	for kind, decode := range decoders {
		dir := filepath.Join(fixturesDir, kind, "valid")
		for _, file := range fixtureFiles(t, dir) {
			t.Run(kind+"/"+file, func(t *testing.T) {
				data := readFixture(t, filepath.Join(dir, file))
				value, codes := decode(data)
				if codes != nil {
					t.Fatalf("rejected: %v", codes)
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
				config.UnitTokensOut: 240, config.UnitTokensReasoning: 96},
			CostNanoUSD: 1234, GatewayTime: time.Date(2026, 9, 24, 10, 0, 0, 123456789, time.UTC),
		},
		{
			RecordID: "fedcba9876543210fedcba9876543210", RequestID: "req-2", GatewayInstance: "gw-1",
			KeyID: "k-alice", Groups: []string{"alice"},
			Model: "gpt-4.1-mini", Deployment: accounting.Deployment{Backend: "azure", Model: "gpt-4.1-mini"},
			Units: accounting.Units{config.UnitTokensIn: 0, config.UnitTokensCached: 0,
				config.UnitTokensOut: 0, config.UnitTokensReasoning: 0},
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
	totals, err := DecodeTotals(readFixture(t, filepath.Join(fixturesDir, "totals", "valid", "windows.json")))
	if err != nil {
		t.Fatal(err)
	}
	if got := totals.Windows[0].Used; got != 123456789012345678 {
		t.Errorf("Used = %d, want 123456789012345678", got)
	}
}

// Integer fields written with a fraction or exponent are integers, as the schemas say.
func TestIntegerSpellings(t *testing.T) {
	resync, err := DecodeResync([]byte(`{}`))
	if err != nil || resync != (Resync{}) {
		t.Fatalf("resync: %v", err)
	}
	status, err := DecodeStatus([]byte(`{"instance":"gw-1","protocol_version":3.0,"state":"ready",
		"started_at":"2026-09-24T10:00:00Z","applied_config_version":4e1,"applied_config_epoch":"0f1e2d3c4b5a69788796a5b4c3d2e1f0","last_rejection":null,
		"backends":{"b":{"in_flight":2.0,"max_in_flight":4e0,"deployments":{}}},"models":{"m":{"queued":1.0}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if status.AppliedConfigVersion == nil || *status.AppliedConfigVersion != 40 ||
		status.Backends["b"].InFlight != 2 || status.Backends["b"].MaxInFlight != 4 || status.Models["m"].Queued != 1 {
		t.Errorf("status = %+v", status)
	}
}

func TestSyntaxError(t *testing.T) {
	_, err := DecodeUsageAck([]byte(`{"batch":`))
	if codes := codesOf(err); !slices.Equal(codes, []string{CodeSyntax}) {
		t.Errorf("codes %v, want [%s]", codes, CodeSyntax)
	}
}

// Runs the message fixtures of protocol/fixtures/duplicate-members (the config ones run
// in the config package): each is refused with duplicate-member at its path, before
// any decoder reads it. Every file there must have an entry naming a known kind.
func TestDuplicateMemberFixtures(t *testing.T) {
	const dir = "../../../protocol/fixtures/duplicate-members"
	data, err := os.ReadFile(filepath.Join(dir, casesFile))
	if err != nil {
		t.Fatal(err)
	}
	var cases map[string]struct {
		Kind   string `json:"kind"`
		Path   string `json:"path"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var files []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") && e.Name() != casesFile {
			files = append(files, e.Name())
		}
	}
	listed := slices.Sorted(maps.Keys(cases))
	if !slices.Equal(listed, files) {
		t.Errorf("%s entries and fixture files differ:\n entries: %v\n files:   %v", casesFile, listed, files)
	}
	for _, file := range files {
		c := cases[file]
		if c.Kind == "config" {
			continue
		}
		if _, known := decoders[c.Kind]; !known {
			t.Errorf("%s: unknown kind %q", file, c.Kind)
			continue
		}
		t.Run(file, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join(dir, file))
			if err != nil {
				t.Fatal(err)
			}
			_, err = decodeByKind(c.Kind, raw)
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

// decodeByKind runs the message decoder of kind and returns its error.
func decodeByKind(kind string, raw []byte) (any, error) {
	switch kind {
	case "config-snapshot":
		return DecodeConfigSnapshot(raw)
	case "resync":
		return DecodeResync(raw)
	case "status":
		return DecodeStatus(raw)
	case "totals":
		return DecodeTotals(raw)
	case "usage-ack":
		return DecodeUsageAck(raw)
	case "usage-batch":
		return DecodeUsageBatch(raw)
	case "usage-record":
		return DecodeUsageRecord(raw)
	}
	return nil, errors.New("unknown kind " + kind)
}
