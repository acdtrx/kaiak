# Step 13 — round-2 gateway

**Status:** not started

## Intent

Implement step 11's contract in the gateway, and fix the gateway-side round-2
findings (`docs/reviews/2026-10-07/AUDIT-2.md`). Removal discipline as in step 12.

## Scope

- **Count every scope** (decision 22; 2H3):
  - The limiter keeps `tokens_per_hour` and `usd_per_month` counts, pushed base plus
    own usage, for global and every group on each request's path, whether or not a
    limit exists.
  - A limit is a check over its scope's count. A reload adds, removes or changes
    limits without touching a count.
  - Per-minute limits stay local shares, as today.
  - File mode uses the same limiter. Its `limits.json` follows if its shape changes;
    bump the format and say so in `GATEWAY.md`.
- **Totals merging** (decision 22):
  - The first totals after each stream connect replace every pushed base.
  - Later totals replace only the windows they list.
  - A window whose `window_start` is not the current window's is ignored, as today.
  - `totals.json` holds every counted window; bump its format if the shape changes.
- **`counted_through` per epoch** (decision 23; 2M2): own usage is retired by each
  epoch the gateway holds (its current one and any restored from the spool).
- **The ack hand-off** (2M3): a batch moves from the queue to the acknowledged list
  under one lock acquisition, counters and wake-ups included.
- **Acknowledged list bound** (2L6): entries whose usage the window roll-over already
  cleared are dropped.
- **Contact** (decision 25; 2M6): only stream bytes refresh the outage contact. An
  ack does not.
- **Rejection** (2M10): a config event whose hash is the running config's clears a
  set rejection and reports status.
- **Metric** (2L7): a gauge that is 1 while `last_rejection` is set. Name it in the
  standard style (`docs/specs/GATEWAY.md` metrics table), and add it to the
  `DEPLOYMENT.md` alert list.
- **Docs:** `GATEWAY.md` documents 2L5 (usage restored from another instance's spool
  is retired only once the new instance's first batch is counted; over-count only).
- **fakecontrol** follows: complete first totals, then changes; the `counted_through`
  array.
- **e2e** (2L8): `never(config applied)` after a reconnect to the same config. The
  cross-half replicas test covers a gateway that rejected a config which dropped a
  limit, and still enforces that limit's spend.
- **Leftovers in the gateway** (2M8): every gateway file listed in AUDIT-2 2M8, by
  rewording.
- **Regression tests:** port [B]'s Go reproductions from the session scratchpad
  (`audit-b3/audit_b_test.go`, `audit-b3/limits_audit_b_test.go`), renamed to
  describe the behaviour; [G]'s are rewritten from their descriptions. Each fails
  before its fix; say so in Result.

## Files likely touched

- `gateway/internal/limits/**`, `gateway/internal/control/**` (`client.go`,
  `usage.go`, `messages.go`, `schema.go`), `gateway/internal/metrics/`,
  `gateway/internal/fakecontrol/`, `gateway/cmd/kaiak/main.go`, `gateway/e2e/**`.
- `docs/specs/GATEWAY.md`, `docs/DEPLOYMENT.md`.

## Acceptance criteria

- `scripts/check-gateway.sh` passes (uncached).
- `scripts/check-all.sh` passes once, cross-half tests included.
- The removal checklist items on the gateway side grep clean, with the commands and
  their output recorded in Result.

## Result
