# Step 4 — e2e and docs

**Status:** not started

## Intent

Prove tiered prices end to end and bring the operator docs and examples to the new
shape. This step ends the phase: the full suite is green.

## Files likely touched

- `gateway/e2e/e2e_test.go`: a two-tier model; a request below and one above the
  threshold settle at their tiers' costs (the fake backend reports the usage).
- The cross-half e2e configs.
- `examples/config.json`, `examples/local-config.json`, `scripts/live/config.go`.
- `docs/DEPLOYMENT.md`: the Azure → Prices bullet with a two-tier example for
  models with long-context pricing.
- `docs/architecture/gateway.html` and anything else that shows a price.
- `README.md` if it shows a price.

## Acceptance criteria

- `scripts/check-all.sh` green, output recorded here.
- No `usd_per_million` left at a price entry's top level anywhere in the repo
  (grep), outside `docs/plans` and `docs/reviews`.
- Status recorded in every step file; the phase is committed.
