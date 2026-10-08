# Step 29 — independent review and green

**Status:** done (2026-10-08)

## Intent

Check the whole branch with fresh eyes before it merges, fix what that finds, and record
which review findings each step resolved.

## Scope

- **Docs sweep:** `docs/ARCHITECTURE.md` and `docs/architecture/*.html` match the package
  graph (the `control → limits` edge, the model check in `provider`, `fixturetest`,
  `fakeotlp`, the `kaiak-control` `listeners` and `test-support` subsystems); every spec
  section an earlier step touched reads true; no history-narrating comment was left in
  touched code. Known: `docs/architecture/control-plane.html` still lists `totals` in the
  core's box (removed in step 8).
- **Upgrade notes:** `docs/DEPLOYMENT.md` → Upgrades gets one bullet for this plan's
  host-visible changes: the `kaiak-control` API changes of decision 11 (`totals`,
  `countedThrough`, store window starts, `BatchCursors.latest`, the sweep methods,
  `configHash`, `libraryName`) and the status state `starting` no longer existing.
- **Independent review:** the user's usual flow — a plain copy of the branch at
  `/tmp/kaiak` (no `docs/plans`, no `docs/reviews`), a Codex review against the plan's
  goal and constraints (behaviour preserved except decision 10; no metric, protocol,
  client API or config change beyond it), its report merged and each finding
  re-checked against code; its reproductions ported as tests. Delete the `/tmp/kaiak*`
  copies afterwards and check no Codex daemon keeps `/tmp/kaiak` as its cwd.
- **Fixes** from that review, each with a test where it is a behaviour.
- **Outcome table:** a section at the end of
  `docs/reviews/2026-10-07-structure/STRUCTURE.md` listing every in-scope finding with
  the step (and commit) that resolved it, and every out-of-scope one with where it went.
- **Removal checklists** of steps 1–28 re-run on the final tree.

## Acceptance criteria

- `scripts/check-all.sh` green **three times in a row**. **Phase 6 and the plan end
  here.**
- OVERVIEW's Verification status filled in.

## Result (part 1: docs sweep)

**Docs changed**

