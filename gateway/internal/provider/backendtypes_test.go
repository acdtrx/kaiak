package provider

// The backend types against what kaiak-control's backend-verify must agree on with
// them (protocol/fixtures/backend-types/), and against the config schema's type enum.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"kaiak/internal/config"
	"kaiak/internal/fixturetest"
)

// backendTypeFixture is a file of protocol/fixtures/backend-types/, named for its type:
// for a sample base_url and credential, the models-list request a backend of that
// type gets — its URL, and the headers it carries besides each half's own Accept and
// User-Agent — or null when the type has no models list.
type backendTypeFixture struct {
	BaseURL    string `json:"base_url"`
	Credential string `json:"credential"`
	ModelsList *struct {
		URL     string            `json:"url"`
		Headers map[string]string `json:"headers"`
	} `json:"models_list"`
}

// recordingTransport records every request and answers each with an empty models
// list; nothing leaves the process.
type recordingTransport struct{ requests []*http.Request }

func (r *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r.requests = append(r.requests, req)
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}},
		Body: io.NopCloser(strings.NewReader(`{"data":[]}`)), Request: req}, nil
}

func TestBackendTypeFixtures(t *testing.T) {
	dir := fixturetest.Dir("backend-types")
	files := fixturetest.Files(t, dir)
	types := make([]string, len(files))
	for i, file := range files {
		types[i] = strings.TrimSuffix(file, ".json")
	}
	slices.Sort(types)
	if want := kindNames(); !slices.Equal(types, want) {
		t.Errorf("fixtures %v, backend types %v", types, want)
	}

	for _, typ := range types {
		t.Run(typ, func(t *testing.T) {
			dec := json.NewDecoder(bytes.NewReader(fixturetest.Read(t, filepath.Join(dir, typ+".json"))))
			dec.DisallowUnknownFields()
			var fixture backendTypeFixture
			if err := dec.Decode(&fixture); err != nil {
				t.Fatal(err)
			}
			b := &config.Backend{ID: "b", Type: config.BackendType(typ), BaseURL: fixture.BaseURL, ConnectTimeout: time.Second}
			transport := &recordingTransport{}
			module := kindOf(b.Type).build(b, &http.Client{Transport: transport}, fixture.Credential)
			serves, err := module.probe(context.Background())
			if err != nil {
				t.Fatalf("probe: %v", err)
			}

			if fixture.ModelsList == nil {
				// A type with no models list cannot tell which models it serves.
				if serves != nil {
					t.Error("the type has no models list, but the probe reports which models it serves")
				}
				if len(transport.requests) != 0 {
					t.Errorf("the type has no models list, but the probe sent %d requests", len(transport.requests))
				}
				return
			}
			if len(transport.requests) != 1 {
				t.Fatalf("the probe sent %d requests, want the models list's one", len(transport.requests))
			}
			req := transport.requests[0]
			if req.Method != http.MethodGet || req.URL.String() != fixture.ModelsList.URL {
				t.Errorf("probe %s %s, want GET %s", req.Method, req.URL, fixture.ModelsList.URL)
			}
			got := make(map[string]string)
			for name, values := range req.Header {
				if name != "Accept" && name != "User-Agent" {
					got[name] = strings.Join(values, ", ")
				}
			}
			want := make(map[string]string)
			for name, value := range fixture.ModelsList.Headers {
				want[http.CanonicalHeaderKey(name)] = value
			}
			if !maps.Equal(got, want) {
				t.Errorf("headers %v, want %v", got, want)
			}
		})
	}
}

// The config schema's backend-type enum names exactly the types kinds holds: a type
// the schema admits without a module would panic on first use (kindOf). The config
// package holds its schema walker's enum to the same list.
func TestKindsAreTheSchemaBackendTypes(t *testing.T) {
	var schema struct {
		Defs struct {
			Backend struct {
				Properties struct {
					Type struct {
						Enum []string `json:"enum"`
					} `json:"type"`
				} `json:"properties"`
			} `json:"backend"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(fixturetest.Read(t, fixturetest.SchemaFile("config.schema.json")), &schema); err != nil {
		t.Fatal(err)
	}
	enum := slices.Sorted(slices.Values(schema.Defs.Backend.Properties.Type.Enum))
	if names := kindNames(); !slices.Equal(enum, names) {
		t.Errorf("schema enum %v, kinds %v", enum, names)
	}
}

// kindNames is every backend type kinds holds, sorted.
func kindNames() []string {
	names := make([]string, 0, len(kinds))
	for typ := range kinds {
		names = append(names, string(typ))
	}
	slices.Sort(names)
	return names
}
