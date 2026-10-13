# Step 3 — the gateway at protocol 8

**Status:** done

## Intent

Move the gateway to protocol 8: the version, `capture` required on every group, and
the flag on the resolved group, read by nothing yet.

## Files likely touched

- `gateway/internal/config/schema.go` — `ProtocolVersion = 8`; `group()` gains
  `"capture": {Required: true, Check: c.Boolean}` (the pattern of `key()`'s
  `disabled`).
- `gateway/internal/config/document.go` — `groupDoc.Capture bool`.
- `gateway/internal/config/snapshot.go` — `Group.Capture`, copied from the group's own
  document in `resolveGroup`; never from a parent or `child_defaults`.
- `gateway/internal/config/fixtures_test.go` — `resolvedGroup` gains `Capture`.
- `gateway/internal/fakecontrol/fakecontrol.go` — the protocol version.
- Every Go test that builds a config (`RESEARCH.md` lists them): protocol 8 and
  `capture` on each group, through the existing helpers where one fits.
- `scripts/live/config.go` — protocol 8; its groups `capture: false` (step 10 turns
  one on).

## Decisions made during planning

- **The flag is a plain copy of the group's own value** — the resolved fixtures pin
  that a parent's value never reaches a child.

## Acceptance criteria

- `scripts/check-gateway.sh` green; `scripts/check-all.sh` green (phase 1 ends here).
- The resolved fixtures pass with `capture` on each group, on both halves.
- A config missing `capture` on a group is refused with the schema error naming the
  group's path.

## Result

### What changed

- `internal/config`:
  - `schema.go`: `ProtocolVersion = 8`; `group()` requires `capture`, a boolean
    (`{Required: true, Check: c.Boolean}`), in the schema's member order.
  - `document.go`: `groupDoc.Capture bool` (a plain bool: the schema has required it).
  - `snapshot.go`: `Group.Capture`, set in `resolveGroup` from the group's own entry
    only, never from the parent or its `child_defaults`. Its comment states the rule and
    points to `GATEWAY.md` → Request capture. Nothing reads it yet (step 6).
- `internal/fakecontrol`: speaks `8`.
- `scripts/live/config.go`: protocol 8; `live`, `live-metered` and `live-media` state
  `capture: false` (step 10 turns one on).
- Tests:
  - `config/fixtures_test.go`: `resolvedGroup.Capture` (a `*bool`, so a fixture that
    leaves `capture` out fails instead of reading as false); `TestResolvedFixtures`
    checks each group's `Capture` against `expected`. New
    `TestCaptureIsRequiredOnEveryGroup`: each of the four new invalid fixtures is
    refused with exactly one issue, at the group that breaks the rule —
    `group-capture-missing` at `/groups/alice` (`missing required field "capture"`),
    `-string`/`-number` at `/groups/app/capture` (`must be a boolean`),
    `child-defaults-capture` at `/groups/users/child_defaults/capture` (`unknown
    field`). `TestInvalidFixtures` checks only the `[schema]` code, so without this
    test any other defect in those files would pass for the wrong reason. This
    follows the pattern of `TestBackendsRequiringAPIKeyEnv`.
  - Every Go config at 7 is now at 8, each group with `"capture": false`. No group
    helper exists in the Go tests, so the groups were edited where they are built, the
    way the protocol 7 bump did it. The per-package builders took the change once:
    `config` (`minimalDoc`), `server` (`testDocWith`), `limits` (`snapshot`,
    `extraGroup`, `treeSnapshot`, `chainSnapshot`), `auth`, `cmd/kaiak` (`writeSeed`)
    and `e2e` (`testConfig`, `groupTreeConfig`, `scenarioConfig`, the media and
    reliability configs, the cross-half config's `transcribers`). The server tests'
    ~25 edits of the base document now match `"research": { "capture": false }`
    where they matched `"research": {}`.
  - `internal/control`: the `Kaiak-Protocol` literals, the mismatch versions (7 is
    now the other version) and the status fixture's `8.0`.
- No dependency added; no docs outside this file touched.

### Mutation checks (reverted)

- `resolveGroup` copying the parent's flag: `TestResolvedFixtures` fails on 6 groups
  of `allowed-models.json` alone, both ways (`capture false, want true` and the
  reverse).
- `capture` not required: `TestInvalidFixtures/group-capture-missing.json` and
  `TestCaptureIsRequiredOnEveryGroup/group-capture-missing.json` fail.

### Suite

- `scripts/check-gateway.sh`: green. gofmt, vet, staticcheck and the telemetry
  boundary are clean; `go test -race -count=1` passes every package (890 tests,
  examples and fuzz targets listed, 47 in `internal/config`). The live-test kit's
  lint is clean and its self-test passes for vllm, llama-server, openai,
  azure-openai, anthropic, azure-anthropic, and vllm with two backends.
- `scripts/check-all.sh`: green, so **phase 1 ends green**. Control `npm test`: 679
  tests, 678 pass, 0 fail, 1 skipped (the store contract's reconnect test, skipped for
  the memory store as before). `npm run lint`: tsc clean, "boundaries ok". The
  cross-half e2e passes. Final lines:

  ```
  ==> cross-half e2e (sample control plane + two gateways; two cores over one store)
  ok  	kaiak/e2e	80.554s
  all checks passed
  ```
