# Pre-merge review, round 3 — 2026-10-07 (`control-replicas` at `f811822`)

## Scope

The branch after phase 5 (`docs/plans/control-replicas/`, steps 11–14):

- no store sequence and no rollback detection;
- configs ordered per stream by read issue;
- one totals feed per plugin: complete first, then changes;
- every scope counted, with limits on top;
- `counted_through` per epoch;
- stream-only contact;
- the store keeps the config text;
- the store contract with a lossy channel and negative controls;
- the new Postgres sketch.

## Reviewers

- Three read-only reviewers worked on a detached worktree at `f811822`:
  - **[R]** kaiak-control's store and core concurrency;
  - **[G]** the gateway;
  - **[K]** the contract across both halves, and the docs.
- **[C]** is the independent review (Codex, a plain copy). Its report is in
  `AUDIT-3-independent.md`. The main session re-ran its 8 reproduction tests, and
  every one fails as claimed. They are kept in the session scratchpad (`audit-c3/`)
  for porting.

Every finding is tagged by how often it shows up under legitimate use: `daily` /
`occasional` / `rare` / `adversarial`. When several reviewers reached a finding on
their own, each of them is named.

## Verdict

**Not ready to merge, but the ordinary path is sound.**

- **Found sound by every reviewer:**
  - ordering by read issue;
  - skip by hash;
  - teardown;
  - per-epoch retirement;
  - the changes-only merge on a healthy stream.
- **What is left is mostly one gap with several causes:** a gateway can keep enforcing
  on totals that have stopped updating, without ever entering outage.
- **Two gaps in the gateway's budget across restarts and reloads:**
  - a crash forgets the spend that is spooled;
  - deleting and recreating a group forgets its spend.

## Decisions taken with the user (2026-10-07)

Recorded in the plan's OVERVIEW as decisions 29–34 and implemented in phase 6 (steps
15–17):

- **One stale-totals signal, on the gateway:** an acknowledged batch keeps the
  usage-waiting clock running until stream totals show it counted. It comes with
  two exact fixes:
  - a stream's first totals come from a read issued after it joined;
  - a malformed totals event ends the stream.
- **The spool keeps a batch until saved totals cover it**, so the saved totals plus
  the spool rebuild own usage exactly after a crash. `totals.json` is then written on
  an interval and in the background.

## High

### 3H1: Totals that stop updating go unnoticed; gateways enforce on frozen bases

`rare`; the cost is money (N gateways can each spend what is left). Found by
[G] G3-M1, [R] R3-M1, R3-M2 and R3-L2, [K] K3-M1 and [C] C1, C5.

**Causes**, all with stream bytes still flowing, so there is no outage:

- **Heartbeats with no totals:**
  - a dead notification channel (a silently dropped `LISTEN` connection);
  - a totals read that keeps failing or never settles (`totals-feed.ts:86-97`).

  Heartbeats keep the contact time fresh, and an ack stops the usage clock
  (`control/usage.go:335-339`).
- **A joining stream gets the feed's latest read**, which may be older than what
  another core already sent that gateway (`totals-feed.ts:119-121`,
  `gateway-stream.ts:99-108`). Counts and `counted_through` go back, and usage the
  gateway already retired is in neither count.
- **A failing feed serves its last good read as a new stream's complete totals.**
  Reconnecting does not repair it.
- **A malformed totals event is skipped** (`stream.go:116-122`; `GATEWAY.md:2050`). A
  skipped change is lost for the connection. A skipped *first* event makes the next
  changes-only one count as complete, which zeroes every base it does not list.

**Outcome:** decisions 29–31 (steps 15–16).

### 3H2: A crash forgets spooled spend from the budget

`rare`. Found by [C] C2, with [G] G3-M2.

- **Where:** `cmd/kaiak/main.go:433, 452`; `limits/persist.go:130`;
  `control/usage.go:283`.
