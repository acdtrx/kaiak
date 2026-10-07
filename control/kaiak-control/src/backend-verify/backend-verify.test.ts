import assert from "node:assert/strict";
import { createServer } from "node:http";
import type { IncomingHttpHeaders, IncomingMessage, ServerResponse } from "node:http";
import type { AddressInfo } from "node:net";
import path from "node:path";
import { test } from "node:test";
import type { TestContext } from "node:test";

import { BACKEND_TYPES } from "../config/index.ts";
import type { BackendType } from "../config/index.ts";
import { fixtureFiles, fixturePath, readJson } from "../test-support/index.ts";
import { verifyBackend } from "./index.ts";
import type { BackendReport, VerifyBackendOptions } from "./index.ts";

// Bodies trimmed from live answers captured on 2026-09-29.
const LLAMA_MODEL = "unsloth/Qwen3.8-27B-GGUF:UD-Q4_K_XL";
const LLAMA_LIST = {
  models: [{ name: LLAMA_MODEL, model: LLAMA_MODEL }],
  object: "list",
  data: [
    {
      id: LLAMA_MODEL,
      aliases: [LLAMA_MODEL],
      tags: [],
      object: "model",
      created: 1790673261,
      owned_by: "llamacpp",
      meta: { vocab_type: true, n_vocab: 248320, n_ctx: 262144, n_ctx_train: 262144, n_embd: 5120, n_params: 27320697856 },
    },
  ],
};
const LLAMA_PROPS = {
  default_generation_settings: { n_ctx: 262144, params: { temperature: 0.8 } },
  total_slots: 1,
  model_alias: LLAMA_MODEL,
  modalities: { vision: true, video: true, audio: false },
  chat_template_caps: {
    supports_object_arguments: true,
    supports_parallel_tool_calls: true,
    supports_preserve_reasoning: true,
    supports_reasoning_effort: true,
    supports_string_content: true,
    supports_system_role: true,
    supports_tool_calls: true,
    supports_tools: true,
    supports_typed_content: false,
  },
  build_info: "b9917-4a7ee3126",
};

const EMBED_MODEL = "/models/qwen3-embedding-0.6b-q8_0.gguf";
const EMBED_LIST = {
  object: "list",
  data: [{ id: EMBED_MODEL, aliases: [EMBED_MODEL], object: "model", owned_by: "llamacpp", meta: { n_ctx: 131072 } }],
};
const EMBED_PROPS = {
  default_generation_settings: { n_ctx: 32768 },
  total_slots: 4,
  modalities: { vision: false, video: false, audio: false },
  chat_template_caps: { supports_system_role: true, supports_tool_calls: true, supports_tools: true },
  build_info: "b9917-4a7ee3126",
};

const VLLM_MODEL = "unsloth/Qwen3.8-27B-NVFP4";
const VLLM_LIST = {
  object: "list",
  data: [
    {
      id: VLLM_MODEL,
      object: "model",
      created: 1790677211,
      owned_by: "vllm",
      root: VLLM_MODEL,
      parent: null,
      max_model_len: 262144,
      permission: [{ id: "modelperm-838d140f73c93323", object: "model_permission" }],
    },
  ],
};

const OPENAI_LIST = {
  object: "list",
  data: [
    { id: "gpt-5.5", object: "model", created: 1790000000, owned_by: "openai" },
    { id: "gpt-5.5-mini", object: "model", created: 1790000000, owned_by: "system" },
  ],
};

const AZURE_LIST = {
  object: "list",
  data: [{ id: "gpt-5.5", object: "model", created: 1790000000, owned_by: "system", capabilities: {} }],
};

// Anthropic's Models API, from its documentation (not captured live): a page of
// entries with max_input_tokens, one without it.
const ANTHROPIC_LIST = {
  data: [
    {
      type: "model",
      id: "claude-sonnet-5-5",
      display_name: "Claude Sonnet 5.5",
      created_at: "2026-08-01T00:00:00Z",
      max_input_tokens: 1000000,
      max_tokens: 128000,
    },
    { type: "model", id: "claude-haiku-4-5", display_name: "Claude Haiku 4.5", created_at: "2025-10-01T00:00:00Z" },
  ],
  has_more: false,
  first_id: "claude-sonnet-5-5",
  last_id: "claude-haiku-4-5",
};
const ANTHROPIC_AUTH_ERROR = { type: "error", error: { type: "authentication_error", message: "invalid x-api-key" } };
const CREDENTIAL = "sk-kaiak-test-SECRET-4f1c9e";

interface SeenRequest {
  method: string;
  path: string;
  headers: IncomingHttpHeaders;
}

type Route = (req: IncomingMessage, res: ServerResponse) => void;

interface FakeBackend {
  // Base URL of the fake: http://127.0.0.1:<port>
  origin: string;
  requests: SeenRequest[];
}

