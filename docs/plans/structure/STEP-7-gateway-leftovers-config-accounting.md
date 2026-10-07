# Step 7 — gateway leftovers: config, accounting, server

**Status:** done (2026-10-08)

## Intent

Finish the gateway's leftovers: the group tree's pointer tree, the two-sink fan-out and
its spec text, the missing-endpoint memory that never forgets a removed backend, and the
validators for messages the gateway only sends.

## Findings

- config F3 (group-tree pointer tree): `Group{ID, PathIDs, AllowedModels, Limits}`;
  `Parent`, `Path`, `Root()`, `ModelSet.all`/`All()`, `Snapshot.Keys` (log
  `len(keysByHash)`), `Identity.allowed` and `Key.AllowedModels()` go.
- observability F6 (two-sink design): `RecorderOptions{Batcher, Metrics}` with one
  metrics interface carrying `Record` and `RecordClamped`; `Fanout` and `OutOfRange` go
  (`Fanout` to a test helper only if a test still needs two sinks). `GATEWAY.md`
  (Sinks decision and the fan-out mentions, :1780-1786, :2064) and `ARCHITECTURE.md`
  (:146, :164) say "batcher, then usage metrics"; the Sinks decision is restated with
  today's date.
- independent B01: the missing-endpoint memory is pruned on config apply to backends the
  applied config still has (`server/endpointmemory.go`).
- control-main F7 / decision 5: `DecodeUsageBatch`, `DecodeStatus`, the `usageBatch`,
  `status`, `backendStatus`, `deploymentStatus` walkers and the `usageBatch`/`status`
  rules move into `_test.go` files; `batchID` stays (the ack uses it).
- Stale comments and fields (review "bugs"):
  - `config/fixtures_test.go` `resolvedGroup.Limits[].Models` (per-model limits);
  - `config/snapshot.go` comment naming `decodeTree`;
  - `accounting/scan.go` header listing only `usage`, `choices`.

## Files likely touched

- `gateway/internal/config/{snapshot,loader}.go`, `fixtures_test.go`, `snapshot_test.go`;
  `gateway/internal/auth/auth.go`.
- `gateway/internal/accounting/accounting.go`, `scan.go`; `server/usage_path_test.go`,
  `server/server_test.go` (wiring); `cmd/kaiak/main.go`.
- `gateway/internal/server/endpointmemory.go` (+ a test), the apply hook in `main.go`.
- `gateway/internal/control/{decode,schema,semantic}.go` → `*_test.go`.
- `docs/specs/GATEWAY.md`, `docs/ARCHITECTURE.md`.

## Decisions made during planning

- B01's prune runs from the config-applied hook, where the router and provider pools are
  already reconfigured (step 14 later hands that hook the snapshot).

## Removal checklist (clean at phase end)

- `git grep -nP '\.Parent\b|\.Root\(\)|ModelSet\)\s*All|\.Keys\[|AllowedModels\(\)|\.allowed\b' gateway/internal/{config,auth}`
  → none.
- `git grep -nE 'Fanout|OutOfRange' gateway/ docs/specs docs/ARCHITECTURE.md` → none
  (a test helper excepted, if kept).
- `git grep -nE 'DecodeUsageBatch|DecodeStatus' gateway/` → `_test.go` only.

## Acceptance criteria

- B01 has a test: a reload that removes a backend leaves no memory entry for it.
- No other behaviour change; existing assertions unchanged.
- `scripts/check-gateway.sh` green, or reds named with the step that clears them.

## Result

**What changed**

- config F3 (group-tree pointer tree):
  - `Group` is `{ID, PathIDs, AllowedModels, Limits}`: `Parent`, `Path` and `Root()`
    are gone. `resolveGroup` builds `PathIDs` from the resolved parent's `PathIDs`
    plus the group's ID, and starts from the parent's `AllowedModels`.
  - `ModelSet.all` and `All()` are gone; `allModelSet` is the same set without the
    flag.
  - `Snapshot.Keys` is gone. The "config applied" line logs `len(keysByHash)`, which
    is the same count because the semantic rules refuse duplicate hashes.
  - `Key.AllowedModels()` and `Identity.allowed` are gone. `Identity.AllowedModels()`
    and `AuthorizeModel` read `id.Group.AllowedModels`. `Identity.AllowedModels()`
    stays because `server/models.go` reads it twice.
  - Stale items: `resolvedGroup.Limits[].Models` is gone from `fixtures_test.go` (no
    resolution fixture carries `models`, and the decode is strict, so a fixture that
    did would fail). The `decodeDocument` comment now names `schemacheck.Decode`.
