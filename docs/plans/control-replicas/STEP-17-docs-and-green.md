# Step 17 — round-3 docs, phase end

**Status:** not started

## Intent

Clear the remaining AUDIT-3 wording and docs, account for every finding, and end
phase 6 and the plan green.

## Scope

- **3M7:** `DEPLOYMENT.md:997-999`. In both modes, a limit added mid-window checks its
  scope's count so far.
- **3L6:** the migration note in `DEPLOYMENT.md` adds:
  - `configEpoch()`, `configsSince()`, `publishConfig`'s `beforeSave`;
  - the `ListenerEvent` variants (`limit-carried-over` removed, `delivery-failed`
    added);
  - `StoredConfig` → `PublishedConfig`/`ConfigEntry`;
  - this phase's spool format and store contract changes.
- **3L7:** each Rejected line listed in AUDIT-3 says the failure it would cause today.
- **3L8:**
  - `GATEWAY.md:2260` (the ack metric is not contact);
  - `protocol/fixtures/duplicate-members/cases.json` ("retires");
  - the boot diagram in `docs/architecture/control-plane.html` (totals come on Run's
    stream; the retried-ID list);
  - `CONTROL-PROTOCOL.md:43, 722`;
  - `spool.go:25`;
  - the OVERVIEW's superseded Scope, Risks and Verification lines, marked as decisions
    4–19 are.
- **`docs/architecture/*.html` and `ARCHITECTURE.md`:** follow decisions 29–34 where
  they describe outage, spool, totals or counters.
- **AUDIT-3 Outcome:** a table with every finding marked fixed (with its commit) or
  dissolved (with the decision).

## Acceptance criteria

- No grep hit for the phrasings 3L8 names. Repeat step 14's removal-checklist greps;
  they are still clean.
- `scripts/check-all.sh` green **three times in a row**. Record the times and counts.
- No orphaned `node --test`, sample or gateway process left.
- **Phase 6 and the plan end here.**

## Result
