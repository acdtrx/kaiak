// A deliberately broken store for the contract tests' negative control
// (negative-control.test.ts runs the contract against it in a child process and expects
// it to fail): it reads the recipient's batch cursor outside the totals snapshot, a
// common database mistake — a batch counted between the two reads is named counted
// while the windows lack it.

import { createMemoryStore } from "../storage/index.ts";
import type { ControlPlaneStore } from "../storage/index.ts";

import { storeContractTests } from "./index.ts";

function tornCursorStore(store: ControlPlaneStore): ControlPlaneStore {
  return {
    ...store,
    async totalsSnapshot(current, instance) {
      const snapshot = await store.totalsSnapshot(current, instance);
      // Yields to other writes, then reads the cursor apart from the snapshot.
      await new Promise<void>((resolve) => setImmediate(resolve));
      snapshot.last = instance === undefined ? undefined : (await store.lastBatch(instance, "b".repeat(32))).latest;
      return snapshot;
    },
  };
}

storeContractTests({ name: "torn cursor (expected to fail)", create: () => tornCursorStore(createMemoryStore()) });
