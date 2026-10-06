import assert from "node:assert/strict";
import { EventEmitter } from "node:events";
import { mkdirSync, mkdtempSync, readFileSync, renameSync, rmSync, symlinkSync, watch, writeFileSync } from "node:fs";
import type { FSWatcher } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import { afterEach, test } from "node:test";

import { createControlPlane, createMemoryStore } from "kaiak-control";
import type { ControlPlane } from "kaiak-control";

import { WATCH_TRIGGER, createConfigFile } from "./index.ts";
import type { ConfigFile, ReloadRun } from "./index.ts";

const MINIMAL = path.resolve(import.meta.dirname, "../../../../protocol/fixtures/config/valid/minimal.json");

// A valid config document distinguishable by its context length.
function configText(n: number): string {
  const config = JSON.parse(readFileSync(MINIMAL, "utf8")) as { models: { llama: { metadata: { context_length: number } } } };
  config.models.llama.metadata.context_length = 1000 + n;
  return JSON.stringify(config, null, 2);
}

interface Fixture {
  dir: string;
  file: string;
  controlPlane: ControlPlane;
  configFile: ConfigFile;
  runs: ReloadRun[];
  // Resolves with the first run, recorded from now on, that matches.
  nextRun(match: (run: ReloadRun) => boolean): Promise<ReloadRun>;
  // Resolves when the watcher's directory watch next reports an entry of this name.
  nextWatchEvent(name: string): Promise<void>;
}

type WatchListener = (event: string, name: string | null) => void;

const cleanups: (() => void)[] = [];

afterEach(() => {
  for (const cleanup of cleanups.splice(0)) cleanup();
});

// A config file on a fresh directory, watched with fs.watch (or `fakeWatch`, handed the
// watcher's listener) as the watcher is in use.
function setUp(initial: string | undefined, fakeWatch?: (listener: WatchListener) => void): Fixture {
  const dir = mkdtempSync(path.join(tmpdir(), "kaiak-sample-config-"));
  const file = path.join(dir, "config.json");
  if (initial !== undefined) writeFileSync(file, initial);
  const controlPlane = createControlPlane({ store: createMemoryStore(), token: "t" });
  const runs: ReloadRun[] = [];
  const waiters: { match: (run: ReloadRun) => boolean; resolve: (run: ReloadRun) => void }[] = [];
  const eventWaiters: { name: string; resolve: () => void }[] = [];
  const configFile = createConfigFile({
    path: file,
    controlPlane,
    debounceMs: 20,
    onReload: (run) => {
      runs.push(run);
      for (const waiter of [...waiters]) {
        if (waiter.match(run)) {
          waiters.splice(waiters.indexOf(waiter), 1);
          waiter.resolve(run);
        }
      }
    },
    onWatchError: (error) => assert.fail(`watch failed: ${String(error)}`),
    watchDirectory: (watched, listener) => {
      if (fakeWatch) {
        fakeWatch(listener);
        return Object.assign(new EventEmitter(), { close: () => {} }) as unknown as FSWatcher;
      }
      return watch(watched, (event, name) => {
        for (const waiter of eventWaiters.filter((w) => w.name === name)) {
          eventWaiters.splice(eventWaiters.indexOf(waiter), 1);
          waiter.resolve();
        }
        listener(event, name);
      });
    },
  });
  cleanups.push(() => {
    configFile.stopWatching();
    rmSync(dir, { recursive: true, force: true });
  });
  return {
    dir,
    file,
    controlPlane,
    configFile,
    runs,
    nextRun: (match) => new Promise((resolve) => waiters.push({ match, resolve })),
    nextWatchEvent: (name) => new Promise((resolve) => eventWaiters.push({ name, resolve })),
  };
}

const WATCH_PROBE = "watch-probe";
const WATCH_PROBE_INTERVAL_MS = 10;

