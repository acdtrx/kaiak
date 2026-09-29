# Backend Verify

> The contract of `verifyBackend`, the `kaiak-control` helper (subsystem
> `src/backend-verify/`) that checks a backend and reads what it reports about its
> models, for the app to show and to turn into declared config. Decisions settled
> 2026-09-29 unless dated otherwise. Plan: `docs/plans/backend-verify/`.

## Purpose

- Model metadata is **declared** in config (`docs/specs/GATEWAY.md`, Model metadata):
  the gateway serves what config says and does no discovery. Declaring context
  lengths and capabilities for many models by hand is slow and error-prone.
- `verifyBackend` is what the app calls when an operator adds a backend or a model:
  1. does the backend answer at the URL config will use, and accept the credential;
  2. what does it report about its models;
  3. a report back to the app, which decides what goes into config and may show the
     report to its users.
- **Discovery lives in `kaiak-control`, not the gateway.** The gateway does no extra
  backend work; the values become declared config, so the config contract, the
  control protocol and the client API are unchanged. Rejected: gateway-side discovery
  (optional metadata filled from the gateway's model check) — it put `null`s into the
  client API and a store of discovered values into the gateway.

## What it never does

- Write config, add a backend or a model, or publish anything. It only reports.
- Keep state: no cache, no history, no log lines. Each call stands alone.
- Run unasked: no schedule, no call from the core or the Fastify plugin, no gateway
  endpoint. It runs when the app calls it.
- Retry. A failed call is re-run by the caller.
- Send anything but `GET`s, or follow a redirect (a redirect could carry the
  credential to another host; the gateway does not follow them either).
- Probe actively (settled 2026-09-29): no chat, completion or embedding request to
  observe a capability. vLLM 0.30.0 reads the same on `/v1/models`, `/tokenize`,
  `/version` and `/metrics` with vision on and off; only an image request told them
  apart. Active probes are a backlog entry.

## Input

`verifyBackend(options): Promise<BackendReport>`, with `options`:

| Option | Required | Meaning |
| --- | --- | --- |
| `type` | yes | `"openai-compatible"` or `"azure-openai"` — the config's backend `type`. |
| `baseUrl` | yes | The backend's `base_url`, exactly as config spells it. |
| `credential` | no | The credential **value** (not an env name), passed by the app. Omitted: nothing is sent. |
| `model` | no | A backend-side model name (a deployment's `model`) to check and describe. |
| `timeoutMs` | no | Bound on each request, connect to last byte. Default 5000. |
| `signal` | no | The caller's `AbortSignal`. |

- **The credential is a parameter** (settled 2026-09-29): the app passes the value it
  holds, so the helper works the same whether backend keys stay in gateway env or
  later move into config.
- **Input is checked first**, with the config schema's own rules (ajv, the package's
  copy of `protocol/schema/`): `type` from the backend `type` enum, `baseUrl` against
  the backend `base_url` pattern (so no userinfo, trailing slash, query or fragment),
  `model` against `backend_model_name`. `credential` is a non-empty string without
  CR, LF or NUL (a valid header value); `timeoutMs` a positive integer up to
  2147483647 (the platform's timer limit). Invalid input **throws**
  `{ code: "verify-input-invalid", message }` and sends nothing — it is the caller's
  error, not the backend's answer. The message never includes the credential.

## Requests

URLs and credential headers mirror the gateway's (`docs/specs/GATEWAY.md`,
Providers → Base URLs, credentials), so a backend that verifies is reached the same
way by the gateway:

| Type | Models list | Credential header |
| --- | --- | --- |
| `openai-compatible` | `GET <baseUrl>/models` | `Authorization: Bearer <credential>` |
| `azure-openai` | `GET <baseUrl>/openai/v1/models` | `api-key: <credential>` |

- Every request sends `Accept: application/json`, `User-Agent: kaiak-control` and the
  credential header when a credential was given; nothing else of its own (Node's
  `fetch` adds its fixed defaults: `Host`, `Connection`, `Accept-Encoding`,
  `Accept-Language: *`, `Sec-Fetch-Mode`).
- **At most two requests, in order**: the models list, then `GET <root>/props` only
  when the list says llama-server (Server recognition) and `<root>` exists (below).
  Nothing runs in parallel.
- **`<root>`** is `baseUrl` with its trailing `/v1` removed. llama-server serves
  `/props` at its root only (b9917: `/v1/props` answers `404`). A `baseUrl` that does
  not end in `/v1` has no known root: `/props` is skipped and each model's `notes`
  says so — no guessed path.
- `/props` carries the same credential header: a llama-server started with
  `--api-key` guards it.

## Server recognition

From the models list's `data[*].owned_by`, openai-compatible only:

| Every entry's `owned_by` | `server` |
| --- | --- |
| `"vllm"` | `"vllm"` |
| `"llamacpp"` | `"llama-server"` |
| anything else, mixed values, or an empty list | `"unknown"` |

An azure-openai backend reports `"unknown"`: its type already says what it is.

## Sources

Each value is read from where the server's answer puts it (CODING-RULES §4), never
inferred from names or other fields.

| Server | Value | Read from | Hint |
| --- | --- | --- | --- |
| vLLM | `context_length` | the model's list entry, `max_model_len` | no |
| llama-server | `context_length` | `/props` `default_generation_settings.n_ctx` | no |
| llama-server | `capabilities.vision` | `/props` `modalities.vision` | no |
| llama-server | `capabilities.tools` | `/props` `chat_template_caps.supports_tools` | yes |
| llama-server | `capabilities.reasoning` | `/props` `chat_template_caps.supports_reasoning_effort` | yes |
| unknown (OpenAI, SGLang, …) | — | reachability, credential and the listed ids only | — |
| azure-openai | — | reachability and credential only | — |

- **llama-server context comes from `/props`**, not the list's `meta.n_ctx`
  (settled 2026-09-29): the server splits its context across slots, and
  `default_generation_settings.n_ctx` is what one request gets. There is no fallback
  to `meta.n_ctx` when `/props` is skipped or lacks the field: the value is absent.
- **`/props` describes the one model a llama-server serves.** Its values are given to
  the list's entry only when the list has exactly one entry; with more (router mode,
  where `/props?model=` selects one), `/props` is not read and each model's `notes`
  says so. Router mode is a backlog entry.
