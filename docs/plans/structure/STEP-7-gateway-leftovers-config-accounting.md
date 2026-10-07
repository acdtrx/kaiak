# Step 7 — gateway leftovers: config, accounting, server

**Status:** not started

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
