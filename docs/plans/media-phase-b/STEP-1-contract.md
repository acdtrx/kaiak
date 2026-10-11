# Step 1 — the contract

**Status:** done — reviewed and committed 2026-10-11

## Intent

Write phase B's whole contract into the specs, and move the shared schemas and
fixtures to protocol 7:
- one version, carried by the config as `protocol_version`;
- `endpoints` mandatory on every model;
- one backend type per model.

The specs describe the finished phase B: uploads, image edits, speech-to-text, and the
one "not found" reading of decision 22. The schemas and fixtures move to protocol 7 only.
Each endpoint's names, and `global.max_upload_bytes`, enter the schema in the step that
builds them (steps 4–6), on both halves at once.

## Files likely touched

- `docs/specs/CONTROL-PROTOCOL.md`:
  - **one version** (OVERVIEW decision 2): the protocol version section, the config's
    top-level shape (`protocol_version`, the integer `7`, equal to `Kaiak-Protocol`),
    the bump rule, and every passage naming a config format version;
  - **Endpoints per model:** mandatory, non-empty, unique; each entry served by the
    model's one type;
  - **a new semantic rule:** every deployment of a model on a backend of the same type
    (suggested code `deployment-types-differ`, path the first deployment whose type
    differs from the first deployment's; judged only when every deployment's backend
    is defined, as `endpoint-not-served` is);
  - `global.max_upload_bytes`;
  - the usage record: `audio_seconds_in` is now produced (by speech-to-text);
  - the endpoint names `images_edits` and `audio_transcriptions` in the list of names.
- `docs/specs/GATEWAY.md`:
  - **Client API:** `/v1/images/edits` and `/v1/audio/transcriptions`, their owned
    parts (decisions 26–27), their refusals; the JSON form of edits refused;
  - **Request bodies:** uploads — the content type, the cap, the upload budget, the
    read deadline, the part rules, the rebuilt body (decisions 17–21, 28);
  - **Model metadata → Endpoints per model:** mandatory; one type per model; the one
    refusal and its message (decision 23);
  - **Providers → Endpoint support:** the table gains the two endpoints (`vllm-omni`:
    edits; `vllm` and `llama-server`: transcriptions); the type-level refusal goes;
  - **An endpoint missing from a server** (decision 22): the section, the
    core-endpoint notion, `vllm`'s no-core rule and the `501` / `503` readings go; a
    route-missing answer on a listed endpoint is one reading on every type,
    `502 upstream_path_missing` at once — neither a circuit failure nor retried — its
    message naming both causes; the endpoint memory goes;
  - **the errors table, the outcome classes and the retry table** follow:
    `upstream_endpoint_missing` removed, `upstream_path_missing` neutral and not
    retried (its retry rule removed); the new error codes (a non-multipart upload;
    an upload that must be measured and cannot be);
  - **Accounting → Media usage:** edits' images; speech-to-text's seconds (the
    backend's, `verbose_json`'s `duration`, the measured upload, the fallback —
    decisions 8, 29, 30) and tokens (decision 9);
  - **Limits:** `audio_seconds_in` counted after the request (no reservation), as
    audio generated;
  - **Observability:** operation and output type (decision 25); the request line and
    metrics for the two endpoints;
  - **Configuration:** `KAIAK_UPLOAD_MEMORY_BYTES`, `KAIAK_UPLOAD_READ_TIMEOUT_MS`.
- `docs/specs/BACKEND-VERIFY.md` if it names the config format or endpoint support.
- `protocol/schema/config.schema.json`: `protocol_version` (const 7) in place of
  `format_version`; `endpoints` required on a model.
- `protocol/fixtures/`:
  - **every config document** (`config/valid`, `config/invalid`, `config/resolved`,
    `messages/config-event`, `duplicate-members`): `protocol_version: 7`, and every
    model given an `endpoints` list its type serves; a fixture whose model mixes
    types is split or reshaped, keeping what it tests;
  - **new invalid fixtures** with `cases.json` entries: a model without `endpoints`;
    deployments on two types; an entry the type does not serve; `protocol_version` 6;
    a `format_version` member;
  - fixtures that test what no longer exists are removed (the optional list's
    "absent means every endpoint", the type-level not-served case).
- `examples/config.json`: `protocol_version: 7`, a list on every model.
- `docs/BACKLOG.md`: new entries — streaming transcription (trigger: someone asks for
  live transcripts); `/v1/audio/translations` (trigger: someone asks for it).

