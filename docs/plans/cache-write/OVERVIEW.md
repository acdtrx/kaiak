# Plan: cache-write unit

## Goal

Price the prompt tokens a backend writes into its cache at their own rate. Azure
OpenAI bills them above plain input: gpt-5.6 and later charge 1.25× input for a write
and 0.1× for a read (LiteLLM's `cache_creation_input_token_cost`). Today the gateway
reads only `cached_tokens`, so written tokens fall into `tokens_in` and are charged at
the input rate — about 25% under on the uncached input of long prompts, which is
almost all written.

Checked against Azure (gpt-5.6-luna through the deployed gateway, 2026-10-02): a
first call with a 2036-token prompt reported `prompt_tokens_details.cache_write_tokens`
2033 and `cached_tokens` 0; the repeat reported 0 and 2033. Writes need no opt-in
(any uncached prompt over ~1k tokens), both counts sit inside `prompt_tokens`, and
the streamed usage chunk carries the field too.

## Scope

- A fifth token unit, `tokens_cache_write`, in usage records, prices, limits, totals
  and metrics.
- The gateway meter reads `prompt_tokens_details.cache_write_tokens`, for every
  backend type (the usage report is the OpenAI format; backends that omit the field
  count 0).
- Both halves validate the new price unit and the new record shape.
- Contract docs, schemas, fixtures, examples, the live-test kit, the `kaiak-control`
  GUIDE (LiteLLM mapping), the sample control plane's usage table.

## Out of scope

- Splitting writes by cache lifetime (5 minutes, 1 hour), as Anthropic and Bedrock
  price them: Azure reports one count, and Bedrock is deferred (settled 2026-10-02).
- Bedrock's `cacheWriteInputTokens` and any other provider's field: the Bedrock
  provider is still in the backlog.
- `backend-verify` checking that a backend reports cache writes: nothing asks for it.

## Decisions

Settled with the user (2026-10-02):

1. **Build it now.** The backlog entry's revisit trigger is met: a deployed model's
   price list charges cache writes, and the backend reports them.
2. **Written tokens count toward the input size that picks a price tier.** They are
   input; the providers' thresholds count the whole prompt.
3. **One unit, not split by cache lifetime** (see Out of scope).
4. **An unpriced `tokens_cache_write` is charged at the tier's `tokens_in` price**,
   as an unpriced `tokens_cached` is: leaving it out must never make written input
   cheaper than plain input.

Made while planning (confirm in review):

5. **Name:** `tokens_cache_write` (the backlog's name). Units stay disjoint:
   - `tokens_in` = `prompt_tokens` − `cached_tokens` − `cache_write_tokens`;
   - `tokens_cached` = `cached_tokens`;
   - `tokens_cache_write` = `cache_write_tokens`.
6. **Clamping:** cached ≤ prompt first, then cache write ≤ prompt − cached, so the
   three input units always sum to `prompt_tokens`. Missing field = 0.
7. **Input size** (tier choice) = `tokens_in + tokens_cached + tokens_cache_write` —
   still the backend's `prompt_tokens`.
8. **Token limits count it:** a record's tokens are `tokens_in + tokens_cached +
   tokens_cache_write + tokens_out` (every token the backend handled), in the gateway's
   local limits and in `kaiak-control`'s aggregation. Without this, written tokens
   would silently leave the token limits.
   *Superseded 2026-10-05 by `1454a17`:* `tokens_cached` no longer counts — a
   record's tokens are `tokens_in + tokens_cache_write + tokens_out`
   (`docs/specs/GATEWAY.md`, Limits → Settle). Written tokens still count.
9. **Every record carries the five token units, zeros included**, like the four today.
   Estimated records report 0 written. Embeddings count `prompt_tokens` only.
10. **Metrics and logs:** `kaiak_usage_tokens_total` gains `unit="tokens_cache_write"`
    (the cardinality formula's per-record series go from 6 to 7); the request log line
    gains `tokens_cache_write`.
11. **Versions** (no backwards compatibility): protocol version 4 (records change);
    config `format_version` 4 (a new price unit an older reader rejects); usage spool
    format 3 (records inside); `last-known-good.json` format 5 (config format 4
    inside). `totals.json` and `limits.json` hold sums, not units: unchanged.
    Operators flush the spool (graceful shutdown) before upgrading.

## Constraints

- Protocol changes land on both halves at once (AGENTS.md → Project-Specific Rules).
- The gateway stays free of third-party dependencies.
- No backwards compatibility: every fixture moves to the new shape; no dual reading.

## Risks

- **Breadth of fixtures:** every usage record fixture gains the unit, every config
  fixture the format version, every status message the protocol version. Mitigation:
  a one-off script in step 1 (not committed), and a second script that checks every
  fixture equals its `HEAD` version with exactly that transform.
- **A limit or sum that misses the unit:** the token sum lives in two places (gateway
  `limits`, `kaiak-control` `aggregate`) and the tier's input size in the gateway.
  Mitigation: step 1 greps every sum of `tokens_cached` and names each site; tests
  in steps 2–4 cover limits and tier choice with written tokens.

## Tag

Tag `main` right before step 1 begins (AGENTS.md → Git: tags are anchors): `v0.9.1`,
message "before the cache-write unit". Local and `origin` only — pushing it to
`github` is a release.

## Phases and steps

- **Phase 1 — cache-write unit, end to end** (steps 1–4). Green at the end.
  1. `STEP-1-contract.md` — specs, schemas, fixtures, versions.
  2. `STEP-2-kaiak-control.md` — types, token sum, versions, GUIDE, examples, sample.
  3. `STEP-3-gateway.md` — meter, cost and tier, limits, metrics, log line, fake
     backend, versions, live-test kit.
  4. `STEP-4-e2e-and-docs.md` — gateway and cross-half e2e, operator docs, backlog
     entry removed; `scripts/check-all.sh` green.

Expected reds inside the phase: after step 1 both halves fail the new fixtures and the
version checks (step 2 clears kaiak-control's, step 3 the gateway's); the cross-half
e2e stays red until both halves speak protocol 4 (step 3, or step 4 if its configs
need moving).

## Verification

- Meter tests: writes reported and not; stream and non-stream; clamping (cached +
  written above prompt); embeddings; a backend that omits the field.
- Cost tests: written tokens at their price; unpriced → the tier's `tokens_in`;
  written tokens move a request across a tier threshold.
- Limits: written tokens count toward token limits, in the gateway and in
  `kaiak-control`.
- Shared fixtures run by both halves: a price with `tokens_cache_write`; a record
  missing it (invalid); old versions refused.
- Gateway e2e: a request whose backend reports writes settles at the write price,
  streamed and not; the metric and the log line carry the unit.
- Cross-half e2e: the unit reaches the control plane and counts toward its totals.
- `scripts/check-all.sh` green at the end of the phase.
- Live, after the phase: the live-test kit against the user's Azure deployment
  (`-kind azure-openai`) with a fresh long prompt shows `tokens_cache_write` in the log
  line and the cost at the write price. Needs the user's endpoint and key.

**Verification status:** done (2026-10-02); phase 1 green. The live check is
pending.

- [x] Meter: writes reported and not, stream and non-stream, clamping, embeddings, a
  backend that omits the field (`STEP-3-gateway.md`).
- [x] Cost: written tokens at their price, unpriced → the tier's `tokens_in`, written
  tokens across a tier threshold (`STEP-3-gateway.md`).
- [x] Limits: written tokens count toward token limits, in the gateway
  (`STEP-3-gateway.md`) and in `kaiak-control` (`STEP-2-kaiak-control.md`).
- [x] Shared fixtures, same codes from both halves (`STEP-1-contract.md`, steps 2–3).
- [x] Gateway e2e: writes settle at the write price streamed and not, across the tier
  threshold, and at `tokens_in` when unpriced; the log line and
  `kaiak_usage_tokens_total` carry the unit (`STEP-4-e2e-and-docs.md`).
- [x] Cross-half e2e: written tokens reach the sample and count toward its
  `tokens_per_hour` total (`STEP-4-e2e-and-docs.md`).
- [x] `scripts/check-all.sh` green 3× in a row (`STEP-4-e2e-and-docs.md`).
- [ ] Live: the live-test kit against the user's Azure deployment
  (`-kind azure-openai`), a fresh long prompt sent twice — `tokens_cache_write` and
  the write-price cost on the first log line. Needs the user's endpoint and key; run
  after the merge.
