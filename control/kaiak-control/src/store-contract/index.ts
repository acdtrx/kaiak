// The store contract as tests (docs/specs/CONTROL-PROTOCOL.md, Control-plane processes):
// what every ControlPlaneStore guarantees the cores running over it. A host app with its
// own store runs them against it from a node:test file:
//
//   import { storeContractTests } from "kaiak-control/store-contract";
//   storeContractTests({ name: "postgres", create: makeEmptyStore, attach: connectAgain });
//
// `attach` gives a second handle on the same store, as a second process holds it (for a
// database: another connection pool and listener). The tests then write through one
// handle and read and listen through the other, and race writes across both — the
// replicas a store must keep in agreement. Notifications may arrive late across
// handles; the tests wait for them up to `notifyTimeoutMs`.
//
// A separate entry from the library's, so a running control plane never loads node:test.

import assert from "node:assert/strict";
import { describe, test } from "node:test";

import type { Config } from "../config/index.ts";
import type { BatchId, UsageRecord } from "../messages/index.ts";
import type {
  ControlPlaneStore,
  CountedBatch,
  CurrentWindows,
  GatewayRecord,
  StoreChange,
  StoredConfig,
  WindowTotal,
} from "../storage/index.ts";

export interface StoreContractSubject {
  // Names the describe block.
  name: string;
  // A new, empty store: no configs, no batches, no gateways, sequence 0.
  create(): ControlPlaneStore | Promise<ControlPlaneStore>;
  // Another handle on the same store, as another process holds it. Default: the store
  // itself (a store serving one process, such as the in-memory store).
  attach?(store: ControlPlaneStore): ControlPlaneStore | Promise<ControlPlaneStore>;
  // Releases a store made by create, and the handles attached to it. Default: nothing.
  dispose?(store: ControlPlaneStore): void | Promise<void>;
  // How long a notification may take to reach another handle. Default 5000.
  notifyTimeoutMs?: number;
}

const HOUR = Date.UTC(2026, 9, 6, 12);
const MONTH = Date.UTC(2026, 9, 1);
const CURRENT: CurrentWindows = { hourStart: HOUR, monthStart: MONTH };
const PREVIOUS: CurrentWindows = { hourStart: HOUR - 3_600_000, monthStart: Date.UTC(2026, 8, 1) };
const BATCH_EPOCH = "b".repeat(32);

// The store keeps configs as given; their content is the core's concern.
const configDoc = (marker: number): Config => ({ marker }) as unknown as Config;
const entry = (version: number): StoredConfig => ({ version, config: configDoc(version), publishedAt: version });
const batchId = (instance: string, sequence: number): BatchId => ({ instance, epoch: BATCH_EPOCH, sequence });
const hourWindow = (used: bigint, group = "g"): WindowTotal => ({ group, type: "tokens_per_hour", windowStart: HOUR, used });

function counted(
  instance: string,
  sequence: number,
  configVersion: number,
  options: { used?: bigint; countedAt?: number; record?: string } = {},
): CountedBatch {
  const { used = 1n, countedAt = 0, record } = options;
  return {
    batch: batchId(instance, sequence),
    configVersion,
    countedAt,
    additions: used === 0n ? [] : [hourWindow(used)],
    records: record === undefined ? [] : [{ receivedAt: countedAt, record: { record_id: record } as unknown as UsageRecord }],
  };
}

function gatewayRecord(instance: string, live: boolean, receivedAt = 0): GatewayRecord {
  return {
    instance,
    status: { instance, started_at: "2026-10-06T12:00:00.000Z" } as unknown as GatewayRecord["status"],
    receivedAt,
    live,
  };
}

const sumUsed = (windows: WindowTotal[]): bigint => windows.reduce((sum, window) => sum + window.used, 0n);

