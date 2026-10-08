// Backend verification (docs/specs/BACKEND-VERIFY.md): checks that a backend answers at
// the URL config will use and accepts the credential, and reads what it reports about
// its models, for the app to show and to turn into declared config. It only reports:
// no state, no retries, GETs only, no redirect followed, at most two requests in order
// (none for azure-anthropic, which has no models list: not checkable).
// Called by the app only — never by the core, the Fastify plugin or a schedule.

import { Ajv2020 } from "ajv/dist/2020.js";

import { BACKEND_TYPES } from "../config/index.ts";
import type { BackendType } from "../config/index.ts";
import { definitionChecker } from "../schemas/index.ts";

export interface VerifyBackendOptions {
  type: BackendType;
  // The backend's base_url, exactly as config spells it.
  baseUrl: string;
  // The credential value (not an env name). Omitted: no credential header is sent.
  credential?: string;
  // A backend-side model name (a deployment's model) to check and describe.
  model?: string;
  // Bound on each request, connect to last byte. Default 5000.
  timeoutMs?: number;
  signal?: AbortSignal;
}

export type VerifyFailureCode =
  | "unreachable"
  | "timeout"
  | "credential-refused"
  | "not-a-models-list"
  | "model-not-listed"
  // The type has nothing to check against (azure-anthropic: no models list); nothing
  // was sent.
  | "not-checkable";

export type VerifiedServer = "vllm" | "llama-server" | "unknown";

export interface BackendReport {
  ok: boolean;
  // Present iff ok is false.
  failure?: { code: VerifyFailureCode; message: string };
  server: VerifiedServer;
  // Every listed entry, in list order; [] before a list was read.
  models: VerifiedModel[];
  // Present iff ok and a model was named.
  metadata?: MetadataFragment;
  // About the backend as a whole; for people, the wording is not a contract.
  notes: string[];
}

export interface VerifiedModel {
  id: string;
  context_length?: number;
  capabilities?: VerifiedCapabilities;
  // One Source per value present above.
  sources: {
    context_length?: Source;
    capabilities?: { vision?: Source; tools?: Source; reasoning?: Source };
  };
  // Skipped reads, missing or malformed fields.
  notes: string[];
}

export interface VerifiedCapabilities {
  vision?: boolean;
  tools?: boolean;
  reasoning?: boolean;
}

export interface Source {
  // The URL read; the input rules keep credentials out of it.
  url: string;
  // Dotted path in that answer, e.g. "default_generation_settings.n_ctx".
  field: string;
  // A value the server derives from something other than the model's real ability.
  hint: boolean;
}

// A partial config metadata object for the requested model: reported non-hint values
// only, under their config names.
export interface MetadataFragment {
  context_length?: number;
  capabilities?: { vision?: boolean };
}

const DEFAULT_TIMEOUT_MS = 5000;
// The platform's timer limit: AbortSignal.timeout refuses a longer delay.
const MAX_TIMEOUT_MS = 2_147_483_647;
// The gateway's model probe reads the same.
const BODY_CAP_BYTES = 1024 * 1024;
const USER_AGENT = "kaiak-control";
// The one Anthropic API version, sent as the gateway sends it.
const ANTHROPIC_VERSION = "2023-06-01";

// Input rules are the config schema's own, so a backend that verifies is one config
// accepts. The type and base_url rules sit inside the backend definition: the pointer
// reaches them there.
const isBackendType = definitionChecker("config.schema.json", "backend/properties/type");
const isBaseUrl = definitionChecker("config.schema.json", "backend/properties/base_url");
const isBackendModelName = definitionChecker("config.schema.json", "backend_model_name");
// A reported context length is kept only when config would accept it.
const isContextLength = definitionChecker("config.schema.json", "model/properties/metadata/properties/context_length");

// The helper's own schemas: lenient, they describe other servers' answers, not the
// kaiak contract (so they are not in protocol/).
const ajv = new Ajv2020({ strict: true });
// A valid header value: non-empty, no CR, LF or NUL.
const isCredential = ajv.compile({ type: "string", minLength: 1, pattern: "^[^\\r\\n\\x00]+$" });
const isTimeout = ajv.compile({ type: "integer", minimum: 1, maximum: MAX_TIMEOUT_MS });
const isModelsList = ajv.compile<ModelsList>({
  type: "object",
  required: ["data"],
  properties: {
    data: {
      type: "array",
      items: { type: "object", required: ["id"], properties: { id: { type: "string" } } },
    },
  },
});
const isObject = ajv.compile<Record<string, unknown>>({ type: "object" });
const isBoolean = ajv.compile<boolean>({ type: "boolean" });

