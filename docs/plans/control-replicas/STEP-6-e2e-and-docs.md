# Step 6 — end to end and docs

**Status:** not started

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

(filled in when the step is done)
