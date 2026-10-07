# gateway-control-main — structure review

Modules: `gateway/internal/control` (2 166 non-test lines in 11 files) and
`gateway/cmd/kaiak` (`main.go`, 797 lines). The spool and last-known-good files
are gone: `spool.go`, `spooldisk.go`, `spoolmemory.go` and `lastknowngood.go`
were deleted in `0a00dc6` (2026-10-07, "no data directory"). What remains of that era is
a few behaviours, not file names (F1).

## Module summaries

### `gateway/internal/control`

Responsibilities:
- The protocol messages: Go types (`messages.go`), strict decoding (`decode.go`: syntax →
  duplicate members → schema walker → message rules → strict typed decode), hand-written
  schema walkers mirroring `protocol/schema/*.json` (`schema.go`), the rules a schema
  can't express (`semantic.go`), and the issue codes and `ValidationError` (`control.go`).
- Transport (`transport.go`): auth, protocol-version and instance headers; redirects
  refused; protocol-version check on every answer; error codes read in the protocol's
  shape; no remote text in errors.
- Config follower (`client.go`, `stream.go`): boot (stream's first config, boot backoff,
  seed fallback, exit reasons), reconnect loop with jittered backoff, idle/open timeouts,
  config apply/skip-by-hash/rejection tracking, totals handed to a callback with the
  usage generation they cover.
- Usage delivery (`usage.go`, `queue.go`): filling batch → sealed → checked and given a
  sequence → in-memory queue → one outstanding POST at a time with retry; refusal
  classification; memory bound; acked-until-counted list; usage generations; the
  ack/uncounted clocks for the outage decision; `FlushUsage` for the drain.
- Status reporter (`status.go`): triggers, interval, minimum gap for routing changes,
  draining flag, log-on-transition.
- `backoff.go`: exponential, capped, full-jitter backoff.

Exported surface used from outside (non-test):
- `cmd/kaiak`: `New`, `Options`, `Client.{Boot, Run, Record (as accounting.Batcher),
  Contact, UsageWaitingSince, UsageUncountedSince, AppliedConfigHash, LastRejection,
  ServingChanged, SetDraining, FlushUsage, ReportStatus}`, `Serving`, `BackendStatus`,
  `DeploymentStatus`, `ModelStatus`, `Circuit*`, `Totals`, `TotalsWindow`, `TotalsUpdate`,
  `IsInstanceID`, `DefaultUsageMemoryBytes`, `DefaultBootWait`.
- `metrics`: implements `UsageObserver` structurally; it doesn't import `control`.
- Tests only: `server/usage_path_test.go` (`New`, `TotalsUpdate`, `DecodeUsageRecord`),
  `metrics_test.go` (`Batch*`), `e2e/*` (`Totals`, `Status`).
- Exported but used only inside the package or its tests: `DecodeStatus`,
  `DecodeUsageBatch`, `TriggerControl`, `TriggerSeed`, `Code*`, `Issue`,
  `ValidationError`, `Dropped*`.

Dependencies: `accounting` (UsageRecord is the wire record, Batcher), `config` (Applier,
LimitType, Unit constants, ID/name predicates, ValidationError codes), `logattr`,
`netfail`, `schemacheck`, `sse`.

Domain concepts and where they live:
- Protocol version and headers: `control.go:18`, `transport.go:20-25`.
- Instance ID shape: `schema.go:15`, `client.go:75`.
- Batch ID (instance, epoch, sequence) and its exactly-once invariants: `messages.go:81`,
  `queue.go:12-23`.
- Usage generation (filling batch number used to retire own usage): `usage.go:109-112`,
  `countedGeneration` at `usage.go:305`.
- counted_through / acked-until-counted: `usage.go:117-128`, `usage.go:387-408`.
- Outage clocks: `lastContact`/`streamOpen` (`client.go:156`), `waitingSince`,
  `uncountedSince` (`usage.go:125-138`).
