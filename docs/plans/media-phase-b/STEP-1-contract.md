# Step 1 — the contract

**Status:** not started

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
