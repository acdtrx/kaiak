# Step 8 — `starting` off both halves; control-side leftovers

**Status:** done (2026-10-08)

## Intent

Remove the unreachable `starting` status from the protocol on both halves (decision 4),
and the control-side leftovers: a totals API shaped like a gateway, read by one page
with a fake instance, and a sample package entry nothing imports.

## Findings

- control-main F5 / decision 4 — `starting`, both halves in one change:
  - gateway: `currentStatus` reports `ready` or `draining`; the `Applier.Loaded()` read
    and `TestStatusIsStartingUntilAConfigIsApplied` go; `StateStarting` and its walker
    value go;
  - `protocol/schema/status.schema.json` (and the synced `kaiak-control/schema/` copy),
    `protocol/fixtures/messages/status/valid/starting.json`;
  - `kaiak-control` `GatewayState`; the sample's `.state-starting` CSS;
  - `docs/specs/CONTROL-PROTOCOL.md` ("Accepted before any config is published: a
    starting gateway…").
- control-core F2, edges F3(a) — `ControlPlane.totals(instance)`, `Usage.totals` and
  `countedThrough` go. The sample page reads `core.readTotals()`: `windowStarts[type]`
  for the start and `liveGateways` for the count; `PAGE_READER`, `HOUR_MS` and
  `monthStart` go. The ~21 test call sites read `readTotals()` or the stream. `GUIDE.md`
  §9 says `readTotals()` (step 26 trims the rest).
- edges F5 — `control/sample/src/index.ts` and the `exports` field go.
- control-main small item — `currentStatus` and `servingStatus` both guard
  `Deployments != nil` and copy `Serving` field by field: keep one guard (with step 19
  in mind, which moves `servingStatus`).

## Files likely touched

- `gateway/internal/control/{status,messages,schema}.go`, `status_test.go`.
- `protocol/schema/status.schema.json`, `protocol/fixtures/messages/status/`,
  `control/kaiak-control/schema/` (sync script), `kaiak-control/src/messages/types.ts`.
- `kaiak-control/src/{usage,control-plane}/index.ts` and tests; `fastify/gateway-stream.ts`
  (keeps the one `counted_through` filter).
- `control/sample/src/page/{sections,format}.ts`, CSS, `src/index.ts`, `package.json`.
- `docs/specs/CONTROL-PROTOCOL.md`, `control/kaiak-control/GUIDE.md` §9.

## Decisions made during planning

- No protocol version bump (decision 9): no current gateway sends `starting`, and both
  halves move together.
- `readTotals` keeps its name; renaming it is not worth a second API change.

## Removal checklist (clean at phase end)

- `git grep -nwi 'starting' -- protocol/ gateway/internal/control control/ docs/specs/CONTROL-PROTOCOL.md`
  → no status-state use (other meanings of the word are fine; list what remains).
- `git grep -nE '\.totals\(|countedThrough|PAGE_READER' control/` → none.
- `git ls-files control/sample/src/index.ts` → none.

## Acceptance criteria

- Status schema, fixtures, both decoders and the spec agree on `ready | draining`; the
  shared fixtures pass on both halves.
- The sample page shows the same spend and window starts as before (its tests).
- `scripts/check-all.sh` green, or reds named with the step that clears them.

## Result

**What changed**

- `starting` off both halves (decision 4; no protocol version bump, decision 9):
  - Gateway:
    - `currentStatus` builds the status as `ready` and sets `draining` from the drain's
      start. Its comment says why: `Run` starts only after a boot that found a config.
    - The `Applier.Loaded()` read is gone. `Applier.Loaded()` had no other caller, so
      it is removed too (`config/loader.go`). `Holder.Loaded()` stays: the loader,
      the admin readiness check and tests read it.
    - `StateStarting` is gone from `messages.go`, and so is the walker's `starting`
      value (`outbound_test.go`'s `states`).
  - Protocol: `starting` is out of `status.schema.json`'s `state` enum.
    `valid/starting.json` is deleted. `invalid/cases.json`'s `state-unknown.json`
    reason reads "state is ready or draining". `kaiak-control/schema/` is synced with
    `npm run sync-schemas` (from `control/`).
  - `kaiak-control`: `GatewayState` is `"ready" | "draining"`. The status route's
    comment says a gateway booted from its seed config reports with nothing applied.
  - Sample: `.state-starting` is gone from the page CSS.
  - Docs:
    - `CONTROL-PROTOCOL.md`: the Status bullet lists `ready` or `draining` (settled
      2026-10-08, with the reason). The status-traffic list is updated. "Accepted
      before any config is published" now names a seed-booted gateway.
    - `GATEWAY.md` (Status minimum gap → State): "there is no `starting` (settled
      2026-10-08)".
    - `docs/architecture/control-plane.html`: the boot sequence's first arrow
      (`POST /v1/status state=starting`) is gone, along with its mention in the
      aria-label. The status-field list says `ready`, `draining`. The arrow was stale:
      the binary's first status follows the boot.
- `totals(instance)` gone (control-core F2, edges F3(a)):
  - `Usage.totals`, `ControlPlane.totals` and the exported `countedThrough` are gone.
    The one `counted_through` filter is the inline one in `fastify/gateway-stream.ts`,
    unchanged. `TotalsRead`'s comment says a host shows its windows against its limits.
  - Sample page:
    - `renderTotals` reads `core.readTotals()` and shows `liveGateways`.
    - A limit with nothing used shows `windowStarts[limit.type]`. If the start does
      not parse, the page shows the text as it is, as the circuit's `opened_at` does.
      Before, it fell back to the clock.
    - `PAGE_READER`, `HOUR_MS` and `monthStart` are gone, and so is the page's use of
      `clock` in this section. `PageSources.core` and `StatusPageOptions.controlPlane`
      pick `readTotals` in place of `totals`. `usdToNano` stays (edges F3(b) is out of
      scope).
  - `GUIDE.md` §9: the table row and the snippet read `readTotals()`. The snippet also
    says where the window start is (`totals.windowStarts[limit.type]`).
  - The 21 test call sites:
    - `control-plane.test.ts` ×10 and `usage-route.test.ts` ×3 read `readTotals()`.
      `liveGateways` replaces `live_gateways`, and `cursors` (with `instance`)
      replaces `counted_through`.
    - `status-totals.test.ts` ×4: the "pushed equals the totals" comparisons use a
      local `completeTotals(core, counted_through)`. It builds the expected message
      from `readTotals()` with the `counted_through` given as a literal.
    - `usage.test.ts` ×3: `currentTotals` returns the sorted `TotalsRead`. Its
      windows and live count are still validated as a totals message.
    - The sample's `app.test.ts` ×1 reads `readTotals().cursors`.
    - The sample's `page.test.ts` / `events.test.ts` fakes serve a `TotalsRead`. The
      page test's `windowStarts` are the windows its fixed clock gave before.
- Sample entry (edges F5):
  - `control/sample/src/index.ts` and `"exports"` in `control/sample/package.json` are
    gone. Nothing needed them:
    - nothing imports `kaiak-sample`;
    - `check-boundaries.ts` checks only relative imports and subsystem `index.ts`
      files;
    - the Dockerfile runs `sample/src/main.ts`, and its dockerignore copies `src`
      whole.
  - Exports that only the entry used lose `export`: `STARTUP_TRIGGER` (app),
    `DEFAULT_LISTEN` (settings) and `formatKey` (keygen). These stay exported:
    - `WATCH_TRIGGER`, because `config-file.test.ts` reads it;
    - the types that appear in exported signatures (`SampleAppOptions`,
      `ProtocolReplica`, `KeygenResult`, `VerifyResult`, …), because they are those
      functions' contracts.
  - `ARCHITECTURE.md`: `kaiak-control` exposes one entry; `sample` is an app with
    process entries only.
- One non-nil guard (control-main small item):
  - The guard stays in the producer. `servingStatus` already makes every collection
    non-nil, removed backends' `Deployments` included.
  - `currentStatus` takes `serving.Backends` and `serving.Models` as they are, so
    its field-by-field copy and the `maps` import are gone.
  - The rule is part of `control.Serving`'s contract: "Its collections are non-nil,
    every backend's Deployments included: the status schema refuses null".
    `servingStatus`'s comment points to it. `servingStatus` did not move (step 19
    moves it).

