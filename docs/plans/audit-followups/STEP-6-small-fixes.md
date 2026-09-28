# Step 6 — small fixes

**Status:** done (2026-09-25) — suite green; **phase 3 complete**

## Items

- **E12** backend-side model names accept the backend's naming (e.g. `/models/x.gguf`;
  both halves + fixtures); docs mention llama-server `--alias`.
- **N-P1** `applied_config_epoch` in status (both halves, page comparison).
- **N-C4** `fakecontrol.Restart` drops old streams (the `-cpu=1` flake); verify with
  `-cpu=1,2 -count=20`.
- **N-S2** refuse `api_key_env` names starting with `KAIAK_` (both halves).
- **N-P5** status `max_in_flight` int64; **N-P6** stale schema descriptions; **N-P7**
  GATEWAY.md 5xx text; **N-P8** retry budget counts sent retries; **N-P9** fixture gaps;
  **N-P10** stream-open 4xx falls back to a snapshot; **N-P11** status `ready` after a
  seed boot; **N-P12** doc drift; **N-M2** accept earlier pushed windows matching the
  gateway clock, warn on future ones; **N-S6** cap page streams in the sample.
- Remaining lows (N-C5, N-C6, N-M3, N-S7, L10 leftover) → documented or backlogged.

## Acceptance

Tests per code change; phase 3 end green.

## Result

Code changes have a test that failed on the old code first (after no-op stubs of a
new API where one was needed). Commits `4e063ff` (N-C4), `bc4673f` (protocol and
gateway fixes), `8b3aaf9` (N-M2), `56214c3` (N-S6), `27d0c97` (docs).

- **E12 backend model names**: new `backend_model_name` definition — 1 to 512
  printable ASCII characters (`^[!-~]{1,512}$`), no spaces — for a deployment's
  `model`, the status `deployments` keys and the usage record's `deployment.model`;
  public names unchanged (`model_name` now describes only their shape). Gateway
  `config.IsBackendModelName`. Fixtures: config valid `backend-model-names.json`
  (path id + 512 chars; failed on the old walker), invalid `deployment-model-space`,
  `-513`, `-non-ascii`; usage-record valid `deployment-model-path`, invalid
  `-space`, `-513`; status `deployment-model-name-invalid` now uses a space (`*` is
  valid now). `TestPathStyleBackendModelNames`: the probe matches the path whole,
  a 404 naming it is `upstream_model_missing`, a longer path is relayed. Docs:
  GATEWAY.md Providers, CONTROL-PROTOCOL.md Config, DEPLOYMENT.md (`--alias`).
