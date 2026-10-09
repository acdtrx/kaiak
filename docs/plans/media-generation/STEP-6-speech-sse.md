# Step 6 — speech: the SSE stream

**Status:** not started

## Intent

Serve speech's SSE mode (`stream: true` or `stream_format: "sse"`):
- `speech.audio.delta` events relay, their base64 audio counted toward
  `audio_seconds_out`;
- `speech.audio.done` carries the token usage and completes the stream;
- `speech.audio.error` is classified as step 1 settled.

## Files likely touched

- `gateway/internal/provider/stream_format.go`: a speech stream format —
  - completeness at `speech.audio.done`;
  - the error event's classification;
  - no request edits beyond the model and the output-limit default.
- `gateway/internal/accounting/`:
  - the speech reader's stream events: usage from `done`, the delta's audio length
    from its base64 size and the format's sample rate;
  - a WAV header inside the first delta read as in step 5.
- `gateway/internal/server/inbound.go`: SSE accepted.
- The fake backend: the SSE mode (delta, done, error).
- Tests beside each.

## Decisions made during planning

- **The base64 is never decoded whole.** The audio length is the decoded size of each
  delta, computed from its encoded length and padding. Only a leading WAV header is
  decoded.

## Acceptance criteria

- **Component tests:**
  - completeness;
  - a stream broken before `done` (partial: the seconds sent, tokens estimated);
  - the error event (backend failure versus the caller's);
  - usage from `done`;
  - the delta length (PCM, WAV);
  - the timers (first event, stall).
- `scripts/check-all.sh` run and recorded. Expected red: none.

## Result

(filled in when the step is done)