// An in-process backend on 127.0.0.1 answering by path; unknown paths answer 404.
// Closed when the test ends.
async function fakeBackend(t: TestContext, routes: Record<string, Route>): Promise<FakeBackend> {
  const requests: SeenRequest[] = [];
  const server = createServer((req, res) => {
    const path = req.url ?? "";
    requests.push({ method: req.method ?? "", path, headers: req.headers });
    const route = routes[path];
    if (route) route(req, res);
    else json(404, { error: { message: "File Not Found", type: "not_found_error", code: 404 } })(req, res);
  });
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  t.after(() => {
    server.closeAllConnections();
    return new Promise<void>((resolve) => server.close(() => resolve()));
  });
  const { port } = server.address() as AddressInfo;
  return { origin: `http://127.0.0.1:${port}`, requests };
}

function json(status: number, body: unknown): Route {
  return (_req, res) => {
    res.writeHead(status, { "content-type": "application/json" });
    res.end(JSON.stringify(body));
  };
}

function text(status: number, body: string): Route {
  return (_req, res) => {
    res.writeHead(status, { "content-type": "text/plain" });
    res.end(body);
  };
}

// Never answers (the test's server close ends the connection).
const hang: Route = () => {};

function verify(options: Partial<VerifyBackendOptions> & { baseUrl: string }): Promise<BackendReport> {
  return verifyBackend({ type: "openai-compatible", ...options });
}

// The credential appears nowhere in what the helper hands back.
function assertNoCredential(value: unknown): void {
  const serialized = value instanceof Error ? `${value.message} ${JSON.stringify(value)}` : JSON.stringify(value);
  assert.ok(!serialized.includes(CREDENTIAL), `credential leaked: ${serialized}`);
  assert.ok(!serialized.includes("SECRET"), `credential part leaked: ${serialized}`);
}

test("llama-server: context, vision, tools and reasoning read from /props at the root", async (t) => {
  const backend = await fakeBackend(t, { "/v1/models": json(200, LLAMA_LIST), "/props": json(200, LLAMA_PROPS) });
  const report = await verify({ type: "llama-server", baseUrl: `${backend.origin}/v1`, model: LLAMA_MODEL });
  const props = `${backend.origin}/props`;
  assert.deepEqual(report, {
    ok: true,
    server: "llama-server",
    models: [
      {
        id: LLAMA_MODEL,
        context_length: 262144,
        capabilities: { vision: true, tools: true, reasoning: true },
        sources: {
          context_length: { url: props, field: "default_generation_settings.n_ctx", hint: false },
          capabilities: {
            vision: { url: props, field: "modalities.vision", hint: false },
            tools: { url: props, field: "chat_template_caps.supports_tools", hint: true },
            reasoning: { url: props, field: "chat_template_caps.supports_reasoning_effort", hint: true },
          },
        },
        notes: [],
      },
    ],
    metadata: { context_length: 262144, capabilities: { vision: true } },
    notes: [],
  });
  assert.deepEqual(
    backend.requests.map((request) => `${request.method} ${request.path}`),
    ["GET /v1/models", "GET /props"],
  );
});

test("llama-server embedding model: no reasoning hint reported, tools hint kept out of metadata", async (t) => {
  const backend = await fakeBackend(t, { "/v1/models": json(200, EMBED_LIST), "/props": json(200, EMBED_PROPS) });
  const report = await verify({ baseUrl: `${backend.origin}/v1`, model: EMBED_MODEL });
  assert.equal(report.ok, true);
  const [model] = report.models;
  assert.equal(model?.context_length, 32768, "context from /props, not meta.n_ctx");
  assert.deepEqual(model?.capabilities, { vision: false, tools: true });
  assert.equal(model?.sources.capabilities?.reasoning, undefined);
  assert.equal(model?.notes.length, 1);
  assert.match(model?.notes[0] ?? "", /reasoning not reported.*chat_template_caps\.supports_reasoning_effort/);
  assert.deepEqual(report.metadata, { context_length: 32768, capabilities: { vision: false } });
});

test("llama-server: /props fields absent or mistyped are left out with a note, never guessed", async (t) => {
  const props = {
    default_generation_settings: { n_ctx: "262144" },
    modalities: "vision",
    chat_template_caps: { supports_tools: 1, supports_reasoning_effort: false },
  };
  const backend = await fakeBackend(t, { "/v1/models": json(200, LLAMA_LIST), "/props": json(200, props) });
  const report = await verify({ baseUrl: `${backend.origin}/v1`, model: LLAMA_MODEL });
  assert.equal(report.ok, true);
  const [model] = report.models;
  assert.equal(model?.context_length, undefined, "no fallback to meta.n_ctx");
  assert.deepEqual(model?.capabilities, { reasoning: false }, "false is reported as false");
  assert.deepEqual(Object.keys(model?.sources.capabilities ?? {}), ["reasoning"]);
  assert.equal(model?.notes.length, 3);
  for (const field of ["default_generation_settings.n_ctx", "modalities.vision", "chat_template_caps.supports_tools"]) {
    assert.ok(model?.notes.some((note) => note.includes(field)), `a note names ${field}`);
  }
  assert.deepEqual(report.metadata, {});

  const empty = await fakeBackend(t, { "/v1/models": json(200, LLAMA_LIST), "/props": json(200, {}) });
  const bare = await verify({ baseUrl: `${empty.origin}/v1` });
  const [bareModel] = bare.models;
  assert.deepEqual(bareModel?.sources, {});
  assert.equal(bareModel?.capabilities, undefined);
  assert.equal(bareModel?.context_length, undefined);
  assert.equal(bareModel?.notes.length, 4);
  assert.ok(bareModel?.notes.every((note) => note.includes("not reported")));
  assert.equal(bare.metadata, undefined, "no model named, no metadata");
});

