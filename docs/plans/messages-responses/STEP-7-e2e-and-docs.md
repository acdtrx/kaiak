# Step 7 — cross-half e2e and docs

**Status:** not started

## Intent

Prove both APIs end to end across the halves, and bring the operator-facing and
architecture docs up to the new shape.

## Files likely touched

- `gateway/e2e/sample_test.go` (build tag `crosshalf`): a Messages request and a
  Responses request through a gateway against the sample. Their records reach the
  sample with the right units and count toward its totals; a token-counting request
  leaves none.
- `docs/ARCHITECTURE.md` and `docs/architecture/gateway.html`:
  - the inbound plugs now built
  - the passthrough matrix: three client APIs by backend type, diagonal built,
    translation in the backlog
  - the new types
  - the written-against line
- `docs/kaiak.md`: the scope lists (Messages and Responses inbound move in; the new
  provider types).
- `docs/DEPLOYMENT.md` and `README.md`:
  - the new endpoints and auth
  - the new types and their env variables
  - the client settings from step 6
- `control/kaiak-control/GUIDE.md`: anything step 2 left for after the gateway
  landed.
- `docs/BACKLOG.md`: confirm step 1's entries; nothing implemented stays listed.

## Decisions made during planning

- Docs describe contracts and decisions, not implementation. The architecture page
  gets the matrix and the plug list, not a description of the code.

## Acceptance criteria

- The cross-half e2e covers both APIs and passes.
- `scripts/check-all.sh` green three times in a row: **phase 4 and the plan end
  here**.
- The OVERVIEW's Verification checklist is ticked, live items marked pending where
  they wait on a tester.
- Ready for review, rebase onto `main`, ff merge. No release until
  `docs/plans/control-replicas/` lands too.

## Result

(filled in when the step is done)
