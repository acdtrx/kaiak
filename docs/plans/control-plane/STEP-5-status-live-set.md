# Step 5 — status, live set and pushes

**Status:** done (2026-09-24) — **phase 2 complete** (steps 4–5, suite green)

## Intent

Track gateway status and the live set, and push `totals` (with the live-gateway count)
over the stream.

## Files likely touched

- `control/kaiak-control/src/gateways/` — status intake, live set, timeouts
- `control/kaiak-control/src/usage/` — push scheduling
- Fastify plugin: `POST /v1/status`; stream emits `totals`

## Decisions made during planning

- A gateway is live from its first status until 30 s without one; joining or leaving
  changes the live count and triggers a push to all streams.
- Totals push per stream: after a batch changes totals, at most once a second per
  gateway (coalesced); also on live-count change and on stream connect.
- The expiry sweep is an invocable mechanism with a scheduling timer as its trigger
  (CODING-RULES §7); tests invoke it with an injected clock.

## Acceptance criteria

- Tests: live set join/leave by clock; pushes coalesced to ≤ 1/s; push on connect;
  status fields stored and readable (for the sample page).
- Phase 2 end: full suite green, committed.

## Result

- `control/`: `npm test` — 306 tests, 306 pass, 0 fail (five consecutive runs green);
  `npm run lint` — `tsc` clean, `boundaries ok`.
- `scripts/check-gateway.sh` — `gateway checks passed` (unaffected).
- Phase 2 ends green: no expected reds.

### Decisions made while implementing

- Status intake, live set (draining stays live), expiry sweep and forgetting,
  duplicate-instance rule, totals push rules (connect, publish, live-set change,
  coalescing, slow readers, stall bound): recorded in CONTROL-PROTOCOL.md, Status
  intake and Config stream (settled 2026-09-24).
- `gateways/` subsystem: `createGateways({ store, clock, liveTimeoutMs,
  forgetAfterMs, sweepIntervalMs, onExpirySweep, onListenerError })` →
  `acceptStatus(instance, doc)` returning `{ ok: true, joined, conflictStarted } |
  { ok: false, error: { code, message, status: 400 } }`; `gateways()` (sorted by
  instance: `{ instance, status, receivedAt, live, conflict? }`); `liveGateways()`;
  `onGatewaysChanged(listener)` with `{ liveChanged }` — fired on every accepted
  status (the sample page wants status updates) and on a sweep that expired someone;
  `expireSilentGateways(trigger)`; `startExpirySweep()` / `stopExpirySweep()`
  (idempotent start). Status changes and sweeps run one at a time (one in-process
  queue), so a sweep never overwrites a status that arrived meanwhile.
- Live membership is an explicit stored flag, set by a status and cleared by the
  sweep — not derived from receipt time at read time — so the count in totals and the
  change signal always agree. Cost: expiry precision is the sweep interval (default
  5 s: a gateway leaves 30–35 s after its last status).
- Storage interface gained `gateway(instance)`, `gateways()`, `saveGateway(g)`,
  `deleteGateways(instances)`; `StoredGateway` keeps the status, receipt time, live
  flag, the replaced start time (for the conflict rule) and the conflict.
- Sweep runs report `{ trigger, at, ok: true, expired, forgotten } | { trigger, at,
  ok: false, error }` to `onExpirySweep` (core option, default no-op; the host logs);
  a manual run also resolves with it (rejects on a store failure). The timer's
  trigger is `"schedule"`.
- Core options: `gatewayLiveTimeoutMs` (30000), `gatewayForgetAfterMs` (3600000),
  `expirySweepIntervalMs` (5000), `onExpirySweep`. `ControlPlane` extends `Gateways`;
  `liveGateways: () => 0` replaced by the live set's size. `ListenerEvent` gained
  `{ type: "gateways-changed", change }`.
- Fastify: `POST /v1/status` → `204` no body; `400 { error, detail }` with
  `status-invalid`, the rule code, or `instance-mismatch`; accepted before any config.
  The route logs a join (info) and a raised conflict (warn). The plugin starts the
  core's sweep in `onReady` and stops it in `onClose` (the plugin taking statuses is
  what needs the set to shrink). Default body limit (1 MiB) — a status is tiny.
- Stream (`fastify/gateway-stream.ts`, renamed from `config-stream.ts`): subscribes to
  configs, totals changes and gateway changes (live changes only) before reading the
  replay; pushes requested before the replay is written are dropped because the
  connect push follows it. Plugin options `totalsPushIntervalMs` (1000) and
  `stalledStreamTimeoutMs` (30000). Coalescing keeps one trailing timer per stream;
  interval measured with `performance.now()`. A totals read in progress absorbs
  further requests into one follow-up. A write that returns `false` starts the stall
  timer and waits for `drain`; the stall ends the stream with `destroy()` (an `end()`
  would wait on the same stuck buffer).
- A publish also triggers a totals push (after its config event): `config_version`
  and the set of limits change with it.

### Tests

- `gateways/gateways.test.ts` (14): join/leave by injected clock, draining stays
  live, forgetting, timer trigger (`"schedule"`, idempotent start), failed sweep
  reported, validation codes, instance mismatch, restart vs alternation, conflict
  clearing, listener isolation.
- `fastify/status-totals.test.ts` (15, real listening app): 204 and readable state,
  status before config, 400 codes, 401, plugin starts/stops the sweep; totals after the
  replay, none on resync, after a counted batch, after a publish, live-count pushes on
  join and on expiry, coalescing (leading + one trailing with the latest, next push is
  the next event); slow readers with a paused raw socket (HTTP/1.0, 12 × 1 MiB configs
  fill the buffers): the stall bound ends the stream, and held totals arrive once with
  the latest count.
- `control-plane.test.ts`: live count in totals and acks, expiry lowers it,
  `gateways-changed` listener errors.
- Existing stream tests now skip `totals` events where they look for configs.
- Mutation-checked: removing the drain hold, the stall `destroy()`, the coalescing
  wait or the connect push each fails its test.

### Deviations from the plan

- None in substance. "At most once a second per gateway" is implemented per stream
  (one stream per gateway).

### Doubts

- Two processes under one instance name also break usage de-duplication: each epoch
  alternation counts as a fresh spool, so a resend after a flip is counted twice. The
  flag makes the misconfiguration visible; fixing it is not attempted.
- The live set and gateway records sit in the store, but the change signals and the
  sweep timer are per process: a multi-replica control plane on a shared store would
  need its own cross-replica notification (the real control plane's concern).

### For later steps

- Step 7: the gateway sends status on connect, on change and every 10 s; `204` =
  accepted, `400` = a bug on the gateway side (log it).
- Step 8: `live_gateways` is now real; 0 reads as 1.
- Step 10: `gateways()` for the list; `onGatewaysChanged` for live updates (fires on
  every accepted status); `conflict` for the duplicate-instance flag;
  `onExpirySweep` for logging sweeps.