- observability F6 (two-sink design):
  - `RecorderOptions` is `{Instance, Batcher, Metrics, Logger}`. `accounting.Metrics`
    is one interface with `Record(UsageRecord)` and `RecordClamped()`.
  - `Sink`, `Fanout` and `OutOfRange` are gone. `Settle` calls `Metrics.RecordClamped`
    where it used to call `OutOfRange`, then `Batcher.Record`, then `Metrics.Record`.
  - `metrics.UsageSink`/`NewUsageSink` are renamed to `UsageMetrics`/`NewUsageMetrics`,
    because "sink" now names nothing. Their test is renamed to
    `TestUsageMetricsCountsRecords`.
  - `cmd/kaiak` passes `Metrics: metrics.NewUsageMetrics(…)`.
  - The server harness's record keeper is now `recordedUsage`. It implements
    `accounting.Metrics` and hands each record to the usage metrics before keeping
    it. No test needs two sinks any more, so no `Fanout` test helper was kept.
  - Docs:
    - `GATEWAY.md`: the Sinks decision is restated as **Where records go**
      (settled 2026-10-08): the batcher first, then the usage metrics, with the
      reason and the rejected fan-out. The Usage batches bullet now calls the client
      "accounting's batcher". Two more mentions the report did not list are fixed:
      Routing's "the usage-metrics sink gets each record" and Observability's "Two
      paths, not derived".
    - `ARCHITECTURE.md`: the `accounting` and `metrics` entries are updated.
    - `docs/architecture/gateway.html`: the accounting row, the extension-point figure
      (aria-label, ⑤ label, comment) and the ⑤ Record consumers row are updated.
    - Code comments: `accounting` package doc, `metrics/registry.go` package doc, and
      the `control/usage.go` "accounting sink" comment.
- B01 (missing-endpoint memory):
  - `server.MissingEndpoints` is now exported, with `NewMissingEndpoints()` and
    `Retain(backends)`. `Retain` forgets every entry whose backend the applied config
    no longer has.
  - `cmd/kaiak` builds the memory before the applier and calls `Retain` from the
    config-applied hook next to `providers.Retain`. `NewAPI` takes the memory and
    hands it to the pipeline, which no longer builds its own.
  - The server harness builds the memory and passes it to `NewAPI`. Its `apply`
    calls `Retain`, as `cmd/kaiak` does.
  - New test `TestAppliedConfigForgetsARemovedBackendsMissingEndpoints` in
    `server/endpointmemory_test.go`:
    - a llama-server backend `ls` answers a Responses request with its 404, so the
      memory remembers `ls`;
    - an entry for `local` is added;
    - a reload to the base test config, which has no `ls`, leaves only `local`.
  - With `Retain` made a no-op, the test fails with "remembered [local ls] after the
    reload".
- control-main F7 / decision 5:
  - Moved into the new `control/outbound_test.go`:
    - `DecodeUsageBatch` and `DecodeStatus`;
    - the walkers `usageBatch`, `status`, `backendStatus` and `deploymentStatus`;
    - the rules `ruleCheck.usageBatch` and `ruleCheck.status`;
    - the `states`, `circuits` and `codePattern` lists, which only the status walker
      read.
  - `batchID` stays in `schema.go`, because the ack walker uses it. `semantic.go`
    loses its `maps` and `slices` imports.
  - `CodeRecordInstanceMismatch`, `CodeRecordIDDuplicate` and `MaxBatchRecords` stay:
    production reads them (`usage.go`'s refusal codes and `client.go`'s batch bound).
  - `GATEWAY.md` records the decision as **Messages the gateway only sends**
    (settled 2026-10-08), under Control-plane mode. `ARCHITECTURE.md`'s `control`
    entry says the package validates what it receives, and its tests validate what it
    only sends.
