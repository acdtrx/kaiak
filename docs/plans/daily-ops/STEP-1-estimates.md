# Step 1 — input estimates (D1, D2)

**Status:** done (2026-09-25)

- **D1** the input estimate counts each inline media item (a string value starting with
  `data:` in chat content parts: `image_url.url`, `input_audio.data`, `file.file_data`, and
  any `data:` URL in known media fields) as a flat 1000 tokens (a documented constant;
  a config field only if trivial — decide), and estimates the rest of the body by bytes.
  One scan of the body the gateway already holds (no regex; token-level). Used by the
  limits reservation, the injected default output fitting and estimated records.
- **D2** the injected default output is fitted against the **largest single prompt's**
  estimate (per-sequence context), while the reservation keeps the total.
- Tests: [A]'s 450 KB photo (injected default not collapsed; reservation ≈ text + 1000);
  [B]'s 1.4 MB PNG under a 100k tokens/min limit (admitted); [B]'s 16×12 KB completions
  batch (per-prompt default not collapsed); estimated record for a cancelled image request.

## Result

- `accounting.EstimateInput(endpoint, body)` — one token-level `json.Decoder` pass over
  the body the gateway already holds (no regex; nesting bounded at 10000, a body that
  does not scan falls back to its size). Taken once per request by the inbound stage
  (`rq.input`) and read by the limits reservation (`Total`), the injected default
  output (`LargestPrompt`) and the meter's estimated records (`NewMeter` now takes
  the input estimate in tokens instead of the body size).
- Failing first (all four red before the change, green after), `internal/server/estimate_test.go`:
  - [A] 450 KB base64 photo, 32k context, default 4096 → injected 4096 (was 256);
    reservation ≈ text + 1000 + 4096 (was 115506).
  - [B] 1.4 MB PNG under 100k tokens/min → admitted (was 429, "needs 350053 tokens").
  - [B] 16 × 12 KB completions batch, 32k context, default 16384 → injected 16384 per
    prompt (was 256).
  - Streamed image request cancelled after it reached the backend → estimated record
    `tokens_in` ≈ text + 1000 (was 349797).
- Unit tests, `internal/accounting/estimate_test.go`: text by bytes; data-URL image;
  `https://` image URL = 1000; bare-string `image_url`; raw-base64 `input_audio.data`;
  a data URL in any field (`file.file_data`); text starting with `data: ` stays text;
  token IDs one per ID (completions, embeddings); numbers elsewhere are text; batch
  largest prompt; token-ID-list batch; the same image at 1 KB and 1.4 MB estimates the same.
- Existing test updated to the new contract: `TestOutputMultiplicityMultipliesTheReservation`'s
  two token-ID cases now expect one token per ID (15 and 19 instead of 13 and 18 by bytes).
- Spec: `docs/specs/GATEWAY.md` — new "The input estimate" entry (settled 2026-09-25)
  under Limits; the injected-default, multiplicity, check-and-reserve, drift and
  Accounting → Estimation entries point at it.
- Suite: `GOFLAGS=-count=1 scripts/check-all.sh` — all green (gateway race tests,
  staticcheck, live kit self-test, control 472 pass, lint, cross-half e2e):

  ```text
  ok  	kaiak/internal/accounting	2.820s
  ok  	kaiak/internal/server	24.074s
  ...
  boundaries ok
  ==> cross-half e2e (sample control plane + two gateways)
  ok  	kaiak/e2e	43.382s
  all checks passed
  ```

## Decisions made during the step

- **Flat 1000 tokens is a constant** (`accounting.InlineMediaTokens`), not a config
  field: a field would touch config, the protocol schemas and fixtures and
  `kaiak-control` for a figure settlement replaces anyway.
- **What counts as a media item:** any string that is a data URL, checked structurally
  (`data:`, a header ≤ 256 bytes with no whitespace naming a media type or `;base64`,
  then a comma) — in any field, so `file.file_data` and future part types are covered
  without a list; plus `image_url.url` (or a bare `image_url` string) in **any form**,
  `https://` included (the backend fetches and bills it); plus `input_audio.data`,
  which OpenAI sends as raw base64 with no `data:` prefix. The data-URL header check
  replaces a length threshold: text such as a pasted `data: {...}` event-stream line
  is not a data URL (whitespace in the header), however long.
- Not counted as media: vLLM's `video_url` / `audio_url` remote URLs (they stay text,
  as before) — a video costs far more than 1000 anyway; revisit if used.
- **Per-sequence figure:** the request less every completion-batch prompt but the
  largest (so `suffix` and other fields still count); chat's equals the total.
  A flat token-ID list is one prompt, as the sequence count reads it.
- **Token IDs count one token each** in a completion's `prompt` and an embeddings
  `input` (the latter also for the reservation).
- Rounding: text is estimated over the summed text bytes, so a body with no media or
  token IDs estimates exactly as before (`ceil(len(body) / 4)`) — existing tests unchanged.

## Decisions for the user to confirm

1. `https://` image URLs count 1000 like inline images (recommended; implemented).
2. Media detection = any structurally valid data URL anywhere + `image_url` (any form)
   + `input_audio.data`; `video_url`/`audio_url` remote URLs not counted as media.
3. No config field for the 1000-token figure.
4. A PDF (`file.file_data`) counts 1000 like any media item, though a long PDF can cost
   far more; settlement corrects it when the backend reports usage.