interface ModelsList {
  data: ModelEntry[];
}

interface ModelEntry {
  id: string;
  [field: string]: unknown;
}

interface Settings {
  type: BackendType;
  baseUrl: string;
  credential?: string;
  model?: string;
  timeoutMs: number;
  signal?: AbortSignal;
}

// What a backend type means for verification. The URL and the headers are the
// gateway's: each row reads against gateway/internal/provider/<type>.go (url, header),
// and protocol/fixtures/backend-types/ pins both halves to the same request.
type TypeRules =
  // No models list: nothing is sent, and the report says not checkable, and why.
  | { list: "none"; why: string }
  | {
      // "base-models": the list names the resource's base models, not the deployments
      // config routes to, so no model in it is reported or checked.
      list: "models" | "base-models";
      // A URL under the base, as the gateway module's url(path).
      url(base: string, path: string): string;
      // The credential header (none without a credential) and the type's fixed headers,
      // as the gateway module's header().
      headers(credential?: string): Record<string, string>;
      // Appended to the models list's URL.
      listQuery?: string;
      // Recognize the server from the entries' owned_by (Server recognition).
      recognizeServer: boolean;
      // The list entry's field read as context_length, whatever the server.
      entryContextField?: string;
    };

const bearer = (credential?: string): Record<string, string> =>
  credential === undefined ? {} : { Authorization: `Bearer ${credential}` };
const apiKey = (credential?: string): Record<string, string> => (credential === undefined ? {} : { "api-key": credential });
const underBase = (base: string, path: string): string => `${base}/${path}`;
const openAiShaped = { list: "models", url: underBase, headers: bearer, recognizeServer: true } as const;

// The compiler holds the rows to BACKEND_TYPES: a new type compiles only with its row.
const TYPE_RULES: Record<BackendType, TypeRules> = {
  "openai-compatible": openAiShaped,
  openai: openAiShaped,
  vllm: openAiShaped,
  "llama-server": openAiShaped,
  "azure-openai": {
    list: "base-models",
    url: (base, path) => `${base}/openai/v1/${path}`,
    headers: apiKey,
    recognizeServer: false,
  },
  anthropic: {
    list: "models",
    url: underBase,
    headers: (credential) => ({
      ...(credential === undefined ? {} : { "x-api-key": credential }),
      "anthropic-version": ANTHROPIC_VERSION,
    }),
    // Anthropic's list is paged, 20 by default: one request asks for the most a page
    // holds.
    listQuery: "?limit=1000",
    // The type already says what the server is.
    recognizeServer: false,
    entryContextField: "max_input_tokens",
  },
  "azure-anthropic": { list: "none", why: "Microsoft Foundry has no models list" },
};

// A list entry's context_length field on a server recognized as vLLM.
const VLLM_CONTEXT_FIELD = "max_model_len";

