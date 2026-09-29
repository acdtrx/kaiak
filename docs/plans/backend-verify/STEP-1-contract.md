# Step 1 — contract

**Status:** done (2026-09-29)

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

- **`docs/specs/BACKEND-VERIFY.md`** (new): purpose; what it never does; input
  (`type`, `baseUrl`, `credential`, `model`, `timeoutMs`, `signal`); requests (URLs and
  credential headers as the gateway builds them); server recognition from `owned_by`;
  sources per server; hints; absent-never-guessed; the report shape and failure codes;
  `metadata` vs the config schema; an example from the live llama-server answer;
  validation (ajv, lenient); bounds; trust.
- **`AGENTS.md`**: the rule is now "Only named code talks to backends": provider
  packages in the gateway, `kaiak-control`'s `backend-verify` in `control/`, only when
  the app calls it. Retitled because the old title named provider packages only.
- **`docs/BACKLOG.md`**: the reasoning-effort entry points to the spec instead of the
  plan; the llama-server entry's "reads its metadata already" said more than the
  helper reads (it reads `/props`, not `total_slots`); new entry "More from
  llama-server in `verifyBackend`" (router mode, default sampling parameters) — the
  OVERVIEW says every out-of-scope item is a backlog entry and these two were missing.
- **`docs/ARCHITECTURE.md`**: unchanged. Its "only code that talks to backends" claim
  sits in the Gateway packages section (true as scoped), and the data-flow line is
  about the request path, which the helper is not on. The new subsystem, its place in
  the `kaiak-control` graph and a control-plane → backends arrow belong with step 2,
  when the shape changes — step 2's file list does not name `ARCHITECTURE.md`; add it.
- `GATEWAY.md` Model metadata and `docs/kaiak.md` (lines 83, 128) still say discovery
  is deferred; they stay for step 3 as planned (still true of the gateway until the
  helper exists).

Decisions made here (dated 2026-09-29 in the spec, for review):

- **Hints stay out of `metadata`**, in `models[]` only, marked in `sources`: a value
  missing from the fragment fails config validation until someone decides it; a
  pasted hint would not (the embedding model's `tools: true`).
- **Input is checked with the config schema's own rules** (`type`, `base_url`,
  `backend_model_name`) and invalid input throws `verify-input-invalid` — the
  caller's error, not a report failure. Keeps userinfo out of every URL a message
  names.
- **Failure codes** as planned. Only the models list decides `ok`; `/props` problems
  are notes. Non-`2xx` other than `401`/`403` (and `3xx`, not followed) is
  `not-a-models-list`. Abort rejects with the signal's reason (not a failure code).
- **`/props` only for a single-model list**: with several entries (router mode) its
  values cannot be attributed, so it is skipped with a note.
- **Server recognition** needs every entry's `owned_by` to agree; azure-openai is
  `unknown` and reports `models: []` (Azure lists base models, not deployments).
- **`timeoutMs` bounds each request** (default 5000), connect to last byte.
- **Messages never carry the backend's body text** — a server can echo a refused key
  (OpenAI does, partly).
- **Private addresses not blocked**: recorded as a deliberate exception to
  CODING-RULES §9 (SSRF), gated by admin-only access.
- `reasoning`: absent `supports_reasoning_effort` stays absent (b9917 emits it only
  when true); `streaming` is never reported.

Suite: not run — docs only, no code changed.
