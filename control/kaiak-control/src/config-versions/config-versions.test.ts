import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import path from "node:path";
import { describe, test } from "node:test";

import type { Config } from "../config/index.ts";
import { createMemoryStore } from "../storage/index.ts";
import type { StoredConfig } from "../storage/index.ts";

import { createConfigVersions } from "./index.ts";
import type { ConfigPosition, ConfigVersions, PublishResult } from "./index.ts";

const MINIMAL = path.resolve(import.meta.dirname, "../../../../protocol/fixtures/config/valid/minimal.json");

function minimalConfig(): Config {
  return JSON.parse(readFileSync(MINIMAL, "utf8")) as Config;
}

// A valid config distinguishable by its context length.
function configNumbered(n: number): Config {
  const config = minimalConfig();
  const model = config.models["llama"];
  assert.ok(model, "the minimal fixture has model llama");
  model.metadata.context_length = 1000 + n;
  return config;
}

function failOnListenerError(error: unknown): void {
  assert.fail(`unexpected listener error: ${String(error)}`);
}

function versions(
  options: {
    historySize?: number;
    now?: () => number;
    onListenerError?: (error: unknown, published: StoredConfig) => void;
  } = {},
): ConfigVersions {
  return createConfigVersions({
    store: createMemoryStore(),
    historySize: options.historySize ?? 100,
    clock: options.now ?? (() => 0),
    onListenerError: options.onListenerError ?? failOnListenerError,
  });
}

function publishedVersion(result: PublishResult): number {
  assert.ok(result.ok, "publish succeeded");
  return result.published.version;
}

// Some epoch; before the first publish every epoch gets resync anyway.
const EPOCH = "0123456789abcdef0123456789abcdef";

// A position in the versions' own epoch.
async function at(configVersions: ConfigVersions, version: number): Promise<ConfigPosition> {
  return { epoch: await configVersions.configEpoch(), version };
}

async function publishMany(configVersions: ConfigVersions, count: number): Promise<void> {
  for (let n = 1; n <= count; n++) await configVersions.publishConfig(configNumbered(n));
}

describe("publishing", () => {
  test("nothing is current before the first publish", async () => {
    assert.equal(await versions().currentConfig(), undefined);
  });

  test("versions start at 1 and increase by one", async () => {
    let now = 5000;
    const configVersions = versions({ now: () => now });
    assert.equal(publishedVersion(await configVersions.publishConfig(configNumbered(1))), 1);
    now = 6000;
    assert.equal(publishedVersion(await configVersions.publishConfig(configNumbered(2))), 2);

    const current = await configVersions.currentConfig();
    assert.deepEqual(current, { version: 2, config: configNumbered(2), publishedAt: 6000 });
  });

  test("concurrent publishes get distinct versions", async () => {
    const configVersions = versions();
    const results = await Promise.all([1, 2, 3].map((n) => configVersions.publishConfig(configNumbered(n))));
    assert.deepEqual(results.map(publishedVersion), [1, 2, 3]);
  });

  test("an invalid config is refused with its issues and the current version stays", async () => {
    const configVersions = versions();
    await configVersions.publishConfig(configNumbered(1));

    const invalid = { ...minimalConfig(), keys: { "k-me": { hash: "sha256:00", group: "me" } } };
    const result = await configVersions.publishConfig(invalid);
    assert.equal(result.ok, false);
    assert.ok(!result.ok && result.issues.length > 0 && result.issues.every((issue) => issue.code === "schema"));

    const unknownGroup = minimalConfig();
    unknownGroup.groups = {};
    const semantic = await configVersions.publishConfig(unknownGroup);
    assert.ok(!semantic.ok && semantic.issues.some((issue) => issue.code === "key-group-unknown"));

    assert.equal((await configVersions.currentConfig())?.version, 1);
    assert.equal(publishedVersion(await configVersions.publishConfig(configNumbered(2))), 2);
  });

  test("changing the published document afterwards does not change the stored version", async () => {
    const configVersions = versions();
    const doc = configNumbered(1);
    await configVersions.publishConfig(doc);
    doc.groups = {};
    assert.deepEqual((await configVersions.currentConfig())?.config, configNumbered(1));
  });

  test("the history size must be a positive integer", () => {
    for (const historySize of [0, -1, 1.5]) {
      assert.throws(() => versions({ historySize }), { code: "config-history-size-invalid" });
    }
  });
});

