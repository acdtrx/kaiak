# Step 2 — kaiak-control

**Status:** done (2026-10-01)

## Intent

`kaiak-control` and the sample accept, validate and verify the five backend types.

## Files likely touched

- `control/kaiak-control/src/config/types.ts`: `BackendType` gains `openai`, `vllm`,
  `llama-server`.
- The check for `openai` without `api_key_env`, if it is not schema-only.
- `control/kaiak-control/src/backend-verify/index.ts`: every type accepted (the input
  check reads the schema's enum already — confirm); `<base_url>/models` for every type
  but `azure-openai`; the bearer credential likewise; a note when `recognizeServer`
  finds `vllm` or `llama-server` and the declared type is another one (wording in
  `BACKEND-VERIFY.md`).
- `control/sample/src/verify/index.ts`: `--type` takes the five types (default
  stays `openai-compatible`); usage text.
- `control/kaiak-control/GUIDE.md`: the types, which one forces the service tier,
  choosing a type.

## Decisions made during planning

- The sample's `--type` list is read from `kaiak-control`'s type, not a second
  hand-written list, if the type is exported as a value; otherwise one list beside the
  type.

## Acceptance criteria

- kaiak-control passes every shared config fixture with the codes `cases.json` names.
- `verifyBackend` tests: each type reaches the right URL with the right header; a
  vLLM answer under `openai-compatible` and a llama-server answer under `vllm` carry
  the note; a matching type carries none.
- The sample's `verify --type` accepts the five types and refuses another with its
  message; exit codes unchanged.
- `npm test` and `npm run lint` green. Suite recorded; expected red: the gateway
  (step 3) and the cross-half e2e (steps 3–4).

## Result

- **Types.** `config/types.ts`: `BACKEND_TYPES`, the five types in the schema's enum
  order, exported as a value from `kaiak-control`; `BackendType` is derived from it.
  A config test pins it to `$defs.backend.properties.type.enum` of the package's
  schema copy, so the value cannot drift from the schema.
- **`verifyBackend`.** The input check already read the schema's enum, so all five
  types were accepted; the refusal message hard-coded two and now lists
  `BACKEND_TYPES`. The models-list URL and bearer header were already
  "azure-openai or not", and recognition already ran for every type but
  azure-openai — unchanged. New: the note when the recognized server is `vllm` or
  `llama-server` and differs from `type`, worded as `BACKEND-VERIFY.md` says; it is
  added right after recognition, so a `model-not-listed` report carries it too. No
  note for `unknown`.
- **Sample `verify`.** `--type` checks against `BACKEND_TYPES` (default
  `openai-compatible`); the refusal and the usage text list the five from it.
- **`GUIDE.md`.** §7 gains a Backend types part (the five, which require
  `api_key_env`, which force the service tier, choosing a type, `base_url` per type);
  the Prices service-tier bullet is limited to `openai` and `azure-openai`; §8 says
  recognition follows the answer and the note is to be shown; the sample command
  shows `--type`; the header dates the guide's update.
- **Tests.** `verifyBackend`: each of the five types reaches its URL with its header
  (and not the other one); a vLLM answer under `openai-compatible`, `openai` and
  `llama-server` and a llama-server answer under `vllm` carry the note, are still
  read as their server (`max_model_len`, `/props`), and a `model-not-listed` report
  carries it; a matching type and an `unknown` server under each non-Azure type
  carry none; the refusal message names the five. The two exact-report tests
  (llama-server, vLLM) now declare their server's type. Sample: `--type` accepts the
  five (azure-openai reads `/openai/v1/models`), refuses `bedrock` with the new
  message; the CLI exits 1 on it with the usage listing the types; the existing
  exit-code checks are unchanged.

Decisions made during the step:

- **One list, beside the type**: `BACKEND_TYPES` in `config/types.ts`, with
  `BackendType` derived from it — the TypeScript side already hand-follows the schema
  ("these types follow it"), so this keeps one list in TypeScript, and the test
  makes the schema the judge. Reading the enum from the schema at runtime was not
  taken: the type would then be a second, hand-written list.
- The note is not echoed on the sample's stderr: stderr carries the metadata to merge
  or the failure, and every report-level note (Azure's, "server not recognized")
  stays in the JSON on stdout. Revisit if operators miss it there.
- No separate check for `openai` without `api_key_env`: it is schema-only
  (step 1's fixture passes through ajv with no code).

Suite (2026-10-01):

- Control `npm test`: 565 passed, 0 failed. `npm run lint`: `tsc` clean,
  boundaries ok.
- `scripts/check-all.sh` stops at the gateway's `go test -race`: one red, the
  expected one — `internal/config` `TestValidFixtures/backend-types.json` (the
  gateway knows two types); step 3 clears it. gofmt, vet, staticcheck clean; every
  other package green.
- Run by hand after it: live-test kit gofmt, vet, staticcheck clean, self-test 16
  passed, 0 failed; cross-half e2e (`TestAcrossHalves`) green — not red as the
  acceptance criteria expected, since no fixture or example uses a new type yet.
