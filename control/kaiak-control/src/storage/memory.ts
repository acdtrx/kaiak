// The in-memory store: state lives as long as the process, so each store has its own
// config epoch. Several cores of one process may share one store; every write below
// runs synchronously from its comparison to its notification, so no other call sees
// part of a write or writes between the comparison and the write.

import { randomBytes } from "node:crypto";

import type { BatchId } from "../messages/index.ts";

import type {
  ControlPlaneStore,
  CurrentWindows,
  ReceivedRecord,
  StoreChange,
  StoreChangeListener,
  StoredConfig,
  StoredGateway,
  WindowKey,
  WindowTotal,
} from "./types.ts";

export function createMemoryStore(): ControlPlaneStore {
  const epoch = randomBytes(16).toString("hex");
  // Oldest first; the last entry is the newest version.
  const configs: StoredConfig[] = [];
  // The last counted batch per instance and when it was counted.
  const lastBatches = new Map<string, { batch: BatchId; countedAt: number }>();
  const totals = new Map<string, WindowTotal>();
  let sequence = 0;
  // Oldest first; the last entry is the newest record.
  const records: ReceivedRecord[] = [];
  const gateways = new Map<string, StoredGateway>();
  const listeners = new Set<StoreChangeListener>();

  const addTotals = (additions: WindowTotal[]): void => {
    for (const addition of additions) {
      const key = windowKey(addition);
      const stored = totals.get(key);
      if (stored) stored.used += addition.used;
      else totals.set(key, structuredClone(addition));
    }
  };

  const isCurrent = (total: WindowTotal, current: CurrentWindows): boolean =>
    total.windowStart === (total.type === "tokens_per_hour" ? current.hourStart : current.monthStart);

  const latestVersion = (): number | undefined => configs.at(-1)?.version;

  const liveCount = (): number => [...gateways.values()].filter((gateway) => gateway.live).length;

  // Every listener hears of the change even if an earlier one throws; a listener's
  // failure is rethrown from a microtask, the way a throwing event listener surfaces.
  const notify = (change: StoreChange): void => {
    for (const listener of [...listeners]) {
      try {
        listener({ ...change });
      } catch (error) {
        queueMicrotask(() => {
          throw error;
        });
      }
    }
  };

  return {
    async configEpoch() {
      return epoch;
    },

    async latestConfig() {
      return structuredClone(configs.at(-1));
    },

    async publishConfig({ entry, carried }, expected, keep) {
      if (latestVersion() !== expected.version || sequence !== expected.sequence) {
        return { saved: false, latestVersion: latestVersion(), sequence };
      }
      if (entry.version !== (expected.version ?? 0) + 1) {
        throw Object.assign(new Error(`version ${entry.version} does not follow ${expected.version ?? "none"}`), {
          code: "config-version-out-of-order",
        });
      }
      // A copy, so a caller changing its document afterwards cannot change what is stored.
      configs.push(structuredClone(entry));
      if (configs.length > keep) configs.splice(0, configs.length - keep);
      addTotals(carried);
      sequence += 1;
      notify({ type: "config-published", version: entry.version, sequence });
      return { saved: true, sequence };
    },

    async configsAfter(version) {
      return structuredClone(configs.filter((entry) => entry.version > version));
    },

    async lastBatch(instance) {
      const last = lastBatches.get(instance);
      return last && { ...last.batch };
    },

    async saveCountedBatch(counted, expectedLast, keepRecords) {
      const last = lastBatches.get(counted.batch.instance)?.batch;
      if (!sameBatch(last, expectedLast) || latestVersion() !== counted.configVersion) {
        return { saved: false, last: last && { ...last }, configVersion: latestVersion() };
      }
      lastBatches.set(counted.batch.instance, { batch: { ...counted.batch }, countedAt: counted.countedAt });
      addTotals(counted.additions);
      records.push(...structuredClone(counted.records));
      if (records.length > keepRecords) records.splice(0, records.length - keepRecords);
      sequence += 1;
      notify({ type: "batch-counted", instance: counted.batch.instance, sequence });
      return { saved: true, sequence };
    },

    async totalsSnapshot(current, instance) {
      const last = instance === undefined ? undefined : lastBatches.get(instance)?.batch;
      return {
        sequence,
        config: structuredClone(configs.at(-1)),
        last: last && { ...last },
        windows: [...totals.values()].filter((total) => isCurrent(total, current)).map((total) => structuredClone(total)),
        liveGateways: liveCount(),
      };
    },

    async dropPastWindowTotals(oldest) {
      for (const [key, total] of totals) {
        const start = total.type === "tokens_per_hour" ? oldest.hourStart : oldest.monthStart;
        if (total.windowStart < start) totals.delete(key);
      }
    },

    async recentRecords(limit) {
      if (limit <= 0) return [];
      return structuredClone(records.slice(-limit).reverse());
    },

    async gateway(instance) {
      const gateway = gateways.get(instance);
      return gateway && structuredClone(gateway);
    },

    async gateways() {
      return structuredClone([...gateways.values()]);
    },

    async saveGateway(record, expectedRevision) {
      const stored = gateways.get(record.instance);
      if (stored?.revision !== expectedRevision) return { saved: false, current: stored && structuredClone(stored) };
      const revision = (stored?.revision ?? 0) + 1;
      gateways.set(record.instance, { ...structuredClone(record), revision });
      const liveChanged = (stored?.live ?? false) !== record.live;
      if (liveChanged) sequence += 1;
      notify({ type: "gateways-changed", liveChanged, sequence });
      return { saved: true, revision, sequence };
    },

    async forgetGateways(forget) {
      const forgotten: string[] = [];
      let liveChanged = false;
      for (const { instance, revision } of forget) {
        const stored = gateways.get(instance);
        if (stored?.revision !== revision) continue;
        gateways.delete(instance);
        forgotten.push(instance);
        if (stored.live) liveChanged = true;
      }
      if (forgotten.length === 0) return [];
      if (liveChanged) sequence += 1;
      notify({ type: "gateways-changed", liveChanged, sequence });
      return forgotten;
    },

    async dropBatchCursorsCountedBefore(cutoff) {
      const dropped: string[] = [];
      for (const [instance, last] of lastBatches) {
        if (last.countedAt < cutoff) {
          lastBatches.delete(instance);
          dropped.push(instance);
        }
      }
      return dropped;
    },

    subscribe(listener) {
      // A wrapper, so the same function subscribed twice is two subscriptions.
      const subscription: StoreChangeListener = (change) => listener(change);
      listeners.add(subscription);
      return () => {
        listeners.delete(subscription);
      };
    },
  };
}

function sameBatch(a: BatchId | undefined, b: BatchId | undefined): boolean {
  if (a === undefined || b === undefined) return a === b;
  return a.instance === b.instance && a.epoch === b.epoch && a.sequence === b.sequence;
}

// Model sets arrive sorted (WindowKey), so equal sets give equal keys.
function windowKey({ group, type, models, windowStart }: WindowKey): string {
  return JSON.stringify([group ?? null, type, models ?? null, windowStart]);
}
