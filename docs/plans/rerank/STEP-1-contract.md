# Step 1 — contract

**Status:** not started

## Intent

Settle what the plan builds in the documents that own it, before any code:
- the rerank endpoint, its cap and the new config field;
- versions, schema and fixtures;
- the wrong-endpoint rule.

Record the server behaviour each rule rests on: sources read, with versions and
dates.

## Files likely touched

- `docs/specs/GATEWAY.md`, each section dated 2026-10-08:
  - **endpoint list, client APIs and formats:** the rerank format, its owned fields
    and the fields that pass untouched (OVERVIEW decisions 9, 10);
  - **endpoint support table:** a `rerank` column — `vllm` and `llama-server` yes,
    the rest no. Record the sources: vLLM 0.30.0 and main (0.31.0), and the
    llama.cpp build read;
  - **base URLs and the path list:** `rerank`;
  - **error table:** the documents cap;
  - **batch caps:** `max_rerank_documents` (decision 3);
  - **input estimate:** the query repeated per document (decision 5);
  - **usage:** rerank counts `prompt_tokens` only (decision 11);
  - **relaying:** the answer is not translated (decision 12);
  - **observability:**
    - the `http.route` list;
    - `gen_ai.operation.name` `rerank`, with the amendment to the well-known-only
      rule (decision 6);
    - the usage-label cardinality: at most 4 operations;
  - **the wrong-endpoint rules** (decision 14):
    - "Wrong path to a host" and "An endpoint missing from a server": `vllm` has no
      core endpoints; llama-server's `501`s, each one decided;
    - the outcome-class table;
    - the endpoint memory per deployment;
    - why: vLLM creates routes from its model's tasks.
- `docs/specs/CONTROL-PROTOCOL.md`: config format 6 and protocol 6, and why.
- `docs/kaiak.md`: v1 scope gains `/v1/rerank`.
- `protocol/schema/config.schema.json`: `global.max_rerank_documents`, with the same
  bounds as `max_embedding_inputs`, default 1000; `format_version` 6.
- `protocol/fixtures/`:
  - the config fixtures that carry `max_embedding_inputs` gain the new field
    (`valid/full.json`, `valid/at-bounds.json`, `resolved/full.json`, the
    config-event `valid/full.json`);
  - a new invalid case, `max-rerank-documents-zero`, with its entry in `cases.json`;
  - every fixture moves to format 6.
- `docs/BACKLOG.md`: an entry for `/v1/models` `endpoints` per loaded model, not per
  type. Revisit trigger: a client relies on `endpoints` to pick a model and is misled
  by a vLLM or llama-server model listing an endpoint its server does not serve.

## Decisions made during planning

- **Read the sources, not memory.** vLLM's rerank request and answer
  (`vllm/entrypoints/pooling/scoring/protocol.py`), its route setup
  (`launchers/api_server/routers.py`, `pooling/factories.py`), and llama-server's
  rerank handler, response format and error types (`tools/server/`).
  - Record the version or commit of each, as step 1 of the Messages plan did.
- **llama-server's `501`s:** list every one the server answers on an endpoint kaiak
  serves, and decide each:
  - endpoint missing — the server is not in the endpoint's mode;
  - the caller's — the request asks for something the model lacks, such as audio;
    relayed, not retried, neutral;
  - or left as a backend failure.

  The audio case is not a missing endpoint: remembering it would keep chat off a
  healthy backend.
- **Endpoint memory per deployment:**
  - Confirm that llama-server's router mode serves several models behind one
    `base_url`, each with its own flags.
  - Confirm that a newer server that gains an endpoint is still found again after
    the interval.

  If per deployment turns out wrong, say why here and keep per backend.

## Acceptance criteria

- Every rule above is in its owning doc, dated, with what it rests on.
- Schema and fixtures at format 6, with the new field and the invalid case.
- `scripts/check-all.sh` run and recorded. Expected reds:
  - both halves' fixture and version tests, cleared by step 2 (`kaiak-control`) and
    step 3 (gateway);
  - the cross-half e2e, cleared by step 3.

## Result

(filled in when the step is done)
