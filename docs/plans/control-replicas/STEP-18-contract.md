# Step 18 — no data directory: contract and docs

**Status:** done (2026-10-07)

## Intent

Write decision 35 (`OVERVIEW.md`) into every document that states a contract or a
rule, so step 19 removes the code against a settled text.

**Phase 7 is a removal.** Tests are expected to fail between steps. Nothing is kept,
renamed, aliased, or left as a compatibility path to keep a test green. Tests that
assert removed behaviour are deleted, not adapted.

## Scope

- **`docs/specs/GATEWAY.md`:**
  - Configuration sources: control-plane mode boots from the stream, else the seed
    config when the control plane is unavailable, else exit. Last-known-good is gone.
  - The Data directory section is removed.
  - Usage spool: in memory only. The drain reserve delivers it; an undrained death
    loses it. Acknowledged batches are remembered in memory, at most 10 000, for the
    stale-totals signal.
  - Restart: no totals or spend survive. The first totals since start gate priced,
    USD-limited models (No totals yet, as today).
  - File mode: counts start from zero at every start; file mode is for local and
    development use.
  - The metrics and log tables lose every data-directory row and field.
  - The decisions it settles are dated 2026-10-07, with a Rejected line for the opt-in
    data directory saying the failure it caused.
- **`docs/specs/CONTROL-PROTOCOL.md`:**
  - A batch epoch is a fresh one at every gateway process start.
  - Remove the spool-survives-restart and last-known-good wording.
  - Status: `applied_config_hash`'s description loses "its last-known-good copy" (in
    `protocol/schema/status.schema.json` and its copy too; `npm run sync-schemas`).
- **`AGENTS.md`** (`[PROJECT]` sections):
  - Deployability: the gateway writes nothing to disk; the read-only filesystem is the
    only mode.
  - Feature-Building Mode: the data-file format paragraph goes.
  - The `Project Facts` hard constraints: "its opt-in data directory is cache and
    spool" goes.
- **Other docs:**
  - `docs/kaiak.md`, `docs/TECH-STACK.md`, `README.md`;
  - `docs/ARCHITECTURE.md`, `docs/architecture/gateway.html`,
    `docs/architecture/control-plane.html`;
  - `docs/DEPLOYMENT.md`:
    - the data-directory and volume section;
    - the format notes;
    - the flush-before-upgrade notes;
    - the alerts;
    - a migration note saying `KAIAK_DATA_DIR` is gone, and that the directory's
      files can be deleted;
  - `docs/testing/LIVE-BACKENDS.md`;
  - `docs/BACKLOG.md`: entries about the spool, epochs on disk or data files are
    resolved and removed.
- **`gateway/Dockerfile`:** the comment about the data directory's volume.

## Removal checklist (gone by step 20)

Grep the whole repo outside `docs/plans/` and `docs/reviews/`, case-insensitive where
it makes sense. A pattern returns nothing, or only its allowed exception.

| Pattern | Allowed exception |
|---|---|
| `KAIAK_DATA_DIR`, `DataDir`, `dataDir`, `data director`, `data dir` | the dated Rejected line; DEPLOYMENT's migration note naming the removed variable |
| `last-known-good`, `lastKnownGood`, `LastKnownGood`, `lastknowngood` | the dated Rejected line; the migration note |
| `totals.json`, `limits.json`, `last-known-good.json` | the migration note |
| `spooldisk`, `batchStore`, on-disk spool index, `usage-batch-`, `index.json` | none |
| `RestoreSpooled`, `RestoreOwn`, `SpoolCovered`, `saveShared`, `SaveShared`, `LoadShared`, `saveSharedPeriodically`, `persistMu`, `WriteVersioned`, `internal/state` | none |
| `format version`, `format [0-9]` for data files; "flush before upgrading" | protocol and config format versions, which are unrelated |
| `PVC`, `volumeClaimTemplates`, `emptyDir`, `StatefulSet` as gateway guidance | none |
| `restart keeps the last totals`, `rebuilt` own usage, `crash rebuild` | none |

## Files likely touched

The files listed above, plus `protocol/schema/status.schema.json` and its copy in
`control/kaiak-control/schema/`.

## Acceptance criteria

- Every document above states decision 35 and nothing of the data directory, apart
  from the allowed exceptions.
- The checklist is copied into Result, each item marked done (docs) or left for step 19
  (code) or step 20.
- Suite run and recorded. Code is unchanged in this step, so the suite should be
  green; name any red.

