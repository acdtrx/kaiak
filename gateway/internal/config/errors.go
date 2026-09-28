package config

import (
	"fmt"
	"slices"
	"strings"
)

// Codes for rejections that are not semantic rules. A schema violation has no per-rule
// code: CodeSchema plus the validator's own message (docs/specs/CONTROL-PROTOCOL.md,
// Config).
const (
	// CodeSyntax: the document is not a single JSON value.
	CodeSyntax = "syntax"
	// CodeDuplicateMember: an object in the document names the same member twice
	// (JSON leaves the meaning open; decoders disagree on which one wins).
	CodeDuplicateMember = "duplicate-member"
	// CodeSchema: the document breaks config.schema.json.
	CodeSchema = "schema"
	// CodeAPIKeyEnvUnset: a backend's api_key_env names a variable that is unset or
	// empty in the gateway's environment. Gateway-side only: it depends on the process
	// environment, so the shared fixtures do not cover it.
	CodeAPIKeyEnvUnset = "api-key-env-unset"
)

// Semantic rule codes. They are part of the contract (docs/specs/CONTROL-PROTOCOL.md,
// Config): kaiak-control reports the same code for the same document.
const (
	CodeKeyGroupUnknown                  = "key-group-unknown"
	CodeKeyHashDuplicate                 = "key-hash-duplicate"
	CodeGroupParentUnknown               = "group-parent-unknown"
	CodeGroupCycle                       = "group-cycle"
	CodeGroupDepthExceeded               = "group-depth-exceeded"
	CodeEffectiveLimitsExceeded          = "effective-limits-exceeded"
	CodeDeploymentBackendUnknown         = "deployment-backend-unknown"
	CodeAllowedModelUnknown              = "allowed-model-unknown"
	CodeAllowedModelsWildcardMixed       = "allowed-models-wildcard-mixed"
	CodeLimitModelUnknown                = "limit-model-unknown"
	CodeLimitDuplicate                   = "limit-duplicate"
	CodeOutputLimitDefaultAboveCeiling   = "output-limit-default-above-ceiling"
	CodeOutputLimitAboveContext          = "output-limit-above-context"
	CodeReasoningEffortsWithoutReasoning = "reasoning-efforts-without-reasoning"
	CodePriceDatesNotIncreasing          = "price-dates-not-increasing"
	CodeDateInvalid                      = "date-invalid"
	CodeTimestampInvalid                 = "timestamp-invalid"
)

// Issue is one reason a config document was rejected.
type Issue struct {
	Code string
	// Path is a JSON Pointer (RFC 6901) into the document; "" is the root.
	Path    string
	Message string
}

func (i Issue) String() string {
	path := i.Path
	if path == "" {
		path = "/"
	}
	return fmt.Sprintf("%s: %s [%s]", path, i.Message, i.Code)
}

// ValidationError rejects a config document and lists every issue found in the stage
// that failed (syntax, then duplicate members, then schema, then semantic rules, then
// the environment).
type ValidationError struct {
	Issues []Issue
}

func (e *ValidationError) Error() string {
	parts := make([]string, len(e.Issues))
	for i, issue := range e.Issues {
		parts[i] = issue.String()
	}
	return "config rejected: " + strings.Join(parts, "; ")
}

// Codes returns the distinct issue codes, sorted.
func (e *ValidationError) Codes() []string {
	codes := make([]string, 0, len(e.Issues))
	for _, issue := range e.Issues {
		codes = append(codes, issue.Code)
	}
	slices.Sort(codes)
	return slices.Compact(codes)
}
