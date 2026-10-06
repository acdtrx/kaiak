# Step 7 — cross-half e2e and docs

**Status:** done (2026-10-06)

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

**What changed**

- Cross-half e2e (`gateway/e2e/sample_test.go`, `crosshalf`): a new subtest, *Messages
  and Responses answers settle at the sample; token counting leaves no record*. The
  config gains the working fake backend again as a `llama-server` backend `ls` (that
  type serves all four new endpoints) and a model `agent` on it. The fake backend
  reports 100 prompt tokens (30 plain, 40 read from the cache, 30 written) and 5 out
  in each API's own shape; a Messages request on gw-a and a Responses request on
  gw-b, each streamed and not, must reach the sample with exactly `tokens_in` 30,
  `tokens_cached` 40, `tokens_cache_write` 30, `tokens_out` 5, `tokens_reasoning` 0,
  on `ls`, not estimated or partial, and add 65 tokens each to the sample's hourly
  token total. `count_tokens` (gw-a) and `input_tokens` (gw-b), sent first on the
  same gateways, must leave no record in any accepted batch.
- `gateway/e2e/proxy_test.go`: the control proxy also keeps the records of every
  usage batch the sample accepted (2xx), by request ID — `record`, `waitRecord` — so
  the test reads what the sample received, as sent.
- `docs/ARCHITECTURE.md`: data flow (three client APIs, passthrough only, the new
  backends), the client-API format per endpoint in the pipeline, the provider and
  accounting packages per format, the fake backend's formats and captures.
- `docs/architecture/gateway.html`: listeners and stages (three APIs, `x-api-key`,
  no defaults, the refusals, `store: false`), the provider row, the plugs figure
  (clients, providers and backends boxes), the seams table (① filled with the three
  formats, next plug translation; ③ with the two new types; ⑥ endpoint support, no
  sticky routing), the passthrough matrix redrawn — three client APIs × five backend
  groups, the same-API diagonal built, everything else translation in the backlog —
  and the written-against line. Every SVG parses.
- `docs/architecture/control-plane.html`: `Kaiak-Protocol: 5`, the written-against
  line.
- `docs/kaiak.md`: model metadata without default sampling parameters (request
  defaults are the backends'); v1 scope gains Messages, Responses and the two
  Anthropic types; "out of v1" names translation; stateful Responses and hosted
  tools recorded as kept out of the gateway (no backlog entry).
- `docs/DEPLOYMENT.md`: a **Clients** section (three APIs, one key either header,
  passthrough only and what reaches what, stateless Responses, hosted tools refused,
  Claude Code and Codex settings, `upstream_endpoint_missing`); a **Claude
  (Anthropic and Microsoft Foundry)** section (types, URLs, credentials, Messages
  only, standard price only and pricing, no client headers, Foundry without a models
  list, the live kit before go-live); the backend-type bullet (endpoints by type,
  `openai-compatible` loses Messages and Responses, tier on Responses); "Model
  defaults" replaced by "Request defaults are the backend's"; the example's two Qwen
  models differ by output limits; the topology shows Claude; the wrong
  model/path/credential alert also matches `endpoint_missing`.
- `README.md`: the three client APIs and the new providers, version 5, a Client APIs
  concept, the new types in the quick start, a Messages `curl` example, the live-kit
  row.
- `control/kaiak-control/GUIDE.md`: the standard tier also covers Responses requests.
- `docs/testing/LIVE-BACKENDS.md`: one stale "as defaults" wording → `-chat-params`.
- `docs/BACKLOG.md`: checked — the two inbound entries are gone (step 1), nothing
  implemented is listed; the llama-server `verifyBackend` entry loses its
  candidate-`defaults` half (model defaults no longer exist).
- `OVERVIEW.md`: Verification status added.

**Inconsistencies found**

- The OVERVIEW's decision 29 gives "new types, no `defaults`" as config format 5's
  reason; the specs (rightly, per the 2026-10-01 rule that types bump no version)
  bump it for the removed `defaults` alone. The specs are authoritative; the OVERVIEW
  line is left as it was planned.
- Removing model defaults removes the way to make two public models from one set of
  hosts differ in template parameters (the example's `qwen3-32b` and
  `qwen3-32b-thinking` were thinking off/on); they now differ in output limits only,
  and clients send `chat_template_kwargs` themselves. The docs say so; it is the
  decision's consequence, not a defect.

**Suite**

- `scripts/check-all.sh` three times in a row, all `all checks passed`: gateway checks
  (gofmt, vet, staticcheck, race tests, the live kit's lint and self-test for every
  kind), control `npm test` 576/576 and lint, cross-half e2e `ok kaiak/e2e` 49.0 s,
  53.8 s, 53.8 s. The gateway's race tests came from Go's test cache in those runs
  (no untagged Go file changed in this step), so they were also run once uncached:
  `go test -race -count=1 ./...` — every package `ok` (e2e 111 s).
- The new subtest failed once while being written: its request IDs held `=`,
  outside the IDs the gateway keeps (`[A-Za-z0-9._:-]`), so the records carried
  generated IDs; the IDs were fixed, not the check.
