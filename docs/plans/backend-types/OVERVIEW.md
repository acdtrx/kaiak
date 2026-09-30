# Plan: a backend type per server

## Goal

Give each server kind its own backend type and gateway module — `openai`, `vllm`,
`llama-server` beside `azure-openai` and the generic `openai-compatible` — so that
what is true of one server has a place to live. Today `openai-compatible` covers
vLLM, llama-server, SGLang and OpenAI alike, so a rule meant for one of them (OpenAI's
service tier) lands on all of them, and a fix for one (llama-server's usage and error
quirks, `docs/BACKLOG.md`) has nowhere to go.

Then use the seam for the first server-specific fix the 2026-09-30 review found: a
backend `base_url` with a wrong path fails silently (O1).

## Scope

- Three new backend types in the config contract, both halves: `openai`, `vllm`,
  `llama-server`. `openai-compatible` and `azure-openai` stay.
- A gateway module per type. The new ones start thin: what they share with
  `openai-compatible` comes from the wire core, not from copies.
- The forced standard service tier moves to `openai` and `azure-openai` only.
- `verifyBackend` and the sample's `verify` take every type; the live-test kit
  emits the type matching its `-kind` and gains `-kind llama-server`.
- A wrong-path `404` becomes the deployment's failure, per module (phase 2).
- Contract docs, fixtures, examples, `GUIDE.md`, `DEPLOYMENT.md`, README.

## Out of scope

- llama-server's quirks — usage of multi-sequence requests, `n_predict`/`n_cmpl`,
  client errors answered `500` (review L1–L3): backlogged (`docs/BACKLOG.md`,
  llama-server quirks), to land in the `llama-server` module later.
- An SGLang type, or any type beyond these three: `openai-compatible` covers the
  rest.
- The other review findings (`docs/reviews/2026-09-30/AUDIT.md`); C5, O2 and O3 are
  already on `main`.

## Decisions

Settled with the user (2026-09-30):

1. **A type per server, thin if need be** — `openai`, `vllm`, `llama-server`,
   `azure-openai`, plus `openai-compatible` as the generic type for any other server
   speaking the OpenAI format. Replaces the 2026-09-29 rule "a new backend type is
   added only when a real difference needs one" (`GATEWAY.md` → Providers): that
   rule left server-specific fixes without a home, and the review found three.
2. **The service tier is forced only where it is billed**: `openai` and
   `azure-openai` keep the 2026-09-29 rule (a client's `service_tier` becomes
   `"default"`; chat completions always carry it). `openai-compatible`, `vllm` and
   `llama-server` pass the client's field untouched and add none — `openai-compatible`
   is not restricted by these constraints, and a strict server that refuses unknown
   fields (review C6) no longer receives one it did not ask for.
3. **llama-server's quirks wait** (backlog); the type comes first.

Made while planning (confirm in review):

4. **Type names** follow the servers: `openai`, `vllm`, `llama-server` — the names
   `verifyBackend` already reports as `server`, and the live-test kit's kinds.
5. **The new modules start as `openai-compatible` does**: `base_url` includes the API
   version path, bearer credential (none without `api_key_env`), the models list as
   probe, `model_not_found` as the missing-model code. Each is its own module (file
   and type in the registry), so a later fix changes one server only; the few lines
   they have in common (URL join, bearer header, list probe) are shared helpers, not
   copies.
6. **`openai` requires `api_key_env`**, as `azure-openai` does (schema, both halves):
   OpenAI answers nothing without a key. `base_url` stays required
   (`https://api.openai.com/v1`); no default URL.
7. **Versions:** config `format_version` 4, protocol version 4, and the data
   directory's last-known-good config format bumps. The types are additive, but the
   meaning of an existing value changes — an OpenAI backend typed `openai-compatible`
   loses the forced tier — so a config is moved on purpose, and halves of different
   versions refuse each other with a clear error rather than a schema complaint about
   a type. Usage records, totals, spool and snapshot formats are untouched.
8. **`verifyBackend`** checks every type; `openai`, `vllm`, `llama-server` and
   `openai-compatible` read `<base_url>/models`. When the server it recognizes
   (`vllm`, `llama-server`) is not the declared type, the report carries a note naming
   the type to use. Recognition itself is unchanged.
