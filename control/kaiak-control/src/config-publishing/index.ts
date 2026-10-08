// Config publishing (docs/specs/CONTROL-PROTOCOL.md, Current config): the app owns its
// config and hands the control plane whole documents; publishing validates one — and
// that no group changes parent from the current config — and makes it the current
// config: its JSON text, written once here, and the text's hash, replacing the one
// before. The control plane keeps no earlier configs. Subscribers hear of the current
// config after every change, whichever process published it.

import { createHash } from "node:crypto";

import { validateConfig } from "../config/index.ts";
import type { Config, ConfigIssue } from "../config/index.ts";
import { createListeners } from "../listeners/index.ts";
import { pointer } from "../schemas/index.ts";
import type { ConfigEntry, ControlPlaneStore, StoreChange } from "../storage/index.ts";

// The current config as the store holds it, with the document its text parses to.
export interface PublishedConfig extends ConfigEntry {
  config: Config;
}

// One read of the current config, with its place in the order this process issued
// config reads in: a stream sends a read only if none issued after it was sent there
// (CONTROL-PROTOCOL.md, Config stream → Order).
export interface ConfigRead {
  published: PublishedConfig;
  read: number;
}

export type PublishResult = { ok: true; published: PublishedConfig } | { ok: false; issues: ConfigIssue[] };

export type ConfigPublishedListener = (published: PublishedConfig) => void;

export type ConfigReadListener = (read: ConfigRead) => void;

// Hears of a listener that threw while being told of the current config.
export type ConfigListenerErrorHandler = (error: unknown, published: PublishedConfig) => void;

export interface ConfigPublishing {
  // Resolves once the config is current and this process's listeners have heard of it.
  publishConfig(doc: unknown): Promise<PublishResult>;
  currentConfig(): Promise<PublishedConfig | undefined>;
  // Reads the current config now, as a stream does on connect; undefined before the
  // first publish. Rejects when the store fails.
  readConfig(): Promise<ConfigRead | undefined>;
  // Calls listener with the current config read after every change the store
  // announced, by any process, and after every publish through this process — so the
  // same config may come more than once (a publish here is heard for its own read and
  // for the store's announcement). Returns the unsubscribe, which is safe to call more
  // than once.
  onConfigPublished(listener: ConfigPublishedListener): () => void;
  // The same reads, with their place in the read order, for a stream.
  onConfigRead(listener: ConfigReadListener): () => void;
  // Takes one change the store announced (the core passes every one on).
  takeChange(change: StoreChange): void;
}

export interface ConfigPublishingOptions {
  store: ControlPlaneStore;
  // Milliseconds since the epoch.
  clock: () => number;
  // Called for each listener that throws; the publish has succeeded either way.
  onListenerError: ConfigListenerErrorHandler;
  // Milliseconds to wait before each new read of the current config after a read that
  // failed; one read more than there are delays.
  retryDelaysMs: readonly number[];
  // Hears that the current config could not be read after every retry: this process's
  // listeners may not have it (their streams should end, so the gateways reconnect and
  // read it on connect).
  onDeliveryFailed: (error: unknown) => void;
}

// The config_hash: lowercase hex SHA-256 of the config's JSON text exactly as the
// control plane writes it (JSON.stringify) and sends it.
function hashOf(text: string): string {
  return createHash("sha256").update(text).digest("hex");
}

export function createConfigPublishing({
  store,
  clock,
  onListenerError,
  retryDelaysMs,
  onDeliveryFailed,
}: ConfigPublishingOptions): ConfigPublishing {
  const listeners = createListeners<ConfigRead>((error, read) => onListenerError(error, read.published));
  // Every read of the current config takes the next number when it is issued.
  let nextRead = 0;
  // Delivery to this process's listeners: the store announces a change, the current
  // config is read back and handed out. `deliveries` runs the reads one at a time, so
  // they are handed out in the order they were issued. Nothing is skipped here: each
  // stream skips a config it already sent.
  let deliveries: Promise<void> = Promise.resolve();

  const readOnce = async (): Promise<ConfigRead | undefined> => {
    const read = nextRead;
    nextRead += 1;
    const entry = await store.currentConfig();
    return entry && { published: { ...entry, config: JSON.parse(entry.text) as Config }, read };
  };

  // A read that fails is tried again after each retry delay; one that still fails is
  // announced (onDeliveryFailed), never dropped quietly.
  const readCurrent = async (): Promise<ConfigRead | undefined> => {
    for (let attempt = 0; ; attempt += 1) {
      try {
        return await readOnce();
      } catch (error) {
        const delay = retryDelaysMs[attempt];
        if (delay === undefined) {
          onDeliveryFailed(error);
          return undefined;
        }
        await new Promise((resolve) => setTimeout(resolve, delay));
      }
    }
  };

  // A run fails only when onDeliveryFailed throws (the core's reports a throwing
  // listener through onListenerError instead); the chain goes on past a failed run
  // either way, so no run stops the ones after it.
  const deliverCurrent = (): Promise<void> => {
    const run = deliveries.then(async () => {
      const read = await readCurrent();
      if (read) listeners.emit(read);
    });
    deliveries = run.catch(() => {
      // The failure is the caller's, through `run`; the chain only orders the reads.
    });
    return run;
  };

  // A publish by any process, or a store catching up after its channel was down, is
  // followed by reading the current config.
  const takeChange = (change: StoreChange): void => {
    if (change.type === "config-published" || change.type === "catch-up") void deliverCurrent();
  };

  const publishOne = async (doc: unknown): Promise<PublishResult> => {
    const result = validateConfig(doc);
    if (!result.ok) return { ok: false, issues: result.issues };
    // The text is taken once, before the first wait; the document the rest of the
    // publish checks and returns is that text's, so a caller changing its object while
    // the publish waits on the store changes nothing of it.
    const text = JSON.stringify(result.config);
    const hash = hashOf(text);
    const config = JSON.parse(text) as Config;
    // A publish that lost to another process's publish is checked again against the
    // config that won, so the parents rule holds against what is really current.
    for (;;) {
      const current = await store.currentConfig();
      const moved = current ? parentChanges(JSON.parse(current.text) as Config, config) : [];
      if (moved.length > 0) return { ok: false, issues: moved };
      const entry: ConfigEntry = { text, hash, publishedAt: clock() };
      const written = await store.publishConfig(entry, current?.hash);
      if (!written.saved) continue;
      // The config is current: the publish succeeded whether or not this process's
      // listeners could be given it (a failed read is announced).
      await deliverCurrent();
      return { ok: true, published: { ...entry, config } };
    }
  };

  return {
    publishConfig: publishOne,

    async currentConfig() {
      return (await readOnce())?.published;
    },

    readConfig: readOnce,

    onConfigPublished: (listener) => listeners.add((read) => listener(read.published)),

    onConfigRead: listeners.add,

    takeChange,
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
