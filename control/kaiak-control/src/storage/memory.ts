// The in-memory store: state lives as long as the process. Several cores of one process
// may share one store; every write below
// runs synchronously from its comparison to its notification, so no other call sees
// part of a write or writes between the comparison and the write.

import { createListeners } from "../listeners/index.ts";
import type { BatchId } from "../messages/index.ts";

import { windowKeyOf } from "./window-key.ts";
import type {
  BatchCursor,
  ConfigEntry,
  ControlPlaneStore,
  ReceivedRecord,
  StoreChange,
  StoredGateway,
  WindowStarts,
  WindowTotal,
} from "./types.ts";

export function createMemoryStore(): ControlPlaneStore {
  // The current config, replaced by every publish.
  let config: ConfigEntry | undefined;
  // The batch cursors, by instance and epoch.
  const cursors = new Map<string, Map<string, BatchCursor>>();
  const totals = new Map<string, WindowTotal>();
  // Oldest first; the last entry is the newest record.
  const records: ReceivedRecord[] = [];
  const gateways = new Map<string, StoredGateway>();
  // Numbers every gateway write of the store, so a revision never repeats for an
  // instance, a forgotten and recreated one included.
  let gatewayWrites = 0;
  // Every listener hears of the change even if an earlier one throws; a listener's
  // failure is rethrown from a microtask, the way a throwing event listener surfaces.
  const listeners = createListeners<StoreChange>((error) => {
    queueMicrotask(() => {
      throw error;
    });
  });

  const addTotals = (additions: WindowTotal[]): void => {
    for (const addition of additions) {
      const key = windowKeyOf(addition);
      const stored = totals.get(key);
      if (stored) stored.used += addition.used;
      else totals.set(key, structuredClone(addition));
    }
  };

  const isCurrent = (total: WindowTotal, current: WindowStarts): boolean => total.windowStart === current[total.type];

  const cursorsOf = (instance: string): BatchCursor[] => structuredClone([...(cursors.get(instance)?.values() ?? [])]);

  return {
    async currentConfig() {
      return config && { ...config };
    },

    async publishConfig(entry, expectedHash) {
      if (config?.hash !== expectedHash) return { saved: false, current: config && { ...config } };
      config = { ...entry };
      listeners.emit({ type: "config-published", hash: entry.hash });
      return { saved: true };
    },

    async lastBatches(instance) {
      return cursorsOf(instance);
    },

    async saveCountedBatch(counted, expectedLast, keepRecords) {
      const { instance, epoch } = counted.batch;
      const epochs = cursors.get(instance) ?? new Map<string, BatchCursor>();
      if (!sameBatch(epochs.get(epoch)?.batch, expectedLast)) return { saved: false, cursors: cursorsOf(instance) };
      addTotals(counted.additions);
      records.push(...structuredClone(counted.records));
      if (records.length > keepRecords) records.splice(0, records.length - keepRecords);
      epochs.set(epoch, { batch: { ...counted.batch }, countedAt: counted.countedAt });
      cursors.set(instance, epochs);
      listeners.emit({ type: "batch-counted", instance: counted.batch.instance });
      return { saved: true };
    },

    async totalsSnapshot(current) {
      return {
        windows: [...totals.values()].filter((total) => isCurrent(total, current)).map((total) => structuredClone(total)),
        cursors: [...cursors.values()].flatMap((epochs) => [...epochs.values()].map(({ batch }) => ({ ...batch }))),
      };
    },

    async dropPastWindowTotals(oldest) {
      for (const [key, total] of totals) {
        if (total.windowStart < oldest[total.type]) totals.delete(key);
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
      gatewayWrites += 1;
      const revision = gatewayWrites;
      gateways.set(record.instance, { ...structuredClone(record), revision });
      listeners.emit({ type: "gateways-changed", liveChanged: (stored?.live ?? false) !== record.live });
      return { saved: true, revision };
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
      listeners.emit({ type: "gateways-changed", liveChanged });
      return forgotten;
    },

    async dropBatchCursorsCountedBefore(cutoff) {
      const dropped: string[] = [];
      for (const [instance, epochs] of cursors) {
        let any = false;
        for (const [epoch, cursor] of epochs) {
          if (cursor.countedAt < cutoff) {
            epochs.delete(epoch);
            any = true;
          }
        }
        if (epochs.size === 0) cursors.delete(instance);
        if (any) dropped.push(instance);
      }
      return dropped;
    },

    subscribe(listener) {
      // Each listener gets its own copy of the change.
      return listeners.add((change) => listener({ ...change }));
    },
  };
}

function sameBatch(a: BatchId | undefined, b: BatchId | undefined): boolean {
  if (a === undefined || b === undefined) return a === b;
  return a.instance === b.instance && a.epoch === b.epoch && a.sequence === b.sequence;
}
