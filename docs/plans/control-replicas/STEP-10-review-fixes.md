# Step 10 — review fixes

**Status:** done (2026-10-07)

## Intent

Fix what remains of the pre-merge review (`docs/reviews/2026-10-07/AUDIT.md`) after
phase 3, so the branch merges with no known counting, enforcement or delivery defect.
This step is **phase 4** and ends green.

## Scope (finding IDs from the AUDIT)

First mark the findings phase 3 dissolved, with the reason, in the AUDIT: H2, L2,
L3, L5, L9, L12, and any part of M6 about epochs and restore. Then fix:

- **H1:** batch cursors per (instance, batch epoch), dropped by retention. Both halves
  of the spec, the store contract and its tests.
- **H3:** a config delivery whose read fails is retried with bounded backoff. If it
  cannot recover, the core closes the affected streams. Every failure is reported to
  the host. No silent catches anywhere in delivery or pushes.
- **M1:** after any (re)subscribe or channel reconnect, the store emits catch-up
  changes. GUIDE: a dedicated `LISTEN` session, reads from the primary. Contract test
  with a reconnect hook.
- **M2:** a totals snapshot whose window bounds changed while it was read is read
  again. The gateway retires own usage only when the applied windows cover it. Spec
  and GUIDE: replicas need synchronized clocks (state the bound).
- **M3:** gateway-record revisions never repeat for an instance. Contract and test.
- **M4:** contract tests:
  - assert snapshot consistency under concurrency (cursor, windows, config, live
    count, sequence);
  - `liveChanged: false` on every status;
  - `liveChanged: true` on forgetting a live gateway;
  - notification order;
  - a deliberately broken adapter as a negative control.
- **M5:** the GUIDE's ledger insert is idempotent by record ID. The retention boundary
  is documented.
- **M6:** what remains of the Postgres sketch:
  - the nullable key;
  - lock order (`store_meta` first);
  - unique violations → `{ saved: false }`;
  - the full gateway write with its notify.
- **M7, L7, L10, L11:** docs and leftovers. ARCHITECTURE's `usage` bullet,
  model-set wording, dead code and comments, fixtures, the host-app migration note
  (store interface, removed options, publish is replace, no versions), and history
  narration.
- **L1:** the false conflict from a late status.
- **L4:** `stop()` releases the store subscriptions.
- **L6:** the small-share warning filters by the scope's allowed models.
- **L8:** restore the three e2e checks with a priced model outside every USD limit.

**Regression tests:** port every surviving reproduction test from [R] and [B] (the
[B] files are in the session scratchpad under `audit-b2/`). Each fails before its fix
and passes after. Reproductions for dissolved findings are not ported; say so in
Result.

## Acceptance criteria

- Every finding is marked fixed (with its commit), dissolved (with the reason), or
  recorded, in the AUDIT.
- The specs state each new rule, dated.
- `scripts/check-all.sh` green three times in a row: **phase 4 and the plan end
  here**.

## Result

