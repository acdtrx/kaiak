# Plan: structure

## Goal

Make future changes cheaper and the codebase more solid, from the structure review of
2026-10-07 (`docs/reviews/2026-10-07-structure/STRUCTURE.md`). Many plans landed in
sequence, each on the shape the previous one left. This plan reshapes what that left:

- **Vocabulary spread:** a domain concept restated wherever it is used, so a new value
  edits 7–10 places in lockstep (limit types, usage units, attempt outcomes,
  endpoints, backend types).
- **The control ↔ limits seam glued in `main.go`**, whose test copy has already drifted.
- **Leftovers of removed features.**
- **One job split by arrival order** (per-attempt state on the request, the expiry
  sweep in `gateways`, per-format stream rules over three files).
- **Test scaffolding** that makes the next test expensive or lets it fail silently.

No module is rewritten: the subsystem cuts stay. Most steps change no behaviour; the
few that do are listed under Decisions.

## Scope

All eleven packages of the review's Recommended order, plus the findings each step
names (the review's module reports hold the detail: file, line, current shape,
proposed shape). Findings are cited as `<report> <ID>`, e.g. `server S1` →
`docs/reviews/2026-10-07-structure/modules/gateway-server.md`, finding S1.

## Out of scope

- **The metrics-side findings** — observability F3 (except the limit-type list, step 11),
  F4, F5, F8 and T12. They go to the OTel metrics export (`docs/BACKLOG.md` →
  OpenTelemetry export → Metrics), which follows this plan so each metric changes once.
  This plan touches `metrics` only where a list it copies goes away (limit types, retry
  reasons), and **no metric name, label or value changes**.
- **What the review says to leave**, for its stated reasons: the hand-written schema
  walker; the `kaiak-control/schema/` copy; the hand-mirrored TS message types; merging
  provider's and accounting's stream readers (observability F9); one shared JSON
  tokenizer (provider F7); stream sequencing moved into the `kaiak-control` core
  (control-core F8, edges F7); `window.go` split by kind; one owner for "this
  gateway's shares" (with *Demand-weighted shares* or *Live count excludes draining
  gateways*).
- **One batch number** (independent F07): investigate only.
- **The outage decision moved into the control client** (routing-limits F10): step 15's
  `LimitsContact()` removes the adapter it was about; placement alone does not pay.
- **A library-owned limit-usage view** (edges F3(b)): a new public `kaiak-control` API
  with one caller (features earn their place). Step 8 takes F3(a).
- **A shared SSE writer for the sample page** (edges F8, sharing): the page feed is fixed
  in place instead (step 25).

## Decisions

Settled with the user (2026-10-07 and 2026-10-08; `STRUCTURE.md` → Decisions):

1. **Provider modules stay full per-type providers.** Provider F3 is rejected; only
   shared helpers the modules call (provider F2).
2. **Metric label lists are owned by the code that produces the values.** The metrics
   side is the OTel metrics plan (Out of scope).
3. **`GUIDE.md` is trimmed** to what only it says (step 26).
4. **Status `starting` is removed** from the protocol on both halves, no version bump
   (step 8).
5. **Outbound-only Go validators move into `_test.go`** (step 7).
6. **Embeddings and completions stop type-checking output-limit keys they ignore**
   (step 23).
7. **Every package is in scope**, test support and the endpoint table included.

Made while planning (confirm in review):

8. **Beyond the eleven packages**, these findings are in, because each makes a later
   change cheaper: server S6 (error class set by the constructor), S8 (stage
   applicability), provider F5 (split `wire.go`), F8 (SSE errors in the reader),
   routing-limits F8 (one counter store), F11 (model check in `provider`), control-core
   F5 (`BatchCursors.latest`), test-scaffolding F6–F9, and the small items and stale
   comments the reports list. Each step names its own.
9. **No protocol version bump.** Removing `starting` changes the schema on both halves,
   but no current gateway sends it. The new shared fixtures (backend types, counted
   units) add test contract, not wire contract.
10. **Visible behaviour changes**, the only ones:
    - `starting` is gone from the protocol (step 8);
    - `kaiak.upstream.error.code` is no longer carried from a failed attempt onto a
      request a later attempt answered (step 9);
    - a Messages message object with a repeated member is refused with `400`, like
      every sibling object (step 23);
    - embeddings and completions stop type-checking `max_tokens` /
      `max_completion_tokens` they ignore (step 23);
    - `azure-openai`'s model check logs the "cannot tell which models it serves" info
      line, as `azure-anthropic` does (step 21);
    - the sample page no longer crashes on a write after `end()` at shutdown (step 25).