- **How:**
  - `totals.json` is saved on each totals event and at shutdown.
  - Usage settled after the last save is spooled durably, and is delivered later.
  - On restart, the limiter gets only the spool's generation, not its amounts, and
    `totalsKnown` is set.
  - A restarted gateway that is still cut off can spend the budget again.
- **Related, [G] G3-M2 (`daily` with a data directory):**
  - `totals.json` (2 MB at 7k groups) is rewritten with two fsyncs on every push, on
    the stream goroutine;
  - that is up to about 170 GB a day, and config events wait behind the disk.
  - Writing less often would widen 3H2's gap.
- **Outcome:** decision 32 (step 16).

## Medium

- **3M1: Deleting and recreating a group forgets its current-window spend**
  (`occasional`). Found by [C] C3, with C8.
  - `sync` (`limits.go:231-265`) rebuilds counters from the config only. The spec says
    a recreated ID keeps its spend.
  - C8 is related: `pushed` keeps expired windows for the life of a stream.
  - **Outcome:** decision 33 (step 16).
- **3M2: The 50 000 effective-limit bound no longer bounds counter memory** (`rare`).
  Found by [C] C6, with [G] G3-L2.
  - Every scope now gets two counters, limited or not.
  - Each hour or month window carries three unused 60-slot minute arrays (about 26 MB
    at 7k groups).
  - **Outcome:** decision 34 (steps 15–16).
- **3M3: The publish checks the parents rule against the caller's mutable object, not
  the captured text** (`rare`). Found by [C] C4 (`config-publishing/index.ts:154-168`).
  - **Outcome:** step 15. Parse the captured text once, and use it for every check and
    for the result.
- **3M4: Restore guidance** (`rare`). Found by [C] C7 and [R] R3-L3.
  - A restored revision allocator can reuse revisions that old operations still hold.
  - A restore done in place, with the channel up, is never announced.
  - **Outcome:** step 15. The GUIDE's restore becomes:
    - stop every control-plane process;
    - restore, and move the revision allocator past every value issued;
    - start (each start catches up).

    The contract says so.
- **3M5: "A read after a notification sees the change" is tested for batches only**
  (`rare`, needs a store bug). Found by [R] R3-M3 and [K] K3-L2.
  - The core relies on it for `config-published` → `currentConfig()` and for
    `gateways-changed` → `gateways()`.
  - The GUIDE requires the primary for `totalsSnapshot` only.
  - **Outcome:** step 15:
    - a contract test per change type, with a negative control (a store that
      announces before its write is visible);
    - GUIDE: every read after a notification goes to the primary.
- **3M6: Following the Postgres sketch fails the contract** (`occasional` for porters).
  Found by [K] K3-M2, K3-L1, K3-L3 and K3-L4.
  - "Records newest first" ties on `received_at`.
  - Two sweeps can deadlock in `forgetGateways`, because the instance order is
    undefined.
  - The torn-read claim is too strong for a real database.
  - `int8` and `numeric` come back as strings.
  - **Outcome:** step 15:
    - an insertion-order column, and `types.ts` defining ties;
    - forgets in instance order;
    - "likely to catch";
    - a representation note.
- **3M7: `DEPLOYMENT.md:997-999` says a file-mode limit added mid-window starts
  empty** (`occasional`). Found by [K] K3-M3. It now checks the count so far.
  - **Outcome:** step 17.

## Low

- **3L1: A failing past-window prune turns a counted batch into a 500** (`rare`).
  Found by [R] R3-L1 (`usage/index.ts:188`). Step 15 makes the prune best-effort and
  logged.
- **3L2: A store call that never settles stalls config delivery on its core** (`rare`).
  Found by [R] R3-L2. Step 15: the contract says every call settles, and the GUIDE
  says to set statement timeouts.
- **3L3: Negative controls match the failing test by name only.** Found by [R] R3-L4
  and [K] K3-L6. Step 15 also checks the failure count and the assertion text.
- **3L4: The acknowledged list is not bounded for priced traffic** (`rare`), and the
  walk on each ack is quadratic. Found by [G] G3-L1. Step 16 folds it into decision
  32, where the spool's own bound applies.
