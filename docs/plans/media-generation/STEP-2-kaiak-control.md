# Step 2 — kaiak-control

**Status:** done — reviewed and committed 2026-10-09

## Intent

`kaiak-control` reads and validates config format 6 and speaks protocol 6. Its usage
intake, totals and storage carry the three new units and the per-hour image and audio
windows. It knows the `vllm-omni` type, and the sample shows a speech model and an
image model.

## Files likely touched

- `control/kaiak-control/schema/` — synced from `protocol/schema/`.
- `control/kaiak-control/src/config/types.ts`:
  - the units (`UsageUnit`);
  - the limit types;
  - the caps;
  - `BACKEND_TYPES`.
- `control/kaiak-control/src/usage/`:
  - aggregation: each new unit summed into its own window, beside tokens and cost;
  - windows.
- `control/kaiak-control/src/messages/` — the totals message.
- The protocol and format version constants.
- `control/kaiak-control/src/storage/` and `store-contract/`: the store keeps the new
  windows' totals. Check whether the stored shape changes, and what the contract
  tests need.
- `control/kaiak-control/src/backend-verify/`: `vllm-omni`, read like `vllm` (its
  models list).
- `examples/config.json`:
  - a `vllm-omni` backend with Qwen3-TTS;
  - a `vllm-omni` backend with Qwen-Image;
  - image and audio limits on a group, so the example shows them.
- `control/sample/`: the page shows the new totals, if it shows token totals.
- Tests beside each.

## Decisions made during planning

- **Each new limit type has its own window total,** summed from the records' units, as
  `tokens_per_hour` is from tokens. No shared "media" total.

## Acceptance criteria

- `kaiak-control` passes the step 1 fixtures.
- Usage records with the new units aggregate into the new hourly totals, and the totals
  message carries them.
- The store contract tests pass for the new windows.
- `npm test` and `npm run lint` are green.
- `scripts/check-all.sh` run and recorded. Expected reds: the gateway's fixture and
  version tests, and the cross-half e2e — both cleared by step 3.

## Result

### What changed

- `control/kaiak-control/schema/`: synced from `protocol/schema/` (`npm run
  sync-schemas`): `common`, `config`, `status`, `totals`, `usage-record`.
- `src/protocol/index.ts`: `PROTOCOL_VERSION` 6. `src/config/types.ts`:
  - `format_version: 6`;
  - `BACKEND_TYPES` gains `vllm-omni` (after `vllm`, the schema's order);
  - `LIMIT_TYPES`, a value in the schema's order, with `LimitType` derived from it
    (pinned to the schema's enum by a test; exported from `config/` for the counter
    bound and the test, not from the package);
  - `UsageUnit` gains `images`, `audio_seconds_out`, `audio_seconds_in`; `PriceUnit` is
    now the four priced token units by name (the media units are not price units);
  - `Global` gains `max_images_per_request`, `max_speech_batch_items`.
- `src/config/semantic.ts` (`countCounters`): five per scope, derived — the limit types
  that are not per-minute (`COUNTERS_PER_SCOPE`); the per-minute set gains the three
  media per-minute types; the message says the count; the 50 000 bound unchanged.
- `src/messages/`: `TotalsLimitType` and `COUNTED_TYPES` gain `images_per_hour`,
  `audio_seconds_out_per_hour`, `audio_seconds_in_per_hour` (the totals schema's
  order). The totals message needs nothing else: it lists every scope and type with
  usage, and the new types are windows like `tokens_per_hour`.
- `src/usage/aggregate.ts`: `amountFor` per counted type — tokens as before, `images`,
  `audio_seconds_out`, `audio_seconds_in` each into its own hour window, the cost
  into the month; a speech record counts toward both its token and its audio window.
  `src/usage/windows.ts`: every per-hour type's window is the same UTC hour
  (`windowsAt`); `formatWindowStarts` names all five, so `readTotals().windowStarts`
  carries them.
- `src/storage/`: the stored shape is unchanged — a window is still `(group, type,
  windowStart, used)`; `type` takes the three new values and `WindowStarts` has five
  keys. The memory store needed no change (it compares each window with
  `current[type]`). Comments name the new units.
- `src/store-contract/`: `CURRENT`/`PREVIOUS` cover every counted type; a new test
  writes a window of every counted type for global and a group, in the current and
  previous windows, from two handles, and checks they sum apart, read in their own
  window and drop past it — so a database store that lists the types (an enum, a check
  list, a query per type) fails until it takes all five.
- `src/backend-verify/`: `vllm-omni` reads its list at `<base>/models` with a bearer
  credential, as `vllm`; a rule `listedAs: "vllm"` keeps the "use type" note off a
  `vllm-omni` backend whose list says `vllm` (it still reads as `server: "vllm"`,
  context from `max_model_len`). A llama-server answer under `vllm-omni` still gets
  its note — the spec exempts only the `vllm` reading.
- `control/sample/src/page/sections.ts`: the totals table names each limit's unit
  (requests, tokens, images, audio seconds out / in) per minute and per hour; the intro
  names images and seconds of audio; recent usage gains Images, Audio out (s), Audio in
  (s) columns.
- `control/kaiak-control/GUIDE.md`: format and protocol 6; the record's units; five
  counters per scope (about 10 000 groups); window totals and the window type for
  store authors; the LiteLLM import's audio and image rates (counted and limited, never
  priced — the old reason, "endpoints the gateway does not serve", no longer held);
  `vllm-omni` and the type note (§7, §8).
