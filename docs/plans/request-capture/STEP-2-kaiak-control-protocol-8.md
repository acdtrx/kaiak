# Step 2 — `kaiak-control` at protocol 8

**Status:** not started

## Intent

Move `kaiak-control` and the sample to protocol 8: the version, the group's required
`capture`, and the guide telling apps what it means and who decides.

## Files likely touched

- `control/kaiak-control/src/protocol/index.ts` (`PROTOCOL_VERSION = 8`) and its test.
- `control/kaiak-control/schema/` — the synced copy (`npm run sync-schemas`).
- `control/kaiak-control/src/config/types.ts` — `Group.capture: boolean` (required).
- The resolved-fixture test (`src/config/limits.test.ts`): `expected` groups carry
  `capture`; the test reads it from the group itself — no resolution function: the
  flag is not resolved along the path.
- TypeScript tests that build groups inline (`config.test.ts`, `limits.test.ts`,
  `config-publishing.test.ts`, `control-plane.test.ts`) — through one helper where
  one fits.
- `control/kaiak-control/GUIDE.md`:
  - protocol 8;
  - **a section on capture:** what the flag does (`GATEWAY.md` → Request capture);
    the app decides it for every group — consent and privacy are the app's; any rule
    such as "a team off means its sub-groups off" is the app's to apply when it
    writes each group; the gateway reads only the key's own group; captures go to the
    trace destination its operator configured, and mapping groups and keys to access
    there is the app's job (decisions 4, 6, 7).
- `control/sample`: the page's group tree shows each group's capture (a tag, as labels
  are shown); its tests.

## Decisions made during planning

- **No new API:** `capture` is a config field like any other; apps write it in the
  documents they publish.

## Acceptance criteria

- `npm test` and `npm run lint` green in `control/`; the schema copy in sync.
- The guide's capture section states who decides and what the gateway reads.
- The sample page shows capture per group (a test asserts it).
- The cross-half e2e is red until step 3 (expected, recorded).
