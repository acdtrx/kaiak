# Step 1 — scaffolding

**Status:** done — all acceptance criteria met

## Intent

Create the two halves' skeletons and the commands every later step runs, so the suite
and the lints exist before any feature code.

## Files likely touched

- `gateway/go.mod`, `gateway/cmd/kaiak/main.go` (starts, logs, exits cleanly)
- `scripts/check-gateway.sh` — gofmt check, `go vet`, pinned staticcheck, `go test -race`
- `control/package.json` (workspaces), `control/kaiak-control/`, `control/sample/`
  (empty package with a placeholder entry), `tsconfig` base
- `control/scripts/check-boundaries.ts` — boundary lint on the TypeScript parser
- `docs/ARCHITECTURE.md` — first version: the gateway package map below, the
  two-halves diagram
- `AGENTS.md` commands line; `docs/TECH-STACK.md` dependency inventory (versions)

## Decisions made during planning

- Gateway packages (flat under `internal/`): `config`, `server` (listeners, pipeline
  wiring), `auth`, `routing`, `provider`, `accounting`, `limits`, `metrics`, `state`
  (data-directory files), `fakebackend` (test backend). The subagent may merge or
  rename with reason; the result goes in `ARCHITECTURE.md`.
- Go and Node at current stable, verified at scaffold time; staticcheck at latest.
- Boundary lint: every import across subsystem folders must target the folder's
  entry file; the import graph must be acyclic; imports are read with
  `ts.preProcessFile` / the TS AST, never regex.
- `npm run lint` = `tsc --noEmit` (strict, `noUncheckedIndexedAccess`,
  `exactOptionalPropertyTypes`, `noUnusedLocals`, `noUnusedParameters`) + boundary lint.

## Acceptance criteria

- `scripts/check-gateway.sh` passes; `go run ./cmd/kaiak` starts and exits on SIGINT.
- `npm test` and `npm run lint` pass in `control/`.
- The boundary lint fails on a planted deep import and a planted cycle (tests).

## Result

Commands run (2026-09-24):

- `scripts/check-gateway.sh` (from `/`) — gofmt, `go vet`, staticcheck 2026.2.1,
  `go test -race` — **pass**.
- Built binary: SIGINT and SIGTERM each log `kaiak stopped` and exit 0. `go run
  ./cmd/kaiak` with Ctrl-C (SIGINT to the process group): the program logs the same
  clean stop; `go run` itself then exits 1, which is `go run`'s own reaction to the
  interrupt, not the program's status.
- `npm test` in `control/` — 5 tests **pass** (both packages, three boundary-lint
  tests: clean fixture passes, planted deep import fails, planted cycle fails).
- `npm run lint` in `control/` — `tsc -p .` clean, then `boundaries ok` — **pass**.
  Needed `@types/node` (dev, 26.6.2) for Node's built-in types; added with the
  user's approval.

Decisions beyond the plan:

- Go module path `kaiak`; `go 1.26.6` (the installed toolchain).
- TypeScript 7 exposes its parser only via `typescript/unstable/*`; the boundary lint
  uses `unstable/sync` (`SourceFile.imports`, the parser's own specifier list).
  Recorded in `docs/TECH-STACK.md`.
- Boundary lint checks relative specifiers only (bare specifiers are package entries,
  fixed by `exports`); type-only and dynamic imports count as graph edges; workspace
  list is read from `control/package.json`, so new packages are covered automatically.
- One shared `control/tsconfig.json` covering both packages and `scripts/`; boundary
  fixtures live in `control/scripts/fixtures/boundaries/` and are excluded from `tsc`
  (a `test/` directory would be picked up by `node --test`'s default patterns).
- Sample package name `kaiak-sample`; it imports `kaiak-control` to prove the
  workspace link and type stripping through it.
- `main.go` split into `main` (signals, exit code) and `run(ctx, logger)` so the
  shutdown path has a test. Log handler: text for now — the JSON/text switch arrives
  with config.

Version gaps (nothing installed): current stable is Go 1.27.1 and Node 26.10.0; the
installed Go 1.26.6 and Node 26.7.0 were used. The user will upgrade Go (to 1.27.x)
and Node (to 26.10) later; `go.mod`'s `go` directive is bumped then — no action now.
