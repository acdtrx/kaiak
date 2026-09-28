# Step 11 — end to end across both halves

**Status:** done (2026-09-24)

## Intent

Prove the whole system as real processes: sample control plane, two gateways, the fake
backend.

## Files likely touched

- `gateway/e2e/` (or a new top-level `e2e/` driver — decide where a test that starts a
  Node and Go process belongs; it must run in the normal suite or a documented script
  run by `scripts/`)
- `README.md` — running with the sample control plane
- `docs/ARCHITECTURE.md`

## Decisions made during planning

- Scenarios: OVERVIEW → End-to-end verification, item 2. Shortened intervals via env
  so the run takes seconds.
- Lost ack simulated by a proxy between gateway and control plane that drops one ack.

### Decisions made during implementation

- **Where it lives**: `TestAcrossHalves` in `gateway/e2e/sample_test.go`, build tag
  `crosshalf`, reusing the Go harness (kaiak build, log-line waits, the in-process
  fake backend). `go test ./...` and `scripts/check-gateway.sh` stay Node-free (their
  vet and staticcheck run with the tag, so the tagged files are still checked); the
  new `scripts/check-all.sh` runs the gateway checks, `npm test` + `npm run lint` in
  `control/` (`npm ci` first when `control/node_modules` is missing) and the cross-half
  test with `-race -count=1` (the test cache does not see `control/`). It is the one
  verification command (AGENTS.md Commands, README Checks). Without Node or the
  installed dependencies the test fails with a message, never skips. Recorded in
  `docs/TECH-STACK.md` (Testing, settled 2026-09-24).
- **Proxy** (`gateway/e2e/proxy_test.go`, standard library): both gateways reach the
  sample through it. It gives the control plane a fixed address while the sample
  stops and starts again on a new port 0 (no free-port guessing), answers 502 while
  no sample is behind it, streams SSE through (`FlushInterval: -1`), and loses one
  usage answer: it forwards the instance's next `POST /v1/usage`, reads the answer
  (the sample counted the batch) and closes the gateway's connection.
- **Observation points**, no test-only API and no parsing of HTML or JSON with
  regular expressions (CODING-RULES §4): the sample's totals are read through the
  control protocol itself — the test holds the token and opens `GET
  /v1/stream?since=0` as instance `e2e-observer` (a stream alone does not join the
  live set; only statuses do), decoding `totals` events with the gateway's
  `internal/sse` reader and `control.DecodeTotals`. The global hourly token window
  (equal to the tokens of every answer served since the store began), the budget's
  USD window and `live_gateways` come from there; the test waits on the next totals
  event that meets the condition. The page's rendering stays covered by the
  sample's own tests. Gateways are read from their logs (`config applied` with
  trigger and version, `usage flushed`, `usage not flushed…`, `usage spool
  restored`), metrics (`kaiak_control_outage`, `kaiak_usage_spool_batches`,
  `kaiak_usage_batch_sends_total`) and the `x-ratelimit-*` headers, polling metrics
  (bounded) where no event exists.
- **Counted once, proven on a later snapshot**: a duplicate changes no total, so no
  push follows it. After the resend is acknowledged the test serves one more request
  on gw-b and waits for totals that include it — a consistent snapshot taken after
  the resend was handled, so a double count would show there.
- **Budget probe without spending**: a second fake backend answers every request 400
  with no usage; the model `priced-probe` routes to it and is covered by the same USD
  budget as `priced`. A probe on gw-b shows its budget decision (400 from the backend
  = allowed; 429 `budget_exceeded` = refused) without adding spend of its own, so the
  refusal can only come from gw-a's spend pushed by the sample.
- **Per-minute share observed** through a 1000-a-minute limit on `chat`: requests
  until the header shows 500 (the sample has pushed two live gateways), then the real
  check on `rpm` (limit 2 → 1 each, the second request 429).
- **Before stopping the sample** both spools are empty (every batch acked), so no
  batch counted by the first sample is resent to the second and counted in both
  stores. The second sample's store starts empty: the token total restarts from the
  outage's spooled usage.
- **No new environment variables**: the sample's defaults (1 s totals push
  coalescing, 30 s live timeout, 5 s sweep, 200 ms config debounce) do not slow the
  scenarios, and the gateway's 5 s seal interval (a protocol figure) and reconnect
  backoff stay as they are. Shortened by existing knobs only:
  `global.control_outage_grace_ms` = 1000 in the test's config,
  `KAIAK_CONTROL_BOOT_WAIT_MS=500` for the last-known-good boot,
  `KAIAK_DRAIN_TIMEOUT_MS=2000` (bounds the failed flush in the outage), the harness's
  `KAIAK_DRAIN_GRACE_MS=0`. Waits after the sample returns are bounded at 40 s: a
  gateway's reconnect delay grows during the outage up to the 30 s backoff cap.
- **Processes**: every process the test starts (sample ×2, kaiak ×3 over the run) is
  killed at test cleanup if still running; the proxy and fake backends are closed.
  A `go test` killed by its own `-timeout` skips cleanups — the test's waits are all
  bounded far below it, so it fails first.

## Result

- Scenarios, in `TestAcrossHalves` (≈ 24 s without the race detector, 32–52 s with `-race`, the recovery after the outage varying with the gateways' reconnect backoff):
  per-minute limit split by two; config file edit → `config applied config_version=2`
  on both gateways; usage from both in the sample's token total; a USD budget spent
  on gw-a refused on gw-b after the push; a lost usage ack counted once (the
  gateway's failed send and resend observed, the total exact); sample stopped → both
  gateways serve `chat`, refuse the money-limited model with 503
  `budget_unavailable` past the 1 s grace; gw-a restarted in the outage boots from
  last-known-good (version 2) with its spool restored and serves; sample started
  again (versions from 1) → both gateways resync and apply version 1, outage over,
  spools delivered, totals exact; SIGTERM on gw-b right after a request → `usage
  flushed` (drain) and the record in the sample's total before exit.
- Flakiness: with the totals read from the protocol stream, `scripts/check-all.sh` plus
  2 further runs with `-race -count=1`, all green (34.2 s, 51.7 s, 32.6 s); the page-based
  version before it passed 5 runs.
- `scripts/check-all.sh` green: gateway checks (gofmt, vet and staticcheck with the
  `crosshalf` tag, `go test -race ./...`, live-test kit lint and self-test), control
  `npm test` (354 pass, 0 fail) and `npm run lint` (tsc, boundaries ok), cross-half
  e2e `ok kaiak/e2e 34.2s`. Plan-end: phase 4 and the plan complete; the manual
  browser check of the page (OVERVIEW item 3) is pending the user.

## Acceptance criteria

- The e2e passes in the suite; plan-end: full suite green, committed.
