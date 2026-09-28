# Step 4 — usage intake and totals

**Status:** done (2026-09-24) — phase 2 continues with step 5

## Intent

Take usage batches exactly once, aggregate totals per scope and window by the control
plane's clock, and answer each batch with an ack carrying fresh totals.

## Files likely touched

- `control/kaiak-control/src/usage/` — intake, de-duplication, aggregation, totals
- storage interface + in-memory store (last batch per instance, totals, recent records)
- Fastify plugin: `POST /v1/usage`

## Decisions made during planning

- De-dup: last counted `{ epoch, sequence }` per instance. Same epoch and sequence ≤
  last → re-ack without counting. New epoch → accepted (a new spool). Sequence gap
  within an epoch is accepted and logged (records lost on the gateway side are its
  problem; the control plane must not stall).
- Records stamped with receipt time; each counts into every applicable scope ×
  limit window (hour, month UTC) of the config version in force — scopes from the
  record's owner, limits whose model set covers the record's model.
- Totals kept per scope × limit (type + model set) × window: requests, tokens
  (in + cached + out, as the gateway counts), cost in nano-USD (bigint where sums can
  exceed safe integers).
- Ack = batch ID + totals for the scopes the batch touched (or all — decide on size).
- Records retained in the store for the sample's "recent records" view (bounded).

## Acceptance criteria

- Tests: exactly-once under resend; new epoch accepted; gap accepted; totals correct
  per scope and window; window rollover by injected clock; ack totals match the
  store; cost sums exact beyond 2^53 nano-USD.

## Result

- `control/`: `npm test` — 276 tests, 276 pass, 0 fail; `npm run lint` — `tsc` clean,
  `boundaries ok`.
- `scripts/check-gateway.sh` — `gateway checks passed` (unaffected).
- No expected reds.

### Decisions made while implementing

- Intake rules, aggregation, orphaned owners, pruning, error codes: recorded in
  CONTROL-PROTOCOL.md, Usage intake (settled 2026-09-24). In short: schema violation
  → `400 usage-batch-invalid`, rule violation → `400 <rule code>`, body instance ≠
  header → `400 instance-mismatch`, no config yet → `503 config-unavailable`
  (nothing counted; the gateway retries); de-dup by last `{epoch, sequence}`; per-
  instance serialization; one atomic store write per counted batch; one receipt time
  per batch; the record's own `team`; removed owners count toward the scopes that
  remain; windows with nothing used are not listed; `used` saturates at 10^18 − 1.
- Ack totals are computed after counting with a fresh clock reading — the same thing
  `totals()` answers at that instant (tested equal).
- `config/` gained `resolveScopeLimits`, `limitIdentity`, `limitCovers` (limit
  resolution mirroring the gateway's `mergeLimits`; tested against `full.json` and
  `user-overrides.json` with the gateway snapshot tests' expectations);
  `limitIdentity` moved out of `semantic.ts` so the rule and the totals share it.
- `usage/` subsystem: `createUsage({ store, clock, recentRecordsSize, liveGateways,
  onListenerError })` → `acceptUsageBatch(instance, doc)` returning `{ ok: true, ack,
  outcome, previous? } | { ok: false, error: { code, message, status } }` with
  `outcome` ∈ `first | next | gap | new-epoch | duplicate`; `totals()` (undefined
  before the first config); `recentRecords()` (newest first); `onTotalsChanged`
  (a signal, no payload: a pusher reads `totals()` when it pushes, so a push is never
  staler than the moment it is sent); `dropPastWindows()` (invocable; intake triggers
  it on the first counted batch of each hour).
- The core logs nothing: intake reports the outcome and the previous batch ID, and the
  Fastify route logs `new-epoch` (info), `gap` (warn) and `duplicate` (info) through
  `request.log`.
- Storage interface: `lastBatch(instance)`, `saveCountedBatch(counted, keepRecords)`
  (last batch + additions + records in one step), `currentWindowTotals(current)`,
  `dropPastWindowTotals(current)`, `recentRecords(limit)`; totals are `bigint`,
  window starts epoch ms, model sets sorted.
- Core: `acceptUsageBatch`, `totals`, `recentRecords`, `onTotalsChanged`; option
  `recentRecordsSize` (default 100). `onListenerError` now receives `(error, event)`
  with `event` = `{ type: "config-published", published } | { type:
  "totals-changed" }` — one handler for both subscriptions.
- `live_gateways`: the core wires `liveGateways: () => 0` into `usage` for now; **step 5
  replaces it with the live set's count** (the option already accepts a promise).
- Fastify: `POST /v1/usage` with a 2 MiB body limit (500 records with 128-character
  request IDs and 64-character key IDs pass, tested). The checked instance reaches the
  route through a `WeakMap<FastifyRequest, string>` filled by the `onRequest` hook —
  not `decorateRequest`, whose type augmentation would land on every `FastifyRequest`
  of the host app. Step 5's `POST /status` uses the same `instanceOf(request)`.
- Mutation-checked: removing the per-instance serialization fails the racing-copies
  test; removing the pruning trigger fails both rollover tests.

### Deviations from the plan

- None in substance. The plan's "(or all — decide on size)" was already settled in
  step 1 (complete totals).

### For later steps

- Step 5: implement the live count and pass it as `liveGateways` in
  `control-plane/index.ts`; push totals on `onTotalsChanged` (coalesced), on live-count
  change and on stream connect, reading `controlPlane.totals()` at push time. A config
  publish also changes totals (`config_version`, the set of limits) — consider pushing
  on it too, or rely on the config event.
- Step 7: a `503 config-unavailable` on `POST /usage` means keep the batch and retry;
  a `400` means the batch can never be accepted (log it; the spool must not wedge on
  it — decide there).
- Step 10: `recentRecords()` gives `{ receivedAt, record }` newest first.

