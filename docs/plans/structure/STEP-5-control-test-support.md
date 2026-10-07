# Step 5 — kaiak-control test support; tests filed by subject

**Status:** done (2026-10-08)

## Intent

`kaiak-control` tests share one support module, so a protocol bump or a new usage unit
touches no test file, and a behaviour's tests live in one file named for that
behaviour on both halves — not in files named after the review round that found them.

## Findings

- test-scaffolding F4, edges F4: `kaiak-control/src/test-support/index.ts` (an ordinary
  subsystem for the boundary lint, not in package `exports`): `fixture(rel)`,
  `gatewayHeaders(instance)` from `PROTOCOL_VERSION`, `startApp(core, opts)`,
  `openStream(base, instance)`, `nextConfig` / `nextTotals`, `configNumbered`,
  `usageBatch(overrides)` built on a `protocol/fixtures` usage record; `sse-client.ts`
  moves there.
- Fold `fastify/round-2.test.ts`, `fastify/round-3.test.ts`,
  `control-plane/{review,round-3}.test.ts` into the files of their subjects (stream
  ordering → `fastify.test.ts`; totals → `status-totals.test.ts`; status receipt →
  control-plane tests; …).
- Gateway side, same rule: the `review_test.go` files in `accounting`, `provider`,
  `server` and `server/inbound_review_test.go` fold into their subjects' test files.
- control-core hint: test literals `"5"` / `format_version: 5` read the exported
  constants.

## Files likely touched

- `control/kaiak-control/src/**/*.test.ts` (fastify, control-plane, usage, messages,
  config), `control/sample/src/main.test.ts`, `sample/src/app/app.test.ts`.
- `control/scripts/check-boundaries.ts` only if the new subsystem needs declaring.
- `gateway/internal/{accounting,provider,server}/*review*_test.go` and their targets.

## Decisions made during planning

- Fold per subject, one commit per subject if that keeps the diff readable.
- A test's name may change to say what it checks; its assertions do not.

## Removal checklist (clean at phase end)

- `git ls-files 'control/**/round-*.test.ts' 'control/**/review.test.ts' 'gateway/**/*review*_test.go'`
  → none.
- `git grep -n '"kaiak-protocol": "5"' control/` → none outside test-support.

## Acceptance criteria

- One test-support module; every fastify test file uses it.
- Test counts per half unchanged (record `npm test` and `go test` counts before/after;
  each moved test findable by name in the Result).
- `scripts/check-all.sh` green. **Phase 1 ends here.**

## Result

**What changed**

