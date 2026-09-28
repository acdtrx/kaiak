# Step 3 — Fastify plugin: config and stream

**Status:** done (2026-09-24) — **phase 1 complete** (steps 1–3, suite green)

## Intent

The HTTP adapter for config: `GET /v1/config` and `GET /v1/stream` (SSE with resume
and resync), with auth and protocol headers.

## Files likely touched

- `control/kaiak-control/src/fastify/` — the plugin; `control/kaiak-control/package.json`
  (fastify, pino as needed — latest stable, verified)
- `docs/TECH-STACK.md` inventory

## Decisions made during planning

- Plugin options: the core instance, the token, route prefix. It registers routes
  only; logging config belongs to the host app.
- SSE: `event:` names as in the spec, `id:` = config version for `config` events,
  heartbeat comment every 15 s (scheduling timer) so idle proxies keep the stream;
  client disconnect unsubscribes.
- `since` equal to current → no replay; within history → replay newer configs; else
  one `resync` event.

## Acceptance criteria

- Tests with a real HTTP client against a listening app: snapshot, stream receives a
  published config, resume/replay, resync, auth 401, protocol mismatch, disconnect
  cleans up the subscription.
- Phase 1 end: full suite green, committed.

## Result

- `control/`: `npm test` — 247 tests, 247 pass, 0 fail; `npm run lint` — `tsc` clean,
  `boundaries ok`.
- `scripts/check-gateway.sh` — `gateway checks passed` (unaffected).
- Phase 1 ends green: no expected reds.

### Changes to step 2's core (decided by the main session)

- A throwing config listener no longer fails the publish: `publishConfig` resolves
  `{ ok: true, published }` once stored, every listener is isolated, and each failure
  goes to `onListenerError(error, published)` (`createControlPlane` option). Default:
  rethrow from a microtask — a listener bug surfaces as an uncaught exception, as a
  throwing event listener does; a host that would rather keep running passes a
  handler that logs. Rejected: `console.error` (a library writing to the console
  behind the host's logger) and making the option required (boilerplate for every
  host for a case that is a bug).
- Spec (Config versions): after a `resync` the gateway applies the snapshot even when
  its version is lower than the one it runs (control-plane restart); step 6.

### Decisions made while implementing

- Subsystem `fastify/` in `kaiak-control`: `controlProtocolPlugin` (entry export),
  options `{ controlPlane, prefix?, heartbeatIntervalMs? }`. `prefix` is Fastify's own
  register option; when the host passes none the plugin mounts its routes under
  `/v1` through an inner registration. A plain encapsulated plugin — no
  `fastify-plugin`, nothing leaks into the host's context.
- The plugin imports only Fastify's types; it runs on the host's instance. `fastify`
  is a regular exact-pinned dependency of `kaiak-control` (types, and the tests' real
  server) — not a peer dependency: inside this workspace the sample uses the same
  copy.
- Checks run in an `onRequest` hook of the plugin's context, after it sets
  `Kaiak-Protocol: 1`, so every answer from its routes, errors included, carries the
  header (Fastify's 404 for an unknown path does not). The context's error handler keeps `{ error, detail }`: a 4xx
  Fastify raises → `request-invalid` with its message; anything else → `500
  internal-error`, logged through `request.log`, no detail. Steps 4–5 (JSON bodies)
  inherit both.
- `GET /v1/config` before the first publish → `503 config-unavailable` (spec,
  Endpoints).
- `since` required, `^(0|[1-9][0-9]*)$` and a safe integer, one value → else `400
  since-invalid` (spec, Config stream). `since=0` follows the core: replay from 1,
  or resync with nothing published.
- Stream: `reply.hijack()`, the reply's headers copied onto the raw response, then
  `writeHead(200)` with the event-stream headers and `flushHeaders()`. HEAD is not
  exposed on `/stream` (it would hold a bodiless stream open).
- Race-free resume as step 2 prescribed: subscribe, then `configsSince`; versions
  pushed before the replay is written are held and sent after it; every send skips a
  version at or below the last sent. Both race orders are tested (publish before the
  replay read; publish after the read, before the write).
- `resync` → one `resync` event, then the stream ends (spec: why not keep it open).
- Heartbeat `: heartbeat` comment on a `setInterval` (default 15 s, injectable),
  skipped while `writableNeedDrain`.
- Backpressure: events are written through Node's writable buffer and wait there for a
  slow reader — nothing dropped, no second queue in front of Node's (it would hold the
  same bytes). Not exercised by a test: filling a loopback socket's buffers with
  configs is impractical; the code path is one `writableNeedDrain` check.
- Disconnect: the raw response's `close` → unsubscribe, clear the heartbeat, drop the
  stream from the open set. The plugin's `preClose` hook ends every open stream, so
  `app.close()` does not wait on gateways (tested; without it the close hangs).
- Tests (`fastify/fastify.test.ts`, 31): a real listening app and Node's `fetch`,
  with a small SSE parser (`fastify/sse-client.ts`: fields, comments, blank-line
  dispatch). Subscriptions are counted by wrapping the core in the test — no
  production API for it. Mutation-checked: removing the dedupe, the pending hold, the
  unsubscribe or the `preClose` hook each fails its test.

### Deviations from the plan

- The plugin takes the core, not a separate token: the token lives in the core
  (`createControlPlane`), which runs the checks.
- The `fastify` dependency landed in the core-fix commit rather than the plugin
  commit (staged together by mistake); history left as is.

### For later steps

- Steps 4–5: add `POST /usage` and `POST /status` inside `registerGatewayRoutes`
  (`fastify/index.ts`) — the hook, header and error handler apply. The checked
  instance is not yet handed to routes; step 5 needs it (body instance must equal the
  header's): run `checkGatewayRequest` result through a `WeakMap<FastifyRequest,
  string>` or re-read the header. A `totals` event goes through the same raw write
  path as `sendConfig` in `fastify/config-stream.ts` — its subscription joins the
  config one (subscribe before reading, unsubscribe on close).
- Step 6: the stream ends after `resync` and on control-plane shutdown; reconnect
  with the version it runs. `503 config-unavailable` from `/config` means retry.
