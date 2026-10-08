import assert from "node:assert/strict";
import { execFile } from "node:child_process";
import { createServer } from "node:http";
import type { IncomingHttpHeaders } from "node:http";
import type { AddressInfo } from "node:net";
import path from "node:path";
import { test } from "node:test";
import type { TestContext } from "node:test";
import { promisify } from "node:util";

import { explain, runVerify } from "./index.ts";

const CLI = path.resolve(import.meta.dirname, "../verify-cli.ts");
const run = promisify(execFile);

// Trimmed from llama-server b9917's answers (2026-09-29).
const LLAMA_MODEL = "unsloth/Qwen3.8-27B-GGUF:UD-Q4_K_XL";
const LLAMA_LIST = { object: "list", data: [{ id: LLAMA_MODEL, object: "model", owned_by: "llamacpp", meta: { n_ctx: 262144 } }] };
const LLAMA_PROPS = {
  default_generation_settings: { n_ctx: 262144 },
  modalities: { vision: true, audio: false },
  chat_template_caps: { supports_tools: true, supports_reasoning_effort: true },
};
const SECRET = "sk-verify-test-SECRET-91ab";

interface Fake {
  baseUrl: string;
  headers: IncomingHttpHeaders[];
}

// An in-process llama-server on 127.0.0.1, answering /v1/models and /props; with
// `key`, anything without `Authorization: Bearer <key>` gets 401. Closed when the test
// ends.
async function fakeLlamaServer(t: TestContext, key?: string): Promise<Fake> {
  const headers: IncomingHttpHeaders[] = [];
  const server = createServer((req, res) => {
    headers.push(req.headers);
    const body = req.url === "/v1/models" ? LLAMA_LIST : req.url === "/props" ? LLAMA_PROPS : undefined;
    const refused = key !== undefined && req.headers.authorization !== `Bearer ${key}`;
    const status = refused ? 401 : body === undefined ? 404 : 200;
    res.writeHead(status, { "content-type": "application/json" });
    res.end(JSON.stringify(status === 200 ? body : { error: { code: status } }));
  });
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  t.after(() => {
    server.closeAllConnections();
    return new Promise<void>((resolve) => server.close(() => resolve()));
  });
  const { port } = server.address() as AddressInfo;
  return { baseUrl: `http://127.0.0.1:${port}/v1`, headers };
}

async function refused(args: string[], pattern: RegExp, env: NodeJS.ProcessEnv = {}): Promise<void> {
  const result = await runVerify(args, env);
  assert.equal(result.kind, "error", args.join(" "));
  assert.ok(result.kind === "error");
  assert.match(result.message, pattern);
}

test("bad arguments are refused with a message, before anything is sent", async () => {
  await refused([], /--base-url is required/);
  await refused(["--base-url", ""], /--base-url is required/);
  await refused(
    ["--base-url", "http://h/v1", "--type", "bedrock"],
    /^type must be one of "openai-compatible", "openai", "azure-openai", "vllm", "llama-server", "anthropic", "azure-anthropic"$/,
  );
  await refused(["--base-url", "http://h/v1", "--api-key", "sk-x"], /--api-key/);
  await refused(["--base-url", "http://h/v1", "extra"], /extra/);
  await refused(["--base-url", "http://h/v1", "--timeout-ms", "5s"], /--timeout-ms takes a whole number/);
  await refused(["--base-url", "http://h/v1", "--timeout-ms", "-1"], /--timeout-ms/);
  await refused(["--base-url", "http://h/v1", "--api-key-env", "NOT A NAME"], /--api-key-env takes a variable name/);
});

test("--type takes each of the five backend types, and the check follows it", async (t) => {
  const fake = await fakeLlamaServer(t);
  for (const type of ["openai-compatible", "openai", "vllm", "llama-server"]) {
    const result = await runVerify(["--base-url", fake.baseUrl, "--type", type], {});
    assert.ok(result.kind === "report", type);
    assert.equal(result.report.ok, true, type);
    assert.equal(result.report.server, "llama-server", type);
    const note = 'the models list says the server is llama-server: use type "llama-server"';
    assert.equal(result.report.notes.some((entry) => entry.startsWith(note)), type !== "llama-server", type);
  }
  // azure-openai reads another path, which this fake does not serve.
  const azure = await runVerify(["--base-url", fake.baseUrl, "--type", "azure-openai"], {});
  assert.ok(azure.kind === "report");
  assert.equal(azure.report.failure?.code, "not-a-models-list");
  assert.match(azure.report.failure?.message ?? "", /\/v1\/openai\/v1\/models answered 404$/);
});

test("azure-anthropic says plainly it was not checked, and fails", async () => {
  const result = await runVerify(["--base-url", "https://res.services.ai.azure.com", "--type", "azure-anthropic"], {
    VERIFY_KEY: "k",
  });
  assert.ok(result.kind === "report");
  assert.equal(result.report.ok, false);
  assert.equal(result.report.failure?.code, "not-checkable");
  assert.match(explain(result.report, { type: "azure-anthropic", baseUrl: "https://res.services.ai.azure.com" }),
    /^verify: not-checkable: Microsoft Foundry has no models list/);
});

test("the API key comes from the named variable, which must be set", async () => {
  await refused(["--base-url", "http://h/v1", "--api-key-env", "VERIFY_KEY"], /VERIFY_KEY is not set or is empty/);
  await refused(["--base-url", "http://h/v1", "--api-key-env", "VERIFY_KEY"], /VERIFY_KEY is not set/, { VERIFY_KEY: "" });
});

