# Follow-up audit — decisions and fix sketch

Findings in `AUDIT.md` (this folder). Decisions by the user, 2026-09-25; items marked
*proposed* awaited confirmation (all confirmed 2026-09-25). Work: `docs/plans/audit-followups/`.

## Decisions

- **E1 Stateless gateway (proposed shape).** Pods read-only, replicas, no persistent
  volume and no scratch directory. In control-plane mode the gateway writes nothing to
  disk: usage batches pending delivery live in memory (existing 10 000-record bound,
  oldest dropped with log + metric), no last-known-good, totals in memory. The disk
  features stay as an opt-in (`KAIAK_DATA_DIR` set) — confirmed; file mode keeps its
  optional limits snapshot the same way.
- **E2 Boot order:** control-plane config (bounded wait) → the **seed config** → refuse
  to start (exit with a clear error; Kubernetes restarts with backoff). The seed is the
  ultimate backup: free local models, used whenever the control plane is unavailable
  at boot — including a control plane that answers without a config (`503`), never on
  a rejected token. A seed containing a priced model is a startup error — confirmed.
  Seed credentials checked at startup (N-P4).
- **E3 Drain reserves time for the usage flush** (last ~10 s of the drain timeout);
  batches keep going out every 5 s throughout the drain.
- **E4 Half-open trial decided at the first event** (N-C1/N-S3).
- **E5 Every metric series pre-created at 0 at startup** (all counters with known label
  sets: config loads, errors, attempts per deployment × outcome, queue rejections,
  circuit transitions, retries, …) (N-O4).
- **E6 Retry pause keeps holding the slot** (confirmed).
- **E7 `n`/`best_of` stay refused as model defaults** (confirmed).
- **E8 Only data events reset the stall timer** (SSE comments do not); silent-reasoning
  backends raise their `stall_timeout_ms`.
- **E9 `max_tokens`/`max_completion_tokens` above the model's `context_length` → 400**;
  limit arithmetic overflow-safe (N-M1).
- **E10 Per-key concurrent-request limit, default 16, per gateway** (counted from
  authentication, before the body is read, to the end of the request); body memory
  taken as bytes arrive, not from the declared length (N-S1). Split across gateways and
  temporary key suspension → `docs/BACKLOG.md`.
- **E11 Control plane durability out of scope** of this audit (gateway only).
- **E12 llama-server naming**: backend-side model names accept the backend's own naming
  (e.g. llama-server's path-style ids); docs mention `--alias`. Live tests gain a
  separate embeddings server (`embed-host:11435`, `qwen3-embedding-0.6b`) and a
  vLLM embeddings entry on the GPU host (launcher entry).
- All other "decisions for the user to confirm" in `docs/plans/audit-fixes/STEP-*.md`:
  confirmed as implemented, except where changed above (step 5 #3 → E8; step 7 #6 →
  E2; step 10 #6 → E3; step 8 #2 → E5; step 6 #6 → E4).

## Independent audit on 0.4.0 (2026-09-25) — `AUDIT-independent.md`

Seven findings, all reproduced against `0.4.0` by running the auditor's own tests in a
scratch worktree (numbers match the report). Handling agreed with the user 2026-09-25:

- **F1 (high) duplicate object members in config** bypass the schema walker (the typed
  decoder merges duplicates the generic tree dropped) — incl. the `KAIAK_` guard.
  *Agreed:* the gateway rejects duplicate object members in every document it
  parses (config, snapshot, messages) — new rejection code; the `KAIAK_` check also at
  the credential-use boundary.
- **F2 (high) duplicate owned request fields** expand the rewritten body (43.6× measured)
  outside the body budget. *Agreed:* a request body with a duplicate top-level member
  → `400`; the rewritten size is then bounded by one edit per owned field.
- **F3 (medium) error status lost when the first body read fails** → billed as
  unanswered, retried as unavailable. *Agreed:* keep the status from the headers for
  accounting and the retry decision.
- **F4 (medium) completion batches bypass capacity** (10 000 prompts in one slot).
  *Agreed:* a cap on generated sequences per request (n × best_of × prompts; default
  16) and on embeddings inputs per request (default 2048, OpenAI's limit); slots stay
  per request, documented.
- **F5 (medium) a delayed message from a replaced control-plane process** is accepted as
  "another restart" (A→B→A). *Agreed:* the gateway remembers the control-plane
  process IDs it has moved away from and ignores their totals (acks still retire their
  batches). One control plane restarting is the case, not several at once; the config
  version/epoch cannot order them (both processes serve the same config).
- **F6 (medium) a reload removing prices between a request's snapshot and admission**
  skips USD enforcement. *Agreed (simplest alignment):* admission decides billability from the
  request's own snapshot — the same one billing prices from. Treated as an edge case.
- **F7 (medium) a forward-then-corrected window** revives an old hold → counter
  saturates, all requests refused. *Agreed:* window incarnation IDs in holds; negative
  states detected.
- **Deployment notes:** the 10 000-record in-memory usage bound is ~100 s at 100
  records/s — far shorter than the 15-min outage grace. *Agreed:* size it by memory
  (configurable, default e.g. 64 MiB ≈ 200 000 records) and document the rate it covers.
  Pre-authentication connections have no aggregate cap — *proposed:* document ingress
  limits, optional `KAIAK_MAX_CONNECTIONS`.

## Daily-operations review (2026-09-25) — `AUDIT-daily-ops.md`

Agreed with the user:
- **D1** inline media (image/audio/file data) estimated at a flat 1000 tokens each; the
  surrounding text by bytes. **D2** default output fitted per prompt (largest prompt).
- **D3** a deployment that answered 429 cools down for `min(Retry-After, 60 s)` (a short
  default without the header): not eligible while another deployment of the model is.
- **D4** failover-only retries: never retry on the same deployment (the client/SDK
  retries); trying another deployment of the model is failover, bounded by the
  deployments and `max_attempts`; the same-deployment pause goes away.
- **D5** (proposed) per-key concurrency limit off by default (optional global setting);
  fair dispatch across owners → backlog. Rationale: body memory is taken as bytes
  arrive, so the cap no longer guards memory; per-key usage is bounded by rate limits
  and budgets.
- **D6** log line: limit scope/type/limit/configured/used and owner on limit refusals;
  `error_code` + upstream code/type on relayed backend errors; TTFT from the answering
  attempt; attempt metrics published when each attempt ends.
- **D7** boot retries the control plane with backoff for the boot wait (default 60 s),
  then the seed, then exit. **D8** readiness waits for the first totals (normally ms);
  past the boot wait without them, priced USD-limited requests are refused until they
  arrive.
- **D9** docs and alerts as listed.
- Backlog: rollout share shrink (draining gateways in the live count); fair dispatch.
