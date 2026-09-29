# Step 3 — sample CLI, docs, live proof

**Status:** done (2026-09-29)

## Intent

Give operators a way to use the helper today: a `verify` command in the sample. Prove
the helper against real backends, and bring the docs in line.

## Files likely touched

- `control/sample/src/verify/index.ts` (+ test) and `control/sample/src/verify-cli.ts`,
  mirroring `keygen`:
  - usage: `npm run verify -w sample -- --base-url <url> [--type azure-openai]
    [--api-key-env <NAME>] [--model <name>] [--timeout-ms <n>]`;
  - prints the report as JSON and, with `--model`, the `metadata` block to paste;
  - exit status 1 on failure.
- `control/sample/package.json`: the `verify` script.
- `control/kaiak-control/GUIDE.md`:
  - a section on `verifyBackend`: when to call it (add backend, add model), showing
    the report, hints to confirm by hand;
  - admin-only, per hard rule 5's spirit.
- `docs/specs/GATEWAY.md` Model metadata, and `docs/kaiak.md` line 83: metadata is
  declared; `verifyBackend` fills it in at add time.
- `AGENTS.md` Project Facts: the `verify` command.

## Decisions made during planning

- **No UI in the sample page.** The sample's config is a file, so a CLI is where the
  operator is.

## Acceptance criteria

- **Live, read-only** (no `cria` changes; the running models are only queried):
  - `dgx.local:11434` → server `llama-server`, context 262144, vision true, tools
    true, reasoning hint true;
  - `llama-embed.local:11435` → context 32768, no reasoning hint.
  - Paste the output here.
  - A live vLLM if one is up; otherwise say so.
- `scripts/check-all.sh` green 3× in a row. OVERVIEW's verification box filled in.

## Result

Built:
- `control/sample/src/verify/index.ts` — `runVerify(args, env)`: parses the arguments,
  reads the API key from the variable `--api-key-env` names, calls `verifyBackend`,
  returns `{ kind: "report", report, stdout, stderr }` or `{ kind: "error", message }`
  (bad arguments, an unset or empty variable, `verify-input-invalid`). `explain()`
  writes the lines for a person. Exported from the sample's entry (`VerifyResult`
  type by name: `export type *` would clash with keygen's `USAGE`).
- `control/sample/src/verify-cli.ts` — the process entry; `verify` script in
  `control/sample/package.json`.
- `verify.test.ts` — 8 tests: argument and env errors, input `verifyBackend` refuses
  (userinfo, trailing slash, model name, timeout, a credential with a newline —
  message never shows it), the happy path with and without `--model` against an
  in-process fake llama-server, `model-not-listed`, the key sent as a bearer
  credential and absent from all output, the process's exit status 0/1 and streams.
- Docs: `GUIDE.md` §8 "Verifying a backend when it is added" (later sections
  renumbered 9–12, the one `§10` reference moved to `§11`) and a row in the
  division-of-labor table; `GATEWAY.md` Model metadata and `docs/kaiak.md` (domain
  model; "Out of v1" now lists reasoning-effort discovery instead of metadata
  discovery, matching the backlog); `AGENTS.md` command line;
  `docs/ARCHITECTURE.md` sample subsystems and graph (`verify-cli.ts → verify →
  kaiak-control`) — not in the file list, but the sample's shape changed.

Decisions:
- **Output layout: stdout is only the report, pretty JSON** — always parseable
  (`| jq`), pass or fail. Everything for a person goes to stderr: on a failed check
  `verify: <code>: <message>`; with `--model` and a passing check, the `metadata`
  as one line of JSON to merge, the hints to confirm by hand (named with their
  values), and what is still to decide. A single `{report, metadata}` object was
  rejected: the report already holds `metadata`. Text after the JSON on stdout was
  rejected: it breaks `jq`.
- **Exit status**: 0 when `report.ok`; 1 on a failed check (report still printed) and
  on errors (nothing checked: message and usage on stderr, stdout empty), like keygen.
- `--type` defaults to `openai-compatible`; `--timeout-ms` must be digits, the range is
  `verifyBackend`'s own check.

Live, read-only, 2026-09-29, from the worktree's `control/`:

