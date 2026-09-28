// Config versions (docs/specs/CONTROL-PROTOCOL.md, Config versions): publishing
// validates a document — and that no group changes parent from the current version —
// and stores it as the next version, subscribers hear of every published version, and
// a bounded history lets a stream resume from a version it already has.

import { validateConfig } from "../config/index.ts";
import type { Config, ConfigIssue } from "../config/index.ts";
import { pointer } from "../schemas/index.ts";
import type { ControlPlaneStore, StoredConfig } from "../storage/index.ts";

export type PublishResult = { ok: true; published: StoredConfig } | { ok: false; issues: ConfigIssue[] };

// Where a gateway stands: the config version it runs and the epoch that version
// counts in.
export interface ConfigPosition {
  epoch: string;
  version: number;
}

// What a stream resuming from a position gets: the store's epoch and the newer
// versions, oldest first (empty when it is current), or resync — fetch the snapshot
// again.
export type ConfigsSince = { resync: false; epoch: string; configs: StoredConfig[] } | { resync: true };

export type ConfigPublishedListener = (published: StoredConfig) => void;

// Hears of a listener that threw while being told of a published version.
export type ConfigListenerErrorHandler = (error: unknown, published: StoredConfig) => void;

export interface ConfigVersions {
  publishConfig(doc: unknown): Promise<PublishResult>;
  currentConfig(): Promise<StoredConfig | undefined>;
  // The store's config epoch (ControlPlaneStore.configEpoch).
  configEpoch(): Promise<string>;
  configsSince(position: ConfigPosition): Promise<ConfigsSince>;
  // Calls listener with every version published from now on; returns the unsubscribe,
  // which is safe to call more than once.
  onConfigPublished(listener: ConfigPublishedListener): () => void;
}

export interface ConfigVersionsOptions {
  store: ControlPlaneStore;
  // How many recent versions a stream can resume across.
  historySize: number;
  // Milliseconds since the epoch.
  clock: () => number;
  // Called for each listener that throws; the publish has succeeded either way.
  onListenerError: ConfigListenerErrorHandler;
}

export function createConfigVersions({
  store,
  historySize,
  clock,
  onListenerError,
}: ConfigVersionsOptions): ConfigVersions {
  if (!Number.isSafeInteger(historySize) || historySize < 1) {
    throw Object.assign(new Error(`config history size must be a positive integer, got ${historySize}`), {
      code: "config-history-size-invalid",
    });
  }

  const listeners = new Set<ConfigPublishedListener>();
  // Publishes run one at a time, so two cannot claim the same version.
  let publishing: Promise<unknown> = Promise.resolve();

  const publishOne = async (doc: unknown): Promise<PublishResult> => {
    const result = validateConfig(doc);
    if (!result.ok) return { ok: false, issues: result.issues };
    const latest = await store.latestConfig();
    const moved = latest ? parentChanges(latest.config, result.config) : [];
    if (moved.length > 0) return { ok: false, issues: moved };
    const published: StoredConfig = { version: (latest?.version ?? 0) + 1, config: result.config, publishedAt: clock() };
    await store.saveConfig(published, historySize);
    notify(published);
    return { ok: true, published };
  };

  // Every listener hears of the version even if an earlier one throws. A listener's
  // failure is its own: it goes to onListenerError and never fails the publish, which
  // has already stored the version.
  const notify = (published: StoredConfig): void => {
    for (const listener of [...listeners]) {
      try {
        listener(published);
      } catch (error) {
        onListenerError(error, published);
      }
    }
  };

  return {
    publishConfig(doc) {
      const run = publishing.then(() => publishOne(doc));
      publishing = run.catch(() => {
        // The caller gets this publish's failure from `run`; the queue only orders publishes.
      });
      return run;
    },

    currentConfig() {
      return store.latestConfig();
    },

    configEpoch() {
      return store.configEpoch();
    },

    async configsSince({ epoch: since, version }) {
      // A version from another epoch names a config of another store (or of this one
      // before it started over): its number says nothing here.
      const epoch = await store.configEpoch();
      if (since !== epoch) return { resync: true };
      const latest = await store.latestConfig();
      if (!latest || !Number.isSafeInteger(version) || version < 0 || version > latest.version) {
        return { resync: true };
      }
      if (version === latest.version) return { resync: false, epoch, configs: [] };
      if (latest.version - version > historySize) return { resync: true };
      const configs = await store.configsAfter(version);
      // Resuming needs every version after `version`, with none missing.
      const complete = configs[0]?.version === version + 1 && configs.length === latest.version - version;
      return complete ? { resync: false, epoch, configs } : { resync: true };
    },

    onConfigPublished(listener) {
      // A wrapper, so the same function subscribed twice is two subscriptions.
      const subscription: ConfigPublishedListener = (published) => listener(published);
      listeners.add(subscription);
      return () => {
        listeners.delete(subscription);
      };
    },
  };
}

// Parents never change (Config versions): a group the current version defines keeps
// its parent — top-level stays top-level. A group absent from the current version is
// new, whatever its parent, so a move is a delete in one publish and a create in a
// later one. Issues carry code "group-parent-changed", as validation issues do.
function parentChanges(current: Config, next: Config): ConfigIssue[] {
  const before = current.groups ?? {};
  const issues: ConfigIssue[] = [];
  for (const [id, group] of Object.entries(next.groups ?? {})) {
    const previous = Object.hasOwn(before, id) ? before[id] : undefined;
    if (previous === undefined || previous.parent === group.parent) continue;
    const path = pointer("/groups", id, "parent");
    const describe = (parent: string | undefined): string => (parent === undefined ? "top-level" : `under "${parent}"`);
    issues.push({
      code: "group-parent-changed",
      message: `${path}: group "${id}" is ${describe(previous.parent)} in the current version, ${describe(group.parent)} here; a move is a delete and a create`,
      path,
    });
  }
  return issues;
}
