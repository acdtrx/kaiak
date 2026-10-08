// The totals every stream of one core sends (docs/specs/CONTROL-PROTOCOL.md, Config
// stream → Totals; Messages → Totals): one store snapshot per push for all of them.
// Each read is compared with the one before it, and the windows that changed go to
// every stream. A stream that joins starts from the whole of the first read issued
// after it joined — never an earlier one, which could be older than what another
// process already sent its gateway, or the last good read of a feed whose reads now
// fail. Reads run one at a time, at most one per push interval, so what a stream sends
// is always from a read issued after the one it sent before.

import { performance } from "node:perf_hooks";

import type { FastifyBaseLogger } from "fastify";

import type { ControlPlane } from "../control-plane/index.ts";
import { scopeTypeKey } from "../messages/index.ts";
import type { BatchId, TotalsWindow } from "../messages/index.ts";

// The latest read: every window with usage by scope and type, every instance's cursors
// and the live count, with the read's place in the order the feed issued its reads.
export interface TotalsState {
  windows: ReadonlyMap<string, TotalsWindow>;
  cursors: readonly BatchId[];
  liveGateways: number;
  read: number;
}

// A stream's place in the feed: how to leave, and the first read it may start from.
export interface TotalsMembership {
  // Stops following; safe to call more than once.
  leave(): void;
  // The number of the first read issued after the join: complete totals come from a
  // read at or past it.
  firstRead: number;
}

// A stream following the totals: told of every read with the windows it changed (none,
// when only the cursors or the live count moved).
export interface TotalsSubscriber {
  take(changed: readonly TotalsWindow[]): void;
}

export interface TotalsFeed {
  // The latest read, undefined before the first.
  readonly state: TotalsState | undefined;
  // Follows the totals from now on, and issues a read for the stream to start from.
  join(subscriber: TotalsSubscriber): TotalsMembership;
  // Stops reading; nothing is read or told after it.
  close(): void;
}

export interface TotalsFeedOptions {
  core: Pick<ControlPlane, "readTotals" | "onTotalsChanged" | "onGatewaysChanged">;
  // At most one read per this many milliseconds.
  intervalMs: number;
  log: FastifyBaseLogger;
}

export function createTotalsFeed({ core, intervalMs, log }: TotalsFeedOptions): TotalsFeed {
  const subscribers = new Set<TotalsSubscriber>();
  let state: TotalsState | undefined;
  let closed = false;
  let reading = false;
  let readAgain = false;
  // Every read takes the next number when it is issued.
  let nextRead = 0;
  let lastReadAt = Number.NEGATIVE_INFINITY;
  let trailingRead: ReturnType<typeof setTimeout> | undefined;
  // A failed read is retried after the interval until one succeeds; the first failure
  // of a run is logged, not every retry.
  let failing = false;

  // The first change after a quiet interval is read at once; later ones within it
  // become one trailing read, and one arriving while a read runs, a read after it.
  const requestRead = (): void => {
    if (closed || trailingRead !== undefined) return;
    if (reading) {
      readAgain = true;
      return;
    }
    const wait = lastReadAt + intervalMs - performance.now();
    if (wait > 0) {
      trailingRead = setTimeout(() => {
        trailingRead = undefined;
        void read();
      }, wait);
      return;
    }
    void read();
  };

  const read = async (): Promise<void> => {
    reading = true;
    const number = nextRead;
    nextRead += 1;
    lastReadAt = performance.now();
    let next: Awaited<ReturnType<typeof core.readTotals>> | undefined;
    try {
      next = await core.readTotals();
    } catch (error) {
      if (!failing) log.error({ err: error }, "config stream: reading the totals to push failed; retrying");
      failing = true;
    }
    reading = false;
    if (closed) return;
    if (!next) {
      requestRead();
      return;
    }
    if (failing) log.info("config stream: reading the totals to push works again");
    failing = false;
    const windows = new Map(next.windows.map((window) => [scopeTypeKey(window.group, window.type), window]));
    const changed = state ? changedWindows(state.windows, windows, next.windowStarts) : [...windows.values()];
    state = { windows, cursors: next.cursors, liveGateways: next.liveGateways, read: number };
    for (const subscriber of [...subscribers]) subscriber.take(changed);
    if (readAgain) {
      readAgain = false;
      requestRead();
    }
  };

  const unsubscribeTotals = core.onTotalsChanged(requestRead);
  const unsubscribeGateways = core.onGatewaysChanged((change) => {
    if (change.liveChanged) requestRead();
  });

  return {
    get state() {
      return state;
    },
    join(subscriber) {
      subscribers.add(subscriber);
      // The number the next read issued takes: a read already running took its number
      // before the join and does not count. requestRead issues one (after the running
      // one, if any).
      const firstRead = nextRead;
      requestRead();
      return {
        leave() {
          subscribers.delete(subscriber);
        },
        firstRead,
      };
    },
    close() {
      closed = true;
      unsubscribeTotals();
      unsubscribeGateways();
      clearTimeout(trailingRead);
      subscribers.clear();
    },
  };
}

// The windows that differ between two reads: new or changed ones, and — listed at
// "0" — one the earlier read had in the same current window that the later one lacks
// (a store restored to less). A window the later read lacks because its window ended
// is not listed: the gateway starts the new window itself.
function changedWindows(
  before: ReadonlyMap<string, TotalsWindow>,
  after: ReadonlyMap<string, TotalsWindow>,
  windowStarts: Readonly<Record<TotalsWindow["type"], string>>,
): TotalsWindow[] {
  const changed: TotalsWindow[] = [];
  for (const [identity, window] of after) {
    const previous = before.get(identity);
    if (previous?.window_start !== window.window_start || previous.used !== window.used) changed.push(window);
  }
  for (const [identity, window] of before) {
    if (!after.has(identity) && window.window_start === windowStarts[window.type]) changed.push({ ...window, used: "0" });
  }
  return changed;
}