test("llama-server: a /props that fails is a note, not a failure", async (t) => {
  const cases: [string, Route][] = [
    ["404", json(404, { error: "not found" })],
    ["not JSON", text(200, "<html>props</html>")],
    ["an array", json(200, [LLAMA_PROPS])],
    ["over 1 MiB", text(200, "x".repeat(1024 * 1024 + 1))],
    ["timeout", hang],
  ];
  for (const [name, route] of cases) {
    const backend = await fakeBackend(t, { "/v1/models": json(200, LLAMA_LIST), "/props": route });
    const report = await verify({ baseUrl: `${backend.origin}/v1`, model: LLAMA_MODEL, timeoutMs: 200 });
    assert.equal(report.ok, true, name);
    assert.equal(report.server, "llama-server", name);
    const [model] = report.models;
    assert.equal(model?.context_length, undefined, name);
    assert.equal(model?.capabilities, undefined, name);
    assert.equal(model?.notes.length, 1, name);
    assert.match(model?.notes[0] ?? "", /\/props/, name);
    assert.deepEqual(report.metadata, {}, name);
  }
});

test("llama-server: a base URL without /v1 skips /props and says so", async (t) => {
  const backend = await fakeBackend(t, { "/api/models": json(200, LLAMA_LIST), "/props": json(200, LLAMA_PROPS) });
  const report = await verify({ baseUrl: `${backend.origin}/api`, model: LLAMA_MODEL });
  assert.equal(report.ok, true);
  assert.equal(report.server, "llama-server");
  const [model] = report.models;
  assert.equal(model?.context_length, undefined);
  assert.match(model?.notes[0] ?? "", /\/props skipped.*\/v1/);
  assert.deepEqual(report.metadata, {});
  assert.deepEqual(
    backend.requests.map((request) => request.path),
    ["/api/models"],
  );
});

test("llama-server listing several models: /props is not read, each model says so", async (t) => {
  const list = { object: "list", data: [...LLAMA_LIST.data, { ...EMBED_LIST.data[0] }] };
  const backend = await fakeBackend(t, { "/v1/models": json(200, list), "/props": json(200, LLAMA_PROPS) });
  const report = await verify({ baseUrl: `${backend.origin}/v1` });
  assert.equal(report.ok, true);
  assert.equal(report.server, "llama-server");
  assert.deepEqual(
    report.models.map((model) => model.id),
    [LLAMA_MODEL, EMBED_MODEL],
  );
  for (const model of report.models) {
    assert.equal(model.context_length, undefined);
    assert.match(model.notes[0] ?? "", /\/props skipped.*router mode/);
  }
  assert.equal(backend.requests.length, 1);
});

test("vLLM: context from max_model_len on the list; no /props request", async (t) => {
  const backend = await fakeBackend(t, { "/v1/models": json(200, VLLM_LIST) });
  const report = await verify({ type: "vllm", baseUrl: `${backend.origin}/v1`, model: VLLM_MODEL });
  assert.deepEqual(report, {
    ok: true,
    server: "vllm",
    models: [
      {
        id: VLLM_MODEL,
        context_length: 262144,
        sources: { context_length: { url: `${backend.origin}/v1/models`, field: "max_model_len", hint: false } },
        notes: [],
      },
    ],
    metadata: { context_length: 262144 },
    notes: [],
  });
  assert.equal(backend.requests.length, 1);
});

test("vLLM: an entry without a valid max_model_len has no context_length and a note", async (t) => {
  const list = {
    object: "list",
    data: [
      { id: "a", owned_by: "vllm" },
      { id: "b", owned_by: "vllm", max_model_len: 0 },
      { id: "c", owned_by: "vllm", max_model_len: 4096.5 },
    ],
  };
  const backend = await fakeBackend(t, { "/v1/models": json(200, list) });
  const report = await verify({ baseUrl: `${backend.origin}/v1`, model: "b" });
  assert.equal(report.server, "vllm");
  assert.deepEqual(report.metadata, {});
  for (const model of report.models) {
    assert.equal(model.context_length, undefined, model.id);
    assert.deepEqual(model.sources, {}, model.id);
    assert.equal(model.notes.length, 1, model.id);
    assert.match(model.notes[0] ?? "", /max_model_len/, model.id);
  }
});

