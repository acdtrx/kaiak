# Step 6 — end to end

**Status:** done — ends phase 3 and the plan (manual item 4 of the OVERVIEW waits for
the user's two-vLLM setup)

## Intent

Prove the reliability behavior with real processes.

## Files likely touched

- `gateway/e2e/`, `gateway/internal/fakebackend/` (scripted failure modes per
  instance), the cross-half test, `scripts/live/` (optional check), README

## Decisions made during planning

- Scenarios: OVERVIEW → End-to-end verification items 2–3.
- Live kit: a two-backend mode is required (changed at step start: the user is setting
  up two vLLM processes serving one model).

## Acceptance criteria

- `scripts/check-all.sh` green, e2e runs repeated 3× without flakes, no leftover
  processes. Plan end.

## Decisions made during the step

- **One gateway per scenario** (`gateway/e2e/reliability_test.go`): fresh backends and
  a fresh round-robin order, so a model's first request goes to its first deployment
  ("a") and each scenario's expectations are exact. Short timings come from config:
  circuit threshold 2 and probes every 100 ms; a 300 ms first-byte timeout on "a";
  one slot per backend, queues of one, a model with a 300 ms queue timeout beside one
  with 10 s. Waits are on log lines, `/metrics` and backend arrivals, all bounded.
- **Refused connections**: a fake closed right after it started gives a port that
  refuses; recovery is a fake listening again on that address (`fakebackend.NewAt`).
  The circuit scenario sends requests until the circuit opens (bounded at 6) rather
  than assuming which request's first attempt lands on "a" after a retry.
- **Cross-half observation**: the in-test proxy between the gateways and the sample
  keeps every `POST /v1/status` it forwards with the sample's answer. The test decodes
  the reports the sample accepted (2xx) with the gateway's own `control.Status` type
  and waits for one with the open circuit (with `opened_at`) and the queued model, then
  one with the queue empty. That is what the sample received, byte for byte; the page
  rendering of it stays covered by the sample's unit tests (step 5: `page.test.ts`,
  `events.test.ts`). The `/events` feed was not used: a gateways-section push also
  follows the 10 s periodic report, so its presence would not tie to this status.
- **Live kit CLI**: `-base-url-2` (env `LIVE_BASE_URL_2`) names a second backend of
  the same kind serving the same `-model`; the config gets backends `live` and
  `live-2`, every chat model deployed on both, embeddings on `live` only, and the
  circuit set to 2 failures / 1 s probes so failover takes seconds. `-max-in-flight N`
  caps each backend and adds the `capacity` check; `-check-failover` adds the
  `failover` check with `-failover-wait` (default 10m) bounding each wait. New checks:
  `spread` (six sequential requests served by both backends, from the log lines'
  `backend`), `capacity` (2N+2 requests at once all answered, the number queued
  reported but not required — a fast backend may never queue — and
  `kaiak_backend_max_in_flight` = N for both), `failover` (the user stops `live-2`
  when told; one request a second, every one must answer 200, until `live-2`'s
  circuit opens; `kaiak_circuit_open` 1 and two requests straight to `live`; the user
  restarts it; the probe's `circuit closed`; a request reaches `live-2` again). The
  self-test adds a "vllm with two backends" run: two fakes, `-max-in-flight 1`, the
  failover check driven by stopping the second fake and starting it again on its
  address.

## Result

- `gateway/e2e/reliability_test.go` (five tests, ~6 s with the race detector):
  - refused connections on "a" → served by "b" (`attempts=2`,
    `tried=a/…:upstream_unavailable,b/…:200`) until `circuit opened` (failures 2,
    `kaiak_circuit_open{backend="a",…} 1`); then one attempt on "b"; "a" listening
    again → `circuit closed` (trigger `interval`, a models probe reached it,
    metric 0) → of the next two requests "a" serves one; `kaiak_retries_total`
    reason `unavailable`;
  - "a" answers 500 once → `tried=a:500,b:200`, then "a" serves the next request,
    circuit closed;
  - "a" answers 429 and "b" 500 once → `tried=a:429,b:500,b:200` ("a" got one
    request), retries by reason `rate_limited` 1, `server_error` 1;
  - first-byte timeout on "a" (300 ms) → `tried=a:upstream_timeout,b:200`, the
    upstream request cancelled, log tokens in > 7 (a's estimate plus b's 7), two
    `kaiak_usage_records_total` (one per backend) for one
    `kaiak_request_duration_seconds_count`;
  - capped backends (one slot each, held streams): a request queues
    (`kaiak_queued_requests` 1), the next gets `429 queue_full` with `Retry-After: 1`,
    a request on the 300 ms model gets `429 queue_timeout` without `Retry-After`
    (its log line has `queue_wait_ms`), a stream ending hands its slot to the queued
    request (200 from "a", `queue_wait_ms`); rejection metrics by reason; then a
    request queued before SIGTERM is served during the drain when a held stream
    ends, the gateway exits 0, `drained` without a cut.
- Cross-half: `TestAcrossHalves` subtest "the sample receives an open circuit and a
  queued model in a status" — models `down` (a refused port, threshold 1 → `502
  upstream_unavailable`, `circuit opened`) and `held` (a backend with
  `max_in_flight` 1, one held stream, one queued request) on gw-a; the proxy sees an
  accepted report with `down`'s deployment open (with `opened_at`), `held` queued 1,
  `capped` 1 of 1 in flight, the working backend closed; then, after the stream ends
  and the queued request is served, a report with the queue empty and the circuit
  still open; token totals still add up across halves.
- Flake runs: `go test -race -count=3` of the five reliability tests — ok (20.2 s);
  `go test -race -tags crosshalf -run '^TestAcrossHalves$' -count=3` — ok
  (121.6 s); earlier `-count=3 -v` and `-count=2 -v` runs all passed.
- Live-test kit self-test: `self-test passed for vllm, openai, azure-openai, vllm
  with two backends` (13/13/13/16 passed; two-backend run: `spread` 3/3,
  `capacity` 4 at once all 200, `failover` 2 served while live-2 was down, both
  retried, circuit opened after 200 ms, closed by a probe 900 ms after the restart).
- `scripts/check-all.sh`: gofmt, vet, staticcheck (gateway and kit), `go test -race`
  (all packages, e2e 32 s), live-test kit self-test, `npm test` (380 pass), lint,
  cross-half e2e (43 s) — `all checks passed`. No expected reds: phase 3 and the plan
  end green.