**Commits:** `c2b1b87` (kaiak-control), `9791c7e` (gateway), `d943b88` (docs),
`238b6ee` (two tests' synchronization), and this Result with the AUDIT outcome.

**Per finding:** the AUDIT's Outcome table (`docs/reviews/2026-10-07/AUDIT.md`) — fixed
with its commit, dissolved with the reason, or not built with the reason. In short:

- **Fixed:** H1, H3, M1, M2 (core side), M3, M4, M5, M6 (sketch), M7, L1, L4, L6, L7,
  L8, L10, L11.
- **Dissolved by phase 3** (each checked against the code, not assumed): H2 (no config
  epoch or mismatch state; totals apply by scope and type), L2 (no `configsSince`), L3
  (no epoch), L5 (no history), L9 (no epoch filter or its log line), L12 (publish
  replaces), M6's restore and epoch part.
- **Deviation — M2's gateway half not built.** The AUDIT asked the gateway to retire
  own usage only when the applied windows cover it. The cause of [B] B4 was the core
  choosing the windows before its snapshot read; that is fixed (the windows current
  after the read; read again if they changed). What a gateway-side rule would still
  cover is clock skew between processes, or between a process and a gateway, which
  predates this branch. It would need the totals to carry the core's window starts on
  the wire (a protocol field on both halves and every totals fixture). The spec and
  GUIDE instead require replica clocks within a second (`CONTROL-PROTOCOL.md`, Time
  reference).

**New interface pieces** (all in `control/kaiak-control`):
- `ControlPlaneStore.lastBatch(instance, epoch)` → `{ inEpoch, latest }`; a refused
  batch write returns `{ saved: false, cursors }`.
- The `catch-up` store change; `StoreContractSubject.reconnect`.
- `ControlPlaneOptions.deliveryRetryDelaysMs`; `ControlPlane.onDeliveryFailed`.
- `stop()` releases the store subscription; `start()` retakes it with a catch-up.

**Regression tests** (each run against the pre-fix code at `90ca3e4` first):

| Test | File | Finding | Before the fix |
|---|---|---|---|
| a stalled write of an older epoch's batch is not counted again after a new epoch | `control-plane/review.test.ts` | H1 ([R] R1, [B] B1) | fails: outcome `new-epoch`, not `duplicate` |
| a stalled first write of an older epoch's batch is counted once, newer epochs kept | same | H1, the crash variant | fails: `new-epoch` |
| a stale forget does not drop a gateway forgotten and joined again meanwhile | same | M3 ([R] R2, [B] B5) | fails: live count 0 |
| a late status from a replaced process does not raise a conflict or win | same | L1 ([R] R4) | fails: the old status stored |
| a config read that fails once after a publish is retried and delivered | same | H3 ([B] B3) | fails: nothing delivered |
| a config read that keeps failing is announced once the retries run out | same | H3 | fails: no announcement exists |
| a totals read across an hour boundary lists the windows current after the read | same | M2 ([B] B4, control half) | fails: the window left out |
| a catch-up from the store delivers what was missed while its channel was down | same | M1 | fails: no catch-up |
| stop releases the store subscription; start takes it again and catches up | same | L4 | fails: 4 subscriptions held |
| the contract tests fail a store whose snapshot is torn | `store-contract/negative-control.test.ts` | M4 ([B] B6) | [B] showed the old suite passed the torn store, 23/23 |
| store contract: per-epoch cursors, consistent cursor under concurrency, status and forget notifications, notification order, catch-up, non-repeating revisions | `store-contract/index.ts` | H1, M1, M3, M4 | not runnable on the old store: the cursor API changed (one old test loops forever on it) |
| `TestShareWarningNamesOnlyModelsTheScopeMayUse` | `gateway/internal/limits/shared_test.go` | L6 | fails: the warning names m1 |

Not ported, with the reason: [B] B2 and its Go test (the config-epoch path, removed);
[B] B4's Go half (a same-revision ack, removed); [B] B7 (it modelled the GUIDE's SQL,
which the fix changes); [B] B8 (history, removed); [R] R3 (`configsSince`, removed).

**Removal discipline:** nothing removed in phase 3 came back. The checklist greps are
unchanged; the only hits are the specs' dated "Rejected" lines and the
`GET /v1/config` 404 guard test.

**Suite** (2026-10-07):
- Control `npm test` 564 pass, 0 fail, 1 skipped (the catch-up contract test: the
  memory store has no channel to reconnect); `npm run lint` passes.
- `scripts/check-all.sh` three times in a row, all green: gateway e2e 112.3 s, 111.4 s,
  111.4 s; control 564/564; the cross-half tests 69.0 s each.
- An earlier set of three runs had two red, each from a test still synchronizing on
  totals arriving with the ack. Both are explained and fixed in `238b6ee`; the AUDIT
  records them. `TestUsageAcksFailingPastTheGraceRefusePricedBudgets` then passed 150
  times in a row.
- **Phase 4 and the plan end here.**
