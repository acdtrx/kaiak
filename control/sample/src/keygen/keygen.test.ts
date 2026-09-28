import assert from "node:assert/strict";
import { execFileSync, spawnSync } from "node:child_process";
import { readFileSync } from "node:fs";
import path from "node:path";
import { test } from "node:test";

import { hashKey, validateConfig } from "kaiak-control";

import { runKeygen } from "./index.ts";

const EXAMPLE = path.resolve(import.meta.dirname, "../../../../examples/config.json");
const CLI = path.resolve(import.meta.dirname, "../keygen-cli.ts");
const KEY_FORMAT = /^kaiak-[A-Za-z0-9]{43}$/;

// The `keys` entry the output ends with, parsed as the member of an object it is.
function entryOf(output: string): Record<string, unknown> {
  const lines = output.trimEnd().split("\n");
  const last = lines.at(-1);
  assert.ok(last !== undefined);
  return JSON.parse(`{${last}}`) as Record<string, unknown>;
}

test("prints the key, its ID and its hash, and a keys entry for its group", () => {
  const result = runKeygen(["--id", "k-new", "--group", "rag-service"]);
  assert.ok(result.ok);
  const { created, output } = result;
  assert.match(created.key, KEY_FORMAT);
  assert.equal(created.hash, hashKey(created.key));
  const lines = output.split("\n");
  assert.equal(lines[0], `key:  ${created.key}`);
  assert.equal(lines[1], "id:   k-new");
  assert.equal(lines[2], `hash: ${created.hash}`);
  assert.deepEqual(entryOf(output), { "k-new": { hash: created.hash, group: "rag-service" } });
});

test("the keys entry makes a valid config when pasted in", () => {
  const result = runKeygen(["--id", "k-new", "--group", "alice"]);
  assert.ok(result.ok);
  const config = JSON.parse(readFileSync(EXAMPLE, "utf8")) as { keys: Record<string, unknown> };
  Object.assign(config.keys, entryOf(result.output));
  const validation = validateConfig(config);
  assert.ok(validation.ok, JSON.stringify(validation.ok ? [] : validation.issues));
  assert.deepEqual(validation.config.keys["k-new"], { hash: result.created.hash, group: "alice" });
});

test("every run mints a different key", () => {
  const first = runKeygen(["--id", "k", "--group", "g"]);
  const second = runKeygen(["--id", "k", "--group", "g"]);
  assert.ok(first.ok && second.ok);
  assert.notEqual(first.created.key, second.created.key);
});

test("bad arguments are refused with a message", () => {
  const refused = (args: string[], pattern: RegExp) => {
    const result = runKeygen(args);
    assert.ok(!result.ok, args.join(" "));
    assert.match(result.message, pattern);
  };
  refused(["--group", "g"], /--id is required/);
  refused(["--id", "k"], /--group is required/);
  refused(["--id", "k", "--group", ""], /--group is required/);
  refused(["--id", "bad id!", "--group", "g"], /not a valid key ID/);
  refused(["--id", "k", "--group", "bad group!"], /"bad group!" is not a valid group ID/);
  refused(["--id", "k", "--group", "a".repeat(129)], /not a valid group ID/);
  refused(["--id", "k", "--user", "u"], /--user/);
  refused(["--id", "k", "--workload", "w"], /--workload/);
  refused(["--id", "k", "--group", "g", "extra"], /extra/);
});

test("the command prints on stdout, or fails with exit status 1 and the usage on stderr", () => {
  const output = execFileSync(process.execPath, [CLI, "--id", "k-cli", "--group", "me"], { encoding: "utf8" });
  assert.match(output, /^key: {2}kaiak-[A-Za-z0-9]{43}\n/);

  const failed = spawnSync(process.execPath, [CLI, "--id", "bad id!", "--group", "me"], { encoding: "utf8" });
  assert.equal(failed.status, 1);
  assert.equal(failed.stdout, "");
  assert.match(failed.stderr, /^keygen: "bad id!" is not a valid key ID\nusage \(from control\/\): npm run keygen -w sample -- --id/);
});
