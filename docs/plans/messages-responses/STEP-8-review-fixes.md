# Step 8 — review fixes

**Status:** not started

## Intent

Fix what the pre-merge review found (`docs/reviews/2026-10-06/AUDIT.md`), as triaged
there and decided with the user, so the branch merges with no known billing, security
or availability defect. This step is **phase 5** and ends green.

## Scope (finding IDs from the AUDIT)

- **H1** — Messages output from `message_start` is provisional. Without a
  `message_delta` usage, `tokens_out = max(reported, estimate from content)`, flagged
  estimated; input and cache fields stay as reported.
- **H2** — The input estimate in one pass. Nested `source` / `content` is never
  rescanned. A `source` counts as media only directly under an image or document
  block, which also fixes the AUDIT's [S] L1.
- **H3** — `shell` passes only without `environment` or with `environment.type:
  "local"`. Allowlisted Responses tools are judged by their members wherever tools
  are named (`tools`, `tool_choice`, `allowed_tools`).
- **M1** — On cloud types (`openai`, `azure-openai`, `anthropic`, `azure-anthropic`), a
  missing model is read from structured fields only:
  - OpenAI and Azure: `code` `model_not_found` / `DeploymentNotFound` or `param:
    "model"`;
  - Anthropic: `not_found_error` whose message starts `model:`.

  Self-hosted types keep the whole-word match.
- **M2** — One recursive scan, once per request, depth-bounded. It refuses
  `cache_control.ttl: "1h"` anywhere under `system`, `messages` and `tools`, and a
  repeated `cache_control` / `ttl`, naming the path. On Anthropic types only, as now.
- **M3** — A (backend, endpoint) found missing is remembered for the probe interval
  and left out of routing for that endpoint, with one warning per interval.
- **M4** — Classify stream error events and Anthropic statuses by their code or type:
  - overload (`529`, `overloaded_error`) and rate limit (`rate_limit_exceeded`) →
    neutral with the cooldown, as `429`;
  - caller codes (Anthropic `invalid_request_error`; OpenAI `invalid_prompt`,
    `invalid_image_url`, `failed_to_download_image`, `*_policy_violation` and the
    other `invalid_*` / `image_*` codes in OpenAI's list) → neutral, not retried;
  - `server_error`, `api_error`, unknown or missing → failure, as now.
- **M5** (decided 2026-10-06: refuse) — references to provider-stored objects:
  - Responses: `prompt`, `item_reference` input items, `file_id` on `input_file` /
    `input_image`;
  - Messages: `file` sources (`file_id`).

  Responses answers `stateful_responses_unsupported`; Messages gets a
  stored-object refusal code named in the spec. The checks are done in H2's pass
  where they ride along cleanly. Rationale for the spec: kaiak has no upload
  endpoint and stores nothing, so such an ID can only name another app's object in
  the shared provider account, and a stored prompt can carry hosted tools.
- **M6** — When the gateway would lower `max_tokens` to the ceiling at or below
  `thinking.budget_tokens` (read only), answer `400 invalid_value` naming `max_tokens`
  and the model's ceiling. Spec rule; operator note in DEPLOYMENT.
- **M7, M8** — DEPLOYMENT's data-file formats (point to GATEWAY.md); the missing log
  attribute rows.
- **L1** — Token-counting requests count only toward requests-per-minute limits:
  - no token or USD counter;
  - no `budget_unavailable`.
- **L2** — `endpoint_not_served` is decided before limits.
- **L3** — A Responses tool with no `type` is refused.
- **L4** — A relayed mid-stream error event keeps its type and code; its message
  becomes gateway text (spec: as the `5xx` rule).
- **L5** — Docs line: deployments name the ids the models list returns.
- **L6** (decided 2026-10-06: not checkable) — `verifyBackend` for `azure-anthropic`
  reports "not checkable" in a way no caller reads as verified (`ok: false` with a
  code, or a distinct status). BACKEND-VERIFY.md decides which; the sample `verify`
  exits non-zero or says so plainly.
- **L9** — Every docs and spec wording item in the AUDIT's L9 list.
- **L10** — Delete the dead `FiniteNumbers`.

Out: L7 and L8 stay recorded in the AUDIT only.

## Files likely touched

- `gateway/internal/accounting/`: `messages_usage.go`, `meter.go`, `estimate.go`.
- `gateway/internal/server/`:
  - `inbound_responses.go`, `inbound_messages.go`, `inbound.go`;
  - `params.go`, `limits.go`;
  - `upstream.go`, `pipeline.go`.
- `gateway/internal/provider/`:
  - `anthropic_price.go`, `wire.go` (model-missing per module), `stream_end.go`;
  - the modules' missing-model rules.
- `gateway/internal/routing/`: the endpoint-missing memory.
- `gateway/internal/schemacheck/`.
- `control/kaiak-control/src/backend-verify/`, `control/sample` (`verify`).
- Specs:
  - `GATEWAY.md` (each rule above, dated 2026-10-06);
  - `BACKEND-VERIFY.md`.
- Docs: `DEPLOYMENT.md`, `kaiak.md`, `TECH-STACK.md`, `AGENTS.md` (the `-kind` list and
  the owned-edits list), `docs/architecture/gateway.html`, `LIVE-BACKENDS.md`, the
  GUIDE.
- `docs/reviews/2026-10-06/AUDIT.md`: mark each finding fixed (commit) or recorded.

## Decisions made during planning

- Each fix gets a regression test that fails before it, where the finding is
  testable:
  - H1 — a cut stream after `message_start`;
  - H2 — a time bound on deep nesting;
  - H3 — hosted `shell`;
  - M1 — echoed IDs on each cloud type;
  - M2 — each nested place and the duplicates;
  - M3 — a second request skipping the remembered backend;
  - M4 — each class;
  - M5 — each field;
  - M6;
  - L1 — a count request under a full token window.
- One commit per finding group (accounting, inbound, provider/routing, control, docs)
  is fine. No unrelated refactoring.

## Acceptance criteria

- Every in-scope finding is fixed, with its test, and marked in the AUDIT.
- The spec states every new rule, dated 2026-10-06.
- `scripts/check-all.sh` green three times in a row: **phase 5 ends here**.
- `check-gateway.sh` and the live kit's self-test still pass. If a live check needs a
  rerun because behaviour visible to the kit changed (M6, L1), say which.

## Result

(filled in when the step is done)