- `control/kaiak-control/src/test-support/` grows from the fixture runner into the
  suites' shared support (still an ordinary subsystem, not in `exports`, excluded from
  `files` and the sample image; the boundary lint needed nothing):
  - `sse-client.ts` moved here from `fastify/` (re-exported: `openSseStream`,
    `SseItem`, `SseStream`). It no longer ships in the package.
  - Gateway messages: `TEST_TOKEN`, `TEST_INSTANCE`, `gatewayHeaders(instance?)` from
    `PROTOCOL_HEADER`/`INSTANCE_HEADER`/`PROTOCOL_VERSION`; `gatewayStatus(instance,
    fields?)` on `status/valid/ready.json`; `usageRecord(fields)` on
    `usage-record/valid/one-group.json` — every unit the fixture lists at 0, not
    estimated, not partial, then `fields` (`fields.units` names the non-zero units) —
    and `usageBatch(batchId, records)`.
  - Configs: `configNumbered(n)`, `numberOf(config)`, `CONFIG_FORMAT_VERSION` (the
    config schema's `format_version` const).
  - Harness: `testCore(options)` (memory store, test token), `failOnListenerError`,
    `closeAfterTest` / `closeOpened` (last opened first), `startApp(core, options,
    onRequest?)` → `{ base, port, close }` (close idempotent), `openStream(base,
    instance?)`, `countingSubscriptions`, `restorable(live, backup)`.
  - Stream reading: `configNumberOf`, `nextConfigOrEnd`, `nextConfig`, `totalsOf`,
    `nextTotals` (the validating versions).
- `fastify.test.ts`, `status-totals.test.ts`, `usage-route.test.ts` use the harness;
  their private fixture readers, cores, closers, `startApp`, `openStream`, headers,
  `configNumbered`, `countingSubscriptions`/`countingStreams`, `nextTotals`/`totalsOf`
  and the inline restorable-store proxy are gone.
- `config-publishing.test.ts` uses the shared `configNumbered`/`numberOf` (a third
  copy); `usage.test.ts`'s `record()` and `control-plane.test.ts`'s `carolBatch` build on
  `usageRecord`/`usageBatch` and spell no zero unit.
- Protocol version literals read the constants: `protocol.test.ts` (`goodHeaders`),
  `control-plane.test.ts`, the fastify files, `sample/src/main.test.ts`,
  `sample/src/app/app.test.ts`, `sample/src/page/page.test.ts` (`protocol_version`),
  through `kaiak-control`'s entry in the sample. `format_version: 5`:
  `config/limits.test.ts` reads `CONFIG_FORMAT_VERSION`; the sample's
  `config-file.test.ts` reads it from `minimal.json` (the sample cannot import
  test-support).
- Round/review files folded by subject and deleted: `fastify/round-2.test.ts`,
  `fastify/round-3.test.ts`, `control-plane/review.test.ts`,
  `control-plane/round-3.test.ts`; `gateway/internal/{accounting,provider,server}/review_test.go`,
  `server/inbound_review_test.go` (its two inbound tests now in `server/inbound_test.go`,
  the tests of `inbound.go`). No other `round-*`/`review*` test file existed.
- `docs/ARCHITECTURE.md`: the `test-support` entry names the builders and the harness.

**Moves** (TS names gain the prefix of the `describe` they joined; leaf names unchanged)

| From | Test | To |
|---|---|---|
| `fastify/round-2` | a stream ended during config delivery writes nothing into the ended response | `fastify.test.ts` › GET /v1/stream |
| `fastify/round-2` | republishing after a restore reaches a stream that connected to the restored config | `fastify.test.ts` › GET /v1/stream |
| `fastify/round-2` | a delivery read issued before a stream's connect read is not sent after it | `fastify.test.ts` › GET /v1/stream |
| `fastify/round-2` | a catch-up sends a stream nothing it already runs, and the stream stays open | `fastify.test.ts` › GET /v1/stream |
| `fastify/round-2` | a config event carries the stored text as it is, its hash the hash of that text | `fastify.test.ts` › GET /v1/stream |
| `fastify/round-2` | a slow totals read is never applied after a later one | `status-totals.test.ts` › totals on the stream |
| `fastify/round-2` | totals keep listing a window the current config no longer limits | `control-plane.test.ts` (top level) |
| `fastify/round-2` | counted_through covers the acknowledged epoch after an older epoch's late write | `control-plane.test.ts` › several cores over one store |
| `fastify/round-2` | a delayed first status read does not overwrite a newer status | `control-plane.test.ts` › several cores over one store |
| `fastify/round-2` | a throwing delivery-failed listener does not stop later deliveries | `control-plane.test.ts` › several cores over one store |
| `fastify/round-3` | all 4 (first totals include every batch…; while totals reads fail…; a counted batch that changes no window…; a window a restored store lacks…) | `status-totals.test.ts` › totals on the stream |
| `control-plane/review` | all 9 (stalled writes ×2, stale forget, late status, config read retried / announced, totals across an hour boundary, catch-up, stop/start) | `control-plane.test.ts` › several cores over one store |
| `control-plane/round-3` | all 3 (publish checks the parents rule…; a store failing to drop past windows…; a sweep forgets gateways in instance order) | `control-plane.test.ts` (top level) |
| `accounting/review_test.go` | `TestMessagesPartialOutputAfterInitialUsage` | `messages_usage_test.go` |
| | `TestResponsesStreamEstimateCountsToolNames`, `TestEstimateResponsesToolOutputMedia` | `responses_usage_test.go` |
| | `TestEstimateToolDataIsNotMedia` | `estimate_test.go` |
| `provider/review_test.go` | `TestAnthropicTypesRefuseRepeatedCachePolicy` | `anthropic_test.go` |
| | `TestUnknownEventsKeepTheirNestedModel` | `model_test.go` |
| | `TestCommentBeforeAFirstErrorEventIsRetriable` | `error_event_test.go` |
| `server/review_test.go` | `TestResponsesHostedShellRefused`, `TestResponsesInputItemHostedToolsRefused`, `TestResponsesStoredItemReferenceRefused`, `TestResponsesDuplicateAllowedToolsRefused` | `responses_test.go` |
| `server/inbound_review_test.go` | `TestInboundRefusalsPastTheTopLevel`, `TestInboundAdmitsClientShellsAndInlineItems` | `inbound_test.go` (new) |
| | `TestThinkingBudgetAboveTheModelsOutputLimit` | `messages_test.go` |
| | helpers `errorFields`, `errorCodeOf` | `server_test.go`, beside `expectError` |

**Decisions made during the step**

- The shared stream readers are the validating ones (`fastify.test.ts`'s config check —
  valid event, no id, hash of its config — and `status-totals.test.ts`'s `totalsOf`):
  the moved round tests, whose local readers only parsed, now check that too (the
  union, as in step 3). `countingSubscriptions` is `fastify.test.ts`'s version, which
  ignores a second unsubscribe; `status-totals`' copy did not.
- One token and instance (`test-token`, `gw-1`) for the moved tests: `round-2`/`gw-r2`,
  `round-3`/`gw-r3`, `review`/`gw-review` were labels no assertion read except through
  the variable. Each moved test keeps its clock (`NOW` = 2026-10-07 12:30), epochs,
  sequences, token counts, costs, groups and `gateway_time`; record and request IDs
  follow one form per file (`status-totals`: sequence in hex; `control-plane`:
  epoch + sequence), distinct per batch and identical on a resend as before.
- Cleanup is one LIFO list per file (`afterEach(closeOpened)`). `fastify.test.ts`
  closed streams then apps in opening order and closed an app the test had already
  closed again; `usage-route.test.ts` closed in opening order.
- The moved multi-core tests use `control-plane.test.ts`'s describe helper (via
  `coreAtNow`), so their cores are stopped after each test; before, they stayed
  subscribed. `stop` is idempotent. Sweep trigger labels `review-held` /
  `review-other-core` became `held` / `other-core` (no assertion reads them).
- Literals kept on purpose (the test's point is the value): `protocol.test.ts` "the
  current version is 5 and passes" (the pin), its mismatch list (`"4"`, `"6"`,
  `"5.0"`, `" 5"`, `"5, 5"`) and the `"9"` of "is checked token first…";
  `fastify.test.ts`'s `"3"` ("another protocol version"). A version bump edits that
  `protocol.test.ts` describe only.
- Comments state constraints only: the round/review IDs on moved tests (2H2, 3H1, M3,
  `[B] B2`, "the pre-merge review's M6", "H3, M5, L3, [B] H3 and [B] L1", …) are gone,
  and so are the review references already in the files touched
  (`anthropic_test.go` ×2, `error_event_test.go`, `estimate_test.go`,
  `server/messages_test.go`, `server/server_test.go`, `usage.test.ts`).
- Left as they are: `gateways.test.ts`'s `statusOf` (the same as `gatewayStatus`; an
  untouched file, 25 call sites) and the sample's `page.test.ts` record literals (the
  sample cannot import test-support). Review references in Go test files this step
  did not touch (`provider/body_test.go`, `provider_test.go`, `remotetext_test.go`,
  `server/bodies_test.go`) remain.