// minimal.json with the given groups; its key stays in "me".
function withGroups(groups: Config["groups"]): Config {
  const config = minimalConfig();
  config.groups = { me: {}, ...groups };
  return config;
}

describe("parents never change", () => {
  test("a publish moving a group to another parent is refused and the current version stays", async () => {
    const configVersions = versions();
    await configVersions.publishConfig(withGroups({ a: {}, b: {}, child: { parent: "a" } }));

    const result = await configVersions.publishConfig(withGroups({ a: {}, b: {}, child: { parent: "b" } }));
    assert.deepEqual(result.ok ? [] : result.issues.map(({ code, path }) => ({ code, path })), [
      { code: "group-parent-changed", path: "/groups/child/parent" },
    ]);
    assert.equal((await configVersions.currentConfig())?.version, 1);
  });

  test("a top-level group given a parent, and a child made top-level, are moves too", async () => {
    const configVersions = versions();
    await configVersions.publishConfig(withGroups({ a: {}, b: {}, child: { parent: "a" } }));

    const result = await configVersions.publishConfig(withGroups({ a: {}, b: { parent: "a" }, child: {} }));
    assert.deepEqual(result.ok ? [] : result.issues.map(({ code, path }) => ({ code, path })), [
      { code: "group-parent-changed", path: "/groups/b/parent" },
      { code: "group-parent-changed", path: "/groups/child/parent" },
    ]);
  });

  test("a group deleted in one publish may come back under another parent in the next", async () => {
    const configVersions = versions();
    await configVersions.publishConfig(withGroups({ a: {}, b: {}, child: { parent: "a" } }));
    assert.equal(publishedVersion(await configVersions.publishConfig(withGroups({ a: {}, b: {} }))), 2);
    assert.equal(
      publishedVersion(await configVersions.publishConfig(withGroups({ a: {}, b: {}, child: { parent: "b" } }))),
      3,
    );
  });

  test("a new group named like an object property is new", async () => {
    const configVersions = versions();
    await configVersions.publishConfig(withGroups({ a: {} }));
    assert.equal(publishedVersion(await configVersions.publishConfig(withGroups({ a: {}, constructor: { parent: "a" } }))), 2);
  });

  test("groups keeping their parents publish, whatever else changes", async () => {
    const configVersions = versions();
    await configVersions.publishConfig(withGroups({ a: {}, child: { parent: "a" } }));
    const edited = withGroups({
      a: { labels: { kind: "team" }, limits: [{ type: "usd_per_month", value: 10 }] },
      child: { parent: "a", allowed_models: ["llama"] },
      added: { parent: "child" },
    });
    assert.equal(publishedVersion(await configVersions.publishConfig(edited)), 2);
  });
});