// Checks a backend and reports what it says about its models. Throws
// { code: "verify-input-invalid" } on invalid options, before sending anything; rejects
// with the signal's reason when the caller's signal aborts.
export async function verifyBackend(options: VerifyBackendOptions): Promise<BackendReport> {
  const settings = checkInput(options);
  settings.signal?.throwIfAborted();
  const rules = TYPE_RULES[settings.type];

  // Nothing is sent, and the report says not checkable — never ok, so no caller reads
  // it as verified.
  if (rules.list === "none") {
    return {
      ok: false,
      failure: {
        code: "not-checkable",
        message: `${rules.why}: the backend cannot be checked; its reachability and credential stay unchecked until the gateway's first request`,
      },
      server: "unknown",
      models: [],
      notes: [],
    };
  }

  const get: GetOptions = {
    headers: { Accept: "application/json", "User-Agent": USER_AGENT, ...rules.headers(settings.credential) },
    timeoutMs: settings.timeoutMs,
    ...(settings.signal === undefined ? {} : { signal: settings.signal }),
  };
  const listUrl = `${rules.url(settings.baseUrl, "models")}${rules.listQuery ?? ""}`;
  const answer = await getJson(listUrl, get);
  const list = modelsListOf(answer, listUrl, settings);
  if ("failure" in list) return { ok: false, failure: list.failure, server: "unknown", models: [], notes: [] };

  if (rules.list === "base-models") {
    return {
      ok: true,
      server: "unknown",
      models: [],
      ...(settings.model === undefined ? {} : { metadata: {} }),
      notes: [
        `${settings.type} lists base models, not deployments: the models are not reported and every deployment name counts as served`,
      ],
    };
  }

  const server = rules.recognizeServer ? recognizeServer(list.data) : "unknown";
  const contextField = rules.entryContextField ?? (server === "vllm" ? VLLM_CONTEXT_FIELD : undefined);
  const models = list.data.map((entry): VerifiedModel => {
    const model: VerifiedModel = { id: entry.id, sources: {}, notes: [] };
    if (contextField !== undefined) readContextLength(entry, listUrl, contextField, model);
    return model;
  });
  const notes: string[] = [];
  // Only the server's own type gets its gateway module's rules; the backend is reached
  // the same way under either, so this is a note, not a failure.
  if (server !== "unknown" && server !== settings.type) {
    notes.push(`the models list says the server is ${server}: use type "${server}", not "${settings.type}"`);
  }
  if (list.data.length === 0) {
    notes.push("the models list is empty");
  } else if (rules.recognizeServer && server === "unknown") {
    notes.push("server not recognized from owned_by: only reachability, the credential and the listed ids are checked");
  }

  const requested = settings.model === undefined ? undefined : models.find((model) => model.id === settings.model);
  if (settings.model !== undefined && requested === undefined) {
    return {
      ok: false,
      failure: { code: "model-not-listed", message: `${listUrl} does not list the model "${settings.model}"` },
      server,
      models,
      notes,
    };
  }

  if (server === "llama-server") await readLlamaServerProps(settings.baseUrl, get, models);

  return {
    ok: true,
    server,
    models,
    ...(requested === undefined ? {} : { metadata: metadataOf(requested) }),
    notes,
  };
}

function checkInput(options: VerifyBackendOptions): Settings {
  const refuse = (message: string): never => {
    throw Object.assign(new Error(message), { code: "verify-input-invalid" });
  };
  if (typeof options !== "object" || options === null) refuse("options must be an object");
  const { type, baseUrl, credential, model, timeoutMs, signal } = options;
  if (!isBackendType(type)) refuse(`type must be one of ${BACKEND_TYPES.map((name) => `"${name}"`).join(", ")}`);
  // Values are never echoed: a malformed baseUrl may carry userinfo, a credential is secret.
  if (!isBaseUrl(baseUrl)) {
    refuse("baseUrl is not a valid backend base_url (http or https; no userinfo, trailing slash, query or fragment)");
  }
  if (credential !== undefined && !isCredential(credential)) {
    refuse("credential must be a non-empty string without CR, LF or NUL");
  }
  if (model !== undefined && !isBackendModelName(model)) {
    refuse("model must be 1 to 512 printable ASCII characters, no spaces");
  }
  if (timeoutMs !== undefined && !isTimeout(timeoutMs)) {
    refuse(`timeoutMs must be an integer from 1 to ${MAX_TIMEOUT_MS}`);
  }
  if (signal !== undefined && !(signal instanceof AbortSignal)) refuse("signal must be an AbortSignal");
  return {
    type,
    baseUrl,
    ...(credential === undefined ? {} : { credential }),
    ...(model === undefined ? {} : { model }),
    timeoutMs: timeoutMs ?? DEFAULT_TIMEOUT_MS,
    ...(signal === undefined ? {} : { signal }),
  };
}

// What every GET of one check carries.
interface GetOptions {
  headers: Record<string, string>;
  timeoutMs: number;
  signal?: AbortSignal;
}

// What one GET got back.
type Answer =
  | { kind: "json"; status: number; body: unknown }
  | { kind: "status"; status: number }
  | { kind: "not-json"; status: number }
  | { kind: "too-large"; status: number }
  | { kind: "cut"; status: number }
  | { kind: "unreachable"; cause?: string }
  | { kind: "timeout" };

