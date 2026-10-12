# Step 10 — review and green

**Status:** done (2026-10-12) — findings fixed (bceb441), green 3× in a row

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
- **C — Codex** (plain copy `/tmp/kaiak`, `CHANGES.diff` since `v0.14.1`;
  `AUDIT-B.md`): three confirmed findings, each with a reproduction test —
  - **B1 (high)**: an MP3 of measurable frames followed by free-format frames, which
    the scan skipped as junk and miniaudio decodes: 288 ms measured, 24.3 s decoded;
  - **B2 (medium)**: an MP3 whose sample rate changes, summed per rate by the scan,
    played by miniaudio at the first frame's rate: 24.9 s measured, 144.8 s decoded;
  - **B3 (medium)**: a prompt part with an empty filename, a file to the gateway and a
    text field to cpp-httplib — out of the input estimate the token limits admit by.

  B1 and B2 showed item 2's planned fix (the larger of the format's length and a scan
  of the whole file) is not enough: the fix pass measures a file only when the decoder
  that opens it provably reads it as the gateway does (Fix pass — gateway, item 2). B3
  is item 3.

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

### Fix pass — docs, control and kit

- **5 — the live kit and vLLM's seconds** (`scripts/live/`):
  - on vLLM the record must carry the answer's seconds — `usage.seconds`, or
    `verbose_json`'s `duration` rounded up — not the upload's length; the answer's
    seconds are held to the length the server hears (`asrHeardMillis`: the upload's
    3.6 s on a live run, so a live run still checks that vLLM counts 4);
  - the self-test's vLLM speech-to-text fake hears 2.5 s (`-audio-millis 2500`,
    `selfTestHeardMillis`): it reports 3 where the uploads measure 4;
  - `transcribe-verbose` skips only on vLLM's refusal `Currently do not support
    verbose_json for <model>` (`vllm/entrypoints/speech_to_text/base/serving.py`,
    0.30.0), read from the relayed error's message; any other `400` fails it;
  - `usage-log/transcribe-*` names where its seconds come from (the server's
    seconds, its duration, the upload measured by the gateway).
  - **Break-check:** `transcription_usage.go`'s `media` made to ignore the answer's
    seconds (`ok && false`, a one-line local change, reverted at once — the file's
    `git diff` empty after): `-self-test -kind vllm` failed `usage-log/transcribe-wav`,
    `-flac`, `-mp3` and `-verbose` (`kaiak.usage.audio.input.duration=4 s, want 3`);
    with the line restored it passes.
- **2 — the kit's MP3 under the strict rule:** the silent MP3 is 100 identical MPEG-1
  Layer III frames (32 kHz, 32 kbps, mono, 144 bytes each) and nothing else — one
  unbroken run, no tags, no trailing bytes — so it needs no change: with the new
  `mp3.go` the llama-server self-test still measures it at 4 s. Its comments and
  the docs no longer call it "the tone" (`main.go`, `transcription.go`, `uploads.go`;
  its upload's filename is `silence.mp3`); the `uploadRate` comment says the 32 kHz
  MP3 is resampled.
- **3 — kaiak-control:**
  - `endpoint-support.ts` and `config.test.ts`: the gateway holds the same table,
    read by its config validation; both held to `protocol/fixtures/backend-types/`;
  - the invalid-fixture runner (`test-support`) reads an optional `path` in a
    semantic `cases.json` entry and then requires every issue at exactly that JSON
    Pointer, as the gateway's runner does; a schema entry naming a path, and an
    entry with an unknown field, fail (the gateway decodes the file strictly).
    Checked with scratch fixtures before the paths landed (the right path passes, a
    wrong one fails `the issue's path`, a schema entry with a path fails), then
    against the 30 paths added to `config/invalid/cases.json`;
  - **found by it:** `key-hash-duplicate.json` — kaiak-control reported the
    duplicate at `/keys/k-alice/hash` (the later key in document order), the gateway
    at `/keys/k-app/hash` (the later key ID in sorted order). kaiak-control now walks
    the keys in byte order of their IDs, as the gateway does (`semantic.ts`), so
    which key is reported no longer depends on the document's member order.