## Result

**Commits:**
- `064f21a`: `GATEWAY.md`, `CONTROL-PROTOCOL.md`, the two schema descriptions (both
  copies), and the GUIDE's epoch wording;
- `0327164`: `AGENTS.md`, `README.md`, `docs/kaiak.md`, `TECH-STACK.md`,
  `ARCHITECTURE.md`, both architecture pages, `DEPLOYMENT.md`, `BACKLOG.md`,
  `LIVE-BACKENDS.md` and the Dockerfile comment;
- this Result.

**What changed**

- **`GATEWAY.md`:**
  - Configuration sources:
    - the minimal gateway is "stateless — it writes nothing, ever";
    - `KAIAK_CONFIG_FILE` says file mode is for local and development use, counts
      from zero at each start;
    - the boot wait falls back to the seed only.
  - **`KAIAK_DATA_DIR` is replaced by "Nothing is written to disk"** (settled
    2026-09-25, E1; with no opt-in either, 2026-10-07). It carries the one dated
    Rejected line: the opt-in data directory, and why it failed (ephemeral disks;
    every review found its restart path mis-counting budgets).
  - The boot table loses its last-known-good column. The last-known-good bullets go,
    and so do the seed's "never saved as last-known-good" and the
    data-directory-file duplicate-member clause.
  - Usage batches:
    - sealed batches queue in memory;
    - the whole Usage spool section is removed;
    - Record checks at seal time move under Usage batches in memory: an invalid
      record is dropped alone;
    - the epoch is 32 random hex digits, new with every process.
  - Limits:
    - "File mode keeps nothing across a restart" replaces the file-mode usage
      snapshot;
    - "A restart counts from the next totals" replaces "Restart keeps the last
      totals";
    - the usage-waiting clocks lose "(or restored at boot)" and the other-instance
      sentence;
    - the file-mode drift's restart line is updated, and so is the Limits intro.
  - **Decision 36 is written into "A count outlives its scope's config":**
    - counts are kept while own usage or a running request's reservation holds them,
      of any amount, zero included, and across a window roll-over;
    - they are dropped once neither holds;
    - they are outside the `counters-exceeded` bound.
  - Metrics:
    - `last-known-good` trigger removed;
    - `spool_unwritable` reason removed;
    - `rejected` means dropped;
    - the queued-bytes and spool-batches help say "in memory".
  - Log table rows removed:
    - `file.name`;
    - `kaiak.data_dir`;
    - `kaiak.usage.acknowledged_batches`;
    - `kaiak.usage.next_sequence`;
    - `kaiak.usage.batch_instance`;
    - `kaiak.limit.windows`/`windows_dropped`;
    - `kaiak.data_file.*`.

    Also removed: the `last-known-good` trigger, the snapshot/totals write trigger,
    and the "new usage epoch" and "discarded limits totals" reasons.
  - Lifecycle: readiness sources; the flush loses "stays in the spool"; the drain and
    `terminationGracePeriodSeconds` lose the snapshot and totals writes.
- **`CONTROL-PROTOCOL.md`:**
  - the epoch is new at every gateway process start;
  - a batch ID is never reused within its epoch;
  - a batch the control plane refuses for good is **dropped**, not set aside;
  - the bullet on another instance's spooled batch is removed, and order is "the
    order they were sealed";
  - the ack and the batch rules lose the spool clauses;
  - status and the outage section lose last-known-good;
  - "no totals yet" loses the data directory.
- **Schemas** (`npm run sync-schemas`):
  - `status.applied_config_hash` loses "or its last-known-good copy";
  - the batch epoch is "created at every gateway process start".
- **`AGENTS.md`** (`[PROJECT]` only):
  - the hard constraint: "the gateway holds no state and writes nothing to disk";
  - Deployability: one stateless bullet with file mode for local and dev; the
    persistent-state bullet is removed;
  - Feature-Building Mode: the data-file paragraph becomes "no database, and the
    gateway writes no files".
