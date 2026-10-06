import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import path from "node:path";
import { describe, test } from "node:test";

import type { Config } from "../config/index.ts";
import { createMemoryStore } from "../storage/index.ts";
import type { ControlPlaneStore, CurrentConfig } from "../storage/index.ts";

import { configHash, createConfigPublishing } from "./index.ts";
import type { ConfigPublishing, PublishResult } from "./index.ts";

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

// Which configNumbered a config is.
function numberOf(config: Config): number {
  return (config.models["llama"]?.metadata.context_length ?? 0) - 1000;
}

function failOnListenerError(error: unknown): void {
  assert.fail(`unexpected listener error: ${String(error)}`);
}

function publishing(
  options: {
    store?: ControlPlaneStore;
    now?: () => number;
    onListenerError?: (error: unknown, published: CurrentConfig) => void;
  } = {},
): ConfigPublishing {
  return createConfigPublishing({
    store: options.store ?? createMemoryStore(),
    clock: options.now ?? (() => 0),
    observeSequence: () => {},
    onListenerError: options.onListenerError ?? failOnListenerError,
  });
}

function published(result: PublishResult): CurrentConfig {
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

  test("the hash is the lowercase hex SHA-256 of the config's JSON as sent", async () => {
    const config = configNumbered(1);
    const expected = createHash("sha256").update(JSON.stringify(config)).digest("hex");
    assert.equal(configHash(config), expected);
    assert.equal(published(await publishing().publishConfig(config)).hash, expected);
  });

  test("publishing identical content again is a publish with the same hash", async () => {
    const configs = publishing();
    const first = published(await configs.publishConfig(configNumbered(1)));
    const again = published(await configs.publishConfig(configNumbered(1)));
    assert.equal(again.hash, first.hash);
    assert.ok(again.sequence > first.sequence);
  });

  test("concurrent publishes from one process each succeed; the last stored is current", async () => {
    const configs = publishing();
    const results = await Promise.all([1, 2, 3].map((n) => configs.publishConfig(configNumbered(n))));
    const stored = results.map(published);
    const last = stored.reduce((latest, entry) => (entry.sequence > latest.sequence ? entry : latest));
    assert.equal((await configs.currentConfig())?.hash, last.hash);
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

  test("publishes racing from both are each stored once, one after another", async () => {
    const [a, b] = twoProcesses();
    const results = await Promise.all([1, 2, 3, 4].map((n) => (n % 2 ? a : b).publishConfig(configNumbered(n))));
    const sequences = results.map((result) => published(result).sequence);
    assert.equal(new Set(sequences).size, 4, "every publish moved the store on once");
    const last = results.map(published).reduce((latest, entry) => (entry.sequence > latest.sequence ? entry : latest));
    assert.equal((await b.currentConfig())?.hash, last.hash);
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
    assert.deepEqual(heardByA, [1, 2, 3]);
    assert.deepEqual(heardByB, [1, 2, 3]);
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
    assert.deepEqual(heard, [1, 2, 1]);
  });
});

describe("subscriptions", () => {
  test("every subscriber hears of each published config", async () => {
    const configs = publishing();
    const heard: CurrentConfig[][] = [[], [], []];
    for (const list of heard) configs.onConfigPublished((current) => list.push(current));

    await publishMany(configs, 2);
    for (const list of heard) {
      assert.deepEqual(
        list.map((entry) => numberOf(entry.config)),
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

    assert.deepEqual(first, [1]);
    assert.deepEqual(second, [1, 2]);
  });

  test("the same listener subscribed twice is two subscriptions", async () => {
    const configs = publishing();
    const heard: number[] = [];
    const listener = (current: CurrentConfig) => heard.push(numberOf(current.config));
    const unsubscribeFirst = configs.onConfigPublished(listener);
    configs.onConfigPublished(listener);

    await configs.publishConfig(configNumbered(1));
    unsubscribeFirst();
    await configs.publishConfig(configNumbered(2));
    assert.deepEqual(heard, [1, 1, 2]);
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
    unsubscribes.forEach((unsubscribe, index) => {
      if (index % 2 === 0) unsubscribe();
    });
    await configs.publishConfig(configNumbered(2));
    counts.forEach((count, index) => assert.equal(count, index % 2 === 0 ? 1 : 2, `subscriber ${index}`));
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
    assert.deepEqual(heard, [1, 2]);
    assert.deepEqual(failures, [
      ["broken subscriber", 1],
      ["broken subscriber", 2],
    ]);
  });
});
