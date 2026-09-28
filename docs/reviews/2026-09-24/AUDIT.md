# Audit — 2026-09-24/25 (at 0.3.0), merged

Two reviews of the same tree (commit `1159d12` content), merged here:

- **[A]** five read-only reviews by this project's agent (security; billing and
  limits; concurrency and lifecycle; the contract between the halves; operability),
  key findings re-checked against the code;
- **[B]** an independent audit by a different agent on a plain copy with no git and no
  review in it — kept verbatim in `AUDIT-independent.md` (its probe sources and logs
  stayed outside the repo, in `/tmp/kaiak-audit/`).

Every [B]-only finding marked *re-checked* was confirmed against this tree before
merging. Both reviews found the full suite green (race detector clean, no leaks). The
target is the user's first real deployment: ~20 vLLM hosts plus Azure OpenAI, many
keys/teams, several gateway replicas, Kubernetes.

Severity: **high** — money or availability wrong in normal operation at this scale;
**medium** — wrong under realistic stress, misuse or operator error; **low** — hygiene,
edge cases, docs. "Decision" = the fix changes a settled spec choice.

## High

- **H1 Upstream stall after the first event** [A][B]. `provider/openai.go:50-100` stops
  the only timer at the first event; a hung vLLM (GPU/engine hang, TCP alive) holds its
  slot until the client leaves, never reports an outcome, its circuit never opens.
- **H2 Client-side progress is unbounded, including before authentication** [A][B].
  `server/listener.go:17,43-48`: only a header deadline — no `IdleTimeout`, no body-read
  deadline, no write-progress deadline. [B] reproduced: headers with `Content-Length: 1`
  and no body, no key → the connection blocks forever (Go drains the small body while
  writing the 401). Also: idle keep-alive never reaped; a slow uploader holds memory
  before limits; a client that stops reading holds a backend slot (`upstream.go:380-429`).
- **H3 Totals for a config the gateway rejected erase a spent budget** [B, re-checked].
  `cmd/kaiak/main.go:481` drops `config_version`; the limiter takes the complete window
  list, zeroes a retained limit whose identity is absent and retires acked local usage.
  A gateway that rejects v2 (e.g. a missing credential env var) keeps enforcing v1
  identities against v2 totals — a spent $ budget resets on every ack, all month.
- **H4 A record that fills a batch is double-counted locally** [B, re-checked].
  `accounting.go:112` publishes to the sender (which seals synchronously at 500
  records, `control/usage.go:130-160`) before the limits finisher settles the record
  (`server/limits.go:45`), so the settlement lands in the next generation and survives
  the ack: usage counted twice, the owner falsely refused until another batch clears it.
  Interval seals racing settlement do the same.
- **H5 One out-of-range usage record drops a whole batch from billing** [A]. Units are
  any int64 (`meter.go:228-249`), cost saturates at int64 (`accounting.go:157`); the
  protocol caps at 2^53−1 → `usage-batch-invalid` → up to 500 records set aside
  (`control/usage.go:53-61`).
- **H6 Work ended before the first event by the client or the drain bills zero** [A][B
  as limitation]. (Decision — agreed D1.) `upstream.go:128-133` → `meter.go:167-169`.
- **H7 One first-byte timeout serves two purposes** [A]. (Decision — agreed D2.) Long
  non-stream generations time out, are retried on 2 more hosts and count as circuit
  failures; `examples/config.json` hits it.
- **H8 A host serving the wrong model attracts traffic** [A]. vLLM's 404 is neutral (no
  retry, no circuit) and fails fast, so least-in-flight prefers it; the probe ignores
  the `/models` body.
- **H9 Usage-metric cardinality** [A][B]. `metrics/usage.go:10` includes `backend`;
  ~240k series per replica at 500 keys × 20 backends; `key_id_label` barely helps.
- **H10 Multi-output requests bypass reservations** [A][B, raised]. `n` (and completion
  prompt arrays, `best_of`) multiply generation; the reservation counts one output
  limit (`server/limits.go:25`, `params.go`). [B] reproduced `n=64` reserving 100.
- **H11 Opening the config stream can hang forever** [A][B, raised]. `control/
  stream.go:41-54` before the idle timer, default client without header timeout; a stuck
  proxy freezes config updates and key revocations indefinitely while acks keep the
  gateway out of outage.
- **H12 No control plane fit for real budgets; single-process assumption unstated**
  [A][B]. (Decision — agreed D7.) In-memory store only; [B] reproduced two
  `createUsage` instances on one store counting the same batch twice (de-dup read and
  atomic write are separate; serialization is per process); totals `revision` ordering
  is per process too.