describe("several processes over one store", () => {
  // Two config versions over one store, as two control-plane processes hold it.
  function twoProcesses(): [ConfigVersions, ConfigVersions] {
    const store = createMemoryStore();
    const make = (): ConfigVersions =>
      createConfigVersions({ store, historySize: 100, clock: () => 0, onListenerError: failOnListenerError });
    return [make(), make()];
  }

  test("publishes racing from both get consecutive versions, each stored once", async () => {
    const [a, b] = twoProcesses();
    const results = await Promise.all([1, 2, 3, 4].map((n) => (n % 2 ? a : b).publishConfig(configNumbered(n))));
    assert.deepEqual(results.map(publishedVersion).sort(), [1, 2, 3, 4]);
    assert.equal((await b.currentConfig())?.version, 4);
  });

  test("each process's listeners hear every version, whichever process published it, in order", async () => {
    const [a, b] = twoProcesses();
    const heardByA: number[] = [];
    const heardByB: number[] = [];
    a.onConfigPublished((published) => heardByA.push(published.version));
    b.onConfigPublished((published) => heardByB.push(published.version));
    await a.publishConfig(configNumbered(1));
    await b.publishConfig(configNumbered(2));
    await a.publishConfig(configNumbered(3));
    // The other process reads each version back after the store's announcement.
    await new Promise((resolve) => setImmediate(resolve));
    assert.deepEqual(heardByA, [1, 2, 3]);
    assert.deepEqual(heardByB, [1, 2, 3]);
  });

  test("a publish that lost the race is checked again against the version that won", async () => {
    const [a, b] = twoProcesses();
    await a.publishConfig(withGroups({ top: {}, other: {} }));
    // Both read version 1, where "child" does not exist. a's version 2 creates it under
    // "top"; b's publish, checked again against version 2, would move it.
    const [won, lost] = await Promise.all([
      a.publishConfig(withGroups({ top: {}, other: {}, child: { parent: "top" } })),
      b.publishConfig(withGroups({ top: {}, other: {}, child: { parent: "other" } })),
    ]);
    assert.equal(publishedVersion(won), 2);
    assert.deepEqual(lost.ok ? [] : lost.issues.map(({ code }) => code), ["group-parent-changed"]);
    assert.equal((await a.currentConfig())?.version, 2);
  });
});

describe("resuming from a version", () => {
  test("the current version gets nothing to replay", async () => {
    const configVersions = versions();
    await publishMany(configVersions, 3);
    assert.deepEqual(await configVersions.configsSince(await at(configVersions, 3)), { resync: false, epoch: await configVersions.configEpoch(), configs: [] });
  });

  test("a version within the history gets every newer version, oldest first", async () => {
    const configVersions = versions({ historySize: 3 });
    await publishMany(configVersions, 5);
    const since = await configVersions.configsSince(await at(configVersions, 2));
    assert.ok(!since.resync);
    assert.deepEqual(
      since.configs.map((entry) => entry.version),
      [3, 4, 5],
    );
    assert.deepEqual(since.configs[0]?.config, configNumbered(3));
  });

  test("a version older than the history gets resync", async () => {
    const configVersions = versions({ historySize: 3 });
    await publishMany(configVersions, 5);
    assert.deepEqual(await configVersions.configsSince(await at(configVersions, 1)), { resync: true });
  });

  test("a version ahead of the current one gets resync", async () => {
    const configVersions = versions();
    await publishMany(configVersions, 2);
    assert.deepEqual(await configVersions.configsSince(await at(configVersions, 3)), { resync: true });
  });

  test("any version before the first publish gets resync", async () => {
    assert.deepEqual(await versions().configsSince({ epoch: EPOCH, version: 1 }), { resync: true });
  });

  test("a version that is no version gets resync", async () => {
    const configVersions = versions();
    await publishMany(configVersions, 2);
    for (const version of [-1, 1.5, Number.NaN]) {
      assert.deepEqual(await configVersions.configsSince(await at(configVersions, version)), { resync: true });
    }
  });

  test("a version from another epoch gets resync, whatever its number", async () => {
    const configVersions = versions();
    await publishMany(configVersions, 3);
    for (const version of [0, 2, 3]) {
      assert.deepEqual(await configVersions.configsSince({ epoch: EPOCH, version }), { resync: true });
    }
  });

  test("the epoch is the store's: a new store starts a new one, the same store keeps it", async () => {
    const store = createMemoryStore();
    const options = { historySize: 10, clock: () => 0, onListenerError: failOnListenerError };
    const first = createConfigVersions({ store, ...options });
    const again = createConfigVersions({ store, ...options });
    const other = createConfigVersions({ store: createMemoryStore(), ...options });
    assert.match(await first.configEpoch(), /^[0-9a-f]{32}$/);
    assert.equal(await again.configEpoch(), await first.configEpoch());
    assert.notEqual(await other.configEpoch(), await first.configEpoch());
  });

  test("a store holding fewer versions than the history size gets resync, not a gap", async () => {
    const store = createMemoryStore();
    const configVersions = createConfigVersions({
      store,
      historySize: 10,
      clock: () => 0,
      onListenerError: failOnListenerError,
    });
    await publishMany(configVersions, 3);
    // A store keeping less than asked: drop everything but the newest version.
    const latest = await store.latestConfig();
    assert.ok(latest);
    assert.ok((await store.publishConfig({ ...latest, version: 4 }, 3, 1)).saved);
    assert.deepEqual(await configVersions.configsSince(await at(configVersions, 1)), { resync: true });
    assert.deepEqual((await configVersions.configsSince(await at(configVersions, 3))).resync, false);
  });
});