- **DGX was not idle.** `cria status` showed `qwen38-27b-nvfp4` (vLLM) running,
  up 46 min, started outside this step. Per the brief nothing was started or stopped
  on the DGX; the running vLLM was only queried (GETs). **The llama-server chat check
  (`qwen38-27b`, context 262144, vision, tools and reasoning hints) was not run**:
  it needs the vLLM stopped. The same server code path is covered live by the
  embedding host below, and the chat values by the fake built from the 2026-09-29
  capture (step 2).
- **DGX llama-server, run afterwards by the main session** (the user approved the
  swap): the vLLM (picks context=256k mtp=2 vision=on) was stopped, `qwen38-27b`
  started (cache=q8 context=256k mtp=3 vision=on), checked, stopped, and the vLLM
  restarted with the same picks. `--model unsloth/Qwen3.8-27B-GGUF:UD-Q4_K_XL` →
  exit 0: server `llama-server`; context 262144 from
  `default_generation_settings.n_ctx`; capabilities vision true (hint false), tools
  true and reasoning true (hints); `metadata`
  `{"context_length":262144,"capabilities":{"vision":true}}`; no notes.
- vLLM `dgx.local:11434`, `--model unsloth/Qwen3.8-27B-NVFP4` → exit 0:

  ```text
  {"ok": true, "server": "vllm",
   "models": [{"id": "unsloth/Qwen3.8-27B-NVFP4", "context_length": 262144,
     "sources": {"context_length": {"url": "http://dgx.local:11434/v1/models", "field": "max_model_len", "hint": false}},
     "notes": []}],
   "metadata": {"context_length": 262144}, "notes": []}
  stderr:
  Merge into the model's "metadata" (reported values only, hints left out):
    {"context_length":262144}
  Still to decide by hand: capabilities.streaming, capabilities.tools, capabilities.vision, capabilities.reasoning, reasoning_efforts (when reasoning is true)
  ```
  Without `--model`: the same report without `metadata`, stderr empty, exit 0.
  `--model does-not-exist` → exit 1, `ok: false`, `models` still listed, stderr
  `verify: model-not-listed: http://dgx.local:11434/v1/models does not list the model "does-not-exist"`.
- Embedding llama-server `llama-embed.local:11435`,
  `--model /models/qwen3-embedding-0.6b-q8_0.gguf` → exit 0:

  ```text
  {"ok": true, "server": "llama-server",
   "models": [{"id": "/models/qwen3-embedding-0.6b-q8_0.gguf",
     "context_length": 32768, "capabilities": {"vision": false, "tools": true},
     "sources": {"context_length": {".../props", "default_generation_settings.n_ctx", hint false},
                 "capabilities": {"vision": {"modalities.vision", hint false},
                                  "tools": {"chat_template_caps.supports_tools", hint true}}},
     "notes": ["reasoning not reported: http://llama-embed.local:11435/props has no chat_template_caps.supports_reasoning_effort"]}],
   "metadata": {"context_length": 32768, "capabilities": {"vision": false}}, "notes": []}
  stderr:
  Merge into the model's "metadata" (reported values only, hints left out):
    {"context_length":32768,"capabilities":{"vision":false}}
  Hints, to confirm by hand: tools true
  Still to decide by hand: capabilities.streaming, capabilities.tools, capabilities.reasoning, reasoning_efforts (when reasoning is true)
  ```
  (abridged: sources' URLs shortened here.) Without `--model`: the same model entry,
  no `metadata`, exit 0.
- DGX end state: unchanged from the start — `qwen38-27b-nvfp4` still running (pid
  557301), one `vllm serve` process, no llama-server. No cria config touched.

Seen live: the report's model objects print `sources` and `notes` before
`context_length`/`capabilities` (the helper adds values after creating the entry).
JSON key order is no contract, but the spec's example reads values first; left as is.

Suite (2026-09-29, in the worktree):
- `npm run lint`: `tsc` clean, `boundaries ok`.
- `scripts/check-all.sh` 3× in a row, each `all checks passed`: control
  `tests 540 · pass 540 · fail 0` (8 new) each run; cross-half e2e
  `ok kaiak/e2e 49.062s`, `48.781s`, `48.725s`. The gateway's Go packages came from
  the test cache (the gateway is unchanged on this branch).
