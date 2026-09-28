# Plan: control plane (P2)

## Goal

Gateways run from a control plane: config pushed live, usage flowing back in batches,
totals and budgets shared across replicas, status visible — and still serving when the
control plane is gone. Delivered as the `kaiak-control` library, the sample control
plane with its read-only live page, and the gateway's control-plane mode.

## Scope

- `protocol/`: schemas and fixtures for every message — config snapshot, stream events
  (`config`, `totals`, `resync`), usage batch and ack, status — and
  `global.control_outage_grace_ms` in the config document.
- `kaiak-control`: config versions and serving, key generation, usage intake with
  batch de-duplication, totals per scope and window, the live-gateway set, status
  tracking, the storage interface with an in-memory store, the Fastify plugin.
- Gateway control-plane mode: snapshot + stream, last-known-good config, status
  reporter, usage batch sender with spool, enforcement from pushed totals and
  live-gateway shares, the outage policy.
- Sample control plane: config file watched and pushed, `keygen` command, the live
  page.
- An end-to-end test across both halves.

## Out of scope

- Backend health, circuit state, queue depth in status (P3).
- A real database store — the storage interface is the seam; the real control plane
  (separate project) implements it.
- Container images (`docs/BACKLOG.md`, Delivery).

## Constraints

- Protocol decisions as settled in `docs/specs/CONTROL-PROTOCOL.md` (2026-09-24):
  one token gateway → control plane, `Kaiak-Protocol` / `Kaiak-Instance` headers,
  usage batches (one outstanding, instance+epoch+sequence IDs, ack carries totals),
  totals not allowances, live-gateway count for per-minute shares, timings.
- The control plane is never on the request path; the gateway never blocks on it.
- Gateway: standard library only. `control/`: Fastify, pino, pino-pretty (dev) enter
  at latest stable (already in the stack decision); nothing else without asking.
- `kaiak-control` core stays HTTP-framework-agnostic; Fastify is an adapter.
- Protocol changes land on both halves in the same change.

## Risks

- **Timing-sensitive tests** (batches, live-set timeouts, pushes) — injectable clocks
  and schedulers on both sides; no sleeps.
- **Two clocks** — windows are the control plane's; the gateway's view is pushed
  totals plus its own unacked usage. Tests must cover an hour/month rollover on the
  control-plane side while a batch is outstanding.
- **SSE through Fastify** — long-lived responses, backpressure, client disconnects;
  prove with a real HTTP client in tests, not only `inject`.
- **Spool correctness** — crash between send and ack must neither lose nor
  double-count; tested with a killed gateway process.

## Phases and steps

- **Phase 1 — protocol and `kaiak-control` core** (steps 1–3). Ends with: a Fastify
  app using the kit serves config snapshots and a resumable config stream.
  1. `STEP-1-protocol-messages.md`
  2. `STEP-2-kit-core.md`
  3. `STEP-3-fastify-config-stream.md`
- **Phase 2 — usage, totals and status in the kit** (steps 4–5). Ends with: the kit
  takes batches exactly once, keeps totals, tracks live gateways and pushes totals.
  4. `STEP-4-usage-intake-totals.md`
  5. `STEP-5-status-live-set.md`
- **Phase 3 — gateway control-plane mode** (steps 6–8). Ends with: a gateway runs
  from a kit-based server, reports usage and status, and enforces shared totals.
  6. `STEP-6-gateway-config-client.md`
  7. `STEP-7-gateway-usage-status.md`
  8. `STEP-8-gateway-enforcement-outage.md`
- **Phase 4 — sample control plane and end to end** (steps 9–11).
  9. `STEP-9-sample-app.md`
  10. `STEP-10-sample-page.md`
  11. `STEP-11-e2e.md`

## End-to-end verification

1. `scripts/check-gateway.sh`, `npm test`, `npm run lint` green.
2. The cross-half e2e (step 11): the sample control plane, two `kaiak` processes and
   the fake backend — config edit pushed to both; usage from both reflected in shared
   totals and a USD limit tripping across replicas; per-minute limit split by two;
   a lost ack not double-counted; control plane stopped → gateways keep serving,
   money-limited model fails closed after the (shortened) grace, recovers on return,
   spool delivered; gateway restarted with the control plane down boots from
   last-known-good; drain flushes the spool.
3. Manual, with the user: the sample page in a browser while traffic runs.

Status (2026-09-24): items 1 and 2 automated and green — `scripts/check-all.sh` runs
both (step 11). Item 3 is pending the user.
