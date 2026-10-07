# Step 4 — fixture runners and cross-half pins

**Status:** not started

## Intent

The shared fixtures are run by one runner per half, and the facts each half states on its
own about backend types and config defaults are pinned by a test instead of by
attention.

## Findings

- test-scaffolding F3: Go — a test-only package (non-`_test` file, like `fakebackend`),
  e.g. `internal/fixturetest`, with `Files`, `InvalidCases`, `RunInvalid`,
  `DuplicateCases`; `control`'s `decoders` return errors, so `decodeByKind` goes. TS —
  the same helpers in step 5's test-support module (this step writes them there first,
  step 5 builds on it).
- T8 / edges hint: `protocol/fixtures/backend-types/` — per type, for a sample
  `base_url`: the models-list URL, the credential header name and form, extra headers
  (`anthropic-version`), whether the type lists models. One test per half: the gateway's
  provider modules (`url()`, `header()`) and `backend-verify`'s requests.
- T8 / config hint: a Go test tying the config schema's backend-type enum to
  `provider.kinds` (today a type in the schema but not in `kinds` panics on first use).
- config hint: a Go test comparing the schema's `default` keywords with what `resolve`
  gives `valid/minimal.json` (17 defaults stated twice, untied today).

## Files likely touched

- New `gateway/internal/fixturetest/`; `config/fixtures_test.go`,
  `control/fixtures_test.go`.
- `control/kaiak-control/src/config/config.test.ts`, `messages/messages.test.ts`,
  `messages/duplicate-members.test.ts`, new test-support module (see step 5).
- New `protocol/fixtures/backend-types/`; `gateway/internal/provider` (a fixture test);
  `control/kaiak-control/src/backend-verify` (a fixture test).
- `docs/specs/CONTROL-PROTOCOL.md` or `docs/TECH-STACK.md` wherever `protocol/fixtures/`
  layout is described.

## Decisions made during planning

- The backend-type fixture holds only what both halves must agree on (URL layout,
  credential header, list or no list). Everything verify-only (`owned_by`, `/props`,
  `max_model_len`) stays out.
- The optional larger change in F3 (config as one more kind under the messages layout)
  is not taken: no fixture moves.

## Acceptance criteria

- One fixture runner per half; adding a message kind is one entry.
- The backend-type fixture covers all seven types; both halves' tests read it; changing
  one URL in either half fails that half's test (checked once by hand, noted in the
  Result).
- The enum-vs-`kinds` and schema-defaults tests exist and pass.
- Fixture test counts unchanged or higher (record before/after).
- `scripts/check-all.sh` green.
