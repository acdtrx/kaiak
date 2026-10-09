# Step 3 — gateway: units, limits, caps, versions

**Status:** done — reviewed and committed 2026-10-09

## Intent

The gateway reads config format 6, speaks protocol 6, and carries the three new units
from a usage record through cost, limits, totals and metrics. It enforces the six new
limit types and the two caps. No endpoint produces the new units yet; the tests drive
them directly.

## Files likely touched

- `gateway/internal/config/`:
  - the units and the unit sets (`TokenUnits`, `PricedUnits`, `CountedUnits`…): decide
    where each new unit belongs (not priced, decision in step 1's backlog entry);
  - the limit-type table: new measures for images and the two audio units, minute
    windows local, hour windows counted;
  - the caps;
  - format 6.
- `gateway/internal/accounting/`: records carry the new units. Cost ignores them
  (unpriced).
- `gateway/internal/limits/`:
  - the new measures;
  - reservation: images reserve the request's count; audio generated reserves nothing
    and settles;
  - totals from the control plane per new window.
- `gateway/internal/control/`: protocol 6; the totals message.
- `gateway/internal/metrics/`: the new usage counters (step 1's names) with the usage
  labels.
- `gateway/internal/server/limits.go`: the reservation hook — what a request reserves
  per measure. The endpoint-specific amounts come in steps 5–8.
- `gateway/internal/server/errors.go`: the two new error codes the spec names
  (`response_format_unsupported`, `external_reference_unsupported`), each with its
  class, so the error-table test holds; their uses come in steps 5–8.
- Tests beside each.

## Decisions made during planning

- **The limit table stays the one place** a limit type is defined. A new measure is a
  new row and a new `Measure`, not a special case in the limiter.
- **A measure the request cannot know before it runs** (audio generated) reserves 0 and
  is counted at settle, as USD is.

## Acceptance criteria

- **Component tests:**
  - each new limit type refusing at its window, local and counted;
  - images reserved and settled;
  - audio settled after;
  - the caps refused;
  - records carrying the units through to the control plane's batch encoding;
  - the metrics series.
- `scripts/check-gateway.sh` and `scripts/check-all.sh` green — the phase-1 reds of
  steps 1–2 cleared, the cross-half e2e included.

## Result

### What changed

- `gateway/internal/config/`:
  - `snapshot.go`: units `images`, `audio_seconds_out`, `audio_seconds_in`. Unit sets:
    `RecordUnits` (new) is every unit, in the usage-record schema's order — what a
    record carries; `TokenUnits` is the token units only (every unit but the media
    ones); `PricedUnits`, `InputUnits`, `CountedUnits` and `PriceFallback` unchanged —
    the media units are recorded and limited, never priced, never input size, never
    token-counted. Measures `MeasureImages`, `MeasureAudioSecondsOut`,
    `MeasureAudioSecondsIn`; `Measure.Units()` says which record units a measure
    counts (tokens: `CountedUnits`; each media measure its own unit; none for requests
    and cost). The limit-type table gains the six rows in the schema's order, minute
    rows local, hour rows counted. `Snapshot.MaxImagesPerRequest` /
    `MaxSpeechBatchItems`, defaults 10 and 32.
  - `document.go`, `schema.go`: the two caps (integer ≥ 1); `FormatVersion` 6.
  - `semantic.go`: `countersPerScope()` derived from the counted types (5); the
    `counters-exceeded` message names it ("5 for global and for every group"), as
    `kaiak-control`'s does.
- `gateway/internal/accounting/`: `withEveryTokenUnit` → `withEveryUnit` (every record
  carries every unit, media at 0); `UsageRecord.OutputType` and `Request.OutputType`
  (never sent, like `Operation`; the usage metrics' `gen_ai.output.type`); `Cost`
  sums `PricedUnits` only, so the media units cost nothing and do not move the tier.
- `gateway/internal/limits/`: `Reserve(Subject, Need)`. `Need{Tokens, Media}` —
  `Media map[config.Measure]int64` names the media measures a request counts toward
  and what each reserves (images: the request's count; audio generated: 0, settled
  after; audio processed: a measured amount — the mechanism phase B fills).
  `applies` decides a counter's applicability by measure; `need` the amount;
  `amountOf` settles any measure through `Measure.Units()`. Totals take the new hour
  windows with no change (the window types follow the table).
- `gateway/internal/control/`: `ProtocolVersion` 6; the record walker requires
  `RecordUnits`; `fakecontrol` speaks 6.
- `gateway/internal/metrics/usage.go`: `gen_ai.output.type` joins the usage labels
  (after the operation); `kaiak.usage.images` (`{image}`),
  `kaiak.usage.audio.output.duration` and `kaiak.usage.audio.input.duration` (`s`)
  under the usage labels, fed only when the record's unit is not 0.
- `gateway/internal/server/`:
  - `inbound.go`: `inboundFields.Media` — the reservation hook: the media formats'
    owned fields (steps 5–8) state their measures and amounts there;
    `limits.go` passes it as `Need.Media`. `errLimited` answers image limits `429`
    type `images` and audio limits type `audio_seconds` (`rate_limit_exceeded`),
    with a "Request too large" wording for images and audio processed above a
    limit's whole value; media limits send no `x-ratelimit-*` headers.
  - `errors.go`: `errResponseFormatUnsupported(param, allowed)` and
    `errExternalReference(param)`, class `invalid_request` (used by steps 5–8; the
    error-table test lists them, so lint sees them used).
  - `pipeline.go` `endpoint.outputType`, carried onto the record (`attempts.go`) and the
    request line (`requestlog.go`: `gen_ai.output.type`, and the media usage fields
    `kaiak.usage.images`, `kaiak.usage.audio.output.duration`,
    `kaiak.usage.audio.input.duration`, summed over the records, only when not 0).
    No endpoint sets an output type yet.
- Format 6 in every Go-written config: the gateway's tests (`cmd/kaiak`, `e2e`,
  `auth`, `config`, `limits`, `server`) and `scripts/live/config.go`.
- Tests updated for the new shape: record units with the media at 0 (server,
  control, cross-half e2e), version literals (control), the full fixture's new
  backends, models and limits (config, `cmd/kaiak` status counts).

### Design choices

- **A media limit applies only to a request that adds to its measure** — an image
  limit to image requests, audio generated to speech, audio processed to uploads —
  as a USD limit applies only to priced requests. Without it a spent audio window
  would refuse every chat request in the scope (audio generated reserves nothing,
  so its check is "window below its value", which a chat request would meet too).
  The spec does not say this outright (see Notes for review).
- **Images settle like tokens, audio generated like USD**: both through the one
  hold/release/add path; a layered answer above `n` takes the window past its value.
  The 2 s "blocked only by running requests" retry stays a token-limit rule.
- **`RecordUnits` in the schema's order** (media first, as `usage-record.schema.json`
  lists `required`), so `TestRecordUnitsAreTheSchemaUnits` (renamed from
  `…TheTokenUnits`) keeps its ordered comparison.
- No cap constructors (images per request, batch items) yet: the endpoints that
  refuse them come in steps 5–8; `invalid_value` is already answered.

### Tests added

- `limits/media_test.go`: each media type at its minute and hour window — images
  reserved by count, refused at the window (type, requested, used), settled to the
  answer's count past the value, released with no record; a request larger than the
  limit; audio generated reserving nothing, admitted below the value, passed by
  requests in flight, then refused; audio processed reserved and settled; no media
  limit meets a request that does not add to it; every scope keeps the three media
  hour counts unlimited, no per-minute counter without a limit; control-plane mode:
  pushed media hour totals plus own usage, retirement by generation, the global
  count, per-minute media shares.
- `config`: `TestUnitSets` (record units, token units, media outside them, what each
  measure counts, six measures), `TestCountCounters` (five per scope, media per-minute
  limits counted), `TestCountersExceededAtRoot` (9 999 groups at 50 000, the message),
  the caps' defaults and schema defaults, `TestBatchCapsRefuseZero` (both caps).
- `accounting`: `TestRecordCarriesItsOutputTypeAndEveryUnit`,
  `TestMediaUnitsCostNothing` (flat and tiered prices).
- `control`: speech and image records encoding to valid records and a valid batch.
- `metrics`: `TestUsageMetricsCountMediaUnits` (series only for records carrying the
  unit, `gen_ai.output.type` present or absent); the full scrape drives the media
  counters.
- `server`: `TestMediaAmountsMeetTheLimitsOfTheirMeasure` (the hook through
  `checkLimits`: `429 images`, `429 audio_seconds`, no media headers, chat untouched),
  `TestMediaRefusalMessages`, `TestRequestLineCarriesTheOutputTypeAndMediaUsage`.

### Reds left — all the `vllm-omni` type, cleared by step 4

The config schema walker does not admit `vllm-omni` (step 4 adds it with its provider
module), so every test parsing a fixture or example that names it is red:

- `internal/config`: `TestBackendTypesAreTheSchemaEnum`; `TestValidFixtures/full.json`,
  `TestValidFixtures/backend-types.json`; `TestResolvedFixtures/full.json`;
  `TestExampleConfigs/config.json`; and the tests reading `full.json`:
  `TestReliabilitySettings`, `TestExplicitValuesOverrideDefaults`,
  `TestModelResolution`, `TestKeysResolveToTheirGroup`,
  `TestWildcardExpandsToEveryModel`, `TestChildDefaultsMergeUnderEachChild`,
  `TestReaderKeepsItsSnapshotAcrossASwap`, `TestLoadAppliesAValidFile`,
  `TestLoadRejectsUnsetAPIKeyVariables`.
- `cmd/kaiak`: `TestServingStatusCoversTheAppliedConfig` (reads `full.json`).
- `internal/provider`: `TestBackendTypeFixtures/vllm-omni`.

Checked: with `vllm-omni` added to the walker's `backendTypes` alone (a scratch edit,
reverted), `internal/config` passes whole and `TestServingStatusCoversTheAppliedConfig`
passes — so those tests already expect the step-1 fixtures (media limits resolved,
caps, six models) and step 4 needs only the type.

