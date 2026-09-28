# Step 11 — end to end and live

**Status:** automated part done (2026-09-25); live kit re-run deferred to the user

## Intent

Prove the fixes as real processes and on the GPU host.

## Items

- e2e additions for the audit scenarios not covered by unit tests (timeouts on real
  sockets, cap split across two gateways, config mismatch totals, projected ConfigMap).
- Live kit re-run on the GPU host two-vLLM setup with failover.

## Acceptance criteria

- `scripts/check-all.sh` green 3×; live kit green; no leftover processes. Plan end.

## Result

**Coverage check first.** Of the audit scenarios, those already proven at process
level before this step: cap split across two gateways (step 6's subtest of
`TestSharedLimitsAcrossGateways`, cap 4 → 2 on both), restart with a spent budget
(M5), first-event timeout and response timeout (step 5). Added only what needed real
sockets or processes and had none:

- `TestClientTimeoutsOnARealListener` (`gateway/e2e/timeouts_test.go`, ≈ 2.5 s): the
  built binary with `KAIAK_BODY_READ_TIMEOUT_MS` / `KAIAK_IDLE_TIMEOUT_MS` /
  `KAIAK_WRITE_TIMEOUT_MS` at 300 ms, raw TCP — the unauthenticated
  `Content-Length: 1` stall gets its 401 and the close; an authenticated stalled body
  gets `400 invalid_body` after the deadline, no backend request; an idle keep-alive
  is closed; a client that stops reading a 32 MiB stream ends `client_closed` and the
  backend slot is freed. The defaults (60 s / 120 s) exceed the 15 s wait bound, so a
  pass proves the environment reaches the listener.
- `TestStalledStreamEndsAndCountsTowardTheCircuit` (≈ 1.6 s): `stall_timeout_ms` 500
  on backend a, a stream hanging after one event — the client's stream stops without
  `[DONE]` no sooner than the stall timeout, relay_end `upstream_stalled`, upstream
  cancelled, `kaiak_upstream_attempts_total{outcome="broke_off"}`, a's circuit opens
  (threshold 1) and the next request goes to b. Checked: with the stall timeout at
  60 s the test fails at its bound.
- `TestRejectedConfigKeepsTheSpentBudget` (`shared_test.go`, ≈ 2.4 s): H3 with
  fakecontrol and two gateways — v1's USD budget spent, v2 adds a backend whose
  `api_key_env` is set on gw-a only and widens the budget; gw-a applies v2, gw-b
  logs `config rejected`, counts it, and reports `last_rejection` `{version 2,
  api-key-env-unset}` in its status. With v2's (unspent) windows pushed — the live
  count used as marker that the totals reached both — gw-a serves `priced`, gw-b
  still answers `429 budget_exceeded` and serves the uncovered model. Checked: with
  the config gate removed from `Limiter.TakeTotals`, gw-b answers 200.
- `TestAcrossHalves` (crosshalf) subtest "a projected ConfigMap update reaches both
  gateways" (≈ 0.2 s): the sample's config now starts in a projected-volume layout
  (`config.json → ..data/config.json`, `..data → ..<stamp>`); the kubelet's swap
  (`..data_tmp` renamed over `..data`, old directory removed) reaches both gateways
  as version 2. The existing edit subtest follows as version 3 (saved over the
  link: a plain file from then on); the last-known-good boot now expects version 3.
  Checked: with the sample's `..` filter removed, the subtest fails ("did not log
  the swapped config").

**Flake runs**: the three gateway tests `-count=5 -race` — 15/15 pass (runtimes
steady to 0.05 s); `TestAcrossHalves` `-count=5 -race` — 5/5 pass (41–56 s). Gateway
e2e package ≈ 44–46 s with the race detector (was ≈ 40 s).

**Doc fixes** (step 10's list): `seed` in `kaiak_config_loads_total`'s help and
GATEWAY.md's metric table; `docs/kaiak.md` loses "fallback order" (and "fallbacks"
in the v1 routing list — the same contradiction); GATEWAY.md termination grace = drain
grace + drain timeout + about 10 s (status 2 s, admin 5 s, file writes → 75 s at the
defaults), matching DEPLOYMENT.md; GATEWAY.md body memory names the non-stream
`choices` capture (≤ 4 MiB per answer in flight) outside the budget; AGENTS.md
Deployability: the flush runs within the drain's deadline, unsent batches stay
spooled, the `usage flushed` / `usage not flushed` line says which.

**Suite**: `GOFLAGS=-count=1 scripts/check-all.sh` three times in a row, all green:

```
run 1: ok kaiak/e2e 46.270s · live kit self-test passed · control 443/443 · boundaries ok · ok kaiak/e2e (crosshalf) 48.318s · all checks passed
run 2: ok kaiak/e2e 44.538s · live kit self-test passed · control 443/443 · boundaries ok · ok kaiak/e2e (crosshalf) 38.199s · all checks passed
run 3: ok kaiak/e2e 44.443s · live kit self-test passed · control 443/443 · boundaries ok · ok kaiak/e2e (crosshalf) 43.157s · all checks passed
```

No leftover processes (`pgrep -fl "kaiak|fakebackend|sample/src/main"` empty).

## Deferred to the user

- **Live kit re-run on the GPU host** (the models there changed; not run by the agent).
  Two vLLM copies of one model on ports 8001/8002, then:

  ```sh
  go -C scripts/live run . -kind vllm \
    -base-url http://gpu-host:8001/v1 \
    -base-url-2 http://gpu-host:8002/v1 \
    -model <model as both list it> \
    -max-in-flight 4 \
    -check-failover
  ```

  (`docs/testing/LIVE-BACKENDS.md`, "Two vLLM processes, one model"; add
  `-chat-defaults '{"chat_template_kwargs":{"enable_thinking":false}}'` for a Qwen3
  thinking model.)

## Open question

- **Drain reserve for the usage flush**: the flush gets only what is left of drain
  grace + drain timeout, so a drain that runs to its timeout leaves it no time and the
  batches stay spooled for the next start (documented, not changed). Should the drain
  reserve a few seconds for it?
