// A change channel that can drop changes, over the in-memory store: the shape of a
// database's listening connection, so the catch-up contract test runs in this repo
// (lossy-channel.test.ts). While the channel is down, every change is missed; when it
// comes back, it announces a `catch-up` to its subscribers. The `announce` option breaks
// that on purpose, for the negative controls (negative-control.test.ts): a channel that
// comes back silently, or that tells only its first subscriber.

import { createMemoryStore } from "../storage/index.ts";
import type { ControlPlaneStore, StoreChange, StoreChangeListener } from "../storage/index.ts";

import type { StoreContractSubject } from "./index.ts";

export type CatchUpAnnouncement = "every-subscriber" | "first-subscriber" | "none";

interface LossyHandle {
  handle: ControlPlaneStore;
  reconnect(whileDown: () => Promise<void>): Promise<void>;
}

// Another handle on `store` whose change channel can be taken down.
function lossyHandle(store: ControlPlaneStore, announce: CatchUpAnnouncement): LossyHandle {
  const listeners = new Set<StoreChangeListener>();
  let up = true;
  const call = (listener: StoreChangeListener, change: StoreChange): void => {
    try {
      listener(change);
    } catch (error) {
      queueMicrotask(() => {
        throw error;
      });
    }
  };
  return {
    handle: {
      ...store,
      subscribe(listener) {
        const subscription: StoreChangeListener = (change) => listener(change);
        listeners.add(subscription);
        const unsubscribe = store.subscribe((change) => {
          if (up) call(subscription, change);
        });
        return () => {
          listeners.delete(subscription);
          unsubscribe();
        };
      },
    },
    async reconnect(whileDown) {
      up = false;
      try {
        await whileDown();
      } finally {
        up = true;
      }
      if (announce === "none") return;
      const told = announce === "first-subscriber" ? [...listeners].slice(0, 1) : [...listeners];
      for (const listener of told) call(listener, { type: "catch-up" });
    },
  };
}

// The contract subject: the in-memory store, with the second handle on a lossy channel.
export function lossyChannelSubject(name: string, announce: CatchUpAnnouncement = "every-subscriber"): StoreContractSubject {
  const handles = new WeakMap<ControlPlaneStore, LossyHandle>();
  return {
    name,
    create: createMemoryStore,
    attach(store) {
      const lossy = lossyHandle(store, announce);
      handles.set(lossy.handle, lossy);
      return lossy.handle;
    },
    async reconnect(handle, whileDown) {
      const lossy = handles.get(handle);
      if (!lossy) throw new Error("not a lossy-channel handle");
      await lossy.reconnect(whileDown);
    },
  };
}
