# Step 5 — a wrong path is the deployment's failure

**Status:** done (2026-10-01)

## Intent

A backend whose `base_url` has the wrong path (the review's O1: no `/v1`) stops
failing silently: its "no such path" `404` is the deployment's failure, visible in
the circuit, retries and alerts, and the config-apply check warns about it.

## Files likely touched

- `docs/specs/GATEWAY.md`:
  - Wrong model on a host, or a new sibling decision (settled on the step's date):
    a wrong path is the deployment's failure; the per-module signatures; the
    catch-all for `openai-compatible`;
  - the error table (`upstream_path_missing`, `502`, `server_error`), the outcome
    and retry tables, and the metric label values;
  - the config-apply background check: a models list answering `404` is a warning
    naming `base_url`.
- `gateway/internal/provider/`: each module names its server's unknown-path answer
  beside its missing-model codes; `wire.go` classifies a matching `404` as the new
  error, as it does a missing model.
- `gateway/internal/routing` / `server`: the outcome class, retry reason and error
  code wiring, following `model_missing`.
- The background model check (wherever `model check skipped` is logged).
- `docs/DEPLOYMENT.md`: the starter alert that covers `model_missing` covers this too.

## Decisions made during planning

- **Signatures to confirm in the step**, each against the real server where at hand
  (llama-server locally, vLLM on the DGX via the live-test kit) and against the
  provider's documentation otherwise:
  - `vllm`: FastAPI's `{"detail": "Not Found"}`;
  - `llama-server`: its `not_found_error` answer;
  - `openai`: `invalid_request_error` whose message starts `Invalid URL`;
  - `azure-openai`: the resource's `404` "Resource not found";
  - `openai-compatible`: a `404` whose body is not an OpenAI-shaped error
    (`{"error": {...}}` or `{"error": "<text>"}`) — plain text included.
- A signature not recognized leaves the `404` the caller's, relayed as today.
- Metric label and retry reason: reuse the missing-model pattern with its own value
  (`path_missing`) rather than folding it into `model_missing`, so an operator can
  tell a wrong URL from a wrong model.

## Acceptance criteria

- Per module: its unknown-path answer gives `502 upstream_path_missing`, a retry on
  another deployment and a circuit failure, with no usage record; a missing-model
  `404` and an ordinary caller's `404` behave exactly as before.
- The config-apply check logs a warning naming the backend and `base_url` for a
  models list answering `404`; an unreachable backend is still an info line.
- **Phase end:** `scripts/check-all.sh` green, output recorded. Live (opt-in):
  `-kind llama-server` and `-kind vllm` on the DGX with a `base_url` missing `/v1`
  show the new code and the warning.

## Result

- **Provider.** `wire.go`: a `404` is read once (`readNotFound`, the first 64 KiB put
  back in front of the body); a missing model is read first (`modelMissing`, now over
  the bytes read), then the module's `unknownPath(answer)` — a match is
  `CodePathMissing` (`upstream_path_missing`; the error names the backend and the URL,
  never the backend's text), the answer closed, not relayed. `readErrorAnswer` reads
  the OpenAI error shape (`{"error": {...}}`, `{"error": "<text>"}`) for the modules.
  `fetchModelsList` turns a `404` models list into a `*PathMissingError` carrying the
  module's hint (`BaseURLHint()`). Each module names its signature beside its
  missing-model codes and its hint: `versionPathHint` ("base_url should end in the
  API version path, e.g. /v1") for all but `azure-openai`, whose hint says the
  base_url is the resource endpoint with no path (the gateway adds `/openai/v1`).
- **Server.** `upstream.go`: retry reason `path_missing`; attempt outcome
  `path_missing`, a circuit failure; the meter is told the backend refused (no
  usage). `errors.go`: `502 server_error upstream_path_missing`, "The model backend's
  address is misconfigured." `metrics.go`: the code is class `upstream_error`.
  `metrics/ops.go`: `AttemptPathMissing` and the `path_missing` retry reason, both
  present at 0 from the start (thirteen attempt outcomes).
- **Routing.** `modelcheck.go`: a probe error with a `BaseURLHint` (a `404` models
  list) logs WARN `the backend has no models list at its base_url` with `backend`,
  `base_url`, `hint`; any other probe error stays the INFO `model check skipped`
  line. Routing reads the error through a small interface (`pathMissing`), so it
  does not import `provider`.
- **Docs.** `GATEWAY.md`: Wrong path to a host (settled 2026-10-01) beside Wrong
  model on a host — the rule, the five signatures with their evidence, the
  openai-compatible catch-all, the config-apply warning and hint, what was rejected;
  the error table, retry and outcome-class tables, the circuit's failure list, the
  `kaiak_retries_total` reasons, the attempt outcomes ("thirteen series") and the
  `upstream_error` class. `DEPLOYMENT.md`: the starter alert is now "Wrong model,
  path or credential", `outcome=~"model_missing|path_missing|auth_failed"`.
  `LIVE-BACKENDS.md` (Reading failures): `upstream_path_missing` and
  `upstream_model_missing`; the relayed-`404` bullet no longer says a missing `/v1`
  or a wrong model is relayed. `BACKLOG.md`: llama-server quirks gains the `/v1`
  finding below (with a revisit trigger).
- **Tests.** `provider/unknownpath_test.go`: `TestUnknownPathByModule` — every
  module against every server's unknown-path answer plus Go's, Express's and nginx's
  pages and an empty body: its own (and, for `openai-compatible`, every non-OpenAI
  shape) is `upstream_path_missing` naming the URL, the rest relayed whole; a caller's
  `404` relayed; a missing model in older vLLM's top-level shape still
  `upstream_model_missing` (read first). `TestProbeOfAMissingModelsList` — per module
  a `404` list is a `PathMissingError` with the module's hint, a `503` is not.
  `server/pathmissing_test.go`: `TestUnknownPathIsTheDeploymentsFailure` — per module
  (backend `local` retyped): retried on `local-b` (`tried` …`:upstream_path_missing`),
  circuit open at threshold 1, one record (the second deployment's),
  `kaiak_retries_total{reason="path_missing"}` and the `path_missing` attempt; with
  no deployment left `502 upstream_path_missing`, no backend text, one record with
  zero units (every routed request's record — as for `model_missing`), class
  `upstream_error`. Rows added to `TestOutcomeClassification` and
  `TestRetrySucceedsOnTheOtherDeployment` (a Go `404 page not found` on
  `openai-compatible`). `TestUnknownPathRefusesTheBackendForTheRequest`: model
  `retry` with `local/first`, `local/sibling` and `local-b/second`, `local` answering
  a wrong path — two attempts, `local` got one request, the retry went to `local-b`. `routing`: `TestModelCheckWarnsPerMissingModel` — the WARN
  line with `base_url` and hint; the unreachable backend still INFO. `e2e`:
  `TestWrongBaseURLIsWarnedAtApply` (a `vllm` backend at the fake's root: the real
  binary warns once, naming it); `TestSeriesStartAtZero` lists `path_missing`.
  `TestModelMissingAnswerIsAnError`'s relayed `not json` became `{"error": "adapter
  foo not found"}`: a non-OpenAI-shaped `404` on `openai-compatible` is now a wrong
  path by contract (covered in `TestUnknownPathByModule`).

Signature evidence (2026-10-01):

- `llama-server` — **run**: Homebrew `llama-server` (version 0.5.0, build 11146,
  commit 7fe450e19) on 127.0.0.1 with Qwen3-Embedding-0.6B-Q8_0 and `--embeddings`.
  `POST /v1/nope`, `/nope`, `/v2/chat/completions`, `/v1/v1/chat/completions`,
  `/api/v1/chat/completions` and `GET /v1/v1/models` all answered `404`,
  `application/json; charset=utf-8`,
  `{"error":{"message":"File Not Found","type":"not_found_error","code":404}}`.
  Source (`tools/server/server-http.cpp`, `set_error_handler`; cpp-httplib calls it
  for every status ≥ 400) gives every `404` that body — router mode's "model is not
  found" included, which is why the spec says a missing model there reads as a
  wrong path. **But `POST /chat/completions` without `/v1` answered `200`**: the
  server registers chat completions, `/models`, `/completions` and `/embeddings`
  without the prefix too (`server.cpp` routes) — the last two in its own formats
  (`{"index":0,"content":…}`, `[{"index":0,"embedding":[[…]]}]`). A missing `/v1` is
  therefore no `404` on llama-server (backlogged, llama-server quirks).
- `vllm` — **source, not run** (no vLLM here): vLLM `main` (latest release v0.30.0)
  registers its HTTP handler for `fastapi.HTTPException`
  (`vllm/entrypoints/serve/exception_handling/register.py`, `handlers/http.py`); the
  router's 404 for an unknown route is Starlette's `HTTPException`, a superclass,
  which that handler does not match, so FastAPI's default handler answers
  `{"detail":"Not Found"}`. vLLM's own 404s (a missing model, an unknown adapter)
  carry the OpenAI shape.
- `openai` — **known behaviour, unverified live**: `404`
  `{"error":{"message":"Invalid URL (POST /chat/completions)","type":"invalid_request_error","param":null,"code":null}}`.
- `azure-openai` — **known behaviour, unverified live**: `404`
  `{"error":{"code":"404","message":"Resource not found"}}`; matched on the message
  (the code's type varies).

Decisions made during the step:

- **Precedence**: a missing model is read before the path signature, so a
  missing-model `404` behaves exactly as before on every module.
- **Older vLLM's top-level `{"message": …}` is not OpenAI-shaped** for the
  `openai-compatible` catch-all (the plan's two shapes only): a proxy's
  `{"message": "no Route matched"}` is exactly a wrong path, and vLLM has its own
  type. A missing model in that shape is still read first.
- **Backend-wide refusal, as `auth_failed`** (coordinator's follow-up, 2026-10-01):
  after a wrong path, every deployment of the model on that backend is refused for
  the request's retries (`avoidAfter`) — `base_url` belongs to the backend, so a
  sibling deployment there would hit the same wrong path and waste the attempt. A
  missing model stays per deployment.
- **The config-apply warning** fires on any `404` models list (no signature needed:
  the list's path is the module's own), names `base_url` and a per-module hint; the
  hint lives in the module (`azurePathHint`, `versionPathHint`) since only the module
  knows its URL layout. Routing reads it through an interface, keeping
  `routing → provider` free of an import.
- **Client message** "The model backend's address is misconfigured." — the platform's
  problem, no backend named.
- **No record-less request**: "no usage" is a zero-unit record for the request, as
  for `model_missing` (every routed request is recorded), and no record of its own
  for a retried attempt.

Mutation checks (each reverted; build and the targeted tests green after):

- Each module's signature broken (`"Not Found"`, `"File Not Found"`,
  `"Invalid URL"`, `"Resource not found"` changed): that module's
  `TestUnknownPathByModule` case and both `TestUnknownPathIsTheDeploymentsFailure`
  cases fail, no other module's.
- `openai-compatible` catch-all off: its provider and server cases plus the new
  `TestOutcomeClassification` and `TestRetrySucceedsOnTheOtherDeployment` rows fail;
  catch-all reading every `404`: `TestUnknownPathByModule/openai-compatible`,
  `TestModelMissingAnswerIsAnError`, `TestMissingModelCodesArePerModule`,
  `TestPathStyleBackendModelNames` fail.
- Path read before the missing model: `TestModelMissingAnswerIsAnError`,
  `TestUnknownPathByModule` fail.
- Not retried / neutral for the circuit / meter not told it was refused / client
  answer falling to `upstream_unavailable`: `TestUnknownPathIsTheDeploymentsFailure`
  fails each time (plus `TestRetrySucceedsOnTheOtherDeployment`, resp.
  `TestOutcomeClassification`).
- Warning at INFO: `TestModelCheckWarnsPerMissingModel` fails. Probe `404` as a plain
  error, or Azure given the version-path hint: `TestProbeOfAMissingModelsList` fails.
- `avoidAfter` without the `retryPathMissing` branch:
  `TestUnknownPathRefusesTheBackendForTheRequest` fails (three attempts,
  `local/sibling` tried).
- `PathMissingError`'s `BaseURLHint` renamed (the provider–routing coupling):
  `e2e TestWrongBaseURLIsWarnedAtApply` fails (no warning within 15 s).

Live (opt-in) on the DGX (`-kind llama-server`, `-kind vllm` with a `base_url`
missing `/v1`): **not run in this step**. Expect `vllm` to show
`upstream_path_missing` and the warning; `llama-server` without `/v1` will not (see
above) — use a wrong path such as `/v2` for it.

Suite: `scripts/check-all.sh`, 2026-10-01, after `go clean -testcache` (every
package ran fresh), run again after the backend-wide refusal: **exit 0, "all checks
passed"**. gofmt, vet, staticcheck clean (gateway and kit); `go test -race ./...`
every package ok (`e2e` 93.0 s, `internal/server` 12.3 s); kit self-test passed for
vllm, llama-server, openai, azure-openai and vllm with two backends; control
`npm test` 565 passed, 0 failed; `npm run lint` clean, boundaries ok; cross-half e2e
ok (63.9 s). Phase 2 ends green; no expected reds.