- `docs/ARCHITECTURE.md` (checked against `go list` and the `kaiak-control`/sample import
  graphs, which `check-boundaries.ts` enforces):
  - `server`: the stage list gains `endpoint support` (step 10's split of
    `model_access`); each stage names the requests it applies to; each endpoint is one
    row of `server`'s endpoint table, a body endpoint identified by its
    `provider.Endpoint` (path and format `provider`'s), the row holding the
    output-limit keys, names and token counting (steps 22–23).
  - `limits`: a refusal carries its client headers and its log fields (step 27).
  - `control`: the drain's last step is the client's `Finish` (step 19).
  - `fixturetest`: also read by `auth`, `limits`, `provider` and `cmd/kaiak`.
  - The `kaiak-control` diagram gains `test-support` and its six edges (`config`,
    `control-plane`, `fastify`, `messages`, `protocol`, `storage`). Every other edge
    of both diagrams matched the code; so did the gateway edges the text names
    (`control → limits`, `sse`/`provider`/`control`/`otlplog` → `netfail`, the model
    check in `provider`, `fakeotlp`, `listeners`, the core's sweep).
- `docs/architecture/gateway.html`: the shared-parts row adds `netfail` and describes
  `schemacheck` as the one validation pipeline; the test row adds `fakeotlp` and
  `fixturetest`; the model check is `provider`'s and names the Azure info line; seam ①
  names the endpoint table; seam ② lists `endpoint support` and stage scopes; the
  footer names the structure work.
- `docs/architecture/control-plane.html`: the core's box lists `readTotals`, not
  `totals`; the footer names the structure work.
- `docs/DEPLOYMENT.md` → Upgrades: the new bullet below. The 0.11 bullet's store item
  is back to 0.11's `lastBatch(instance, epoch)` (step 24 had rewritten it in place);
  the change to `lastBatches` is in the new bullet.
- Specs (`git diff v0.11.1..HEAD -- docs/specs`: `GATEWAY.md`, `CONTROL-PROTOCOL.md`,
  `BACKEND-VERIFY.md`): every touched section read against the code — the Messages
  message duplicate rule, the per-endpoint output-limit keys, the Azure model-check
  info line, Where records go (`RecorderOptions`: batcher, then metrics), Messages the
  gateway only sends (`checkRecords` at seal; outbound decoders in `_test.go`), no
  `starting`, the last attempt's upstream error fields, the counted-units and
  backend-types fixtures (file shapes as described). No edit needed. Removed names
  grep clean over `docs/` (outside plans and reviews), `README.md` and `GUIDE.md`.

**Comments rewritten** (files changed on this branch; rule applied: review and audit
provenance — `AUDIT-n` IDs, `[X]` reviewer tags, "the … review's …", "reviewer's",
"repro" — removed, keeping the substance and any spec pointer; bare decision labels
the specs define (`D1`–`D8`, `N-C2`, `N-M1`, `N-S1`, `M2`, `H8`, `E12`, …) kept, as
pointers to a live decision; labels with no home in the docs (`3M2`, `N-P5`, `D-H3`,
`decision 36`, `B01`) removed; past-tense bug narration rewritten as the constraint):

- control: `kaiak-control/src/config/config.test.ts` (×2), `src/usage/usage.test.ts`.
- gateway e2e: `backendtypes_test.go`, `keylimits_test.go`, `logexport_test.go`,
  `messages_test.go`, `startup_test.go`.
- gateway internal: `config/snapshot_test.go`; `control/client_test.go` (×2),
  `control/totals_test.go` (×3); `limits/limits_test.go` (×5, one "the review's repro
  was told…" narration), `limits/shared_test.go` (×7, one "subtracted a hold that no
  longer existed" narration); `provider/provider_test.go`, `provider/send.go`,
  `provider/body_test.go` and `provider/remotetext_test.go` (step 5's leftovers),
  `server/bodies_test.go` (step 5's leftover; "they used to take… Now…"),
  `server/endpointmemory_test.go`, `server/endpoints_test.go` (×2),
  `server/estimate_test.go` (×3), `server/limits_test.go`, `server/visibility_test.go`.
- A wider grep over the branch's changed code for `used to`, `previously`, `formerly`,
  `(was `, `was removed/renamed/moved`, `Now …`, `legacy`, `step N`, `structure
  review` finds nothing else; the remaining "no longer" hits state current behaviour
  (a window no longer enforced, a config that no longer has a backend).

**Upgrade note** (`DEPLOYMENT.md` → Upgrades, "After 0.11.1"): host apps — `totals(instance)`
gone (`readTotals()`), `startExpirySweep`/`stopExpirySweep` gone (`start`/`stop`),
`control-plane-option-invalid`; the store's `WindowStarts` and
`lastBatches(instance)`/`BatchCursor` replacing `lastBatch`/`BatchCursors`; `libraryName`
gone, `MODEL_CAPABILITIES`/`scopeTypeKey`/`isCountedType` new; `starting` refused.
Gateways — the Messages message duplicate refusal, unchecked output-limit keys the
endpoint does not take, the `azure-openai` info line, the last attempt's upstream error
fields. Not listed, because no host could see them: `countedThrough` and `configHash`
were exported by their subsystems only, never by the package entry (`configHash` lives
on in `test-support`).

**Removal checklists** (final tree, `git grep`, `-P` where `\b` appears)

| Step | Result |
|---|---|
| 1, 3, 4, 26 | no checklist |
| 2 | clean |
| 5 | clean |
| 6 | clean; `\bcaps\b` → prose (5 lines) and one test message, as step 6 recorded |
| 7 | item 1 → the same 10 accepted false positives step 7 recorded (`Identity.AllowedModels()`, `groupDoc.Parent` ×7 incl. `entry.Parent`, `doc.Keys[id]`, `ModelSet) Allows`; line numbers moved); `DecodeUsageBatch|DecodeStatus` → `_test.go` only |
| 8 | clean; `starting` → other meanings only: those step 8 listed, plus `control/status.go:151` (`// starting, or retrying cannot fix it`, the first failure of a report, added by step 19) |
| 9 | clean |
| 10 | clean (`takesBody()` no longer exists at all: step 22 replaced it with the row's `body` field) |
| 11 | item 1 clean; item 2 → the four constants in `config/snapshot.go` and `fakecontrol.go:317`, accepted at step 11 |
| 12 | clean |
| 13 | `JSON.stringify([` → `scopeTypeKey` and `windowKeyOf` only, as accepted |
| 14 | items 1–2 clean; `holder.Current()` → none in the applied-config path (the status `Serving` callback, now in `controlplane.go` after step 19's split; the scrape-time gauges and usage metrics; tests), as accepted at step 14 |
| 15 | `UsageWaitingSince|UsageUncountedSince` → the `limits.Contact` fields and their readers/fillers, as accepted; the rest clean |
| 16 | clean |
| 17 | not taken; skipped |
| 18 | clean |
| 19 | clean |
| 20 | `stripUsage` → the core only (`body.go`, `send.go`, `response.go`, `stream_format.go`, the `passthroughBody` tests), as accepted; `wire.go` gone |
| 21 | clean |
| 22 | clean |
| 23 | `MaxTokens\b` → `responses_test.go:312` `HonorMaxTokens`, accepted at step 23; `decodeMembersRepeats` → none |
| 24 | `batchCursorRetentionMs` → the core's host option (`control-plane/index.ts`, its test, `GUIDE.md`), kept by design; the listener helper's comment → one hit |
| 25 | clean |
| 27 | clean |
| 28 | clean |

No finding: every hit is clean, another meaning, or a false positive accepted in its
step's Result.

**Suite**: `scripts/check-all.sh` passed (exit 0) on the final tree of this part.

```
==> gofmt / go vet / staticcheck 2026.2.1 (gateway)
==> go test -race -count=1 (gateway): ok cmd/kaiak, e2e (109s), accounting, auth, clip,
    config, control, limits, logattr, metrics, netfail, otlplog, provider, routing,
    schemacheck, server, sse
==> gofmt / go vet / staticcheck (live-test kit)
==> live-test kit self-test: passed for vllm, llama-server, openai, azure-openai,
    anthropic, azure-anthropic, vllm with two backends
==> npm test (control): tests 629, pass 628, fail 0 (1 skipped)
==> npm run lint (control): tsc + boundaries ok
==> cross-half e2e: ok kaiak/e2e 66s
all checks passed
```

## Result (part 2: review fixes)

### S1 — a hurry after the drain's deadline still sent the final status

**Mechanism**: `controlPlane.finish` folded the hurry context and the drain deadline
into one child context and passed it to `control.Client.Finish`, which skipped the
final status only on `errors.Is(ctx.Err(), context.Canceled)`. Once the child's
deadline passed, its `Err()` stays `DeadlineExceeded` even when the parent hurry is
cancelled afterwards — so a second stop signal after the deadline but before the flush
returned still sent the final draining status (up to `finalStatusTimeout`, 2 s),
against `docs/specs/GATEWAY.md` (Lifecycle → Draining: a hurried drain skips it).

**Before** — `TestHurryAfterTheDrainDeadlineSkipsTheFinalStatus`
(`gateway/cmd/kaiak/controlplane_test.go`: deadline already past, the hurry cancelled
from the logger as `usage flushed` is logged, a fake transport counting `/v1/status`
requests), on the unfixed code:

```
--- FAIL: TestHurryAfterTheDrainDeadlineSkipsTheFinalStatus (0.00s)
    controlplane_test.go:111: 1 final status reports after the hurry, want none
FAIL
FAIL	kaiak/cmd/kaiak	0.425s
```

**Fix**: `Client.Finish(hurry context.Context, deadline time.Time)` keeps the two
apart: the flush runs on `context.WithDeadline(hurry, deadline)`; after it,
`hurry.Err() != nil` skips the final status, whenever the hurry ended; a flush cut by
the deadline alone still sends it, on its own `finalStatusTimeout`. `controlPlane.finish`
passes both through. `TestFinishFlushesThenReportsDrainingUnlessHurried` changes only
its two call sites (`Finish(hurried, now+1h)`, `Finish(context.Background(), now)`);
its assertions are unchanged.

**Suite**:
- `go test -race -count=5 ./cmd/kaiak/ ./internal/control/` → `ok kaiak/cmd/kaiak 10.3s`,
  `ok kaiak/internal/control 38.3s`; the new test,
  `TestFinishFlushesThenReportsDrainingUnlessHurried`, `TestStopSignalDrains`,
  `TestSecondStopSignalSkipsTheRemainingDrain`, `TestSecondSignalCutsTheFinalLogFlush`
  pass.
- `scripts/check-gateway.sh` → exit 0: gofmt, vet, staticcheck 2026.2.1; race tests
  ok for every package incl. `e2e` (106 s); live-test kit lint and self-test passed for
  all seven setups; `gateway checks passed`.

### S2

**Kept as an intended change** (main session, 2026-10-08). The two output-limit
rejection messages print integers in plain digits (`default 3000000 is above ceiling
2000000`) where 0.11.1 printed exponent form for values of a million or more
(`3e+06`), since step 14 decodes those fields as `int64`. Codes and paths are
unchanged, and `kaiak-control`, which validates the same configs, already printed
plain digits: the two halves now give one message. The reviewer's test pinned the old
text, so it is not ported.

The review report is kept as
`docs/reviews/2026-10-07-structure/BRANCH-REVIEW-independent.md`.

### End-of-plan cleanup

Four small items from STRUCTURE.md → Outcome → Not handled (moved there to their
module tables as done); behaviour unchanged, no assertion weakened.

- **gateway/server:** `ClearBodyDeadline` → `clearBodyDeadline` (in-package only);
  `BodyBudget.InUse` → `inUse` (read only by `bodies_test.go`, package `server`).
- **gateway/provider:** `upstreamBodyReader.held()` removed; `release_test.go` (package
  `provider`) reads `r.data == nil` under `r.mu`, the same check the method made.
- **gateway/control:** `batchRefusals` uses constants only: `codeUsageBatchInvalid`,
  `codeInstanceMismatch`, `codeRequestInvalid` declared beside the message rule codes in
  `control.go` (values unchanged).
- **kaiak-control:** `files` excludes `src/store-contract/*-store.ts` and
  `src/store-contract/lossy-channel.ts` (the negative-control stores and their lossy
  channel, test-only). `npm pack --dry-run` from `control/kaiak-control/`: 40 files
  (was 45), `src/store-contract/index.ts` the only store-contract file; it imports only
  `node:*`, `../messages/index.ts` and `../storage/index.ts`.
- Left as recorded: the Messages/Responses stored-file error codes, the per-format
  seam, `keyedBackendTypes`, test SSE parsers and log-vocabulary matching.

**Suite**: `scripts/check-all.sh` → exit 0, `all checks passed`: gofmt, vet,
staticcheck 2026.2.1, race tests incl. `e2e` (108.7 s); live-test kit self-test for all
seven setups; control `npm test` 629 tests, 628 pass, 0 fail, 1 skipped; lint and
boundaries ok; cross-half e2e `ok kaiak/e2e 65.3s`.

### Plan end

`scripts/check-all.sh` green **three times in a row** on `3e10260` (main session,
2026-10-08): gateway gofmt, vet, staticcheck, race tests (e2e ~106 s); the live-test
kit self-test; control `npm test` 629 tests (628 pass, 1 skipped), lint and boundaries;
the cross-half e2e (~65 s). Phase 6 and the plan end here.
