// Package fixturetest runs the shared fixtures of protocol/fixtures/, which
// kaiak-control's suite runs too: a valid fixture passes both halves, an invalid one
// fails both, and a semantic fixture fails with the rule code its cases.json entry
// names. Test tooling only: nothing in the gateway binary imports it.
package fixturetest

import (
	"bytes"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"kaiak/internal/schemacheck"
)

// protocolDir is protocol/ seen from a package directory two levels below gateway/
// (internal/<package>, cmd/<command>), where go test runs a package's tests.
const protocolDir = "../../../protocol"

// CasesFile is the file of a fixture directory that says what each fixture there
// breaks; it is not a fixture itself.
const CasesFile = "cases.json"

// Dir is the path of protocol/fixtures/<parts>.
func Dir(parts ...string) string {
	return filepath.Join(append([]string{protocolDir, "fixtures"}, parts...)...)
}

// SchemaFile is the path of protocol/schema/<name>.
func SchemaFile(name string) string {
	return filepath.Join(protocolDir, "schema", name)
}

// Read returns the content of the file at path.
func Read(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// Files lists the fixtures in dir: its .json files but the cases file, sorted. A
// directory without one fails the test.
func Files(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var files []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") && e.Name() != CasesFile {
			files = append(files, e.Name())
		}
	}
	slices.Sort(files)
	if len(files) == 0 {
		t.Fatalf("no fixtures in %s", dir)
	}
	return files
}

// InvalidCase is a cases.json entry of an invalid fixture: a schema case breaks the
// schema and names no code; a semantic case breaks the rule its code names.
type InvalidCase struct {
	Kind   string  `json:"kind"`
	Code   *string `json:"code"`
	Reason string  `json:"reason"`
}

// InvalidCases reads dir's cases file, failing the test on an entry that breaks the
// rules of InvalidCase or gives no reason.
func InvalidCases(t *testing.T, dir string) map[string]InvalidCase {
	t.Helper()
	var cases map[string]InvalidCase
	decodeStrict(t, filepath.Join(dir, CasesFile), &cases)
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

// RunValid runs every fixture in dir as a subtest named for its file: decode must
// accept it.
func RunValid(t *testing.T, dir string, decode func(data []byte) error) {
	t.Helper()
	for _, file := range Files(t, dir) {
		t.Run(file, func(t *testing.T) {
			if err := decode(Read(t, filepath.Join(dir, file))); err != nil {
				t.Fatalf("rejected: %v", err)
			}
		})
	}
}

// RunInvalid runs every fixture in dir as a subtest named for its file, after checking
// the cases file has an entry for each fixture and no other: decode must refuse it
// with a *schemacheck.ValidationError whose codes are exactly [schema] for a schema
// case, or exactly the case's code for a semantic one — a semantic fixture breaks one
// rule, so no other code may appear.
func RunInvalid(t *testing.T, dir string, decode func(data []byte) error) {
	t.Helper()
	files := Files(t, dir)
	cases := InvalidCases(t, dir)
	if listed := slices.Sorted(maps.Keys(cases)); !slices.Equal(listed, files) {
		t.Errorf("%s entries and fixture files differ:\n entries: %v\n files:   %v", CasesFile, listed, files)
	}
	for _, file := range files {
		expected, ok := cases[file]
		if !ok {
			continue
		}
		t.Run(file, func(t *testing.T) {
			err := decode(Read(t, filepath.Join(dir, file)))
			invalid, ok := errors.AsType[*schemacheck.ValidationError](err)
			if !ok {
				t.Fatalf("want a validation error (%s), got %v", expected.Reason, err)
			}
			want := schemacheck.CodeSchema
			if expected.Kind == "semantic" {
				want = *expected.Code
			}
			if codes := invalid.Codes(); !slices.Equal(codes, []string{want}) {
				t.Errorf("codes %v, want [%s] (%s)\n%v", codes, want, expected.Reason, err)
			}
		})
	}
}

// DuplicateCase is a cases.json entry of protocol/fixtures/duplicate-members/: a raw
// document of the message kind Kind (or "config") repeating the object member at
// Path, which the gateway refuses (duplicate-member, at Path) before any decoder
// reads it. kaiak-control's suite checks JSON.parse's reading of each (the last
// occurrence) is otherwise valid, so the repeat is each file's only defect.
type DuplicateCase struct {
	Kind   string `json:"kind"`
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// DuplicateCases reads the duplicate-member fixtures' cases file, failing the test
// unless it has an entry for each fixture and no other.
func DuplicateCases(t *testing.T) map[string]DuplicateCase {
	t.Helper()
	dir := Dir("duplicate-members")
	var cases map[string]DuplicateCase
	decodeStrict(t, filepath.Join(dir, CasesFile), &cases)
	if listed, files := slices.Sorted(maps.Keys(cases)), Files(t, dir); !slices.Equal(listed, files) {
		t.Errorf("%s entries and fixture files differ:\n entries: %v\n files:   %v", CasesFile, listed, files)
	}
	return cases
}

// decodeStrict decodes the JSON file at path into v, refusing unknown fields.
func decodeStrict(t *testing.T, path string, v any) {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(Read(t, path)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}