### Suite (2026-10-09)

- **`scripts/check-gateway.sh`**: gofmt, vet (crosshalf tag included), staticcheck
  2026.2.1 on go1.27.1 and the telemetry boundary pass. `go test -race -count=1
  ./...`: 15 tests fail in 3 packages (`cmd/kaiak` 1, `internal/config` 13,
  `internal/provider` 1) — the `vllm-omni` reds above, nothing else; the other 17
  packages pass, `e2e` included.
- **`scripts/check-all.sh`**: stops at the same gateway reds. The later stages, run on
  their own with the scripts' commands:
  - live-test kit (`scripts/live`): gofmt, vet, staticcheck pass; `go run .
    -self-test`: 34 passed, 0 failed, 1 skipped, every kind.
  - control `npm test`: 655 tests, 654 pass, 0 fail, 1 skipped. `npm run lint`:
    passes (`boundaries ok`).
  - **cross-half e2e** (`go test -race -tags crosshalf -run '^TestAcrossHalves'
    -count=1 ./e2e`): **passes** — both halves at protocol 6 and format 6.

### Notes for review

- **Media limits' applicability** (design choice 1): GATEWAY.md, Limits → Check and
  reserve, says images reserve "nothing for any other" request and an audio limit
  "refuses once its window has reached its value", while Local counters says "every
  limit of a scope applies to every request in it". Read together, a spent audio or
  image window would refuse chat. The code applies a media limit only to requests of
  its measure; suggested spec sentence for Check and reserve: "An image or audio
  limit applies only to a request that adds to its measure — image limits to image
  requests, audio-generated limits to speech, audio-processed limits to uploads — as
  a USD limit applies only to a priced request."
- `kaiak.limit.requested` stays on token refusals only, as the request-line table
  says; an image refusal's requested count is in its client message.

