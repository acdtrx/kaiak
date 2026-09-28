# Step 3 — gateway config loading

**Status:** done (2026-09-24) — ends phase 1; phase 1 complete, suite green.

## Intent

The gateway reads, validates and swaps config: strict decode, semantic validation,
immutable snapshot, atomic replacement, reload on SIGHUP, rejection logged.

## Files likely touched

- `gateway/internal/config/` — types, strict decode, validation, snapshot holder
- `gateway/internal/state/` — data-directory resolution and versioned-file helpers
  (used by step 8's snapshot)
- `gateway/cmd/kaiak/main.go` — env (`KAIAK_CONFIG_FILE`, `KAIAK_DATA_DIR`,
  `KAIAK_INSTANCE_ID`, listen addresses), wiring
- `docs/specs/GATEWAY.md` — reload behavior recorded

## Decisions made during planning

- **File-mode reload on SIGHUP** (standard library; no file watcher — watching needs
  per-OS syscalls or a dependency, and Kubernetes ConfigMap updates can trigger a
  SIGHUP via the user's tooling). A reload that fails validation is logged and the
  running config stays.
- The Go test suite runs **the same fixtures** from `protocol/fixtures/config/` —
  every valid one decodes and validates, every invalid one is rejected.
- Starting with no `KAIAK_CONFIG_FILE` (and no control URL, which arrives in P2) is a
  startup error with a clear message.
- Readiness flag is set once a config is loaded (served in step 4).

## Acceptance criteria

- Fixture tests pass; a unit test shows a request-holding reader keeps its snapshot
  across a swap.
- Manual: `kaiak` with a valid file starts; editing it to be invalid + SIGHUP logs the
  rejection and keeps serving the old config.
- Phase 1 end: full suite green, committed.

## Result

Commands run (2026-09-24):

- `scripts/check-gateway.sh` — gofmt, `go vet`, staticcheck 2026.2.1 clean;
  `go test -race ./...` **pass** (`cmd/kaiak`, `internal/config`, `internal/state`).
  Also `go test -race -count=10 ./...` — pass, no flakes.
- `npm test` in `control/` — 86 tests **pass**; `npm run lint` — `tsc` clean,
  `boundaries ok`.
- Manual, built binary with a scratch copy of `valid/full.json` (API-key env vars
  set), text logs: startup `config applied … backends=3 models=4 keys=6`; edited
  `output_limit.default` above its ceiling + SIGHUP → `config rejected …
  codes=[output-limit-default-above-ceiling] running_config=kept`; restored a valid
  file + SIGHUP → `config applied trigger=sighup`; SIGINT → `kaiak stopped`, exit 0.
  Also: no `KAIAK_CONFIG_FILE` → clear error, exit 1; truncated JSON → `codes=[syntax]`,
  exit 1; `KAIAK_LOG_FORMAT=yaml` → error, exit 1; default log format is JSON. Scratch
  files deleted.

Delivered:

- `gateway/internal/config`: `Parse(data) (*Snapshot, error)` — syntax check (exactly
  one JSON value), a structural walker equivalent to `config.schema.json` (every issue
  with its JSON Pointer path), a strict typed decode (`DisallowUnknownFields`), then the
  semantic rules with the contract codes. Rejections are `*ValidationError{Issues}`,
  each `Issue{Code, Path, Message}`; `Codes()` gives the distinct codes.
- `Snapshot` (immutable by contract): backends with default timeouts applied; models
  with deployments pointing at their backend, raw-JSON `defaults`, typed output limit
  and prices (`time.Time` effective dates); teams; workloads (team resolved); users with
  effective allowed models and limits already merged over `global.default_user`; keys
  by ID plus `KeyByHash`, each key's owner resolved (`Workload` or `User`).
  `ModelSet` expands `"*"` to concrete names and remembers it was `"*"` (`All()`).
- `Holder` (`atomic.Pointer`): `Current`, `Swap`, `Loaded` (the readiness condition for
  config, served in step 4).
- `FileLoader.Load(trigger)`: read → parse → credential presence check → swap, logging
  the trigger and the result; serialized by a mutex. `cmd/kaiak` wires startup and
  SIGHUP as triggers; the SIGHUP loop is owned by `run` and stops on context cancel.
- `gateway/internal/state`: `Open(path)` creates the data directory;
  `WriteVersioned` / `ReadVersioned` store a `{format_version, data}` envelope, written
  atomically (temp file in the same dir, fsync, rename, dir fsync); another version is
  deleted and logged; a corrupt file is an error for the caller to decide on.
- Fixture parity: `internal/config/fixtures_test.go` reads `protocol/fixtures/config`
  in place (`../../../protocol/…`); every valid fixture parses; every invalid one is
  rejected with exactly `[schema]` or exactly its semantic code; the test fails if
  `cases.json` and the fixture files differ in either direction.
- Docs: `GATEWAY.md` Configuration sources (data dir, log format, startup errors,
  SIGHUP reload, rejection codes, defaults at load); `ARCHITECTURE.md` package map.

Decisions beyond the plan:

- **Structural validation walks the generic JSON tree** rather than relying on typed
  decode errors: `encoding/json` errors carry no map keys or array indexes, and its
  unknown-field error no path at all, which would leave an operator guessing which
  backend is wrong. The typed decode with `DisallowUnknownFields` still runs after it,
  so a schema field the Go types lack fails loudly instead of being dropped.
- **Numbers follow JSON Schema, as ajv does**: `8192.0` is an integer; `null` is never
  "absent" (rejected wherever the schema has no null); the ECMAScript `\s` in the
  `base_url` pattern is spelled out in Go (Go's `\s` omits `\v` and Unicode spaces).
- **`api-key-env-unset`**: a backend's API-key variable must be set and non-empty at
  load time (startup and every reload). Gateway-side only (the shared fixtures carry no
  environment), recorded in `GATEWAY.md`. The value itself is read by step 5.
- **`syntax` code** for a document that is not one JSON value (kaiak-control's caller
  parses before validating, so the contract has no such code).
- Env reading lives in `cmd/kaiak` (one place for all process settings); `state` owns
  the directory and `DefaultDir`. Listen addresses are left to step 4.
- Disabled and expired keys stay in the snapshot (`KeyByHash` returns them); auth
  (step 4) decides how to answer them.

Deviations: none from the acceptance criteria.

Open doubts:

- Integers ≥ 2^63 are rejected ("too large") where ajv would accept them; no real
  config comes near.
- Issue order differs from kaiak-control's (Go walks sorted keys, ajv document order);
  only the codes are part of the contract.
