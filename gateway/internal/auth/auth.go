// Package auth resolves a client's key to an identity and decides which models
// that identity may use. Past this package a key is its key ID; the key itself is never
// stored, returned or logged.
package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"kaiak/internal/clip"
	"kaiak/internal/config"
)

// Code names why a request was refused. The server maps codes to HTTP answers.
type Code string

const (
	CodeMissingKey    Code = "missing_key"
	CodeMalformedKey  Code = "malformed_key"
	CodeUnknownKey    Code = "unknown_key"
	CodeDisabledKey   Code = "disabled_key"
	CodeExpiredKey    Code = "expired_key"
	CodeModelNotFound Code = "model_not_found"
)

// Error is a refusal. Message is safe to show the client: it never contains the key.
// KeyID is set when the key was recognized (disabled, expired), for the log.
type Error struct {
	Code    Code
	Message string
	KeyID   string
}

func (e *Error) Error() string { return string(e.Code) + ": " + e.Message }

// Identity is an authenticated key: its ID and its group, whose Path gives the
// request's group scopes.
type Identity struct {
	KeyID   string
	Group   *config.Group
	allowed config.ModelSet
}

// Authenticate resolves the client's key to an identity in snapshot: the
// Authorization header's bearer token, or — when the request has no Authorization
// header — the x-api-key header's value, as the Anthropic SDKs send it
// (docs/specs/GATEWAY.md, Client API → Auth). A key is valid through its expires_at
// instant and expired after it; now is the request's start time.
func Authenticate(snapshot *config.Snapshot, authorization, apiKey string, now time.Time) (Identity, *Error) {
	key := apiKey
	switch {
	case authorization != "":
		var ok bool
		if key, ok = bearerToken(authorization); !ok {
			return Identity{}, &Error{Code: CodeMalformedKey,
				Message: "Malformed Authorization header. Send the API key as \"Bearer <key>\"."}
		}
	case apiKey == "":
		return Identity{}, &Error{Code: CodeMissingKey,
			Message: "No API key provided. Send it in the Authorization header as \"Bearer <key>\", or in the x-api-key header."}
	}
	sum := sha256.Sum256([]byte(key))
	k, found := snapshot.KeyByHash("sha256:" + hex.EncodeToString(sum[:]))
	if !found {
		return Identity{}, &Error{Code: CodeUnknownKey, Message: "Invalid API key."}
	}
	if k.Disabled {
		return Identity{}, &Error{Code: CodeDisabledKey, Message: "This API key is disabled.", KeyID: k.ID}
	}
	if !k.ExpiresAt.IsZero() && now.After(k.ExpiresAt) {
		return Identity{}, &Error{Code: CodeExpiredKey, Message: "This API key has expired.", KeyID: k.ID}
	}

	return Identity{KeyID: k.ID, Group: k.Group, allowed: k.AllowedModels()}, nil
}

// bearerToken extracts the token from an RFC 6750 "Bearer <token>" value; the scheme
// is case-insensitive, the token one non-empty run without spaces.
func bearerToken(authorization string) (string, bool) {
	scheme, token, ok := strings.Cut(authorization, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	token = strings.TrimLeft(token, " ")
	if token == "" || strings.ContainsAny(token, " \t") {
		return "", false
	}
	return token, true
}

// AllowedModels lists the models the identity's group path allows, sorted. The slice is
// shared: do not modify it.
func (id Identity) AllowedModels() []string {
	return id.allowed.Names()
}

// AuthorizeModel is the one check of model access: model must exist and be allowed
// for the identity's group path. Both failures give the same error, built only from the
// requested name (clipped: the client controls it), so a key never learns which other
// models exist.
func (id Identity) AuthorizeModel(model string) *Error {
	// A group's allowed set holds only models of its snapshot, so one lookup answers
	// both "exists" and "allowed".
	if id.allowed.Allows(model) {
		return nil
	}
	return &Error{Code: CodeModelNotFound,
		Message: fmt.Sprintf("The model `%s` does not exist or you do not have access to it.", clip.String(model))}
}
