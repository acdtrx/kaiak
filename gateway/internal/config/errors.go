package config

// A rejected config is a *schemacheck.ValidationError with this subject: its text
// reads "config rejected: …". Syntax, duplicate members and schema violations carry
// schemacheck's stage codes; the codes below are config's own
// (docs/specs/CONTROL-PROTOCOL.md, Config).
const rejectedSubject = "config"

// CodeAPIKeyEnvUnset: a backend's api_key_env names a variable that is unset or empty
// in the gateway's environment. Gateway-side only: it depends on the process
// environment, so the shared fixtures do not cover it.
const CodeAPIKeyEnvUnset = "api-key-env-unset"

// Semantic rule codes. They are part of the contract (docs/specs/CONTROL-PROTOCOL.md,
// Config): kaiak-control reports the same code for the same document.
const (
	CodeKeyGroupUnknown                  = "key-group-unknown"
	CodeKeyHashDuplicate                 = "key-hash-duplicate"
	CodeGroupParentUnknown               = "group-parent-unknown"
	CodeGroupCycle                       = "group-cycle"
	CodeGroupDepthExceeded               = "group-depth-exceeded"
	CodeCountersExceeded                 = "counters-exceeded"
	CodeDeploymentBackendUnknown         = "deployment-backend-unknown"
	CodeAllowedModelUnknown              = "allowed-model-unknown"
	CodeAllowedModelsWildcardMixed       = "allowed-models-wildcard-mixed"
	CodeLimitDuplicate                   = "limit-duplicate"
	CodeOutputLimitDefaultAboveCeiling   = "output-limit-default-above-ceiling"
	CodeOutputLimitAboveContext          = "output-limit-above-context"
	CodeReasoningEffortsWithoutReasoning = "reasoning-efforts-without-reasoning"
	CodePriceDatesNotIncreasing          = "price-dates-not-increasing"
	CodePriceTierFirstNotZero            = "price-tier-first-not-zero"
	CodePriceTiersNotIncreasing          = "price-tiers-not-increasing"
	CodeDateInvalid                      = "date-invalid"
	// CodeTimestampInvalid is also a protocol message rule's: a timestamp names an
	// instant that does not exist, wherever the shared $defs timestamp appears.
	CodeTimestampInvalid = "timestamp-invalid"
)
