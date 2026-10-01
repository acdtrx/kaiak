# Whole-app review — 2026-09-30 (at v0.8.0)

Scope: the whole app at `v0.8.0` (`6fbf8cb`), with extra attention on what changed
since the last review at 0.7.0 (service tier, per-backend-type modules over the
OpenAI wire core, tiered prices, `verifyBackend` and the sample `verify` command).
Six read-only reviewers, each on its own detached worktree at `v0.8.0`:

- **[G]** gateway correctness (pipeline, backend modules, streaming, pricing, control
  client, drain; chaos test and differential fuzzing);
- **[K]** `kaiak-control` and the sample;
- **[P]** contract parity between the halves (differential testing);
- **[S]** security and abuse (reproduced against a real local llama-server);
- **[T]** test quality (mutation testing, ~180 mutations);
- **[O]** daily operations (the docs followed as written, a real run with fake
  localhost backends).

An independent review of a plain copy joins this file as **[B]** (Independent review,
below; its report is `AUDIT-independent.md`).

Every finding is tagged by frequency under legitimate use: `daily` / `occasional` /
`rare` / `adversarial`. The main session re-checked the key claims against the code:
the missing `n_predict`/`n_cmpl` handling, the unguarded redirect policy of the
control client, the 1 MiB probe read, the relay reading past `[DONE]` to EOF, the
uncaught carry-over callback, the unbounded publish size, the price picked by request
start, and the README's `docker run` lines.

## Verdict

**The core holds.** `scripts/check-all.sh` is green. A chaos run under the race
detector (random backend failures, client cancels, reloads every 15 ms) leaked no slot,
queue entry, body-budget byte or per-key count, and every token counter equalled the
sum of the settled records. ~5 M differential fuzz runs over the parsers and body
edits found no difference. The halves agreed on 32 k configs, 21 k messages, 88 k
usage records, 3 k carry-over pairs and 200 k pricing cases. Nothing recorded as fixed
by earlier audits has regressed; the 2026-09-27 R1 and R3 are confirmed fixed (25 s →
0.3 s, 18 s → 0.03 s) and its test gaps T1–T6 are closed.

**Where the work is:**
- **llama-server breaks assumptions the gateway makes about backends**: it
  under-reports usage for multi-sequence requests, reads its own parameter names in
  preference to the ones the gateway owns, and answers `500` to input a client
  controls. Two of the three are high where llama-server serves a limited or priced
  model.
- **Silent operational failures**: a wrong `base_url` path, a config over 16 MiB,
  and `docker stop`'s default timeout all fail without a clear signal.
- **Tests**: one high gap (Azure's forced service tier is unpinned since the module
  split) and a handful of unreached limit and ack branches.

## Findings

### llama-server behaviour

