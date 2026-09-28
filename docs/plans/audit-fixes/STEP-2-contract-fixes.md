# Step 2 — contract fixes (both halves)

**Status:** done (2026-09-25) — commits `5d83f24`, `22cdb11`, `b7b2480`, `7a6cd34`, `983f342`

## Intent

Fix the protocol-level findings in one change on both halves.

## Items

- **M10** `config_epoch` (random per control-plane store) in snapshot and stream; a
  different epoch → resync whatever the version numbers; the gateway stores it with LKG.
- **H12 / D7** store contract: the counted-batch write is conditional on the last batch
  ID (the store reports "duplicate" atomically); the memory store implements it; a
  store lease so a second control-plane process on the same store refuses to start
  (memory store: trivially one); spec states one process per store. Test: two cores on
  one store count a batch once.
- **M14** reject userinfo in `base_url` (schema + Go walker + fixtures).
- **L6** schema `maximum` 2^53−1 on config integers; **L7** reject non-finite `defaults`
  numbers in Go, document the JS number bound; **L9** app-level `Kaiak-Protocol` hook,
  drop unreachable branches; **L4** prune `lastBatches` with the forget sweep,
  `bodyLimit` 64 KiB on `/status`, document the shared-token trust model; **L14**
  fixtures (integer spellings, ID/instance length boundaries).

## Acceptance criteria

- Fixtures pass/fail on both halves; the new behaviors tested; specs updated.

## Result

- **M14 / L6 / L7 / L14** (`5d83f24`, both halves): `config.schema.json` bounds every
  config integer and limit `value` at 2^53 − 1 (Go: `IntegerAtLeast` now caps there,
  `NumberBetween` for `value`); `base_url` refuses `@` in the authority (schema
  pattern and Go regex); `defaults` values go through a recursive `json_value` so
  ajv's strict numbers refuse `1e400` at any depth, and the Go walker's
  `FiniteNumbers` does the same. Fixtures: valid `integer-spellings` (`1e6`, `1E2`,
  `30000.0`, `8192.0`, `1.024e3`), valid `at-bounds` (every bounded integer at 2^53 − 1,
  IDs of 128); invalid `base-url-userinfo`, `base-url-user-only`,
  `integer-above-safe`, `integer-above-int64` (`1e20`), `limit-value-above-safe`,
  `defaults-number-out-of-range`, `defaults-nested-number-out-of-range`,
  `id-too-long` (129); messages: status `instance-253` (valid) / `instance-254`,
  `rejection-codes-duplicate`, totals `window-models-duplicate`, usage-record
  `request-id-128` (valid) / `request-id-129`, `key-id-129`, `instance-254`.
  `TestHugeMaxInFlightSaturates` now saturates from 2^53 − 1 (1e18 is no longer a
  valid config).
- **M10 config epoch** (`22cdb11`): wire — snapshot `{ config_epoch, version, config }`
  (32 lowercase hex, `common.schema.json#/$defs/config_epoch`), `config` events carry
  the same snapshot, stream `GET /v1/stream?since=<v>&config_epoch=<e>` (both
  required; either missing or malformed → `400 since-invalid`; another epoch →
  `resync`). Store: `configEpoch()` (memory store: random per store). Gateway: keeps
  `{epoch, version}` as its position, applies a snapshot or event of another epoch
  whatever its version, last-known-good format 2 carries the epoch (a format-1 file
  is discarded at boot, as the data-directory rule says). Tests:
  `TestLastKnownGoodFromAnotherEpochIsReplacedAtTheSameVersion` (the [A] scenario),
  `TestStreamConfigFromAnotherEpochIsAppliedWhateverItsVersion`, stream/config-versions
  tests for another epoch → resync, epoch per store.
- **H12 / D7** (`b7b2480`): `saveCountedBatch(counted, expectedLast, keepRecords)` →
  `{ saved: true } | { saved: false, last }` — compare-and-write in one atomic store
  operation; a lost race is decided again against the returned last batch (so it
  becomes `duplicate`, or `next`/`gap` behind the winner). Lease:
  `acquireLease(holder, now, expiresAt)` → `{ ok } | { ok: false, lease }`,
  `releaseLease(holder)`; the core's `start()` takes it (holder = the control-plane
  ID), renews every TTL/3 (default TTL 30 s), `stop()` releases it after any renewal
  in flight; a held lease → `store-lease-held` naming holder and expiry; a failed
  renewal → `onStoreLeaseLost` (default: rethrow, stopping the process). The Fastify
  plugin calls `start` on ready (so `app.ready()` fails) and `stop` on close. Tests:
  "two cores sharing one store count a batch once" (the [B] reproduction — fails with
  `['first','first']` when the store ignores the expected ID), the lost-race re-decide,
  memory-store conditional write and lease, core start/refuse/expire/renew-lost, plugin
  refuses to start on a held store.
- **L4** (`b7b2480`, `7a6cd34`): the memory store's `deleteGateways` (the forget
  sweep) drops the instance's last batch too (test at store and core level); status
  `bodyLimit` 64 KiB (413 test); the shared-token trust model in CONTROL-PROTOCOL.md
  (Control-plane processes → Trust model); backlog entry "Per-instance gateway
  tokens" with its revisit trigger.
- **L9** (`7a6cd34`): the plugin's not-found handler answers `404 not-found` behind the
  request checks, so unknown paths, undefined methods and HEAD on the stream carry
  `Kaiak-Protocol`; the checks read one value per header — the "repeated" branches are
  gone (Node joins repeated `Kaiak-Protocol`/`Kaiak-Instance` into `1, 1`, keeps the
  first `Authorization`); tests over real HTTP for both.
- **Specs**: CONTROL-PROTOCOL.md — Endpoints, Request checks (repeated headers,
  `not-found`, 64 KiB, the header on every answer), Config versions (Config epoch),
  Config stream, Messages table, Config (integer bound, `defaults` numbers, no
  userinfo), Usage intake (Exactly once in the store), Status intake (forgetting drops
  the last batch), new section Control-plane processes (one process per store, the
  lease, the trust model). GATEWAY.md — Stream and Last-known-good. ARCHITECTURE.md —
  storage, usage, control-plane, fastify lines.

## Decisions made during implementation

- The conditional write is compare-and-write on the expected last batch ID rather than
  the store running the outcome rules itself: the store stays free of domain logic,
  and the duplicate decision still rests on the store's atomic comparison.
- The epoch travels as its own query parameter, `config_epoch`, under the existing
  `since-invalid` code; `since=0` stays valid (with an epoch).
- Lease holder = the control-plane ID (the totals revision's); expiry is by the
  holder's clock (`now`, `expiresAt` passed in) — a database store may prefer its own
  clock.
- The lease is taken by `start()`, which the plugin calls; core operations are not
  gated on it (a host using the core without the plugin must call `start()`).
- A URL Fastify cannot decode is refused by the host app before the plugin's prefix
  applies, so it lacks `Kaiak-Protocol`; documented as outside the plugin's reach
  (gateways never send one) instead of a root-level hook on the host's other routes.

## Suite

`scripts/check-all.sh` (2026-09-25, at `983f342`) → gofmt, vet, staticcheck clean;
`go test -race ./...` ok (gateway e2e 35.5 s); live-test kit self-test passed; control
`npm test` 427 pass / 0 fail; lint `boundaries ok`; cross-half e2e ok (48.5 s);
`all checks passed`. No expected reds.
