# Step 2 — log vocabulary

**Status:** not started

## Intent

Every gateway log line follows the field table step 1 settled — on stderr today,
and on OTLP once steps 3–4 land. One vocabulary for both outputs.

## Files likely touched

- `gateway/internal/server/api.go` (the request line) and every other `slog` call in
  `gateway/` whose attributes the table renames (operational events: config, circuits,
  drain, control-plane contact, limits).
- Token counts: the request line computes `gen_ai.usage.input_tokens` (plain + read +
  written) and its cache parts from the record's units, rather than renaming.
- Tests that read log fields (`server`, `e2e`, `cmd/kaiak`, `limits`, `control`, …).
- `scripts/live/checks.go`: the live-test kit parses the request line.
- `docs/DEPLOYMENT.md`, `docs/testing/LIVE-BACKENDS.md`, the architecture pages, the
  GUIDE — wherever a log field is named.

## Decisions made during planning

- Attribute keys stay flat dotted strings in `slog` (`"http.response.status_code"`),
  not `slog` groups: the JSON on stderr then shows the same keys OTLP will carry.
- No dual output of old and new names (no backwards compatibility).

## Acceptance criteria

- Every attribute the gateway logs is in the table, and the table's names are what
  the code writes (a test over the request line's keys for success, limit refusal,
  `401` and upstream error).
- Repo-wide grep, outside `docs/plans` and `docs/reviews`: no old field name left
  where a log field is meant.
- `scripts/check-all.sh` green (live-test kit self-test included). Suite recorded.

## Result

_Not started._
