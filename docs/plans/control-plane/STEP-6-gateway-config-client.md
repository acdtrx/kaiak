# Step 6 — gateway: config from the control plane

**Status:** done (2026-09-24)

## Intent

Control-plane mode in the gateway: fetch the snapshot, follow the stream, apply
configs, keep a last-known-good copy, boot from it when the control plane is
unreachable.

## Files likely touched

- `gateway/internal/control/` — HTTP client, SSE follower (reusing the in-repo SSE
  reader if it fits, or its own), reconnect with backoff
- `gateway/internal/config/` — apply path shared with file mode
- `gateway/internal/state/` — last-known-good file
- `gateway/cmd/kaiak/main.go` — `KAIAK_CONTROL_URL` + `KAIAK_CONTROL_TOKEN`; both
  modes set → startup error
- `docs/specs/GATEWAY.md` — Configuration sources

## Decisions made during planning

- Boot: try the snapshot (bounded wait); unreachable → last-known-good if present,
  else not ready and keep trying. Readiness once any config is applied.
- Stream reconnect: exponential backoff with jitter, capped (e.g. 30 s); resume with
  `since=<applied version>`; `resync` → refetch snapshot.
- A config the gateway rejects is not applied; the rejection is kept for the status
  report (step 7). Last-known-good is written only after a successful apply.
- Only this package talks to the control plane (capability fence, like providers).

## Acceptance criteria

- Tests against a Go test double of the control plane (fixtures-driven): snapshot
  boot, live update, reconnect + resume, resync, rejection kept, LKG boot when down,
  not ready with neither.

## Result

- `scripts/check-gateway.sh` — gofmt, vet, staticcheck 2026.2.1, `go test -race ./...`
  (all packages ok, `internal/sse` and the control-mode e2e included), live-test kit
  self-test: `gateway checks passed`. `go test -race -count=10 -cpu 1,2,8` on
  `internal/control` and `cmd/kaiak`: ok.
- `control/`: `npm test` — 306 tests, 306 pass, 0 fail; `npm run lint` — `tsc` clean,
  `boundaries ok`.
- No expected reds.

### Decisions made while implementing

- **SSE reader shared**: the provider's reader moved to `internal/sse` (same purpose:
  splitting an SSE byte stream into blocks) and learned `event` and `id`; the block
  size limit is the caller's (`NewReader(r, max)`). Provider behavior and tests
  unchanged. Separate commit.
- **One apply path**: `config.Applier` (validate, credentials check, swap, the
  `config applied` / `config rejected` lines, the `kaiak_config_loads_total` count
  through a `LoadObserver`) serves the file loader, the control client and the
  last-known-good boot. `FileLoader` only reads the file; an unreadable file goes
  through `Applier.Reject`. `cmd/kaiak`'s `loadConfig` wrapper is gone.
- **Triggers**: `control` (snapshot and stream) and `last-known-good`, logged with
  `config_version`.
- **Boot**: one snapshot attempt bounded by `KAIAK_CONTROL_BOOT_WAIT_MS` (default 5 s),
  then last-known-good, then not ready. The listeners bind either way in control mode;
  the API answers `503 config_not_loaded` (error class `not_ready`) from the admission
  stage while no config is in force — the pipeline stages assume a snapshot.
- **Stream position**: `since` is the latest version *taken* — applied or rejected —
  so a rejected config is not replayed on each reconnect; a config event at or below
  it is ignored. After a boot from last-known-good it is that copy's version (unless a
  snapshot was fetched and rejected, then the snapshot's). Clarified in
  CONTROL-PROTOCOL.md, Config stream.
- **Resync**: stream ends → backoff delay → snapshot fetched and applied whatever its
  version → stream reopened after it.
- **Reconnect**: one backoff for every retry (snapshot or stream): full jitter,
  500 ms doubling to a 30 s cap; reset after a stream open ≥ 30 s. Idle stream (no
  block, heartbeat included, for 45 s) is cancelled and reconnected.
- **Protocol mismatch** (response `Kaiak-Protocol` other than exactly `1`, or none):
  logged at error on every attempt, retried on the same backoff, never exits.
- **Rejections**: `Client.LastRejection()` returns the latest rejection only while
  newer than the applied version; `Client.AppliedVersion()`. The client's lock is held
  across the apply, so both always match the config in force.
- **Totals**: `Options.OnTotals` (nil in `cmd/kaiak` for now) receives each decoded
  totals event on the client goroutine.
- **Last-known-good**: `last-known-good.json`, format 1, `{version, config}` (raw
  document), written via `state` after each successful control apply; a write failure
  is logged and the config stays applied.
- **Drain**: the client runs with the other background work and stops after the drain
  (step 5 of Lifecycle), so requests admitted in the grace period see new config and
  totals. Step 7 adds the usage flush before it stops.
- **SIGHUP** in control mode is logged and ignored.
- **Env**: `KAIAK_CONTROL_URL` + `KAIAK_CONTROL_TOKEN` together, never with
  `KAIAK_CONFIG_FILE`; URL must be http(s) with a host and no query; neither source →
  "no config source" error; instance ID shape checked in control mode only.

### Deviations from the plan

- The test double is its own package, `internal/fakecontrol` (like `fakebackend`),
  not an `httptest` server inside the control tests: the e2e test needs the same
  double, and a test file cannot be shared across packages. It imports nothing from
  the gateway, so `control`'s internal tests use it without a cycle.

### For later steps

- Step 7: status reads `Client.AppliedVersion()` and `Client.LastRejection()`; the
  usage flush goes before `stopBackground()` in `run`, after `drain.Run`.
- Step 8: wire `Options.OnTotals` to limits; decide whether the file-mode usage
  snapshot (`limits.json`) still loads/saves in control mode (it does today,
  unchanged). Connection state for the outage timer: a stream connecting (the
  `config stream connected` point in `followStream`) is a successful contact.
