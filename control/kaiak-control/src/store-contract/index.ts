// The store contract as tests (docs/specs/CONTROL-PROTOCOL.md, Control-plane processes):
// what every ControlPlaneStore guarantees the cores running over it. A host app with its
// own store runs them against it from a node:test file:
//
//   import { storeContractTests } from "kaiak-control/store-contract";
//   storeContractTests({ name: "postgres", create: makeEmptyStore, attach: connectAgain, reconnect: dropListener });
//
// `attach` gives a second handle on the same store, as a second process holds it (for a
// database: another connection pool and listener). The tests then write through one
// handle and read and listen through the other, and race writes across both — the
// replicas a store must keep in agreement. Notifications may arrive late across
// handles; the tests wait for them up to `notifyTimeoutMs`.
//
// A separate entry from the library's, so a running control plane never loads node:test.

import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { describe, test } from "node:test";

import type { BatchId, UsageRecord } from "../messages/index.ts";
import type {
  ControlPlaneStore,
  CountedBatch,
  CurrentWindows,
  ConfigEntry,
  GatewayRecord,
  StoreChange,
  TotalsSnapshot,
  WindowTotal,
} from "../storage/index.ts";

export interface StoreContractSubject {
  // Names the describe block.
  name: string;
  // A new, empty store: no config, no batches, no gateways.
  create(): ControlPlaneStore | Promise<ControlPlaneStore>;
  // Another handle on the same store, as another process holds it. Default: the store
  // itself (a store serving one process, such as the in-memory store).
  attach?(store: ControlPlaneStore): ControlPlaneStore | Promise<ControlPlaneStore>;
  // Releases a store made by create, and the handles attached to it. Default: nothing.
  dispose?(store: ControlPlaneStore): void | Promise<void>;
  // How long a notification may take to reach another handle. Default 5000.
  notifyTimeoutMs?: number;
  // Takes a handle's change channel down, runs `whileDown` (writes through the other
  // handle, which the channel misses), and brings the channel back, as a dropped
  // listening connection that the store opens again (for a database: closes the
  // LISTEN session and holds it closed until `whileDown` resolves). Required for a
  // store whose channel can drop changes; without it the catch-up test is skipped,
  // which only a store whose channel cannot drop a change (the in-memory store) may
  // do.
  reconnect?(store: ControlPlaneStore, whileDown: () => Promise<void>): void | Promise<void>;
}

const HOUR = Date.UTC(2026, 9, 6, 12);
const MONTH = Date.UTC(2026, 9, 1);
const CURRENT: CurrentWindows = { hourStart: HOUR, monthStart: MONTH };
const PREVIOUS: CurrentWindows = { hourStart: HOUR - 3_600_000, monthStart: Date.UTC(2026, 8, 1) };
const BATCH_EPOCH = "b".repeat(32);
const OTHER_EPOCH = "c".repeat(32);

// The store keeps a config's text and hash as given; producing them is the core's
// concern, so the text needs to be no real config.
const sha256 = (text: string): string => createHash("sha256").update(text).digest("hex");
const configText = (marker: number): string => JSON.stringify({ marker });
const hashOf = (marker: number): string => sha256(configText(marker));
const entry = (marker: number): ConfigEntry => ({ text: configText(marker), hash: hashOf(marker), publishedAt: marker });
const batchId = (instance: string, sequence: number, epoch = BATCH_EPOCH): BatchId => ({ instance, epoch, sequence });
const hourWindow = (used: bigint, group = "g"): WindowTotal => ({ group, type: "tokens_per_hour", windowStart: HOUR, used });