- **9 — docs:** `docs/kaiak.md` (a model's client APIs are its `endpoints` list,
  each one its type serves); `control-plane.html` `Kaiak-Protocol: 7` (both places);
  `gateway.html` the provider row lists vLLM-Omni; DEPLOYMENT's next-release notes —
  llama-server's `501` and vLLM-Omni's image `503`, read as endpoint missing in
  0.14.0, are plain `5xx`s (no "again"); vLLM's `405` is `upstream_path_missing` on
  `vllm` only, relayed on other types (also under Clients); `upstream_path_missing`'s
  new message; the MP3 is silence, not the tone.
- **The gateway side's new rules, documented:**
  - DEPLOYMENT, Uploads: a part name or filename holding `\` or `"`, an empty
    filename, invalid UTF-8 in a text part, a name or a filename → `400
    invalid_body`, with the reason for each;
  - DEPLOYMENT, Speech-to-text: which llama-server uploads are taken (WAV and FLAC
    with headers its decoder accepts; an MP3 as one unbroken run of frames of one
    version, layer, sample rate and channel count, an ID3v2 tag skipped, nothing
    trailing but one ID3v1 tag), measuring only a fallback on vLLM; `n` capped by
    `global.max_n` (`400 n_too_large`); a transcription sent and never answered
    records 0 `audio_seconds_in`;
  - the next-release notes carry each (`n_too_large` joins the upload endpoints'
    codes); LIVE-BACKENDS: the fakes' 2.5 s, the `transcribe-*` rows,
    `transcribe-verbose`'s skip, the troubleshooting entries.
- **11 — `examples/config.json`:** `global.max_upload_bytes` (20 MiB); a `vllm-asr`
  backend and the model `whisper-large-v3-turbo` (`audio_transcriptions`); an
  `omni-edit` backend (`vllm-omni`) and the model `qwen-image-edit` (`images_edits`,
  `vision` true); both in the `users` defaults' `allowed_models`, with
  `audio_seconds_in_per_hour` 3600. README's description and DEPLOYMENT's Image edits
  and Speech-to-text point at them. Both halves' example tests check it.
