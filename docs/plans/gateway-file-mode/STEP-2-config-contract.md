# Step 2 — config contract

**Status:** done — all acceptance criteria met

## Intent

Settle the config document — every field name and shape — as JSON Schema with fixtures,
and prove `kaiak-control` validates them. This is the contract both halves build on.

## Files likely touched

- `protocol/schema/config.schema.json`
- `protocol/fixtures/config/valid/*.json`, `protocol/fixtures/config/invalid/*.json`
  (each invalid fixture paired with the reason it must fail)
- `control/kaiak-control/` — ajv added; a test running every fixture
- `docs/specs/GATEWAY.md`, `docs/specs/CONTROL-PROTOCOL.md` — field names recorded

## Decisions made during planning

- The document covers: format version; backends (type `openai-compatible` |
  `azure-openai`, base URL, credential env-var name, connect and first-byte timeouts);
  models (public name, deployments = backend + backend-side name, declared metadata,
  declared defaults, output-limit default and ceiling, prices per unit with effective
  dates); keys (ID, SHA-256 hash, owner = workload or user, expiry, disabled);
  workloads (team, allowed models, limits); teams (limits); users (allowed models and
  limit overrides); global (limits, default user limits, request body cap, metrics
  key-ID label switch).
- A limit = type (`requests_per_minute`, `tokens_per_minute`, `tokens_per_hour`,
  `usd_per_month`) + value + model set (all models when omitted).
- Fields for P2/P3 (reporting intervals, outage grace, concurrency caps) are **not**
  added now — each plan adds what it uses.
- Cross-reference rules the schema cannot express (a key's workload exists, a
  deployment's backend exists, price dates ordered) are listed in the spec and checked
  by code in both halves; invalid fixtures cover each.

## Acceptance criteria

- Every valid fixture passes and every invalid fixture fails ajv validation, including
  cross-reference fixtures via `kaiak-control`'s semantic check.
- Spec files name the fields; `npm test`/`npm run lint` green.

## Result

Commands run (2026-09-24):

- `npm test` in `control/` — 86 tests **pass**: 6 valid fixtures, 74 invalid fixtures
  (52 schema, 22 semantic, each semantic one rejected with exactly its code), the
  `cases.json` ↔ files completeness check, plus the step-1 tests.
- `npm run lint` in `control/` — `tsc` clean, `boundaries ok` — **pass**.
- `scripts/check-gateway.sh` — gateway unchanged — **pass**.

Delivered:

- `protocol/schema/config.schema.json` (draft 2020-12); fixtures in
  `protocol/fixtures/config/{valid,invalid}/`, with `invalid/cases.json` mapping each
  invalid file to `{ kind, code?, reason }`. Each invalid fixture is one mutation away
  from a valid document.
- `kaiak-control`: `src/config/` subsystem, `validateConfig(doc)` exported from the
  package entry. Returns `{ ok: true, config }` or `{ ok: false, issues }`, each issue
  `{ code, message, path }` (path = JSON Pointer); schema issues carry code `schema`,
  semantic rules run only once the schema passes and report every violation.
- Spec: `CONTROL-PROTOCOL.md` Config (shape, conventions, override semantics, the
  semantic rule list); `GATEWAY.md` (base-URL joining, timeout and body-cap defaults,
  defaults application); `TECH-STACK.md` inventory (ajv 8.20.0); `ARCHITECTURE.md`.

Decisions beyond the plan:

- Required top level: `format_version`, `global`, `backends`, `models`, `keys`;
  `teams`, `workloads`, `users` optional. `global.default_user.allowed_models` is
  required (explicit, even when there are no users).
- Backend timeouts optional with defaults (connect 5000 ms, first byte 300000 ms —
  non-streaming responses arrive whole); `max_request_body_bytes` default 16 MiB;
  `metrics.key_id_label` default true. Defaults are in the schema (`default`) and
  `GATEWAY.md`.
- `base_url`: no trailing slash, query or fragment (schema pattern), so joining is
  plain concatenation. openai-compatible includes the `/v1` path.
- `api_key_env` required for `azure-openai` (schema `if`/`then`).
- `metadata` required with `context_length` and all four capabilities;
  `output_limit` optional (embedding models) and, when present, needs both fields.
- `defaults` refuses gateway-owned / content fields (`model`, `messages`, `prompt`,
  `input`, `stream`, `stream_options`, `max_tokens`, `max_completion_tokens`).
- A user must have a `users` entry (possibly `{}`) to own keys.
- Limit `value` ≥ 0 (0 blocks); integer for request/token types. Limit `models` is
  non-empty when present and cannot hold `"*"`.
- Extra semantic rules beyond the brief: `key-hash-duplicate`,
  `output-limit-above-context`, `reasoning-efforts-without-reasoning`.
- IDs: `^[A-Za-z0-9][A-Za-z0-9._@-]{0,127}$` (emails work as user IDs); model names
  additionally allow `/` and `:` (Hugging Face names).
- ajv runs with `allErrors`, `strictTypes`, `strictTuples`, `allowUnionTypes`;
  `strictRequired` stays off (it refuses the `if`/`then` required-when pattern).

Open for later steps:

- Unit overlap (are `tokens_cached` / `tokens_reasoning` part of `tokens_in` /
  `tokens_out`?) decides whether a reasoning model needs a `tokens_reasoning` price —
  settle in step 7 and record in `CONTROL-PROTOCOL.md`.
- Output-limit reservation for a model without `output_limit` — step 8.
- `defaults` values are scalars or arrays, as briefed; object values (vLLM
  `chat_template_kwargs: { enable_thinking: false }`, a common Qwen3 default) are
  refused. Widening to objects is a one-line schema change if wanted.
- The schema is read from `protocol/` relative to the package source; fine while
  `kaiak-control` is private and used from this repo — revisit if it is published.