- **Other docs:**
  - `README.md`, `kaiak.md` and `TECH-STACK.md`: Persistence becomes "none in the
    gateway" (settled 2026-10-07, pointing at `GATEWAY.md`), and the image bullet is
    updated.
  - `ARCHITECTURE.md`:
    - the deployment shape;
    - the `state` package bullet is removed;
    - `limits` and `control` updated;
    - the e2e line.
  - `gateway.html`:
    - Stateless;
    - the `state` box, its dashed edge and its marker removed;
    - the package table;
    - the config-source diagram with three sources;
    - the boot list;
    - the drain caption;
    - the backlog row.
  - `control-plane.html`: the boot list; the usage diagram ("in memory"); "Dropped";
    the memory bound; the outage table.
  - `DEPLOYMENT.md`:
    - the minimal gateway and probes;
    - the outage sizing;
    - What a pod loses;
    - the env table row removed;
    - the alerts;
    - secrets;
    - the **Optional: the data directory** section removed;
    - the Upgrades spool bullet removed;
    - **the migration note**: flush, delete the directory (its files named), remove
      the volume, unset `KAIAK_DATA_DIR`.
  - `BACKLOG.md`: "Old-epoch spool order" removed (resolved); the credentials entry
    loses the last-known-good cache.
  - `LIVE-BACKENDS.md` and the Dockerfile comment are updated.

**Decisions made in this step**

- **The word "spool" stays only in the metric names** `kaiak_usage_spool_batches` and
  `kaiak_usage_spool_records`. Their help says "in memory". Renaming metrics is not
  this removal's business; the queue they count still exists. The docs call it "the
  queue" or "queued batches".
- **A batch refused for good is dropped** (logged at error), in both specs. In memory
  there is nowhere to set it aside, and that was already the no-data-directory
  behaviour.
- **Decision 36's text went into `GATEWAY.md` here** with decision 35, so step 19 has
  one settled text for both.
- **The one dated Rejected line is in `GATEWAY.md`** (Nothing is written to disk).
  `TECH-STACK.md`'s Persistence bullet points at it rather than repeating it.

**Removal checklist status** (`git grep -l -i -E <pattern> -- . ':!docs/plans'
':!docs/reviews'`):

| Pattern | Docs | Left for |
|---|---|---|
| `KAIAK_DATA_DIR`, `DataDir`, `dataDir`, `data director`, `data dir` | done; remaining hits `GATEWAY.md` (the dated Rejected line) and `DEPLOYMENT.md` (the migration note), both allowed | step 19: `cmd/kaiak` (main, tests incl. `datadir_test.go`, `stateless_test.go`), `e2e`, `internal/{control,limits,metrics,state}`, `scripts/live/{process,config}.go` |
| `last-known-good`, `lastKnownGood`, `LastKnownGood`, `lastknowngood` | done; same two allowed hits | step 19: `cmd/kaiak`, `e2e`, `config/loader.go`, `control` (incl. `lastknowngood.go`, `messages.go`, `status.go`), `metrics/ops.go`, `state`; the sample page comment and test (`control/sample/src/page/`) |
| `totals.json`, `limits.json`, `last-known-good.json` | done; the migration note (allowed). `protocol/fixtures/config/invalid/cases.json:286` `key-with-limits.json` is a fixture name, unrelated | step 19: `e2e`, `control` (`restart_test.go`, `spool*.go`, `usage*.go`), `limits` (`persist.go`, `snapshot.go`, tests) |
| `spooldisk`, `batchStore`, the on-disk index, `usage-batch-` | done; the migration note names `usage-batch-*.json` (allowed); `usage-batch-invalid` is an error code, unrelated | step 19: `control/spool.go`, `spooldisk.go`, `usage.go`, `e2e/control_test.go` |
| `RestoreSpooled`, `RestoreOwn`, `SpoolCovered`, `saveShared`, `SaveShared`, `LoadShared`, `saveSharedPeriodically`, `persistMu`, `WriteVersioned`, `internal/state` | no doc hits | step 19: `cmd/kaiak`, `control`, `limits`, `server/usage_path_test.go`, `state` |
| `format version` / "flush before upgrading" for data files | done (the remaining "format version" hits are config and protocol versions) | — |
| `PVC`, `volumeClaimTemplates`, `emptyDir`, `StatefulSet` | done | step 19: `cmd/kaiak/stateless_test.go` |
| `restart keeps the last totals`, `rebuilt` own usage, `crash rebuild` | done | — |

**Suite** (2026-10-07, after both commits; code unchanged in this step):
- Control `npm test`: 614 tests, 613 pass, 0 fail, 1 skipped (the memory store's
  catch-up contract test, as before); `npm run lint`: `tsc` clean, boundaries ok.
- `scripts/check-gateway.sh` (uncached, race): green, gateway e2e 115.8 s, every
  package ok, live-kit self-test passed. No red: the schema changes are descriptions
  only.
- Cross-half tests: not run in this step (no code changed).