// One bounded GET with the helper's headers: the timeout covers connect to last byte,
// the caller's signal aborts it (rejecting with its reason), a redirect is returned as
// it is, and a 2xx body is read up to BODY_CAP_BYTES and parsed as JSON. Other bodies
// are not read: no backend text reaches a report.
async function getJson(url: string, get: GetOptions): Promise<Answer> {
  const timeout = AbortSignal.timeout(get.timeoutMs);
  const signal = get.signal ? AbortSignal.any([get.signal, timeout]) : timeout;
  const aborted = (): Answer => {
    get.signal?.throwIfAborted();
    return { kind: "timeout" };
  };

  let response: Response;
  try {
    response = await fetch(url, { method: "GET", headers: get.headers, redirect: "manual", signal });
  } catch (error) {
    if (signal.aborted) return aborted();
    return { kind: "unreachable", ...causeOf(error) };
  }

  const { status } = response;
  let bytes: Uint8Array | undefined;
  try {
    if (status < 200 || status > 299) {
      await response.body?.cancel();
      return { kind: "status", status };
    }
    bytes = response.body ? await readCapped(response.body) : new Uint8Array();
  } catch {
    if (signal.aborted) return aborted();
    return { kind: "cut", status };
  }
  if (bytes === undefined) return { kind: "too-large", status };

  try {
    return { kind: "json", status, body: JSON.parse(new TextDecoder("utf-8", { fatal: true }).decode(bytes)) };
  } catch {
    // Invalid UTF-8 or JSON: the answer is reported as not JSON, never its text.
    return { kind: "not-json", status };
  }
}

// The platform's error code for a failed connection (ECONNREFUSED, ENOTFOUND, …) — a
// code only, never a message.
function causeOf(error: unknown): { cause?: string } {
  const cause = error instanceof Error ? error.cause : undefined;
  const code = typeof cause === "object" && cause !== null && "code" in cause ? cause.code : undefined;
  return typeof code === "string" && /^[A-Z0-9_]+$/.test(code) ? { cause: code } : {};
}

// Reads a body up to BODY_CAP_BYTES; undefined when it is longer (the rest is not read).
async function readCapped(body: ReadableStream<Uint8Array>): Promise<Uint8Array | undefined> {
  const reader = body.getReader();
  const chunks: Uint8Array[] = [];
  let size = 0;
  for (;;) {
    const { done, value } = await reader.read();
    if (done) break;
    size += value.byteLength;
    if (size > BODY_CAP_BYTES) {
      await reader.cancel();
      return undefined;
    }
    chunks.push(value);
  }
  return Buffer.concat(chunks);
}

function modelsListOf(
  answer: Answer,
  url: string,
  settings: Settings,
): ModelsList | { failure: { code: VerifyFailureCode; message: string } } {
  const fail = (code: VerifyFailureCode, message: string) => ({ failure: { code, message } });
  if (answer.kind === "json") {
    if (isModelsList(answer.body)) return answer.body;
    return fail("not-a-models-list", `${url} answered ${answer.status} without a data array of entries with a string id`);
  }
  if (answer.kind === "status" && (answer.status === 401 || answer.status === 403)) {
    const sent = settings.credential === undefined ? "no credential was sent" : "the credential was refused";
    return fail("credential-refused", `${url} answered ${answer.status}: ${sent}`);
  }
  if (answer.kind === "status" && answer.status >= 300 && answer.status <= 399) {
    return fail("not-a-models-list", `${url} answered ${answer.status}, a redirect (not followed)`);
  }
  const code = answer.kind === "unreachable" || answer.kind === "timeout" ? answer.kind : "not-a-models-list";
  return fail(code, describeAnswer(answer, url, settings.timeoutMs));
}

// Why an answer that is not JSON gave nothing to read.
function describeAnswer(answer: Exclude<Answer, { kind: "json" }>, url: string, timeoutMs: number): string {
  switch (answer.kind) {
    case "unreachable":
      return `no answer from ${url}${answer.cause === undefined ? "" : ` (${answer.cause})`}`;
    case "timeout":
      return `${url} did not answer whole within ${timeoutMs} ms`;
    case "status":
      return `${url} answered ${answer.status}`;
    case "cut":
      return `${url} answered ${answer.status} but the body was cut off`;
    case "too-large":
      return `${url} answered ${answer.status} with a body over 1 MiB`;
    case "not-json":
      return `${url} answered ${answer.status} with a body that is not JSON`;
  }
}

// From every entry's owned_by, for the types whose row asks (TYPE_RULES).
function recognizeServer(entries: ModelEntry[]): VerifiedServer {
  if (entries.length === 0) return "unknown";
  if (entries.every((entry) => entry.owned_by === "vllm")) return "vllm";
  if (entries.every((entry) => entry.owned_by === "llamacpp")) return "llama-server";
  return "unknown";
}

