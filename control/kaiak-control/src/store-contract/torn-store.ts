// A deliberately broken store for the contract tests' negative control
// (negative-control.test.ts runs the contract against it in a child process and expects
// it to fail): it reads the batch cursors outside the windows' snapshot, a common
// database mistake — a batch counted between the two reads is named counted while the
// windows lack it.

import { createMemoryStore } from "../storage/index.ts";
import type { ControlPlaneStore } from "../storage/index.ts";

import { storeContractTests } from "./index.ts";

function tornCursorStore(store: ControlPlaneStore): ControlPlaneStore {
  return {
    ...store,
    async totalsSnapshot(current) {
      const { windows } = await store.totalsSnapshot(current);
      // Yields to other writes, then reads the cursors apart from the windows.
      await new Promise<void>((resolve) => setImmediate(resolve));
      const { cursors } = await store.totalsSnapshot(current);
      return { windows, cursors };
    },
  };
}

storeContractTests({ name: "torn cursor (expected to fail)", create: () => tornCursorStore(createMemoryStore()) });
