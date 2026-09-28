# Step 9 — sample control plane app

**Status:** done (2026-09-24)

## Intent

The thin app on `kaiak-control`: Fastify, config from a watched file, in-memory store,
`keygen` command.

## Files likely touched

- `control/sample/src/` — app, config file loader + watcher, keygen CLI
- `control/sample/package.json` — `dev`, `start`, `keygen` scripts
- `README.md`, `AGENTS.md` commands

## Decisions made during planning

- Env: config file path, listen address, token (required), log format.
- Logging: pino; dev uses `pino-pretty` with `translateTime: 'HH:MM:ss.l'`,
  `singleLine: true`, ignore `pid,hostname,reqId,req.host,req.remoteAddress,req.remotePort`,
  per-request logging on; production plain JSON.
- File watch (`fs.watch`, debounced): valid edit → published as a new version; invalid
  → rejected, current version stays, the error kept for the page.
- `npm run keygen -w control/sample -- --id <key-id>` prints the key once, its ID and
  hash, ready to paste into config (and usable for file mode).

### Decisions made during implementation

- **Structure**: subsystems `settings`, `logging`, `config-file`, `app`, `keygen`
  under `control/sample/src/`; process entries `main.ts` (server) and
  `keygen-cli.ts`. `createSampleApp({ configFile, token, logger, configDebounceMs })`
  returns `{ app, controlPlane, configFile }` — step 10 registers its page routes on
  `app` and reads `configFile.state()` (`lastRun`, `lastFailure`) and the core.
- **Env**: `KAIAK_SAMPLE_CONFIG` (required), `KAIAK_CONTROL_TOKEN` (required, the
  gateway's name), `KAIAK_SAMPLE_LISTEN` (default `127.0.0.1:8090`, clear of the
  gateway's 8080/9090 and the fake backend's 8000; `:port`, `[v6]:port`, port 0),
  `KAIAK_LOG_FORMAT` (`json` default / `text`, mirroring the gateway — recorded in
  `docs/TECH-STACK.md`, Logging). A relative config path resolves against `INIT_CWD`
  (where `npm` was run) or the working directory. Missing or invalid → one line on
  stderr naming the variable, exit 1.
- **Readiness** (for step 11): once listening, main logs
  `sample control plane listening on <url>` with a `url` field; port 0 works.
- **Startup with a missing or invalid file**: the server starts, logs the rejection
  at error level, and answers gateways `503 config-unavailable` until the watcher
  publishes a fixed file. One reload path: startup is just the first trigger.
- **Reload** is `configFile.reload(trigger)`, serialized; every run records trigger,
  time and result (`published` / `unchanged` with the version, or the failure with
  `file-unreadable` / `json-invalid` / `config-invalid` + issues). The latest failure
  is kept until a run succeeds. **Unchanged**: a file whose parsed document equals the
  one last published from it makes no new version (editors and `touch` rewrite files
  unchanged; gateways would otherwise re-apply identical versions).
- **Watcher**: `fs.watch` on the file's directory, filtered to the file name (or any
  event when the platform gives no name), debounced 200 ms; started before the
  startup read so an edit between the two is not missed; stopped in `onClose`.
  Watcher failures are logged; reload stays invocable.
- **Logging**: each run is one line (`config file published as version N`,
  `… unchanged`, `config file rejected (<code>): …` with the issues in a
  `rejection` field — not `error`, which pino-pretty treats as an error object and
  drops from single-line output). Expiry sweeps log only when they changed something
  or failed; listener failures at error.
- **Shutdown**: SIGTERM/SIGINT → `app.close()` (kit hooks end the streams and stop
  the sweep, the app's stops the watcher) → exit 0.
- **keygen**: `--id` plus exactly one of `--workload` / `--user` (a complete entry to
  paste); prints `key:`, `id:`, `hash:` lines, a note that the key is shown once, and
  the `keys` member as one line of JSON. Invalid key ID or arguments → stderr + usage,
  exit 1. The owner ID's shape is left to config validation on publish.
- **Local config**: `examples/local-config.json` — one model `demo` on the fake
  backend (`127.0.0.1:8000`), priced, a team, a workload, a user, empty `keys` (filled
  with keygen). `examples/config.json` stays the production-shaped reference; the
  local one serves both file mode and the sample.
- **Dependencies**: `pino-pretty 13.1.3` (dev, latest, published 2026-07) and
  `fastify 5.12.5` as a direct dependency of the sample (the kit's pin, deduped — not
  a new package).
- **Deviation**: the workspace flag is `-w sample` from `control/` (`npm run dev -w
  sample`); `-w control/sample` finds no workspace from `control/` and there is no
  root `package.json`. AGENTS.md's Commands line now says so.

## Result

- `control/`: `npm test` — 339 tests, 339 pass (22 in `sample`: settings, keygen incl.
  the CLI process, config file incl. real-fs watcher edits, rename-replace and invalid
  edits, app incl. a real stream push and 503-then-publish, main as a process incl.
  readiness line, text logs and SIGTERM → 0). `npm run lint` — `boundaries ok`.
- `scripts/check-gateway.sh` — `gateway checks passed`.
- Manual (scratch dir, deleted): fake backend + `npm run dev -w sample` + built
  `kaiak` in control mode. Gateway booted from version 1 (`keys=0`) and a request
  with a fresh keygen key got 401; the keygen entry pasted into the file → sample
  `published as version 2`, gateway `config applied config_version=2 keys=1`, the
  same request 200. `{ broken` → `rejected (json-invalid)`; an unknown workload →
  `rejected (config-invalid)` with `key-owner-unknown` in the line; both left the
  gateway on version 2. A rename-replace save → version 3 applied. SIGTERM → the
  sample logged `closing`, ended the gateway's stream, exited; restarted, the gateway
  resynced to the new process's version 1.
- Expected reds: none.

## Acceptance criteria

- Tests: file edit → new version on the stream; invalid edit kept out with its error;
  keygen output format and hash; app boots with the kit plugin mounted.
