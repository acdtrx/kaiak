# Step 2 — kaiak-control at protocol 7

**Status:** not started

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