function counted(
  instance: string,
  sequence: number,
  options: { used?: bigint; countedAt?: number; record?: string; epoch?: string; group?: string } = {},
): CountedBatch {
  const { used = 1n, countedAt = 0, record, epoch, group } = options;
  return {
    batch: batchId(instance, sequence, epoch),
    countedAt,
    additions: used === 0n ? [] : [hourWindow(used, group)],
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
const byBatch = (a: BatchId, b: BatchId): number =>
  a.instance.localeCompare(b.instance) || a.epoch.localeCompare(b.epoch) || a.sequence - b.sequence;
const sortedCursors = (snapshot: TotalsSnapshot): BatchId[] => [...snapshot.cursors].sort(byBatch);
// The sequence of the last batch a snapshot names counted for one instance and epoch,
// 0 for none.
const countedIn = (snapshot: TotalsSnapshot, instance: string, epoch = BATCH_EPOCH): number =>
  snapshot.cursors.find((cursor) => cursor.instance === instance && cursor.epoch === epoch)?.sequence ?? 0;
const usedBy = (snapshot: TotalsSnapshot, group: string): bigint =>
  snapshot.windows.find((window) => window.group === group && window.type === "tokens_per_hour")?.used ?? 0n;

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

  // Publishes configs 1..n through `store`, each replacing the one before, from an
  // empty store.
  const publishConfigs = async (store: ControlPlaneStore, n: number): Promise<void> => {
    for (let marker = 1; marker <= n; marker += 1) {
      const result = await store.publishConfig(entry(marker), marker === 1 ? undefined : hashOf(marker - 1));
      assert.ok(result.saved, `publishing config ${marker}`);
    }
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
    describe("publishing", () => {
      test(
        "a config replaces the current one only against the expected current config",
        withStore(async (a, b) => {
          assert.equal(await a.currentConfig(), undefined);
          assert.deepEqual(await a.publishConfig(entry(1), undefined), { saved: true });
          // Another process that checked against "no config yet" finds config 1.
          assert.deepEqual(await b.publishConfig(entry(2), undefined), { saved: false, current: entry(1) });
          assert.deepEqual(await b.publishConfig(entry(2), hashOf(1)), { saved: true });
          assert.deepEqual(await a.currentConfig(), entry(2));
          // The same content published again is a publish like any other.
          const again = { ...entry(2), publishedAt: 20 };
          assert.deepEqual(await a.publishConfig(again, hashOf(2)), { saved: true });
          assert.deepEqual(await b.currentConfig(), again);
        }),
      );

      test(
        "the config text comes back exactly as published, so its hash still matches",
        withStore(async (a, b) => {
          // Members in no sorted order, nested, with characters a JSON type might
          // normalize: a store that keeps the document as anything but text can change
          // it.
          const text = '{"zeta":1,"alpha":{"y":[3,{"k":"v","c":null}],"b":"é\\u00e9 \\"q\\""},"mid":true,"n":-0.5}';
          const published: ConfigEntry = { text, hash: sha256(text), publishedAt: 1 };
          assert.ok((await a.publishConfig(published, undefined)).saved);
          const read = await b.currentConfig();
          assert.equal(read?.text, text);
          assert.equal(sha256(read?.text ?? ""), published.hash);
          const refused = await b.publishConfig(entry(2), undefined);
          assert.equal(refused.saved === false && refused.current?.text, text);
        }),
      );

      test(
        "a publish leaves the windows as they are",
        withStore(async (a, b) => {
          await publishConfigs(a, 1);
          assert.ok((await a.saveCountedBatch(counted("gw-1", 1, { used: 40n }), undefined, 10)).saved);
          assert.ok((await b.publishConfig(entry(2), hashOf(1))).saved);
          assert.deepEqual((await a.totalsSnapshot(CURRENT)).windows, [hourWindow(40n)]);
          assert.equal((await a.currentConfig())?.hash, hashOf(2));
        }),
      );

      test(
        "two processes publishing against the same config: one is stored",
        withStore(async (a, b) => {
          await publishConfigs(a, 1);
          const markers = [2, 3, 4, 5];
          const results = await Promise.all(markers.map((marker, i) => [a, b][i % 2]!.publishConfig(entry(marker), hashOf(1))));
          const won = markers.filter((_, i) => results[i]!.saved);
          assert.equal(won.length, 1);
          assert.deepEqual(await b.currentConfig(), entry(won[0]!));
        }),
      );
    });

    describe("counting batches", () => {
      test(
        "a batch is counted only against the expected last batch",
        withStore(async (a, b) => {
          await publishConfigs(a, 1);
          assert.deepEqual(await a.saveCountedBatch(counted("gw-1", 1), undefined, 10), { saved: true });
          // Another process that decided against "no batch yet" finds batch 1.
          const standing = { inEpoch: batchId("gw-1", 1), latest: batchId("gw-1", 1) };
          assert.deepEqual(await b.saveCountedBatch(counted("gw-1", 1), undefined, 10), { saved: false, cursors: standing });
          assert.deepEqual(await b.saveCountedBatch(counted("gw-1", 2), batchId("gw-1", 3), 10), {
            saved: false,
            cursors: standing,
          });
          assert.deepEqual(await b.saveCountedBatch(counted("gw-1", 2), batchId("gw-1", 1), 10), { saved: true });
          assert.deepEqual(await a.lastBatch("gw-1", BATCH_EPOCH), { inEpoch: batchId("gw-1", 2), latest: batchId("gw-1", 2) });
          assert.deepEqual(await a.lastBatch("gw-2", BATCH_EPOCH), { inEpoch: undefined, latest: undefined });
        }),
      );

      test(
        "a batch is decided against its own epoch's last batch, whatever epochs came after",
        withStore(async (a, b) => {
          await publishConfigs(a, 1);
          assert.ok((await a.saveCountedBatch(counted("gw-1", 1, { countedAt: 1 }), undefined, 10)).saved);
          // The gateway's resend of batch 2 is counted by one process, then it moves to a
          // new epoch; a write of batch 2 decided before all that is refused.
          assert.ok((await b.saveCountedBatch(counted("gw-1", 2, { countedAt: 2 }), batchId("gw-1", 1), 10)).saved);
          assert.ok((await b.saveCountedBatch(counted("gw-1", 1, { countedAt: 3, epoch: OTHER_EPOCH }), undefined, 10)).saved);
          assert.deepEqual(await a.saveCountedBatch(counted("gw-1", 2, { countedAt: 4 }), batchId("gw-1", 1), 10), {
            saved: false,
            cursors: { inEpoch: batchId("gw-1", 2), latest: batchId("gw-1", 1, OTHER_EPOCH) },
          });
          // A batch of the earlier epoch never counted is counted against that epoch's
          // last batch, and becomes the one counted last.
          assert.ok((await a.saveCountedBatch(counted("gw-1", 3, { countedAt: 5 }), batchId("gw-1", 2), 10)).saved);
          assert.deepEqual(await b.lastBatch("gw-1", OTHER_EPOCH), {
            inEpoch: batchId("gw-1", 1, OTHER_EPOCH),
            latest: batchId("gw-1", 3),
          });
          // Totals name each epoch's last batch, the late one of the earlier epoch
          // included.
          assert.deepEqual(sortedCursors(await b.totalsSnapshot(CURRENT)), [batchId("gw-1", 3), batchId("gw-1", 1, OTHER_EPOCH)]);
        }),
      );

      test(
        "publishes and batches never refuse each other",
        withStore(async (a, b) => {
          await publishConfigs(a, 1);
          // A publish lands between deciding a batch and writing it: the batch is counted.
          assert.ok((await b.publishConfig(entry(2), hashOf(1))).saved);
          assert.deepEqual(await a.saveCountedBatch(counted("gw-1", 1, { used: 7n }), undefined, 10), { saved: true });
          // A batch lands between checking a publish and writing it: the publish is stored.
          assert.ok((await b.saveCountedBatch(counted("gw-2", 1, { used: 3n }), undefined, 10)).saved);
          assert.deepEqual(await a.publishConfig(entry(3), hashOf(2)), { saved: true });
          // Racing at once, every write lands.
          const results = await Promise.all([
            a.publishConfig(entry(4), hashOf(3)),
            b.saveCountedBatch(counted("gw-1", 2, { used: 1n }), batchId("gw-1", 1), 10),
            a.saveCountedBatch(counted("gw-2", 2, { used: 1n }), batchId("gw-2", 1), 10),
          ]);
          assert.ok(results.every((result) => result.saved));
          assert.equal((await a.currentConfig())?.hash, hashOf(4));
          assert.equal(sumUsed((await a.totalsSnapshot(CURRENT)).windows), 12n);
        }),
      );

      test(
        "a batch counted by two processes at once is counted once",
        withStore(async (a, b) => {
          await publishConfigs(a, 1);
          const results = await Promise.all(
            Array.from({ length: 8 }, (_, i) => (i % 2 === 0 ? a : b).saveCountedBatch(counted("gw-1", 1, { used: 5n }), undefined, 10)),
          );
          assert.equal(results.filter((result) => result.saved).length, 1);
          assert.deepEqual((await b.totalsSnapshot(CURRENT)).windows, [hourWindow(5n)]);
        }),
      );

      test(
        "records are kept newest first, at most the newest `keepRecords`",
        withStore(async (a, b) => {
          await publishConfigs(a, 1);
          for (let sequence = 1; sequence <= 4; sequence += 1) {
            const store = sequence % 2 === 0 ? a : b;
            const result = await store.saveCountedBatch(
              counted("gw-1", sequence, { record: `r${sequence}` }),
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
          await publishConfigs(a, 1);
          await a.saveCountedBatch(counted("gw-1", 1, { countedAt: 100 }), undefined, 10);
          await b.saveCountedBatch(counted("gw-2", 1, { countedAt: 200 }), undefined, 10);
          const saved = await a.saveGateway(gatewayRecord("gw-1", true), undefined);
          assert.ok(saved.saved);
          assert.deepEqual(await a.forgetGateways([{ instance: "gw-1", revision: saved.revision }]), ["gw-1"]);
          assert.deepEqual((await b.lastBatch("gw-1", BATCH_EPOCH)).inEpoch, batchId("gw-1", 1));
          // Each epoch's last batch ages on its own.
          await b.saveCountedBatch(counted("gw-2", 1, { countedAt: 100, epoch: OTHER_EPOCH }), undefined, 10);
          assert.deepEqual((await b.dropBatchCursorsCountedBefore(150)).sort(), ["gw-1", "gw-2"]);
          assert.deepEqual(await a.lastBatch("gw-1", BATCH_EPOCH), { inEpoch: undefined, latest: undefined });
          assert.deepEqual(await a.lastBatch("gw-2", OTHER_EPOCH), { inEpoch: undefined, latest: batchId("gw-2", 1) });
          assert.deepEqual((await a.lastBatch("gw-2", BATCH_EPOCH)).inEpoch, batchId("gw-2", 1));
          assert.deepEqual((await b.totalsSnapshot(CURRENT)).cursors, [batchId("gw-2", 1)]);
        }),
      );
    });

    describe("snapshots", () => {
      test(
        "a snapshot holds exactly the batches its cursors name, whichever process wrote",
        withStore(async (a, b) => {
          await publishConfigs(a, 1);
          // Eight instances count three batches in each of two epochs through both
          // handles while both read. Each (instance, epoch) adds 1 per batch to a group
          // of its own, so in every snapshot that group's window equals the epoch's
          // cursor.
          const epochs = [BATCH_EPOCH, OTHER_EPOCH];
          const group = (instance: string, epoch: string): string => `g-${instance}-${epoch[0]}`;
          const writes: Promise<unknown>[] = [];
          const reads: Promise<TotalsSnapshot>[] = [];
          for (let round = 1; round <= 3; round += 1) {
            for (let gw = 0; gw < 8; gw += 1) {
              for (const epoch of epochs) {
                const instance = `gw-${gw}`;
                const store = gw % 2 === 0 ? a : b;
                writes.push(
                  store.saveCountedBatch(
                    counted(instance, round, { epoch, group: group(instance, epoch) }),
                    round === 1 ? undefined : batchId(instance, round - 1, epoch),
                    1000,
                  ),
                );
                reads.push((gw % 2 === 0 ? b : a).totalsSnapshot(CURRENT));
              }
            }
          }
          // Writes for one epoch are issued in order; one refused because its predecessor
          // had not landed yet is counted below.
          await Promise.all(writes);
          for (let gw = 0; gw < 8; gw += 1) {
            for (const epoch of epochs) {
              const instance = `gw-${gw}`;
              for (;;) {
                const last = (await a.lastBatch(instance, epoch)).inEpoch;
                const next = (last?.sequence ?? 0) + 1;
                if (next > 3) break;
                await a.saveCountedBatch(counted(instance, next, { epoch, group: group(instance, epoch) }), last, 1000);
              }
            }
          }
          reads.push(b.totalsSnapshot(CURRENT));
          for (const snapshot of await Promise.all(reads)) {
            for (let gw = 0; gw < 8; gw += 1) {
              for (const epoch of epochs) {
                const instance = `gw-${gw}`;
                assert.equal(
                  usedBy(snapshot, group(instance, epoch)),
                  BigInt(countedIn(snapshot, instance, epoch)),
                  `${instance} in epoch ${epoch[0]}`,
                );
              }
            }
          }
          const final = await a.totalsSnapshot(CURRENT);
          assert.equal(sumUsed(final.windows), 48n);
          assert.equal(final.cursors.length, 16);
        }),
      );

      test(
        "a snapshot names a batch counted exactly when its windows hold it",
        withStore(async (a, b) => {
          await publishConfigs(a, 1);
          // One instance counts batch after batch while reads through both handles run
          // in between: each snapshot's cursor and windows agree.
          const reads: Promise<TotalsSnapshot>[] = [];
          let last: BatchId | undefined;
          for (let sequence = 1; sequence <= 16; sequence += 1) {
            for (const reader of [a, b]) reads.push(reader.totalsSnapshot(CURRENT));
            const written = await (sequence % 2 === 0 ? a : b).saveCountedBatch(counted("gw-0", sequence), last, 10);
            assert.ok(written.saved);
            last = batchId("gw-0", sequence);
          }
          for (const snapshot of await Promise.all(reads)) {
            assert.equal(usedBy(snapshot, "g"), BigInt(countedIn(snapshot, "gw-0")), "cursor and windows");
          }
        }),
      );

      test(
        "a snapshot reads only the current windows, and dropping past ones keeps the current",
        withStore(async (a, b) => {
          await publishConfigs(a, 1);
          const past: WindowTotal = { group: "g", type: "tokens_per_hour", windowStart: PREVIOUS.hourStart, used: 3n };
          const month: WindowTotal = { type: "usd_per_month", windowStart: MONTH, used: 9n };
          const batch = counted("gw-1", 1, { used: 0n });
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
        "the same window adds up across writes; scopes and types name windows apart",
        withStore(async (a, b) => {
          await publishConfigs(a, 1);
          const global = (used: bigint): WindowTotal => ({ type: "tokens_per_hour", windowStart: HOUR, used });
          const money = (used: bigint): WindowTotal => ({ group: "g", type: "usd_per_month", windowStart: MONTH, used });
          const one = counted("gw-1", 1, { used: 0n });
          one.additions = [hourWindow(2n), global(5n), money(7n)];
          const two = counted("gw-2", 1, { used: 0n });
          two.additions = [hourWindow(3n), global(1n), money(1n)];
          assert.ok((await a.saveCountedBatch(one, undefined, 10)).saved);
          assert.ok((await b.saveCountedBatch(two, undefined, 10)).saved);
          const windows = (await a.totalsSnapshot(CURRENT)).windows;
          assert.equal(windows.length, 3);
          assert.equal(windows.find((window) => window.group === "g" && window.type === "tokens_per_hour")?.used, 5n);
          assert.equal(windows.find((window) => window.group === undefined)?.used, 6n);
          assert.equal(windows.find((window) => window.type === "usd_per_month")?.used, 8n);
        }),
      );

      test(
        "an empty store's snapshot has no windows and no cursors",
        withStore(async (a) => {
          assert.deepEqual(await a.totalsSnapshot(CURRENT), { windows: [], cursors: [] });
        }),
      );
    });

    describe("gateway records", () => {
      test(
        "a record is written only against the expected revision",
        withStore(async (a, b) => {
          const first = await a.saveGateway(gatewayRecord("gw-1", true), undefined);
          assert.ok(first.saved);
          // Another process that judged against "no record" finds the first record.
          const stale = await b.saveGateway(gatewayRecord("gw-1", true, 5), undefined);
          assert.equal(stale.saved, false);
          assert.equal(stale.saved === false && stale.current?.revision, first.revision);
          const second = await b.saveGateway(gatewayRecord("gw-1", true, 5), first.revision);
          assert.ok(second.saved);
          assert.ok(second.revision > first.revision, `revision ${second.revision} after ${first.revision}`);
          const stored = await a.gateway("gw-1");
          assert.equal(stored?.revision, second.revision);
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
            Array.from({ length: 6 }, (_, i) =>
              (i % 2 === 0 ? a : b).saveGateway(gatewayRecord("gw-1", i % 3 !== 0, i), first.revision),
            ),
          );
          const written = results.filter((result) => result.saved);
          assert.equal(written.length, 1);
          assert.equal((await b.gateway("gw-1"))?.revision, written[0]!.revision);
          assert.notEqual(written[0]!.revision, first.revision);
        }),
      );

      test(
        "a gateway is forgotten only at the revision judged, and its revisions never come back",
        withStore(async (a, b) => {
          const live = await a.saveGateway(gatewayRecord("gw-1", true), undefined);
          const idle = await a.saveGateway(gatewayRecord("gw-2", false), undefined);
          assert.ok(live.saved && idle.saved);
          // A status taken meanwhile by the other process moves gw-1's revision on.
          const moved = await b.saveGateway(gatewayRecord("gw-1", true, 9), live.revision);
          assert.ok(moved.saved);
          assert.deepEqual(
            await a.forgetGateways([
              { instance: "gw-1", revision: live.revision },
              { instance: "gw-2", revision: idle.revision },
              { instance: "gw-unknown", revision: 1 },
            ]),
            ["gw-2"],
          );
          assert.deepEqual(await b.forgetGateways([{ instance: "gw-1", revision: moved.revision }]), ["gw-1"]);
          assert.equal(await a.gateway("gw-1"), undefined);
          // A forgotten gateway that reports again gets a revision it never had, so a
          // write computed from its forgotten record never matches the new one.
          const back = await b.saveGateway(gatewayRecord("gw-1", true), undefined);
          assert.ok(back.saved);
          assert.ok(![live.revision, moved.revision].includes(back.revision), `revision ${back.revision} repeats`);
          assert.deepEqual(await a.forgetGateways([{ instance: "gw-1", revision: live.revision }]), []);
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
            await publishConfigs(a, 1);
            await b.saveCountedBatch(counted("gw-1", 1), undefined, 10);
            await a.saveGateway(gatewayRecord("gw-1", true), undefined);
            const expected: StoreChange[] = [
              { type: "config-published", hash: hashOf(1) },
              { type: "batch-counted", instance: "gw-1" },
              { type: "gateways-changed", liveChanged: true },
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
        "every status is announced, the live set changed or not; forgetting a live gateway changes it",
        withStore(async (a, b) => {
          const heard = listen(b);
          try {
            const joined = await a.saveGateway(gatewayRecord("gw-1", true), undefined);
            assert.ok(joined.saved);
            const again = await a.saveGateway(gatewayRecord("gw-1", true, 5), joined.revision);
            assert.ok(again.saved);
            assert.deepEqual(await a.forgetGateways([{ instance: "gw-1", revision: again.revision }]), ["gw-1"]);
            await heard.until("three gateway changes", () => heard.changes.length >= 3);
            assert.deepEqual(
              heard.changes.map((change) => change.type === "gateways-changed" && change.liveChanged),
              [true, false, true],
            );
          } finally {
            heard.unsubscribe();
          }
        }),
      );

      test(
        "changes written at once through both handles are heard in one order by every subscriber",
        withStore(async (a, b) => {
          await publishConfigs(a, 1);
          const onA = listen(a);
          const onB = listen(b);
          try {
            await Promise.all(
              Array.from({ length: 12 }, (_, i) =>
                i % 3 === 0
                  ? (i % 2 === 0 ? a : b).saveGateway(gatewayRecord(`gw-${i}`, true), undefined)
                  : (i % 2 === 0 ? a : b).saveCountedBatch(counted(`gw-${i}`, 1), undefined, 10),
              ),
            );
            for (const heard of [onA, onB]) await heard.until("twelve changes", () => heard.changes.length >= 12);
            assert.equal(onA.changes.length, 12);
            assert.deepEqual(onB.changes, onA.changes);
          } finally {
            onA.unsubscribe();
            onB.unsubscribe();
          }
        }),
      );

      test(
        "a channel that reconnects announces a catch-up to every subscriber",
        { skip: subject.reconnect === undefined && "the store's channel never drops changes" },
        withStore(async (a, b) => {
          // Two subscribers on the reconnecting handle, as two cores share a process's
          // store handle.
          const first = listen(b);
          const second = listen(b);
          const catchUps = (heard: ReturnType<typeof listen>): number =>
            heard.changes.filter((change) => change.type === "catch-up").length;
          try {
            await publishConfigs(a, 1);
            for (const heard of [first, second]) await heard.until("the publish", () => heard.changes.length >= 1);
            for (let round = 1; round <= 2; round += 1) {
              // Written while the channel is down: the channel may miss them; the
              // catch-up must cover them.
              await subject.reconnect?.(b, async () => {
                assert.ok((await a.publishConfig(entry(round + 1), hashOf(round))).saved);
                assert.ok((await a.saveCountedBatch(counted("gw-1", round), round === 1 ? undefined : batchId("gw-1", round - 1), 10)).saved);
              });
              for (const heard of [first, second]) await heard.until(`catch-up ${round}`, () => catchUps(heard) >= round);
              // What a subscriber reads after the catch-up holds what the channel missed.
              assert.equal((await b.currentConfig())?.hash, hashOf(round + 1));
              assert.equal(countedIn(await b.totalsSnapshot(CURRENT), "gw-1"), round);
            }
            // The channel is back: changes are heard again.
            await a.saveGateway(gatewayRecord("gw-1", true), undefined);
            for (const heard of [first, second]) {
              await heard.until("a change after the reconnects", () => heard.changes.some((change) => change.type === "gateways-changed"));
            }
          } finally {
            first.unsubscribe();
            second.unsubscribe();
          }
        }),
      );

      test(
        "a change is heard after its write: a read in the listener sees what it announced",
        withStore(async (a, b) => {
          // Each kind of change read back by a listener on the other handle, as a core
          // does: the current config after a publish, the totals after a counted batch,
          // the gateway after a gateway change. A read that misses the change keeps a
          // core's streams on what was there before.
          const reads: Promise<string>[] = [];
          const unsubscribe = b.subscribe((change) => {
            if (change.type === "config-published") {
              reads.push(b.currentConfig().then((config) => `config ${config?.hash === change.hash ? "seen" : "missed"}`));
            } else if (change.type === "batch-counted") {
              reads.push(b.totalsSnapshot(CURRENT).then((snapshot) => `batch ${countedIn(snapshot, "gw-1")}`));
            } else if (change.type === "gateways-changed") {
              reads.push(b.gateway("gw-1").then((gateway) => `gateway ${gateway?.receivedAt}`));
            }
          });
          try {
            await publishConfigs(a, 2);
            await a.saveCountedBatch(counted("gw-1", 1), undefined, 10);
            await a.saveGateway(gatewayRecord("gw-1", true, 7), undefined);
            const deadline = Date.now() + notifyTimeoutMs;
            while (reads.length < 4 && Date.now() < deadline) await new Promise((resolve) => setTimeout(resolve, 10));
            assert.deepEqual(await Promise.all(reads), ["config seen", "config seen", "batch 1", "gateway 7"]);
          } finally {
            unsubscribe();
          }
        }),
      );

      test(
        "refused writes are not announced; unsubscribing stops the calls",
        withStore(async (a, b) => {
          await publishConfigs(a, 1);
          const heard = listen(b);
          await a.saveCountedBatch(counted("gw-1", 1), undefined, 10);
          await heard.until("the counted batch", () => heard.changes.length >= 1);
          // Refused: the same batch again, a stale publish, a stale gateway write.
          await a.saveCountedBatch(counted("gw-1", 1), undefined, 10);
          await a.publishConfig(entry(2), undefined);
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
