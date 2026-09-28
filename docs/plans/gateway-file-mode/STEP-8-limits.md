# Step 8 — limits

**Status:** done (2026-09-24) — phase 3 continues with step 9; suite green, no expected reds.

## Intent

Enforce every limit in file mode with one local window counter: check and reserve
before routing, settle after accounting, persist longer windows across restarts.

## Files likely touched

- `gateway/internal/limits/` — window counters, scope resolution, reserve/settle
- `gateway/internal/state/` — file-mode usage snapshot (format-versioned)
- `docs/specs/GATEWAY.md` — header names and snapshot behavior recorded

## Decisions made during planning

- One counter type parameterized by window kind: **sliding minute** (bucketed by
  second), **fixed UTC hour**, **calendar month UTC**. In control-plane mode (P2) the
  same counter runs per-minute windows only, on the pushed share.
- Scopes checked: global, team, workload for workload keys; global, user for personal
  keys (user limits = global default merged with the user's overrides). A limit applies
  when its model set includes the request's model.
- Reserve = one request + (estimated input tokens + output limit) + nothing for USD
  (cost is known only after); settle replaces the reservation with actual units and
  cost. USD limits therefore overshoot by in-flight requests — matches principle 6.
- Rejection: `429`, `Retry-After`, `x-ratelimit-limit-*` / `-remaining-*` /
  `-reset-*` for requests and tokens, naming the tightest scope's numbers.
- Snapshot: hour and month counters written every 30 s and on shutdown; a snapshot with
  a different format version is discarded with a log line. Per-minute windows are not
  persisted.
- Config reload keeps counters for limits that still exist (matched by scope + type +
  model set); removed limits drop their counters.

## Acceptance criteria

- Tests: each window kind trips and recovers; every scope enforced; reservation
  released on settle and on error; headers present; snapshot restores hour/month
  counters and rejects a wrong version; reload preserves matching counters.

## Result

Commands run (2026-09-24):

- `scripts/check-gateway.sh` — gofmt, `go vet`, staticcheck 2026.2.1 clean;
  `go test -race ./...` **pass** (`cmd/kaiak`, `accounting`, `auth`, `config`,
  `limits`, `provider`, `routing`, `server`, `state`). Also `-count=5` on `limits`,
  `server`, `cmd/kaiak` — pass, no flakes.
- `npm test` in `control/` — 87 tests **pass**; `npm run lint` — `tsc` clean,
  `boundaries ok`.
- Manual, built binary against the fake backend from a throwaway main (deleted with
  every scratch file, scratch data dir included); workload `wl` with 2 requests/min
  and 10000 tokens/hour, global 0.0004 USD/month, model priced 10 / 20 USD per million
  in / out (each request 7 + 4 tokens = 0.00015 USD):
  - workload requests 1–2 → `200` with `x-ratelimit-limit-requests: 2`,
    `remaining-requests: 1` then `0`, `reset-requests: 1m0s`,
    `limit-tokens: 10000`, `remaining-tokens: 9921` then `9910` (reservation: 60-byte
    body → 15 + output default 64; the second also has the first's 11 settled
    tokens), `reset-tokens: 34m53s` (to the UTC hour);
  - request 3 → `429`, `Retry-After: 60`, `type: requests`,
    `code: rate_limit_exceeded`, "Rate limit reached: workload limit of 2 requests
    per minute (used 2). Retry after 1m0s.";
  - user key → `200` (global spend 0.00045 USD, over the budget by the one request in
    flight), next → `429 budget_exceeded`, `type: budget`, `Retry-After: 578093`
    (month end), no rate-limit headers (the user has no request/token limits);
  - shutdown → `limits snapshot written trigger=shutdown windows=2`; `limits.json`
    held `usd_per_month … "used":450000` and the workload's `tokens_per_hour`
    `"used":22`;
  - restart → `limits snapshot restored windows=2 dropped=0`; the user key still
    `429 budget_exceeded` (month counter survived), the workload's
    `remaining-tokens: 9978` (hour counter survived) and `remaining-requests: 2`
    (minute window not kept).

Delivered:

- `gateway/internal/limits`:
  - `window` — the one counter type, parameterized by kind (`SlidingMinute`: 60
    one-second buckets; `UTCHour`; `UTCMonth`), counting an integer quantity
    (requests, tokens, nano-USD) against an effective `limit` (P2 sets a share
    there); `reserve` / `settle` / `release` / `keep`, `waitFor(need)` (Retry-After)
    and `resetIn` (reset headers); clock passed in on every call.
  - `Limiter` (`New(holder, now)`): counters per limit and scope keyed by scope +
    owner ID + type + model set; follows the holder's live snapshot (re-matched when
    the pointer changes); `Reserve(Subject, tokens) (*Reservation, *Rejection)`,
    `Settle(res, *UsageRecord)`, `Usage()` (read access for step 9's metrics and
    tests), `SaveSnapshot(dir)` / `LoadSnapshot(dir)`, `SnapshotInterval`.
