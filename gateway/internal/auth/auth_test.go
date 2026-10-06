package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strings"
	"testing"
	"time"

	"kaiak/internal/config"
)

func hashOf(key string) string {
	sum := sha256.Sum256([]byte(key))
	return "sha256:" + hex.EncodeToString(sum[:])
}

const (
	workloadKey = "kaiak-test-workload"
	userKey     = "kaiak-test-user"
	disabledKey = "kaiak-test-disabled"
	expiringKey = "kaiak-test-expiring"
)

var expiresAt = time.Date(2026, 6, 30, 23, 59, 59, 0, time.UTC)

func testSnapshot(t *testing.T) *config.Snapshot {
	t.Helper()
	model := func(name string) string {
		return `"` + name + `": {
      "deployments": [{ "backend": "local", "model": "` + name + `" }],
      "metadata": { "context_length": 8192,
        "capabilities": { "streaming": true, "tools": false, "vision": false, "reasoning": false } } }`
	}
	doc := `{
  "format_version": 5,
  "global": {},
  "backends": { "local": { "type": "openai-compatible", "base_url": "http://localhost:8000/v1" } },
  "models": { ` + model("open") + `, ` + model("secret") + ` },
  "groups": {
    "research": {},
    "eval": { "parent": "research", "allowed_models": ["*"] },
    "users": { "child_defaults": { "allowed_models": ["open"] } },
    "ann": { "parent": "users" }
  },
  "keys": {
    "k-eval": { "hash": "` + hashOf(workloadKey) + `", "group": "eval" },
    "k-ann": { "hash": "` + hashOf(userKey) + `", "group": "ann" },
    "k-off": { "hash": "` + hashOf(disabledKey) + `", "group": "ann", "disabled": true },
    "k-exp": { "hash": "` + hashOf(expiringKey) + `", "group": "ann", "expires_at": "2026-06-30T23:59:59Z" }
  }
}`
	s, err := config.Parse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

var now = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func TestAuthenticateResolvesGroups(t *testing.T) {
	s := testSnapshot(t)

	id, err := Authenticate(s, "Bearer "+workloadKey, "", now)
	if err != nil {
		t.Fatal(err)
	}
	if id.KeyID != "k-eval" || id.Group != s.Groups["eval"] || !slices.Equal(id.Group.PathIDs, []string{"research", "eval"}) {
		t.Errorf("workload key resolved to %+v", id)
	}

	id, err = Authenticate(s, "bearer "+userKey, "", now)
	if err != nil {
		t.Fatal(err)
	}
	if id.KeyID != "k-ann" || id.Group != s.Groups["ann"] || !slices.Equal(id.Group.PathIDs, []string{"users", "ann"}) {
		t.Errorf("user key resolved to %+v", id)
	}
}

func TestAuthenticateRefusals(t *testing.T) {
	s := testSnapshot(t)
	cases := []struct {
		name, header string
		want         Code
		keyID        string
	}{
		{"missing", "", CodeMissingKey, ""},
		{"basic scheme", "Basic " + userKey, CodeMalformedKey, ""},
		{"no token", "Bearer", CodeMalformedKey, ""},
		{"empty token", "Bearer   ", CodeMalformedKey, ""},
		{"two tokens", "Bearer " + userKey + " extra", CodeMalformedKey, ""},
		{"bare key", userKey, CodeMalformedKey, ""},
		{"unknown", "Bearer kaiak-not-a-key", CodeUnknownKey, ""},
		{"disabled", "Bearer " + disabledKey, CodeDisabledKey, "k-off"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Authenticate(s, c.header, "", now)
			if err == nil || err.Code != c.want || err.KeyID != c.keyID {
				t.Fatalf("got %+v, want code %s key ID %q", err, c.want, c.keyID)
			}
			for _, secret := range []string{userKey, disabledKey} {
				if strings.Contains(err.Message, secret) {
					t.Errorf("message %q contains the key", err.Message)
				}
			}
		})
	}
}

func TestKeyIsValidThroughItsExpiryInstant(t *testing.T) {
	s := testSnapshot(t)
	if _, err := Authenticate(s, "Bearer "+expiringKey, "", expiresAt); err != nil {
		t.Errorf("at expires_at: %v, want valid", err)
	}
	_, err := Authenticate(s, "Bearer "+expiringKey, "", expiresAt.Add(time.Nanosecond))
	if err == nil || err.Code != CodeExpiredKey || err.KeyID != "k-exp" {
		t.Errorf("after expires_at: %+v, want expired", err)
	}
}

func TestAuthorizeModel(t *testing.T) {
	s := testSnapshot(t)
	user, _ := Authenticate(s, "Bearer "+userKey, "", now)
	workload, _ := Authenticate(s, "Bearer "+workloadKey, "", now)

	if err := user.AuthorizeModel("open"); err != nil {
		t.Errorf("user on an allowed model: %v", err)
	}
	if err := workload.AuthorizeModel("secret"); err != nil {
		t.Errorf("workload with * on an existing model: %v", err)
	}

	// "secret" exists but is not allowed; "ghost" does not exist. The answers must not
	// differ beyond the name the client sent.
	notAllowed := user.AuthorizeModel("secret")
	unknown := user.AuthorizeModel("ghost")
	if notAllowed == nil || unknown == nil {
		t.Fatalf("got %v and %v, want two refusals", notAllowed, unknown)
	}
	if notAllowed.Code != CodeModelNotFound || unknown.Code != CodeModelNotFound {
		t.Errorf("codes %s, %s; want %s", notAllowed.Code, unknown.Code, CodeModelNotFound)
	}
	if strings.Replace(notAllowed.Message, "secret", "ghost", 1) != unknown.Message {
		t.Errorf("messages differ: %q vs %q", notAllowed.Message, unknown.Message)
	}
	if err := workload.AuthorizeModel("ghost"); err == nil || err.Code != CodeModelNotFound {
		t.Errorf("* allowed a model that does not exist: %v", err)
	}
}