11. **Public `kaiak-control` API changes are allowed** (feature-building mode): `totals`
    and `countedThrough` go (step 8); store window starts become a record keyed by
    limit type (step 13); `BatchCursors.latest` goes (step 24);
    `startExpirySweep`/`stopExpirySweep`, `configHash`, `libraryName` go (step 24). No
    external store exists yet.
12. **`control` imports `limits` types** (step 15), the `accounting.UsageRecord`
    precedent: `limits` stays unaware of `control`, and the glue leaves un-importable
    `main`.
13. **Boot keeps one stream** (step 17) only if handing an open stream to `Run` stays
    simple: the step starts with a spike and stops to report if it does not.
14. **Config types carry the `/v1/models` JSON tags** (step 14, config F4): that metadata
    passes through unchanged by contract, so the coupling is real.
15. **Regression tests are filed by subject**, not by review round (step 5): the
    `round-N`, `review` test files fold into the files of what they test.
16. **Removal discipline.** A removed field, method, type or protocol value is gone: no
    rename, alias or compatibility path, and tests that assert removed behaviour are
    deleted, not adapted. Each removal step carries a grep checklist that must be clean
    at its phase end.
17. **Behaviour is pinned by the existing tests.** A step that means to change nothing
    leaves every existing assertion standing (moved or renamed with its subject is
    fine); a failing test is a finding to report, never a test to weaken.

## Constraints

- Zero third-party Go dependencies; no new npm dependency.
- The gateway ↔ control-plane protocol, the client API, config format, log vocabulary
  and metric names stay as they are, except decision 10.
- Protocol changes land on both halves in one step (`CONTROL-PROTOCOL.md`, `protocol/`,
  the gateway, `kaiak-control`).
- Specs change in the same step as the contract they describe; implementation-only
  changes need no doc edit.

## Risks

- **Breadth.** About thirty steps over both halves; a regression could hide in a
  mechanical move. Mitigation: phases end green, each step runs the suite and records
  it, steps that mean no behaviour change keep every assertion.
