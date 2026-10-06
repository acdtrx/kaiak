package auth

// The shared key fixtures in protocol/fixtures/keys/ pair a key in the settled format
// with the hash kaiak-control's key generation puts in config. A key must authenticate
// against its fixture hash, taken verbatim, so the two halves hash keys the same way.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"kaiak/internal/config"
)

const keyFixturesDir = "../../../protocol/fixtures/keys"

// keyFormat is the settled key format (docs/specs/CONTROL-PROTOCOL.md, Config).
var keyFormat = regexp.MustCompile(`^kaiak-[A-Za-z0-9]{43}$`)

func TestSharedKeyFixturesAuthenticate(t *testing.T) {
	entries, err := os.ReadDir(keyFixturesDir)
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		found++
		t.Run(e.Name(), func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join(keyFixturesDir, e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			dec := json.NewDecoder(strings.NewReader(string(data)))
			dec.DisallowUnknownFields()
			var fixture struct {
				Key  string `json:"key"`
				Hash string `json:"hash"`
			}
			if err := dec.Decode(&fixture); err != nil {
				t.Fatal(err)
			}
			if !keyFormat.MatchString(fixture.Key) {
				t.Fatalf("key %q is not in the settled format", fixture.Key)
			}
			snapshot, err := config.Parse([]byte(`{
  "format_version": 5,
  "global": {},
  "backends": { "local": { "type": "openai-compatible", "base_url": "http://localhost:8000/v1" } },
  "models": { "m": { "deployments": [{ "backend": "local", "model": "m" }],
    "metadata": { "context_length": 8192,
      "capabilities": { "streaming": true, "tools": false, "vision": false, "reasoning": false } } } },
  "groups": { "ann": {} },
  "keys": { "k-fixture": { "hash": "` + fixture.Hash + `", "group": "ann" } }
}`))
			if err != nil {
				t.Fatal(err)
			}
			id, authErr := Authenticate(snapshot, "Bearer "+fixture.Key, "", now)
			if authErr != nil {
				t.Fatalf("the fixture key does not authenticate against its hash: %v", authErr)
			}
			if id.KeyID != "k-fixture" {
				t.Errorf("KeyID = %q, want k-fixture", id.KeyID)
			}
		})
	}
	if found == 0 {
		t.Fatalf("no key fixtures in %s", keyFixturesDir)
	}
}
