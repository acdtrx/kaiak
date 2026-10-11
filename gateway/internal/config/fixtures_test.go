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
		"max-images-per-request-zero.json":    "max_images_per_request",
		"max-speech-batch-items-zero.json":    "max_speech_batch_items",
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

// The endpoint-not-served rule's table is the shared one: each backend type serves
// what protocol/fixtures/backend-types/<type>.json lists in its endpoints, and the
// fixtures name every type the table does. kaiak-control's table is held to the same
// files.
func TestEndpointSupportIsTheSharedTable(t *testing.T) {
	dir := fixturetest.Dir("backend-types")
	var types []string
	for _, file := range fixturetest.Files(t, dir) {
		typ := strings.TrimSuffix(file, ".json")
		types = append(types, typ)
		var fixture struct {
			Endpoints []EndpointName `json:"endpoints"`
		}
		if err := json.Unmarshal(fixturetest.Read(t, filepath.Join(dir, file)), &fixture); err != nil {
			t.Fatal(err)
		}
		got := slices.Sorted(slices.Values(endpointSupport[BackendType(typ)]))
		if want := slices.Sorted(slices.Values(fixture.Endpoints)); !slices.Equal(got, want) {
			t.Errorf("%s serves %v, the shared table says %v", typ, got, want)
		}
	}
	var tableTypes []string
	for typ := range endpointSupport {
		tableTypes = append(tableTypes, string(typ))
	}
	slices.Sort(types)
	if slices.Sort(tableTypes); !slices.Equal(types, tableTypes) {
		t.Errorf("fixtures for %v, table rows for %v", types, tableTypes)
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

// endpointColumns names the columns of the spec's endpoint support table
// (docs/specs/GATEWAY.md, Providers → Endpoint support) by the endpoints they cover.
var endpointColumns = map[string][]EndpointName{
	"chat, completions, embeddings": {EndpointChatCompletions, EndpointCompletions, EndpointEmbeddings},
	"messages":                      {EndpointMessages},
	"messages_count_tokens":         {EndpointMessagesCountTokens},
	"responses":                     {EndpointResponses},
	"responses_input_tokens":        {EndpointResponsesInputTokens},
	"rerank":                        {EndpointRerank},
	"audio_speech, audio_speech_batch, images_generations": {EndpointAudioSpeech, EndpointAudioSpeechBatch,
		EndpointImagesGenerations},
}

// Each type serves exactly what the spec's endpoint support table says, the table
// names every type, every column is one endpointColumns knows, and every endpoint name
// is in a column.
func TestEndpointSupportFollowsTheSpec(t *testing.T) {
	covered := map[EndpointName]bool{}
	for _, es := range endpointColumns {
		for _, e := range es {
			covered[e] = true
		}
	}
	for _, e := range endpointNames {
		if !covered[EndpointName(e)] {
			t.Errorf("%s is in no column of endpointColumns", e)
		}
	}

	doc := fixturetest.Read(t, filepath.Join("..", "..", "..", "docs", "specs", "GATEWAY.md"))
	var header []string
	seen := map[BackendType]bool{}
	for line := range strings.Lines(string(doc)) {
		line = strings.TrimSpace(line)
		cells := strings.Split(strings.Trim(line, "|"), "|")
		for i := range cells {
			cells[i] = strings.TrimSpace(cells[i])
		}
		switch {
		case strings.HasPrefix(line, "| Type | chat, completions, embeddings |"):
			header = cells
			for _, column := range header[1:] {
				if _, ok := endpointColumns[column]; !ok {
					t.Errorf("the spec's column %q is not in endpointColumns", column)
				}
			}
			continue
		case header == nil:
			continue
		case !strings.HasPrefix(line, "|"):
			header = nil
			continue
		case strings.HasPrefix(cells[0], "---"):
			continue
		}
		typ := BackendType(strings.Trim(cells[0], "`"))
		seen[typ] = true
		for i, cell := range cells[1:] {
			for _, e := range endpointColumns[header[i+1]] {
				if want := strings.HasPrefix(cell, "yes"); typ.Serves(e) != want {
					t.Errorf("%s serves %s: %v, the spec says %q", typ, e, typ.Serves(e), cell)
				}
			}
		}
	}
	for typ := range endpointSupport {
		if !seen[typ] {
			t.Errorf("the spec's endpoint support table has no row for %s", typ)
		}
	}
}