| # | Finding | Freq | Sev | Found by |
|---|---|---|---|---|
| L1 | **Usage of multi-sequence requests is under-reported, and the gateway settles from it.** llama-server's `usage` for a `/v1/completions` `prompt` list counts one prompt, and for `n` > 1 one choice; the gateway settles limits, budgets and records from that report (`accounting/meter.go:183`), with `estimated=false`. Live: `prompt: ["Hi", 800-token text ×3]` billed `tokens_in 2, tokens_out 5`; chat `n: 4` billed `tokens_out 30`, the same as `n: 1`. Up to 1/16 (prompt lists) or 1/`max_n` (choices) of what ran. llama-server embeddings sum correctly. vLLM is documented to sum; not verified. Fix options: floor settled usage at the gateway's own figures when `Sequences > 1` and flag the record `estimated`; or refuse lists and `n` > 1 on llama-server deployments (a module). | adversarial (trivial); also honest batch or `n` > 1 clients on llama-server | **high** where llama-server serves a priced or limited model | S |
| L2 | **llama-server's own names bypass the owned output-limit and multiplicity fields.** `n_predict` and `n_cmpl` are passed through untouched, and llama-server reads them before `max_completion_tokens`/`max_tokens` and `n`; on `/v1/completions` it reads the untouched `max_completion_tokens` before `max_tokens`. Defeats the output ceiling, the refusal of `-1` (which exists because of llama-server), `max_n`, `max_sequences_per_request` and the reservation. Live: `max_completion_tokens: 8, n_predict: 1500` generated 1385 tokens against a ceiling of 32 and a 400 tokens/min limit; `n_predict: -1` ran to the slot's context; `n_cmpl: 4` returned 4 choices. Bounded by slot context × `--parallel`. Same class as N-S4 (`best_of` on chat). Fix: own these names (check, lower, reserve) or refuse them. | adversarial / occasional (llama.cpp-aware clients) | medium | G, S |
| L3 | **A backend `5xx` the client provokes opens the circuit for everyone.** llama-server answers `500` to a `response_format` schema with an unresolvable `$ref` (and to `{"type":"nope"}`); a backend `5xx` is a circuit failure (`routing/circuit.go`, `GATEWAY.md` Outcome classes). Live: 5 such requests opened the circuit, another key got `503 no_healthy_deployment`; after the 10 s probe one more bad request was the half-open trial and reopened it. The attacker is billed $0, so one request per 10 s sustains the outage. An honest client with a broken schema does the same by accident. Needs a decision on what makes a `5xx` a deployment failure (e.g. failures from ≥ 2 distinct keys, or a failed trial alone never reopens). | adversarial; occasional by accident | **high** for single-deployment llama-server models; medium otherwise | S |

### Correctness

