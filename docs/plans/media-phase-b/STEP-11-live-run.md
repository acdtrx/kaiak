# Step 11 — the live run on the DGX

**Status:** not started

## Intent

Run the live-test kit against real servers on the DGX:
- Qwen-Image-Edit on vLLM-Omni;
- an ASR model on vLLM;
- an ASR model on llama-server.

Confirm that their real answers match the fake backend's shapes in every field the
gateway reads, and that the measured seconds match the servers' own.

## How

- **Ask the user when the DGX is free.** Then record `cria status --json` and
  `~/spark-setup/vllm.sh status`, stop the user's model through cria only, and restore
  exactly that state afterwards, verified.
- **No changes outside the test setup:** no sudo, no edits to cria or vLLM configs or
  existing venvs.
- **Servers:**
  - vLLM-Omni 0.30.0 in `~/opt/vllm-omni-kaiak` with Qwen-Image-Edit-2511 (about
    60 GB with its text encoder; downloaded once and kept);
  - plain `vllm serve` from the same venv with an ASR model (Qwen3-ASR or Whisper
    large-v3, the smaller that the kit's checks need);
  - llama-server from the kept build in `~/opt/llama.cpp-kaiak-rerank` with a GGUF
    ASR model and its audio projector (Qwen3-ASR or Voxtral).
- **Per server:** the kit's kind run against kaiak in front of it. Captures of every
  answer shape the gateway reads, compared with the fake backend. Each finding fixed
  or put to the user.
- **Kept afterwards** for re-tests: the weights and builds.

## Acceptance criteria

- The kit's checks pass on all three servers (counts recorded).
- The fake backend matches every real answer shape the gateway reads, or is
  corrected (with tests).
- The measured seconds (llama-server) equal the decoded length, rounded up. vLLM's
  reported seconds equal the files' known lengths.
- The user's model restored and verified.
- `scripts/check-all.sh` green after any fix, 3× in a row if code changed.
