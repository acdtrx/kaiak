// GET /v1/stream (docs/specs/CONTROL-PROTOCOL.md, Config stream, Budgets): the config
// versions newer than the gateway's, then every version published while it stays
// connected, and the totals — on connect, then whenever they or the live-gateway count
// change, at most once per push interval — as server-sent events written straight to
// the socket.

import { performance } from "node:perf_hooks";

import type { FastifyBaseLogger, FastifyReply } from "fastify";

import type { ConfigPosition } from "../config-versions/index.ts";
import type { ControlPlane } from "../control-plane/index.ts";
import type { ConfigSnapshot, Totals } from "../messages/index.ts";
import type { StoredConfig } from "../storage/index.ts";

export interface GatewayStreamOptions {
  core: Pick<ControlPlane, "configsSince" | "onConfigPublished" | "totals" | "onTotalsChanged" | "onGatewaysChanged">;
  // The version the gateway runs with its epoch, and its checked instance ID (the
  // totals carry its counted_through).
  since: ConfigPosition;
  instance: string;
  heartbeatIntervalMs: number;
  // At most one totals event per this many milliseconds.
  totalsPushIntervalMs: number;
  // A stream whose socket has not taken earlier writes for this long is ended.
  stalledStreamTimeoutMs: number;
  // Holds this stream's end function while it is open.
  openStreams: Set<() => void>;
  log: FastifyBaseLogger;
}

const EVENT_STREAM_HEADERS = {
  "content-type": "text/event-stream; charset=utf-8",
  "cache-control": "no-cache",
  // Keeps proxies that buffer responses (nginx) from holding events back.
  "x-accel-buffering": "no",
};

const HEARTBEAT = ": heartbeat\n\n";
const RESYNC_EVENT = "event: resync\ndata: {}\n\n";

