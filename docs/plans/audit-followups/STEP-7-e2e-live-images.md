# Step 7 — e2e, live, images

**Status:** implementation done (2026-09-25) — suite 3× green; **pending (main session):** the live GPU-host run, images pushed

## Items

- e2e for stateless boot (control plane down + seed; down without seed → exit), the
  per-key limit, the drain reserve, `max_tokens` above context.
- Live kit: `-embeddings-base-url` (a separate embeddings server), run on the GPU host two
  vLLM copies + `embed-host:11435` (`qwen3-embedding-0.6b`), with failover.
- Images rebuilt, smoke-tested read-only with no volume, pushed.

## Acceptance

`scripts/check-all.sh` 3× green; live kit green incl. embeddings; images pushed. Plan end.

## Result

Commits `c8b37cd` (e2e), `249e603` (live kit), `678e97b` (build script size line).

- **e2e — already covered, checked, not duplicated**: stateless boot
  (`stateless_test.go`: `TestMinimalControlPlaneSetup`, `TestNoConfigAtBootExits`,
  `TestSeedServesWithTheControlPlaneDown`, `TestPricedSeedFailsTheStart`) and the
  drain reserve (`TestDrainReserveDeliversTheCutRequestsUsage`), all step 1.
- **e2e — added** (`gateway/e2e/keylimits_test.go`):
  - `TestPerKeyConcurrencyLimit`: 16 streams from one key held open by the fake
    backend → the 17th gets `429 concurrency_limit_exceeded`, `Retry-After: 1`,
    never reaching the backend; another key is served 200; after one held stream
    ends (its request log line settled) the key is served again. Red check: with
    `max_concurrent_requests_per_key: 17` in the config it fails ("request 17 from
    one key: 200").
  - `TestOutputLimitAboveTheContextIsRefused`: `max_tokens` and
    `max_completion_tokens` 8193 on a 8192-context model → `400 invalid_value`
    with `param` naming the key, nothing sent to the backend; 8192 is accepted and
    lowered to the ceiling (128).
  - Both `-race -count=5`: `ok kaiak/e2e 12.6s`.
- **Live kit** (`scripts/live`): `-embeddings-base-url` (env
  `LIVE_EMBEDDINGS_BASE_URL`) with `-embeddings-model` adds a third backend
  `live-embeddings` (openai-compatible whatever `-kind`; no key unless
  `-embeddings-api-key-env`); `live-embed` deploys there under its public name
  whatever the backend-side name (path-style allowed). `-embeddings-model` alone
  still uses `-base-url`. Flag errors: base URL without model, key env without base
  URL, key env unset. The `embeddings` check also requires the log line's backend
  to be `live-embeddings` in that mode. Self-test: the two-backend run adds a third
  fake (bearer key via `-embeddings-api-key-env`, lists
  `/models/fake-embed-q8_0.gguf`): `PASS embeddings 3 dimensions, 7 tokens, served
  by live-embeddings`, 16/16; no "does not list" warning in the gateway log.
  `LIVE-BACKENDS.md`: new section "Embeddings on a server of their own" with the GPU host
  command shape (two vLLM chat copies on 8001/8002 + vLLM embeddings launcher entry
  `qwen3-embed` on 8003 as `qwen3-embedding-0.6b`, or llama-server
  `http://embed-host:11435/v1` with `/models/qwen3-embedding-0.6b-q8_0.gguf`).
- **Images** (commit `249e603`, version `0.3.1-249e603`, context `dev`):
  `scripts/build-images.sh` then `scripts/smoke-images.sh` → both gateways
  `--read-only` with no volume (file mode mounts only the config, read-only),
  `kaiak_build_info{version="0.3.1-249e603",go_version="go1.27.1"} 1`, both chats
  200, `gw-file: exit 0, logged "kaiak stopped"`, `gw-control: exit 0, logged "usage
  flushed"`, `smoke passed`. Image config: `User 65532:65532`, `Volumes null`, no
  `KAIAK_DATA_DIR`. Sizes: **kaiak 18.9 MB** unpacked (4.5 MB content),
  **kaiak-sample 373 MB** unpacked (87.7 MB content). The build script printed "4
  MB uncompressed" for the gateway: on the containerd image store `image inspect
  .Size` is the compressed content — fixed to print `image ls`'s unpacked size.
  Built images removed from `dev`; the smoke's containers, network, volume and
  fake-backend image removed by its own cleanup (buildx cache left as is).
- **Suite**: `GOFLAGS=-count=1 scripts/check-all.sh` three times in a row, each
  `all checks passed` (gateway race tests incl. e2e `ok kaiak/e2e` 72.6/72.3/68.6 s;
  live-kit self-test for vllm, openai, azure-openai, vllm with two backends; control
  462 tests + lint; cross-half e2e `ok kaiak/e2e` 58.4/53.3/38.4 s).
- **Pending for the main session**: the live GPU-host run (kit incl. embeddings, with
  failover) and `scripts/build-images.sh --push` from the final commit.

## Live GPU-host run (main session, 2026-09-25)

Two vLLM copies of `Qwen/Qwen3.5-2B` (`qwen3.5-2b`, ports 8001/8002, launcher entry `qwen35-2b-a/-b`)
with `-max-in-flight 4`:

- **+ vLLM embeddings** (launcher entry `qwen3-embed`, `Qwen/Qwen3-Embedding-0.6B` as
  `qwen3-embedding-0.6b`, port 8003) with `-check-failover`: **16 passed, 0 failed, 0
  skipped** — embeddings 1024 dimensions served by `live-embeddings`, spread 3/3,
  capacity 10 over 2×4 all 200 (2 queued), output ceiling (32768 → 16), rate limit,
  failover: circuit opened after 7.4 s, half-open by a probe after the restart, closed by
  its trial, traffic back.
- **+ llama-server embeddings** (`http://embed-host:11435/v1`, path-style model
  `/models/qwen3-embedding-0.6b-q8_0.gguf`): **15 passed, 0 failed, 1 skipped**
  (failover, not requested) — no "does not list" warning from the model check.

The GPU host was restored to its regular model afterwards.