- Batch refusal codes (kaiak-control's error codes, partly literals): `usage.go:76-92`.
- Config hash, rejection, skip-by-hash: `client.go:159-167`, `client.go:420-459`.
- Gateway state, circuit states: `messages.go:102-156`. Totals limit types and token
  units are repeated in `schema.go:28-36`.
- Boot vs reconnect backoff: `client.go:36-53`.

### `gateway/cmd/kaiak` (`main.go`)

Responsibilities, in file order:
- Logger format (`newLogger`).
- Env settings, about 230 lines: `settings`, `controlSettings`, `readSettings`,
  `readControlSettings`, `readSeed` (seed check, including no priced models),
  `wholeNumber`, `durationMS`.
- `run`, about 245 lines: log-export wrap; dependency graph (holder, providers,
  metrics, router, model checker, applier callback); mode branch (file loader or
  control client + shared limiter + boot + first-totals wait); recorder; listeners;
  background loops; stop/hurry handling; drain; control-plane finish; shutdown.
- Lifecycle helpers: `stopOnSignal`, `waitFirstTotals`, `finishWithControlPlane`,
  `finishLogExport`, `later`, `reloadOnSignal`, `ignoreReloads`.
- Control → other-package glue: `controlContact` (→ `limits.Contact`), `controlState`
  (→ metrics), `servingStatus` (routing + config → `control.Serving`), `limitsTotals`
  (→ `limits.Totals`).

Exported surface: none (package main). The tests drive `run` as a black box, plus
`readSettings`, `servingStatus` and `limitsTotals` directly.

Concepts it holds: the two modes and how they are told apart (settings), the boot
deadline (a second copy beside the client's), the drain choreography and its "hurry",
the seed's "free models only" rule (`readSeed`).

## Findings

### F1 — Spool-era leftovers in usage delivery: a dead seal at stop, a per-request `instance`, an unused ack value
- **Kind**: cross-function.
- **Where**: `internal/control/usage.go:245-260` (`runSealer`, `case <-ctx.Done(): u.seal("stop")`),
  `client.go:368-371` (the `Run` doc: "When ctx ends the filling usage batch is sealed and
  queued"); `transport.go:59,73,79,96-99` (`get`/`post`/`send` take `instance`);
  `usage.go:487` (`batch.Batch.Instance`), `status.go:216` (`r.c.opts.Instance`),
  `usage.go:98,155` (`usageSender.instance`); `usage.go:433,480-508` (`post` returns
  `(UsageAck, error)` and the only caller discards the ack).
- **Now**: when `Run`'s ctx ends, the sealer seals and queues the filling batch, but
  the sender stops on the same ctx, so nothing ever sends it. In the binary, the drain's
  `FlushUsage` (`main.go:631`) has already sealed and flushed before `stopBackground`
  (`main.go:542`). `send(req, instance)` receives `c.opts.Instance` from every caller.
- **How it got here**: before `0a00dc6` the stop seal wrote to the disk spool, so the
  next process sent it. The old doc said "When ctx ends it seals and saves what is left,
  so a stop keeps every settled record in the store". The `instance` parameter was
  added in `19825b2` (private) because the spool could hold batches of a previous
  instance ID; `0a00dc6` removed every `e.id.Instance != u.instance` branch and left the
  parameter.
- **Proposed shape**: `runSealer` returns on `ctx.Done()` without sealing. Fix the `Run`
  doc and the comment at `usage.go:242-244`, which now wrongly says it is "so the
  drain's flush finds every settled record queued". `send(req)` and
  `post(ctx, path, body, want)` set `Kaiak-Instance` from `c.opts.Instance`, and
  `usageSender.instance` goes. `usageSender.post` returns `error` only.
- **Payoff**: removes one dead step and one false invariant comment, one parameter
  across 3 functions and 4 call sites, and one field.
- **Cost / risk**: about 15 lines, internal only. No test seals at stop (checked
  `*_test.go`). No contract is touched.
- **Confidence**: high. The history and the call order in `main.go` both show it.

### F2 — The control → limits adapters live in `main` and are copied, with drift, into a server test
- **Kind**: cross-module.
- **Where**: `cmd/kaiak/main.go:723-729` (`controlContact`), `main.go:789-797`
  (`limitsTotals`), `main.go:411-415` (`OnTotals`); copies in
  `internal/server/usage_path_test.go:65-68` and `:106-116` (`testLimitsTotals`).
  Accessors: `control/client.go:240` (`Contact`), `usage.go:282`
  (`UsageWaitingSince`), `usage.go:293` (`UsageUncountedSince`). Types:
  `control.TotalsWindow` (`messages.go:53`) and `limits.PushedWindow`
  (`limits/shared.go:38`) are the same four fields; `control.TotalsUpdate`
  (`messages.go:69`) is `limits.Totals` plus `Counted`.
- **Now**: three getters with two locks are assembled into `limits.Contact` in `main`.
  Totals are converted field by field. The test copies have already drifted:
  `testLimitsTotals` drops `Complete`, and the test contact drops
  `UsageUncountedSince`. Because the adapters sit in package `main`, they can't be
  imported, and the test is not exercising the binary's wiring.
- **How it got here**: each outage clock was added by its own plan (stream contact in
  `2a8590c`, waiting/uncounted in round 3, `f5cfcba`), each with a getter, and the
  glue grew in `main`.
- **Proposed shape**: `control` imports `limits` for its input types, following the
  `accounting.UsageRecord` precedent (a domain type the wire messages embed; no cycle,
  since `limits` imports only `accounting`, `config`, `logattr`).
  `Client.LimitsContact() limits.Contact` replaces `Contact` + `UsageWaitingSince` +
  `UsageUncountedSince` for the limiter. `Contact()` stays for metrics.
  `Options.OnTotals func(limits.Totals, counted uint64)`. `limits.PushedWindow` either
  carries the JSON tags, so `Totals.Windows` is `[]limits.PushedWindow`, or the one
  conversion moves into `control.takeTotals`. Delete `limitsTotals`,
  `controlContact`, `testLimitsTotals` and the test's contact closure.
- **Payoff**: removes 4 adapter copies, 2 exported getters and one mirrored type
  (`TotalsWindow` or `TotalsUpdate`). Adding an outage clock touches the control getter,
  `limits.Contact`, `main` and the test copy today (4 places); afterwards it touches 2.
  It also makes the server test use the real wiring.
- **Cost / risk**: small, about 60 lines net removed. No protocol change. It adds a
  `control → limits` import edge, so check it against `docs/ARCHITECTURE.md`'s package
  diagram. `TestLimitsTotalsCarryWindowsByGroupAndType` moves to `control`.
- **Confidence**: medium-high. The import direction is the only real choice. The
  limits reviewer may prefer `limits` to define a `ContactSource` interface that
  `*control.Client` satisfies, which is equally fine.

### F3 — "Hurry" is a channel watched by three hand-rolled goroutines; as a context it collapses
- **Kind**: in-function / cross-function (`main.go`).
- **Where**: `main.go:316-330` (the `hurry` / `endSignalWatch` variables and deferred
  closure), `main.go:516-537` (the second-signal watcher), `main.go:620-646`
  (`finishWithControlPlane`: a watch goroutine turning `hurry` into a cancel, then a
  `select` to learn whether it was hurried), `main.go:659-682` (`finishLogExport`:
  another watch goroutine), and `main.go:560-577` (`stopOnSignal`, the same
  "ctx ends on a value from stop" helper, used only for boot).
- **Now**: four goroutine + `WaitGroup`/`done` constructs implement one idea: a
  context that ends on the parent ctx or the next stop signal. The `ctx.Err() != nil →
  close(hurry)` special case exists only because `hurry` is not derived from ctx.
- **How it got here**: the drain's hurry came first (as a chan for `server.Drain.Run`).
  The usage flush (2026-09-24), the boot stop (D7) and the log-export flush
  (2026-10-05, `699637f`) each added their own watcher.
- **Proposed shape**: `hurryCtx, endHurry := stopOnSignal(ctx, stop, onSignal)` reuses
  the boot helper (with an optional log callback for the "second stop signal" line).
  `drain.Run(api, times, hurryCtx.Done(), logger)`.
  `finishWithControlPlane` becomes `flushCtx, cancel := context.WithDeadline(hurryCtx,
  drainDeadline)`, then `FlushUsage`, then `if hurryCtx.Err() != nil { return }`, then
  the final status, with no goroutine. `finishLogExport` uses `context.AfterFunc(hurryCtx,
  …)` to cut the deadline to the floor. Before a drain starts it receives
  `context.Background()` instead of a nil chan.
- **Payoff**: about 40 lines and 3 goroutine/WaitGroup constructs removed from `main.go`.
  One representation of "hurry" instead of chan + ctx + nil-chan special case.
- **Cost / risk**: small, contained in `main.go`. `server.Drain.Run` is unchanged (it
  takes `Done()`). Covered by `TestSecondStopSignalSkipsTheRemainingDrain`,
  `TestSecondSignalCutsTheFinalLogFlush` and `TestStopSignalDrains`.
- **Confidence**: high.

### F4 — Boot still has the shape of the snapshot fetch it replaced: two streams per boot, a `firstOnly` mode, and the first-totals wait split over three places
- **Kind**: cross-module (`control` ↔ `main` ↔ `limits`).
- **Where**: `control/stream.go:46` (`followStream(ctx, firstOnly bool)`, with
  `firstOnly` branches at `:106-113` and `:120-122`), `stream.go:28-31`
  (`streamResult.first`), `client.go:303-341` (`bootConfig`, closes the stream once the
  config arrives), `client.go:415-438` (Run's new stream gets the same config again and
  skips it by hash); `main.go:423` (a second boot deadline), `main.go:435-437,585-610`
  (`waitFirstTotals`); `limits/limits.go:161-164`, `limits/shared.go:64,265`
  (`firstTotals` chan, used only by `waitFirstTotals`).
- **Now**: Boot opens the stream, reads to the first config, closes it, and applies the
  config. `main` starts `Run`, which opens a second stream; that stream re-sends the
  config (skipped by hash) and then the totals. `main` then waits on the limiter's
  `FirstTotals()` until its own copy of the boot deadline. Every boot costs two
  connections, the stream reader has two modes, and the "boot wait" deadline is
  computed twice (client `bootCtx`, main `bootDeadline`).
- **How it got here**: the boot used to be a snapshot fetch (`fetchSnapshot`). Step 9
  of control-replicas (`d5c45cd`, 2026-10-07) swapped the fetch for "open the stream,
  take the first config, close it" and kept the fetch-shaped call sequence
  (`docs/plans/control-replicas/STEP-9-broadcast-gateway.md`, Result). The first-totals
  wait (D8, 2026-09-25) predates this and was built in `main` around the limiter.
- **Proposed shape**: Boot keeps the stream open. It reads to the first config and
  applies it. For a control-plane config, it keeps reading until the first totals
  have gone through `OnTotals`, or until the same boot deadline. It then hands the open
  stream (response, `sse.Reader`, start time, first-totals flag) to `Run`, whose
  `followConfig` resumes it before reconnecting. Boot logs the totals-wait lines. The
  `firstOnly` parameter and `streamResult.first` go. `waitFirstTotals` and
  `bootDeadline` leave `main`. `limits.firstTotals`/`FirstTotals()` go.
- **Payoff**: one connection per boot; one stream-reader mode; one boot deadline; about
  60 lines removed over 3 packages; the "skipped by hash" path is left for real
  reconnects only.
- **Cost / risk**: moderate. The spec's Boot and Readiness text
  (`GATEWAY.md` Control-plane mode → Boot / Readiness) describes behaviour, not the
  second stream, so the spec impact is small, but the boot diagram (control-replicas
  step 17) mentions it. Client boot tests in `client_test.go`/`seed_test.go` count
  opened streams (`h.bootStreams`), so expect test edits. The no-config-published case
  (stream open, nothing sent) must still end at the boot deadline with
  `errNoConfigYet`. The stop-signal-during-boot path in `main` must still cancel the
  totals wait. No protocol change.
- **Confidence**: medium. The leftover shape is certain. The payoff depends on the
  hand-off of an open stream staying simple. A spike on `followStream` taking an
  already-open stream would settle it.

### F5 — Status state `starting` cannot be sent by the gateway any more
- **Kind**: cross-function (with an optional protocol trim).
- **Where**: `control/status.go:168,181-188` (`loaded := c.opts.Applier.Loaded()` →
  `StateStarting`), `messages.go:106`, `schema.go:31`;
  `status_test.go:84-96` (`TestStatusIsStartingUntilAConfigIsApplied`, which calls
  `Run` after a failed `Boot`, something the binary never does).
- **Now**: `Run`, and so the status reporter, starts only after `Boot` returned nil
  (`main.go:427-432`), and then a config (control plane or seed) is always in force, so
  `currentStatus` reports `ready` or `draining`. The `starting` branch and its test
  cover a path the binary can't reach.
- **How it got here**: it is left over from the 2026-09-24 behaviour "start not ready and
  wait for a config", which the spec records as rejected (`GATEWAY.md` Boot → Exit:
  "Rejected: starting not ready and waiting").
- **Proposed shape**: the gateway reports `ready` or `draining` only; drop the
  `Applier.Loaded()` read and the test. Optionally, under the protocol rule,
  drop `starting` from `status.schema.json`, the TS `GatewayState`, the
  `valid/starting.json` fixture, the sample's `.state-starting` CSS and
  `CONTROL-PROTOCOL.md` ("Accepted before any config is published: a starting
  gateway…"). That part is a coordinated both-halves change and could be left as
  vocabulary.
- **Payoff**: removes a dead branch and a test of an unreachable state. The protocol
  trim would remove one enum value in 5 places.
- **Cost / risk**: gateway-only part is tiny. The protocol trim is small but touches
  the spec, schema, fixtures and kaiak-control.
- **Confidence**: high on reachability. If F4 is done in the "start Run before boot"
  variant instead, `starting` becomes live again, so decide F4 first.

### F6 — `main.go` mixes settings, lifecycle and control-plane glue; the mode is a nil check at six points in `run`
- **Kind**: cross-function (one file).
- **Where**: `run` branches on `s.control`/`client`/`loader` at `main.go:335-338`
  (log source), `:391-446` (setup and boot), `:455-457` (batcher), `:488-492`
  (SIGHUP), `:511-515` (reserve, `SetDraining`), `:539-541` (finish). Control-only
  helpers: `:579-646`, `:710-797`. Settings: `:66-297`; `controlSettings` (`:97-105`)
  mirrors 5 fields of `control.Options` and is converted at `:404-406`.
  `KAIAK_SEED_CONFIG_FILE` is read at both `:135` and `:227`. Defaults for boot wait
  and usage memory are set in both `readSettings` and `control.New`
  (`main.go:126,210` / `client.go:176-178,215-217`). The drain's control step uses
  three client calls (`SetDraining`, `FlushUsage`, `ReportStatus`), two of them
  exported only for this.
- **Now**: 7 of the 11 commits to `main.go` since 0.7.3 added a control-plane or
  log-export concern (boot, totals, flush/final status, stateless boot, OTLP export),
  each as another branch or step in `run`. `run` is about 245 lines, and the file holds
  four jobs.
- **How it got here**: each plan touched `run` where its concern fit in time order
  (`git log -- gateway/cmd/kaiak/main.go`; on main: `d5c45cd`, `2a8590c`, `f5cfcba`,
  `0a00dc6`, `699637f` …).
- **Proposed shape**: no interface, since two modes don't need one. Split the file and
  pull the control-plane half behind one constructor:
  - `settings.go`: env parsing. `readControlSettings` returns a partly filled
    `control.Options` (URL, Token, BootWait, SeedConfig, SeedFile) instead of
    `controlSettings`. The seed variable is read once.
  - `controlplane.go`: `startControlPlane(ctx, stop, deps) (*controlPlane, error)` builds
    the shared limiter + client and boots (F4 shrinks this further). Methods
    `run(bgCtx)`, `beforeDrain()` and `finish(hurryCtx, deadline)` replace the inline
    branches. It also holds `servingStatus`, `controlState`, `ignoreReloads`.
  - `control.Client.Finish(ctx)`, the flush then the final draining status bounded to
    2 s and skipped when `ctx` was hurried (`context.Cause`), replaces
    `finishWithControlPlane`. `FlushUsage` and `ReportStatus` become unexported.
  - `run` keeps lifecycle only: `if cp != nil` at 3 points (setup, drain times,
    finish).
- **Payoff**: `run` shrinks by about 100 lines. Mode branch points go from 6 to 3. Two
  exported client methods and one mirror type (`controlSettings`) go. A new
  control-plane lifecycle concern then touches `controlplane.go` (+ `control`), not
  `run`.
- **Cost / risk**: medium-small, mechanical, inside `cmd/kaiak` plus one client method.
  Tests drive `run` and `readSettings` as black boxes, so they mostly stay. Do it after
  F3/F4, since each changes what moves.
- **Confidence**: medium. The split is clearly right. How much `run` shrinks depends on
  F3/F4.

### F7 — Validators for messages the gateway only sends live in production code as a test oracle
- **Kind**: cross-module (mirror of `protocol/`).
- **Where**: `control/decode.go:47-60` (`DecodeUsageBatch`, `DecodeStatus`),
  `schema.go:154-224` (`usageBatch`, `status`, `backendStatus`, `deploymentStatus`
  walkers; `batchID` is shared with the ack), `semantic.go:67-101` (`usageBatch`,
  `status` rules). Callers: only `fixtures_test.go`, `status_test.go`,
  `usage_test.go`.
- **Now**: about 100 lines of Go mirror of `status.schema.json` and
  `usage-batch.schema.json` (plus their rules) ship in the binary and are kept in step
  with the shared fixtures, but at runtime the gateway never receives these messages.
  Only the usage-record walker (`checkRecords`) and the ack, totals and config-event
  decoders run in production. Two of the batch rules (`record-instance-mismatch`,
  `record-id-duplicate`) can't fail for batches the gateway builds.
- **How it got here**: the decoder set was built symmetric with kaiak-control's
  validators (`control/kaiak-control/src/messages/index.ts`), where each side validates
  what it receives.
- **Proposed shape**: move the outbound-only walkers, rules and decoders into
  `_test.go` files. They stay the oracle for the fixture tests and for "what the
  gateway sends passes the protocol". Alternatively, drop them and rely on
  kaiak-control's real JSON-Schema validation in the cross-half e2e, which removes one
  of the places to touch when a status or batch field changes.
- **Payoff**: moving removes about 100 production lines (none from the repo). Dropping
  takes a status/batch field change from 6 places to 5, but loses the fast unit-level
  conformance check.
- **Cost / risk**: moving is trivial. Dropping weakens the Go-side fixture coverage, so
  it needs a decision.
- **Confidence**: low-medium. This is a judgment call on where a test oracle belongs.

### Small items (each about 5–15 lines; take them with whichever finding touches the file)
- **Two guards for one non-nil rule**: `servingStatus` already ensures
  `Deployments != nil` (`main.go:757-779`). `currentStatus` repeats it and copies
  `Serving` field by field (`status.go:189-198`). Keep one, either in the producer or
  in `currentStatus`.
- **Log level by error class, five copies**: `client.go:328-334`, `:470-476`,
  `:483`, `usage.go:453-456` (`configProblem`), `status.go:151`. One
  `failureLevel(err)` would serve all five, though they differ slightly (stream-end
  adds `errMalformedTotals`; usage/status add 4xx).
- **`durationMS` lacks the `least` parameter `wholeNumber` has**: the "=0: want above
  0" check is hand-written twice (`main.go:168-171` for 3 variables, `:236-238`).
  `otlplog/settings.go:93-98` has a third copy of the millisecond parser.

## Cross-module hints
- `limits` only needs `limits.firstTotals` / `FirstTotals()` (`limits/limits.go:161-164`,
  `shared.go:64,265`) for `main.waitFirstTotals`. It goes with F4.
- Token units are listed by hand in `control/schema.go:33-35`, besides the constants in
  `config/snapshot.go` and the meter map in `accounting/meter.go:21`. Adding a unit
  touches all three plus TS and the schema.
- Totals limit types (`tokens_per_hour`, `usd_per_month`) and their window-start shapes
  are encoded in `control/schema.go:30,142-151`. Check against `limits`' window logic
  and kaiak-control's calendar.
- `batchRefusals` (`control/usage.go:76-83`) mixes constants with string literals for
  kaiak-control's error codes (`"usage-batch-invalid"`, `"instance-mismatch"`,
  `"request-invalid"`). No shared list exists, and the fixtures don't cover error
  codes.
- `servingStatus` (`main.go:753`) reads 3 router snapshots plus the config. Metrics'
  `ops.go:221-266` reads the same router accessors and also `MaxInFlightByBackend`
  (the per-gateway split cap), while the status reports the config's cap. Check whether
  `max_in_flight` in status (`CONTROL-PROTOCOL.md:387`, "the applied cap") means the
  configured or the split cap.
- The Go `messages.go` / `schema.go` / `semantic.go` / `decode.go` set mirrors
  `kaiak-control/src/messages/{types,semantic,index}.ts` and the JSON schemas by hand.
  Given zero Go deps this is inherent; the shared fixtures are the guard. The Go
  config-event decoder checks only the envelope, while TS also validates the inner
  config at message level. That is intended (a rejected config is a status rejection,
  not a malformed message) but worth one line in the protocol spec if it isn't there.

## Bugs noticed in passing
- Test-only drift, not a production bug: `server/usage_path_test.go:65-68,106-116`
  wires the limiter without `UsageUncountedSince` and builds `limits.Totals` without
  `Complete`, so that path test runs the limiter on "changes-only" totals with no
  uncounted clock, which differs from the binary (see F2).
- `runSealer`'s comment (`usage.go:242-244`) claims the stop seal serves the drain's
  flush; the flush happens before the stop (see F1).