- **Azure lists base models, not deployments** — the names requests carry are
  deployment names. So an azure-openai report has `models: []` and a top-level note,
  and `model-not-listed` never applies there: as in the gateway's model check, every
  name counts as served.
- A `/props` that fails (no answer, timeout, non-`2xx`, over the read cap, not JSON)
  is a note on each model, not a failure: the backend answered and accepted the
  credential. Its values are then absent.

## Hints

- A **hint** is a value the server derives from something other than the model's
  real ability, so it can be wrong. `sources` marks each value `hint: true` or
  `false`.
- `capabilities.tools` from `chat_template_caps.supports_tools` is a hint: it says
  the chat template renders tools, and an embedding model reports it `true` (b9917).
- `capabilities.reasoning` from `chat_template_caps.supports_reasoning_effort` is a
  hint: it says the template takes a reasoning effort. Only this structured field is
  read — the template text is not scanned (reasoning-effort heuristics are a backlog
  entry), and no `reasoning_efforts` are reported: no backend lists them.
- b9917 emits `supports_reasoning_effort` only when it is `true` (absent on the
  embedding model). Absent stays absent (next section); `false` is reported as
  `false` if a server sends it.

## Absent, never guessed

- A value the backend does not report is **absent** from the report — not `null`,
  not a default (settled 2026-09-29; CODING-RULES §4: visible-and-absent over
  silent-and-plausible). No server reports `streaming`; it is always absent.
- A field present with the wrong type or out of range (a `max_model_len` or `n_ctx`
  that is not a positive integer, a capability that is not a boolean) is treated as
  absent, with a note naming the field.
- A model whose value is missing gets a note saying where it was looked for.

## Report

Plain JSON-serializable data: the app can store it or show it as it is.

```ts
type BackendReport = {
  ok: boolean;
  failure?: { code: VerifyFailureCode; message: string }; // present iff ok is false
  server: "vllm" | "llama-server" | "unknown";
  models: VerifiedModel[];      // every listed entry, in list order; [] before a list was read
  metadata?: MetadataFragment;  // present iff ok and a model was named
  notes: string[];              // about the backend as a whole
};

type VerifiedModel = {
  id: string;                   // the entry's id, as listed
  context_length?: number;
  capabilities?: { vision?: boolean; tools?: boolean; reasoning?: boolean };
  sources: {                    // mirrors the values present above, one Source each
    context_length?: Source;
    capabilities?: { vision?: Source; tools?: Source; reasoning?: Source };
  };
  notes: string[];              // skipped reads, missing or malformed fields
};

type Source = {
  url: string;                  // the URL read (never carries a credential)
  field: string;                // dotted path in that answer, e.g. "default_generation_settings.n_ctx"
  hint: boolean;
};

type MetadataFragment = {
  context_length?: number;
  capabilities?: { vision?: boolean };
};
```

- `capabilities` on a model is present only when at least one capability was found;
  the same for `sources.capabilities` and the fragment's `capabilities`.
