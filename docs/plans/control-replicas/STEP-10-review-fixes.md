# Step 10 — review fixes

**Status:** not started

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

(filled in when the step is done)
