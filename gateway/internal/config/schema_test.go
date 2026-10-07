package config

// The schema walker and the resolved snapshot against protocol/schema/config.schema.json
// itself, for the facts both state: the backend types, and the defaults of omitted
// fields.

import (
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"testing"
	"time"

	"kaiak/internal/fixturetest"
)

func readConfigSchema(t *testing.T) map[string]any {
	t.Helper()
	var schema map[string]any
	if err := json.Unmarshal(fixturetest.Read(t, fixturetest.SchemaFile("config.schema.json")), &schema); err != nil {
		t.Fatal(err)
	}
	return schema
}

// The walker admits the schema's backend types, in the schema's order; the provider
// package holds its modules to the same list.
func TestBackendTypesAreTheSchemaEnum(t *testing.T) {
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
	if enum := schema.Defs.Backend.Properties.Type.Enum; !slices.Equal(backendTypes, enum) {
		t.Errorf("walker %v, schema enum %v", backendTypes, enum)
	}
}

// Every "default" keyword of the schema is what resolve gives minimal.json, which sets
// none of those fields. Each default is read where the snapshot holds it: the queue
// and retry settings through minimal.json's one model, which inherits them from
// global, the backend timeouts through its one backend, disabled through its one key.
func TestSchemaDefaultsAreResolved(t *testing.T) {
	ms := func(d time.Duration) int64 { return d.Milliseconds() }
	resolved := map[string]func(s *Snapshot) any{
		"/$defs/global/properties/max_request_body_bytes":               func(s *Snapshot) any { return s.MaxRequestBodyBytes },
		"/$defs/global/properties/control_outage_grace_ms":              func(s *Snapshot) any { return ms(s.ControlOutageGrace) },
		"/$defs/global/properties/max_n":                                func(s *Snapshot) any { return s.MaxN },
		"/$defs/global/properties/max_sequences_per_request":            func(s *Snapshot) any { return s.MaxSequencesPerRequest },
		"/$defs/global/properties/max_embedding_inputs":                 func(s *Snapshot) any { return s.MaxEmbeddingInputs },
		"/$defs/global/properties/max_concurrent_requests_per_key":      func(s *Snapshot) any { return s.MaxConcurrentRequestsPerKey },
		"/$defs/global/properties/circuit/properties/failure_threshold": func(s *Snapshot) any { return s.Circuit.FailureThreshold },
		"/$defs/global/properties/circuit/properties/probe_interval_ms": func(s *Snapshot) any { return ms(s.Circuit.ProbeInterval) },
		"/$defs/global/properties/metrics/properties/key_id_label":      func(s *Snapshot) any { return s.KeyIDLabel },
		"/$defs/global/properties/metrics/properties/group_label":       func(s *Snapshot) any { return s.GroupLabel },
		"/$defs/queue_settings/properties/size":                         func(s *Snapshot) any { return s.Models["llama"].Queue.Size },
		"/$defs/queue_settings/properties/timeout_ms":                   func(s *Snapshot) any { return ms(s.Models["llama"].Queue.Timeout) },
		"/$defs/retry_settings/properties/max_attempts":                 func(s *Snapshot) any { return s.Models["llama"].MaxAttempts },
		"/$defs/backend/properties/connect_timeout_ms":                  func(s *Snapshot) any { return ms(s.Backends["local"].ConnectTimeout) },
		"/$defs/backend/properties/first_event_timeout_ms":              func(s *Snapshot) any { return ms(s.Backends["local"].FirstEventTimeout) },
		"/$defs/backend/properties/response_timeout_ms":                 func(s *Snapshot) any { return ms(s.Backends["local"].ResponseTimeout) },
		"/$defs/backend/properties/stall_timeout_ms":                    func(s *Snapshot) any { return ms(s.Backends["local"].StallTimeout) },
		"/$defs/key/properties/disabled":                                func(s *Snapshot) any { return s.Keys["k-me"].Disabled },
	}

	defaults := make(map[string]any)
	collectDefaults(readConfigSchema(t), "", defaults)
	if got, want := slices.Sorted(maps.Keys(defaults)), slices.Sorted(maps.Keys(resolved)); !slices.Equal(got, want) {
		t.Fatalf("schema defaults at %v, the test reads %v", got, want)
	}

	s := parseFixture(t, "minimal.json")
	for at, schemaDefault := range defaults {
		// Through JSON, so a Go integer compares with the schema's number.
		encoded, err := json.Marshal(resolved[at](s))
		if err != nil {
			t.Fatal(err)
		}
		var got any
		if err := json.Unmarshal(encoded, &got); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, schemaDefault) {
			t.Errorf("%s: resolved %v, schema default %v", at, got, schemaDefault)
		}
	}
}

// collectDefaults records the "default" keyword of the schema node at pointer at and
// of every schema below it. Members of properties, $defs and the like are names, and
// const, enum and examples hold values, not schemas: a property named "default" is
// not a keyword.
func collectDefaults(node any, at string, found map[string]any) {
	schema, ok := node.(map[string]any)
	if !ok {
		return
	}
	if d, ok := schema["default"]; ok {
		found[at] = d
	}
	for keyword, child := range schema {
		switch keyword {
		case "default", "const", "enum", "examples":
		case "properties", "$defs", "patternProperties", "dependentSchemas":
			members, _ := child.(map[string]any)
			for name, sub := range members {
				collectDefaults(sub, at+"/"+keyword+"/"+name, found)
			}
		default:
			if list, ok := child.([]any); ok {
				for i, sub := range list {
					collectDefaults(sub, fmt.Sprintf("%s/%s/%d", at, keyword, i), found)
				}
				continue
			}
			collectDefaults(child, at+"/"+keyword, found)
		}
	}
}