test("OpenAI-style and mixed owned_by: server unknown, ids only, no /props request", async (t) => {
  const mixed = { object: "list", data: [...LLAMA_LIST.data, ...VLLM_LIST.data] };
  for (const list of [OPENAI_LIST, mixed, { object: "list", data: [] }]) {
    const backend = await fakeBackend(t, { "/v1/models": json(200, list), "/props": json(200, LLAMA_PROPS) });
    const report = await verify({ baseUrl: `${backend.origin}/v1` });
    assert.equal(report.ok, true);
    assert.equal(report.server, "unknown");
    assert.deepEqual(
      report.models,
      list.data.map((entry) => ({ id: entry.id, sources: {}, notes: [] })),
    );
    assert.equal(report.notes.length, 1);
    assert.deepEqual(
      backend.requests.map((request) => request.path),
      ["/v1/models"],
    );
  }
});

test("OpenAI-style with a listed model: ok with empty metadata; an unlisted one is model-not-listed", async (t) => {
  const backend = await fakeBackend(t, { "/v1/models": json(200, OPENAI_LIST) });
  const listed = await verify({ baseUrl: `${backend.origin}/v1`, model: "gpt-5.5", credential: CREDENTIAL });
  assert.equal(listed.ok, true);
  assert.deepEqual(listed.metadata, {});

  const unlisted = await verify({ baseUrl: `${backend.origin}/v1`, model: "gpt-5", credential: CREDENTIAL });
  assert.equal(unlisted.ok, false);
  assert.equal(unlisted.failure?.code, "model-not-listed");
  assert.equal(unlisted.metadata, undefined);
  assert.deepEqual(
    unlisted.models.map((model) => model.id),
    ["gpt-5.5", "gpt-5.5-mini"],
    "the models still list what the list said",
  );
});

test("model-not-listed matches the id whole and does not read /props", async (t) => {
  const backend = await fakeBackend(t, { "/v1/models": json(200, LLAMA_LIST), "/props": json(200, LLAMA_PROPS) });
  for (const model of ["unsloth/Qwen3.8-27B-GGUF", "Qwen3.8-27B-GGUF:UD-Q4_K_XL"]) {
    const report = await verify({ baseUrl: `${backend.origin}/v1`, model });
    assert.equal(report.failure?.code, "model-not-listed", model);
    assert.equal(report.server, "llama-server", model);
    assert.deepEqual(
      report.models.map((entry) => entry.id),
      [LLAMA_MODEL],
      model,
    );
  }
  assert.ok(backend.requests.every((request) => request.path === "/v1/models"));
});

test("vLLM model-not-listed still reports what the list said", async (t) => {
  const backend = await fakeBackend(t, { "/v1/models": json(200, VLLM_LIST) });
  const report = await verify({ baseUrl: `${backend.origin}/v1`, model: "other" });
  assert.equal(report.failure?.code, "model-not-listed");
  assert.equal(report.models[0]?.context_length, 262144);
});

test("azure-openai: the list at /openai/v1/models with api-key; no models, a note, any name served", async (t) => {
  const backend = await fakeBackend(t, { "/openai/v1/models": json(200, AZURE_LIST) });
  const report = await verifyBackend({
    type: "azure-openai",
    baseUrl: backend.origin,
    credential: CREDENTIAL,
    model: "my-deployment",
  });
  assert.equal(report.ok, true);
  assert.equal(report.server, "unknown");
  assert.deepEqual(report.models, []);
  assert.deepEqual(report.metadata, {});
  assert.equal(report.notes.length, 1);
  const [request] = backend.requests;
  assert.equal(backend.requests.length, 1);
  assert.equal(request?.path, "/openai/v1/models");
  assert.equal(request?.headers["api-key"], CREDENTIAL);
  assert.equal(request?.headers.authorization, undefined);
  assertNoCredential(report);

  const noModel = await verifyBackend({ type: "azure-openai", baseUrl: backend.origin, credential: CREDENTIAL });
  assert.equal(noModel.metadata, undefined);
});

