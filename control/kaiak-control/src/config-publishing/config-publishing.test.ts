import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import path from "node:path";
import { describe, test } from "node:test";

import type { Config } from "../config/index.ts";
import { createMemoryStore } from "../storage/index.ts";
import type { ControlPlaneStore } from "../storage/index.ts";
import { configNumbered, numberOf } from "../test-support/index.ts";

import { configHash, createConfigPublishing } from "./index.ts";
import type { ConfigPublishing, PublishedConfig, PublishResult } from "./index.ts";

const MINIMAL = path.resolve(import.meta.dirname, "../../../../protocol/fixtures/config/valid/minimal.json");

function minimalConfig(): Config {
  return JSON.parse(readFileSync(MINIMAL, "utf8")) as Config;
}

// A list with each run of repeats kept once: listeners may hear one config more than
// once (a publish through this process is heard for its own read and for the store's
// announcement).
function runs<T>(list: readonly T[]): T[] {
  return list.filter((item, index) => index === 0 || item !== list[index - 1]);
}

function failOnListenerError(error: unknown): void {
  assert.fail(`unexpected listener error: ${String(error)}`);
}

function publishing(
  options: {
    store?: ControlPlaneStore;
    now?: () => number;
    onListenerError?: (error: unknown, published: PublishedConfig) => void;
  } = {},
): ConfigPublishing {
  const store = options.store ?? createMemoryStore();
  const configs = createConfigPublishing({
    store,
    clock: options.now ?? (() => 0),
    onListenerError: options.onListenerError ?? failOnListenerError,
    retryDelaysMs: [],
    onDeliveryFailed: (error) => assert.fail(`unexpected delivery failure: ${String(error)}`),
  });
  // The core hands its modules every change the store announces.
  store.subscribe(configs.takeChange);
  return configs;
}

function published(result: PublishResult): PublishedConfig {
  assert.ok(result.ok, "publish succeeded");
  return result.published;
}

async function publishMany(configs: ConfigPublishing, count: number): Promise<void> {
  for (let n = 1; n <= count; n++) await configs.publishConfig(configNumbered(n));
}

describe("publishing", () => {
  test("nothing is current before the first publish", async () => {
    assert.equal(await publishing().currentConfig(), undefined);
  });

  test("a publish replaces the current config, with its hash and publish time", async () => {
    let now = 5000;
    const configs = publishing({ now: () => now });
    published(await configs.publishConfig(configNumbered(1)));
    now = 6000;
    const second = published(await configs.publishConfig(configNumbered(2)));

    const current = await configs.currentConfig();
    assert.deepEqual(current, second);
    assert.deepEqual(current?.config, configNumbered(2));
    assert.equal(current?.publishedAt, 6000);
  });

  test("the store keeps the config's JSON text, and the hash is its lowercase hex SHA-256", async () => {
    const store = createMemoryStore();
    const config = configNumbered(1);
    const expected = createHash("sha256").update(JSON.stringify(config)).digest("hex");
    assert.equal(configHash(config), expected);
    const entry = published(await publishing({ store }).publishConfig(config));
    assert.equal(entry.hash, expected);
    assert.equal(entry.text, JSON.stringify(config));
    assert.deepEqual(await store.currentConfig(), { text: JSON.stringify(config), hash: expected, publishedAt: 0 });
  });

  test("publishing identical content again is a publish with the same hash, heard again", async () => {
    const configs = publishing();
    const heard: string[] = [];
    configs.onConfigPublished((current) => heard.push(current.hash));
    const first = published(await configs.publishConfig(configNumbered(1)));
    const count = heard.length;
    const again = published(await configs.publishConfig(configNumbered(1)));
    assert.equal(again.hash, first.hash);
    assert.ok(heard.length > count, "listeners hear the publish");
  });

  test("concurrent publishes from one process each succeed; one of them is current", async () => {
    const configs = publishing();
    const results = await Promise.all([1, 2, 3].map((n) => configs.publishConfig(configNumbered(n))));
    const hashes = results.map((result) => published(result).hash);
    assert.ok(hashes.includes((await configs.currentConfig())?.hash ?? ""));
  });

  test("every read takes its place in the order it was issued, however late it completes", async () => {
    const store = createMemoryStore();
    const release = Promise.withResolvers<void>();
    let hold = true;
    const slow: ControlPlaneStore = {
      ...store,
      async currentConfig() {
        if (hold) {
          hold = false;
          await release.promise;
        }
        return store.currentConfig();
      },
    };
    const configs = publishing({ store: slow });
    const reads: number[] = [];
    configs.onConfigRead((read) => reads.push(read.read));
    const early = configs.readConfig();
    // The publish's own check and its delivery reads are issued after the held read.
    published(await configs.publishConfig(configNumbered(1)));
    release.resolve();
    const held = await early;
    assert.ok(held, "the held read completes with the config");
    assert.ok(reads.length > 0 && reads.every((read) => read > held.read), "reads issued later carry later places");
  });

  test("an invalid config is refused with its issues and the current config stays", async () => {
    const configs = publishing();
    const first = published(await configs.publishConfig(configNumbered(1)));

    const invalid = { ...minimalConfig(), keys: { "k-me": { hash: "sha256:00", group: "me" } } };
    const result = await configs.publishConfig(invalid);
    assert.equal(result.ok, false);
    assert.ok(!result.ok && result.issues.length > 0 && result.issues.every((issue) => issue.code === "schema"));

    const unknownGroup = minimalConfig();
    unknownGroup.groups = {};
    const semantic = await configs.publishConfig(unknownGroup);
    assert.ok(!semantic.ok && semantic.issues.some((issue) => issue.code === "key-group-unknown"));

    assert.equal((await configs.currentConfig())?.hash, first.hash);
  });

  test("changing the published document afterwards does not change the current config", async () => {
    const configs = publishing();
    const doc = configNumbered(1);
    await configs.publishConfig(doc);
    doc.groups = {};
    assert.deepEqual((await configs.currentConfig())?.config, configNumbered(1));
  });
});

