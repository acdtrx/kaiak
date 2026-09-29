# Step 4 — e2e and docs

**Status:** done (2026-09-29)

## Intent

Prove tiered prices end to end and bring the operator docs and examples to the new
shape. This step ends the phase: the full suite is green.

## Files likely touched

- `gateway/e2e/e2e_test.go`: a two-tier model; a request below and one above the
  threshold settle at their tiers' costs (the fake backend reports the usage).
- The cross-half e2e configs.
- Examples and the live-kit configs moved in steps 2 and 3; check nothing else
  builds a config in the old shape.
- `docs/architecture/control-plane.html` shows `Kaiak-Protocol: 2`: now 3.
- `docs/DEPLOYMENT.md`: the Azure → Prices bullet with a two-tier example for
  models with long-context pricing.
- `docs/architecture/gateway.html` and anything else that shows a price.
- `README.md` if it shows a price.

## Acceptance criteria

- `scripts/check-all.sh` green, output recorded here.
- No `usd_per_million` left at a price entry's top level anywhere in the repo
  (grep), outside `docs/plans` and `docs/reviews`.
- Status recorded in every step file; the phase is committed.

## Result

**What changed**

- `gateway/e2e/e2e_test.go`: `TestTieredPrices` — the file-mode gateway with the
  e2e config plus a model `tiered`: tier 0 (`tokens_in` 1, `tokens_cached` 0.5,
  `tokens_out` 2 USD per million) and a tier above 100 input tokens (3, 1.5, 6). The
  fake backend reports each request's usage (10 tokens out); the request's log line
  must carry the units and `cost_usd`:
  - 50 prompt tokens (below) → 0.00007 (tier 0);
  - exactly 100 → 0.00012 (tier 0: the threshold itself stays below);
  - 101 → 0.000363 (the upper tier, output included);
  - 101 with 51 cached → 0.0002865 (cached input counts toward the size; the upper
    tier's cached price applies);
  - and `kaiak_usage_cost_usd_total` for the model sums them, 0.0008395.

  Checked against a mutation: with the threshold moved to 99, the at-threshold case
  and the metric fail.
- The cross-half e2e configs moved in step 3 (the helper `testConfig` is their base);
  examples in step 2 and the live-kit configs in step 3.
- Docs: `DEPLOYMENT.md` (Azure → Prices: when a model needs a second tier, a two-tier
  example above 272000); `README.md` (config format and protocol at version 3);
  `docs/architecture/control-plane.html` (`Kaiak-Protocol: 3`, footer);
  `docs/architecture/gateway.html` (cost picks the tier by input size; prices per unit
  and tier; footer); `docs/kaiak.md` (prices are tiered by input size);
  `control/kaiak-control/GUIDE.md` (header names format 3 / protocol 3).
- Repo-wide check, outside `docs/plans` and `docs/reviews`: every `usd_per_million`
  is inside a tier, in the schemas, or in prose about the tier shape, except the
  deliberate old-shape cases — the invalid fixture `price-entry-usd-per-million.json`
  and the discarded last-known-good file in `internal/control/seed_test.go`.
  `format_version: 2` remains only in the invalid fixture `format-version-2.json` and
  that seed test; protocol version 2 only in the mismatch tests.

**Suite** — `scripts/check-all.sh` three times in a row, the Go test cache cleared
before each (no cached package): all three passed.

- Gateway: gofmt, vet, staticcheck; `go test -race ./...` every package ok (e2e
  99.3 s, 93.5 s, 99.1 s); live-test kit lint and self-test pass.
- `control npm test`: 558 pass, 0 fail, each run. `npm run lint`: `tsc` clean,
  `boundaries ok`.
- Cross-half e2e: ok (48.8 s, 44.1 s, 58.8 s).
- The phase is green; no expected reds remain.
