# Step 16 — round-3 gateway

**Status:** not started

## Intent

Implement decisions 29 and 31–34 in the gateway, as step 15 wrote them into
`GATEWAY.md`, and fix the gateway-side round-3 findings
(`docs/reviews/2026-10-07/AUDIT-3.md`).

## Scope

- **Stale-totals signal (decision 29, 3H1):**
  - An acknowledged own batch keeps `UsageWaitingSince` set until totals covering it
    are applied.
  - Batches restored from another instance's spool do not hold it.
  - The outage rule is unchanged otherwise.
- **Malformed totals (decision 31):** a totals event that fails decoding ends the
  stream and is logged. `client_test.go`'s skip test is replaced.
- **Spool until covered (decision 32, 3H2, 3L4):**
  - A batch stays in the spool, marked acknowledged and never resent, until a
    completed `totals.json` save covers it.
  - On restart, own usage is rebuilt from the spooled batches beyond the saved
    `counted_through`.
  - `totals.json` is written in the background:
    - at an interval (pick one and state it; `SnapshotInterval` is the obvious
      candidate);
    - at shutdown;
    - never on the stream goroutine;
    - with current windows only.
  - The acknowledged list becomes part of the spool, and the spool's own bounds apply.
  - Bump the spool format, and `totals.json` if its shape changes, in `GATEWAY.md`
    and `DEPLOYMENT.md`.
  - Without a data directory, behaviour is unchanged.
- **Counters by scope (decision 33, 3M1):**
  - Hour and month counters are kept until their window ends, whatever the config. A
    recreated ID reuses its counters, and reservations stay on them.
  - Pushed windows whose window has passed are pruned.
  - This applies to file mode too.
- **Memory (decision 34, 3M2):**
  - The gateway's semantic check counts allocated counters, as step 15 did on the
    control side.
  - Minute buckets are allocated only for per-minute counters.
  - A changes-only `TakeTotals` touches only the listed windows, the per-minute
    shares (only when the live count changed), and the counters with own usage.
- **fakecontrol (3L8):** an ended window is not listed at "0".
- **Regression tests:** port [C]'s Go reproductions from the session scratchpad
  (`audit-c3/control_audit_c_test.go`, `limits_audit_c_test.go`), renamed to describe
  the behaviour. Write [G]'s (G3-M1, G3-L1) from AUDIT-3. Each fails before its fix;
  say so in Result. Add a benchmark or recorded measurement for `TakeTotals` and the
  `totals.json` write at 7k groups.

## Files likely touched

- `gateway/internal/limits/**`, `gateway/internal/control/**` (spool, usage, stream,
  client), `gateway/internal/config/semantic.go`, `gateway/cmd/kaiak/main.go`,
  `gateway/internal/fakecontrol/`, `gateway/e2e/**`.
- `docs/specs/GATEWAY.md` (format numbers), `docs/DEPLOYMENT.md` (format notes).

## Acceptance criteria

- `scripts/check-gateway.sh` passes (uncached).
- `scripts/check-all.sh` passes once, cross-half tests included.
- Results record the measurements before and after.

## Result