// minimal.json with the given groups; its key stays in "me".
function withGroups(groups: Config["groups"]): Config {
  const config = minimalConfig();
  config.groups = { me: {}, ...groups };
  return config;
}

describe("parents never change", () => {
  test("a publish moving a group to another parent is refused and the current config stays", async () => {
    const configs = publishing();
    const first = published(await configs.publishConfig(withGroups({ a: {}, b: {}, child: { parent: "a" } })));

    const result = await configs.publishConfig(withGroups({ a: {}, b: {}, child: { parent: "b" } }));
    assert.deepEqual(result.ok ? [] : result.issues.map(({ code, path }) => ({ code, path })), [
      { code: "group-parent-changed", path: "/groups/child/parent" },
    ]);
    assert.equal((await configs.currentConfig())?.hash, first.hash);
  });

  test("a top-level group given a parent, and a child made top-level, are moves too", async () => {
    const configs = publishing();
    await configs.publishConfig(withGroups({ a: {}, b: {}, child: { parent: "a" } }));

    const result = await configs.publishConfig(withGroups({ a: {}, b: { parent: "a" }, child: {} }));
    assert.deepEqual(result.ok ? [] : result.issues.map(({ code, path }) => ({ code, path })), [
      { code: "group-parent-changed", path: "/groups/b/parent" },
      { code: "group-parent-changed", path: "/groups/child/parent" },
    ]);
  });

  test("a group deleted in one publish may come back under another parent in the next", async () => {
    const configs = publishing();
    await configs.publishConfig(withGroups({ a: {}, b: {}, child: { parent: "a" } }));
    published(await configs.publishConfig(withGroups({ a: {}, b: {} })));
    published(await configs.publishConfig(withGroups({ a: {}, b: {}, child: { parent: "b" } })));
  });

  test("a new group named like an object property is new", async () => {
    const configs = publishing();
    await configs.publishConfig(withGroups({ a: {} }));
    published(await configs.publishConfig(withGroups({ a: {}, constructor: { parent: "a" } })));
  });

  test("groups keeping their parents publish, whatever else changes", async () => {
    const configs = publishing();
    await configs.publishConfig(withGroups({ a: {}, child: { parent: "a" } }));
    const edited = withGroups({
      a: { labels: { kind: "team" }, limits: [{ type: "usd_per_month", value: 10 }] },
      child: { parent: "a", allowed_models: ["llama"] },
      added: { parent: "child" },
    });
    published(await configs.publishConfig(edited));
  });
});