// Resolves once the replay is written; the stream stays open until the client leaves,
// a resync ends it, its socket stalls, or the app closes.
export async function streamToGateway(reply: FastifyReply, options: GatewayStreamOptions): Promise<void> {
  const { core, since, instance, heartbeatIntervalMs, totalsPushIntervalMs, stalledStreamTimeoutMs, openStreams, log } = options;

  // The stream bypasses Fastify's reply handling; the headers set so far (the protocol
  // version) go out with the event-stream ones.
  reply.hijack();
  const raw = reply.raw;
  for (const [name, value] of Object.entries(reply.getHeaders())) {
    if (value !== undefined) raw.setHeader(name, value);
  }
  raw.writeHead(200, EVENT_STREAM_HEADERS);
  raw.flushHeaders();

  const connection = new AbortController();
  // Versions at or below this one are not sent again: a version published while the
  // replay is read arrives both in the replay and from the subscription.
  let lastSent = since.version;
  // The store's epoch, which every config event carries; known once the replay is read
  // (config events wait in pending until then).
  let epoch = "";
  // Versions published before the replay is written wait here, so none is sent out of
  // order. While it is set, totals wait too: the connect push follows the replay.
  let pending: StoredConfig[] | undefined = [];

  // Writes go through Node's own buffer, so a client slow to read only delays config
  // events, never loses them. A socket that takes nothing for the stall timeout ends
  // the stream: the gateway reconnects and resumes from the version it runs.
  let stallTimer: ReturnType<typeof setTimeout> | undefined;
  const write = (chunk: string): void => {
    if (raw.write(chunk) || stallTimer !== undefined) return;
    stallTimer = setTimeout(() => {
      log.warn({ stalledStreamTimeoutMs }, "config stream: the gateway has not read for too long; ending the stream");
      raw.destroy();
    }, stalledStreamTimeoutMs);
    raw.once("drain", drained);
  };

  const sendConfig = (entry: StoredConfig): void => {
    if (entry.version <= lastSent) return;
    lastSent = entry.version;
    const snapshot: ConfigSnapshot = { config_epoch: epoch, version: entry.version, config: entry.config };
    write(`event: config\nid: ${entry.version}\ndata: ${JSON.stringify(snapshot)}\n\n`);
  };

  // Totals are latest-wins: at most one push is waiting at a time — for the interval to
  // pass, for the socket to drain, or for a read in progress — and it reads the totals
  // when it is sent, so it always carries the newest.
  let lastTotalsAt = Number.NEGATIVE_INFINITY;
  let trailingPush: ReturnType<typeof setTimeout> | undefined;
  let readingTotals = false;
  let pushAfterRead = false;
  let pushAfterDrain = false;

  const requestTotals = (): void => {
    if (pending || connection.signal.aborted || trailingPush !== undefined) return;
    const wait = lastTotalsAt + totalsPushIntervalMs - performance.now();
    if (wait > 0) {
      trailingPush = setTimeout(() => {
        trailingPush = undefined;
        void pushTotals();
      }, wait);
      return;
    }
    void pushTotals();
  };

  const pushTotals = async (): Promise<void> => {
    if (readingTotals) {
      pushAfterRead = true;
      return;
    }
    if (raw.writableNeedDrain) {
      pushAfterDrain = true;
      return;
    }
    readingTotals = true;
    lastTotalsAt = performance.now();
    let totals: Totals | undefined;
    try {
      totals = await core.totals(instance);
    } catch (error) {
      log.error({ err: error }, "config stream: reading the totals to push failed");
    }
    readingTotals = false;
    if (connection.signal.aborted) return;
    // Undefined before the first config: there is nothing to push.
    if (totals) write(`event: totals\ndata: ${JSON.stringify(totals)}\n\n`);
    if (pushAfterRead) {
      pushAfterRead = false;
      requestTotals();
    }
  };

  function drained(): void {
    clearTimeout(stallTimer);
    stallTimer = undefined;
    if (!pushAfterDrain) return;
    pushAfterDrain = false;
    requestTotals();
  }

  // Subscribe before reading the replay, so a version published in between is not lost.
  // A publish changes the totals' config version and limits, so totals follow it.
  const unsubscribeConfigs = core.onConfigPublished((published) => {
    if (pending) {
      pending.push(published);
      return;
    }
    sendConfig(published);
    requestTotals();
  });
  const unsubscribeTotals = core.onTotalsChanged(requestTotals);
  const unsubscribeGateways = core.onGatewaysChanged((change) => {
    if (change.liveChanged) requestTotals();
  });
  // A heartbeat is skipped while earlier writes wait to drain — the connection is
  // plainly not idle.
  const heartbeat = setInterval(() => {
    if (!raw.writableNeedDrain) write(HEARTBEAT);
  }, heartbeatIntervalMs);
  const end = (): void => {
    raw.end();
  };
  openStreams.add(end);
  const close = (): void => {
    connection.abort();
    unsubscribeConfigs();
    unsubscribeTotals();
    unsubscribeGateways();
    clearInterval(heartbeat);
    clearTimeout(trailingPush);
    clearTimeout(stallTimer);
    openStreams.delete(end);
  };
  raw.once("close", close);
  if (raw.destroyed) {
    close();
    return;
  }

  let answer: Awaited<ReturnType<typeof core.configsSince>>;
  try {
    answer = await core.configsSince(since);
  } catch (error) {
    log.error({ err: error }, "config stream: reading the versions to replay failed");
    raw.destroy();
    return;
  }
  if (connection.signal.aborted) return;
  if (answer.resync) {
    // The gateway fetches the snapshot and reconnects from its version.
    raw.end(RESYNC_EVENT);
    return;
  }
  epoch = answer.epoch;
  for (const entry of answer.configs) sendConfig(entry);
  const publishedMeanwhile = pending;
  pending = undefined;
  for (const entry of publishedMeanwhile) sendConfig(entry);
  requestTotals();
}
