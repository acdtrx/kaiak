# Plan: request outcomes for the control plane

## Goal

Let an app built on `kaiak-control` see, per key and group, which requests failed
and which were refused — today it sees only usage. Two kinds of request are
invisible:

1. **A request that reached a backend and failed** (a backend `5xx`, a timeout, a
   wrong path) sends a usage record, but nothing on it says it failed: a backend
   error settles as zero units with neither flag, the same as a success that used no
   tokens.
2. **A request the gateway refused before routing** (an unknown key, an unknown or
   forbidden model, an invalid or oversize body, a limit, a full queue) sends
   nothing at all.

The management app hit both (`docs/BACKLOG.md`, Observability → Errors and refusals
per team, 2026-10-01).

## Scope

- **Part 1 — the record says how its request ended**: two fields on every usage
  record, the HTTP `status` the client got and the gateway's `error_code`.
- **Part 2 — refusals are counted**: each usage batch carries **refusal counts** —
  how many requests were refused, per key, model and code, over the batch's span.
- Both halves at **protocol version 4**: schemas, fixtures, the gateway filling them,
  `kaiak-control` accepting, storing and exposing them, the sample showing them.
- Contract docs, the GUIDE (what a host's store must keep), DEPLOYMENT.

## Out of scope

- Group or key labels on the gateway's Prometheus error metrics (the backlog entry's
  other option): the app's need is on the control-plane side. The backlog keeps
  that part.
- One record per refused request (request ID, time): counts are enough for now
  (the user, 2026-10-01).
- Refusals counting toward limits or budgets: a refusal used nothing.
- Alerting or dashboards in the sample beyond showing the new data.

## Decisions

Settled with the user (2026-10-01):

1. **Both parts, one protocol version.** Part 1 alone would show failures but not
   refusals; the app needs both.
2. **Refusals as counts**, per key, model and code, inside the usage batch — riding
   its exactly-once delivery — not a record per refusal: a client hammered with
   `429`s would otherwise multiply the usage traffic.

Made while planning (confirm in review):

3. **Record fields**: `status` (integer, the HTTP status the client got) and
   `error_code` (string, absent when the request succeeded) — the same values the
   gateway's request log line carries (`status`, `error_code`), so the log and the
   record never disagree. A record for a **retried attempt** (sent in full,
   unanswered — `GATEWAY.md`, usage across attempts) never answered the client: it
   carries the attempt's failure as `error_code` and no `status`. A stream cut after
   its `200` (client gone, backend failed mid-stream) keeps `status` 200 with the
   error code the log line gives it.
4. **What a refusal count holds**: `code` (the gateway's error code), `key_id` and
   `groups` (the key's path, as records carry it) — both absent when the request had
   no valid key — `model` (the public name) only when it names a configured model —
   so a client cannot grow the counts with made-up names — and `count`, `first_time`,
   `last_time`. Every error answer that leaves no usage record is counted, on every
   API endpoint (the model endpoints and unknown paths included); nothing else is.
5. **Bounds**: at most 1 000 refusal counts per batch; the gateway seals a batch when
   it reaches that, as it does at 500 records. A batch may carry records, refusal
   counts or both, never neither.
6. **The control plane** validates them with the batch, counts nothing from them
   (totals and budgets are unchanged), and hands them to the store with the batch:
   `CountedBatch` gains `refusals`. Duplicate batches are dropped as today, so a
   count is stored once. The memory store keeps the most recent ones, bounded like
   its records, and `kaiak-control` exposes them as `recentRefusals()` beside
   `recentRecords()`.
7. **A host's store must keep them** — the store interface changes, so an app with
   its own store (the management app's database store) adds a place for refusal
   counts and the two record fields. The GUIDE says what to keep and gives the
   Postgres sketch.
8. **Versions**: protocol 4 (both halves refuse 3); the usage spool format bumps
   (records and batches change), so a gateway with a data directory discards a
   spool of the old format — flush before upgrading (`AGENTS.md`, Feature-Building
   Mode). The config format, the last-known-good config and the totals cache are
   untouched.

## Constraints

- Protocol changes land on both halves at once (`AGENTS.md` → Project-Specific Rules).
- Nothing sensitive leaves the gateway: codes, statuses, key IDs and public model
  names only — no prompt, no backend text, no client-supplied model name that is
  not configured.
- No blocking control-plane I/O on the request path: counting a refusal is an
  in-memory increment, delivered with the batches.
- The gateway stays free of third-party dependencies; `kaiak-control` adds none.

## Risks

- **The management app's store** must change in step with the upgrade: an app whose
  store ignores `refusals` loses them silently. Mitigation: the store interface's
  type makes the field required, the GUIDE section is explicit, and the release notes
  say so.
- **Unbounded variety** of refusal counts (many keys refused at once): bounded by
  the 1 000-per-batch cap and the sealing rule; requests without a valid key share
  one count per code.
- **Record and log drift**: the record's `status`/`error_code` and the log line's
  must stay the same values. Mitigation: one source in the gateway for both, and a
  test that compares them.

## Tag

Tag `main` right before step 1 begins (`AGENTS.md` → Git): `v0.8.2`, "the world
before request outcomes". Local only.

## Phases and steps

Branch `request-outcomes`, worktree `.claude/worktrees/request-outcomes`.

- **Phase 1 — request outcomes, end to end** (steps 1–4). Green at the end.
  1. `STEP-1-contract.md` — specs, schemas, fixtures, protocol 4.
  2. `STEP-2-kaiak-control.md` — accepting, storing and exposing them; the store
     interface; GUIDE.
  3. `STEP-3-gateway.md` — the record fields, refusal counting, sealing, spool
     format, protocol 4.
  4. `STEP-4-e2e-sample-and-docs.md` — cross-half e2e, the sample showing them,
     DEPLOYMENT, BACKLOG.

Expected reds inside the phase: after step 1 both halves fail the new fixtures and
the protocol-version checks (step 2 clears kaiak-control's, step 3 the gateway's);
the cross-half e2e stays red until both halves speak protocol 4 (step 3).

## Verification

- Shared fixtures: valid records with and without `status`/`error_code`; valid
  batches with records only, refusals only, both; invalid: neither, more than 1 000
  refusal counts, a refusal count without `code` or with `count` 0, unknown fields —
  the same verdicts from both halves.
- Gateway: per ending (success, backend `5xx`, timeout, wrong path, client gone
  mid-stream, retried attempt), the record's `status` and `error_code` equal the log
  line's; per refusal kind (unknown key, unknown and forbidden model, body too large,
  limit, queue full, unknown path), one count with the right key, groups, model and
  code; counts merge within a batch and seal at 1 000.
- `kaiak-control`: refusals stored once per batch (a duplicate batch stores
  nothing), totals unchanged by them, `recentRefusals()` bounded.
- Cross-half e2e: a failed request and refused requests reach the sample's store
  with the right fields.
- `scripts/check-all.sh` green at the phase end.

**Verification status:** not started.