- **3L5: The "0" rule has no control-side test.** Found by [R]. Step 15.
- **3L6: Migration note gaps** (`DEPLOYMENT.md:1003-1044`). Found by [K] K3-L5. Step 17.
- **3L7: Rejected lines say why something existed, not why it fails.** Found by
  [K] K3-L7. Step 17.
- **3L8: Stale wording.** Step 17. Found by [K] K3-L8 and K3-L9, [C] C9 and [G] G3-L3:
  - `GATEWAY.md:2260` (the ack metric is not contact);
  - the duplicate-members `cases.json` ("retires");
  - the boot diagram in `control-plane.html`;
  - `CONTROL-PROTOCOL.md:43, 722`;
  - `spool.go:25`;
  - fakecontrol listing an ended window at "0";
  - the OVERVIEW's unmarked superseded lines.

## Accepted, not filed

- **Clock skew within decision 28's second** ([R]):
  - a gateway clock slightly ahead loses that record from its current hour for the
    rest of the hour;
  - a core behind another keeps a pre-boundary read until its next notification.

## Checked and sound

- **Config order** ([R], [K], [C]):
  - read numbers are taken at issue, retries included;
  - the connect read against deliveries is correct in every traced interleaving;
  - skip by the stream's own hash;
  - A→B→A, catch-up and duplicate notifications are harmless.
- **Totals on a healthy stream** ([R], [G], [K], [C]):
  - complete first, then changes;
  - coalescing for slow readers;
  - one read per push;
  - boundary re-read;
  - `counted_through` from the same read as its windows;
  - the "0" rule;
  - merging on the gateway, including scopes the config lacks.
- **Retirement** ([G], [C]):
  - send order;
  - per-epoch coverage;
  - the counted mark only moves up;
  - records that settle late;
  - the ack hand-off under one lock.
- **Teardown** ([R], [C]): close-first on every path, idempotent; no write after end;
  timers and subscriptions released.
- **Contact and config on the gateway** ([G]):
  - an ack is not contact;
  - readiness waits for real totals;
  - the running hash clears a rejection;
  - the rejected gauge.
- **Spec against code, schemas, fixtures, metrics and log fields** ([K]): consistent.
  The ARCHITECTURE diagram matches the imports edge for edge.
- **Postgres sketch** ([K], [C]):
  - valid SQL;
  - text config;
  - conditional writes;
  - REPEATABLE READ snapshot;
  - notify on commit;
  - catch-up after `LISTEN` is back (beyond 3M4 and 3M6).
- **Removal** (all four): clean in code; only dated Rejected lines remain.
- **Scale** ([G], [C]):
  - Reserve plus Settle about 0.7 µs;
  - first totals about 1.4 MB per gateway at 7k groups;
  - a one-group change 273 B;
  - feed fan-out 5 ms sparse, 66 ms near-full.

## Outcome (2026-10-07)

Phase 6 (`docs/plans/control-replicas/STEP-15..17`). Commits:
- `86b905d`: the specs, both halves;
- `a7526cd`: kaiak-control, the store contract, the GUIDE, the fixtures;
- `f5cfcba`: the gateway;
- `e206388`: gateway docs and format notes;
- `740989b`: the remaining docs.

The regression tests and their before-fix runs are tabled in each step's Result.

