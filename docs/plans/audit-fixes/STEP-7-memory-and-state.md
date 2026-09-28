# Step 7 — memory and state

**Status:** done (2026-09-25) — commits `4990c32` (M2), `e047e75` (M11)

## Items

- **M2** global in-flight body-bytes budget acquired before reading (config, default
  sized from the body cap); drop `rq.body` and the edited copy after the first event;
  default body cap 4 MiB.
- **M11** exclusive lock file in the data dir (second process refuses to start);
  optional `KAIAK_SEED_CONFIG_FILE` used only when neither control plane nor LKG is
  available at boot.

## Acceptance criteria

- Tests: budget blocks excess bodies, bodies released after first event, lock refusal,
  seed config boot.

## Result

Each test below was run and seen failing before its fix (behavioral failure, or a
build failure where the test needs the new API; the provider and release tests were
run against the old code with the new-API assertion disabled and failed on the
behavior).

**M2 — memory bounds** (`gateway/internal/server/bodies.go`, `inbound.go`,
`upstream.go`, `errors.go`, `metrics.go`; `provider/body.go`, `openai.go`;
`metrics/ops.go`; `config/snapshot.go`; `protocol/schema/config.schema.json`)

- `BodyBudget` (env `KAIAK_BODY_MEMORY_BYTES`, default 512 MiB, whole bytes > 0):
  taken before the body is read — `Content-Length` at once (one allocation of that
  size, then read to EOF so keep-alive survives), unknown lengths by doubling steps
  from 64 KiB, each step taken before it is allocated. Refused at once with
  `503 server_busy`, `Retry-After: 1`; new error class `server_busy` in
  `kaiak_errors_total`. Given back by `releaseBody`: at the start of
  `relayResponse` (no retry after that; meters and limits already took the size) and
  as a request finisher.
- Effective body cap = min(`max_request_body_bytes`, budget) → 413 with that limit;
  a config whose cap exceeds the budget is applied with a warning.
- Provider: the edited upstream body is a releasable reader (drops its bytes at EOF
  or close); `GetBody` works until `Send` returns, then errors; the httptrace
  closure no longer captures `*provider.Request` (it kept the client's body alive
  for the stream's life, defeating the release).
- Default `max_request_body_bytes` 16 MiB → 4 MiB (schema default, Go default,
  GATEWAY.md). No kit type or fixture asserts the default.
- Tests: `TestBodyBudgetRefusesARequestWhenSpent` (real concurrency: 503 +
  Retry-After, metric class, budget back to 0 after the requests end),
  `TestBodyIsReleasedOnceTheResponseRelays` (counter 0 while a paced stream is open —
  failed before the fix with 600 held), `TestBodyOverTheWholeBudgetIsTooLarge`,
  `TestBodyOfUnknownLengthTakesTheBudgetAsItArrives`,
  `provider.TestSendKeepsNoRequestBodyOnceTheFirstEventIsIn` (weak pointer + GC on
  the client's body; the upstream request cannot replay the edited copy — both failed
  before), `TestDefaultsAppliedForOmittedFields` (4 MiB), `cmd/kaiak`
  `TestBodyMemorySetting`, `TestBodyCapAboveTheBudgetIsWarned`.

**M11 — data directory** (`state/state.go`, `lock_unix.go`, `lock_other.go`;
`control/client.go`; `cmd/kaiak/main.go`)

- `state.Open` takes `syscall.Flock(LOCK_EX|LOCK_NB)` on `<dir>/kaiak.lock`
  (build tag `unix`; no-op elsewhere, documented); `Dir.Close` releases it; `run`
  holds it until it returns. A second gateway fails: "data directory … is in use by
  another kaiak process (it holds the lock on …/kaiak.lock)".
- `KAIAK_SEED_CONFIG_FILE`: control-plane mode only (startup error beside
  `KAIAK_CONFIG_FILE`), read and `config.Parse`-validated at startup. `Boot`
  applies it (trigger `seed`, via `Applier.Apply`, no version, never saved as LKG)
  after the snapshot and the LKG both failed **and** the control plane was not
  reached. The client then fetches the snapshot (position 0) and its config
  replaces the seed.
- Tests: `state.TestSecondOpenOfADirectoryInUseFails` (+ reopen after Close),
  `cmd/kaiak` `TestRunRefusesADataDirectoryInUse`, `TestSeedConfigSetting`;
  `control` `TestSeedConfigServesAColdBootWithTheControlPlaneDown` (seed served, no
  LKG written, the control plane's config replaces it and becomes LKG),
  `TestLastKnownGoodWinsOverTheSeedConfig`,
  `TestSeedConfigIsIgnoredWhenTheControlPlaneAnswers`,
  `TestSeedConfigIsIgnoredWhenTheControlPlaneHasNoConfig`.
- `state.TestVersionedRoundTrip` counted every directory entry; it now ignores the
  lock file (its intent — no temporary files left — unchanged).

Docs: GATEWAY.md (error table rows, "Request bodies" under Request pipeline, env vars,
data-directory lock, seed in Boot, `seed` trigger, readiness, error classes);
ARCHITECTURE.md (`state` lock, `control` seed). CONTROL-PROTOCOL.md does not state the
body-cap default, so no edit there.

Suite: `GOFLAGS=-count=1 scripts/check-all.sh` → `all checks passed` (gofmt, vet,
staticcheck, gateway race tests incl. e2e, live kit, control 436/436, lint,
cross-half e2e); run twice, before and after the commits, both green. The M2 commit was also checked on its own (detached worktree:
vet + race tests of `cmd/` and `internal/` green). No expected reds.

Flake hunt (the main session's report after step 6): an extra
`go test -race -count=3 -json ./...` of the gateway (e2e included, partly run
concurrently with other test runs, so under load) — every package passed, no test
failed; the failure seen after step 6 did not reproduce, so no root cause could be
named.

## Decisions for the user to confirm

1. **Budget size and refusal**: default 512 MiB; refuse at once (`503 server_busy`,
   `Retry-After: 1`), no waiting; its own error class `server_busy`.
2. **Budget counts the client's body only**, not the provider's edited copy (alive
   only while an attempt is sent) nor parse garbage — peak ≈ 2× budget; the spec says
   to size `GOMEMLIMIT` above it (step 10's guide).
3. **Unknown-length bodies** take the budget in doubling steps from 64 KiB (capped at
   the limit), so a small chunked body briefly holds up to 64 KiB of budget.
4. **Cap above budget**: not a startup error (a control-plane config cannot fail the
   start); effective cap = min, warned on every apply.
5. **Release point**: the body is dropped when relaying starts (before the status is
   written), which is the first moment no retry is possible.
6. **Seed trigger condition, narrow reading**: only when the control plane was *not
   reached* — connection failure/timeout, a cut body, or an answer without this
   `Kaiak-Protocol` (a proxy's 502/503, or another protocol version). A control
   plane that answers (`503 config-unavailable`, a refused token, a snapshot the
   gateway rejects) does not trigger the seed. Alternative: seed as the last fallback
   whenever boot ends with no config.
7. **Seed and status/totals**: the seed has no control-plane version — status reports
   no applied version; totals do not apply until a control-plane config is in force.
8. **Seed validation at startup** is schema + semantic only; backend credentials
   (`api-key-env-unset`) are checked when it is applied.
9. **Lock file** `kaiak.lock` stays on disk after release (removing it would race a
   waiting opener); non-Unix builds take no lock.

## Deviations

- One new metric label value (`server_busy`) lands here rather than in step 8: without
  it the refusal would have counted as `internal`.
- `NewAPI` gained a `*BodyBudget` parameter.
