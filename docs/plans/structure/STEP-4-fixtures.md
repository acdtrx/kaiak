# Step 4 — fixture runners and cross-half pins

**Status:** done (2026-10-08)

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

## Result

**What changed**

- `gateway/internal/fixturetest/` (new, test tooling like `fakebackend`): `Dir(parts…)`
  and `SchemaFile(name)` — the one statement of where `protocol/` is (seen from a
  package two levels below `gateway/`); `Read`, `Files`, `InvalidCase` /
  `InvalidCases` (the `cases.json` rules), `RunValid(t, dir, decode)`,
  `RunInvalid(t, dir, decode, schemaCode)` (entries equal files, then exactly
  `[schema]` or `[code]` per file; codes read through any error with `Codes()`, so
  `config` and `control` errors alike), `DuplicateCase` / `DuplicateCases(t)` (strict
  decode, entries equal files).
- `config/fixtures_test.go` — `invalidCase`, `fixtureFiles`, `readCases`,
  `duplicateCase` and the loops gone: `TestValidFixtures`, `TestExampleConfigs`,
  `TestInvalidFixtures` are one `RunValid`/`RunInvalid` call each over `parse`;
  the duplicate-member and resolution tests read through `fixturetest`.
  `fixturesDir` is `fixturetest.Dir("config")` (still read by `loader_test.go` and
  `snapshot_test.go`).
- `control/fixtures_test.go` — `decoders` return `(value, error)`, so codes (through
  `RunInvalid`) and the duplicate-member test's issue path come from the same
  decoder; `decodeByKind`, `codesOf`, `readFixture` and the copied helpers gone.
  Adding a message kind is one `decoders` entry. `fixturesDir` is
  `fixturetest.Dir("messages")` (still read by `client_test.go`).
- The other Go path literals of `protocol/fixtures` route through `fixturetest.Dir`:
  `cmd/kaiak/main_test.go` (2), `auth/keyfixture_test.go`, `provider/provider_test.go`,
  `control/client_test.go`.
- `control/kaiak-control/src/test-support/index.ts` (new subsystem): `fixturePath`,
  `fixture`, `readJson`, `CASES_FILE`, `fixtureFiles`, `InvalidCase`, `readCases`,
  and the two runners `testValidFixtures(dir, validate, prefix)` /
  `testInvalidFixtures(dir, validate, prefix)` (test names keep their prefixes).
  Used by `config.test.ts`, `config/limits.test.ts`, `messages.test.ts`,
  `duplicate-members.test.ts` and `backend-verify.test.ts`. Not in `exports`;
  `"!src/test-support"` in the package's `files` (`npm pack --dry-run` lists it no
  more) and excluded from the sample image's `Dockerfile.dockerignore`. The boundary
  lint needed nothing.
- `protocol/fixtures/backend-types/<type>.json` (new, all seven types): `base_url`,
  `credential`, and `models_list` — `{url, headers}` (headers besides `Accept` and
  `User-Agent`: the credential header with its value form, `anthropic-version` on
  `anthropic`) or `null` (`azure-anthropic`).
- `provider/backendtypes_test.go` (new): `TestBackendTypeFixtures` — fixture files
  equal `kinds`; per type, the module from `kindOf(type).build` over a recording
  transport, its real `probe`: one `GET` at the fixture's URL with exactly the
  fixture's headers, or no request when the fixture has no list, and `ListsModels`
  agreeing. `TestKindsAreTheSchemaBackendTypes` — the schema file's backend-type enum
  equals `kinds`.
- `config/schema_test.go` (new): `TestBackendTypesAreTheSchemaEnum` — the walker's
  `backendTypes` equals the schema file's enum, in order (the TS pin's Go twin);
  `TestSchemaDefaultsAreResolved` — every `default` keyword in
  `config.schema.json` (found by a schema-aware walk: a property *named* `default`,
  `output_limit.default`, is not one) equals what `Parse` (→ `resolve`) gives
  `valid/minimal.json`, read where the snapshot holds it; a default the test does not
  read fails it.
- `backend-verify.test.ts`: "each type's models-list request is the gateway's
  (protocol/fixtures/backend-types)" — fixture files equal `BACKEND_TYPES`; per type,
  `verifyBackend` with `fetch` mocked (`t.mock.method`): `ok` only with a list, no
  request without one, else one `GET` at the fixture's URL with exactly its headers.
- Docs: `BACKEND-VERIFY.md` Requests (the fixture pins the table, settled
  2026-10-08); `TECH-STACK.md` and `ARCHITECTURE.md` (`protocol/` holds it);
  `ARCHITECTURE.md` lists `fixturetest` and `test-support`.

