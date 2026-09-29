# Step 3 — sample CLI, docs, live proof

**Status:** not started

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

(not started)