// Sets the model's context_length from the field, or notes why it is left out.
function readContextLength(doc: Record<string, unknown>, url: string, field: string, model: VerifiedModel): void {
  const read = readField(doc, field, isContextLength);
  if (read.kind === "value") {
    model.context_length = read.value as number;
    model.sources.context_length = { url, field, hint: false };
  } else {
    model.notes.push(absentNote("context_length", url, field, read.kind, "a positive integer"));
  }
}

// The /props fields read and the values they give (docs/specs/BACKEND-VERIFY.md, Sources).
const PROPS_CAPABILITIES = [
  { capability: "vision", field: "modalities.vision", hint: false },
  { capability: "tools", field: "chat_template_caps.supports_tools", hint: true },
  { capability: "reasoning", field: "chat_template_caps.supports_reasoning_effort", hint: true },
] as const;
const PROPS_CONTEXT_FIELD = "default_generation_settings.n_ctx";

// llama-server serves /props at its root only, and it describes the one model the
// server holds: read only when the root is known and the list has one entry.
async function readLlamaServerProps(baseUrl: string, get: GetOptions, models: VerifiedModel[]): Promise<void> {
  const noteAll = (note: string) => models.forEach((model) => model.notes.push(note));
  if (!baseUrl.endsWith("/v1")) {
    noteAll("/props skipped: the base URL does not end in /v1, so the server's root is unknown");
    return;
  }
  const [model] = models;
  if (model === undefined || models.length > 1) {
    noteAll(`/props skipped: it describes one model and the list has ${models.length} (router mode)`);
    return;
  }

  const url = `${baseUrl.slice(0, -"/v1".length)}/props`;
  const answer = await getJson(url, get);
  const props = propsOf(answer, url, get.timeoutMs);
  if (typeof props === "string") {
    model.notes.push(`${props}: its values are not reported`);
    return;
  }

  readContextLength(props, url, PROPS_CONTEXT_FIELD, model);

  const capabilities: VerifiedCapabilities = {};
  const sources: NonNullable<VerifiedModel["sources"]["capabilities"]> = {};
  for (const { capability, field, hint } of PROPS_CAPABILITIES) {
    const read = readField(props, field, isBoolean);
    if (read.kind === "value") {
      capabilities[capability] = read.value as boolean;
      sources[capability] = { url, field, hint };
    } else {
      model.notes.push(absentNote(capability, url, field, read.kind, "a boolean"));
    }
  }
  if (Object.keys(capabilities).length > 0) {
    model.capabilities = capabilities;
    model.sources.capabilities = sources;
  }
}

// The /props document, or why it could not be read.
function propsOf(answer: Answer, url: string, timeoutMs: number): Record<string, unknown> | string {
  if (answer.kind !== "json") return describeAnswer(answer, url, timeoutMs);
  return isObject(answer.body) ? answer.body : `${url} answered ${answer.status} with JSON that is not an object`;
}

type FieldRead = { kind: "value"; value: unknown } | { kind: "missing" } | { kind: "malformed" };

// Reads a dotted field from where the answer puts it. A field or a parent that is
// absent is missing; one present with the wrong type is malformed.
function readField(doc: Record<string, unknown>, field: string, check: (value: unknown) => boolean): FieldRead {
  let value: unknown = doc;
  for (const key of field.split(".")) {
    if (!isObject(value)) return { kind: "malformed" };
    if (!Object.hasOwn(value, key)) return { kind: "missing" };
    value = value[key];
  }
  return check(value) ? { kind: "value", value } : { kind: "malformed" };
}

function absentNote(
  name: string,
  url: string,
  field: string,
  kind: "missing" | "malformed",
  expected: string,
): string {
  return kind === "missing"
    ? `${name} not reported: ${url} has no ${field}`
    : `${name} left out: ${field} in ${url} is not ${expected}`;
}

// Non-hint values only (context_length, vision): a hint pasted into config would pass
// validation unchecked, where a value left out fails it until someone decides.
function metadataOf(model: VerifiedModel): MetadataFragment {
  const fragment: MetadataFragment = {};
  if (model.context_length !== undefined) fragment.context_length = model.context_length;
  if (model.capabilities?.vision !== undefined) fragment.capabilities = { vision: model.capabilities.vision };
  return fragment;
}