| Finding | Outcome | Commit | How |
|---|---|---|---|
| 3H1: stale totals unnoticed | fixed | `86b905d`, `a7526cd`, `f5cfcba` | Decision 29: an acknowledged batch waits to be shown counted, its own outage reason past the grace. Decision 30: a stream's first totals come from a read issued after it connected, and nothing is sent while reads fail. Decision 31: a malformed totals event ends the stream. R3-L2's hung store call: the contract says every call settles (statement timeouts). |
| 3H2: a crash forgets spooled spend | fixed | `f5cfcba`, `e206388` | Decision 32: spool format 4 keeps acknowledged batches until a covering `totals.json` write, and boot rebuilds own usage from the batches beyond the saved `counted_through`. `totals.json` format 5 is written in the background every 30 s and at shutdown (G3-M2: 15.2 ms on every push on the stream goroutine → at most every 30 s, off it). |
| 3M1: a recreated group forgets its spend | fixed | `f5cfcba` | Decision 33: a removed scope's hour and month counters are retained while their window holds own usage, and taken back by a recreated ID. Ended pushed windows are pruned once an hour (C8). |
| 3M2: the bound no longer bounds memory | fixed | `a7526cd`, `f5cfcba` | Decision 34: `counters-exceeded` (two per scope plus each effective per-minute limit, 50 000) on both halves, replacing `effective-limits-exceeded`. Minute buckets for per-minute windows only (counter heap at 7k groups 29.7 → 7.6 MiB). A changes-only `TakeTotals` 1.49 ms → 0.21 µs. |
| 3M3: parents checked against a mutable object | fixed | `a7526cd` | The publish takes the text once before its first wait; the parents rule and the returned `config` parse it. |
| 3M4: restore guidance | documented | `86b905d`, `a7526cd` | Contract and GUIDE: restore with every process stopped, the revision allocator moved past every value issued, then start (each start catches up). No ported test: the memory store has no restore operation, so a test could only show a broken store is broken. |
| 3M5: read after notification tested for batches only | fixed | `a7526cd` | A contract test per change type, and the `early-announcement-store` negative control. GUIDE: every read after a notification goes to the primary. |
| 3M6: the Postgres sketch fails the contract | fixed | `a7526cd` | An insertion-order column, with ties defined in `types.ts`; the sweep forgets in instance order; "likely to catch"; the `int8`/`numeric` note. |
| 3M7: file-mode limits "start empty" | fixed | `740989b` | `DEPLOYMENT.md`: in both modes a limit added mid-window checks its scope's count so far. |
| 3L1: a failing prune fails a counted batch | fixed | `a7526cd` | Past windows are dropped by the expiry sweep, once an hour per process; a failure fails that sweep run, reported through `onExpirySweep`. The brief's "best-effort, logged" was not possible: the core has no logger, and silent catches are not allowed. |
| 3L2: a hung store call stalls delivery | fixed | `a7526cd` | `types.ts` and the GUIDE: every call settles, and a database store sets statement timeouts. |
| 3L3: negative controls matched by name only | fixed | `a7526cd` | Each control asserts the failure count and the assertion text. |
| 3L4: the acknowledged list unbounded for priced traffic | fixed | `f5cfcba` | With a data directory the acknowledged batches live in the spool. Without one, at most 10 000 are remembered and the wait keeps its time. The quadratic walk is gone with `clearedAt`. |
| 3L5: no control-side test of the "0" rule | fixed | `a7526cd` | `fastify/round-3.test.ts`. |
| 3L6: migration note gaps | fixed | `740989b` | `configEpoch()`, `configsSince()`, `beforeSave`, the `ListenerEvent` variants, `StoredConfig` → `PublishedConfig`/`ConfigEntry`, `counters-exceeded`, and this phase's store contract rules. The spool 4 and `totals.json` 5 notes come from `e206388`. |
| 3L7: Rejected lines narrate | fixed | `740989b` | Each says the failure the alternative causes: `CONTROL-PROTOCOL.md` (the history, the config epoch, `config-unavailable`, counting under the config's limits) and `GATEWAY.md` (the totals revision). |
| 3L8: stale wording | fixed | `86b905d`, `f5cfcba`, `740989b` | The ack metric is not contact; `cases.json` "names"; the boot diagram (the running stream brings totals); the ack box (kept until covered); the retried-ID list; `CONTROL-PROTOCOL.md:43, 722`; `spool.go`'s invariant; fakecontrol's ended window; the OVERVIEW's superseded lines. |
| Clock skew within a second | accepted | — | Decision 28. |
