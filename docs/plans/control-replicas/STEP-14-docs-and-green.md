# Step 14 — round-2 docs, checklist, phase end

**Status:** not started

## Intent

Clear what remains of AUDIT-2, prove the removal is complete, and end phase 5 and
the plan green.

## Scope

- **Leftovers (2M8)** not already fixed by steps 11–13:
  - `docs/ARCHITECTURE.md`: line 155; the module diagram at 354–381, which still has
    `config-versions` and the `fastify --> cv` edge, and lacks `config-publishing -->
    schemas` and `store-contract`;
  - `docs/TECH-STACK.md:227-230`;
  - `config.schema.json:5` (both copies);
  - `GATEWAY.md:175, 2235`;
  - `CONTROL-PROTOCOL.md:936`;
  - `BACKLOG.md:81`, and the "Totals size bound" entry (resolved: remove it).
- **2L10:** complete the host-app migration note (`DEPLOYMENT.md`, the note around
  1006–1015):
  - removed exports and their replacements;
  - `publishConfig`'s result;
  - the status hash fields;
  - `onDeliveryFailed` and `deliveryRetryDelaysMs`;
  - the store interface changes of this phase (no sequence, config text, the
    snapshot);
  - totals as changes.
- **2L11:** rewrite the narrating Rejected lines as why the alternative fails.
- **2L12:** ARCHITECTURE's control-plane bullet names the subscription in
  `start`/`stop`.
- **`docs/architecture/*.html`:** follow decisions 21–27 wherever they describe
  ordering, rollback, totals contents or contact.
- **AUDIT-2 Outcome:** a table with every finding marked fixed (with its commit) or
  dissolved (with the decision). Nothing is left unaccounted for.

## Acceptance criteria

- Every removal checklist pattern from step 11 greps clean over the repo outside
  `docs/plans/` and `docs/reviews/`, apart from its allowed exception. Record the
  commands and their output in Result.
- `scripts/check-all.sh` green **three times in a row**. Record the times and counts.
- No orphaned `node --test`, sample or gateway process left (`pgrep` empty).
- **Phase 5 and the plan end here.**

## Result
