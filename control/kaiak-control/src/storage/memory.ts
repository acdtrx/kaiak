// The in-memory store: state lives as long as the process. Several cores of one process
// may share one store; every write below
// runs synchronously from its comparison to its notification, so no other call sees
// part of a write or writes between the comparison and the write.

import type { BatchId } from "../messages/index.ts";

import type {
  BatchCursors,
  ConfigEntry,
  ControlPlaneStore,
  CurrentWindows,
  ReceivedRecord,
  StoreChange,
  StoreChangeListener,
  StoredGateway,
  WindowKey,
  WindowTotal,
} from "./types.ts";

export function createMemoryStore(): ControlPlaneStore {
  // The current config, replaced by every publish.
  let config: ConfigEntry | undefined;
  // The last counted batch per instance and epoch, and when it was counted.
  const lastBatches = new Map<string, Map<string, { batch: BatchId; countedAt: number }>>();
  const totals = new Map<string, WindowTotal>();
  // Oldest first; the last entry is the newest record.
  const records: ReceivedRecord[] = [];
  const gateways = new Map<string, StoredGateway>();
  // Numbers every gateway write of the store, so a revision never repeats for an
  // instance, a forgotten and recreated one included.
  let gatewayWrites = 0;
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

  const cursorsOf = (instance: string, epoch: string): BatchCursors => {
    const epochs = lastBatches.get(instance);
    const inEpoch = epochs?.get(epoch)?.batch;
    // The epoch counted last; of two counted at one instant, the one counted later
    // (a Map iterates in insertion order, and a write moves its epoch to the end).
    let latest: { batch: BatchId; countedAt: number } | undefined;
    for (const cursor of epochs?.values() ?? []) if (!latest || cursor.countedAt >= latest.countedAt) latest = cursor;
    return { inEpoch: inEpoch && { ...inEpoch }, latest: latest && { ...latest.batch } };
  };

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
    async currentConfig() {
      return config && { ...config };
    },

    async publishConfig(entry, expectedHash) {
      if (config?.hash !== expectedHash) return { saved: false, current: config && { ...config } };
      config = { ...entry };
      notify({ type: "config-published", hash: entry.hash });
      return { saved: true };
    },

    async lastBatch(instance, epoch) {
      return cursorsOf(instance, epoch);
    },

    async saveCountedBatch(counted, expectedLast, keepRecords) {
      const { instance, epoch } = counted.batch;
      const cursors = cursorsOf(instance, epoch);
      if (!sameBatch(cursors.inEpoch, expectedLast)) return { saved: false, cursors };
      addTotals(counted.additions);
      records.push(...structuredClone(counted.records));
      if (records.length > keepRecords) records.splice(0, records.length - keepRecords);
      const epochs = lastBatches.get(instance) ?? new Map();
      epochs.delete(epoch);
      epochs.set(epoch, { batch: { ...counted.batch }, countedAt: counted.countedAt });
      lastBatches.set(instance, epochs);
      notify({ type: "batch-counted", instance: counted.batch.instance });
      return { saved: true };
    },

    async totalsSnapshot(current) {
      return {
        windows: [...totals.values()].filter((total) => isCurrent(total, current)).map((total) => structuredClone(total)),
        cursors: [...lastBatches.values()].flatMap((epochs) => [...epochs.values()].map(({ batch }) => ({ ...batch }))),
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
      gatewayWrites += 1;
      const revision = gatewayWrites;
      gateways.set(record.instance, { ...structuredClone(record), revision });
      notify({ type: "gateways-changed", liveChanged: (stored?.live ?? false) !== record.live });
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
      notify({ type: "gateways-changed", liveChanged });
      return forgotten;
    },

    async dropBatchCursorsCountedBefore(cutoff) {
      const dropped: string[] = [];
      for (const [instance, epochs] of lastBatches) {
        let any = false;
        for (const [epoch, cursor] of epochs) {
          if (cursor.countedAt < cutoff) {
            epochs.delete(epoch);
            any = true;
          }
        }
        if (epochs.size === 0) lastBatches.delete(instance);
        if (any) dropped.push(instance);
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

function windowKey({ group, type, windowStart }: WindowKey): string {
  return JSON.stringify([group ?? null, type, windowStart]);
}