**Decisions made during the step**

- Guard placement: the guard stays in the producer because then no assertion changes:
  - `TestServingStatusCoversTheAppliedConfig` keeps asserting that a retired backend
    has non-nil, empty `Deployments` and that a config-less result has non-nil maps;
  - `TestFirstStatusReportsWhatTheGatewayRuns` keeps its "gone" assertion. Its
    `Serving` input now gives `"gone"` an empty `Deployments`, as the contract
    requires.

  With the guard in `currentStatus` instead, the producer's test would have lost an
  assertion (decision 17). The status tests decode every report with the schema
  walker, so a test producer that broke the contract would fail loudly.
- Tests whose input was `starting` but whose subject stays:
  - `status-totals.test.ts` "a status before any config is accepted" now posts a
    seed-booted status (`ready`, `applied_config_hash: null`). That is the status a
    gateway really sends before any publish.
  - `gateways.test.ts` "a status stores the latest report…" sends a first report with
    `applied_config_hash: null` where it had `state: "starting"`. That keeps the
    second report different from the first.

  The assertions of both tests are unchanged.
- `counted_through`'s per-instance filter is the stream's job now, so its assertions
  moved there:
  - `usage.test.ts` "counted_through is the recipient instance's last counted batch of
    each epoch" is renamed "the cursors are each instance's last counted batch of each
    epoch". It asserts the read's full cursor list, gw-2's cursor included.
  - The gw-2 and gw-3 filter assertions moved to a new `status-totals.test.ts` test,
    "counted_through is the stream's instance's last counted batch of each epoch":
    - a gw-2 stream gets `[B/7]`;
    - a gw-1 stream gets `[A/3, B/2]`;
    - a gw-3 stream gets `[]`.

    With the stream's filter made a pass-through, this test fails.
