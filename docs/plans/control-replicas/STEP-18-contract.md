# Step 18 — no data directory: contract and docs

**Status:** not started

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
