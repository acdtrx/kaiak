# Step 1 — input hardening (F1, F2, F4)

**Status:** done (2026-09-25) — phase 1 ends green

## Items

- **F1** the gateway rejects duplicate object members at any depth in every JSON
  document it parses from outside (config file, control-plane snapshot and stream
  events, totals, acks, the seed): one shared detector (a token-level scan with
  `json.Decoder.Token`, no regex) before both the schema walker and the typed decode;
  new rejection code (e.g. `duplicate-member`) in the spec's code list; raw-byte
  fixtures on both halves (the TS side documents that `JSON.parse` keeps the last and
  the gateway refuses). `KAIAK_` names also refused where a backend credential is
  read (provider), not only in validation. Regression: the auditor's
  `TestAuditDuplicateConfigBypassesReservedSecret`.
- **F2** a client request body with a duplicate **top-level** member → `400
  invalid_request_error` (code e.g. `duplicate_member`), before any rewrite.
  Regression: `TestAuditRepeatedModelExpansion` (must be refused; add a bound test on
  rewritten size ≤ body + a small constant per owned field).
- **F4** `global.max_sequences_per_request` (n × max(n,best_of) × prompts on
  completions/chat; default 16) and `global.max_embedding_inputs` (default 2048), both
  halves; above → `400 invalid_value` with `param`. Slots stay per request —
  documented in GATEWAY.md and DEPLOYMENT.md (set backend batch limits accordingly).
  Regression: `TestAuditBatchMultiplicityExceedsMaxN`.

## Acceptance

Each regression fails on `0.4.0` and passes; fixtures on both halves; specs dated
2026-09-25; suite green.

## Result

- **F1** one shared detector, `schemacheck.FirstDuplicateMember` (a `json.Decoder`
  token scan; memory = the names of the objects open at the current position),
  run inside `schemacheck.Decode` after the syntax check — so before both the walker
  and the typed decode, for every document that goes through it: config file, seed,
  last-known-good config (all via `config.Parse`), snapshots / `config` events,
  totals, acks (via `control.decode`). Rejection code `duplicate-member` with the
  repeated member's pointer (config rejection code list; message decode error).
  `state.ReadVersioned` refuses a repeated member in any data-directory file (the
  LKG envelope included). `provider.Registry.credential` gives no credential for a
  `KAIAK_` name (`config.IsReservedEnvName`). Raw-byte fixtures in
  `protocol/fixtures/duplicate-members/` (7 files, `cases.json` with kind/path/reason):
  the gateway refuses each at its path (config and control packages); the kit's
  suite (`messages/duplicate-members.test.ts`) checks `JSON.parse`'s reading of each
  is valid, so the repeat is the only defect.
- **F2** `parseOwnedFields` decodes the top-level object by tokens and refuses a
  repeated member → `400 invalid_request_error duplicate_member`, `param` = the
  (clipped) name; the same for `stream_options` (the one owned object the provider
  edits — `param` `stream_options.<name>`). The editor (`editObject`) refuses a
  repeated edited key as a gateway fault instead of editing every occurrence. Bound
  test on the rewritten size. Nested repeats elsewhere pass through (tested).
- **F4** `global.max_sequences_per_request` (default 16) and
  `global.max_embedding_inputs` (default 2048) on both halves (schema + sync, walker,
  document/snapshot, TS types, valid fixtures `full.json`/`at-bounds.json`/snapshot
  `full.json`, invalid `max-*-zero.json`). Above → `400 invalid_value`; `param`
  `prompt` (more than one prompt), else `n`/`best_of` (the larger), `input` for
  embeddings. Slots-per-request documented in GATEWAY.md and DEPLOYMENT.md.
- Regression tests (each confirmed failing on the unfixed code first):
  - `config.TestDuplicateMembersCannotHideABackendFromTheSchema` (from
    `TestAuditDuplicateConfigBypassesReservedSecret`) — failed: `got <nil>`;
  - `provider.TestPassthroughRefusesARepeatedOwnedKey` (from
    `TestAuditRepeatedModelExpansion`) — failed: `120015 client bytes became 5230015`;
    plus `server.TestRepeatedTopLevelMemberIsRefusedBeforeAnyRewrite` (full handler,
    failed with 200);
  - `server.TestBatchSizeIsCappedPerRequest` (from
    `TestAuditBatchMultiplicityExceedsMaxN`) — failed: `accepted 10000 sequences`;
  - `provider.TestReservedVariablesAreNeverSentAsACredential` — failed: credential
    `gateway-token` sent.
- Changed existing tests (intended behavior changes, not weakened):
  `TestEditObjectSplicesOnlyOwnedValues` lost its "every duplicate is edited" case
  (now `TestEditObjectRefusesARepeatedEditedKey`); `TestOutputMultiplicity…` "n at a
  raised max_n" also raises `max_sequences_per_request`; `…ReservationSaturates`
  uses one prompt with `n` = 2^53 − 1 (the cap makes 4 × 2^53 − 1 unreachable; the
  reservation still saturates); `TestEveryErrorCodeHasItsClass` knows
  `duplicate_member` (class `invalid_request`).
- Suite: `GOFLAGS=-count=1 scripts/check-all.sh` → `all checks passed` (gateway race
  tests incl. e2e, live-test kit, control 472 tests + lint, cross-half e2e).

## Decisions made during the step

- The detector runs **after** the syntax check inside `Decode` (syntax first bounds
  nesting; the scan only sees valid JSON). Names compare after decoding
  (`"mod\u0065l"` = `"model"`).
- The kit does **not** detect repeats (no tokenizer, no dependency): it only ever
  writes with `JSON.stringify`; documented in CONTROL-PROTOCOL.md → Messages.
- `stream_options` repeats are refused inbound too — otherwise the provider's refusal
  would surface as a `500` for a client error.
- Sequence math reuses the existing `max(n, best_of) × prompts` (the brief's
  "n × max(n, best_of)" read as a slip — per prompt the backend generates
  max(n, best_of)); embeddings inputs reuse `promptCount` (same shape rules).
- No semantic rule tying `max_n` to `max_sequences_per_request`: a `max_n` above
  the sequence cap is simply capped by it.
- Data-directory files: `state.ReadVersioned` refuses repeats for every versioned
  file, not only the LKG copy (one place, cheap).

## Decisions for the user to confirm

- **Confirmed by the user (2026-09-25):** default `max_sequences_per_request` 16
  refuses today-valid requests such as 3 prompts × `n` 8, or 17 single prompts —
  intended, a visible change for batch clients.
- `param` choice on the sequence cap (`prompt` when several prompts, else `n` /
  `best_of`) — still open.
- **Confirmed by the user (2026-09-25):** the kit does not detect repeats (above).
- **Confirmed by the user (2026-09-25):** repeats refused in `stream_options` as well
  as at the top level (the brief said top level only).

## User confirmation

All open "Decisions for the user to confirm" in this step were confirmed by the user on
2026-09-25, as implemented.