9. **Live-test kit:** each `-kind` emits its type (`vllm` → `vllm`, `openai` →
   `openai`, `azure-openai` → `azure-openai`); `-kind llama-server` is added.
10. **Examples:** the example config's self-hosted backends take their server's type.
11. **Wrong path (phase 2):** a `404` that the module recognizes as its server's
    "no such path" answer is the deployment's failure, as a missing model is
    (settled 2026-09-25, `GATEWAY.md` → Wrong model on a host): `502` with a new code
    (`upstream_path_missing`), not relayed, retried on another deployment, counted
    toward the circuit, no usage. For `openai-compatible`, whose server is unknown, a
    `404` whose body is not an OpenAI-shaped error is read the same way. The
    background check on config apply logs a models list answering `404` as a warning
    naming `base_url`, apart from the info line for an unreachable backend.

## Constraints

- Protocol changes land on both halves at once (`AGENTS.md` → Project-Specific Rules).
- The gateway stays free of third-party dependencies.
- No backwards compatibility: every config and fixture moves to format 4; no dual
  reading.
- Only provider packages talk to backends; `verifyBackend` stays the control half's
  only backend client.

## Risks

- **A silent loss of the forced tier**: an OpenAI deployment kept as
  `openai-compatible` after the upgrade runs on the project's own tier. Mitigation:
  the format bump makes every config a deliberate edit; `DEPLOYMENT.md`, `GUIDE.md`
  and the release notes say which type forces the tier.
- **Unknown-path answers vary by server and version** (phase 2). Mitigation: each
  module's signature is checked against the real server where one is at hand
  (llama-server locally, vLLM on the DGX) and against documented shapes otherwise;
  anything not recognized stays the caller's `404`, as today.
- **Fixture breadth**: ~120 fixtures carry `format_version`. Mitigation: a one-off
  script (not committed), diff reviewed.

## Tag

Tag `main` right before step 1 begins (`AGENTS.md` → Git: tags are anchors):
`v0.8.1`, "the world before backend types". Local only; pushing it to `github` would
be a release.

## Phases and steps

Branch `backend-types`, worktree `.claude/worktrees/backend-types`.

- **Phase 1 — backend types, end to end** (steps 1–4). Green at the end.
  1. `STEP-1-contract.md` — specs, schema, fixtures, versions.
  2. `STEP-2-kaiak-control.md` — types, versions, `verifyBackend`, the sample's
     `verify`, GUIDE.
  3. `STEP-3-gateway.md` — config types and rules, the modules, the service tier by
     type, versions, the live-test kit.
  4. `STEP-4-e2e-and-docs.md` — e2e, examples, DEPLOYMENT, README, architecture
     pages; `scripts/check-all.sh` green.
- **Phase 2 — wrong path is the deployment's failure** (step 5). Green at the end.
  5. `STEP-5-wrong-path.md` — per-module unknown-path answers, the new code, the
     config-apply warning, spec.

Expected reds inside phase 1: after step 1 both halves fail the new fixtures and the
version checks (step 2 clears kaiak-control's, step 3 the gateway's); the cross-half
e2e and sample configs stay red until step 3 or 4.

## Verification

- Shared fixtures: a valid config with every type; invalid: an unknown type, `openai`
  without `api_key_env`; the same codes from both halves.
- One table test runs every gateway module through the same service-tier cases:
  `openai` and `azure-openai` force `"default"` (chat always, other endpoints when
  sent); the other three pass the client's value untouched and add none (closes the
  review's T1).
- Each module's URL, credential header and probe tested against the fake backend.
- `verifyBackend`: every type accepted; a vLLM or llama-server answer under another
  type carries the note.
- Phase 2: per module, the unknown-path answer gives `502 upstream_path_missing`, a
  retry on another deployment and a circuit failure; a model-missing `404` and a
  caller's `404` behave as today; the config-apply warning names `base_url`.
- Live (opt-in, `docs/testing/LIVE-BACKENDS.md`): `-kind llama-server` and
  `-kind vllm` against the DGX, each with a wrong `base_url` for phase 2.
- `scripts/check-all.sh` green at each phase end.

**Verification status:** not started.