- `accounting/scan.go`: the header now says the scanner keeps `"usage"` and the
  format's content member: `choices` for OpenAI, `content` for Messages, `output` for
  Responses (`meter.go`: `newMemberScanner(map[string]int{"usage": …,
  m.reader.contentMember(): …})`).

**Decisions made during the step**

- F6: I chose a new `accounting.Metrics` interface over adding `Clamped` to `Sink`.
  With `Fanout` gone, "sink" described nothing. Both methods have one implementation,
  the usage metrics.
- B01 wiring: the memory is an exported type built in `cmd/kaiak` and passed to
  `NewAPI`, matching `providers.Retain`. It cannot be a method on `API`:
  - the applier exists, and applies the boot config, before the API is built;
  - in control-plane mode `client.Run`, which applies later configs, starts before
    `NewAPI`, so a late-assigned `*API` read from the hook would be a data race.

  The exported surface is one type, its constructor and `Retain`.
- B01 and in-flight requests: a request still running on an older snapshot can
  remember a removed backend after the prune. The next applied config forgets it.
  `Retain`'s comment says so. No second cleanup path was added.
- Harness order: `recordedUsage.Record` hands the record to the usage metrics first,
  then keeps it and signals `settled`. Before, the fan-out was {test sink, usage
  metrics}. With the new order, a test that waits on `settled` and then scrapes can
  no longer race the metrics update. No assertion changed.
- Adapted assertions (decisions 16/17, removed members):
  - `TestKeysResolveToTheirGroup`:
    - the `Parent`/`Root()`/`Path[1]` clauses are dropped; the `PathIDs` check that
      sat beside them stays, and `k.Group == s.Groups["support-bot"]` is added;
    - `users` being top-level is checked as `PathIDs == ["users"]`, not
      `Parent == nil && Root() == users`;
    - keys are looked up through `KeyByHash` (new test helper `keyByHash`), and their
      IDs are checked.
  - `.All()` assertions:
    - `TestWildcardExpandsToEveryModel`, `TestChildDefaultsMergeUnderEachChild`
      (users, alice): "all" is now checked as `Names() == s.ModelNames`, which the
      `"*"` assertion already checked next to it;
    - the negative `All()` checks (support, locked-out) are dropped, and their
      name-list checks stay.
  - `TestResolvedFixtures`: the `Path`/`PathIDs` agreement loop is deleted, because it
    asserted the removed second form of the path. A fixture's `"all"` is now compared
    as `s.ModelNames`.
  - `TestSchemaDefaultsAreResolved`: `disabled` is read through
    `KeyByHash(minimalKeyHash)`.
  - `TestRecorderSettlesOneRecordToEverySink` asserted the removed two-sink fan-out.
    It is renamed `TestRecorderSettlesOneRecordToTheBatcherThenTheMetrics`, and it
    keeps its record-JSON and unique-ID assertions. It now asserts that the batcher
    and the metrics each get the record once, and that the metrics and the caller
    see the batcher's generation (the batcher sees 0, the metrics and the caller see
    7).
  - The clamp tests count `RecordClamped` on the test metrics in place of an
    `OutOfRange` callback. Same counts.
- Removal-checklist item 1 matches 10 lines, all false positives (below).
  `Identity.AllowedModels()` is kept on purpose: F3 removes `Key.AllowedModels()` and
  has `Identity`'s methods read the group.

**Report vs code** (034329e; code at 4288768)

- config F3:
  - As reported, but the report missed `schema_test.go:74`, which reads
    `s.Keys["k-me"]`. That test came from step 4's schema-defaults pin.
  - The `fixtures_test.go` agreement loop is at :140-144 (report: :273-281). Step 4
    restructured the file.
  - `snapshot.go` line numbers matched within 1–2.
