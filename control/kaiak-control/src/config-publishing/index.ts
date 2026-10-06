// Config publishing (docs/specs/CONTROL-PROTOCOL.md, Current config): the app owns its
// config and hands the control plane whole documents; publishing validates one — and
// that no group changes parent from the current config — and makes it the current
// config with its content hash, replacing the one before. The control plane keeps no
// earlier configs. Subscribers hear of the current config whenever it changes,
// whichever process published it.

import { createHash } from "node:crypto";

import { validateConfig } from "../config/index.ts";
import type { Config, ConfigIssue } from "../config/index.ts";
import { pointer } from "../schemas/index.ts";
import type { ControlPlaneStore, CurrentConfig } from "../storage/index.ts";

export type PublishResult = { ok: true; published: CurrentConfig } | { ok: false; issues: ConfigIssue[] };

export type ConfigPublishedListener = (published: CurrentConfig) => void;

// Hears of a listener that threw while being told of the current config.
export type ConfigListenerErrorHandler = (error: unknown, published: CurrentConfig) => void;

export interface ConfigPublishing {
  // Resolves once the config is current and this process's listeners have heard of it.
  publishConfig(doc: unknown): Promise<PublishResult>;
  currentConfig(): Promise<CurrentConfig | undefined>;
  // Calls listener with the current config every time it changes, by any process;
  // returns the unsubscribe, which is safe to call more than once.
  onConfigPublished(listener: ConfigPublishedListener): () => void;
}

export interface ConfigPublishingOptions {
  store: ControlPlaneStore;
  // Milliseconds since the epoch.
  clock: () => number;
  // Hears of every store sequence this module reads (CONTROL-PROTOCOL.md, Config stream
  // → Rollback).
  observeSequence: (sequence: number) => void;
  // Called for each listener that throws; the publish has succeeded either way.
  onListenerError: ConfigListenerErrorHandler;
}

// The config_hash: lowercase hex SHA-256 of the config's JSON text exactly as the
// control plane sends it (JSON.stringify, as every event is written).
export function configHash(config: Config): string {
  return createHash("sha256").update(JSON.stringify(config)).digest("hex");
}

export function createConfigPublishing({
  store,
  clock,
  observeSequence,
  onListenerError,
}: ConfigPublishingOptions): ConfigPublishing {
  const listeners = new Set<ConfigPublishedListener>();
  // Delivery to this process's listeners: the store announces a change, the current
  // config is read back and handed out. `deliveries` runs the reads one at a time, so a
  // slow read never hands out a config after a newer one; `delivered` is the hash last
  // handed out, so a config read twice is handed out once. A hash carries no order, so
  // a store restored to an older config hands that config out like any other.
  let delivered: string | undefined;
  let deliveries: Promise<unknown> = Promise.resolve();

  const notify = (published: CurrentConfig): void => {
    for (const listener of [...listeners]) {
      try {
        listener(published);
      } catch (error) {
        onListenerError(error, published);
      }
    }
  };

  const deliverCurrent = (): Promise<void> => {
    const run = deliveries.then(async () => {
      const current = await store.currentConfig();
      if (!current) return;
      observeSequence(current.sequence);
      if (current.hash === delivered) return;
      delivered = current.hash;
      notify(current);
    });
    deliveries = run.catch(() => {
      // The caller gets the failure from `run`; the chain only orders the reads.
    });
    return run;
  };

  store.subscribe((change) => {
    if (change.type !== "config-published") return;
    void deliverCurrent().catch(() => {
      // The next change announced reads the current config again.
    });
  });

  const publishOne = async (doc: unknown): Promise<PublishResult> => {
    const result = validateConfig(doc);
    if (!result.ok) return { ok: false, issues: result.issues };
    const hash = configHash(result.config);
    // A publish that lost to another process's publish is checked again against the
    // config that won, so the parents rule holds against what is really current.
    for (;;) {
      const current = await store.currentConfig();
      const moved = current ? parentChanges(current.config, result.config) : [];
      if (moved.length > 0) return { ok: false, issues: moved };
      const entry = { config: result.config, hash, publishedAt: clock() };
      const written = await store.publishConfig(entry, current?.hash);
      if (!written.saved) continue;
      await deliverCurrent().catch(() => {
        // The config is current: the publish succeeded. A store that failed to read it
        // back leaves this process's listeners to hear of it with the next change, and
        // its streams to get it when they reconnect.
      });
      return { ok: true, published: { ...entry, sequence: written.sequence } };
    }
  };

  return {
    publishConfig: publishOne,

    currentConfig() {
      return store.currentConfig();
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

// Parents never change (Current config): a group the current config defines keeps its
// parent — top-level stays top-level. A group absent from the current config is new,
// whatever its parent, so a move is a delete in one publish and a create in a later
// one. Issues carry code "group-parent-changed", as validation issues do.
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
      message: `${path}: group "${id}" is ${describe(previous.parent)} in the current config, ${describe(group.parent)} here; a move is a delete and a create`,
      path,
    });
  }
  return issues;
}