test("anthropic: the paged list with x-api-key and anthropic-version; context from max_input_tokens", async (t) => {
  const listPath = "/v1/models?limit=1000";
  const backend = await fakeBackend(t, { [listPath]: json(200, ANTHROPIC_LIST) });
  const url = `${backend.origin}${listPath}`;
  const report = await verifyBackend({
    type: "anthropic",
    baseUrl: `${backend.origin}/v1`,
    credential: CREDENTIAL,
    model: "claude-sonnet-5-5",
  });
  assert.deepEqual(report, {
    ok: true,
    server: "unknown",
    models: [
      {
        id: "claude-sonnet-5-5",
        context_length: 1000000,
        sources: { context_length: { url, field: "max_input_tokens", hint: false } },
        notes: [],
      },
      {
        id: "claude-haiku-4-5",
        sources: {},
        notes: [`context_length not reported: ${url} has no max_input_tokens`],
      },
    ],
    metadata: { context_length: 1000000 },
    notes: [],
  });
  const [request] = backend.requests;
  assert.equal(backend.requests.length, 1);
  assert.equal(request?.headers["x-api-key"], CREDENTIAL);
  assert.equal(request?.headers["anthropic-version"], "2023-06-01");
  assert.equal(request?.headers.authorization, undefined);
  assertNoCredential(report);

  const missing = await verifyBackend({
    type: "anthropic",
    baseUrl: `${backend.origin}/v1`,
    credential: CREDENTIAL,
    model: "claude-nope",
  });
  assert.equal(missing.failure?.code, "model-not-listed");

  const refused = await fakeBackend(t, { [listPath]: json(401, ANTHROPIC_AUTH_ERROR) });
  const denied = await verifyBackend({ type: "anthropic", baseUrl: `${refused.origin}/v1`, credential: CREDENTIAL });
  assert.equal(denied.failure?.code, "credential-refused");
  assertNoCredential(denied);
});

test("azure-anthropic: no request is sent; not checkable, never reported as verified", async (t) => {
  const backend = await fakeBackend(t, {});
  const message = "Microsoft Foundry has no models list: the backend cannot be checked; its reachability and credential stay unchecked until the gateway's first request";
  for (const model of ["my-claude-deployment", undefined]) {
    const report = await verifyBackend({
      type: "azure-anthropic",
      baseUrl: backend.origin,
      credential: CREDENTIAL,
      ...(model === undefined ? {} : { model }),
    });
    assert.deepEqual(report, {
      ok: false,
      failure: { code: "not-checkable", message },
      server: "unknown",
      models: [],
      notes: [],
    });
    assertNoCredential(report);
  }
  assert.deepEqual(backend.requests, []);
});

test("each type reads its models list at its URL with its credential header", async (t) => {
  // azure-anthropic has no models list: it sends nothing (its own test below).
  const cases: [BackendType, string, string, string][] = [
    ["openai-compatible", "/v1", "/v1/models", "authorization"],
    ["openai", "/v1", "/v1/models", "authorization"],
    ["vllm", "/v1", "/v1/models", "authorization"],
    ["llama-server", "/v1", "/v1/models", "authorization"],
    ["azure-openai", "", "/openai/v1/models", "api-key"],
    ["anthropic", "/v1", "/v1/models?limit=1000", "x-api-key"],
  ];
  assert.deepEqual(
    [...cases.map(([type]) => type), "azure-anthropic"].sort(),
    [...BACKEND_TYPES].sort(),
    "every type is covered",
  );
  const credentialHeaders = ["authorization", "api-key", "x-api-key"];
  for (const [type, basePath, listPath, header] of cases) {
    const backend = await fakeBackend(t, { [listPath]: json(200, OPENAI_LIST) });
    const report = await verifyBackend({ type, baseUrl: `${backend.origin}${basePath}`, credential: CREDENTIAL });
    assert.equal(report.ok, true, type);
    assert.deepEqual(
      backend.requests.map((request) => request.path),
      [listPath],
      type,
    );
    const [request] = backend.requests;
    const expected = header === "authorization" ? `Bearer ${CREDENTIAL}` : CREDENTIAL;
    assert.equal(request?.headers[header], expected, `${type}: ${header}`);
    for (const other of credentialHeaders.filter((name) => name !== header)) {
      assert.equal(request?.headers[other], undefined, `${type}: no ${other}`);
    }
    assert.equal(request?.headers["anthropic-version"], type === "anthropic" ? "2023-06-01" : undefined, type);
    assertNoCredential(report);
  }
});

// A file of protocol/fixtures/backend-types/, named for its type: for a sample base_url
// and credential, the models-list request a backend of that type gets — its URL, and
// the headers it carries besides each half's own Accept and User-Agent — or null when
// the type has no models list. The gateway's provider tests read the same files.
interface BackendTypeFixture {
  base_url: string;
  credential: string;
  models_list: { url: string; headers: Record<string, string> } | null;
}