- observability F6:
  - The report's `server/usage_path_test.go:97` `Fanout` use no longer exists: step 1
    moved the wiring into `server_test.go`'s harness, the one `Fanout` use left.
  - `main.go` is at :449-455 (report: :452-458).
  - `ARCHITECTURE.md` is at :147/:165 (report: :146/:164).
  - Two more `GATEWAY.md` mentions (:942, :2372) and three code comments were not in
    the report.
  - `gateway.html` had the same claim in five places.
- control-main F7: as reported (`decode.go:47-60`, `schema.go:154-224`,
  `semantic.go:67-101`). The move also took three package vars that only the status
  walker used.
- B01: as reported (`endpointmemory.go:33/45`, `upstream.go:76/229`).
- `scan.go:8-13`: as reported.

**Tests** (before → after)

| Package | `go test -list` | `-v` `=== RUN` |
|---|---|---|
| `internal/config` | 34 → 34 | 204 → 204 |
| `internal/auth` | 5 → 5 | 14 → 14 |
| `internal/accounting` | 36 → 36 | 122 → 122 |
| `internal/control` | 76 → 76 | 240 → 240 |
| `internal/server` | 184 → 185 | 432 → 433 |
| `cmd/kaiak` | 29 → 29 | 33 → 33 |

- Names diffed:
  - accounting: `TestRecorderSettlesOneRecordToEverySink` →
    `TestRecorderSettlesOneRecordToTheBatcherThenTheMetrics` (renamed, see above);
  - server: + `TestAppliedConfigForgetsARemovedBackendsMissingEndpoints` (B01);
  - metrics, not in the table: `TestUsageSinkCountsRecords` →
    `TestUsageMetricsCountsRecords`;
  - otherwise identical.
- No test function was deleted. Assertions on removed members were dropped or
  restated, as listed under Decisions.

**Removal checklist**

- `git grep -nP '\.Parent\b|\.Root\(\)|ModelSet\)\s*All|\.Keys\[|AllowedModels\(\)|\.allowed\b' gateway/internal/{config,auth}`
  matches 10 lines, none of them a removed member:
  - `auth.go:101`: `Identity.AllowedModels()`, kept by design;
  - `semantic.go:181/242/246/249` and `snapshot.go:457-459`: `groupDoc.Parent`, the
    document's `parent` field;
  - `semantic.go:52`: `doc.Keys[id]`, the document's keys;
  - `snapshot.go:266`: `ModelSet) Allows`, which the `All` prefix matches.

  A narrower grep finds no removed member anywhere in `gateway/`:
  `git grep -nP '\.Root\(\)|\.All\(\)|ModelSet\)\s*All\(|s\.Keys\b|snapshot\.Keys\b|Key\)\s*AllowedModels|id\.allowed|g\.Parent\b|Group\.Parent|\.Path\[|g\.Path\b|Parent\s+\*Group|Path\s+\[\]\*Group|all\s+bool'`.
  Its only matches are `maps.Keys(…)` and an unrelated `stall bool`.
- `git grep -nE 'Fanout|OutOfRange' gateway/ docs/specs docs/ARCHITECTURE.md` →
  none. No test helper was kept.
- `git grep -nE 'DecodeUsageBatch|DecodeStatus' gateway/` → `_test.go` only
  (`fixtures_test.go`, `status_test.go`, `usage_test.go`, `outbound_test.go`).
- Also none outside the plan and review docs: `Sink:`, `NewUsageSink`, `UsageSink`,
  `*missingEndpoints` / `newMissingEndpoints` (the old unexported type), `decodeTree`.

**Suite**: `scripts/check-gateway.sh` passes on its own, and no red carries over to a
later step.

```
==> gofmt / go vet / staticcheck 2026.2.1 (gateway)
==> go test -race -count=1 (gateway): ok cmd/kaiak, e2e (109s), accounting, auth, clip,
    config, control, limits, logattr, metrics, netfail, otlplog, provider, routing,
    schemacheck, server, sse
==> gofmt / go vet / staticcheck (live-test kit)
==> live-test kit self-test: passed for vllm, llama-server, openai, azure-openai,
    anthropic, azure-anthropic, vllm with two backends
gateway checks passed
```