describe("several processes over one store", () => {
  // Two config publishers over one store, as two control-plane processes hold it.
  function twoProcesses(): [ConfigPublishing, ConfigPublishing] {
    const store = createMemoryStore();
    return [publishing({ store }), publishing({ store })];
  }

  test("publishes racing from both each succeed; one of them is current", async () => {
    const [a, b] = twoProcesses();
    const results = await Promise.all([1, 2, 3, 4].map((n) => (n % 2 ? a : b).publishConfig(configNumbered(n))));
    const hashes = results.map((result) => published(result).hash);
    assert.ok(hashes.includes((await b.currentConfig())?.hash ?? ""));
  });

  test("each process's listeners hear every current config, whichever process published it, in order", async () => {
    const [a, b] = twoProcesses();
    const heardByA: number[] = [];
    const heardByB: number[] = [];
    a.onConfigPublished((current) => heardByA.push(numberOf(current.config)));
    b.onConfigPublished((current) => heardByB.push(numberOf(current.config)));
    await a.publishConfig(configNumbered(1));
    await b.publishConfig(configNumbered(2));
    await a.publishConfig(configNumbered(3));
    // The other process reads the current config back after the store's announcement.
    await new Promise((resolve) => setImmediate(resolve));
    assert.deepEqual(runs(heardByA), [1, 2, 3]);
    assert.deepEqual(runs(heardByB), [1, 2, 3]);
  });

  test("a publish that lost the race is checked again against the config that won", async () => {
    const [a, b] = twoProcesses();
    await a.publishConfig(withGroups({ top: {}, other: {} }));
    // Both read a config where "child" does not exist. a's publish creates it under
    // "top"; b's publish, checked again against a's, would move it.
    const [won, lost] = await Promise.all([
      a.publishConfig(withGroups({ top: {}, other: {}, child: { parent: "top" } })),
      b.publishConfig(withGroups({ top: {}, other: {}, child: { parent: "other" } })),
    ]);
    const winner = published(won);
    assert.deepEqual(lost.ok ? [] : lost.issues.map(({ code }) => code), ["group-parent-changed"]);
    assert.equal((await a.currentConfig())?.hash, winner.hash);
  });

  test("a store restored to an older config hands that config out like any other", async () => {
    const store = createMemoryStore();
    const configs = publishing({ store });
    const heard: number[] = [];
    configs.onConfigPublished((current) => heard.push(numberOf(current.config)));
    await configs.publishConfig(configNumbered(1));
    await configs.publishConfig(configNumbered(2));
    // The same content as an earlier publish is handed out again once something else
    // was current in between: a hash says what, never when.
    await configs.publishConfig(configNumbered(1));
    assert.deepEqual(runs(heard), [1, 2, 1]);
  });
});

describe("subscriptions", () => {
  test("every subscriber hears of each published config", async () => {
    const configs = publishing();
    const heard: PublishedConfig[][] = [[], [], []];
    for (const list of heard) configs.onConfigPublished((current) => list.push(current));

    await publishMany(configs, 2);
    for (const list of heard) {
      assert.deepEqual(
        runs(list.map((entry) => numberOf(entry.config))),
        [1, 2],
      );
    }
  });

  test("an invalid publish notifies nobody", async () => {
    const configs = publishing();
    let calls = 0;
    configs.onConfigPublished(() => calls++);
    await configs.publishConfig({});
    assert.equal(calls, 0);
  });

  test("an unsubscribed listener hears nothing more; others keep hearing", async () => {
    const configs = publishing();
    const first: number[] = [];
    const second: number[] = [];
    const unsubscribe = configs.onConfigPublished((current) => first.push(numberOf(current.config)));
    configs.onConfigPublished((current) => second.push(numberOf(current.config)));

    await configs.publishConfig(configNumbered(1));
    unsubscribe();
    unsubscribe();
    await configs.publishConfig(configNumbered(2));

    assert.deepEqual(runs(first), [1]);
    assert.deepEqual(runs(second), [1, 2]);
  });

  test("the same listener subscribed twice is two subscriptions", async () => {
    const configs = publishing();
    const heard: number[] = [];
    const listener = (current: PublishedConfig) => heard.push(numberOf(current.config));
    const unsubscribeFirst = configs.onConfigPublished(listener);
    configs.onConfigPublished(listener);

    await configs.publishConfig(configNumbered(1));
    const reads = heard.length / 2;
    unsubscribeFirst();
    await configs.publishConfig(configNumbered(2));
    // Twice for every read of the first config, once for every read of the second.
    assert.deepEqual(heard, [...Array<number>(reads * 2).fill(1), ...Array<number>(reads).fill(2)]);
  });

  test("many subscribers come and go cleanly", async () => {
    const configs = publishing();
    const counts = new Array<number>(1000).fill(0);
    const unsubscribes = counts.map((_, index) =>
      configs.onConfigPublished(() => {
        counts[index] = (counts[index] ?? 0) + 1;
      }),
    );
    await configs.publishConfig(configNumbered(1));
    const reads = counts[0] ?? 0;
    unsubscribes.forEach((unsubscribe, index) => {
      if (index % 2 === 0) unsubscribe();
    });
    await configs.publishConfig(configNumbered(2));
    counts.forEach((count, index) => assert.equal(count, index % 2 === 0 ? reads : 2 * reads, `subscriber ${index}`));
  });

  test("a failing subscriber is reported, the others still hear, and the publish succeeds", async () => {
    const failures: [string, number][] = [];
    const configs = publishing({
      onListenerError: (error, current) => failures.push([(error as Error).message, numberOf(current.config)]),
    });
    const heard: number[] = [];
    configs.onConfigPublished(() => {
      throw new Error("broken subscriber");
    });
    configs.onConfigPublished((current) => heard.push(numberOf(current.config)));

    published(await configs.publishConfig(configNumbered(1)));
    published(await configs.publishConfig(configNumbered(2)));
    assert.deepEqual(runs(heard), [1, 2]);
    assert.equal(failures.length, heard.length, "one failure for every config handed out");
    assert.deepEqual(runs(failures.map(([message, n]) => `${message} ${n}`)), ["broken subscriber 1", "broken subscriber 2"]);
  });
});
