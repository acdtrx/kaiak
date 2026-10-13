# Step 2 — `kaiak-control` at protocol 8

**Status:** done

## Intent

Move `kaiak-control` and the sample to protocol 8: the version, the group's required
`capture`, and the guide telling apps what it means and who decides.

## Files likely touched

- `control/kaiak-control/src/protocol/index.ts` (`PROTOCOL_VERSION = 8`) and its test.
- `control/kaiak-control/schema/` — the synced copy (`npm run sync-schemas`).
- `control/kaiak-control/src/config/types.ts` — `Group.capture: boolean` (required).
- The resolved-fixture test (`src/config/limits.test.ts`): `expected` groups carry
  `capture`; the test reads it from the group itself — no resolution function: the
  flag is not resolved along the path.
- TypeScript tests that build groups inline (`config.test.ts`, `limits.test.ts`,
  `config-publishing.test.ts`, `control-plane.test.ts`) — through one helper where
  one fits.
- `control/kaiak-control/GUIDE.md`:
  - protocol 8;
  - **a section on capture:** what the flag does (`GATEWAY.md` → Request capture);
    the app decides it for every group — consent and privacy are the app's; any rule
    such as "a team off means its sub-groups off" is the app's to apply when it
    writes each group; the gateway reads only the key's own group; captures go to the
    trace destination its operator configured, and mapping groups and keys to access
    there is the app's job (decisions 4, 6, 7).
- `control/sample`: the page's group tree shows each group's capture (a tag, as labels
  are shown); its tests.

## Decisions made during planning

- **No new API:** `capture` is a config field like any other; apps write it in the
  documents they publish.

## Acceptance criteria

- `npm test` and `npm run lint` green in `control/`; the schema copy in sync.
- The guide's capture section states who decides and what the gateway reads.
- The sample page shows capture per group (a test asserts it).
- The cross-half e2e is red until step 3 (expected, recorded).

## Result

### What changed

- `control/kaiak-control/schema/`: `npm run sync-schemas` — `config.schema.json`
  (const 8, `capture` required on `$defs.group`) and `status.schema.json` (const 8)
  byte-equal to `protocol/schema/` again.
- `src/protocol/index.ts`: `PROTOCOL_VERSION = 8`; the repeated-header example
  `"8, 8"`. `protocol.test.ts`: the version 8 passes; `7`, `9`, `8.0`, `""`, `" 8"`,
  missing and `"8, 8"` are mismatches (the schemas' const test already holds both
  schemas to the constant).
- `src/config/types.ts`: `Group.capture: boolean`, required, its comment stating the
  rule (the group's own, not inherited, not in `child_defaults`, ancestors never
  read; any propagation is the app's) and pointing to `GATEWAY.md` → Request capture.
- `src/test-support/index.ts` — **the one helper for groups in tests**:
  - `group(fields?)`: a `Group` with `capture: false` unless `fields` set it
    (`GroupFields` = `Group` with `capture` optional);
  - `configWithGroups(groups)`: `minimal.json` with `me` plus the given groups, each
    through `group` — the two identical local `withGroups` helpers
    (`config.test.ts`, `config-publishing.test.ts`) are gone, and
    `control-plane.test.ts`'s inline tree uses it too.
- Tests:
  - `config/limits.test.ts`: the resolved fixtures' `expected` groups carry
    `capture`, read from the group's own entry (`config.groups[id].capture`) — no
    resolution function; the fixtures' parent/child pairs that differ both ways make
    a resolved value fail. Its inline cycle config builds groups with `group`.
  - `config/config.test.ts`: the group tree rules build through `configWithGroups`
    and `group` (`perMinute` now typed `Limit[]`).
  - `config-publishing/config-publishing.test.ts`: `configWithGroups`; "groups
    keeping their parents publish, whatever else changes" also turns a group's
    capture on.
  - `fastify/status-totals.test.ts`: the slow readers' padding groups through
    `group`.
  - The new invalid fixtures (`group-capture-missing`, `-number`, `-string`,
    `child-defaults-capture`) and the moved examples run through the existing
    fixture runners; no change needed there.
- `control/sample/src/page/sections.ts`: each group's line in the tree shows its own
  flag as a tag right after its ID — `capture on` (a plain tag) or `capture off`
  (muted) — read from the group's entry, never resolved. `page.test.ts`: the tree
  test asserts every group of `full.json`'s flag (a child differing from its parent
  both ways: `users` off with `alice` and `carol` on) and the tag's place before
  labels and before `models:`.
