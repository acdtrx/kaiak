# Step 1 — contract

**Status:** not started

## Intent

Settle what `verifyBackend` promises before building it, and amend the rule that only
the gateway's providers talk to backends.

## Files likely touched

- `docs/specs/BACKEND-VERIFY.md` (new):
  - purpose;
  - what the helper never does (write config, keep state, run unasked);
  - the input and report shapes;
  - the failure codes;
  - per-server sources (OVERVIEW Scope);
  - the hint labelling;
  - bounds (timeout, abort, bytes);
  - trust (admin-only).
  - Decisions dated 2026-09-29.
- `AGENTS.md`, Project-Specific Rules:
  - "Only provider packages talk to backends" becomes: in the gateway, only provider
    packages; in `control/`, only `kaiak-control`'s `backend-verify`, and only when
    the app calls it.
  - Project Facts' command line gets the sample's `verify` command when step 3 lands.
- `docs/ARCHITECTURE.md`: a line or an arrow if its diagrams claim that only the
  gateway reaches backends.
- `docs/BACKLOG.md` is already updated at planning time: credentials in config,
  gateway drift warning, reasoning heuristics. Verify it's consistent.

## Decisions made during planning

- **Report shape** (the spec fixes the exact names):
  - `ok`;
  - `failure?: { code, message }` with codes `unreachable`, `timeout`,
    `credential-refused`, `not-a-models-list`, `model-not-listed`;
  - `server: "vllm" | "llama-server" | "unknown"`;
  - `models: [{ id, context_length?, capabilities?: { vision?, tools?, reasoning? },
    sources, notes }]`;
  - `metadata?`: a config `metadata` block for the requested model, holding only what
    was found. Absent when no model was named.
- **Messages** may name the URL, never the credential.
- The report is plain JSON-serializable data, so the app can store or show it as it is.

## Acceptance criteria

- The spec reads as a contract a second implementation could follow. The user has
  reviewed it.
- No code changes. Suite not required (docs only); state that in the result.

## Result

(not started)
