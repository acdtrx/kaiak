# Step 4 — e2e and docs

**Status:** not started

## Intent

Prove the unit end to end and bring the operator docs to it. This step ends the
phase: the full suite is green.

## Files likely touched

- `gateway/e2e/e2e_test.go`: a model pricing `tokens_cache_write`; the fake backend
  reports writes on a non-stream and a stream request; each settles at the write
  price; the log line and `kaiak_usage_tokens_total` carry the unit; a request whose
  written tokens take it over a tier threshold settles at the upper tier.
- The cross-half e2e: a record with writes reaches the sample control plane, and its
  tokens count toward a `tokens_per_hour` total.
- `docs/DEPLOYMENT.md`: Azure → Prices — price `tokens_cache_write` for gpt-5.6 and
  later (1.25× input), with the example entry.
- `docs/architecture/gateway.html`, `docs/architecture/control-plane.html`: units,
  `Kaiak-Protocol: 4`.
- `docs/kaiak.md`, `README.md`: anywhere they list units or name the versions.
- `docs/BACKLOG.md`: remove the *Cache-write usage unit* entry (resolved).

## Acceptance criteria

- `scripts/check-all.sh` green three times in a row with the Go test cache cleared,
  output recorded here.
- Repo-wide grep, outside `docs/plans` and `docs/reviews`: every list of token units
  names five (or says why not); protocol 3 and config format 3 remain only in the
  old-version cases.
- Status recorded in every step file and the overview's verification status; the
  phase is committed.

## Result

_Not started._