## Decisions made during planning

- **The specs describe the end state of phase B now;** the schema gains each
  endpoint's names in its own step, so phases 2 and 3 stay green step by step.
- **Every rule is dated** `(settled 2026-10-11, with the user)`, or, for decisions
  23–30, as confirmed at review.
- **Removal discipline:** the endpoint-missing class, the memory and the type-level
  refusal leave the specs entirely — no "formerly", no aliases.

## Acceptance criteria

- The specs read consistently: no passage left naming a format version, an optional
  list, the endpoint memory, `upstream_endpoint_missing` or core endpoints (grep
  checklist recorded in the result).
- The schema validates every valid fixture and refuses every invalid one as its case
  says (checked with either half's validator, or recorded as the expected red below).
- `scripts/check-all.sh` run and recorded. **Expected reds:** both halves fail the
  moved fixtures and their version checks — `kaiak-control` cleared by step 2, the
  gateway by step 3; the cross-half e2e red until step 3.

## Result

### What changed

- `docs/specs/CONTROL-PROTOCOL.md`:
  - **Shape → One version** (new): what the version covers, the config carrying it,
    the bump rule (unreadable or invalid bumps it; additions bump nothing), the
    rejected document version of the config's own; the current version `7`; the
    request-check table and the repeated-header example at `7`.
  - **Messages**: the config document states its own `protocol_version`; the event's
    envelope carries none.
  - **Config**: top-level `protocol_version` (the integer `7`, equal to
    `Kaiak-Protocol`); the old "bumps neither / bump both" passages rewritten under
    the one-version rule; Types bump no version restated as an addition; the upload
    cap among the global settings; **Endpoints per model** required, judged against
    the one type, the two new names; **One backend type per model** (new: the rule,
    the cost, the rejected mixed types); `audio_seconds_in` produced by
    speech-to-text; the media-units paragraph; the semantic rules
    `deployment-types-differ` (new) and `endpoint-not-served` (one type, judged only
    when every backend is defined and the types agree); the document used by file
    mode, seed and sample.
- `docs/specs/GATEWAY.md`:
  - **Client API**: the two endpoints; nine formats, image edits and speech-to-text
    each its own (uploads, their units; the JSON form of edits refused); the
    rerank and media rejected alternatives without a type's wrong-path endpoints;
    errors table — new `unsupported_content_type` and `audio_format_unsupported`
    rows, a `stream`-part `invalid_value` row, the upload cases folded into
    `duplicate_member`, `invalid_body`, `missing_required_parameter`, `invalid_type`,
    `invalid_value` (images cap), `response_format_unsupported`,
    `external_reference_unsupported`, `request_too_large`, `server_busy`;
    `endpoint_not_served` with the one message (decision 23); `upstream_path_missing`
    with its message naming both causes; `upstream_endpoint_missing` removed;
    **owned parts** of image edits and speech-to-text (decisions 16, 26, 27); the
    `/v1/models` entry shows the list; the Anthropic list reads the list; Files API
    wording in stored objects.
  - **Model metadata**: Endpoints per model rewritten (required; why — what vLLM,
    vLLM-Omni and llama-server answer for an endpoint the loaded model lacks;
    rejected: an optional list, a per-backend list, a widening list); **One backend
    type per model** (new).
  - **Request pipeline**: stage 2 (the list alone, the llama-server measurement
    refusal), stage 4 (all the model's deployments); **Uploads** (new: content type,
    cap, budget, in memory, read deadline, part rules, repeats, the rebuilt body —
    decisions 17–21, 28).
  - **Providers**: `vllm-omni` with edits; the Endpoint support table's two new
    columns' endpoints (`images_edits` with the media column, a new
    `audio_transcriptions` column) with their sources; image edits and
    speech-to-text by type; the list names what a model serves; base-URL paths;
    passthrough edits and headers for uploads; media answers for edits and
    transcriptions; vLLM's transcription model check; `vllm-omni` edits' model
    mismatch; **A path the server does not have** replaces the wrong-path and
    endpoint-missing sections (decision 22: one reading on every type, `502` at
    once, not retried, neutral; signatures per module, vLLM's included).
  - **Routing and reliability**: deployments on one type; the retry and outcome
    tables (path missing not retried, neutral; the `501`/`503` exceptions and the
    endpoint-missing rows gone); health; the timers for edits and transcriptions.
  - **Limits**: the input estimate for edits and transcriptions; no output limit
    there; `n` capped and reserved on edits; `audio_seconds_in` counted after the
    request, nothing reserved (decision 8, with the rejected reservation); which
    requests each media limit applies to; file-mode drift.
  - **Accounting**: image edits' images; speech-to-text's `audio_seconds_in` per
    type, tokens, measuring an upload (decisions 8, 9, 29, 30); estimation and
    partial records.
  - **Configuration sources**: `KAIAK_UPLOAD_MEMORY_BYTES`,
    `KAIAK_UPLOAD_READ_TIMEOUT_MS`; the upload cap's default.
  - **Observability**: routes; `generate_content` and `gen_ai.output.type` `text`
    (decision 25); `gen_ai.request.stream` `false` on uploads;
    `kaiak.usage.audio.input.duration` produced; attempt outcomes (`path_missing`
    neutral, `endpoint_missing` gone: thirteen series), retry reasons (six), the
    error class list; cardinality; the upload-cap warning's attributes
    (`kaiak.config.max_upload_size`, `kaiak.upload_budget.size`); the endpoint-missing
    log line and `kaiak.endpoint` removed.
  - **Lifecycle**: the upload read deadline.
- `docs/specs/BACKEND-VERIFY.md`: unchanged — it names neither a config version nor
  how endpoints are judged (its pointer to the backend-type fixtures' `endpoints`
  stays true).
- `docs/kaiak.md`: v1 scope gains uploads (image edits, speech-to-text), models on
  one type with their lists, backend-reported seconds; out of v1 now video,
  streaming transcription and translations.
- `docs/BACKLOG.md`: new **Streaming transcription** and **Audio translations**;
  **Image and audio models** points at both media plans; **Measuring compressed
  speech audio** notes the MP3/FLAC upload readers; **llama-server quirks** (the
  encoder-model chat case refused by the list; the section name); **Pricing media
  units** (no format version); **vLLM-Omni quirks** removed — its only content was
  the image `503` reading, which decision 22 removes, and the lists keep image
  requests off speech models.
- `protocol/schema/config.schema.json`: `protocol_version` (const 7) in place of
  `format_version`; `endpoints` required; the deployments' and endpoints'
  descriptions. `status.schema.json`: `protocol_version` const 7.
  `usage-record.schema.json`: the units' description (where `audio_seconds_in`
  comes from). The endpoint enum is unchanged (steps 5–6).
