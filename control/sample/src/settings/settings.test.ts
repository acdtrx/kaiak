import assert from "node:assert/strict";
import { test } from "node:test";

import { readSettings } from "./index.ts";
import type { Environment } from "./index.ts";

const REQUIRED: Environment = { KAIAK_SAMPLE_CONFIG: "config.json", KAIAK_CONTROL_TOKEN: "secret" };

function settingsError(env: Environment): string {
  try {
    readSettings(env, "/work");
  } catch (error) {
    assert.equal((error as { code?: unknown }).code, "settings-invalid");
    return error instanceof Error ? error.message : String(error);
  }
  return assert.fail("readSettings accepted the environment");
}

test("defaults: loopback port 8090, JSON logs, the config path resolved against the working directory", () => {
  assert.deepEqual(readSettings(REQUIRED, "/work"), {
    configFile: "/work/config.json",
    listen: { host: "127.0.0.1", port: 8090 },
    token: "secret",
    logFormat: "json",
  });
});

test("a relative config path resolves against the directory npm ran from", () => {
  const settings = readSettings({ ...REQUIRED, INIT_CWD: "/repo" }, "/repo/control/sample");
  assert.equal(settings.configFile, "/repo/config.json");
  assert.equal(readSettings({ ...REQUIRED, KAIAK_SAMPLE_CONFIG: "/abs/c.json" }, "/work").configFile, "/abs/c.json");
});

test("listen addresses: host:port, port 0, all interfaces, IPv6", () => {
  const listen = (value: string) => readSettings({ ...REQUIRED, KAIAK_SAMPLE_LISTEN: value }, "/work").listen;
  assert.deepEqual(listen("127.0.0.1:0"), { host: "127.0.0.1", port: 0 });
  assert.deepEqual(listen(":9000"), { host: "0.0.0.0", port: 9000 });
  assert.deepEqual(listen("localhost:65535"), { host: "localhost", port: 65535 });
  assert.deepEqual(listen("[::1]:8090"), { host: "::1", port: 8090 });
});

test("text logs on request", () => {
  assert.equal(readSettings({ ...REQUIRED, KAIAK_LOG_FORMAT: "text" }, "/work").logFormat, "text");
});

test("missing or invalid settings name the variable", () => {
  assert.match(settingsError({ KAIAK_CONTROL_TOKEN: "secret" }), /KAIAK_SAMPLE_CONFIG is required/);
  assert.match(settingsError({ KAIAK_SAMPLE_CONFIG: "c.json" }), /KAIAK_CONTROL_TOKEN is required/);
  assert.match(settingsError({ ...REQUIRED, KAIAK_CONTROL_TOKEN: "" }), /KAIAK_CONTROL_TOKEN is required/);
  for (const value of ["8090", "127.0.0.1", "127.0.0.1:65536", "127.0.0.1:x", "::1:8090", "host:80:80"]) {
    assert.match(settingsError({ ...REQUIRED, KAIAK_SAMPLE_LISTEN: value }), /KAIAK_SAMPLE_LISTEN=/, value);
  }
  assert.match(settingsError({ ...REQUIRED, KAIAK_LOG_FORMAT: "pretty" }), /KAIAK_LOG_FORMAT="pretty": want json or text/);
});