describe("subscriptions", () => {
  test("every subscriber hears of each published version", async () => {
    const configVersions = versions();
    const heard: StoredConfig[][] = [[], [], []];
    for (const list of heard) configVersions.onConfigPublished((published) => list.push(published));

    await publishMany(configVersions, 2);
    for (const list of heard) {
      assert.deepEqual(
        list.map((entry) => entry.version),
        [1, 2],
      );
    }
  });

  test("an invalid publish notifies nobody", async () => {
    const configVersions = versions();
    let calls = 0;
    configVersions.onConfigPublished(() => calls++);
    await configVersions.publishConfig({});
    assert.equal(calls, 0);
  });

  test("an unsubscribed listener hears nothing more; others keep hearing", async () => {
    const configVersions = versions();
    const first: number[] = [];
    const second: number[] = [];
    const unsubscribe = configVersions.onConfigPublished((published) => first.push(published.version));
    configVersions.onConfigPublished((published) => second.push(published.version));

    await configVersions.publishConfig(configNumbered(1));
    unsubscribe();
    unsubscribe();
    await configVersions.publishConfig(configNumbered(2));

    assert.deepEqual(first, [1]);
    assert.deepEqual(second, [1, 2]);
  });

  test("the same listener subscribed twice is two subscriptions", async () => {
    const configVersions = versions();
    const heard: number[] = [];
    const listener = (published: StoredConfig) => heard.push(published.version);
    const unsubscribeFirst = configVersions.onConfigPublished(listener);
    configVersions.onConfigPublished(listener);

    await configVersions.publishConfig(configNumbered(1));
    unsubscribeFirst();
    await configVersions.publishConfig(configNumbered(2));
    assert.deepEqual(heard, [1, 1, 2]);
  });

  test("many subscribers come and go cleanly", async () => {
    const configVersions = versions();
    const counts = new Array<number>(1000).fill(0);
    const unsubscribes = counts.map((_, index) =>
      configVersions.onConfigPublished(() => {
        counts[index] = (counts[index] ?? 0) + 1;
      }),
    );
    await configVersions.publishConfig(configNumbered(1));
    unsubscribes.forEach((unsubscribe, index) => {
      if (index % 2 === 0) unsubscribe();
    });
    await configVersions.publishConfig(configNumbered(2));
    counts.forEach((count, index) => assert.equal(count, index % 2 === 0 ? 1 : 2, `subscriber ${index}`));
  });

  test("a failing subscriber is reported, the others still hear, and the publish succeeds", async () => {
    const failures: [string, number][] = [];
    const configVersions = versions({
      onListenerError: (error, published) => failures.push([(error as Error).message, published.version]),
    });
    const heard: number[] = [];
    configVersions.onConfigPublished(() => {
      throw new Error("broken subscriber");
    });
    configVersions.onConfigPublished((published) => heard.push(published.version));

    assert.equal(publishedVersion(await configVersions.publishConfig(configNumbered(1))), 1);
    assert.equal(publishedVersion(await configVersions.publishConfig(configNumbered(2))), 2);
    assert.deepEqual(heard, [1, 2]);
    assert.deepEqual(failures, [
      ["broken subscriber", 1],
      ["broken subscriber", 2],
    ]);
  });
});