- `examples/config.json`: format 6; backends `omni-tts` and `omni-image` (`vllm-omni`);
  models `qwen3-tts` (`Qwen/Qwen3-TTS-12Hz-1.7B-CustomVoice`, `context_length` 32768 —
  the talker's `max_position_embeddings` in the model's `config.json`; output limit
  2048 / 4096, vLLM-Omni's default and bound) and `qwen-image` (`Qwen/Qwen-Image`,
  `context_length` 1024 — `tokenizer_max_length` in vLLM-Omni v0.30.0's
  `pipeline_qwen_image.py`, which refuses a longer prompt); both with every capability
  false but `streaming` for speech. The `users` group's `child_defaults` allow both and
  add `images_per_hour` 40 and `audio_seconds_out_per_hour` 900.
  `examples/local-config.json`: format 6.
- Tests: `LIMIT_TYPES` pinned to the schema; the counter-bound test rebuilt on the
  five-per-scope fixture (each case at or one past the bound, the per-minute media
  types included) and the no-limits case made exact (9 998 groups at the bound, one
  more past it); media units into their windows beside tokens and cost, with the totals
  message validated (`usage.test.ts`); a media window rolling over by the hour; dropping
  past windows of every type; the speech and image record fixtures reaching a gateway's
  stream in their own windows (`status-totals.test.ts`); `vllm-omni` in backend-verify
  (its list, no note, the llama-server note, the URL and header cases, no-note-when-
  unknown); the sample page's media limits and columns; version and type-list strings
  where tests pin them (protocol, verify messages, the sample's verify help).

### Notes for review

- **Fixture value:** `config/valid/full.json` gives `qwen-image` `context_length` 4096;
  vLLM-Omni v0.30.0 accepts at most 1024 prompt tokens for Qwen-Image. Harmless in a
  fixture; the example uses 1024.
- **README** (step 3 of the quick start) enumerates the example's hosts and does not
  name the two vLLM-Omni ones — left to step 10, which owns `README.md`.

### Suite (2026-10-09)

- **Control `npm test`**: 655 tests, **654 pass, 0 fail**, 1 skipped (the store
  contract's catch-up test, skipped for the in-memory store as before); no hang, no
  `--test-timeout` needed.
- **Control `npm run lint`**: passes (`tsc`, boundaries ok).
- **`scripts/check-all.sh`**: stops at the gateway's `go test` (gofmt, vet, staticcheck
  2026.2.1 on go1.27.1 and the telemetry boundary pass). **75 tests fail in 7
  packages** — step 1's 74, unchanged per package, plus `TestExampleConfigs` in
  `internal/config` (the examples now at format 6, with `vllm-omni` and media limit
  types): `cmd/kaiak` 15, `internal/config` 24, `internal/control` 32,
  `internal/limits` 1, `internal/metrics` 1, `internal/provider` 1, `internal/server`
  1 → **step 3** (format, protocol, units, limit types, caps, error codes), and
  **step 4** for the `vllm-omni` type (`TestBackendTypesAreTheSchemaEnum`, the
  fixtures and example naming it), unless step 3 takes the type into config. The
  other 13 packages pass, `e2e` included.
- **Later stages, run on their own:**
  - live-test kit (`scripts/live`): gofmt, vet, staticcheck pass; `go run .
    -self-test`: 34 passed, 1 skipped, every kind.
  - **cross-half e2e** (`go test -race -tags crosshalf -run '^TestAcrossHalves'
    -count=1 ./e2e`): **red, as expected until step 3**, for the version mismatch
    only: `TestAcrossHalves` and `TestAcrossHalvesReplicas` — the sample refuses the
    Go-built config (`/format_version must be equal to constant`: the gateway's
    `FormatVersion` is 5) and its stream answers the harness `400` (`Kaiak-Protocol`
    from the gateway's `ProtocolVersion`, 5).