- Totals validation in `usage.test.ts`: `currentTotals` validates the read's windows
  and live count as a totals message, with an empty `counted_through`. The library no
  longer builds a gateway's `counted_through` outside the stream. The stream tests
  validate every pushed message, `counted_through` included (`totalsOf`).
- Upgrade notes: `docs/DEPLOYMENT.md` → Upgrades was not edited. The removed
  `totals(instance)` and status `starting` are host-visible `kaiak-control` and
  protocol changes. Whether the release notes name them is for the release.

**Report vs code** (034329e; code at 596cc8a)

- control-main F5:
  - `status.go:168,181-188`, `messages.go:106` and `status_test.go:84-96` are as
    reported.
  - `schema.go:31`'s `states` list is now in `outbound_test.go:20`, where step 7
    moved it.
- The guard small item: `main.go`'s guard is at :775-782 (report: :757-779, the whole
  function), and `status.go` is at :189-198, as reported.
- control-core F2: `usage/index.ts:59-61,216-219,235-240`, `control-plane/index.ts:63`
  and GUIDE :594/:603 are as reported. The `gateway-stream.ts` filter is at :102-104
  (report: :113-115).
- Call sites: the code has 21, as the report says, but `control-plane.test.ts` has
  10 of them (report: 6), because step 5 folded the review/round tests in.
- edges F3(a): `sections.ts`'s `PAGE_READER` is at :227, `HOUR_MS` at :229, `totals`
  at :233 and `monthStart` at :292, as reported.
- edges F5: as reported. `WATCH_TRIGGER` is also read by its module's test, so it
  stays exported.
- Not in the reports:
  - `GATEWAY.md` (Status minimum gap → State), which named `starting`;
  - the `fastify/index.ts` status-route comment;
  - `docs/architecture/control-plane.html`: the boot arrow, the aria-label and the
    field list;
  - `ARCHITECTURE.md`'s "Each package exposes one entry";
  - two TS tests that used `starting` as input.

**Tests** (before → after)

| Suite | Count |
|---|---|
| TS `npm test` | 615 → 615 tests, 45 suites (614 pass, 1 skipped, as before) |
| Go `internal/control` | `-list` 76 → 75, `=== RUN` 240 → 237 |
| Go `cmd/kaiak` | `-list` 29 → 29, `=== RUN` 33 → 33 |
| Go `internal/config` | `-list` 34 → 34, `=== RUN` 204 → 204 |

- Deleted for asserting removed behaviour (decision 16):
  - Go `TestStatusIsStartingUntilAConfigIsApplied`;
  - the fixture cases for `status/valid/starting.json`: TS `valid/starting.json`, and
    Go `TestValidMessageFixtures/status/starting.json` and
    `TestValidMessageFixturesRoundTrip/status/starting.json`.
- TS names diffed (`--test-reporter=tap`, numbering stripped), with everything else
  identical:
  - `valid/starting.json`: deleted;
  - "counted_through is the recipient instance's…" → "the cursors are each
    instance's…" (usage);
  - new: "counted_through is the stream's instance's…" (stream);
  - `invalid/state-unknown.json`: its reason text changed.

**Removal checklist**

- `git grep -nwi 'starting' -- protocol/ gateway/internal/control control/ docs/specs/CONTROL-PROTOCOL.md`
  finds no status-state use. What remains has other meanings:
  - `api_key_env` names "starting with KAIAK_" (`config.schema.json` ×2,
    `GUIDE.md:70`);
  - "starting each stream's totals" (`GUIDE.md:152`), "starting a new stream"
    (`CONTROL-PROTOCOL.md:216`);
  - "Starting twice keeps one timer" (`gateways/index.ts:83`);
  - "windows starting at/before" (`storage/types.ts:206,210`), "a new limit starting
    at 0" (`CONTROL-PROTOCOL.md:877`);
  - "a model's queue starting or ending" (`control/status.go:15,82`);
  - `CONTROL-PROTOCOL.md:372`, the settled note "no `starting`".

  Also none: `StateStarting`, `state-starting`, `"starting"` in `gateway/`,
  `control/`, `protocol/`, `docs/specs`, `docs/architecture`.
- `git grep -nE '\.totals\(|countedThrough|PAGE_READER' control/` → none.
- `control/sample/src/index.ts` is deleted on disk. `git ls-files` lists it until the
  deletion is committed.

**Suite**

`scripts/check-all.sh` passes. No red carries over to a later step.

```
==> gofmt / go vet / staticcheck 2026.2.1 (gateway)
==> go test -race -count=1 (gateway): ok cmd/kaiak, e2e (108s), accounting, auth, clip,
    config, control, limits, logattr, metrics, netfail, otlplog, provider, routing,
    schemacheck, server, sse
==> gofmt / go vet / staticcheck (live-test kit)
==> live-test kit self-test: passed for vllm, llama-server, openai, azure-openai,
    anthropic, azure-anthropic, vllm with two backends
gateway checks passed
==> npm test (control): 615 tests, 614 pass, 0 fail, 1 skipped
==> npm run lint (control): boundaries ok
==> cross-half e2e (sample control plane + two gateways; two cores over one store): ok (65s)
all checks passed
```
