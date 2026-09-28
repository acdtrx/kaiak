// The in-memory store: state lives as long as the process, so each process's store has
// its own config epoch.

import { randomBytes } from "node:crypto";

import type { BatchId } from "../messages/index.ts";

import type {
  ControlPlaneStore,
  CurrentWindows,
  ReceivedRecord,
  StoredConfig,
  StoreLease,
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
  let lease: StoreLease | undefined;
  const totals = new Map<string, WindowTotal>();
  // Oldest first; the last entry is the newest record.
  const records: ReceivedRecord[] = [];
  const gateways = new Map<string, StoredGateway>();

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

  return {
    async configEpoch() {
      return epoch;
    },

    async latestConfig() {
      return configs.at(-1);
    },

    async saveConfig(entry, keep) {
      // A copy, so a caller changing its document afterwards cannot change what is stored.
      configs.push(structuredClone(entry));
      if (configs.length > keep) configs.splice(0, configs.length - keep);
    },

    async configsAfter(version) {
      return configs.filter((entry) => entry.version > version);
    },

    async acquireLease(holder, now, expiresAt) {
      if (lease && lease.holder !== holder && lease.expiresAt > now) return { ok: false, lease: { ...lease } };
      lease = { holder, expiresAt };
      return { ok: true };
    },

    async releaseLease(holder) {
      if (lease?.holder === holder) lease = undefined;
    },

    async lastBatch(instance) {
      const last = lastBatches.get(instance);
      return last && { ...last.batch };
    },

    // Synchronous from start to end, so no other call sees part of a batch or writes
    // between the comparison and the write.
    async saveCountedBatch(counted, expectedLast, keepRecords) {
      const last = lastBatches.get(counted.batch.instance)?.batch;
      if (!sameBatch(last, expectedLast)) return { saved: false, last: last && { ...last } };
      lastBatches.set(counted.batch.instance, { batch: { ...counted.batch }, countedAt: counted.countedAt });
      addTotals(counted.additions);
      records.push(...structuredClone(counted.records));
      if (records.length > keepRecords) records.splice(0, records.length - keepRecords);
      return { saved: true };
    },

    async addWindowTotals(additions) {
      addTotals(additions);
    },

    async currentWindowTotals(current) {
      return [...totals.values()].filter((total) => isCurrent(total, current)).map((total) => structuredClone(total));
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

    async saveGateway(gateway) {
      gateways.set(gateway.instance, structuredClone(gateway));
    },

    async deleteGateways(instances) {
      for (const instance of instances) gateways.delete(instance);
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