**Report vs code** (034329e): as reported — 17 `"kaiak-protocol"` sites in 9 files
(9 header literals, 8 response-header assertions); usage-record builders ×6. Beyond
the report: `protocol_version: 5` ×3 in the sample's `page.test.ts`, `format_version:
5` in `limits.test.ts` and `config-file.test.ts`, a third `configNumbered` in
`config-publishing.test.ts`, and a third copy of the restorable-store proxy
(`fastify.test.ts`, `round-2`, `round-3`). Edges F4 proposed `fastify/test-harness.ts`;
the step's test-support subsystem holds it instead.

**Test inventory** (before → after)

- TS `npm test`: 615 → 615 tests, 45 → 45 suites (614 pass, 1 skipped, as before).
  Names diffed: 22 moved tests carry a new suite prefix (`GET /v1/stream › ` ×5,
  `totals on the stream › ` ×5, `several cores over one store › ` ×12), the other 4
  moved tests keep their names; with those prefixes stripped the name lists are
  identical. Per file: `fastify.test.ts` 28 → 33 tests, `status-totals.test.ts`
  16 → 21, `control-plane.test.ts` 14 → 30, round/review files 26 → 0.
- Go (`accounting`, `provider`, `server`): `go test -list` 36 / 54 / 184 → identical
  (274 names); `-v` `=== RUN` 122 / 201 / 432 → identical (755 names).

**Removal checklist**

- No `round-*.test.ts`, `review.test.ts` or `*review*_test.go` left on disk under
  `control/` or `gateway/` (`git ls-files` lists the four TS and four Go files until the
  deletions are committed).
- `git grep -n '"kaiak-protocol": "5"' control/` → none.

**Suite** — `scripts/check-all.sh`, green (phase 1 end):

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
ok  	kaiak/e2e	65.355s
all checks passed
```
