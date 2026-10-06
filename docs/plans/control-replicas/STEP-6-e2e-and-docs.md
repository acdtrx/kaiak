# Step 6 — end to end and docs

**Status:** done (2026-10-06)

## Intent

Prove two real gateways following two control-plane cores over one store, and
rewrite the docs that tell host apps to run one process.

## Files likely touched

- `control/sample/`: `KAIAK_SAMPLE_PROTOCOL_PORTS`, a list of ports. Each port gets
  its own core over the one memory store, behind its own Fastify listener. The page
  and file watching stay on the first.
- `gateway/e2e/sample_test.go` (`crosshalf`): two gateways, each on its own sample
  port. Usage from both counts once, and both converge on the same totals; a config
  change reaches both. One sample port restarting does not disturb the gateway on the
  other.
- `control/kaiak-control/GUIDE.md`:
  - §2 hard rules: rule 1 rewritten — one process or many, depending on the store.
  - §4: no lease options.
  - §5: the new methods and their guarantees; the Postgres sketch with each
    conditional write and LISTEN/NOTIFY; running the exported contract tests.
  - §10: replicas behind a load balancer; rolling updates now fine.
  - §12: the "two replicas is refused" pitfall replaced by "a store without
    conditional writes or notification".
  - The written-against line.
- `docs/architecture/control-plane.html`, `docs/ARCHITECTURE.md`,
  `docs/DEPLOYMENT.md`: the replica shape.
- `README.md` if it mentions the single process.

## Decisions made during planning

- **The sample stays a single process.** Several ports over one in-memory store is a
  demonstration and a test harness, not a deployment pattern. The GUIDE says so.

## Acceptance criteria

- The cross-half e2e with two cores passes. `scripts/check-all.sh` green three times
  in a row: **phase 2 and the plan end here**.
- The OVERVIEW's Verification checklist ticked.
- Ready for review, rebase onto `main`, ff merge. Release only with
  messages-responses (pushing a tag to `github` is a release: only when the user
  asks).

## Result

**What changed**

- **Sample** (`control/sample/`): `KAIAK_SAMPLE_PROTOCOL_PORTS`, a comma-separated
  list of ports on the `KAIAK_SAMPLE_LISTEN` host (`0` picks a free one). Each is a
  protocol replica: its own core over the app's one in-memory store, in a Fastify
  instance of its own serving only the gateway endpoints, logging `sample protocol
  replica N listening on <url>`. The page and the config file stay with the main app;
  SIGTERM closes every listener. Tests: settings (parsing, refusals), the app (a batch
  posted to one replica and resent to the main app counted once, totals read through
  another replica, a reload reaching both replicas' cores), main (three listeners, one
  epoch behind all of them).
- **Cross-half e2e** (`gateway/e2e/replicas_test.go`, `TestAcrossHalvesReplicas`):
  the sample with one replica, gateway gw-a on the main core and gw-b on the replica,
  each behind its own proxy (a load balancer). Subtests: both cores show two live
  gateways; usage through both counts once, the hourly token total exact on both
  cores' totals; a config published through the main core's file reaches gw-b on the
  replica; gw-b's proxy moves it to the main core and breaks its stream — it
  reconnects and resumes (its usage counted, the next published version applied),
  neither gateway logs a resync or a refused epoch, gw-a's stream never breaks.
  `startSampleReplicas` in `sample_test.go` starts the sample with replicas.
  `scripts/check-all.sh` runs `^TestAcrossHalves` (both tests); AGENTS.md's command
  line says so.
- **GUIDE**: written-against line; §2 rule 1 (how many processes is the store's
  choice; memory store: one) and rule 2 (no store write behind the core); §4 several
  cores = several `createControlPlane`, the sample's replicas a demonstration; §5 the
  contract's four guarantees, the method table as it is now (`publishConfig`,
  `saveCountedBatch`, `totalsSnapshot`, gateway revisions, `forgetGateways`,
  `subscribe`), windows without models, the Postgres sketch with each conditional
  write and `pg_notify` / `LISTEN`; §9 totals matched by scope and type (the removed
  `limitIdentity`); §10 replicas and rolling updates; §11 running
  `storeContractTests` with `attach`; §12 the new pitfall (replicas over a store
  without the contract), the unsorted-`models` pitfall removed.
- **Docs**: `README.md` (the store's contract, the new sample variable),
  `docs/ARCHITECTURE.md` (deployment shape, `storage` and `control-plane` package
  notes, the replicas e2e), `docs/architecture/control-plane.html` (ordering by the
  store's sequence, totals per scope, the inside-the-control-plane figure and caption,
  footer), `docs/DEPLOYMENT.md` (deployment shape, the sample's one process, the new
  variable, `child_defaults` by type, upgrade notes: limits without `models`,
  `limits.json`/`totals.json` format 3), `docs/kaiak.md` (Limits: one per type per
  scope, every model).

**Flake hunt.** The failure seen once after step 5 (`FAIL kaiak/e2e 91.203s`, log not
kept) did not recur. On a detached worktree at `5f59dcc` (step 5's commit), logs
kept: `scripts/check-all.sh` 5 runs, `go test -race -count=3 ./e2e/`, and
`go test -race -count=3 -tags crosshalf -run TestAcrossHalves ./e2e/` — all green.
On this step's branch: `go test -race -count=2 ./e2e/` green (213 s), and the three
final `check-all.sh` runs below. With the main session's 10 green runs after the
failure, that is the gateway e2e uncached 6 times and the cross-half e2e 21 times
with no failure; no mechanism could be proved without the failing log, so nothing was
changed for it. If it recurs, the log names the test.

**Suite** (2026-10-06): `scripts/check-all.sh` three times in a row, all "all checks
passed" (cross-half e2e, both tests, 69.5 s, 79.3 s, 69.2 s); control `npm test`
596 pass, 0 fail; lint ok. **Phase 2 and the plan end green.**
