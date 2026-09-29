# Plan: tiered prices (long-context pricing)

## Goal

Price requests whose prompt passes a size threshold at that threshold's rates, as
the providers bill them:
- OpenAI and Azure (gpt-5.4 and later): above 272k input tokens, the whole request
  is billed at about 2× input and 1.5× output, cached input included.
- Claude on Azure AI Foundry: the same pattern at 200k.
- Qwen's hosted APIs: several brackets (0–32k, 32k–128k, 128k+).

Today a price entry has one rate per unit, so long prompts on these models are
under-priced, and USD budgets let them through too cheaply.

## Scope

- A price entry holds a list of **tiers**, each a threshold plus a full set of
  per-million prices; the first tier's threshold is 0. One code path for every model:
  a model without long-context pricing has one tier.
- Both halves validate the new shape: schema, and semantic rules for what the schema
  cannot express.
- The gateway picks the tier when it prices a record.
- Contract docs, fixtures, examples, the live-test kit's configs, and the
  `kaiak-control` GUIDE.

## Out of scope

- Per-service-tier prices (priority, flex): the gateway keeps every request on
  standard (`GATEWAY.md` → Providers → Service tier, settled 2026-09-29).
- Per-region or per-deployment prices: the user will come back to it.
- Cache-write prices (`cache_creation_input_token_cost`), per-search fees, audio and
  image rates: no unit for them, and no deployment has asked for them.
- An importer for LiteLLM's price list: that is the app's job (GUIDE → Prices).

## Decisions

Settled with the user (2026-09-29):

1. **One array of tiers starting at threshold 0,** not a base price plus a separate
   long-context block, and not a fixed two-tier shape. One code path, and it covers
   bracketed pricing (Qwen) without another format change. Rejected:
   `usd_per_million` + `usd_per_million_large` + a threshold — flat, but capped at
   two tiers.
2. **Each tier is complete in itself.** It lists its own prices; nothing is inherited
   from the tier below. Within a tier, the existing rule holds: an unpriced
   `tokens_cached` is charged at that tier's `tokens_in` price; an unpriced
   `tokens_in` or `tokens_out` costs 0.

Made while planning (confirm in review):

3. **Shape:**
   ```json
   { "effective_from": "2026-10-01",
     "tiers": [
       { "above_input_tokens": 0,      "usd_per_million": { "tokens_in": 4, "tokens_cached": 0.4, "tokens_out": 20 } },
       { "above_input_tokens": 272000, "usd_per_million": { "tokens_in": 8, "tokens_cached": 0.8, "tokens_out": 30 } }
     ] }
   ```
   `usd_per_million` keeps its current meaning and unit rules inside each tier.
4. **Which tier applies:** the last tier whose `above_input_tokens` is strictly less
   than the request's input size; the first tier (0) applies to every other request,
   an input of 0 included. "Above 272000" matches the providers' ">272K".
5. **Input size = `tokens_in + tokens_cached`**, the backend's `prompt_tokens`. An
   estimated record uses its estimated input, the figure limits reserved. The whole
   record is priced at the chosen tier.
6. **Bounds:** 1 to 8 tiers per entry (schema); thresholds are integers from 0 to
   2^53 − 1.
7. **New semantic rules, both halves:** `price-tier-first-not-zero` (the first tier's
   threshold is not 0) and `price-tiers-not-increasing` (a threshold not above the
   one before). The first could be schema, but a semantic rule gives it a stable code
   and a clear message.
8. **Versions:** config `format_version` 3 and protocol version 3, following the
   group-tree precedent. The data directory's last-known-good config bumps its format
   version, so a gateway discards a cached config in the old shape and says so.
   Totals cache, usage spool and snapshot are untouched: usage records do not change.
9. **Usage records do not change.** Units already carry `tokens_in` and
   `tokens_cached`, so the control plane can re-price with the same rule.
10. **USD limits are unaffected:** they reserve nothing before a request runs, and
    cost is known only when it settles.

## Constraints

- Protocol changes land on both halves at once (AGENTS.md → Project-Specific Rules).
- The gateway stays free of third-party dependencies.
- No backwards compatibility: every config and fixture moves to the new shape; no
  dual reading.

## Risks

- **Breadth of fixtures:** about 120 config fixtures, the examples and the live-kit
  configs carry `usd_per_million`. Mitigation: migrate them with a one-off script in
  step 1 (not committed), and review the diff for anything that isn't the plain
  wrapping.
- **Rule drift between halves:** tier selection is only in the gateway, but both
  halves validate. Mitigation: shared valid and invalid fixtures with codes in
  `cases.json`, run by both.

## Tag

Tag `main` right before step 1 begins (AGENTS.md → Git: tags are anchors), for
example `v0.7.5` (the next tag after `v0.7.4`), with a one-line message saying it is the world before tiered
prices.

## Phases and steps

- **Phase 1 — tiered prices, end to end** (steps 1–4). Green at the end.
  1. `STEP-1-contract.md` — specs, schema, fixtures, versions.
  2. `STEP-2-kaiak-control.md` — types, semantic rules, versions, GUIDE.
  3. `STEP-3-gateway.md` — config parsing and rules, tier selection in `Cost`,
     versions, the last-known-good file.
  4. `STEP-4-e2e-and-docs.md` — e2e (gateway and cross-half), examples, live-kit
     configs, DEPLOYMENT, architecture pages; `scripts/check-all.sh` green.

Expected reds inside the phase: after step 1 both halves fail the new fixtures and
the version checks (step 2 clears kaiak-control's, step 3 the gateway's); the
cross-half e2e and sample configs stay red until step 4.

## Verification

- Unit tests for tier selection at the edges: input 0, exactly on a threshold, one
  above; cached input counted toward the size; estimated records; a one-tier entry
  pricing exactly as today.
- Shared fixtures: valid (one tier; 272k two tiers; Qwen-style three tiers) and
  invalid (no tiers, first not 0, not increasing, more than 8, negative or
  fractional threshold, old `usd_per_million` at the entry level), with the same
  codes from both halves.
- Gateway e2e: a request above the threshold of a two-tier model settles at the upper
  tier's cost; one below, at the base tier's cost.
- `scripts/check-all.sh` green at the end of the phase.

**Verification status:** done (2026-09-29); phase 1 green.

- [x] Tier selection at the edges, cached input, estimated records, one tier as the
  flat arithmetic (`STEP-3-gateway.md`; kaiak-control in `STEP-2-kaiak-control.md`).
- [x] Shared fixtures, same codes from both halves (`STEP-1-contract.md`, steps 2–3).
- [x] Gateway e2e: below, exactly at and above the threshold, and cached input
  above it, settle at their tiers' costs (`STEP-4-e2e-and-docs.md`).
- [x] `scripts/check-all.sh` green 3× in a row (`STEP-4-e2e-and-docs.md`).