- **Hot paths** (routing's eligibility walk, the attempt loop, the relay). Mitigation:
  the dispatch bench before and after step 27; the stream-format step keeps one decode
  per event at most.
- **Boot hand-off** (step 17, medium confidence). Mitigation: spike first; the boot and
  seed tests count streams and exits.
- **One counter store** (step 18, medium confidence). Mitigation: `shared_test.go` stays
  valid unchanged in what it asserts.
- **Test restructuring hides lost coverage** (phase 1). Mitigation: count tests before
  and after each test step; every test moved, none dropped without a named reason.
- **Drift between plan and code**: line numbers in the reports date from `034329e`.
  Each step re-reads the code first; where the report and the code disagree, the code
  wins and the step says so.

## Tag

`v0.11.1` on `main`, "before the structure plan", immediately before step 1 starts.

## Branch and worktree

Branch `structure`, worktree `.claude/worktrees/structure`. It rebases onto `main`
before the ff merge. Steps are implemented by subagents (the same model as the main
session), one step per brief; the main session reviews each against its acceptance
criteria and commits at the step boundary.

## Phases and steps

- **Phase 1 — Test support** (steps 1–5). Tests that fail loudly and are cheap to write,
  before the phases that edit them. Green at the end.
  1. `STEP-1-server-harness.md` — one gateway test harness with options; config edits
     that fail on a missing anchor; one key-hash helper.
  2. `STEP-2-fakes.md` — fakebackend fault shape and captures; fakecontrol dead surface;
     one fake OTLP collector.
  3. `STEP-3-e2e.md` — one backend-type table and one passthrough scenario; one process
     and one wait primitive in the harness.
  4. `STEP-4-fixtures.md` — one fixture runner per half; the backend-type fixture; the
     config-enum and schema-defaults pins.
  5. `STEP-5-control-test-support.md` — `kaiak-control` test support; regression tests
     filed by subject on both halves.
- **Phase 2 — Leftovers and attempt classification** (steps 6–10). Removed features
  leave no trace; one owner for an attempt's result. Green at the end.
  6. `STEP-6-gateway-leftovers-control-limits.md` — control and limits/routing leftovers.
  7. `STEP-7-gateway-leftovers-config-accounting.md` — config, accounting and server
     leftovers; the endpoint-memory prune; outbound validators into `_test.go`.
  8. `STEP-8-starting-and-control-leftovers.md` — `starting` off both halves;
     `totals(instance)` gone; the sample's unused entry.
  9. `STEP-9-attempt-result.md` — per-attempt state on the attempt; one classification
     and a rule table.
  10. `STEP-10-attempt-loop-shape.md` — error class set by the constructor; the attempts
      stage as a struct; stage applicability; `upstream.go` split.
- **Phase 3 — Limit types, units and the config pipeline** (steps 11–14). One table per
  vocabulary; one validation pipeline. Green at the end.
  11. `STEP-11-limit-type-table.md` — one limit-type table in `config`, read everywhere.
  12. `STEP-12-unit-sets.md` — named unit sets; no positional unit constructor.
  13. `STEP-13-counted-units-both-halves.md` — the counted-units fixture; window starts
      keyed by limit type; one window identity.
  14. `STEP-14-config-pipeline.md` — `schemacheck.Validate` + `DecodeTyped`; one model
      metadata type; `Load.Snapshot`.
- **Phase 4 — The control ↔ limits seam and `main.go`** (steps 15–19). Glue lives in the
  packages it connects. Green at the end.
  15. `STEP-15-control-limits-seam.md` — `control` speaks `limits` types; adapters and
      their drifted test copy gone.
  16. `STEP-16-hurry-context.md` — "hurry" as a context.
  17. `STEP-17-boot-one-stream.md` — one stream per boot. **Not taken** (user,
      2026-10-08): the hand-off did not stay simple (decision 13).
  18. `STEP-18-limiter-counters.md` — one counter store; counters carry what they need.
  19. `STEP-19-main-split.md` — `main.go` split by job; `Client.Finish`.
- **Phase 5 — Endpoints, provider and inbound** (steps 20–23). One place per endpoint
  and per format. Green at the end.
  20. `STEP-20-provider-formats.md` — one stream-format type per API; shared module
      helpers; `wire.go` split; SSE errors in the reader.
  21. `STEP-21-model-check.md` — "cannot tell" said once; the model check in `provider`.
  22. `STEP-22-endpoint-table.md` — one endpoint table keyed by `provider.Endpoint`.
  23. `STEP-23-inbound.md` — one parse prologue; output-limit keys from the table; two
      JSON-walk helpers.
- **Phase 6 — `kaiak-control` composition, routing and accounting** (steps 24–29). The
  last local tidy-ups, the GUIDE, and the plan's end. Green at the end.
  24. `STEP-24-control-core.md` — the sweep in the core; one listener helper; stores
      without `latest`; small items.
  25. `STEP-25-control-edges.md` — backend-verify type table; the sample page feed and
      verify CLI.
  26. `STEP-26-guide.md` — `GUIDE.md` trimmed.
  27. `STEP-27-routing-internals.md` — one eligibility pass; circuit transitions emitted
      once; rejection log attrs from `limits`.
  28. `STEP-28-accounting.md` — one inclusive-token rule; the log-export count mirror.
  29. `STEP-29-review-and-green.md` — an independent review of the branch, its fixes,
      the review's outcome table, `check-all.sh` green three times.

Expected reds inside a phase are named in each step's Result, with the step that clears
them.

## Verification

- Each step: `scripts/check-all.sh` (or the half it touches, when the step says so),
  recorded in its Result.
- Each phase end: `scripts/check-all.sh` green, committed.
- Removal steps: their grep checklists clean at the phase end.
- Behaviour fixes: each has a test that fails before it (decision 10).
- Hot paths: `dispatch_bench_test` before and after step 27, recorded.
- Live: after phase 5, the live-test kit against `vllm` and `llama-server` on the DGX
  (`docs/testing/LIVE-BACKENDS.md`), restoring the DGX's model afterwards.
- Plan end: an independent review of the branch diff (step 29), its findings fixed or
  recorded; `scripts/check-all.sh` green three times in a row; every review finding in
  scope marked done with its step in `STRUCTURE.md`'s outcome table.

**Verification status:** not started.
