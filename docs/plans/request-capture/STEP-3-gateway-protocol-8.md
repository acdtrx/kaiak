# Step 3 — the gateway at protocol 8

**Status:** not started

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
