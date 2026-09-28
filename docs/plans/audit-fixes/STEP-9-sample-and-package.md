# Step 9 — sample and package

**Status:** done (2026-09-25) — commits `0798282` (flaky watcher test, M15),
`10e115b` (H12, M14), `f621f22` (L18), `9c09d65` (L5 correction), `d3b15dc` (three
gateway e2e timing fixes). **Phase 3 complete**:
`GOFLAGS=-count=1 scripts/check-all.sh` green three runs in a row.

## Items

- **M15** watcher reacts to the `..data` symlink swap (projected ConfigMaps); test it.
- **H12** the sample states visibly (startup log, page) that budgets reset on restart.
- Page: credential-free URL rendering (defense in depth for M14).
- **L18** `kaiak-control` ships its schemas inside the package (copied at pack/build
  time or read from a package-relative path); the images use it.
- Added by the main session: the flaky watcher test (root cause first), and the L5
  correction (no backend error text in logs).

## Acceptance criteria

- Tests; phase 3 end: `scripts/check-all.sh` green.

## Result

**Flaky watcher test — root cause** (`control/sample/src/config-file/`)

- Symptom: "the watcher publishes an edit as a new version" timed out at 5000 ms once
  in a `check-all` run. Reproduced: the committed test file failed 2 of 64 runs (8 in
  parallel next to the kit's suite), hanging in "publishes an edit" and, once, in
  "rename over" — the same mechanism.
- Mechanism: on macOS, `fs.watch` on a directory uses FSEvents, and libuv starts the
  FSEvents stream on its own thread **after** `watch()` returns. A change made in that
  moment is never reported, not even late. The tests edited the file about a
  millisecond after `startWatching()` (only the `startup` reload in between), so under
  load the edit fell before the stream was live and no event ever came.
- Proof (a standalone probe: watch a fresh directory, write the file right away, wait
  2 s for an event): under load (16 busy processes, 8 probes in parallel) 27 of 800
  writes made 0.1–0.7 ms after `watch()` returned got **no event at all** (the
  recorded event list was empty — not a different name, not late). The same probe
  with the write 100 ms after `watch()`: 0 of 800 missed. On Linux (inotify) the watch
  is registered synchronously inside `watch()`, so there is no such window.
- Fix, in the tests (the watcher has no bug to fix: Node offers no "watch is live"
  signal, and the app starts watching before its startup read — on macOS a change in
  the first moment after startup waits for the next edit; the doc comments on
  `startWatching` and the app's `onReady` now say so):
  - `createConfigFile` takes `watchDirectory` (default `fs.watch`), as it takes
    `clock`. The tests wrap the real `fs.watch` to see its raw events, and
    `startWatchingLive` rewrites a `watch-probe` entry — one the watcher ignores, so it
    causes no reload — until the watch reports it; only then does a test edit. The
    probe interval is the handshake's cadence, not a sleep that hides a race: the test
    proceeds on an observed event.
  - The app tests had the same race (watch started in `onReady`, edits soon after):
    `watchLive` rewrites the config file unchanged until a `file-changed` run is
    logged.
- After the fix, same load: 0 of 128 runs failed. (Load from busy processes alone
  did not discriminate: 0 of 370 for either version; the kit's suite running
  alongside did.)

**M15 — projected ConfigMaps** (`config-file/index.ts`)

- The watcher reloads on the file's own entry, a nameless event, or any entry whose
  name starts with `..` — Kubernetes' atomic writer names every entry of its swap so
  (`..data`, `..data_tmp`, `..<timestamp>`). Debounced as before; the
  unchanged-content check keeps the swap's several events from publishing twice.
  Other entries in the directory are still ignored (no reload noise from busy
  directories).
- Observed on macOS for the swap: `rename ..v2`, `rename ..data_tmp`, `rename ..data`
  (twice), `rename ..v1` — never `config.json`, which is why the old filter missed it.
- Tests: "the watcher sees a projected volume's atomic symlink swap and publishes it
  once" (real files: `config.json -> ..data/config.json`, `..data` swapped from a
  directory with context length 1000 to one with 2000 — published once, trigger
  `file-changed`, a later check reload is `unchanged`); "the watcher reloads on the
  entries of a projected volume's swap" (the same swap with the events delivered by
  hand through `watchDirectory` — deterministic; fails with the old filter). Plain
  writes and rename-over saves keep their tests.

**H12 — in-memory warning** (`app/index.ts`, `page/document.ts`)