// Starts the watcher and returns once its watch is live. fs.watch gives no signal that
// it is: on macOS FSEvents starts on its own thread a moment after watch() returns, and
// a change made before then is never reported — an edit made right away can go unseen
// for good. The watch is live once it reports a change, so an entry the watcher ignores
// is rewritten until the watch reports it; the probe itself causes no reload.
async function startWatchingLive(fixture: Fixture): Promise<void> {
  const seen = fixture.nextWatchEvent(WATCH_PROBE);
  fixture.configFile.startWatching();
  const probe = path.join(fixture.dir, WATCH_PROBE);
  writeFileSync(probe, "");
  const rewrite = setInterval(() => writeFileSync(probe, ""), WATCH_PROBE_INTERVAL_MS);
  try {
    await seen;
  } finally {
    clearInterval(rewrite);
  }
}

const publishedAs = (version: number) => (run: ReloadRun) =>
  run.trigger === WATCH_TRIGGER && run.ok && run.outcome === "published" && run.version === version;
const failedWith = (code: string) => (run: ReloadRun) =>
  run.trigger === WATCH_TRIGGER && !run.ok && run.error.code === code;

test("a reload publishes the file and records its trigger, time and result", async () => {
  const { configFile, controlPlane } = setUp(configText(1));
  const run = await configFile.reload("manual");
  assert.equal(run.ok, true);
  assert.equal(run.trigger, "manual");
  assert.equal(typeof run.at, "number");
  assert.ok(run.ok && run.outcome === "published" && run.version === 1);
  assert.equal((await controlPlane.currentConfig())?.version, 1);
  assert.deepEqual(configFile.state().lastRun, run);
  assert.equal(configFile.state().lastFailure, undefined);
});

test("reloading an unchanged file makes no new version", async () => {
  const { configFile, controlPlane } = setUp(configText(1));
  await configFile.reload("manual");
  const again = await configFile.reload("manual");
  assert.ok(again.ok && again.outcome === "unchanged" && again.version === 1);
  assert.equal((await controlPlane.currentConfig())?.version, 1);
});

test("each failure kind is rejected, kept, and cleared by the next good run", async () => {
  const { configFile, controlPlane, file } = setUp(undefined);

  const missing = await configFile.reload("manual");
  assert.ok(!missing.ok && missing.error.code === "file-unreadable");
  assert.equal(await controlPlane.currentConfig(), undefined);

  writeFileSync(file, configText(1));
  await configFile.reload("manual");

  writeFileSync(file, "{ not json");
  const badJson = await configFile.reload("manual");
  assert.ok(!badJson.ok && badJson.error.code === "json-invalid");

  writeFileSync(file, JSON.stringify({ format_version: 5 }));
  const invalid = await configFile.reload("manual");
  assert.ok(!invalid.ok && invalid.error.code === "config-invalid");
  assert.ok(invalid.error.code === "config-invalid" && invalid.error.issues.length > 0);
  assert.deepEqual(configFile.state().lastFailure, invalid);
  assert.equal((await controlPlane.currentConfig())?.version, 1, "the current version stays");

  // Back to the published document: nothing new to publish, but the failure is over.
  writeFileSync(file, configText(1));
  const fixed = await configFile.reload("manual");
  assert.ok(fixed.ok && fixed.outcome === "unchanged");
  assert.equal(configFile.state().lastFailure, undefined);
});

test("the watcher publishes an edit as a new version", { timeout: 5000 }, async () => {
  const fixture = setUp(configText(1));
  await startWatchingLive(fixture);
  await fixture.configFile.reload("startup");

  const published = fixture.nextRun(publishedAs(2));
  writeFileSync(fixture.file, configText(2));
  await published;
  const current = await fixture.controlPlane.currentConfig();
  assert.equal(current?.config.models["llama"]?.metadata.context_length, 1002);
});

test("the watcher sees an edit saved by renaming a new file over the old one", { timeout: 5000 }, async () => {
  const fixture = setUp(configText(1));
  await startWatchingLive(fixture);
  await fixture.configFile.reload("startup");

  for (const version of [2, 3]) {
    const published = fixture.nextRun(publishedAs(version));
    const temp = path.join(fixture.dir, `.config.json.${version}.tmp`);
    writeFileSync(temp, configText(version));
    renameSync(temp, fixture.file);
    await published;
  }
});