test("each type's models-list request is the gateway's (protocol/fixtures/backend-types)", async (t) => {
  const dir = fixturePath("backend-types");
  const types = fixtureFiles(dir).map((file) => path.basename(file, ".json"));
  assert.deepEqual([...types].sort(), [...BACKEND_TYPES].sort(), "one fixture per type");
  for (const type of types) {
    const fixture = readJson(path.join(dir, `${type}.json`)) as BackendTypeFixture;
    const sent: { url: string; method: string | undefined; headers: Headers }[] = [];
    const fetchMock = t.mock.method(globalThis, "fetch", async (input: string | URL | Request, init?: RequestInit) => {
      sent.push({ url: String(input), method: init?.method, headers: new Headers(init?.headers) });
      return new Response(JSON.stringify({ data: [] }), { status: 200, headers: { "content-type": "application/json" } });
    });
    const report = await verifyBackend({
      type: type as BackendType,
      baseUrl: fixture.base_url,
      credential: fixture.credential,
    });
    fetchMock.mock.restore();

    assert.equal(report.ok, fixture.models_list !== null, `${type}: ok only with a models list`);
    if (fixture.models_list === null) {
      assert.deepEqual(sent, [], `${type}: no request`);
      continue;
    }
    assert.equal(sent.length, 1, `${type}: one request`);
    const [request] = sent;
    assert.equal(request?.method, "GET", type);
    assert.equal(request?.url, fixture.models_list.url, type);
    const headers = Object.fromEntries(
      [...(request?.headers ?? new Headers())].filter(([name]) => name !== "accept" && name !== "user-agent"),
    );
    const expected = Object.fromEntries(
      Object.entries(fixture.models_list.headers).map(([name, value]) => [name.toLowerCase(), value]),
    );
    assert.deepEqual(headers, expected, `${type}: headers`);
  }
});

test("a recognized server under another type gets a note naming its type", async (t) => {
  const vllm = await fakeBackend(t, { "/v1/models": json(200, VLLM_LIST) });
  for (const type of ["openai-compatible", "openai", "llama-server"] as const) {
    const report = await verify({ type, baseUrl: `${vllm.origin}/v1`, model: VLLM_MODEL });
    assert.equal(report.ok, true, type);
    assert.equal(report.server, "vllm", type);
    assert.deepEqual(report.notes, [`the models list says the server is vllm: use type "vllm", not "${type}"`], type);
    assert.deepEqual(report.metadata, { context_length: 262144 }, `${type}: read as vLLM all the same`);
  }

  const llama = await fakeBackend(t, { "/v1/models": json(200, LLAMA_LIST), "/props": json(200, LLAMA_PROPS) });
  const report = await verify({ type: "vllm", baseUrl: `${llama.origin}/v1`, model: LLAMA_MODEL });
  assert.equal(report.server, "llama-server");
  assert.deepEqual(report.notes, ['the models list says the server is llama-server: use type "llama-server", not "vllm"']);
  assert.equal(report.models[0]?.context_length, 262144, "/props read all the same");

  const unlisted = await verify({ type: "openai-compatible", baseUrl: `${llama.origin}/v1`, model: "other" });
  assert.equal(unlisted.failure?.code, "model-not-listed");
  assert.deepEqual(unlisted.notes, [
    'the models list says the server is llama-server: use type "llama-server", not "openai-compatible"',
  ]);
});

test("no type note when the type matches the server, or the server is unknown", async (t) => {
  const vllm = await fakeBackend(t, { "/v1/models": json(200, VLLM_LIST) });
  assert.deepEqual((await verify({ type: "vllm", baseUrl: `${vllm.origin}/v1` })).notes, []);

  const llama = await fakeBackend(t, { "/v1/models": json(200, LLAMA_LIST), "/props": json(200, LLAMA_PROPS) });
  assert.deepEqual((await verify({ type: "llama-server", baseUrl: `${llama.origin}/v1` })).notes, []);

  const openai = await fakeBackend(t, { "/v1/models": json(200, OPENAI_LIST) });
  for (const type of ["openai-compatible", "openai", "vllm", "llama-server"] as const) {
    const report = await verify({ type, baseUrl: `${openai.origin}/v1` });
    assert.equal(report.server, "unknown", type);
    assert.ok(report.notes.every((note) => !note.includes("use type")), `${type}: ${report.notes.join("; ")}`);
  }
});

test("headers: Accept, User-Agent and the credential header as the gateway sends it", async (t) => {
  const backend = await fakeBackend(t, { "/v1/models": json(200, LLAMA_LIST), "/props": json(200, LLAMA_PROPS) });
  await verify({ baseUrl: `${backend.origin}/v1`, credential: CREDENTIAL });
  assert.equal(backend.requests.length, 2);
  for (const request of backend.requests) {
    assert.equal(request.method, "GET");
    assert.equal(request.headers.accept, "application/json");
    assert.equal(request.headers["user-agent"], "kaiak-control");
    assert.equal(request.headers.authorization, `Bearer ${CREDENTIAL}`, `${request.path} carries the credential`);
    assert.equal(request.headers["api-key"], undefined);
  }

  const bare = await fakeBackend(t, { "/v1/models": json(200, LLAMA_LIST), "/props": json(200, LLAMA_PROPS) });
  await verify({ baseUrl: `${bare.origin}/v1` });
  for (const request of bare.requests) {
    assert.equal(request.headers.authorization, undefined);
    assert.equal(request.headers["api-key"], undefined);
    assert.equal(request.headers["user-agent"], "kaiak-control");
  }
});