- At `onReady`, a warn-level line: "state is kept in memory: budgets, usage totals and
  usage batch de-duplication reset when this process restarts — the sample is not a
  billing system".
- The page leads with a notice (`<p class="notice">`, warn-coloured): "State lives in
  memory. Budgets, usage totals and the de-duplication of usage batches reset when
  this process restarts: the sample is for demos and validation, not a billing
  system."
- Tests: `app.test.ts` "the app warns at startup…" (level 40, the text);
  `page.test.ts` "the page says its state lives in memory…" (the notice, before
  `<main>`).

**M14 — credential-free URLs on the page** (`page/format.ts`, `page/sections.ts`)

- `formatUrl`: parses with `URL`, clears `username`/`password`, renders `href`; a
  value that does not parse shows as `(not a URL)`. Backend URLs on the page go
  through it. (A side effect of `href`: a bare origin gains its `/`.)
- Test: "backend URLs are shown without userinfo, whatever the config holds" — a
  config with `http://operator:s3cret-pass@vllm-a.internal:8000/v1` (bypassing
  validation) renders `http://vllm-a.internal:8000/v1`, neither the user nor the
  password anywhere in the page.

**L18 — schemas inside the package** (`control/kaiak-control/schema/`,
`control/scripts/sync-schemas.ts`, `kaiak-control/src/schemas/index.ts`,
`kaiak-control/package.json`, `control/package.json`, `control/sample/Dockerfile`
and its `.dockerignore`)

- `kaiak-control` reads only its own `schema/` — a committed byte-for-byte copy of
  `protocol/schema/`, which stays the source of truth. `npm run sync-schemas`
  (control/) refreshes the copy; `scripts/sync-schemas.test.ts` fails `npm test` while
  any file is missing, extra or differs, and tests the drift/sync functions on a
  temporary pair.
- `kaiak-control/package.json` `files`: `src` (tests excluded) and `schema`.
  Verified: `npm pack -w kaiak-control`, extracted outside the repo, validated
  `minimal.json` (`{"ok":true,…}`).
- Sample image: copies `control/kaiak-control/schema` instead of `protocol/schema`.
  Verified with `scripts/build-images.sh` (no push) on `dev`, then
  `scripts/smoke-images.sh` against those images: "smoke passed" (the sample image
  validated and served the config; control-plane mode chat 200). The six image tags
  were removed from `dev` afterwards; the smoke script removed its containers,
  network and fake-backend image.
- TECH-STACK records the decision (settled 2026-09-25); ARCHITECTURE's `schemas` line
  updated.

**L5 correction — no backend error text in logs** (`server/backend_errors.go`,
`server/upstream.go`, `server/api.go`, `server/pipeline.go`; GATEWAY.md)

- `upstream_body` is gone. For a backend `5xx` the log line carries
  `upstream_error_code` and `upstream_error_type` when the body names them — the
  OpenAI/Azure shape (`{"error":{…}}`) or vLLM's flat one — only identifiers
  (letters, digits, `_.:-`, or an integer code), clipped to 64 bytes; at most 4 KiB of
  the body is read. The backend's status is the line's `status` (the answer is sent
  under it) and each attempt's is in `tried`.
- Tests: `TestBackendErrorBodies` (type logged, no code, no `CUDA`/`gpu-7`/
  `upstream_body` in the line), `TestAllAttemptsFailingAnswerTheLastError` (type
  logged, the message `third` absent), `TestBackendErrorFields` (11 cases: the three
  shapes, null and non-integer codes, text-like codes refused, clipping, trailing
  text, not JSON, cut off, empty).

**Three gateway e2e timing assumptions** (`gateway/e2e/`; commit `d3b15dc`)

The first three `check-all` runs after the items above went red, green, red — in
gateway e2e tests this step did not touch. Each failure's mechanism was named and
proven before the fix:

- `TestAcrossHalves` (run 1; "the hourly token total at 319 not seen", then at 341):
  the run crossed 02:00:00 UTC (gateway logs 01:59:49–02:00:45 UTC; the totals'
  `tokens_per_hour` window started 02:00:00 with `used` 11). The sample's hourly total
  restarts at the top of the hour; the test expected everything served since the
  sample began. Fix: the expected total is the answers received in the totals'
  window, an answer within 1 s of either edge counting either way (its record is
  stamped just before it arrives). Checked with a throwaway test of the helper
  (window before/after the hour, the edge, the old expectation refused).
