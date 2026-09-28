// The sample's config source: one JSON file, read and published as a new config
// version. Reloading is an invocable run (startup, the file watcher and tests are its
// triggers); every run records its trigger, time and result. A file that fails to
// read, parse or validate is rejected: the current version stays, and the failure is
// kept until a later run succeeds.

import { watch } from "node:fs";
import type { FSWatcher } from "node:fs";
import { readFile } from "node:fs/promises";
import path from "node:path";

import type { ConfigIssue, ControlPlane } from "kaiak-control";

export type ConfigFileError =
  | { code: "file-unreadable"; message: string }
  | { code: "json-invalid"; message: string }
  | { code: "config-invalid"; message: string; issues: ConfigIssue[] };

// "unchanged": the file holds the document last published from it, so no new version
// is made — editors and `touch` rewrite files without changing them.
export type ReloadRun =
  | { trigger: string; at: number; ok: true; outcome: "published" | "unchanged"; version: number }
  | { trigger: string; at: number; ok: false; error: ConfigFileError };

export type FailedReloadRun = ReloadRun & { ok: false };

export interface ConfigFileState {
  path: string;
  lastRun: ReloadRun | undefined;
  // The latest failed run, cleared by the next successful one.
  lastFailure: FailedReloadRun | undefined;
}

export interface ConfigFile {
  // Reads the file and publishes it. Runs one at a time, in call order.
  reload(trigger: string): Promise<ReloadRun>;
  state(): ConfigFileState;
  // Reloads on every change to the file, debounced. Watching twice keeps one watcher.
  // On macOS the watch goes live a moment after this returns (FSEvents starts on its
  // own thread), and a change within that moment is not reported; on Linux (inotify)
  // it is live on return.
  startWatching(): void;
  stopWatching(): void;
}

export interface ConfigFileOptions {
  path: string;
  controlPlane: Pick<ControlPlane, "publishConfig">;
  // Hears of every run, for the host to log.
  onReload: (run: ReloadRun) => void;
  // Hears of the watcher failing; reload keeps working when invoked.
  onWatchError: (error: unknown) => void;
  // Quiet time after the last change event before the watcher reloads. Default 200.
  debounceMs?: number;
  // Milliseconds since the epoch. Default Date.now.
  clock?: () => number;
  // Watches a directory, calling back with each change and the entry's name when the
  // platform reports it. Default fs.watch.
  watchDirectory?: (dir: string, listener: (event: string, name: string | null) => void) => FSWatcher;
}

const DEFAULT_DEBOUNCE_MS = 200;
export const WATCH_TRIGGER = "file-changed";

export function createConfigFile(options: ConfigFileOptions): ConfigFile {
  const {
    controlPlane,
    onReload,
    onWatchError,
    debounceMs = DEFAULT_DEBOUNCE_MS,
    clock = Date.now,
    watchDirectory = (dir, listener) => watch(dir, listener),
  } = options;
  const filePath = path.resolve(options.path);
  let lastRun: ReloadRun | undefined;
  let lastFailure: FailedReloadRun | undefined;
  // The document last published from the file (as JSON text) and its version.
  let lastPublished: { text: string; version: number } | undefined;
  let queue: Promise<unknown> = Promise.resolve();
  let watcher: FSWatcher | undefined;
  let debounce: NodeJS.Timeout | undefined;

  const runOnce = async (trigger: string): Promise<ReloadRun> => {
    const failed = (error: ConfigFileError): ReloadRun => ({ trigger, at: clock(), ok: false, error });
    let text: string;
    try {
      text = await readFile(filePath, "utf8");
    } catch (error) {
      return failed({ code: "file-unreadable", message: messageOf(error) });
    }
    let doc: unknown;
    try {
      doc = JSON.parse(text);
    } catch (error) {
      return failed({ code: "json-invalid", message: messageOf(error) });
    }
    const canonical = JSON.stringify(doc);
    if (lastPublished?.text === canonical) {
      return { trigger, at: clock(), ok: true, outcome: "unchanged", version: lastPublished.version };
    }
    const result = await controlPlane.publishConfig(doc);
    if (!result.ok) {
      const count = result.issues.length;
      return failed({ code: "config-invalid", message: `${count} issue${count === 1 ? "" : "s"}`, issues: result.issues });
    }
    lastPublished = { text: canonical, version: result.published.version };
    return { trigger, at: clock(), ok: true, outcome: "published", version: result.published.version };
  };

  const record = (run: ReloadRun): ReloadRun => {
    lastRun = run;
    lastFailure = run.ok ? undefined : run;
    onReload(run);
    return run;
  };

  const reload = (trigger: string): Promise<ReloadRun> => {
    const run = queue.then(() => runOnce(trigger)).then(record);
    queue = run.catch(() => {
      // The caller gets this run's failure from `run`; the queue only orders runs.
    });
    return run;
  };

  const scheduleReload = (): void => {
    clearTimeout(debounce);
    debounce = setTimeout(() => {
      debounce = undefined;
      reload(WATCH_TRIGGER).catch(onWatchError);
    }, debounceMs);
  };

  return {
    reload,

    state() {
      return { path: filePath, lastRun, lastFailure };
    },

    startWatching() {
      if (watcher) return;
      // The directory, not the file: editors save by writing a new file and renaming
      // it over the old one, and a watch on the old file's inode would see nothing
      // after the first such save.
      const name = path.basename(filePath);
      watcher = watchDirectory(path.dirname(filePath), (_event, changed) => {
        if (isRelevant(changed, name)) scheduleReload();
      });
      watcher.on("error", onWatchError);
    },

    stopWatching() {
      clearTimeout(debounce);
      debounce = undefined;
      watcher?.close();
      watcher = undefined;
    },
  };
}

// Changes that may change what the file reads as. Some platforms do not report the
// name. A Kubernetes ConfigMap volume projects the file as a symlink into `..data`,
// itself a symlink it replaces atomically with one to a new `..<timestamp>` directory:
// the file's own entry never changes, and every entry of the swap starts with "..".
// A reload that finds the content unchanged publishes nothing.
function isRelevant(changed: string | null, name: string): boolean {
  return changed === null || changed === name || changed.startsWith("..");
}

function messageOf(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}