- `control/kaiak-control/GUIDE.md`: header "protocol version 8"; §7
  `PROTOCOL_VERSION` (8); the group shape gains `capture`; a new paragraph
  **Capture: your app decides, for every group** — what the flag does (the three
  chat endpoints, request as sent and answer as received, OTel spans,
  `GATEWAY.md` → Request capture); required on every group with no default anywhere,
  a privacy choice whose consent and policy are the app's; never inherited, no place
  in `child_defaults`, propagation rules ("a team off means its sub-groups off") the
  app's to apply when writing each group, shown per group from
  `config.groups[id].capture` (`resolveScopes` does not carry it); the gateway reads
  only the key's own group; captures go to the one trace destination the operator
  switched on (a gateway without it captures nothing and warns), carry the key and
  group IDs and `kaiak.request.id`, and access in the tool is the app's to arrange;
  best effort, not a usage record. §12 gains "a parent's capture is not its
  children's".
- No sample config files at protocol 7 remain: the sample has none of its own, and
  `scripts/smoke-images.sh` and the README's local run start from
  `examples/local-config.json` (moved in step 1). A local `data/sample-config.json`
  copied earlier (gitignored) is now refused: copy the example again and paste the
  key entry back. Docs outside `control/` that still say protocol 7
  (`docs/DEPLOYMENT.md`'s upgrade notes, `docs/architecture/*.html` footers) are
  step 10's.

### Decisions made here

- **Test groups default to `capture: false`** in the helper only — the library and
  the schema hold no default; a test about capture states it.
- **The sample shows both states**, not only "on": every group states the flag, so
  the page does too; "on" is the prominent tag, "off" muted.

### Suite

- From `control/`: `npm test` — 679 tests, 678 pass, 0 fail, 1 skipped (the store
  contract's reconnect test, skipped for the memory store as before); `npm run lint`
  — `tsc` clean, "boundaries ok"; the schema copy's drift test passes.
- `scripts/check-all.sh` stops at the gateway stage, as expected — **expected reds,
  all cleared by step 3**:
  - `go test` red packages: `cmd/kaiak` (15 tests), `internal/config` (25),
    `internal/control` (28) — every failure a protocol 8 document refused
    (`/groups/<id>/capture: unknown field`, `/protocol_version: must be 7`), the
    status's `ProtocolVersion` 7, or the gateway schema const test; `cmd/kaiak`'s
    `TestRunExitsWithNoConfigAtBoot` reports goroutines left by its earlier tests
    that failed on the same refusal. Every other package green, `e2e` included (its
    configs are still at 7). gofmt, vet, staticcheck and the telemetry boundary
    pass; the live kit's checks did not run (the script stops at `go test`).
  - Cross-half e2e, run apart (`go test -race -tags crosshalf -run
    '^TestAcrossHalves' -count=1 ./e2e`): red, `TestAcrossHalves` and
    `TestAcrossHalvesReplicas` — the sample refuses the e2e's protocol 7 config
    document at startup, and answers `400` to the test's totals stream sent with
    `Kaiak-Protocol: 7`. Both gateway-side (`gateway/e2e` documents and
    `config.ProtocolVersion`), cleared by step 3.

### Fixture alignment

- `protocol/fixtures/config/resolved/full.json` ("the full valid fixture's groups")
  had every group's `capture` inverted from `config/valid/full.json` by step 1's
  conversion; aligned at the main session's request: its `config.groups` are
  identical to the valid fixture's `groups` again (only `global` differs, by
  design), and each `expected.groups.<id>.capture` is that group's own flag.
  `limits.test.ts` green (8/8), `npm test` 679 / 678 pass / 1 skipped, lint clean.
