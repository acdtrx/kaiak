# Step 4 — per-key concurrency and body memory (E10, N-S1)

**Status:** done (2026-09-25) — **phase 2 complete**; suite green

## Items

- Global `max_concurrent_requests_per_key` (default 16), per gateway, counted from
  authentication (before the body is read) to the end of the request; above it → `429`
  (code e.g. `concurrency_limit_exceeded`, `Retry-After: 1`). Config on both halves.
- Body budget taken as bytes arrive (the doubling path even with a known
  `Content-Length`, capped at the declared length); no up-front allocation.
- **N-S5** `MaxHeaderBytes` 64 KiB on both listeners.
- Added by the main session: a negative `max_tokens` / `max_completion_tokens` is
  refused on every model.

## Acceptance

The reviewer's scenario (idle connections declaring 4 MiB) no longer starves others;
per-key limit tested incl. release on every path.

## Result

Each item had a test that failed on the old code first.

- **E10 per-key concurrency** (`server/keylimit.go`, `pipeline.go`, `errors.go`,
  `metrics.go`; `config/`; schema both copies; kit `types.ts`; fixtures):
  - Config `global.max_concurrent_requests_per_key`, integer ≥ 1, default 16 (Go
    walker + snapshot default, schema `default`, TS type). Fixtures: `full.json` (32,
    checked by `TestExplicitValuesOverrideDefaults`), `at-bounds.json`, the
    config-snapshot message fixture; invalid `max-concurrent-per-key-zero.json` and
    `-fractional.json`. The kit's fixture test failed first ("must NOT have
    additional properties"); the Go snapshot test failed to build first.
  - New stage `key_concurrency` right after `auth`, before `inbound`: one counter
    per key ID (entry removed at 0), limit from the request's snapshot; the slot is
    released by a request finisher.
  - Refusal: `429`, type `requests`, code `concurrency_limit_exceeded`,
    `Retry-After: 1`, message names the limit; body unread (connection closes).
    Class `rate_limited`.
  - Tests: `TestPerKeyConcurrencyLimit` (limit 2, two stalled streams; chat and
    `GET /v1/models` of the same key refused, another key served, backend untouched,
    `kaiak_errors_total{class="rate_limited"} 2`, log `error_code`; slots back after
    the streams end) — failed first ("k-eval in flight 0, want 2").
    `TestPerKeySlotIsReleasedOnEveryPath` (limit 1; success, invalid JSON, 413,
    404, 502, rate limited, queue full, client disconnect, broken-off stream =
    handler panic, drain cut; each then 0 in flight and the next request admitted) —
    checked by mutation: removing the finisher fails all 10 cases.
  - `TestMetricsUnderConcurrentRequests` sends 24 requests at once on one key: the
    test raises the per-key limit to 24 (its subject is metrics, not the limit).
- **N-S1 body memory as bytes arrive** (`server/bodies.go`): one read path for known
  and unknown lengths — the buffer doubles from `bodyStep` (now **4 KiB**, was 64 KiB),
  each step taken from the budget before it is made, capped at the declared
  `Content-Length` (and the cap); a declared body that ends short is `invalid_body`.
  Test `TestIdleDeclaredBodiesDoNotStarveOtherKeys` (the reviewer's scenario scaled
  down: budget 2 MiB, cap 1 MiB, 20 raw connections of one key declaring 1 MiB and
  sending nothing, then another key's chat) — failed first: the victim got `503
  server_busy`, 2 MiB held by 2 connections, attackers answered 503. Now: 16 held
  at ≤ 4 KiB each, 4 refused `429 concurrency_limit_exceeded`, the victim `200`.
- **N-S5** (`server/listener.go`): `MaxHeaderBytes` 64 KiB in `Listen` (used by both
  the API and admin listeners). `TestOversizeHeadersAreRefused`: 80 KiB of headers →
  `431`, 32 KiB → `200` — failed first (80 KiB got 200).
- **Negative output limits** (`server/params.go`, `errors.go`): a client key below 0
  → `400 invalid_request_error`, code `invalid_value`, `param` = the key, message
  `Invalid '<key>': integer below minimum value. Expected a value >= 0, but got N
  instead.`, before the context check, on every model. `TestNegativeOutputLimitIsRefused`
  (7 cases: no-`output_limit` model, with one, the second chat key, completions,
  `0` passes, completions' unowned `max_completion_tokens` and embeddings untouched)
  — failed first (4 refusal cases got 200). `TestDeclaredDefaultsAndOutputLimit`'s
  "negative … lowered" case replaced by an in-ceiling value (same position, keeps
  its round-robin order). STEP-3's decision note #2 marked superseded.
- **Docs**: GATEWAY.md — error table row (`concurrency_limit_exceeded`), the
  `invalid_value` row now "below 0 or above the context"; pipeline stage 1; Limits
  → "Per-key concurrency" (settled 2026-09-25); "Output limit out of range" (below 0,
  settled 2026-09-25); request bodies (4 KiB doubling, N-S1); Lifecycle 64 KiB
  headers (N-S5); defaults list; error classes. ARCHITECTURE.md pipeline list.
  DEPLOYMENT.md setting. BACKLOG.md split entry now names the setting.
- **Suite**: `GOFLAGS=-count=1 scripts/check-all.sh` → `all checks passed` (gateway
  gofmt/vet/staticcheck/race tests incl. e2e, live-kit self-test 13/13/13/16,
  control 445 tests + lint, cross-half e2e).

## Decisions made

- Code `concurrency_limit_exceeded`, type `requests`: OpenAI has no concurrency
  refusal; its rate-limit shape (`429`, type `requests`) is what clients already
  retry, and a distinct code tells it from `rate_limit_exceeded`.
- Class `rate_limited` (no new class): the caller's own doing, like its rate limits;
  the log line's `error_code` separates the two.
- Every authenticated request counts, the model endpoints included — simplest, still
  bounded, and model endpoints finish in microseconds.
- No per-key metric label (unbounded series); the refusal shows in
  `kaiak_errors_total{class="rate_limited"}` and in the requests metric's 429s.
- First body step 4 KiB instead of waiting for the first byte: a one-byte client
  would still pin a full step, so the step size is what matters; the extra doublings
  cost about one more copy of a large body.
- Negative output limit: code `invalid_value` (as in step 3) with OpenAI's
  "integer below minimum value" wording; `0` passes.

## Decisions for the user to confirm

1. Class `rate_limited` for `concurrency_limit_exceeded` rather than its own class
   (e.g. `concurrency_limited`) — an alert on per-key concurrency would need the log.
2. The model endpoints count against the per-key limit (a client listing models while
   16 generations run is refused).
3. First body step 4 KiB (idle declared bodies hold ≤ 4 KiB each; 16 × 4 KiB per key
   per gateway).
4. Negative output limits are refused (not lowered) on every model, `0` allowed.
