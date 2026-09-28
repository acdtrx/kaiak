# Step 3 — output limits (E9, N-M1)

**Status:** done (2026-09-25) — phase 2 continues with step 4; suite green

## Items

- `max_tokens` / `max_completion_tokens` above the model's `context_length` → `400
  invalid_request_error` (code e.g. `max_tokens_too_large`, `param` named).
- Overflow-safe limit arithmetic (`admits`, `waitFor`, `add`), reservation capped at
  2^53−1; regression test with `used > 0` and a saturated need.
- **N-S4** `best_of` read on chat too (reservation, `max_n`).

## Acceptance

The reviewer's reproduction as a failing-first test; both halves unaffected (no config
change) unless a new code needs the error table.

## Result

Each item had a test that failed on the old code first.

- **N-M1 overflow-safe limits** (`limits/window.go`, `limits/limits.go`,
  `server/limits.go`):
  - `window.admits` / `waitFor` share `fits(used, need, limit)`:
    `used <= limit && need <= limit - used` (need 0: `used < limit`) — no sum.
  - `add` saturates each count at the int64 maximum and records what it actually
    added in the hold, so a release takes back exactly that; `usedAt` (minute sum,
    fixed `used + pushed`) and `amountOf` (in + cached + out) saturate.
  - `checkLimits` caps the token reservation at `accounting.MaxAmount` (2^53 − 1).
  - Tests: `TestAHugeReservationCannotWrapTheCounter` (limits; the reviewer's
    scenario on `tokens_per_hour` and `tokens_per_minute`: team limit 1000, 10 settled,
    `Reserve(MaxInt64)` → refused with `Requested > Max`, used stays 10, 991 refused,
    990 admitted) — failed first: "admitted, want refused";
    `TestWindowArithmeticDoesNotOverflow` (all three window kinds; failed with
    admitted/no wait/used −9223372036854775799); `TestHugeMaxTokensDoesNotDisableTokenLimits`
    (server, over HTTP); `TestOutputMultiplicityReservationSaturates` now expects the
    capped 9007199254740991.
- **E9** (`server/params.go`, `server/errors.go`): a client `max_tokens` /
  `max_completion_tokens` above `metadata.context_length` → `400
  invalid_request_error`, code **`invalid_value`**, `param` = the key, message
  `"<key> is too large: N. This model supports at most C tokens (its context
  length), whereas you provided N."` Checked per key the endpoint owns (chat: both;
  completions: `max_tokens`), before the ceiling; within the context but above the
  ceiling → still lowered. Test `TestOutputLimitAboveTheContextIsRefused` (8 cases incl.
  exact context, above-ceiling lowering, completions' unowned `max_completion_tokens`,
  embeddings untouched; backend gets nothing on refusal; `class="invalid_request"`).
  `TestDeclaredDefaultsAndOutputLimit`'s "untouched" case used 999999 on an 8192
  context model — now within the context (8000), same intent.
- **Error class mapping** (N-P2's lesson, step 5's item done here): `invalid_value` and
  `n_too_large` → `invalid_request`; `TestEveryErrorCodeHasItsClass` lists every code
  and reads the Client API table from `docs/specs/GATEWAY.md`, failing on any code
  the table has and the test does not (failed first on the old mapping:
  `invalid_value`/`n_too_large: class internal`).
- **N-S4** (`server/inbound.go`): `best_of` read and validated on chat like on
  completions (`max_n`, `invalid_type`, reservation × `max(n, best_of)`). Rows added to
  `TestOutputMultiplicityMultipliesTheReservation` (failed first: 200 / wrong remaining).
- **Docs**: GATEWAY.md (error table row; owned fields; "Output limit above the
  context", settled 2026-09-25; Output multiplicity: `best_of` on chat, 2^53 − 1 cap;
  Check and reserve: overflow-safe form). Config schema's `max_n` description (both
  copies, `npm run sync-schemas`). Live kit: `output-ceiling` asks for the model's
  whole context length instead of 100000 (which E9 now refuses); LIVE-BACKENDS.md.
- **Suite**: `GOFLAGS=-count=1 scripts/check-all.sh` → `all checks passed` (gateway
  race tests, live-kit self-test 13/13/13/16, control tests + lint, cross-half e2e).

## Decisions made

- Code `invalid_value` rather than `max_tokens_too_large`: OpenAI answers the same
  mistake with `invalid_request_error` / `invalid_value` / `param` and the message
  `max_tokens is too large: …`; client libraries already handle it.
- The check is against `context_length`, not the input estimate + value: the estimate
  is rough (bytes ÷ 4), and prompt-plus-output overflow stays the backend's `400`.
- The reservation cap sits in the limits stage (server); the limiter's own arithmetic
  is overflow-safe independently, so any caller is safe.

## Decisions for the user to confirm

1. `invalid_value` (OpenAI's code) instead of a gateway-specific `max_tokens_too_large`.
2. ~~A negative output limit on a model without `output_limit` passes untouched.~~
   Superseded in step 4 (main-session decision, 2026-09-25): a negative
   `max_tokens` / `max_completion_tokens` is refused on every model, `400
   invalid_value` naming the key (llama-server reads `-1` as unlimited).
3. `best_of` on chat is owned (validated, multiplies the reservation) but not removed
   from the body — backends that do not know it keep refusing or ignoring it.
