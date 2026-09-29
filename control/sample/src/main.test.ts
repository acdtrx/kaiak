import assert from "node:assert/strict";
import { spawn, spawnSync } from "node:child_process";
import type { ChildProcess } from "node:child_process";
import { once } from "node:events";
import { copyFileSync, mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import { createInterface } from "node:readline";
import { afterEach, test } from "node:test";

const MAIN = path.resolve(import.meta.dirname, "main.ts");
const MINIMAL = path.resolve(import.meta.dirname, "../../../protocol/fixtures/config/valid/minimal.json");
const TOKEN = "main-token";

const cleanups: (() => void)[] = [];

afterEach(() => {
  for (const cleanup of cleanups.splice(0)) cleanup();
});

function configFile(): string {
  const dir = mkdtempSync(path.join(tmpdir(), "kaiak-sample-main-"));
  cleanups.push(() => rmSync(dir, { recursive: true, force: true }));
  const file = path.join(dir, "config.json");
  copyFileSync(MINIMAL, file);
  return file;
}

function start(env: Record<string, string>): ChildProcess {
  const child = spawn(process.execPath, [MAIN], {
    env: { PATH: process.env["PATH"] ?? "", KAIAK_SAMPLE_LISTEN: "127.0.0.1:0", KAIAK_CONTROL_TOKEN: TOKEN, ...env },
    stdio: ["ignore", "pipe", "pipe"],
  });
  cleanups.push(() => child.kill("SIGKILL"));
  return child;
}

// The first stdout line that matches.
async function lineMatching(child: ChildProcess, pattern: RegExp): Promise<string> {
  assert.ok(child.stdout);
  for await (const line of createInterface({ input: child.stdout })) {
    if (pattern.test(line)) return line;
  }
  return assert.fail(`the process ended without printing ${pattern}`);
}

test("the server logs its address once listening, serves gateways and exits 0 on SIGTERM", { timeout: 10000 }, async () => {
  const child = start({ KAIAK_SAMPLE_CONFIG: configFile() });
  const line = await lineMatching(child, /"msg":"sample control plane listening on http:\/\/127\.0\.0\.1:\d+"/);
  const { url } = JSON.parse(line) as { url: string };

  const response = await fetch(`${url}/v1/config`, {
    headers: { authorization: `Bearer ${TOKEN}`, "kaiak-protocol": "3", "kaiak-instance": "gw-1" },
  });
  assert.equal(response.status, 200);
  assert.equal(((await response.json()) as { version: number }).version, 1);

  const exited = once(child, "exit");
  child.kill("SIGTERM");
  assert.deepEqual(await exited, [0, null]);
});

test("text logs go through pino-pretty on one line each", { timeout: 10000 }, async () => {
  const child = start({ KAIAK_SAMPLE_CONFIG: configFile(), KAIAK_LOG_FORMAT: "text" });
  const line = await lineMatching(child, /sample control plane listening on http:/);
  assert.match(line, /^\[\d{2}:\d{2}:\d{2}\.\d{3}\] INFO: /);
  const exited = once(child, "exit");
  child.kill("SIGINT");
  assert.deepEqual(await exited, [0, null]);
});

test("missing settings stop the process with exit status 1 and the reason", () => {
  const result = spawnSync(process.execPath, [MAIN], {
    env: { PATH: process.env["PATH"] ?? "", KAIAK_CONTROL_TOKEN: TOKEN },
    encoding: "utf8",
  });
  assert.equal(result.status, 1);
  assert.match(result.stderr, /^kaiak-sample: KAIAK_SAMPLE_CONFIG is required/);
});