test("credential-refused on 401 and 403; the message says whether a credential was sent", async (t) => {
  for (const status of [401, 403]) {
    const backend = await fakeBackend(t, {
      "/v1/models": json(status, { error: { message: `Incorrect API key provided: ${CREDENTIAL}` } }),
    });
    const sent = await verify({ baseUrl: `${backend.origin}/v1`, credential: CREDENTIAL });
    assert.deepEqual({ ok: sent.ok, code: sent.failure?.code, server: sent.server, models: sent.models, notes: sent.notes }, {
      ok: false,
      code: "credential-refused",
      server: "unknown",
      models: [],
      notes: [],
    });
    assert.match(sent.failure?.message ?? "", new RegExp(`${status}.*credential was refused`));
    assertNoCredential(sent);

    const none = await verify({ baseUrl: `${backend.origin}/v1` });
    assert.equal(none.failure?.code, "credential-refused");
    assert.match(none.failure?.message ?? "", /no credential was sent/);
  }
});

test("not-a-models-list: redirects, other statuses, non-JSON, over the cap, wrong shape", async (t) => {
  const redirect: Route = (_req, res) => {
    res.writeHead(302, { location: "http://127.0.0.2:1/steal" });
    res.end();
  };
  const cases: [string, Route, RegExp][] = [
    ["302", redirect, /302, a redirect \(not followed\)/],
    ["500", text(500, `internal error for key ${CREDENTIAL}`), /answered 500$/],
    ["404", json(404, {}), /answered 404$/],
    ["non-JSON", text(200, `<html>${CREDENTIAL}</html>`), /not JSON/],
    ["over 1 MiB", text(200, `{"data":[],"pad":"${"x".repeat(1024 * 1024)}"}`), /over 1 MiB/],
    ["no data", json(200, { object: "list", models: [{ id: "m" }] }), /data array/],
    ["data not an array", json(200, { data: { id: "m" } }), /data array/],
    ["id not a string", json(200, { data: [{ id: 7 }] }), /data array/],
    ["an array", json(200, [{ id: "m" }]), /data array/],
  ];
  for (const [name, route, message] of cases) {
    const backend = await fakeBackend(t, { "/v1/models": route });
    const report = await verify({ baseUrl: `${backend.origin}/v1`, credential: CREDENTIAL });
    assert.equal(report.ok, false, name);
    assert.equal(report.failure?.code, "not-a-models-list", name);
    assert.match(report.failure?.message ?? "", message, name);
    assert.deepEqual(report.models, [], name);
    assert.equal(backend.requests.length, 1, `${name}: one request, nothing followed`);
    assertNoCredential(report);
  }
});

test("a body just under the cap is read", async (t) => {
  const prefix = JSON.stringify(VLLM_LIST).slice(0, -1);
  const body = `${prefix},"pad":"${"x".repeat(1024 * 1024 - prefix.length - 10)}"}`;
  assert.ok(Buffer.byteLength(body) <= 1024 * 1024);
  const backend = await fakeBackend(t, { "/v1/models": text(200, body) });
  const report = await verify({ baseUrl: `${backend.origin}/v1` });
  assert.equal(report.ok, true);
  assert.equal(report.server, "vllm");
});

test("timeout: no answer, or a body that stalls, within timeoutMs", async (t) => {
  const stalledBody: Route = (_req, res) => {
    res.writeHead(200, { "content-type": "application/json" });
    res.write('{"data":[');
  };
  for (const route of [hang, stalledBody]) {
    const backend = await fakeBackend(t, { "/v1/models": route });
    const started = performance.now();
    const report = await verify({ baseUrl: `${backend.origin}/v1`, timeoutMs: 150 });
    assert.equal(report.failure?.code, "timeout");
    assert.match(report.failure?.message ?? "", /150 ms/);
    assert.ok(performance.now() - started < 2000, "bounded by timeoutMs");
  }
});

test("unreachable: connection refused", async () => {
  const closed = createServer();
  await new Promise<void>((resolve) => closed.listen(0, "127.0.0.1", resolve));
  const { port } = closed.address() as AddressInfo;
  await new Promise<void>((resolve) => closed.close(() => resolve()));
  const report = await verify({ baseUrl: `http://127.0.0.1:${port}/v1`, credential: CREDENTIAL });
  assert.equal(report.failure?.code, "unreachable");
  assert.match(report.failure?.message ?? "", /ECONNREFUSED/);
  assertNoCredential(report);
});