- **Suite** (2026-10-12, both agents' changes in the worktree): `control/` `npm test`
  674 pass, 0 fail, 1 skipped (the memory store's reconnect case, as before); `npm
  run lint` clean; the kit's lint and `-self-test` pass for every kind;
  `scripts/check-all.sh` — `all checks passed` (3:49).

### Fix pass — gateway (2026-10-12)

Gateway, `protocol/` and `docs/specs/` share of the table above and of Codex's report
(`AUDIT-B.md`: B1, B2, B3). Not committed: the main session reviews and commits.

**1 — form names (A-H1, A2-L2).** `server/form.go` (`readDisposition`) refuses a name or
a filename holding `\` or `"`, `400 invalid_body`. The provider (`provider/form.go`)
no longer escapes: names and filenames reach it free of control characters,
backslashes and quotes and are written between quotes as they are — the refusal is the
invariant. Pinned by `TestFormRefusals` ("a name ending in a backslash, its filename
read as parameters" — the first reviewer's reproduction, a file part named `zzz\` with
filename `; name=url; x=`; a quote in a name; a backslash in a filename; a quote in an
RFC 2231 filename); the random-form generator and the provider's form test now use
names with spaces, `;` and `=` instead. Checked against the backends' readers: Python's
`email` parser (python-multipart's `parse_options_header`) reads the header Go's writer
makes, `name="zzz\\"; filename="; name=url; x="`, as `[name='zzz"; filename=',
name='url', x='"']` — the part becomes `url`; cpp-httplib (b11526) reads the name as
`zzz\\`, unescaped (scratch programs in `/tmp/kaiak-audit-b`, outside the repo).

**3 — empty filename (A2-M2; Codex B3).** Refused, `400 invalid_body`
(`readDisposition`). Pinned by `TestFormRefusals` ("an empty filename") and
`TestTranscriptionPromptWithAnEmptyFilenameIsRefused` — Codex's
`TestAuditBEmptyFilenamePromptCountsAgainstTokens` ported: the 2 000-byte prompt as a
text part is `429` on the group's 100 tokens a minute, the same prompt with
`filename=""` is `400 invalid_body`, nothing reaches the backend. Codex's oracle
re-run: `cpp-httplib: prompt is a TEXT field, 2000 bytes`.

**4 — invalid UTF-8 (A2-L1).** Refused in a text part's bytes (`readForm`), a name and a
filename (`readDisposition`), `400 invalid_body`; file parts are untouched. Pinned by
`TestFormRefusals` (a name, a filename, a text part).

**2 — uploaded audio (A2-H1; Codex B1, B2).** Measured only when the decoder that opens
it provably reads it as the gateway does (`accounting/upload_audio.go`, `mp3.go`):
- **Order.** `UploadAudioLength` follows `ma_decoder_init_memory`: `RIFF`…`WAVE` →
  WAV, else `fLaC`/`ID3`/sync → FLAC when dr_flac opens it, else MP3.
- **WAV** — a new in-memory walk mirroring `ma_dr_wav_init__internal` (RIFF, memory,
  no metadata): chunks skipped by size and pad, a chunk past the end ends the walk;
  the last `fmt ` before the first `data`, at least 16 bytes; an extension size whose
  extension lies within the file (dr_wav seeks over it); the extensible format's
  22-byte extension in a chunk ≥ 40; a `fact` chunk ≥ 4 bytes (dr_wav reads 4, then
  seeks `size − 4`: below 4 it fails); rate ≤ 384 000, channels ≤ 256, bits ≤ 64
  (`MA_DR_WAV_MAX_*`); the format rules shared with `wavClip` (`wavFormat.byteRate`).
  The length stays bytes present ÷ (rate × block) — never less than dr_wav's whole
  frames.
- **FLAC** — dr_flac's open: ID3 tags skipped, `fLaC`, STREAMINFO (34 bytes), then
  every metadata block whole within the file and at least dr_flac's minimum for its
  type (APPLICATION 4, VORBIS_COMMENT 8, CUESHEET 396, PICTURE 32), up to the last.
  Stricter than dr_flac on a truncated block (it reads one as the last, then fails to
  seek past a seek table) — refused, not modelled.
- **MP3** — one unbroken run of frames from the end of the leading ID3v2 tag (dr_mp3
  skips one) to the stream's end as dr_mp3 finds it (before an ID3v1 tag: the last 128
  bytes starting `TAG`; an `APETAGEX` footer there is unmeasurable), every frame of the
  first frame's version, layer, rate and channel count, none free-format, the last
  whole. The old resynchronizing scan (`nextMPEGFrame`) is gone.
- **Decisions of mine:**
  - *channel mode* compared as the channel count (mono vs two channels), not the
    2-bit mode: dr_mp3 reads stereo, joint stereo and dual channel alike as two
    channels; an exact match would refuse a stream mixing stereo and joint-stereo
    frames, which decodes the same;
  - *Xing/Info kept* (the longer of declared and scanned): dr_mp3 takes the declared
    count as its length, so the gateway must count at least it. Tightened: dr_mp3
    looks for the tag in *any* layer (it runs Layer III side-info parsing on the first
    sample-producing frame) and reads the tag's bytes from the file past the frame's
    end; and it reads the tag in the first frame that *decodes to samples*, which a
    crafted stream can make a later frame. So the tag bytes are read from the file;
    a tag in a Layer I/II first frame, in any later frame, or whose bytes run past the
    file is unmeasurable;
  - the FLAC truncated-block case and the WAV corner cases above refused rather than
    modelled (stricter than the decoder only).
- **Tests.** `TestMP3RateChangeIsUnmeasurable`, `TestMP3FreeFormatFramesAreUnmeasurable`
  (Codex's two tests ported, expecting unmeasurable); `TestUnmeasurableMP3Uploads` now
  also covers junk before/between/after frames, a second file, two ID3v2 tags, APE,
  `TAG` inside the last frame, version/layer/rate/channel changes, a Xing header later,
  a Layer II Xing header, Xing bytes past the file; `TestMP3UploadLength` adds stereo
  and joint stereo mixed, ID3v2 + ID3v1; `TestUnmeasurableWAVUploads` adds a rate,
  channels and sample size past dr_wav's limits, a 2-byte `fact`, a 14-byte `fmt `, an
  extension past the end, MP3 frames behind a header dr_wav refuses;
  `TestWAVUploadLength` adds the limits at their edge, an extension dr_wav seeks over,
  two `fmt ` chunks; `TestUnmeasurableFLACUploads` adds each type below its minimum, a
  block past the end, no block flagged last, MP3 frames behind a FLAC dr_flac refuses;
  `TestFLACUploadLength` adds every block type after STREAMINFO. Fuzz targets updated:
  `FuzzWAVUploadLength` re-derives the byte rate from the file's own `fmt ` fields by a
  walk of its own (item 10), `FuzzFLACUploadLength` re-walks the metadata, `FuzzMP3UploadLength`
  gained Codex's two shapes; each fuzzed 20 s, no failure.
- **Oracle** (miniaudio 0.11.25 from llama.cpp b11526, compiled outside the repo; the
  corpus generator was a temporary test, removed). Every measured file is decoded as
  llama-server decodes it (`ma_decoder_init_memory`, f32 mono 16 kHz); checked: the
  decoder chose the gateway's format; its own length (frames at the file's rate) ≤ the
  gateway's measure, exactly and in whole seconds; its 16 kHz length ≤ the whole
  seconds:

  ```
  cc -O1 -I/tmp/kaiak-audit-b /tmp/kaiak-audit-b/oracle.c -lpthread -lm -o /tmp/kaiak-audit-b/oracle-bin
  KAIAK_ORACLE_CORPUS=<dir> KAIAK_ORACLE_N=<n> KAIAK_ORACLE_SEED=<s> go test ./internal/accounting -run TestZZOracleCorpus   # temporary
  /tmp/kaiak-audit-b/oracle-bin <dir>
  ```

  | corpus | measured | decoded by wav / flac / mp3 | backend differs | decoder longer | in whole seconds | +1 resampler frame |
  |---|---|---|---|---|---|---|
  | seed 1, 60 012 files | 15 357 | 1 787 / 11 140 / 2 074 | 0 | 0 | 0 | 1 |
  | seed 2, 300 012 | 76 365 | 9 352 / 55 279 / 10 056 | 0 | 0 | 0 | 1 |
  | seed 3, 300 012 | 76 475 | 9 272 / 55 572 / 9 936 | 0 | 0 | 0 | 1 |
  | seed 4, 300 012 | 76 318 | 9 297 / 55 159 / 10 096 | 0 | 0 | 0 | 2 |
  | seed 5, 300 012 | 76 346 | 9 226 / 55 414 / 10 016 | 0 | 0 | 0 | 1 |
  | MP3 only, seed 11, 400 012 | 40 736 | 2 / 2 / 39 951 | 0 | 0 | 0 | 2 |
  | WAV only, seed 11, 400 012 | 43 082 | 36 945 / 2 / 6 | 0 | 0 | 0 | 1 |

  The measured files the decoder refuses (356; 1 678–1 766 per later seed) are
  255- and 256-channel WAVs — dr_wav takes them, miniaudio's converter caps channels
  at 254, so llama-server fails the request (no usage) — and MP3s whose frames decode
  to nothing; neither is an undercount. Sanity check of the oracle: the old reader's
  failures, marked "measured 0 s", are all caught — Codex's rate change (1 158 336
  frames at 8 kHz, 145 s), Codex's free-format tail (1 164 672 at 48 kHz, 25 s), and
  three fallthroughs to dr_mp3 (a WAV at 400 000 Hz, a WAV with a 2-byte `fact`, a FLAC
  with a 2-byte comment, each hiding 1 000 MP3 frames: decoder backend `mp3`, 27 s);
  the new reader measures none of the five. Codex's own decoder oracle re-run:
  `sample-rate change: length=2316672 … (144.792 s)`, `free-format tail:
  length=388225 … read=388224` — both now unmeasurable.
- **Found on the way (for the main session):**
  - *miniaudio's second round.* When dr_wav, dr_flac and dr_mp3 all refuse a file
    from memory, `ma_decoder_init_memory` tries the three again through stream
    callbacks whose seeks clamp instead of failing. A measured file is always opened
    in the first round, so this never decides a measured length; but a WAV with a
    `fact` chunk under 4 bytes then sends dr_wav's `seek_forward` through ~2³³
    clamped seeks — minutes of CPU in llama-server per request (the oracle hung on
    it). The gateway refuses such files on llama-server models (unmeasurable); a
    client sending them to llama-server directly is llama.cpp's matter.
  - *the resampler's extra frame.* miniaudio's decoded length at 16 kHz is
    `floor(frames × 16000 ÷ rate) + 1` for a rate that is a multiple of 16 000 (32, 48,
    96 kHz: `ma_calculate_frame_count_after_resampling`), one frame past the samples
    it decodes — a frame of silence. For a 48 kHz clip of exactly k seconds,
    llama-server allocates k s + 62.5 µs. The gateway counts the audio (k s), not the
    padding frame — written in the spec; say if it should round that frame up.
- **Spec** (`GATEWAY.md`, Accounting → Media usage → Speech-to-text → Measuring an
  upload, settled 2026-10-11): the "chosen by its magic bytes" claim replaced by
  miniaudio's order and the rule above, WAV/FLAC/MP3 rules rewritten, the second round
  and the resampler frame named; the llama-server bullet and the
  `audio_format_unsupported` errors row say why a WAV/FLAC/MP3 can still be refused.

**5 — speech-to-text `n` (ruling).** `inbound_transcription.go`
(`transcriptionSequences`): a whole number in decimal, else `400 invalid_type`; above
`global.max_n` `400 n_too_large`, `param: "n"`; below 1 counts as 1; it sets the
request's sequences (it multiplies no reservation: no output limit). The decimal
reading is shared with edits' `n` (`uploads.go`, `uploadWholeNumber`). Pinned by
`TestTranscriptionRefusals` (above max_n, `2.0`, past int64, a file, twice) and
`TestTranscriptionReadsItsParts` (8, `+3`, 0, −4). Spec: owned parts, errors row,
Limits → Output multiplicity.

**6 — the unanswered rule (ruling).** `GATEWAY.md`, Accounting → Billed from the moment
the request was sent: a transcription sent but never answered records 0
`audio_seconds_in`, as an unanswered image request records 0 images (settled
2026-10-11, with the user; the measured upload as input rejected for consistency);
`CONTROL-PROTOCOL.md`'s `images` and `audio_seconds_in` units point to it and to the
cut-short rule. No code change: `TestUnansweredTranscriptionRecordsNoAudio` pins it on
vllm and on llama-server (whose upload was measured): `504 upstream_timeout`, the input
estimate, 0 seconds, estimated and partial.

**7 — `KAIAK_UPLOAD_*` wiring (B-M4).** `e2e/timeouts_test.go`: the timeout gateway sets
`KAIAK_UPLOAD_READ_TIMEOUT_MS` to 600 ms (twice its 300 ms body-read deadline); an
upload stalled after its first part is answered `400 invalid_body` no sooner than
600 ms. `e2e/uploads_test.go`, `TestUploadBudgetFromTheEnvironment`: a 64 KiB
`KAIAK_UPLOAD_MEMORY_BYTES`; a 40 000-byte transcription read whole and held at a
backend that does not answer holds its share, so a 30 000-byte upload is `503
server_busy` (`Retry-After: 1`) before any backend, chat is `200`, and once the first
client goes the upload is read and answered. (A first version polled small uploads
while the holder grew; the poller's share could make the holder's last budget step
fail — racy, 4 of 8 runs — so the holder's arrival at the backend is the barrier
now; 10 runs in a row passed.)

**8 — wording (B).** `GATEWAY.md`: the one seconds rule stated before the per-type
figures; the 1000 parts as 1000 in all within Starlette's 1000 fields and 1000 files
(also `maxFormParts`' comment); Uploads → Parts gains the empty form, the first
boundary line, header names as tokens and values as visible characters, and the new
refusals with their reasons (backslash/quote, empty filename, UTF-8); `url`/`url[]`
file parts on edits pass (vLLM-Omni answers `422`); the upload deadline's timing
(Uploads and Lifecycle: set in the upload path after auth, the key's concurrency, the
content type and the declared length's cap); `server_busy` in the error-class list
covers the upload budget; the `invalid_body` and `n_too_large` rows; `CONTROL-PROTOCOL.md`
`images` points to the cut-short rule.

**9 — stale (B, my files).** `messages_test.go` (the Anthropic model list's comment:
models whose endpoints list names Messages; no "as before"); `errDuplicateMember`'s
comment names the repeated part; `protocol/fixtures/config/invalid/cases.json` drops
"(format 5)"; `protocol/fixtures/messages/usage-record/invalid/cases.json` drops "(no
endpoint produces it yet)".

**10 — weak tests (B).**
- `TestEndpointSupportFollowsTheSpec` fails when a column vanishes (checked by deleting
  the spec's last column: `the spec's endpoint support table has no column
  "audio_transcriptions"`) and when a row's cells do not match the header.
- `path` on semantic config cases: 30 of the 33 carry the JSON Pointer both halves
  report (`""` for the root, as kaiak-control reports `counters-exceeded`); the three
  that report two issues (`endpoints-not-served`, `group-cycle`,
  `group-depth-exceeded-two-levels`) carry none. The gateway's runner
  (`fixturetest.RunInvalid`) requires every issue at `path` — the semantics
  kaiak-control's runner already had (the set of issue paths equals `[path]`); a schema
  case naming a path is an error. Checked by breaking one path: `an issue at
  "/keys/k-app/hash", want every one at "/keys/k-app/group"`.
- Transcription usage: `{"duration":6.5}` (verbose_json) and
  `{"usage":{"type":"duration","seconds":5}}`.
- `e2e/zeroseries_test.go`: exactly 13 attempt-outcome series per deployment and 6
  retry-reason series per model and backend.
- `FuzzWAVUploadLength` re-derives the byte rate (above).
- `TestUploadCap`: the declared-length cases send a body that fails the test if read,
  with an explicit `ContentLength`.
- `TestCapAboveItsBudgetIsWarned`: asserts `kaiak.config.max_upload_size=26214400`,
  `kaiak.upload_budget.size=1000` (and the body warning's two sizes).

**11 — the cross-half flake.** Root cause in the test, proven from Codex's log
(`/tmp/kaiak-audit-b/crosshalf.log`; that run crossed 20:00 UTC):
- the speech/image subtest waited for the top of the hour and served at 23:00:00.004
  and .007 (local, +03:00) — 25 tokens inside `hourEdge` of 20:00 UTC, which
  `servedAmounts.counted` lets count in either hour (`used ≥ sure && used ≤ sure +
  edge`);
- the held-stream subtest served `held-stream` and `held-queued` (11 tokens each) at
  23:00:19.839 on gw-a. Its `allCounted` passed with those 22 tokens not yet at the
  sample — 22 ≤ the 25 of tolerance;
- the barrier before stopping the first sample waited for `kaiak_usage_queue_batches
  == 0`, which counts sealed batches only — the two records were in gw-a's filling
  batch (it seals every 5 s, at x.465/x.474 there). The first sample's last usage POST
  was at 23:00:19.490, its stop at 23:00:20.852: the two records never reached it;
- `tokens` restarts empty for the second sample, but gw-a's next batch (23:00:23.474)
  carried the two held records with the outage chat and the priced one: the second
  store's 20:00 window read 55 against 33 expected, then 66 against 44 after gw-b's
  SIGTERM flush — exactly the 22 extra.

Not a product bug: undelivered usage waits in memory and goes to whichever control plane
answers next, as the contract says. Fix (`e2e/sample_test.go`, `proxy_test.go`): the
barrier before the stop is now exact — for each gateway, the records the proxy saw the
sample accept (each batch once, by batch ID) equal the records the gateway settled
(`kaiak_usage_records_total` over all series, less `kaiak_usage_dropped_records_total`);
`counted` keeps its tolerance, needed for the hour's edge, but no longer serves as a
barrier. Runs (`go test -race -tags crosshalf -run '^TestAcrossHalves$' -count=1 ./e2e`):
the first after the fix **failed** — it crossed 21:00 UTC, and its output was lost
(only its tail was kept: the first sample stopped 70 ms after a usage POST, i.e. the
new barrier passed; the run was ~6 s shorter than a passing one, so an instant
assertion failed, not a wait — which one is unknown); the next 8 passed, and a 9th
started at 00:59:50 local to cross 22:00 UTC the same way passed every subtest.
Not established; if it recurs, keep the `-v` output.

**Verification.** `scripts/check-gateway.sh` green; `scripts/check-all.sh` green twice
(00:16 and 01:05, with the control-side fix pass in flight). Upload fuzz targets and
`FuzzFormReadsAsMimeMultipart` fuzzed 20 s each. No temporary file left in the repo
(the oracle corpus generator was a test file, removed); the oracle and corpora are in
`/tmp/kaiak-audit-b` (`oracle.c`, `oracle-bin`, `oracle*/`, `backslash_test.cpp`).

### Verification (2026-10-12)

- Both fix passes reviewed by the main session; committed as bceb441.
- `scripts/check-all.sh` on bceb441, **3× in a row, green**: 22:11:18–22:15:07,
  22:15:07–22:18:50, 22:18:50–22:22:37 UTC (`all checks passed` each), after one more
  green run on the uncommitted tree (22:06–22:10).
- The cross-half test run once more with `-v` across the top of an hour (22:59:15 →
  23:00:36 UTC, `ok kaiak/e2e 80.588s`): the barrier holds; the one failure lost in
  the fix pass did not recur.
- The `/tmp` copies (`/tmp/kaiak`, `/tmp/kaiak-audit-b`, `/tmp/kaiak-audit-prompt.md`)
  deleted; no process had its working directory in them.