- `examples/config.json`, `examples/local-config.json`: `protocol_version: 7`, a list
  on every model (the local demo keeps OpenAI's three, as it served).

### Error codes named

- `unsupported_content_type` — `400 invalid_request_error`: an upload endpoint's body
  not `multipart/form-data` with a boundary (OpenAI's JSON form of edits included).
- `audio_format_unsupported` — `400 invalid_request_error`, `param: "file"`: a
  transcription for a `llama-server` model whose upload cannot be measured.
- Existing codes reused (no new code): a malformed multipart body → `invalid_body`;
  a repeated single-value part → `duplicate_member`; a `stream` part other than
  `false` → `invalid_value` (`param: "stream"`); `vllm_xargs` →
  `external_reference_unsupported`.

### Fixtures

- **Moved to protocol 7** (text-level edits, formatting kept; an `endpoints` list
  after each model's `deployments`, in the file's own array style): 183 config
  documents — `config/valid` 16, `config/invalid` 150 (148 + the two renamed below),
  `config/resolved` 5, `messages/config-event` 7, `duplicate-members` 5 (their
  repeats intact) — and 30 status messages (29 at `protocol_version: 7`, one renamed
  below). Reshaped: `valid/anthropic-types.json` (a model per type),
  `valid/backend-types.json` (a model per type), `valid/model-endpoints.json` (the
  reranker on two `vllm` backends; the mixed model split into a `llama-server` model
  and a Claude model; `chat` given a list). `invalid/endpoints-unknown-name.json`
  names `chat` instead of `images_edits`, which step 5 makes a name. `config_hash`
  recomputed for both valid config events.
- **Added** (with `cases.json` entries): `endpoints-missing.json` (schema),
  `deployment-types-differ.json` (`deployment-types-differ`; the third deployment
  differs, so the path is not simply index 1), `endpoints-types-differ.json`
  (`deployment-types-differ` alone: an entry one of the two types serves is not
  judged), `format-version-member.json` (schema: `format_version` beside
  `protocol_version`), and by rename `protocol-version-6.json`,
  `protocol-version-missing.json`, `messages/status/invalid/protocol-version-6.json`.
- **Removed**: `format-version-5.json`, `format-version-missing.json`,
  `messages/status/invalid/protocol-version-5.json` (replaced by the renames). No
  fixture tested an absent list or the type-level refusal on its own: the valid
  `model-endpoints.json` held both, and was reshaped.
- **Checked** with the control half's ajv (`Ajv2020`, kaiak-control's options) over
  `protocol/schema/`: 308 checks — every valid config, resolved config, example and
  message valid; every schema-kind invalid fixture invalid, every semantic-kind one
  schema-valid; every duplicate-member file valid as `JSON.parse` reads it; every
  `cases.json` matching its directory. The version and endpoints fixtures fail for
  their own reason (`required endpoints`, `const 7`, `required protocol_version`,
  `additionalProperties format_version`, and the existing `uniqueItems`, `minItems`,
  `enum`), and no other invalid fixture gained an endpoints or version error.

### Grep checklist (`docs/specs/`)

`format_version` 0 · "config format" 0 · "format 6" 0 · `format-5` 0 ·
`upstream_endpoint_missing` 0 · `endpoint_missing` 0 · "core endpoint" 0 ·
"endpoint memory" 0 · "endpoint-missing" 0 · "none of its backends" 0 ·
"no endpoint produces it yet" 0 · "reserved from the measured upload" 0 ·
"An endpoint missing from a server" 0 · "Wrong path to a host" 0 ·
"optional `endpoints`" 0 · "Without a list" 0 · "remembered" 3, all unrelated (batch
cursors and gateways). "optional list" 1: the rejected alternative in Model metadata
→ Endpoints per model, recorded on purpose.

### Suite

`scripts/check-all.sh` stops at the gateway stage, as expected. `go test`: red
packages and their clearing step —
- `cmd/kaiak` (15 tests), `internal/config` (23), `internal/control` (28): the moved
  fixtures, examples and status at protocol 7 — step 3;
- `internal/provider` `TestEndpointSupportFollowsTheSpec` and `internal/server`
  `TestEveryErrorCodeHasItsClass`: they read GATEWAY.md's endpoint-support and
  error tables, which now describe phase B's end state — the new columns
  (`images_edits`, `audio_transcriptions`; steps 5–6) and codes
  (`unsupported_content_type`, step 4; `audio_format_unsupported`, step 7;
  `upstream_endpoint_missing` gone, step 3). **Not cleared by step 3: see below;**
- `e2e` `TestSeriesStartAtZero`: unrelated (it writes its own config, no fixture or
  spec), passed 3× when run alone — a flake under the full race run.

Every other gateway package green. The live kit's checks did not run (the script
stops at `go test`). `control/`, run apart: `npm test` 37 failed, 8 cancelled, 124
passed — every test file that imports `test-support` (it reads the schema's
`format_version`), the packaged schema copy's byte-for-byte check (config, status,
usage-record), and the protocol-7 fixtures — step 2; `npm run lint` green. The
cross-half e2e, run apart: green (neither half's code moved yet); red from step 2
until step 3, as planned.

**For the main session — phase ends:** the two spec-reading tests stay red after
step 3 as long as the spec describes the end state: the error table until steps 4
and 7, the endpoint table until steps 5 and 6. Phases 1 and 2 cannot end green as
planned unless one of these is chosen: the spec's two columns and two error rows
land with their steps (each step adds its row, as the schema's names do), or those
tests learn the phase-B rows earlier, or the reds are accepted until step 7.

**Review (main session, 2026-10-11):** the first option taken — each table row lands
with the code that answers it. `GATEWAY.md`'s endpoint-support table is back to its
seven columns (no `images_edits` in the media column, no `audio_transcriptions`
column, the sources sentence without them), and the errors table without the
`unsupported_content_type` and `audio_format_unsupported` rows; the exact rows and
columns are recorded in steps 4, 5, 6 and 7, which land them. After the move,
`TestEndpointSupportFollowsTheSpec` passes, and `TestEveryErrorCodeHasItsClass`
fails only on `upstream_endpoint_missing` — cleared by step 3. The rest of the spec
keeps describing phase B's end state. `TestSeriesStartAtZero`'s single failure is
watched: if it fails again, it gets a root cause, not a retry.
