package config

// Runs the shared config fixtures in protocol/fixtures/config/. kaiak-control's suite
// runs the same files: a valid fixture passes both, an invalid one fails both, and a
// semantic fixture fails with the rule code its cases.json entry names.

import (
	"bytes"
	"encoding/json"
	"errors"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"kaiak/internal/fixturetest"
	"kaiak/internal/schemacheck"
)

const examplesDir = "../../../examples"

// fixturesDir is protocol/fixtures/config.
var fixturesDir = fixturetest.Dir("config")

// parse is Parse as a fixture decoder.
func parse(data []byte) error {
	_, err := Parse(data)
	return err
}

func TestValidFixtures(t *testing.T) {
	fixturetest.RunValid(t, fixturetest.Dir("config", "valid"), parse)
}

// The documented example configs (examples/*.json) must stay valid; kaiak-control's
// suite checks them too.
func TestExampleConfigs(t *testing.T) {
	fixturetest.RunValid(t, examplesDir, parse)
}

func TestInvalidFixtures(t *testing.T) {
	fixturetest.RunInvalid(t, fixturetest.Dir("config", "invalid"), parse)
}

// The fixtures for a backend type that requires api_key_env are refused for the
// missing key at the backend, not as an unknown type: TestInvalidFixtures sees only
// the schema code, which both reasons share.
func TestBackendsRequiringAPIKeyEnv(t *testing.T) {
	for file, backend := range map[string]string{
		"openai-without-api-key-env.json": "openai",
		"azure-without-api-key-env.json":  "vllm",
	} {
		t.Run(file, func(t *testing.T) {
			_, err := Parse(fixturetest.Read(t, filepath.Join(fixturesDir, "invalid", file)))
			var invalid *schemacheck.ValidationError
			if !errors.As(err, &invalid) {
				t.Fatalf("want a *schemacheck.ValidationError, got %v", err)
			}
			want := "/backends/" + backend
			if len(invalid.Issues) != 1 || invalid.Issues[0].Path != want ||
				!strings.Contains(invalid.Issues[0].Message, "need api_key_env") {
				t.Errorf("issues %v, want one at %s saying the backend needs api_key_env", invalid.Issues, want)
			}
		})
	}
}

// The batch caps' zero fixtures are refused by their bound, at their field — not as
// an unknown field: TestInvalidFixtures sees only the schema code, which both reasons
// share.
func TestBatchCapsRefuseZero(t *testing.T) {
	for file, field := range map[string]string{
		"max-sequences-per-request-zero.json": "max_sequences_per_request",
		"max-embedding-inputs-zero.json":      "max_embedding_inputs",
		"max-rerank-documents-zero.json":      "max_rerank_documents",
	} {
		t.Run(file, func(t *testing.T) {
			_, err := Parse(fixturetest.Read(t, filepath.Join(fixturesDir, "invalid", file)))
			var invalid *schemacheck.ValidationError
			if !errors.As(err, &invalid) {
				t.Fatalf("want a *schemacheck.ValidationError, got %v", err)
			}
			want := "/global/" + field
			if len(invalid.Issues) != 1 || invalid.Issues[0].Path != want ||
				!strings.HasPrefix(invalid.Issues[0].Message, "must be between 1 and") {
				t.Errorf("issues %v, want one at %s refusing 0 by the field's bound", invalid.Issues, want)
			}
		})
	}
}

// The config fixtures of protocol/fixtures/duplicate-members (the message ones run in
// the control package): each is refused with duplicate-member at its path, before any
// decoder reads it.
func TestDuplicateMemberFixtures(t *testing.T) {
	cases := fixturetest.DuplicateCases(t)
	ran := 0
	for _, file := range slices.Sorted(maps.Keys(cases)) {
		c := cases[file]
		if c.Kind != "config" {
			continue // a message fixture: the control package runs it
		}
		ran++
		t.Run(file, func(t *testing.T) {
			_, err := Parse(fixturetest.Read(t, filepath.Join(fixturetest.Dir("duplicate-members"), file)))
			var invalid *schemacheck.ValidationError
			if !errors.As(err, &invalid) || !slices.Equal(invalid.Codes(), []string{schemacheck.CodeDuplicateMember}) {
				t.Fatalf("want a %s rejection (%s), got %v", schemacheck.CodeDuplicateMember, c.Reason, err)
			}
			if invalid.Issues[0].Path != c.Path {
				t.Errorf("path %q, want %q", invalid.Issues[0].Path, c.Path)
			}
		})
	}
	if ran == 0 {
		t.Error("no config fixtures among the duplicate-member fixtures")
	}
}

// resolvedFixture is a file of protocol/fixtures/config/resolved/: a valid config and
// what both halves must derive from it for every group (docs/specs/CONTROL-PROTOCOL.md,
// Config → The group tree → Resolution fixtures).
type resolvedFixture struct {
	Reason   string          `json:"reason"`
	Config   json.RawMessage `json:"config"`
	Expected struct {
		Groups map[string]resolvedGroup `json:"groups"`
	} `json:"expected"`
}

type resolvedGroup struct {
	Path []string `json:"path"`
	// AllowedModels is "all" or a sorted list of names.
	AllowedModels json.RawMessage `json:"allowed_models"`
	Limits        []struct {
		Type  string  `json:"type"`
		Value float64 `json:"value"`
	} `json:"limits"`
}

func TestResolvedFixtures(t *testing.T) {
	dir := filepath.Join(fixturesDir, "resolved")
	for _, file := range fixturetest.Files(t, dir) {
		t.Run(file, func(t *testing.T) {
			dec := json.NewDecoder(bytes.NewReader(fixturetest.Read(t, filepath.Join(dir, file))))
			dec.DisallowUnknownFields()
			var fixture resolvedFixture
			if err := dec.Decode(&fixture); err != nil {
				t.Fatal(err)
			}
			s, err := Parse(fixture.Config)
			if err != nil {
				t.Fatalf("rejected: %v", err)
			}
			if got, want := slices.Sorted(maps.Keys(s.Groups)), slices.Sorted(maps.Keys(fixture.Expected.Groups)); !slices.Equal(got, want) {
				t.Fatalf("groups %v, want %v", got, want)
			}
			for id, want := range fixture.Expected.Groups {
				g := s.Groups[id]
				if !slices.Equal(g.PathIDs, want.Path) {
					t.Errorf("%s: path %v, want %v", id, g.PathIDs, want.Path)
				}

				// "all" is every model of the snapshot.
				var wantAllowed any
				if err := json.Unmarshal(want.AllowedModels, &wantAllowed); err != nil {
					t.Fatal(err)
				}
				if wantAllowed == "all" {
					wantAllowed = s.ModelNames
				}
				gotAllowed, _ := json.Marshal(g.AllowedModels.Names())
				encoded, _ := json.Marshal(wantAllowed)
				if string(gotAllowed) != string(encoded) {
					t.Errorf("%s: allowed models %s, want %s", id, gotAllowed, encoded)
				}

				// The fixture lists the effective limits in the order the merge gives
				// them: the parent's defaults in place, then the group's own.
				wantLimits := make([]Limit, len(want.Limits))
				for i, l := range want.Limits {
					wantLimits[i] = Limit{Type: LimitType(l.Type), Value: l.Value}
				}
				assertLimits(t, id, g.Limits, wantLimits)
			}
		})
	}
}
