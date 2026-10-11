# Step 10 — review and green

**Status:** not started

## Intent

Review the whole branch before the live run, fix what the user rules in, and leave the
suite green three times in a row.

## How

- **Two agent reviews of the worktree:** one on correctness and security (the
  multipart reader and the rebuild, the audio readers, the budgets and deadlines, the
  routing and circuit changes), one on spec conformance and tests.
- **An independent Codex review** of a plain copy (`/tmp/kaiak`, no git, no plan or
  review folders), with the diff since `v0.14.1` as `CHANGES.diff`. The user runs it;
  its report is `AUDIT-B.md`.
- **Triage:**
  - every finding checked against the code, its reproduction run;
  - fixed with a test when it affects legitimate daily use or is high-impact;
  - design-level findings put to the user;
  - the rest recorded here.
- Decisions 23–30 (made while planning) confirmed with the user, or changed.

## Acceptance criteria

- A table of findings: source, verdict, outcome.
- `scripts/check-all.sh` green **3× in a row** on the final code, recorded.
- The `/tmp` copies deleted, with no Codex processes left in them.

## Result

### Reviews (2026-10-11)

- **A — correctness and security** (agent, worktree): stopped partway by a safety
  check after one confirmed finding; the areas it did not reach went to **A2**.
- **A2 — correctness and security, the rest** (agent, worktree).
- **B — spec conformance, tests, docs** (agent, worktree).
- **C — Codex** (plain copy `/tmp/kaiak`, `CHANGES.diff` since `v0.14.1`): pending.

### The user's rulings (2026-10-11)

- **An unanswered transcription records 0 `audio_seconds_in`**, as an unanswered image
  request records 0 images (phase A) — stated in the spec, dated; no code change.
  (B-M1, A2-M1; the alternative put to the user — billing the measured upload as input,
  as `tokens_in` is — declined.)
- **Speech-to-text's `n` is owned and capped by `global.max_n`**, as chat's is
  (B-M2, A2-L3: vLLM's `use_beam_search` takes `n` as its beam width, uncounted).
- **Decisions 23–30 confirmed** as built.

### Findings to fix (one pass, after Codex's report)

| # | Source | Severity | Finding | Fix |
|---|---|---|---|---|
| 1 | A-H1, A2-L2 | high | The rebuilt `Content-Disposition` escapes `\` and `"` as Go's writer does; python-multipart (vLLM, vLLM-Omni) and cpp-httplib (llama-server) misread a name ending in `\` — a crafted file part reaches the backend as another field (`url`, `lora`, `n`, `vllm_xargs`…) | refuse a name or filename holding `\` or `"` (`400 invalid_body`); test the reproduction |
| 2 | A2-H1 | high | miniaudio tries WAV, then FLAC, then MP3 — dr_mp3 searches the whole buffer — so a header the gateway measures but dr_wav/dr_flac refuses can hide MP3 audio llama-server decodes: `audio_seconds_in` under-counted without bound | measure as the larger of the format's length and an MP3 frame scan over the whole file (never less than the decoder falls through to); correct the spec's "chosen by magic bytes" claim |
| 3 | A2-M2 | medium | an empty `filename` is a file part to the gateway, a text field to cpp-httplib | refuse an empty filename (`400 invalid_body`) |
| 4 | A2-L1 | low→fix | invalid UTF-8 in a text part or a part name makes llama-server answer `500` (a circuit failure a client can cause at will) | refuse invalid UTF-8 in text parts, names and filenames (`400 invalid_body`) |
| 5 | B-M3 | medium | the live kit cannot tell vLLM's reported seconds from the gateway's measurement (both 4) | expect the answer's seconds; the self-test's vLLM fake at a length that rounds otherwise |
| 6 | B-M4 | medium | nothing tests the binary wires `KAIAK_UPLOAD_*` into the API | e2e: an upload stalled past `KAIAK_UPLOAD_READ_TIMEOUT_MS`; a small `KAIAK_UPLOAD_MEMORY_BYTES` → `503`, chat served |
| 7 | rulings | — | transcription `n` capped by `global.max_n`; the unanswered rule in the spec | as ruled |
| 8 | B (low) | low | spec/code wording: the per-type seconds rule vs the one rule; MP3 "ends the file" (within 4 bytes); the 1000-part Starlette claim (1000 fields + 1000 files); three part-grammar refusals missing from the spec; `url`/`url[]` file parts on edits; Lifecycle's upload deadline timing; `server_busy` covering the upload budget; `images` in CONTROL-PROTOCOL | reword the spec and comments |
| 9 | B (low) | low | stale: `endpoint-support.ts` and `config.test.ts` comments (modules' tables); `messages_test.go` comment; `docs/kaiak.md` "follow from backend types"; `cases.json` "(format 5)" and "(no endpoint produces it yet)"; `control-plane.html` `Kaiak-Protocol: 6`; `gateway.html` provider row; `errDuplicateMember`'s comment; DEPLOYMENT's "503 … again" and the missing 405 / new-message notes; the silent MP3 called "the tone" | fix each |
| 10 | B (low) | low | weak tests: `TestEndpointSupportFollowsTheSpec` (a vanished column passes); semantic fixture paths compared on the gateway side only; the "name escaped" case without an escape; zero-series not exact; the WAV fuzz checks the reader's own byte rate; `TestUploadCap` cannot see the declared-length check; the upload-cap warning's attributes; `transcribe-verbose` skips on any `400` | strengthen each; `path` in semantic `cases.json`, compared by both halves |
| 11 | B (low) | optional→fix | `examples/config.json` shows none of phase B | an edit model, an ASR model, `max_upload_bytes`, an `audio_seconds_in_*` limit |
