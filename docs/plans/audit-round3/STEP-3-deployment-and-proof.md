# Step 3 — deployment notes and proof

**Status:** done (2026-09-25) — phase 3 ends green, the plan ends; images built and
smoke-tested on `dev`, **push pending (main session)**

## Items

- In-memory usage bound sized by bytes: `KAIAK_USAGE_MEMORY_BYTES` (default 64 MiB),
  replacing the fixed 10 000-record bound in stateless mode; metric of queued bytes;
  DEPLOYMENT.md: the record rate the default covers vs the outage grace.
- Optional `KAIAK_MAX_CONNECTIONS` (gateway-wide, pre-authentication; 0 = no cap):
  connections beyond it are refused at accept time; DEPLOYMENT.md ingress limits.
- e2e where process-level proof is missing (duplicate-member request refused through
  the binary; sequence cap); `scripts/check-all.sh` 3× green.
- Images rebuilt and pushed (main session).

## Acceptance

Tests; guide verified against the code; images pushed. Plan end.

## Result

- **Usage memory bound by bytes** — `control.Options.UsageMemoryBytes`
  (`KAIAK_USAGE_MEMORY_BYTES`, default `control.DefaultUsageMemoryBytes` = 64 MiB,
  whole bytes above 0) replaces `maxSealedRecords`. Each checked record counts its
  exact encoded size, taken from the `json.Marshal` the seal-time record check
  already does (`recordIssues` now returns the length): no second encoding. Sealed
  batches carry their bytes into their queue entry; the sender keeps `sealedBytes` /
  `queuedBytes`. `boundQueued` (no data directory) drops the oldest queued batches
  past the bound, never the outstanding one; `boundSealed` (spool unwritable) the
  oldest sealed ones, always keeping the newest — same logs (now with `kept_bytes`,
  `bound_bytes`) and drop reasons. Before bounding on a failed spool write every
  unchecked sealed batch is checked (`checkAllSealed`), so each has its size.
  `UsageObserver.UsageSpoolDepth` gained the bytes; new gauge
  `kaiak_usage_queued_bytes`, created at 0.