| # | Finding | Freq | Sev | Found by |
|---|---|---|---|---|
| C1 | **A config over 16 MiB is published but no gateway can read it.** `kaiak-control` publishes a document of any size; the gateway caps snapshots and stream events at 16 MiB (`control/transport.go:27`). The failure happens before parsing, so it is not a rejection and the control plane is never told. Live: a booting gateway exits after the boot wait (every pod crash-loops; the sample saw 57 config fetches and 0 status reports); a running gateway reconnects in a loop, re-downloading the 19 MB event, and stays on the old version — then refuses priced USD-limited models once the config-mismatch grace passes. Needs ~151 k keys (~111 B each) or ~3 800 groups with full labels. Fix: a document-size bound checked by `kaiak-control` at publish, or report the size failure as a rejection. | rare (very large deployments) | high (fleet-wide, silent) | P |
| C2 | **A stream that sent `[DONE]` but whose connection the backend keeps open is cut as stalled.** The relay reads to EOF after `[DONE]` (`provider/wire.go:444-481`); if EOF never comes the stall timer fires: the client, which already has everything, sees "unexpected EOF", the record is `partial`, the attempt is a circuit failure, and the slot is held for `stall_timeout_ms` (120 s). None of the four target servers is known to do this; a proxy in front could. Fix: end the response at `[DONE]` on a 2xx stream. | rare | medium when hit | G |
| C3 | **The health probe fails on any models list over 1 MiB.** The probe reads at most 1 MiB, then fails to decode the truncated list: "the answer is not a models list" (`provider/wire.go:166,187,205`). For a backend with a large list (aggregators), an open circuit never half-opens again until the deployment is removed or the gateway restarts. Reproduced with a 1.24 MB list. | rare | medium for the affected deployment | G |
| C4 | **A throwing `onLimitCarriedOver` breaks a publish and loses the carry-over.** The callback is not isolated (`usage/index.ts:234-242`) and runs before the carried amounts are written, after the version was stored and sent to the streams. `publishConfig` rejects, nothing carries, the spend is forgiven; a UI that retries publishes yet another version. Every other host callback goes through `onListenerError`. | rare (needs a host callback that throws) | low–medium | K |
| C5 | **The control-plane client follows redirects and resends `KAIAK_CONTROL_TOKEN`.** Default redirect policy (`control/client.go:64`); Go forwards `Authorization` to a redirect with the same hostname whatever the port or scheme, so an https→http redirect sends it in cleartext. Live: a `307` to another port delivered the bearer token there. The backend client already refuses redirects (`provider/provider.go:296`). Fix: `CheckRedirect: http.ErrUseLastResponse`. | rare (needs control of the control URL's answers, e.g. an ingress) | low | S |
| C6 | *Unverified risk:* **every chat request to every `openai-compatible` backend carries `"service_tier": "default"`** (settled 2026-09-29, `GATEWAY.md` Service tier). The spec says servers that do not define the field ignore it — true for vLLM and llama-server; hosted OpenAI-compatible APIs that validate strictly (Mistral refuses unknown fields; Groq's tiers are `on_demand`/`auto`/`flex`/`performance`) would refuse every chat request. Worth one live check against the backends actually deployed, or restricting the always-add to OpenAI and Azure. | depends on deployment | high if hit | G |
| C7 | **Inserting two or more members into an empty JSON object gives invalid JSON** (no commas; `provider/body.go:121-126`). Latent: unreachable while `model` is always present at the top level and `stream_options` gets a single insert. Found by fuzzing. | none today | low | G |

### Scale

| # | Finding | Freq | Sev | Found by |
|---|---|---|---|---|
| R1 | **Totals reads cost gateways × windows, one at a time on the event loop.** Every stream push does its own full totals read (`fastify/gateway-stream.ts:123` → `usage/index.ts:162`), differing only in `counted_through`, all queued in the one `exclusive` turn acks and publishes use. Each read clones every window (`storage/memory.ts:94`) and every gateway status (`gateways/index.ts:231`). At 10 k active windows and 100 gateways: 12–54 ms per read, an ack waited 1 344 ms behind one round of pushes; 20 batches/s needs more than the loop has. Fine at 20 gateways × 2 000 windows (2.4 ms, 51 ms). Separate from the backlog's "Totals size bound" (message size). Fix: one totals read per round, shared by every stream. | occasional (~100 gateways and ~10 k windows, or ~500 gateways) | medium | K |

### Operations

| # | Finding | Freq | Sev | Found by |
|---|---|---|---|---|
| O1 | **A backend `base_url` with a wrong path (no `/v1`) fails silently.** At apply the only sign is an INFO "did not answer" (it answered 404). Every request relays the backend's bare `404 page not found`, classed `upstream_client_error`: no circuit, no retry, no starter alert. Clients see what looks like a missing gateway endpoint. | occasional (every new backend, every hand-edited URL) | medium | O |
| O2 | **The README's docker control-plane example exits at boot.** It uses the example config but passes no backend API-key variables (the file-mode example does); the gateway rejects version 1 (`api-key-env-unset`) and exits 1, and its message says "fix the published config" when the fix is the gateway's environment. | occasional (first docker trial) | medium | O |
| O3 | **The README's `docker run` examples set no `--stop-timeout`.** `docker stop` kills after 10 s; the drain needs about 75 s. A request still running leaves no `request` line, no usage record and no "usage not flushed" line. `DEPLOYMENT.md` sizes `terminationGracePeriodSeconds` but says nothing for docker or compose. | occasional (docker and compose users, every restart) | medium | O |
| O4 | An unreachable backend at config apply (DNS failure, refused, 404 list) is logged only at INFO; `DEPLOYMENT.md`'s "the gateway warns … for a deployment its backend does not list" holds only when the list is answered. | occasional | low | O |
| O5 | After a last-known-good boot with restored totals, the boot log says priced USD-limited requests are refused until totals arrive, but they are served (correctly, from the restored totals). `GATEWAY.md`'s description of that line is wrong in this case. | rare (data-dir users) | low | O |
| O6 | No log line when the outage refusal starts or ends — only the `kaiak_control_outage` gauge and per-request 503s. | rare (outage past the grace) | low–medium | O |
| O7 | "usage not flushed: lost at exit" is WARN; the other billing-data losses (`memory_bound` drops) are ERROR, so ERROR-level alerting misses this one. | occasional (rollout during an outage) | low | O |
| O8 | The `draining` line's `timeout` is the drain timeout minus the flush reserve (50 s at defaults), a value the operator never set; the reserve is not logged. | occasional (every rollout) | low | O |
| O9 | A config with several errors reports `api-key-env-unset` only after the other errors are fixed — one extra fix-and-reload round. | occasional | low | O |
| O10 | Sample: a gateway's rejection of a config is not logged by the sample, only shown on its status page. | occasional (demos) | low | O |
| O11 | Images (read, not built): `build-images.sh` tags untagged builds `latest`, so `--push` after a release moves `latest` to a dev build (documented in `DEPLOYMENT.md`, contradicts README's "newest stable release"); base images pinned by tag, not digest. README's tag example still says `0.7.4`. | rare | low | O, S |

### Test gaps (mutation-proven: the mutation left every suite green)

| # | Gap | Sev | Found by |
|---|---|---|---|
| T1 | **Azure's forced standard service tier is unpinned** (`provider/azure_openai.go:35`: dropping `standardServiceTier` survives; the openai-compatible twin is caught). Azure's deployment-level priority is the reason for the 2026-09-29 decision. Sketch: one table test running every backend module through the same service-tier cases. | high | T |
| T2 | `Retry-After` must be the longest wait among refusing limits (`limits.go:463`); taking the first survives — the test has one refusing limit. | medium | T |
| T3 | Carry-over to a **second** new limit from the same predecessor is never executed (`limits.go:318/323`, `window.go:378`): mutants that share one counter between the two, or copy in-flight holds nothing releases, survive. | medium | T |
| T4 | Reusing a per-minute bucket without resetting its count survives (`window.go:210`). | medium | T |
| T5 | The cost saturation branch is never executed (`accounting.go:248`); without it, linux/amd64 (the shipped image) gives a negative cost `clampToProtocol` does not catch. The clamp test stays far from int64's edge. | medium | T |
| T6 | A usage ack naming a different batch is taken for the outstanding one, which is dropped uncounted (`control/usage.go:462`, never executed). | medium | T |
| T7 | `kaiak-control` carry-over: a kept limit inherits from a sibling dropped in the same publish (`aggregate.ts:126`); `carried > own` → `carried > 0n` (a window lowered) survives. | medium | T |
| T8 | "Previous month" as −30 days survives in both halves (tests only use boundaries where it happens to be right); the in-memory store's record bound is untested; a batch racing a publish counting against the old config survives. | low–medium | T |
| T9 | Carried over from 2026-09-27 T7, still open: schema-kind invalid fixtures don't pin the failing path (a fixture made valid on its own rule and broken elsewhere stays green); no resolution fixture resolves a path deeper than 4. | low | T |
| T10 | Weak assertions: "sub-nano costs round per record" can't tell rounding from truncation; `TestSendKeepsNoRequestBodyOnceTheFirstEventIsIn` passes when `GetBody` is never set. About 25 further low survivors (log levels, backoff reset, bearer whitespace, drain edges, verify notes) are left for when a change touches that code. | low | T |

No flaky tests in 3× runs of each half. Slowest: `e2e` ~58 s (TestControlModeEndToEnd
12 s, TestSharedLimitsAcrossGateways 10 s, TestMinimalControlPlaneSetup 10 s).

### Independent review [B]

Run on a plain copy of `main` at `627ebfc` (2026-10-01: v0.8.0 plus C5, O2, O3 and the
backend types), report in `AUDIT-independent.md`. Every reproduction was re-run by the
main session and failed as reported, and each mechanism was checked in the code.
Its baseline passed except staticcheck, which it could not download offline.

| # | Finding | Freq | Sev |
|---|---|---|---|
| B1 | **A control-plane `4xx` with code `request-invalid` sets a usage batch aside, whatever the status.** The gateway decides by code alone (`control/usage.go:402`, `batchRefusals[refused.code]`); `kaiak-control`'s Fastify error handler maps every `4xx` it did not produce itself to `request-invalid` (`fastify/index.ts:102`). So a host's own hook answering `429` (throttling) or `401` (its auth) makes the gateway drop the batch — kept only with a data directory — where the spec allows setting aside only on `400`/`413` with those codes (`CONTROL-PROTOCOL.md`, Sending). Reproduced: `429 request-invalid` → batch 1 set aside, batch 2 acked. Fix: the gateway requires the documented status and code; the adapter keeps `request-invalid` for its own body checks. | occasional (hosts adding hooks) | medium (usage lost) |
| B2 | **A failed carry-over write commits the config and loses the spend** — the same root as C4: publishing stores and announces the version before the carried totals are written (`config-versions/index.ts:73-75`, `usage/index.ts:242`), and a retry compares against the already-committed model set, so nothing carries. Reproduced with an injected store failure: $100 spent, version 2 live, carry `[]` after the retry. Fix C4 and B2 together: carry-over and publish as one step, or the carry made before the version is announced. | rare | medium |
| B3 | **A status report over 64 KiB is refused, so that gateway never joins the live set** (`fastify/index.ts:40`); every deployment is in the status (`cmd/kaiak/main.go:694-712`) and nothing in the config bounds their number. 30 backends × 20 models (600 deployments) gives ~88 KiB; the bound falls near 450 deployments. With no live gateway counted, each gateway enforces the whole per-minute limit and backend cap instead of its share (`limits/shared.go:69`). Reproduced through the real adapter and the gateway's status builder. Related to C1 (config size) and the config-size metrics in `docs/BACKLOG.md`. | daily, once a config has ~450+ deployments | medium (shares wrong fleet-wide, status lost) |
| B4 | **The memory store hands out its stored config, not a copy** (`storage/memory.ts:48`, `configsAfter` likewise; `saveConfig` copies): a host that edits `currentConfig()` before publishing changes the active config even when the publish is then refused. Reproduced: a rejected edit left `requests_per_minute: -1` active at version 1. Fix: copy on read, as on write. | occasional (library hosts) | medium |

### Minor and by design (recorded only)

- **Re-pricing is not exactly reproducible**: the gateway rounds in float64
  (`accounting.go:245-251`); 402 of 200 000 cases differ from exact arithmetic by
  1 nano-USD (half-nano ties). The spec promises the control plane "can re-price by
  the same rule" without stating the formula or tie rounding. [P]
- **The price date is not in the record**: the entry is chosen by request start
  (`accounting.go:135`), the record carries only the settle time — a request spanning
  a price change at UTC midnight is re-priced by the wrong day. [P]
- **Model defaults are not access control**: a key allowed a cheap model on a shared
  deployment can send another model's distinguishing parameters (`lora`,
  `enable_thinking`) itself. Worth a sentence in the spec, like 2026-09-27's S2. [S]
- *Suspicions, not reproduced* (no vLLM/OpenAI/Azure here): vLLM's `priority` field
  lets a key jump the backend queue; audio output tokens priced at the text rate;
  Azure `data_sources`. Same class as `service_tier`. [S]
- A deployment model named `__proto__` is schema-valid, but Fastify refuses every
  status report containing it; that gateway never joins the live set. [P]
- `effective-limits-exceeded` is counted differently when `child_defaults` repeats a
  limit (already invalid; both reject with different code sets); `key-hash-duplicate`
  names a different key in each half. [P]
- `validateConfig` throws `RangeError` on model defaults nested ~3 000–9 000 deep; the
  gateway accepts up to 10 000. [P]
- `verifyBackend` reports `unreachable` for a credential that is not a valid header
  value (en dash, control characters); the spec's "a valid header value" is wrong. [K]
- The boundary lint misses cross-package relative imports into another package's
  `src/` (`check-boundaries.ts:84`). Nothing does it today. [K]
- Root-path config-snapshot issues get a garbled message (`"/config51000 effective
  limits …"`, `messages/index.ts:48`). [K]
- `usage-record.schema.json:108` still calls `gateway_time` "informational"; since D4
  it chooses the record's window. [K]