**Decisions made during the step**

- Enum vs `kinds` is pinned through the schema file, with no new export: `config`
  ties its walker's list to the file's enum, `provider` ties `kinds` to it. Together
  they say walker = `kinds`; each test lives where it sees its list.
- The fixture models the one request both halves send — the models list — so the
  credential header is pinned as that request carries it. Request URLs other than the
  list's are gateway-only (the e2e table of step 3 states them).
- One file per type, so a new type is one file and both halves' tests fail until it
  exists. Sample hosts are the spec's (`api.openai.com`, `my-resource.openai.azure.com`,
  …); neither test opens a connection (Go: a recording `RoundTripper`; TS: mocked
  `fetch`).
- `RunValid` and the TS runners were added beyond the brief's list: the valid loop
  was the same in three places per half.
- The Go valid/invalid message tests now run a subtest per kind, then per file:
  the same full names (`TestInvalidMessageFixtures/config-event/x.json`), one extra
  `=== RUN` line per kind.
- Assertion messages only: messages' "the message is rejected" now reads "the
  document is rejected"; `TestSyntaxError` checks with `errors.As` instead of
  `codesOf` (same condition).

**Report vs code** (034329e line numbers; code unchanged since in these files): as
reported, except `config/fixtures_test.go` `TestInvalidFixtures` is at :108 (report
:158), `control/fixtures_test.go` `TestInvalidMessageFixtures` :171 (:173) and
`decodeByKind` :367 (:366). The schema has **18** `default` keywords, not 17: the
18th, `key.disabled: false`, has no `Default*` constant (Go's zero value) and is
pinned too. No disagreement between gateway, backend-verify and the specs
(`GATEWAY.md` Base URLs / credentials, `BACKEND-VERIFY.md` Requests).

**By-hand break checks** (each reverted with `git checkout` of the untouched file)

- Go `anthropic.go` list `?limit=100` → `TestBackendTypeFixtures/anthropic` fails
  (`probe GET …?limit=100, want …?limit=1000`).
- Go `azure_openai.go` header `X-Api-Key` → `/azure-openai` fails (headers differ).
- TS `modelsListUrl` azure-openai `/openai/v2/models` → the fixture test fails
  (actual vs expected URL).
- TS `requestHeaders` without `anthropic-version` → fails (`anthropic: headers`).
- Go `DefaultMaxN = 9` → `TestSchemaDefaultsAreResolved` fails at `/$defs/global/properties/max_n`.
- Go walker without `azure-anthropic` → `TestBackendTypesAreTheSchemaEnum` fails;
  `kinds` without it → `TestKindsAreTheSchemaBackendTypes` fails.

**Test inventory** (before → after; names diffed: every old name kept)

- Go `go test -list`: `config` 32 → 34 (+`TestBackendTypesAreTheSchemaEnum`,
  `TestSchemaDefaultsAreResolved`); `control` 76 → 76; `provider` 52 → 54
  (+`TestBackendTypeFixtures`, `TestKindsAreTheSchemaBackendTypes`).
- Go `=== RUN` with subtests: `config` 202 → 204; `control` 228 → 240 (the 12
  per-kind levels above); `provider` 192 → 201 (the two tests, seven type subtests).
- TS `npm test` (control): 614 → 615 tests (613 → 614 pass, 1 skipped as before).
  Per file: `config.test.ts` 164 → 164, `limits.test.ts` 8 → 8, `messages.test.ts`
  117 → 117, `duplicate-members.test.ts` 8 → 8 (names identical);
  `backend-verify.test.ts` 27 → 28 (the new test).


**Suite** — `scripts/check-all.sh`, green:

```
==> gofmt / go vet / staticcheck 2026.2.1 (gateway)
==> go test -race -count=1 (gateway): ok cmd/kaiak, e2e, accounting, auth, clip,
    config, control, limits, logattr, metrics, netfail, otlplog, provider, routing,
    schemacheck, server, sse
==> gofmt / go vet / staticcheck (live-test kit)
==> live-test kit self-test: passed for vllm, llama-server, openai, azure-openai,
    anthropic, azure-anthropic, vllm with two backends
gateway checks passed
==> npm test (control): 615 tests, 614 pass, 0 fail, 1 skipped
==> npm run lint (control): boundaries ok
==> cross-half e2e (sample control plane + two gateways; two cores over one store)
ok  	kaiak/e2e	65.347s
all checks passed
```
