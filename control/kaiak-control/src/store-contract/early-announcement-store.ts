// A deliberately broken store for the contract tests' negative control: it announces a
// publish before the write is visible, as a store that notifies ahead of its commit, or
// reads the current config from a replica behind the primary, appears to its cores —
// a core that hears of the publish reads the config before it.

import { createMemoryStore } from "../storage/index.ts";
import type { ControlPlaneStore, StoreChangeListener } from "../storage/index.ts";

import { storeContractTests } from "./index.ts";

function earlyAnnouncementStore(store: ControlPlaneStore): ControlPlaneStore {
  const listeners = new Set<StoreChangeListener>();
  store.subscribe((change) => {
    // The publish was announced already, before its write.
    if (change.type === "config-published") return;
    for (const listener of [...listeners]) listener(change);
  });
  return {
    ...store,
    async publishConfig(entry, expectedHash) {
      if ((await store.currentConfig())?.hash === expectedHash) {
        for (const listener of [...listeners]) listener({ type: "config-published", hash: entry.hash });
        await new Promise((resolve) => setTimeout(resolve, 20));
      }
      return store.publishConfig(entry, expectedHash);
    },
    subscribe(listener) {
      const subscription: StoreChangeListener = (change) => listener(change);
      listeners.add(subscription);
      return () => {
        listeners.delete(subscription);
      };
    },
  };
}

storeContractTests({ name: "early announcement (expected to fail)", create: () => earlyAnnouncementStore(createMemoryStore()) });
