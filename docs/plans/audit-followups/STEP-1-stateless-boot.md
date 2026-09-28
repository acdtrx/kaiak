# Step 1 — stateless gateway and boot order

**Status:** done (2026-09-25) — phase 1 continues with step 2

## Items

- **E1** control-plane mode writes nothing without `KAIAK_DATA_DIR`: in-memory pending
  usage batches (existing bound and drop policy), no LKG, totals in memory; with
  `KAIAK_DATA_DIR` set, today's persistence (opt-in). File mode's limits snapshot the
  same way (opt-in). No data-dir lock without a data dir. The default `KAIAK_DATA_DIR`
  becomes unset.
- **E2** boot: control-plane config (bounded wait) → seed (`KAIAK_SEED_CONFIG_FILE`) →
  exit with a clear error. The seed is used when the control plane is unreachable *or*
  answers without a config (`503 config-unavailable`), never on a rejected token or a
  snapshot the gateway rejects. A seed with any priced model is a startup error; its
  credentials are checked at startup. With a data dir (opt-in), LKG keeps its place
  before the seed.
- **E3** the drain reserves its last ~10 s (configurable, bounded by the drain timeout)
  for the usage flush and the final status; streams still running are cut at
  `drain timeout − reserve`.

## Acceptance

Tests for each path (no data dir: nothing written — check the filesystem; boot with
control plane up / down + seed / down without seed → exit code; 503 → seed; rejected
token → no seed; priced seed refused; drain reserve flushes a tail batch); specs
(principle 1/2 wording in `docs/kaiak.md` updated with the dated decision).

## Result

- **E1 no disk by default.** `KAIAK_DATA_DIR` has no default. `control` gained one
  batch store interface (`spool.go`) with two stores: `diskStore` (`spooldisk.go`,
  today's spool files, unchanged behavior) and `memoryStore` (`spoolmemory.go`); the
  sealer, record checks, queue, bounds and sender are shared. Without a data dir:
  fresh epoch per process, queued records bounded at 10 000 (oldest queued batches
  dropped except the outstanding one; log + `kaiak_usage_dropped_records_total{reason="memory_bound"}`),
  no LKG read/write, no totals cache, no limits snapshot (file mode), no lock.
  Tests: `control/memory_test.go` (delivery from memory, fresh epoch, bound, no
  LKG); `cmd/kaiak/stateless_test.go` `TestRunWithoutADataDirectoryWritesNothing`
  (both modes, temp cwd stays empty) and `TestDataDirectoryHasNoDefault` — both red
  before the change (`files written with no data directory: [data]`, `data
  directory "data" by default`).
- **E2 boot order.** `Client.Boot` returns an error; `run` exits on it before
  binding. Order and table: GATEWAY.md → Control-plane mode → Boot. Seed checked at
  startup with `config.Check` (schema + semantic + credentials, N-P4) and refused
  when any model is priced. Status `ready` derives from the config in force
  (`Applier.Loaded`), so a seed boot reports ready with no version (N-P11). Tests:
  `control/seed_test.go` (down → seed, 503 → seed, 500 → seed, 401 → exit, rejected
  snapshot → exit, no source → exit message, LKG before seed, status after seed);
  `cmd/kaiak` `TestRunExitsWithNoConfigAtBoot` (red before: run never returned),
  `TestSeedIsCheckedAtStartup`, `TestRunBootsFromTheSeedWithTheControlPlaneDown`.
- **E3 drain flush reserve.** `KAIAK_DRAIN_FLUSH_RESERVE_MS` (control-plane mode):
  the drain's request phase runs `timeout − reserve`; the flush deadline stays
  grace + timeout. e2e `TestDrainReserveDeliversTheCutRequestsUsage` (no data dir):
  a stream finishing during the drain is delivered by the 5 s seal while another
  still runs; the hanging stream is cut at timeout − reserve and its partial record
  is counted by fakecontrol before exit. Red with the reserve disabled (`no counted
  record of e2e-drain-cut`).
- **e2e** (`gateway/e2e/stateless_test.go`): `TestMinimalControlPlaneSetup` (only
  `KAIAK_CONTROL_URL` + `KAIAK_CONTROL_TOKEN`, plus loopback listen addresses so test
  runs never collide on 8080/9090; read-only cwd: boots, serves, usage counted,
  drain flush, statuses ready/draining, writes nothing), `TestNoConfigAtBootExits`
  (non-zero, message, within the 5 s boot wait), `TestSeedServesWithTheControlPlaneDown`,
  `TestPricedSeedFailsTheStart`. The harness leaves `KAIAK_DATA_DIR` unset when a
  test passes no data dir; persistence tests keep theirs.
- **Docs**: `docs/kaiak.md` principles 1–2; GATEWAY.md (minimal gateway, seed,
  data dir, listeners, drain reserve, Boot table, LKG, usage batches in memory,
  spool, status, Limits restart/snapshot notes, metrics, Lifecycle);
  CONTROL-PROTOCOL.md (usage batches, status `applied_config_version` null while on
  the seed, outage); ARCHITECTURE.md; AGENTS.md Project Facts + Deployability;
  TECH-STACK persistence; README step 4; DEPLOYMENT.md env table rows only (step 2
  rewrites the guide).
- **Suite**: `GOFLAGS=-count=1 scripts/check-all.sh` → `all checks passed` (gateway
  gofmt/vet/staticcheck/race tests incl. e2e, live kit self-test, control tests +
  lint, cross-half e2e).

## Decisions (recorded in the specs, dated 2026-09-25)

- Seed on **unavailable** = connection failure/timeout, cut body, missing or other
  `Kaiak-Protocol`, **any 5xx** (not only `503 config-unavailable`: `500
  internal-error` is the control plane failing too). Exit on any 4xx (401 token,
  a wrong path answered by a kaiak control plane) and on a snapshot message the
  rules refuse.
- Rejected snapshot: LKG (with a data dir) as before, else **exit** with the version
  and codes — never the seed.
- Flush reserve: default 10 s but at most half the timeout when unset; explicit
  value must be ≤ timeout (equal = cut at once, whole timeout to the flush), above →
  startup error (no silent clamp). File mode ignores it.
- The memory bound keeps the outstanding batch (it may be on the wire).
- `config_not_loaded` stays as the admission stage's guard, unreachable from the
  binary (listeners bind only with a config); `starting` is never reported.

## Decisions for the user to confirm

1. Any 5xx at boot → seed (beyond the brief's 503).
2. A rejected snapshot with no LKG → exit rather than seed.
3. Reserve default capped at half the timeout; explicit > timeout is a startup error.
4. Missing/other `Kaiak-Protocol` counts as unavailable (seed), as before.

## Handoff to step 2

- `gateway/Dockerfile` still sets `ENV KAIAK_DATA_DIR=/data` (and ships `/data`):
  the image does **not** yet give the minimal stateless setup — on a read-only root
  it would fail to create/lock `/data`. Remove the ENV and the directory; make
  `scripts/smoke-images.sh` set `KAIAK_DATA_DIR` where it checks persistence (and
  add a read-only, no-volume run). README's container section and DEPLOYMENT.md
  still describe the StatefulSet/volume model.
