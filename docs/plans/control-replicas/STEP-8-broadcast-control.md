# Step 8 — broadcast-only kaiak-control

**Status:** not started

## Intent

Implement step 7's contract in `kaiak-control` and the sample, removing every
control-side item on the removal checklist. This is a removal step: nothing removed
survives under another name, and tests that assert removed behaviour are deleted.

## Files likely touched

- `src/config-versions/`: becomes the current config.
  - Publish: validate, apply the parents rule against the current config, then a
    conditional replace; retry when another publish won.
  - Delivery: on a change notification, read the current config and send it on each
    stream if it is newer by the store's sequence than what that stream last got
    (per-stream ordering).
  - No history, no `configsSince`, no resync.
  - Rename the module only if its name no longer says what it does. Moving code to
    keep an old concept alive is not allowed.
- `src/usage/`:
  - Totals without revision, epoch or config version.
  - Acks are `{ batch }` only.
  - Totals are pushed on the stream only, per stream, never older than the last one
    sent there.
- `src/fastify/`:
  - `GET /v1/config` removed.
  - `GET /v1/stream` takes no parameters: it sends the current config, then totals,
    then changes.
  - A store sequence going backwards (seen in a notification or a snapshot) closes
    every open stream on this core.
- `src/messages/`, `src/protocol/`, `src/index.ts`: message types and exports
  follow.
- Sample: no config versions in its page or logs; `KAIAK_SAMPLE_PROTOCOL_PORTS`
  unchanged.
- GUIDE:
  - the app/library split;
  - publish is replace;
  - the store interface;
  - the stream;
  - the Postgres sketch without history or epoch;
  - rollback handled by the core.
- Tests:
  - Delete the version, history, resume and resync tests.
  - Add:
    - a conditional replace racing another publish;
    - per-stream ordering with two reads racing;
    - a rollback closing streams;
    - a stream's first events being config then totals;
    - totals on the stream only, with the ack carrying none;
    - two cores converging on one current config.

## Acceptance criteria

- `npm test` and `npm run lint` from `control/` pass.
- Every control-side item on step 7's removal checklist greps clean in `control/`,
  recorded in Result with the grep commands and their empty output.
- The gateway and the cross-half tests may still fail (step 9). Name them.

## Result

(filled in when the step is done)
