# Step 9 — end to end

**Status:** not started

## Intent

Prove the feature through the whole gateway and across both halves.

## Files likely touched

- `gateway/e2e/capture_test.go` (new), the gateway against the fake backend and
  `fakeotlp` receiving traces:
  - each endpoint, streamed and not: one span per request, its attributes and content
    as the spec's examples show;
  - a capturing and a non-capturing group side by side; a child group capturing under
    a parent that does not, and the reverse;
  - refusals after the stage (model access, limits, queue): spans with `error.type`
    and no output; a refusal before it (bad JSON): no span;
  - a client that leaves mid-stream: the partial output, `kaiak.relay_end`;
  - an upstream error answered `upstream_error`;
  - the session header: kept; malformed — the request served, the span without a
    session, `kaiak.session_id.ignored` on the span and the line; absent;
  - the budget: a tiny `KAIAK_CAPTURE_MEMORY_BYTES` drops captures and the requests
    still succeed; the counts and `kaiak.capture` say why;
  - traces off while the group captures: the warning, `export_off`, no span;
  - the drain: captures of requests finished before SIGTERM reach the collector;
  - the backend receives the same request bytes with capture on and off.
- `gateway/e2e/` cross-half (`crosshalf` tag): the sample control plane serves a config
  whose group captures; a gateway's capture follows a publish that turns it off.
- `gateway/internal/fakebackend/` if a shape the tests need is missing (reasoning
  deltas, tool-call fragments, Messages thinking, Responses items).

## Acceptance criteria

- The tests above pass; `scripts/check-all.sh` green (phase 3 ends here).