test("the caller's signal rejects the call with its reason, in any phase", async (t) => {
  const reason = new Error("operator cancelled");

  const before = await fakeBackend(t, { "/v1/models": json(200, VLLM_LIST) });
  await assert.rejects(verify({ baseUrl: `${before.origin}/v1`, signal: AbortSignal.abort(reason) }), (error) => {
    assert.equal(error, reason);
    return true;
  });
  assert.equal(before.requests.length, 0, "an aborted signal sends nothing");

  const listing = await fakeBackend(t, { "/v1/models": hang });
  const listAbort = new AbortController();
  const listCall = verify({ baseUrl: `${listing.origin}/v1`, signal: listAbort.signal });
  await waitFor(() => listing.requests.length === 1);
  listAbort.abort(reason);
  await assert.rejects(listCall, (error) => error === reason);

  const propsing = await fakeBackend(t, { "/v1/models": json(200, LLAMA_LIST), "/props": hang });
  const propsAbort = new AbortController();
  const propsCall = verify({ baseUrl: `${propsing.origin}/v1`, signal: propsAbort.signal });
  await waitFor(() => propsing.requests.length === 2);
  propsAbort.abort(reason);
  await assert.rejects(propsCall, (error) => error === reason);
});

test("invalid input throws verify-input-invalid and sends nothing", async (t) => {
  const backend = await fakeBackend(t, { "/v1/models": json(200, VLLM_LIST) });
  const base = `${backend.origin}/v1`;
  const cases: [string, unknown][] = [
    ["no options", undefined],
    ["unknown type", { type: "bedrock", baseUrl: base }],
    ["no baseUrl", { type: "openai-compatible" }],
    ["userinfo", { type: "openai-compatible", baseUrl: `http://user:${CREDENTIAL}@127.0.0.1:1/v1` }],
    ["trailing slash", { type: "openai-compatible", baseUrl: `${base}/` }],
    ["query", { type: "openai-compatible", baseUrl: `${base}?key=${CREDENTIAL}` }],
    ["fragment", { type: "openai-compatible", baseUrl: `${base}#x` }],
    ["not http", { type: "openai-compatible", baseUrl: "ftp://127.0.0.1/v1" }],
    ["empty credential", { type: "openai-compatible", baseUrl: base, credential: "" }],
    ["credential with LF", { type: "openai-compatible", baseUrl: base, credential: `${CREDENTIAL}\nX-Evil: 1` }],
    ["credential with CR", { type: "openai-compatible", baseUrl: base, credential: `${CREDENTIAL}\r` }],
    ["credential with NUL", { type: "openai-compatible", baseUrl: base, credential: `${CREDENTIAL}\u0000` }],
    ["credential not a string", { type: "openai-compatible", baseUrl: base, credential: 42 }],
    ["model with a space", { type: "openai-compatible", baseUrl: base, model: "my model" }],
    ["empty model", { type: "openai-compatible", baseUrl: base, model: "" }],
    ["zero timeout", { type: "openai-compatible", baseUrl: base, timeoutMs: 0 }],
    ["fractional timeout", { type: "openai-compatible", baseUrl: base, timeoutMs: 1.5 }],
    ["timeout past the timer limit", { type: "openai-compatible", baseUrl: base, timeoutMs: 2 ** 31 }],
    ["signal not a signal", { type: "openai-compatible", baseUrl: base, signal: {} }],
  ];
  for (const [name, options] of cases) {
    await assert.rejects(verifyBackend(options as VerifyBackendOptions), (error) => {
      assert.equal((error as { code?: unknown }).code, "verify-input-invalid", name);
      assertNoCredential(error);
      return true;
    });
  }
  assert.equal(backend.requests.length, 0);

  await assert.rejects(verifyBackend({ type: "bedrock" as BackendType, baseUrl: base }), (error: Error) => {
    assert.equal(
      error.message,
      'type must be one of "openai-compatible", "openai", "azure-openai", "vllm", "llama-server", "anthropic", "azure-anthropic"',
    );
    return true;
  });
});

test("the credential appears in no report of any outcome", async (t) => {
  const echo: Route = (req, res) => {
    res.writeHead(200, { "content-type": "application/json" });
    res.end(JSON.stringify({ ...LLAMA_PROPS, echoed: req.headers.authorization, build_info: CREDENTIAL }));
  };
  const backend = await fakeBackend(t, { "/v1/models": json(200, LLAMA_LIST), "/props": echo });
  const reports = [
    await verify({ baseUrl: `${backend.origin}/v1`, credential: CREDENTIAL, model: LLAMA_MODEL }),
    await verify({ baseUrl: `${backend.origin}/v1`, credential: CREDENTIAL, model: "absent" }),
    await verify({ baseUrl: `${backend.origin}/v1`, credential: CREDENTIAL, timeoutMs: 1 }),
  ];
  for (const report of reports) assertNoCredential(report);
});

// Waits for a condition the fake backend's request log sets; the fake records a request
// as soon as it arrives, so this resolves on the next turns of the event loop.
async function waitFor(condition: () => boolean): Promise<void> {
  const deadline = performance.now() + 2000;
  while (!condition()) {
    if (performance.now() > deadline) throw new Error("condition not met within 2 s");
    await new Promise<void>((resolve) => setImmediate(resolve));
  }
}