- **`KAIAK_MAX_CONNECTIONS`** — `server.Listener.LimitConnections(max, refused)`
  wraps the API listener (0: unwrapped): an atomic count of open connections, a
  connection accepted beyond the cap closed at once, `refused` called; a
  `countedConn` gives its place back once on close and passes `CloseWrite` through
  (net/http's half-close after an early answer depends on it). New counter
  `kaiak_connections_refused_total` (Ops), created at 0. The admin listener stays
  uncapped. Settings parsing: one `wholeNumber` helper for
  `KAIAK_BODY_MEMORY_BYTES`, `KAIAK_USAGE_MEMORY_BYTES` (≥ 1),
  `KAIAK_MAX_CONNECTIONS` (≥ 0).
- **Record size, measured** (a scratch test, removed): a record with 32-character
  record and request IDs, a pod-name instance (`kaiak-gateway-7d9f8b6c5d-x2k4p`),
  ordinary key/team/workload/model names and all four token units encodes to
  **506 bytes**; 200 000 of them held **536 bytes of heap each** — the encoded size
  is a fair stand-in for memory. Arithmetic in DEPLOYMENT.md (≈ 510 bytes/record):
  64 MiB ÷ 510 ≈ 131 600 records; ÷ 900 s (the 15-min grace) ≈ **145 records/s**
  covered; 20 records/s → ≈ 110 min, 100/s → ≈ 22 min, 1 000/s → ≈ 2 min. Sizing
  rule: bytes ≈ R × T × 520 (1 000 records/s × 900 s ≈ 470 MB → 512 MiB).
- **Docs**: GATEWAY.md — both variables in Configuration sources, the byte bound
  (settled 2026-09-25, with the rejected record count) in Usage batches in memory
  and Sustained write failure, the connection cap (settled 2026-09-25, close vs
  `503`) under Lifecycle → Client timeouts, both metrics in the metric list.
  DEPLOYMENT.md — outage coverage table and sizing, the lost-usage table, Resources
  headroom, env table rows, alert rows (`kaiak_usage_queued_bytes > 33554432`;
  `increase(kaiak_connections_refused_total[5m]) > 0`), "Connection floods: limit at
  the ingress" (ingress-nginx `limit-connections` / `limit-rps`), the data-directory
  reason. ARCHITECTURE.md — the bound's wording.
- **Tests**
  - `control.TestWithoutADataDirectoryTheBoundCountsEncodedBytes` (new): two long
    records take the room of more than four short ones and are dropped under a
    4-short-records bound (a record count of 4 would have kept them); the reported
    bytes equal the held records' encoded size, 0 after the flush. It could not
    compile against the previous code (no byte option), which is its failing state.
  - `TestWithoutADataDirectoryQueuedRecordsAreBounded` and
    `TestSealedRecordsInMemoryAreBoundedWhileTheSpoolCannotBeWritten`: bound now 4
    records' worth of bytes (`recordSize(testRecord(9))`); same expectations.
  - `server.TestConnectionsBeyondTheCapAreClosedAtAccept` (cap 2: third connection
    closed with nothing sent, one refusal counted; after one closes a new one is
    served, the kept one still served), `TestNoCapLeavesTheListenerUncapped`.
  - `metrics`: `kaiak_usage_queued_bytes` and `kaiak_connections_refused_total` at 0
    initially, then set/counted. `cmd/kaiak.TestUsageMemoryAndConnectionSettings`:
    defaults, values, refused values naming the variable.
  - e2e `TestRequestInputCapsThroughTheBinary` (repeated `model` → `400
    duplicate_member` `param` `model`; 17 prompts → `400 invalid_value` `prompt`;
    2049 embedding inputs → `input`; nothing reaches the backend; 16 prompts and 2048
    inputs pass) and `TestConnectionCapThroughTheBinary` (`KAIAK_MAX_CONNECTIONS=1`:
    a held keep-alive connection, the next closed with nothing sent, the counter at
    1 on the uncapped admin port). Both `-race -count=5` green (12 s).
  - Changed packages `-race -count=5`: control, server, metrics, cmd/kaiak green.
- **Images** (`scripts/build-images.sh`, no `--push`, context `dev`, from
  `23f812e`): `kaiak:0.4.1-23f812e` **18.9 MB**, `kaiak-sample:0.4.1-23f812e`
  **373 MB** unpacked. `scripts/smoke-images.sh` → `smoke passed` (file mode and
  control-plane mode on read-only roots, chat 200 through each, `kaiak_build_info`
  `0.4.1-23f812e`, `usage flushed` on stop). The six tags were removed from `dev`
  afterwards; its image list matches the one before the build; no `kaiak-smoke`
  container left. **Images pushed: pending — main session.**
- **Suite**: `GOFLAGS=-count=1 scripts/check-all.sh` three times in a row, all
  green — see the tails below.

## Decisions made during the step

- **Encoded size, exact**, not a per-record estimate: it comes free from the check
  encoding, and records vary (client request IDs up to 128 characters).
- **One setting for both bounds**: the spool-unwritable case (with a data
  directory) uses `KAIAK_USAGE_MEMORY_BYTES` too — it bounds the same thing, usage
  held in memory; two knobs would only invite a mismatch.
- **What counts**: checked sealed batches, plus queued ones when the queue is in
  memory. Unchecked sealed batches wait at most one seal tick (checked before the
  spool-failure bound runs); the filling batch is bounded by `BatchMaxRecords`
  (500). Batches restored from the spool count 0 (on disk).
- **`kaiak_usage_queued_bytes`** is the bytes *held in memory* against the bound —
  with a data directory only the sealed records the spool could not write (the name
  is the brief's; the help text and spec say exactly what it holds).
- **Close at accept, not `503`**: no goroutine, read or write per refusal, so a
  flood cannot make refusals expensive; a client sees a close with no answer.
- **No log line per refusal** (a flood would flood the log): the counter and its
  alert row are the signal.
- **The cap counts idle keep-alive connections** and applies to the API listener
  only; the admin listener stays uncapped so probes and scrapes pass a flood.
- **`KAIAK_USAGE_MEMORY_BYTES` in file mode** is accepted and ignored (no usage
  queue), like `KAIAK_DRAIN_FLUSH_RESERVE_MS`.

## Decisions for the user to confirm

- The default stays **64 MiB**, which covers about 145 records/s through the
  15-minute grace (the implementation plan's "≈ 200 000 records" assumed smaller
  records; measured ones are ~510 bytes). Higher-rate pods raise it per the sizing
  rule.
- `KAIAK_MAX_CONNECTIONS` defaults to **no cap**; refusals are silent in the log.
- One bound for both memory cases (above).

## check-all tails

At `23f812e` (code final; the commit after it only edits plan docs), each run
`GOFLAGS=-count=1 scripts/check-all.sh`, exit 0:

| Run | Gateway (live-test kit) | Control | Cross-half e2e | Result |
|---|---|---|---|---|
| 1 | `gateway checks passed` | `tests 472` | `ok kaiak/e2e 38.5s` | `all checks passed` |
| 2 | `gateway checks passed` | `tests 472` | `ok kaiak/e2e 38.2s` | `all checks passed` |
| 3 | `gateway checks passed` | `tests 472` | `ok kaiak/e2e 48.2s` | `all checks passed` |

## User confirmation

All open "Decisions for the user to confirm" in this step were confirmed by the user on
2026-09-25, as implemented.
