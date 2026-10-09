# Step 10 — live kit and docs

**Status:** not started

## Intent

- **One kit run checks a real vLLM-Omni server** for speech and one for images.
- **An operator can serve Qwen3-TTS and Qwen-Image behind kaiak** from
  `DEPLOYMENT.md` alone.

Only the kit's `-self-test` runs in the suite.

## Files likely touched

- `scripts/live/`: a `vllm-omni` kind, or flags for a speech server and an image server
  beside a run of another kind — whichever fits the kit's structure. Checks:
  - **speech:**
    - each mode answers audio;
    - usage exact for the whole clip and SSE, estimated for raw audio;
    - `audio_seconds_out` close to the clip's real length (decoded locally);
    - a compressed format refused;
    - `ref_audio` with a URL refused;
  - **batch:** usage summed, the cap;
  - **images:** an image answered, `images` counted, `n` capped, `file`, `url` and
    `lora` refused;
  - **limits:** an image limit and an audio limit refusing;
  - **endpoints:** chat to the image model is kaiak's `400 endpoint_not_served`; speech
    sent to the image model (vLLM-Omni's answer, which step 1 could not settle from
    source) is recorded;
  - the self-test against the fakes.
- `docs/testing/LIVE-BACKENDS.md`: the new kind and checks; vLLM-Omni's start commands.
- `docs/DEPLOYMENT.md`:
  - a Speech and images section: install vLLM-Omni (its own venv, the vLLM version it
    pins);
  - `vllm serve <model> --omni` for Qwen3-TTS and Qwen-Image;
  - one backend per model;
  - the formats accepted;
  - the units, limits and caps;
  - the upgrade note for format and protocol 6 (both halves together).
- `README.md`, `docs/ARCHITECTURE.md`, `docs/architecture/gateway.html`: the endpoints
  and the type.

## Decisions made during planning

- **Real audio length in the live check** is decoded on the Mac (WAV header and bytes)
  and compared with the gateway's `audio_seconds_out`, within rounding.

## Acceptance criteria

- `go -C scripts/live run . -self-test` passes, covering every new check.
- `scripts/check-all.sh` green.

## Result

(filled in when the step is done)