test("the watcher keeps an invalid edit out and exposes its error until a good edit", { timeout: 5000 }, async () => {
  const fixture = setUp(configText(1));
  await startWatchingLive(fixture);
  await fixture.configFile.reload("startup");

  const rejected = fixture.nextRun(failedWith("json-invalid"));
  writeFileSync(fixture.file, "{ broken");
  const run = await rejected;
  assert.deepEqual(fixture.configFile.state().lastFailure, run);
  assert.equal((await fixture.controlPlane.currentConfig())?.version, 1);

  const published = fixture.nextRun(publishedAs(2));
  writeFileSync(fixture.file, configText(2));
  await published;
  assert.equal(fixture.configFile.state().lastFailure, undefined);
});

// A Kubernetes ConfigMap volume: config.json -> ..data/config.json, ..data -> ..<stamp>/,
// and an update writes a new ..<stamp>/ directory, points a ..data_tmp symlink at it,
// renames that over ..data and removes the old directory.
test("the watcher sees a projected volume's atomic symlink swap and publishes it once", { timeout: 5000 }, async () => {
  const fixture = setUp(undefined);
  const project = (stamp: string, n: number): void => {
    mkdirSync(path.join(fixture.dir, stamp));
    writeFileSync(path.join(fixture.dir, stamp, "config.json"), configText(n));
  };
  project("..2026_09_25_00_00_00.1", 0);
  symlinkSync("..2026_09_25_00_00_00.1", path.join(fixture.dir, "..data"));
  symlinkSync("..data/config.json", fixture.file);
  await startWatchingLive(fixture);
  await fixture.configFile.reload("startup");
  assert.equal((await fixture.controlPlane.currentConfig())?.config.models["llama"]?.metadata.context_length, 1000);

  const published = fixture.nextRun(publishedAs(2));
  project("..2026_09_25_00_01_00.2", 1000);
  symlinkSync("..2026_09_25_00_01_00.2", path.join(fixture.dir, "..data_tmp"));
  renameSync(path.join(fixture.dir, "..data_tmp"), path.join(fixture.dir, "..data"));
  rmSync(path.join(fixture.dir, "..2026_09_25_00_00_00.1"), { recursive: true });
  await published;
  assert.equal((await fixture.controlPlane.currentConfig())?.config.models["llama"]?.metadata.context_length, 2000);

  // Every other run the swap's events caused found the content unchanged.
  fixture.configFile.stopWatching();
  const settled = await fixture.configFile.reload("check");
  assert.ok(settled.ok && settled.outcome === "unchanged" && settled.version === 2);
  assert.equal(fixture.runs.filter((run) => run.ok && run.outcome === "published").length, 2);
});

// The same swap with the events Linux (inotify) and macOS (FSEvents) report for it,
// delivered by hand: none names config.json, and the swap is published once.
test("the watcher reloads on the entries of a projected volume's swap", { timeout: 5000 }, async () => {
  let emit: WatchListener = () => assert.fail("not watching");
  const fixture = setUp(undefined, (listener) => (emit = listener));
  const stamp1 = path.join(fixture.dir, "..2026_09_25_00_00_00.1");
  const stamp2 = path.join(fixture.dir, "..2026_09_25_00_01_00.2");
  mkdirSync(stamp1);
  writeFileSync(path.join(stamp1, "config.json"), configText(0));
  symlinkSync(path.basename(stamp1), path.join(fixture.dir, "..data"));
  symlinkSync("..data/config.json", fixture.file);
  fixture.configFile.startWatching();
  await fixture.configFile.reload("startup");

  mkdirSync(stamp2);
  writeFileSync(path.join(stamp2, "config.json"), configText(1000));
  symlinkSync(path.basename(stamp2), path.join(fixture.dir, "..data_tmp"));
  renameSync(path.join(fixture.dir, "..data_tmp"), path.join(fixture.dir, "..data"));
  rmSync(stamp1, { recursive: true });
  const published = fixture.nextRun(publishedAs(2));
  for (const name of [path.basename(stamp2), "..data_tmp", "..data", path.basename(stamp1)]) emit("rename", name);
  await published;

  const unchanged = fixture.nextRun((run) => run.trigger === WATCH_TRIGGER);
  emit("rename", "..data");
  const again = await unchanged;
  assert.ok(again.ok && again.outcome === "unchanged" && again.version === 2);
});