- `notes` are for people: their wording is not a contract. Codes are.

### Failure codes

Only the models-list request decides `ok`. The first failure ends the call.

| Code | When |
| --- | --- |
| `unreachable` | No HTTP answer: DNS, connection refused or reset, TLS failure. |
| `timeout` | The models list did not arrive whole within `timeoutMs`. |
| `credential-refused` | The models list answered `401` or `403` (the message says whether a credential was sent). |
| `not-a-models-list` | Any other answer that is not a `2xx` JSON body of at most 1 MiB matching the models-list schema: another status (a `3xx` included — not followed), a body that is not JSON, over the cap, or without a `data` array of entries with a string `id`. The message names the status or what was wrong. |
| `model-not-listed` | openai-compatible only: `model` was given and no entry's `id` equals it whole (the gateway's model check matches the same way). `models` still lists what the list said; `/props` is not read. |

- **Abort is not a failure**: when the caller's `signal` aborts, the call rejects
  with the signal's reason, as platform APIs do.
- **Messages** may name the URL (the input rules keep userinfo out of it) and the
  HTTP status. They never contain the credential, and never the backend's body text
  (a server can echo a refused key back in its error, OpenAI partially does).

### `metadata` and the config

- `metadata` is a **partial** config `metadata` object for the requested model
  (`protocol/schema/config.schema.json`: `models.<name>.metadata`, which requires
  `context_length` and `capabilities {streaming, tools, vision, reasoning}`). It holds
  only reported values under their config names, so it can be merged into the model's
  metadata as it is; the app completes the rest (`streaming` always, the
  capabilities not reported, `reasoning_efforts`) and publishing validates the whole.
  `{}` when nothing was reported (OpenAI, Azure, a vLLM entry without
  `max_model_len`).
- **Hints stay out of `metadata`**; they are in `models[]`, marked in `sources`
  (settled 2026-09-29). A value missing from the fragment fails config validation
  until someone decides it; a pasted hint would not (the embedding model's
  `tools: true`). An app that wants to prefill a form from a hint reads it from
  `models[]`, where it is labelled.

### Example

llama-server b9917, one model, `baseUrl` `http://dgx.local:11434/v1`, `model`
`unsloth/Qwen3.8-27B-GGUF:UD-Q4_K_XL` (the id it lists), abridged:

```json
{
  "ok": true,
  "server": "llama-server",
  "models": [{
    "id": "unsloth/Qwen3.8-27B-GGUF:UD-Q4_K_XL",
    "context_length": 262144,
    "capabilities": { "vision": true, "tools": true, "reasoning": true },
    "sources": {
      "context_length": { "url": "http://dgx.local:11434/props", "field": "default_generation_settings.n_ctx", "hint": false },
      "capabilities": {
        "vision": { "url": "http://dgx.local:11434/props", "field": "modalities.vision", "hint": false },
        "tools": { "url": "http://dgx.local:11434/props", "field": "chat_template_caps.supports_tools", "hint": true },
        "reasoning": { "url": "http://dgx.local:11434/props", "field": "chat_template_caps.supports_reasoning_effort", "hint": true }
      }
    },
    "notes": []
  }],
  "metadata": { "context_length": 262144, "capabilities": { "vision": true } },
  "notes": []
}
```

## Validation

- Backend answers are validated with ajv, `kaiak-control`'s one validation mechanism
  (`docs/TECH-STACK.md`), against **lenient** schemas of the helper's own (not part
  of `protocol/`: they describe other servers, not the kaiak contract):
  - the models list: an object with `data`, an array of objects each with a string
    `id`; everything else optional, unknown fields allowed;
  - `/props`: an object; each field read is typed on its own, so one malformed field
    makes only that value absent.
- A list that fails its schema is `not-a-models-list`; a `/props` that is not an
  object is a note.

## Bounds

- `timeoutMs` bounds each request, from connect to the last byte read; the caller's
  `signal` aborts whatever is in flight.
- Each body is read up to **1 MiB** (the gateway's probe reads the same); a longer
  body is not read further and counts as not-a-models-list (the list) or a failed
  `/props` (a note).

## Trust

- **Admin-only** (settled 2026-09-29). The helper fetches a URL its caller supplies
  and sends a credential there; the app exposes it only to people allowed to
  configure backends (`control/kaiak-control/GUIDE.md`).
- **Private addresses are not blocked** — a deliberate exception to CODING-RULES §9
  (SSRF): backends live on private networks, so blocking them would block the
  helper's purpose. The gate is who may call it, not where it may go.
- What an admin learns from a report is what the backend reports plus status codes;
  no response body text is relayed (Failure codes).