- `TestSharedLimitsAcrossGateways` "a budget spent through one gateway is enforced on
  the other" (run 3; 1 in 15 alone): the test waited for gw-b's
  `kaiak_control_totals_applied_timestamp_seconds` to move past the value read before
  `SetWindows`, but the totals pushed when gw-a's batch was counted (still without
  the windows) could land after that read. Proof: with temporary instrumentation (a
  log line per totals applied, with the window count) and a 30 ms delay per frame on
  the fake control plane's streams, 3 of 3 runs failed, each showing
  `before` read → windows=0 applied → the request (200) → windows=1 applied. Fix: set a
  live count of 1 after the windows (a marker only totals carrying them hold) and wait
  for gw-b's `kaiak_backend_max_in_flight` to double; restore 2 after. With the same
  30 ms delay: 3 of 3 pass.
- `TestSharedLimitsAcrossGateways` "the control plane back ends the outage" (seen
  once under heavy extra load): gw-a's stream attempts at +0.06, +0.7, +1.0, +1.3,
  +1.6 s, then silence past the 15 s wait — the 6th reconnect delay is uniform up to
  16 s (base 500 ms doubling, full jitter; cap 30 s). Fix: that wait uses
  `recoverLimit` (40 s, above the cap) — the bound `TestAcrossHalves` already used for
  the same reason; `waitMetricWithin` and `recoverLimit` moved to the untagged
  harness.

**Suite**

- `GOFLAGS=-count=1 scripts/check-all.sh`, three runs in a row after these fixes: see
  Phase 3 end below.

## Phase 3 end

`GOFLAGS=-count=1 scripts/check-all.sh`, three runs in a row on `d3b15dc`, all green
(gofmt, vet, staticcheck, gateway race tests incl. e2e, live kit self-test, control
443/443, lint, cross-half e2e). Tail of each run:

```
run 1: ok  kaiak/e2e 45.253s · control 443/443 · boundaries ok · ok kaiak/e2e (crosshalf) 39.600s · all checks passed
run 2: ok  kaiak/e2e 39.169s · control 443/443 · boundaries ok · ok kaiak/e2e (crosshalf) 48.176s · all checks passed
run 3: ok  kaiak/e2e 39.738s · control 443/443 · boundaries ok · ok kaiak/e2e (crosshalf) 43.182s · all checks passed
```

The runs before the e2e fixes (on `9c09d65`): red (`TestAcrossHalves`, hour
boundary), green, red (`TestSharedLimitsAcrossGateways`, stale totals) — see Result.

**Phase 3 complete (2026-09-25), suite green.**

## Decisions for the user to confirm

1. **L18 — a committed copy** of `protocol/schema/` in `kaiak-control/schema/`, kept
   equal by a test and a sync script, rather than a copy generated at pack time (that
   would need the repo to keep a second read path) or a configurable path option.
   Cost: a schema change touches two directories (`npm run sync-schemas`; the test
   names the command when it fails).
2. **M15 filter** — the watcher reacts to `..`-prefixed entries (Kubernetes' naming),
   not to every entry of the directory: a config file kept in a busy directory gets
   no reload noise. A symlinked config file pointing elsewhere (not the Kubernetes
   layout) is still watched only by its own entry.
3. **L5 correction** — identifiers only (charset-restricted), not any string clipped
   to 64 bytes: a `code`/`type` holding free text (spaces, punctuation) is dropped,
   not logged. No new `upstream_status` field: the line's `status` and `tried`
   already carry the backend status.
4. **Flake fix in the tests, not the watcher** — the macOS startup window (a change
   within about a millisecond of startup waits for the next edit) is documented, not
   coded around; Linux, where the images run, has no such window.
5. **`watchDirectory` option** on `createConfigFile` — added for the tests (as
   `clock` is); it is part of the sample's exported API.

6. **e2e marker** — the shared-limits test marks the pushed windows with a live
   count of 1 (then restores 2), since the gateway exposes no totals revision.
   Exposing the applied totals revision as a metric would be a cleaner signal but is
   product surface; not added.

## Deviations

- The app tests got the same live-watch handshake (not named in the brief; same
  mechanism, same flake risk).
- Three gateway e2e test fixes (`d3b15dc`), not in the brief: they blocked the phase
  end's three green runs. Test-only; no gateway code changed. The reconnect one may
  be the unexplained failure the main session saw after step 6 (STEP-7's flake hunt).