export function storeContractTests(subject: StoreContractSubject): void {
  const { name, notifyTimeoutMs = 5000 } = subject;
  const attach = (store: ControlPlaneStore): Promise<ControlPlaneStore> =>
    Promise.resolve(subject.attach ? subject.attach(store) : store);

  // Runs body with a fresh store and a second handle on it, disposing of both after.
  const withStore =
    (body: (a: ControlPlaneStore, b: ControlPlaneStore) => Promise<void>) => async (): Promise<void> => {
      const a = await subject.create();
      try {
        await body(a, await attach(a));
      } finally {
        await subject.dispose?.(a);
      }
    };

  // Publishes versions 1..n with no carry, through `store`, from an empty store.
  const publishVersions = async (store: ControlPlaneStore, n: number): Promise<number> => {
    let sequence = 0;
    for (let version = 1; version <= n; version += 1) {
      const result = await store.publishConfig(
        { entry: entry(version), carried: [] },
        { version: version === 1 ? undefined : version - 1, sequence },
        100,
      );
      assert.ok(result.saved, `publishing version ${version}`);
      sequence = result.sequence;
    }
    return sequence;
  };

  // Collects the changes `store`'s subscription hears; until() waits for a condition.
  const listen = (store: ControlPlaneStore) => {
    const changes: StoreChange[] = [];
    let wake: (() => void) | undefined;
    const unsubscribe = store.subscribe((change) => {
      changes.push(change);
      wake?.();
    });
    const until = async (what: string, ready: () => boolean): Promise<void> => {
      const deadline = Date.now() + notifyTimeoutMs;
      while (!ready()) {
        const left = deadline - Date.now();
        if (left <= 0) assert.fail(`no notification within ${notifyTimeoutMs} ms: ${what}`);
        await new Promise<void>((resolve) => {
          const timer = setTimeout(resolve, left);
          wake = () => {
            clearTimeout(timer);
            resolve();
          };
        });
        wake = undefined;
      }
    };
    return { changes, until, unsubscribe };
  };

  describe(`store contract: ${name}`, () => {
    describe("config epoch", () => {
      test(
        "is 32 lowercase hex digits and stays the same",
        withStore(async (a, b) => {
          const epoch = await a.configEpoch();
          assert.match(epoch, /^[0-9a-f]{32}$/);
          assert.equal(await a.configEpoch(), epoch);
          assert.equal(await b.configEpoch(), epoch);
        }),
      );
    });

    describe("publishing", () => {
      test(
        "a version is stored only against the expected latest version and sequence",
        withStore(async (a, b) => {
          assert.deepEqual(await a.publishConfig({ entry: entry(1), carried: [] }, { version: undefined, sequence: 0 }, 10), {
            saved: true,
            sequence: 1,
          });
          // Another process that checked against "no version yet" finds version 1.
          assert.deepEqual(await b.publishConfig({ entry: entry(1), carried: [] }, { version: undefined, sequence: 0 }, 10), {
            saved: false,
            latestVersion: 1,
            sequence: 1,
          });
          // The right version but a stale sequence: its carry was read before a change.
          assert.deepEqual(await b.publishConfig({ entry: entry(2), carried: [] }, { version: 1, sequence: 0 }, 10), {
            saved: false,
            latestVersion: 1,
            sequence: 1,
          });
          assert.deepEqual(await b.publishConfig({ entry: entry(2), carried: [] }, { version: 1, sequence: 1 }, 10), {
            saved: true,
            sequence: 2,
          });
          assert.equal((await a.latestConfig())?.version, 2);
          assert.deepEqual(
            (await a.configsAfter(0)).map((stored) => stored.version),
            [1, 2],
          );
          assert.deepEqual(
            (await a.configsAfter(1)).map((stored) => stored.version),
            [2],
          );
        }),
      );

      test(
        "the carried spend is written with the version",
        withStore(async (a, b) => {
          await a.publishConfig({ entry: entry(1), carried: [] }, { version: undefined, sequence: 0 }, 10);
          await a.publishConfig({ entry: entry(2), carried: [hourWindow(40n, "carry")] }, { version: 1, sequence: 1 }, 10);
          const snapshot = await b.totalsSnapshot(CURRENT);
          assert.equal(snapshot.sequence, 2);
          assert.equal(snapshot.config?.version, 2);
          assert.deepEqual(snapshot.windows, [hourWindow(40n, "carry")]);
          // A refused publish carries nothing.
          await b.publishConfig({ entry: entry(3), carried: [hourWindow(5n, "carry")] }, { version: 1, sequence: 2 }, 10);
          assert.deepEqual((await a.totalsSnapshot(CURRENT)).windows, [hourWindow(40n, "carry")]);
        }),
      );

      test(
        "the newest `keep` versions stay",
        withStore(async (a) => {
          let sequence = 0;
          for (let version = 1; version <= 5; version += 1) {
            const result = await a.publishConfig(
              { entry: entry(version), carried: [] },
              { version: version === 1 ? undefined : version - 1, sequence },
              2,
            );
            assert.ok(result.saved);
            sequence = result.sequence;
          }
          const kept = (await a.configsAfter(0)).map((stored) => stored.version);
          assert.ok(kept.includes(4) && kept.includes(5), `kept ${kept.join(",")}`);
          assert.deepEqual(kept, [...kept].sort((x, y) => x - y));
        }),
      );

      test(
        "two processes publishing against the same state: one is stored",
        withStore(async (a, b) => {
          const sequence = await publishVersions(a, 1);
          const results = await Promise.all(
            [a, b, a, b].map((store) => store.publishConfig({ entry: entry(2), carried: [] }, { version: 1, sequence }, 10)),
          );
          assert.equal(results.filter((result) => result.saved).length, 1);
          assert.equal((await a.configsAfter(1)).length, 1);
          assert.equal((await b.totalsSnapshot(CURRENT)).sequence, sequence + 1);
        }),
      );

      test(
        "stored configs share no state with what callers hold, read or written",
        withStore(async (a) => {
          const written = entry(1);
          await a.publishConfig({ entry: written, carried: [] }, { version: undefined, sequence: 0 }, 10);
          (written.config as unknown as { marker: number }).marker = 100;
          const latest = await a.latestConfig();
          (latest?.config as unknown as { marker: number }).marker = 200;
          const [after] = await a.configsAfter(0);
          (after?.config as unknown as { marker: number }).marker = 300;
          const snapshot = await a.totalsSnapshot(CURRENT);
          (snapshot.config?.config as unknown as { marker: number }).marker = 400;
          assert.equal(((await a.latestConfig())?.config as unknown as { marker: number }).marker, 1);
        }),
      );
    });

    describe("counting batches", () => {
      test(
        "a batch is counted only against the expected last batch",
        withStore(async (a, b) => {
          const sequence = await publishVersions(a, 1);
          assert.deepEqual(await a.saveCountedBatch(counted("gw-1", 1, 1), undefined, 10), { saved: true, sequence: sequence + 1 });
          // Another process that decided against "no batch yet" finds batch 1.
          assert.deepEqual(await b.saveCountedBatch(counted("gw-1", 1, 1), undefined, 10), {
            saved: false,
            last: batchId("gw-1", 1),
            configVersion: 1,
          });
          assert.deepEqual(await b.saveCountedBatch(counted("gw-1", 2, 1), batchId("gw-1", 3), 10), {
            saved: false,
            last: batchId("gw-1", 1),
            configVersion: 1,
          });
          assert.deepEqual(await b.saveCountedBatch(counted("gw-1", 2, 1), batchId("gw-1", 1), 10), {
            saved: true,
            sequence: sequence + 2,
          });
          assert.deepEqual(await a.lastBatch("gw-1"), batchId("gw-1", 2));
          assert.equal(await a.lastBatch("gw-2"), undefined);
        }),
      );

      test(
        "a batch is counted only under the latest config version",
        withStore(async (a, b) => {
          let sequence = await publishVersions(a, 1);
          // A publish lands between computing the batch under version 1 and writing it.
          const published = await b.publishConfig({ entry: entry(2), carried: [] }, { version: 1, sequence }, 10);
          assert.ok(published.saved);
          sequence = published.sequence;
          assert.deepEqual(await a.saveCountedBatch(counted("gw-1", 1, 1, { used: 7n }), undefined, 10), {
            saved: false,
            last: undefined,
            configVersion: 2,
          });
          const snapshot = await a.totalsSnapshot(CURRENT, "gw-1");
          assert.deepEqual([snapshot.sequence, snapshot.last, snapshot.windows], [sequence, undefined, []]);
          assert.deepEqual(await a.saveCountedBatch(counted("gw-1", 1, 2, { used: 7n }), undefined, 10), {
            saved: true,
            sequence: sequence + 1,
          });
        }),
      );

      test(
        "a batch counted by two processes at once is counted once",
        withStore(async (a, b) => {
          await publishVersions(a, 1);
          const results = await Promise.all(
            Array.from({ length: 8 }, (_, i) => (i % 2 === 0 ? a : b).saveCountedBatch(counted("gw-1", 1, 1, { used: 5n }), undefined, 10)),
          );
          assert.equal(results.filter((result) => result.saved).length, 1);
          assert.deepEqual((await b.totalsSnapshot(CURRENT)).windows, [hourWindow(5n)]);
        }),
      );

      test(
        "records are kept newest first, at most the newest `keepRecords`",
        withStore(async (a, b) => {
          await publishVersions(a, 1);
          for (let sequence = 1; sequence <= 4; sequence += 1) {
            const store = sequence % 2 === 0 ? a : b;
            const result = await store.saveCountedBatch(
              counted("gw-1", sequence, 1, { record: `r${sequence}` }),
              sequence === 1 ? undefined : batchId("gw-1", sequence - 1),
              10,
            );
            assert.ok(result.saved);
          }
          const ids = (await a.recentRecords(3)).map((received) => (received.record as unknown as { record_id: string }).record_id);
          assert.deepEqual(ids, ["r4", "r3", "r2"]);
          assert.deepEqual(await a.recentRecords(0), []);
        }),
      );

      test(
        "the batch cursor stays when its gateway is forgotten; the retention drops it",
        withStore(async (a, b) => {
          await publishVersions(a, 1);
          await a.saveCountedBatch(counted("gw-1", 1, 1, { countedAt: 100 }), undefined, 10);
          await b.saveCountedBatch(counted("gw-2", 1, 1, { countedAt: 200 }), undefined, 10);
          const saved = await a.saveGateway(gatewayRecord("gw-1", true), undefined);
          assert.ok(saved.saved);
          assert.deepEqual(await a.forgetGateways([{ instance: "gw-1", revision: saved.revision }]), ["gw-1"]);
          assert.deepEqual(await b.lastBatch("gw-1"), batchId("gw-1", 1));
          assert.deepEqual(await b.dropBatchCursorsCountedBefore(150), ["gw-1"]);
          assert.equal(await a.lastBatch("gw-1"), undefined);
          assert.deepEqual(await a.lastBatch("gw-2"), batchId("gw-2", 1));
        }),
      );
    });

    describe("the totals sequence and snapshots", () => {
      test(
        "moves by one with every change to the totals, and only then",
        withStore(async (a, b) => {
          const sequence = await publishVersions(a, 1);
          assert.equal(sequence, 1);
          await a.saveCountedBatch(counted("gw-1", 1, 1), undefined, 10);
          assert.equal((await b.totalsSnapshot(CURRENT)).sequence, 2);
          // Joining the live set moves it; a status that leaves it as it is does not.
          const joined = await b.saveGateway(gatewayRecord("gw-1", true), undefined);
          assert.ok(joined.saved);
          assert.equal(joined.sequence, 3);
          const again = await a.saveGateway(gatewayRecord("gw-1", true, 10), joined.revision);
          assert.ok(again.saved);
          assert.equal(again.sequence, 3);
          // Leaving moves it.
          const left = await a.saveGateway(gatewayRecord("gw-1", false, 10), again.revision);
          assert.ok(left.saved);
          assert.equal(left.sequence, 4);
          // Refused writes and dropping past windows change nothing.
          await b.saveCountedBatch(counted("gw-1", 1, 1), undefined, 10);
          await b.dropPastWindowTotals(PREVIOUS);
          assert.equal((await a.totalsSnapshot(CURRENT)).sequence, 4);
        }),
      );

      test(
        "a snapshot holds exactly the batches counted at its sequence, whichever process wrote",
        withStore(async (a, b) => {
          const base = await publishVersions(a, 1);
          // Eight instances count four batches each through both handles while both read.
          const writes: Promise<unknown>[] = [];
          const reads: Promise<{ sequence: number; used: bigint; last: BatchId | undefined }>[] = [];
          for (let round = 1; round <= 4; round += 1) {
            for (let gw = 0; gw < 8; gw += 1) {
              const store = gw % 2 === 0 ? a : b;
              const instance = `gw-${gw}`;
              writes.push(
                store.saveCountedBatch(counted(instance, round, 1), round === 1 ? undefined : batchId(instance, round - 1), 1000),
              );
              const reader = gw % 2 === 0 ? b : a;
              reads.push(
                reader.totalsSnapshot(CURRENT, "gw-0").then((snapshot) => ({
                  sequence: snapshot.sequence,
                  used: sumUsed(snapshot.windows),
                  last: snapshot.last,
                })),
              );
            }
          }
          // Writes for one instance are issued in order; one refused because its
          // predecessor had not landed yet is counted below.
          await Promise.all(writes);
          for (let gw = 0; gw < 8; gw += 1) {
            const instance = `gw-${gw}`;
            for (;;) {
              const last = await a.lastBatch(instance);
              const next = (last?.sequence ?? 0) + 1;
              if (next > 4) break;
              await a.saveCountedBatch(counted(instance, next, 1), last, 1000);
            }
          }
          for (const read of await Promise.all(reads)) {
            // Every batch adds 1 and moves the sequence by one: the windows equal the
            // batches counted since the publish.
            assert.equal(read.used, BigInt(read.sequence - base), `snapshot at ${read.sequence}`);
          }
          const final = await b.totalsSnapshot(CURRENT, "gw-0");
          assert.equal(final.sequence, base + 32);
          assert.equal(sumUsed(final.windows), 32n);
          assert.deepEqual(final.last, batchId("gw-0", 4));
        }),
      );

      test(
        "a snapshot reads only the current windows, and dropping past ones keeps the current",
        withStore(async (a, b) => {
          await publishVersions(a, 1);
          const past: WindowTotal = { group: "g", type: "tokens_per_hour", windowStart: PREVIOUS.hourStart, used: 3n };
          const month: WindowTotal = { type: "usd_per_month", windowStart: MONTH, used: 9n };
          const batch = counted("gw-1", 1, 1, { used: 0n });
          batch.additions = [past, hourWindow(2n), month];
          assert.ok((await a.saveCountedBatch(batch, undefined, 10)).saved);
          const current = (await b.totalsSnapshot(CURRENT)).windows;
          assert.equal(current.length, 2);
          assert.deepEqual(
            current.find((window) => window.type === "usd_per_month"),
            month,
          );
          assert.deepEqual((await b.totalsSnapshot(PREVIOUS)).windows, [past]);
          await b.dropPastWindowTotals(CURRENT);
          assert.deepEqual((await a.totalsSnapshot(PREVIOUS)).windows, []);
          assert.equal((await a.totalsSnapshot(CURRENT)).windows.length, 2);
        }),
      );

      test(
        "the same window adds up across writes; model sets name windows apart",
        withStore(async (a, b) => {
          await publishVersions(a, 1);
          const models = (used: bigint): WindowTotal => ({ ...hourWindow(used), models: ["a", "b"] });
          const one = counted("gw-1", 1, 1, { used: 0n });
          one.additions = [hourWindow(2n), models(5n)];
          const two = counted("gw-2", 1, 1, { used: 0n });
          two.additions = [hourWindow(3n), models(1n)];
          assert.ok((await a.saveCountedBatch(one, undefined, 10)).saved);
          assert.ok((await b.saveCountedBatch(two, undefined, 10)).saved);
          const windows = (await a.totalsSnapshot(CURRENT)).windows;
          assert.equal(windows.find((window) => window.models === undefined)?.used, 5n);
          assert.equal(windows.find((window) => window.models !== undefined)?.used, 6n);
        }),
      );

      test(
        "a snapshot counts the live set",
        withStore(async (a, b) => {
          assert.equal((await a.totalsSnapshot(CURRENT)).liveGateways, 0);
          await a.saveGateway(gatewayRecord("gw-1", true), undefined);
          await b.saveGateway(gatewayRecord("gw-2", true), undefined);
          await b.saveGateway(gatewayRecord("gw-3", false), undefined);
          assert.equal((await a.totalsSnapshot(CURRENT)).liveGateways, 2);
          const empty = await a.totalsSnapshot(CURRENT, "gw-1");
          assert.deepEqual([empty.config, empty.last], [undefined, undefined]);
        }),
      );
    });

    describe("gateway records", () => {
      test(
        "a record is written only against the expected revision",
        withStore(async (a, b) => {
          const first = await a.saveGateway(gatewayRecord("gw-1", true), undefined);
          assert.ok(first.saved);
          assert.equal(first.revision, 1);
          // Another process that judged against "no record" finds revision 1.
          const stale = await b.saveGateway(gatewayRecord("gw-1", true, 5), undefined);
          assert.equal(stale.saved, false);
          assert.equal(stale.saved === false && stale.current?.revision, 1);
          const second = await b.saveGateway(gatewayRecord("gw-1", true, 5), 1);
          assert.ok(second.saved);
          assert.equal(second.revision, 2);
          const stored = await a.gateway("gw-1");
          assert.equal(stored?.revision, 2);
          assert.equal(stored?.receivedAt, 5);
          assert.deepEqual(
            (await b.gateways()).map((gateway) => gateway.instance),
            ["gw-1"],
          );
        }),
      );

      test(
        "two processes writing one record against the same revision: one is written",
        withStore(async (a, b) => {
          const first = await a.saveGateway(gatewayRecord("gw-1", true), undefined);
          assert.ok(first.saved);
          const results = await Promise.all(
            Array.from({ length: 6 }, (_, i) => (i % 2 === 0 ? a : b).saveGateway(gatewayRecord("gw-1", i % 3 !== 0, i), 1)),
          );
          assert.equal(results.filter((result) => result.saved).length, 1);
          assert.equal((await b.gateway("gw-1"))?.revision, 2);
        }),
      );

      test(
        "a gateway is forgotten only at the revision judged; forgetting a live one moves the sequence",
        withStore(async (a, b) => {
          const live = await a.saveGateway(gatewayRecord("gw-1", true), undefined);
          const idle = await a.saveGateway(gatewayRecord("gw-2", false), undefined);
          assert.ok(live.saved && idle.saved);
          const sequence = (await a.totalsSnapshot(CURRENT)).sequence;
          // A status taken meanwhile by the other process moves gw-1's revision on.
          assert.ok((await b.saveGateway(gatewayRecord("gw-1", true, 9), live.revision)).saved);
          assert.deepEqual(
            await a.forgetGateways([
              { instance: "gw-1", revision: live.revision },
              { instance: "gw-2", revision: idle.revision },
              { instance: "gw-unknown", revision: 1 },
            ]),
            ["gw-2"],
          );
          assert.equal((await b.totalsSnapshot(CURRENT)).sequence, sequence);
          assert.deepEqual(await b.forgetGateways([{ instance: "gw-1", revision: 2 }]), ["gw-1"]);
          assert.equal((await a.totalsSnapshot(CURRENT)).sequence, sequence + 1);
          assert.equal(await a.gateway("gw-1"), undefined);
          // A forgotten gateway that reports again starts over.
          const back = await b.saveGateway(gatewayRecord("gw-1", true), undefined);
          assert.equal(back.saved && back.revision, 1);
        }),
      );

      test(
        "gateway records share no state with what callers hold",
        withStore(async (a) => {
          const record = gatewayRecord("gw-1", true);
          await a.saveGateway(record, undefined);
          record.receivedAt = 99;
          const read = await a.gateway("gw-1");
          if (read) read.receivedAt = 77;
          const [listed] = await a.gateways();
          if (listed) listed.receivedAt = 55;
          assert.equal((await a.gateway("gw-1"))?.receivedAt, 0);
        }),
      );
    });

    describe("change notification", () => {
      test(
        "every subscriber hears every change, from either process, in order",
        withStore(async (a, b) => {
          const onA = listen(a);
          const onB = listen(b);
          try {
            await publishVersions(a, 1);
            await b.saveCountedBatch(counted("gw-1", 1, 1), undefined, 10);
            await a.saveGateway(gatewayRecord("gw-1", true), undefined);
            const expected: StoreChange[] = [
              { type: "config-published", version: 1, sequence: 1 },
              { type: "batch-counted", instance: "gw-1", sequence: 2 },
              { type: "gateways-changed", liveChanged: true, sequence: 3 },
            ];
            for (const heard of [onA, onB]) {
              await heard.until("three changes", () => heard.changes.length >= 3);
              assert.deepEqual(heard.changes, expected);
            }
          } finally {
            onA.unsubscribe();
            onB.unsubscribe();
          }
        }),
      );

      test(
        "a change is heard after its write: a read in the listener sees it",
        withStore(async (a, b) => {
          const seen: Promise<number>[] = [];
          const unsubscribe = b.subscribe((change) => {
            if (change.type === "batch-counted") seen.push(b.totalsSnapshot(CURRENT).then((snapshot) => snapshot.sequence));
          });
          try {
            await publishVersions(a, 1);
            await a.saveCountedBatch(counted("gw-1", 1, 1), undefined, 10);
            const deadline = Date.now() + notifyTimeoutMs;
            while (seen.length === 0 && Date.now() < deadline) await new Promise((resolve) => setTimeout(resolve, 10));
            assert.equal(seen.length, 1, "the batch was heard");
            assert.ok((await seen[0]!) >= 2);
          } finally {
            unsubscribe();
          }
        }),
      );

      test(
        "refused writes are not announced; unsubscribing stops the calls",
        withStore(async (a, b) => {
          await publishVersions(a, 1);
          const heard = listen(b);
          await a.saveCountedBatch(counted("gw-1", 1, 1), undefined, 10);
          await heard.until("the counted batch", () => heard.changes.length >= 1);
          // Refused: the same batch again, a stale publish, a stale gateway write.
          await a.saveCountedBatch(counted("gw-1", 1, 1), undefined, 10);
          await a.publishConfig({ entry: entry(2), carried: [] }, { version: undefined, sequence: 0 }, 10);
          await a.saveGateway(gatewayRecord("gw-9", true), 3);
          heard.unsubscribe();
          heard.unsubscribe();
          await a.saveGateway(gatewayRecord("gw-1", true), undefined);
          // A marker through a fresh subscription shows the earlier writes have arrived.
          const marker = listen(b);
          await a.saveGateway(gatewayRecord("gw-2", true), undefined);
          await marker.until("the marker", () => marker.changes.some((change) => change.type === "gateways-changed"));
          marker.unsubscribe();
          assert.deepEqual(
            heard.changes.map((change) => change.type),
            ["batch-counted"],
          );
        }),
      );
    });
  });
}
