# Step 12 — live run

**Status:** not started

## Intent

The real vLLM-Omni server confirms what steps 1–10 built from its source: the speech
modes, the batch, image generation, their usage and units, the refusals, and the
`DEPLOYMENT.md` setup. Then the branch merges.

## Files likely touched

- This file: commands, versions, model revisions, the kit's output, captured real
  answers (no long base64).
- Fixes for anything the run shows, each with its test.
- `docs/plans/media-generation/OVERVIEW.md`: the verification status.

## Decisions made during planning

- **On the DGX:**
  - vLLM-Omni at its latest release, in a venv of its own (the existing vLLM 0.30.0
    venv stays untouched);
  - Qwen3-TTS (CustomVoice) and Qwen-Image.
- **The DGX rules of the rerank run apply:**
  - record `cria status --json` and `vllm.sh status` first;
  - stop the user's model only as needed;
  - restore and verify it at the end;
  - no sudo, no changes to cria's or vLLM's config;
  - keep the new venv and the downloaded weights, listed with sizes.

## Acceptance criteria

- The kit's speech and image checks pass against the real server.
- The real answers match the fake backend's shapes in every field the gateway reads.
  Otherwise the fake is fixed first.
- `scripts/check-all.sh` green after any fix.
- The user's model is restored and verified.
- The branch rebases onto `main` and merges ff, after the user's go.

## Result

(filled in when the step is done)
