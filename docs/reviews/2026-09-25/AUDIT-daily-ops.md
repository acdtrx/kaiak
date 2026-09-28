# Daily-operations review — 2026-09-25 (at 0.5.0)

Focus (user): what affects **daily operation under legitimate use**. Every finding is
tagged `daily` / `occasional` / `rare`; only `daily` (and high-impact `occasional`) go to
implementation — the rest stay recorded here. Two reviews merged:

- **[A]** five read-only reviewers (client compatibility with real SDK runs, money under
  normal traffic, routing and lifecycle, operator experience with live runs,
  performance with measurements);
- **[B]** an independent review on a plain copy — verbatim in
  `AUDIT-daily-ops-independent.md`.

Performance verdict: the gateway is not the bottleneck at the target load; memory (≈70 KB
live per open stream), not CPU, runs out first (~5–7k streams per replica at the default
limit). Parsing overheads are real but small (≈0.5 core per replica at peak, GC ≈1%).

## Implement (agreed with the user 2026-09-25)

| # | Finding | Freq | Found by |
|---|---|---|---|
| D1 | Inline images/files counted by base64 bytes (≈170k "tokens" per screenshot): answers cut to 256, "request too large" 429s, huge estimated charges | daily | A (2 reviewers, SDK run), B |
| D2 | Completion batches subtract the whole batch from one context window → per-prompt default output 256 | daily (batch) | B |
| D3 | A deployment answering 429 attracts nearly all first attempts; ~75% of requests fail while another Azure resource is idle | daily (quota) | A (experiment), B (probe) |
| D4 | Gateway retries on the same deployment multiply with SDK retries (up to 9 hits per call), opening single-deployment circuits | occasional, high | A (2 reviewers) |
| D5 | Per-key concurrency 16 per gateway refuses shared service keys and batch jobs | daily | A (SDK run) |
| D6 | Operators can't see why: limit 429s log no scope/limit; relayed backend 4xx/429 have no error code; TTFT blames the answering backend; attempt metrics published only at request end | daily | A, B |
| D7 | Boot makes one control-plane attempt → crash-loop during a control-plane restart | occasional | A |
| D8 | A fresh pod serves budgeted traffic before its first totals arrive (treats budgets as unspent) | occasional, high | B (binary repro) |
| D9 | Docs/alerts: README sample walkthrough breaks after a backend restart; alerts page falsely/duplicated; stream memory missing from sizing; token reservation of the full output unexplained; vLLM cached-token flag | daily | A |

## Backlog

- Rollout share shrink: draining gateways stay counted in `live_gateways` (caps and
  per-minute shares halve during rollouts) — A (2 reviewers), occasional.
- Fair dispatch across owners in model queues (the proper replacement for per-key caps).

## Recorded only (rare, or efficiency without real impact)

Per-chunk JSON decoded 3–4× and request bodies scanned twice (≈0.5 core at peak);
`Retry-After` overstated when room is held by reservations; integer fields sent as floats
refused; the sample page at 20 hosts × N gateways; tool-call streams silent past the stall
timeout (verify on real vLLM); estimated records from cancelled streams (±, Azure
reasoning not streamed) and an `estimated` usage label; `/v1/responses` and Azure-style
client paths absent; no Go runtime metrics; synchronous log writes.
