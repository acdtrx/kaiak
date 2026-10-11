# Step 2 — kaiak-control at protocol 7

**Status:** done — reviewed and committed 2026-10-11

## Intent

Move `kaiak-control` and the sample control plane to protocol 7: the one version, the
mandatory `endpoints` list, and one backend type per model.

## Files likely touched

- `control/kaiak-control/src/protocol/index.ts`: `PROTOCOL_VERSION` 7.
- `control/kaiak-control/schema/config.schema.json`: the packaged copy of the shared
  schema, kept byte-equal.
- `control/kaiak-control/src/config/`:
  - `types.ts`: `protocol_version` in place of `format_version`; `endpoints` required;
  - `semantic.ts`:
    - the new rule, every deployment on one type (its code from step 1);
    - `endpoint-not-served` judged against the model's one type;
  - `endpoint-support.ts`, `index.ts`: whatever the rules need from the table.
- `control/kaiak-control/src/test-support/` and every test building a config: models
  built with a list, once, centrally where possible.
- `control/sample/`: its config file and example, `protocol_version: 7`, a list on
  every model.
- `control/kaiak-control/GUIDE.md`: protocol 7, the version field, the two rules, what
  an app's model form needs (`MODEL_ENDPOINTS` for the choices; the type's endpoints).

## Decisions made during planning

- **The version is read from one constant:** `PROTOCOL_VERSION` is both the
  `Kaiak-Protocol` value and the `protocol_version` a config must carry.
- **No helper beyond the rules** (YAGNI): an app that wants to offer a type's endpoints
  in a form reads `ENDPOINT_SUPPORT`.

## Acceptance criteria

- Every shared fixture passes or fails in `kaiak-control` as its case says, the new
  ones included.
- Tests for each rule: a missing list, an empty list, two types, an entry the type does
  not serve, `protocol_version` 6, a `format_version` member.
- From `control/`, `npm test` and `npm run lint` green.
- `scripts/check-all.sh` run and recorded. **Expected reds:** the gateway's fixture and
  version tests (step 3); the cross-half e2e, now a protocol mismatch, until step 3.

## Result

### What changed

- `control/kaiak-control/schema/`: `npm run sync-schemas` — `config.schema.json`,
  `status.schema.json`, `usage-record.schema.json` byte-equal to `protocol/schema/`
  again.
- `src/protocol/index.ts`: `PROTOCOL_VERSION = 7`; its comment names it as both the
  `Kaiak-Protocol` value and the `protocol_version` a config and a status carry.
- `src/config/`:
  - `types.ts`: `Config.protocol_version: typeof PROTOCOL_VERSION` (as
    `GatewayStatus` already had) in place of `format_version: 6`; `Model.endpoints`
    required; the comments on `deployments` and `endpoints` state the two rules;
  - `semantic.ts`: `checkEndpoints` becomes `checkBackendType` — a model on an
    unknown backend reports `deployment-backend-unknown` alone; otherwise
    `deployment-types-differ` once, at `/models/<m>/deployments/<i>/backend` of the
    first deployment whose type differs from the first one's (then the list is not
    judged); otherwise `endpoint-not-served` per entry the one type does not serve.
    New code in `SemanticRuleCode`;
  - `endpoint-support.ts`: comment (judged against the one type; a value for apps);
  - `index.ts`, and the package entry `src/index.ts`: `ENDPOINT_SUPPORT` exported.
- `src/test-support/index.ts`: `CONFIG_FORMAT_VERSION` (read from the schema's
  `format_version`) removed. Every other config the tests build comes from the shared
  fixtures (`minimal.json`, `full.json`), which already list their endpoints — so no
  central model builder was needed.
- Tests:
  - `protocol.test.ts`: the version 7 and its mismatches; new — the config and status
    schemas' `protocol_version` const is `PROTOCOL_VERSION`;
  - `config.test.ts`: "endpoints list rule" (which tested the union of mixed types)
    rewritten as "one backend type per model and its endpoints": one type passes;
    each unserved entry at its index; types differing reported once at the third
    deployment, the list not judged; an unknown backend reported alone;
  - `limits.test.ts`, `sample/src/config-file/config-file.test.ts`:
    `protocol_version: PROTOCOL_VERSION` in place of the format version.
  - The rule cases the step names run as shared fixtures: `endpoints-missing`,
    `endpoints-empty`, `deployment-types-differ`, `endpoints-types-differ`,
    `endpoints-not-served`, `protocol-version-6`, `protocol-version-missing`,
    `format-version-member` (config), and `status/invalid/protocol-version-6`.
- `control/sample/`: it has no config file of its own — it runs on
  `examples/config.json` (moved in step 1: protocol 7, a list on every model, one
  type per model, checked by the "example configs" tests); nothing in `sample/src`
  names a version or endpoints but the one test above.
- `GUIDE.md`: header "It matches protocol version 7."; §3 the composed config states
  the version; §7 `protocol_version` (from `PROTOCOL_VERSION`, refused when another,
  a bump can change the document), one backend type per model
  (`deployment-types-differ`, the failover cost), the required list
  (`endpoint-not-served`, what to list), and what a model form needs (backends of one
  type; `ENDPOINT_SUPPORT[type]` as the endpoint choices, by `MODEL_ENDPOINTS`' names).

### Decisions made here

- **`ENDPOINT_SUPPORT` exported** from the package: the step's planning decision has
  apps read it, and it was internal to `src/config/`. No other helper.
- **The version from one constant:** the type ties `Config.protocol_version` to
  `PROTOCOL_VERSION`, and a test holds the config and status schemas' const to it;
  validation stays the schema's const (no second check in code).
- **Rule messages:** `deployment-types-differ` names the backend and both types;
  `endpoint-not-served` names the model's type.

### Grep checklist (`git grep`, `control/`, lock file aside)

`format_version` 0 · `formatVersion` 0 · `FORMAT_VERSION` 0 · "config format" 0 ·
"format 6" 0 · "format version" 0 · "version 6" 0 · `endpoints?:` 0 · "narrows" 0 ·
"absent = all" 0 · "optional `endpoints`" 0 · "deployments' types" 0 ·
`checkEndpoints` 0.

### Suite

- From `control/`: `npm test` — 672 tests, 671 pass, 0 fail, 1 skipped (the store
  contract's reconnect test, skipped for the memory store as before); `npm run lint` —
  `tsc` clean, "boundaries ok".
- `scripts/check-all.sh` stops at the gateway stage, as expected. `go test` red
  packages, all step 3:
  - `cmd/kaiak` (15 tests), `internal/config` (23), `internal/control` (28): the
    fixtures, examples and status at protocol 7;
  - `internal/server` `TestEveryErrorCodeHasItsClass`: `upstream_endpoint_missing`
    checked in code but gone from the Client API table.

  Every other package green, `e2e` included (`TestSeriesStartAtZero` passed this
  run). The live kit's checks did not run (the script stops at `go test`).
- Cross-half e2e, run apart (`go test -race -tags crosshalf -run '^TestAcrossHalves'
  -count=1 ./e2e`): red, `TestAcrossHalves` and `TestAcrossHalvesReplicas` — the
  test's totals stream sends the gateway's `Kaiak-Protocol` 6 and the sample answers
  `400`; the sample also refuses the e2e's own config document (`format_version: 6`,
  no `endpoints`). Both are gateway-side — `control.ProtocolVersion` and the
  `gateway/e2e` config documents — cleared by step 3.
