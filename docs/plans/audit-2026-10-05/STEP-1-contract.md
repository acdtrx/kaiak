# Step 1 — contract

**Status:** not started

## Intent

Write the fixes' rules into the contracts and docs first: what the gateway reserves,
what a log line may never carry, how the exporter treats redirects, unreadable answers
and `Retry-After`, the refusal's retry rule, the L5/L6 vocabulary changes, and the
doc items D1–D5.

## Files likely touched

- `docs/specs/CONTROL-PROTOCOL.md` and `GATEWAY.md`: `api_key_env` reserves `OTEL_`
  (H1, extending the 2026-09-25 N-S2 decision); the remote-text rule for log lines
  (M2/M5); exporter Delivery — redirects refused (M1), what counts as delivered (M3),
  the retry wait (L1), a second signal during the final flush and the unthrottled exit
  report (L2), panicking values (L3); Limits → Refusal — the short `Retry-After` when
  only in-flight reservations block (M4); the field tables — `kaiak.limit.group`
  (L5), `kaiak.limit.used` in dollars on every line (L4), no status code for a
  response never sent (L6).
- `protocol/schema/config.schema.json` (+ `npm run sync-schemas`), fixtures and
  `cases.json` for `OTEL_` references.
- Docs: D1 (`control/kaiak-control/GUIDE.md` LiteLLM rule), D3 and D5
  (`docs/DEPLOYMENT.md`), D4 leftovers (`DEPLOYMENT.md:750`, `BACKLOG.md` OpenTelemetry
  entry, `docs/testing/LIVE-BACKENDS.md:370-372`, the Units bullet, a superseded note
  on `docs/plans/cache-write/OVERVIEW.md` decision 8). D2 is a release note — record it
  in this step's Result for the release.

## Acceptance criteria

- Each rule stated in its owning section, dated 2026-10-05, with the reason.
- Schema copies in sync; the new fixtures fail on both halves (expected reds, cleared
  by step 3); everything else green.

## Result

_Not started._