- `gateway/internal/server`: stage `limits` between `model_params` and `routing`
  (body endpoints only), settlement finisher registered before accounting's (so it
  runs after it and reads `rq.usage`), `Retry-After` and `x-ratelimit-*` headers on
  refusals and admitted responses, `429` answers. `NewAPI` takes the limiter.
- `cmd/kaiak`: limiter built over the holder; snapshot restored after the startup
  load, before the listeners; periodic writer goroutine owned by `run` (stops on
  cancel); final write after the listeners stop (`saveLimits(…, "shutdown")`, an
  invocable function step 10 slots into the drain sequence). Interval writes log at
  debug level, startup/shutdown at info, failures at error/warn.
- Docs: `GATEWAY.md` — error table rows, pipeline notes, Limits (local counters,
  check-and-reserve, settle, refusal, headers, reload, snapshot, drift per window),
  sinks bullet, data dir; `ARCHITECTURE.md` — `accounting` and `limits` entries.

Decisions beyond the plan:

- **Settlement through a finisher, not a sink**: settling needs the request's own
  reservation; a sink sees only the record and would need a lookup table from record
  to reservation. The finisher has both, and also covers "no record" (release).
  Limits register no sink; `main` and `GATEWAY.md` say so.
- **Atomicity by one lock**: the limiter's mutex covers check and reserve across all
  of a request's counters — check every counter first, reserve only if all admit —
  so there is never a partial reservation to roll back (tested: a refusal by the
  team leaves the workload's and global counters untouched; 200 concurrent requests
  against a limit of 10 admit exactly 10). A handful of counter operations per
  request under one mutex; revisit only if profiling shows contention.
- **Counters follow the live config, not the request's snapshot**: a lowered limit
  applies immediately, and there is no config-apply hook to add — the limiter
  re-matches when `holder.Current()` changes.
- **Cached tokens count** toward token limits (`tokens_in + tokens_cached +
  tokens_out`): every token the backend handled; reasoning is inside `tokens_out`.
- **A request counts whatever its outcome** (upstream failure, disconnect, no record):
  it took a slot. Token reservations of zero-unit records are released.
- **Where amounts count**: a reservation in the bucket/window where it was made; the
  actual at settlement time; a reservation whose bucket/window has passed is not
  released from the new one. Every amount counts in exactly one window.
- **USD limits reserve nothing**: the reservation holds the cost counters with 0 and
  settlement adds the record's cost to those same counters.
- **Codes**: request/token limits `429` `type: requests|tokens`,
  `code: rate_limit_exceeded` (OpenAI's shape); USD `429` `type: budget`,
  `code: budget_exceeded`. A token request larger than the limit itself is refused
  with a message saying so (same code).
- **Rejection reported** is the refusing limit with the longest wait;
  `Retry-After` is that wait (all refusing limits have room by then).
- **Headers on admitted responses too**, from the least-remaining limit per kind
  (tokens: minute and hour alike); reset as a Go duration (`1m0s`, `34m53s`), whole
  seconds rounded up; none for USD or when no limit of a kind applies.
- **Model endpoints are not limited** (no model capacity used) — tested.
- **Snapshot keeps settled usage only** (`used − held`): in-flight reservations are
  left out; windows with zero usage are not written. An unreadable snapshot is logged
  and ignored (cache).

Deviations: none from the acceptance criteria.

Open doubts:

- OpenAI client libraries retry `429` automatically (twice by default, ignoring a
  `Retry-After` longer than their cap), so a spent budget costs a client a few quick
  refused retries. A different status (e.g. `402`/`403`) would stop that but break
  the brief's "still 429" and OpenAI's quota convention.
- Token usage of a long request moves windows: while it runs its reservation counts
  in the minute it started; its actual counts in the minute it ends. A stream longer
  than a minute is invisible to minute windows in between (bounded by its
  reservation at start).
- Every configured user gets its default-user counters up front (one sliding minute
  ≈ 1 KB). Fine for hundreds to low thousands of users; a very large user list would
  want lazy counters.

