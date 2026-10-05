# Step 3 — credentials and remote text

**Status:** not started

## Intent

H1 on both halves, and M5: no remote bytes in provider or control-plane log lines.

## Files likely touched

- Gateway: `config/schema.go` (the `api_key_env` guard), `provider/provider.go` (last
  guard), `provider/wire.go`, `routing/modelcheck.go`, `server/api.go` (transport
  errors as local classes), `control/transport.go`, `control/client.go` (version
  mismatch, error codes).
- kaiak-control: `src/config/` validation for `OTEL_`.
- Tests: Codex's `provider/audit_b_test.go`, `control/audit_b_test.go` and
  `audit-b.test.ts` ported.

## Acceptance criteria

- Ported repros fail before, pass after; step 1's `OTEL_` fixtures pass on both halves.
- `scripts/check-all.sh` green; suite recorded.

## Result

_Not started._
