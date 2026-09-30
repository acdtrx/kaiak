# Step 5 — a wrong path is the deployment's failure

**Status:** not started

## Intent

A backend whose `base_url` has the wrong path (the review's O1: no `/v1`) stops
failing silently: its "no such path" `404` is the deployment's failure, visible in
the circuit, retries and alerts, and the config-apply check warns about it.

## Files likely touched

- `docs/specs/GATEWAY.md`:
  - Wrong model on a host, or a new sibling decision (settled on the step's date):
    a wrong path is the deployment's failure; the per-module signatures; the
    catch-all for `openai-compatible`;
  - the error table (`upstream_path_missing`, `502`, `server_error`), the outcome
    and retry tables, and the metric label values;
  - the config-apply background check: a models list answering `404` is a warning
    naming `base_url`.
- `gateway/internal/provider/`: each module names its server's unknown-path answer
  beside its missing-model codes; `wire.go` classifies a matching `404` as the new
  error, as it does a missing model.
- `gateway/internal/routing` / `server`: the outcome class, retry reason and error
  code wiring, following `model_missing`.
- The background model check (wherever `model check skipped` is logged).
- `docs/DEPLOYMENT.md`: the starter alert that covers `model_missing` covers this too.

## Decisions made during planning

- **Signatures to confirm in the step**, each against the real server where at hand
  (llama-server locally, vLLM on the DGX via the live-test kit) and against the
  provider's documentation otherwise:
  - `vllm`: FastAPI's `{"detail": "Not Found"}`;
  - `llama-server`: its `not_found_error` answer;
  - `openai`: `invalid_request_error` whose message starts `Invalid URL`;
  - `azure-openai`: the resource's `404` "Resource not found";
  - `openai-compatible`: a `404` whose body is not an OpenAI-shaped error
    (`{"error": {...}}` or `{"error": "<text>"}`) — plain text included.
- A signature not recognized leaves the `404` the caller's, relayed as today.
- Metric label and retry reason: reuse the missing-model pattern with its own value
  (`path_missing`) rather than folding it into `model_missing`, so an operator can
  tell a wrong URL from a wrong model.

## Acceptance criteria

- Per module: its unknown-path answer gives `502 upstream_path_missing`, a retry on
  another deployment and a circuit failure, with no usage record; a missing-model
  `404` and an ordinary caller's `404` behave exactly as before.
- The config-apply check logs a warning naming the backend and `base_url` for a
  models list answering `404`; an unreachable backend is still an info line.
- **Phase end:** `scripts/check-all.sh` green, output recorded. Live (opt-in):
  `-kind llama-server` and `-kind vllm` on the DGX with a `base_url` missing `/v1`
  show the new code and the warning.

## Result

(to be filled when the step is done)