test("input verifyBackend refuses is an error, not a report", async () => {
  await refused(["--base-url", "http://user:pw@h/v1"], /baseUrl is not a valid backend base_url/);
  await refused(["--base-url", "http://h/v1/"], /baseUrl is not a valid backend base_url/);
  await refused(["--base-url", "http://h/v1", "--model", "has space"], /model must be/);
  await refused(["--base-url", "http://h/v1", "--timeout-ms", "0"], /timeoutMs must be/);
  // The credential's own rules apply to the variable's value; the message never shows it.
  const result = await runVerify(["--base-url", "http://h/v1", "--api-key-env", "K"], { K: "a\nb" });
  assert.ok(result.kind === "error");
  assert.match(result.message, /credential must be/);
  assert.doesNotMatch(result.message, /a\nb/);
});

test("with --model: the report on stdout, the metadata to merge and what is left on stderr", async (t) => {
  const fake = await fakeLlamaServer(t);
  const result = await runVerify(["--base-url", fake.baseUrl, "--model", LLAMA_MODEL], {});
  assert.ok(result.kind === "report");
  assert.ok(result.report.ok);
  assert.deepEqual(JSON.parse(result.stdout), result.report);
  assert.deepEqual(result.report.metadata, { context_length: 262144, capabilities: { vision: true } });
  assert.equal(
    result.stderr,
    [
      `Merge into the model's "metadata" (reported values only, hints left out):`,
      "",
      `  {"context_length":262144,"capabilities":{"vision":true}}`,
      "",
      "Hints, to confirm by hand: tools true, reasoning true",
      "Still to decide by hand: capabilities.streaming, capabilities.tools, capabilities.reasoning, reasoning_efforts (when reasoning is true)",
      "",
    ].join("\n"),
  );
});

test("without --model: the report only, nothing on stderr", async (t) => {
  const fake = await fakeLlamaServer(t);
  const result = await runVerify(["--base-url", fake.baseUrl], {});
  assert.ok(result.kind === "report");
  assert.ok(result.report.ok);
  assert.equal(result.report.metadata, undefined);
  assert.equal(result.report.models[0]?.context_length, 262144);
  assert.equal(result.stderr, "");
});

test("a failed check is a report with the reason on stderr", async (t) => {
  const fake = await fakeLlamaServer(t);
  const result = await runVerify(["--base-url", fake.baseUrl, "--model", "does-not-exist"], {});
  assert.ok(result.kind === "report");
  assert.equal(result.report.failure?.code, "model-not-listed");
  assert.match(result.stderr, /^verify: model-not-listed: .*does not list the model "does-not-exist"\n$/);
});

test("the key read from the environment is sent as a bearer credential", async (t) => {
  const fake = await fakeLlamaServer(t, SECRET);
  const refusedCheck = await runVerify(["--base-url", fake.baseUrl], {});
  assert.ok(refusedCheck.kind === "report");
  assert.equal(refusedCheck.report.failure?.code, "credential-refused");

  const result = await runVerify(["--base-url", fake.baseUrl, "--api-key-env", "LLAMA_KEY"], { LLAMA_KEY: SECRET });
  assert.ok(result.kind === "report");
  assert.ok(result.report.ok);
  assert.equal(fake.headers.at(-1)?.authorization, `Bearer ${SECRET}`);
  assert.ok(!result.stdout.includes(SECRET) && !result.stderr.includes(SECRET));
});

test("the command exits 0 on a passing check, 1 on a failed one or bad arguments", async (t) => {
  const fake = await fakeLlamaServer(t);
  const passed = await run(process.execPath, [CLI, "--base-url", fake.baseUrl, "--model", LLAMA_MODEL], { encoding: "utf8" });
  assert.equal((JSON.parse(passed.stdout) as { ok: boolean }).ok, true);
  assert.match(passed.stderr, /^Merge into the model's "metadata"/);

  const failed = await run(process.execPath, [CLI, "--base-url", fake.baseUrl, "--model", "nope"], { encoding: "utf8" }).then(
    () => assert.fail("expected exit status 1"),
    (error: { code: number; stdout: string; stderr: string }) => error,
  );
  assert.equal(failed.code, 1);
  assert.equal((JSON.parse(failed.stdout) as { ok: boolean }).ok, false);
  assert.match(failed.stderr, /^verify: model-not-listed: /);

  const bad = await run(process.execPath, [CLI], { encoding: "utf8" }).then(
    () => assert.fail("expected exit status 1"),
    (error: { code: number; stdout: string; stderr: string }) => error,
  );
  assert.equal(bad.code, 1);
  assert.equal(bad.stdout, "");
  assert.match(bad.stderr, /^verify: --base-url is required.*\nusage \(from control\/\): npm run verify -w sample -- --base-url/);

  const badType = await run(process.execPath, [CLI, "--base-url", fake.baseUrl, "--type", "bedrock"], {
    encoding: "utf8",
  }).then(
    () => assert.fail("expected exit status 1"),
    (error: { code: number; stdout: string; stderr: string }) => error,
  );
  assert.equal(badType.code, 1);
  assert.equal(badType.stdout, "");
  assert.match(badType.stderr, /^verify: type must be one of [^\n]*\nusage /);
  assert.match(badType.stderr, /\n {2}--type {9}the backend's type: openai-compatible, openai, azure-openai, vllm, llama-server, anthropic, azure-anthropic /);
});
