package config

// Runs the shared config fixtures in protocol/fixtures/config/. kaiak-control's suite
// runs the same files: a valid fixture passes both, an invalid one fails both, and a
// semantic fixture fails with the rule code its cases.json entry names.

import (
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const (
	fixturesDir = "../../../protocol/fixtures/config"
	examplesDir = "../../../examples"
	casesFile   = "cases.json"
)

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

func TestValidFixtures(t *testing.T) {
	dir := filepath.Join(fixturesDir, "valid")
	for _, file := range fixtureFiles(t, dir) {
		t.Run(file, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join(dir, file))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := Parse(data); err != nil {
				t.Fatalf("rejected: %v", err)
			}
		})
	}
}

// The documented example configs (examples/*.json) must stay valid; kaiak-control's
// suite checks them too.
func TestExampleConfigs(t *testing.T) {
	for _, file := range fixtureFiles(t, examplesDir) {
		t.Run(file, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join(examplesDir, file))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := Parse(data); err != nil {
				t.Fatalf("rejected: %v", err)
			}
		})
	}
}

func TestInvalidFixtures(t *testing.T) {
	dir := filepath.Join(fixturesDir, "invalid")
	files := fixtureFiles(t, dir)
	cases := readCases(t, dir)

	caseFiles := make([]string, 0, len(cases))
	for file := range cases {
		caseFiles = append(caseFiles, file)
	}
	slices.Sort(caseFiles)
	if !slices.Equal(caseFiles, files) {
		t.Errorf("%s entries and fixture files differ:\n entries: %v\n files:   %v", casesFile, caseFiles, files)
	}

	for _, file := range files {
		expected, ok := cases[file]
		if !ok {
			continue
		}
		t.Run(file, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join(dir, file))
			if err != nil {
				t.Fatal(err)
			}
			_, err = Parse(data)
			var invalid *ValidationError
			if !errors.As(err, &invalid) {
				t.Fatalf("want a *ValidationError (%s), got %v", expected.Reason, err)
			}
			want := CodeSchema
			if expected.Kind == "semantic" {
				want = *expected.Code
			}
			// A semantic fixture breaks exactly one rule, so no other code may appear.
			if codes := invalid.Codes(); !slices.Equal(codes, []string{want}) {
				t.Errorf("codes %v, want [%s] (%s)\n%v", codes, want, expected.Reason, err)
			}
		})
	}
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
			data, err := os.ReadFile(filepath.Join(fixturesDir, "invalid", file))
			if err != nil {
				t.Fatal(err)
			}
			_, err = Parse(data)
			var invalid *ValidationError
			if !errors.As(err, &invalid) {
				t.Fatalf("want a *ValidationError, got %v", err)
			}
			want := "/backends/" + backend
			if len(invalid.Issues) != 1 || invalid.Issues[0].Path != want ||
				!strings.Contains(invalid.Issues[0].Message, "need api_key_env") {
				t.Errorf("issues %v, want one at %s saying the backend needs api_key_env", invalid.Issues, want)
			}
		})
	}
}

// duplicateCase is an entry of protocol/fixtures/duplicate-members/cases.json: raw
// documents repeating an object member, which the gateway refuses (duplicate-member,
// at path) before any decoder reads them. kaiak-control's suite runs the same files
// and checks JSON.parse's reading of each (the last occurrence) is otherwise valid, so
// the repeat is each file's only defect.
type duplicateCase struct {
	Kind   string `json:"kind"`
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

const duplicatesDir = "../../../protocol/fixtures/duplicate-members"

func TestDuplicateMemberFixtures(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(duplicatesDir, casesFile))
	if err != nil {
		t.Fatal(err)
	}
	var cases map[string]duplicateCase
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	ran := 0
	for _, file := range fixtureFiles(t, duplicatesDir) {
		c, ok := cases[file]
		if !ok || c.Kind != "config" {
			continue // a message fixture: the control package runs it
		}
		ran++
		t.Run(file, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join(duplicatesDir, file))
			if err != nil {
				t.Fatal(err)
			}
			_, err = Parse(raw)
			var invalid *ValidationError
			if !errors.As(err, &invalid) || !slices.Equal(invalid.Codes(), []string{CodeDuplicateMember}) {
				t.Fatalf("want a %s rejection (%s), got %v", CodeDuplicateMember, c.Reason, err)
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
		Type   string   `json:"type"`
		Value  float64  `json:"value"`
		Models []string `json:"models"`
	} `json:"limits"`
}

func TestResolvedFixtures(t *testing.T) {
	dir := filepath.Join(fixturesDir, "resolved")
	for _, file := range fixtureFiles(t, dir) {
		t.Run(file, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join(dir, file))
			if err != nil {
				t.Fatal(err)
			}
			dec := json.NewDecoder(strings.NewReader(string(data)))
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
				for i, member := range g.Path {
					if member.ID != g.PathIDs[i] || (i > 0 && member.Parent != g.Path[i-1]) {
						t.Errorf("%s: Path and PathIDs disagree at %d", id, i)
					}
				}

				gotAllowed := `"all"`
				if !g.AllowedModels.All() {
					encoded, _ := json.Marshal(g.AllowedModels.Names())
					gotAllowed = string(encoded)
				}
				var wantAllowed any
				if err := json.Unmarshal(want.AllowedModels, &wantAllowed); err != nil {
					t.Fatal(err)
				}
				encoded, _ := json.Marshal(wantAllowed)
				if gotAllowed != string(encoded) {
					t.Errorf("%s: allowed models %s, want %s", id, gotAllowed, encoded)
				}

				// A snapshot limit holds its model set sorted; the fixture writes it as
				// the config does. Identity ignores the order, the list order counts.
				wantLimits := make([]Limit, len(want.Limits))
				for i, l := range want.Limits {
					var models []string
					if l.Models != nil {
						models = slices.Sorted(slices.Values(l.Models))
					}
					wantLimits[i] = Limit{Type: LimitType(l.Type), Value: l.Value, Models: models}
				}
				assertLimits(t, id, g.Limits, wantLimits)
			}
		})
	}
}