- **N-P1** `applied_config_epoch` in status (required, null exactly when the
  version is: schema `if/then/else`, gateway walker rule). Gateway `Client.applied`
  is a position (epoch + version). Fixtures: every status fixture carries it; new
  invalid `applied-epoch-missing`, `-without-version`, `applied-version-without-epoch`,
  `applied-epoch-shape`. Sample page compares epoch then version: another store's
  version shows `from another store` (page test with gw-3, another store's v7).
- **N-C4** `fakecontrol`: `Restart`/`closeStreams` and `Stream.Close` remove streams
  from the set under the lock before ending them. The stress command failed on the
  first `-cpu=1` run before; after: `go test -race -cpu=1,2 -count=20 -run
  TestResyncAppliesALowerVersion ./internal/control/` → `ok`.
- **N-S2** `api_key_env` starting with `KAIAK_` refused (schema `not` pattern; gateway
  walker `apiKeyEnv`). Fixture `api-key-env-kaiak-prefix.json` (both halves).
- **N-P5** backend caps are `int64` from the snapshot through routing's share and the
  status (`maxInFlight`, no saturation). `TestHugeMaxInFlightIsKeptWhole` (replaces
  the saturation test); status fixture `ready.json` has a 2^53 − 1 cap.
- **N-P6** schema descriptions: circuit (probe → half-open → trial at its first data
  event), `failure_threshold` (the full failure list; response timeouts from the 3rd
  in a row), `control_outage_grace_ms` (priced USD-limited models; no contact, usage
  waiting, totals for another config), `max_in_flight` and `live_gateways` (the cap
  split), status `max_in_flight` (configured, not the share). **N-P7** the 5xx
  Client API row. **N-P12** ARCHITECTURE (`fastify → config-versions`, heading, e2e
  list, trial wording), DEPLOYMENT (`LIVE_*`/`INIT_CWD` scope note), build script
  ("MB uncompressed" — `docker image inspect .Size` is not compressed), `-w sample`
  comment, the "settings below" reference, CONTROL-PROTOCOL `live_gateways`, the
  circuit failure list; GATEWAY.md spool order (random across old epochs, not by
  age — it said "older epochs first").
- **N-P8** retry budget: `allowRetry` only checks; `retry` counts the retry (and its
  attempt) when sent. `TestRetryBudgetCountsSentRetries` (failed: approval 11 denied)
  and `TestRetryBudgetWindow` updated to send the retries it approves.
- **N-P9** fixtures `first-event-timeout-zero`, `response-timeout-string`,
  `user-label-string`.
- **N-P10** `400 since-invalid` on opening the stream → snapshot, then the stream from
  its position (`positionRefused`); a last-known-good file whose epoch is not 32 hex
  digits or whose version is outside 1..2^53 − 1 is discarded at boot (warn `last-known-good
  config discarded: malformed position`). Tests `TestSinceInvalidFetchesTheSnapshot`
  (failed: no load in 10 s), `TestMalformedLastKnownGoodIsDiscarded` (failed: applied).
- **N-P11** already done in step 1 (`seed_test.go`: status `ready` after a seed boot).
- **N-M2** `window.setBase(now, …)`: a window ahead of the gateway's clock goes back
  when a push names the gateway's own clock window; a push naming another earlier
  window moves nothing. Warn `pushed window ahead of the gateway's clock` for a
  window starting more than 1 min ahead, once per window start.
  `TestEarlierPushedWindowMatchingTheGatewayClockIsTaken` (the reviewer's scenario;
  failed: hour used 0 instead of 800, no warning).
- **Trial first event = data event** (main session): the relay decides the trial at
  a stream's first event carrying data, or a body's first bytes. fakebackend
  `Reply.PingFirst`. `TestTrialIsDecidedAtItsFirstDataEvent` (failed: "circuit closed
  by the trial's comment block"). Step 5's decision note updated.
- **Half-open not reported as open** (main session): `routing.Router.Circuits()`
  (state + opening time) replaces `OpenCircuits`; `kaiak_circuit_open` 1 only while
  open; new `kaiak_circuit_half_open{backend,deployment_model}`, pre-created at 0.
  Status enum gains `half_open` (both halves, `opened_at` kept): cheap — schema,
  gateway walker and type, kit type, page (`circuit half-open` warn tag, summary
  count). Tests: server `TestOpenDeploymentIsSkippedAndProbedBackIn` (failed: metric
  lines), `TestServingStatusCoversTheAppliedConfig` (half-open deployment), e2e
  `TestRetryOnAnotherDeploymentAndCircuit` (half-open gauge 1, open 0), status
  fixtures `ready.json` (half-open) and `circuit-half-open-without-opened-at`. DEPLOYMENT
  "Circuit open" alert text: half-open reads 0, so an idle recovered deployment no
  longer pages.
- **N-S6** sample page: at most `maxStreams` (default 32) page streams; one more is
  `503 page-streams-full`, `Retry-After: 10`, a warn line once per flood. Test
  "page streams past the cap are refused with 503 until one ends".
- **Remaining lows** → `docs/BACKLOG.md` "Gateway edge cases": N-C5 body budget per
  model, N-C6 live count during an outage, N-M3 old-epoch spool order, N-S7 slow
  readers hold slots, L10 removed backends' pools — each with a revisit trigger.
- **Suite**: `GOFLAGS=-count=1 scripts/check-all.sh` → `all checks passed` (gateway
  gofmt/vet/staticcheck/race tests incl. e2e, live-kit self-test for vllm, openai,
  azure-openai, vllm with two backends; control 462 tests + lint; cross-half e2e
  `ok kaiak/e2e 43.4s`). Stress command green (above).

## Decisions made

- `backend_model_name` is printable ASCII only: byte and character counts agree in
  both halves (JSON Schema `maxLength` counts characters), and names stay plain in
  logs and metric labels. Non-ASCII or spaced names need a backend alias.
- `half_open` added to the status enum rather than kept as `open` — a few lines per
  half; the page shows it apart from open.
- N-P10: only `400 since-invalid` triggers the snapshot fetch; other stream refusals
  keep the reconnect backoff.
- N-M2: the window goes back only to exactly the gateway's clock window; the warning
  has a 1-minute margin so ordinary skew at a boundary does not log every hour.
- N-P8: a sent retry counts as an attempt and a retry, as before (the ratio is
  retries over all attempts).
- N-S6: default cap 32, `Retry-After: 10`, error code `page-streams-full`.
- No "half-open under traffic" alert added (not needed yet).

## Decisions for the user to confirm

1. Backend model names: 1–512 printable ASCII, no spaces (not Unicode).
2. Status `circuit` gains `half_open` (protocol change on both halves).
3. N-M2 rule: move back only to the gateway's own clock window; warn only past a
   1-minute lead, once per window start.
4. N-S6 cap 32 page streams, 503 with `Retry-After: 10`.
5. N-S2 without the optional "https required for Azure" rule.