## Medium

- **M1 `max_in_flight` is per gateway replica** [A][B]. (Decision — D3, pending.)
- **M2 Memory has only per-request bounds** [A]. Bodies read before limits, copied per
  attempt, held while queued and streaming.
- **M3 Queue dispatch scans every waiter under the router lock** [A]: 446 µs per release
  at 5 models × 20 deployments × queue 500.
- **M4 No per-backend ops metrics** [A].
- **M5 A restarted gateway enforces hour/month from zero until first totals** [A][B].
- **M6 Outage turns tokens/hour into "since the outage began"** [A]. (Decision — agreed D4.)
- **M7 Per-minute token shares can make ordinary requests impossible** [A].
- **M8 Editing a budget's model list forgives the month's spend** [A]. (Agreed D5.)
- **M9 The outage policy refuses free models** [A]. (Agreed D6.)
- **M10 A restarted control plane can reuse the gateway's version number** [A].
- **M11 Data directory under Kubernetes** [A]: no lock, emptyDir loses the spool, no
  cold boot during an outage.
- **M12 No packaging or operator guide** [A][B]. (D8 — Dockerfile + build check agreed.)
- **M13 A clean EOF mid-stream counts as a complete answer** [B, re-checked by
  reading]. The relay ends successfully on HTTP EOF without a terminal chunk or
  `[DONE]`: `partial=false`, circuit success, estimated usage.
- **M14 Userinfo credentials in `base_url` are accepted and shown on the sample page**
  [A low][B raised]. Both validators accept `user:pass@`; the page prints it.
- **M15 The sample watcher misses projected ConfigMap updates** [B]. The `..data` symlink
  swap is filtered out (`control/sample/src/config-file/index.ts:132`).
- **M16 Partial control-plane failure** [B]. A live stream counts as contact while
  `/usage` keeps failing, so the outage policy never engages and each replica enforces
  only its own view; monitor ack age / spool depth, and consider ack age in "contact"
  for money limits.

## Low

- L1 Retries amplify global overload; Azure `retry-after-ms` not relayed; immediate
  same-deployment retry [A].
- L2 `/metrics` unauthenticated on all interfaces, exposing org chart and spend [A].
- L3 Unbounded client strings in logs/errors [A].
- L4 Shared-token trust model undocumented; `lastBatches` never pruned; 1 MiB `/status`
  body [A].
- L5 Backend error bodies relayed may name internals [A].
- L6 Config integers ≥ 2^63 [A]. L7 `defaults` number fidelity across halves [A].
- L8 Spool order across old epochs [A]. L9 unreachable header branches; Fastify-level
  answers lack `Kaiak-Protocol` [A]. L10 small unpruned maps [A]. L11 output default
  ignores prompt length [A]. L12 no half-open circuit [A]. L13 shares vs Kubernetes
  Services (backlog) [A][B]. L14 fixture gaps [A].
- L15 Duplicate `kaiak_circuit_open` samples when two public models share a
  deployment [B] (`metrics/ops.go:158`).
- L16 Sustained spool write failure keeps sealed records in memory without bound [B]
  (`control/spool.go:216`).
- L17 `docs/TECH-STACK.md:51` still says the spool is append-only JSON lines [B].
- L18 `kaiak-control` resolves schemas relative to the repo — an extracted package or
  container must carry them [A][B].

## Operational (not code)

- Live Azure check before go-live [A][B]; config repetition, a backend `disabled` flag,
  sample page fleet roll-up [A].

## Agreement between the reviews

Found by both: H1, H2, H9, H10, H11, H12, M1, M5, M12, M14, L13, L18 and the Azure check.
Only [B]: H3, H4 (both re-checked, both real and high), M13, M15, M16, L15–L17. Only [A]:
H5, H7, H8, M2–M4, M6–M11, most lows. No finding of one review was contradicted by the
other; [B] rated H10, H11 and M14 higher than [A] did, and this merge takes the higher
rating.

## Checked and fine (both)

Auth and model access, header allowlists, JSON body splicing, secrets never logged,
state-file permissions and atomic writes, constant-time control token, sample page
escaping + CSP, retries × limits (one reservation and request count per client
request, settlement sums records), usage extraction and cost arithmetic, spool
durability ordering and stable resend IDs, single-process batch de-dup, lock ordering,
goroutine ownership, drain ordering, cross-half regex/number/semantic-rule parity.
