// GET /v1/stream (docs/specs/CONTROL-PROTOCOL.md, Config stream, Budgets): the current
// config on connect, then the totals, then the current config every time it changes
// and the totals whenever they or the live-gateway count change, at most once per push
// interval — as server-sent events written straight to the socket. The stream is the
// gateway's one way to get its config and its totals.

import { performance } from "node:perf_hooks";

import type { FastifyBaseLogger, FastifyReply } from "fastify";

import type { ControlPlane } from "../control-plane/index.ts";
import type { ConfigEvent, Totals } from "../messages/index.ts";
import type { CurrentConfig } from "../storage/index.ts";

export interface GatewayStreamOptions {
  core: Pick<ControlPlane, "currentConfig" | "onConfigPublished" | "totals" | "onTotalsChanged" | "onGatewaysChanged">;
  // The gateway's checked instance ID (the totals carry its counted_through).
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

// Resolves once the current config is read; the stream stays open until the client
// leaves, its socket stalls, or the app closes it (when the app stops, or when its core
// saw the store roll back).
export async function streamToGateway(reply: FastifyReply, options: GatewayStreamOptions): Promise<void> {
  const { core, instance, heartbeatIntervalMs, totalsPushIntervalMs, stalledStreamTimeoutMs, openStreams, log } = options;

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
  // The store's sequence of the config last sent on this stream: a config read before
  // it (a slow read overtaken by a faster one) is not sent. The sequence never goes on
  // the wire; a store that went back closes the stream instead (Rollback).
  let lastSent: number | undefined;
  // Configs heard of while the connect read is in progress wait here, so none is sent
  // before it. While it is set, totals wait too: they follow the first config.
  let pending: CurrentConfig[] | undefined = [];

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

  const sendConfig = (current: CurrentConfig): void => {
    if (lastSent !== undefined && current.sequence <= lastSent) return;
    lastSent = current.sequence;
    const event: ConfigEvent = { config_hash: current.hash, config: current.config };
    write(`event: config\ndata: ${JSON.stringify(event)}\n\n`);
  };

  // Totals are latest-wins: at most one push is waiting at a time — for the interval to
  // pass, for the socket to drain, or for a read in progress — and it reads the totals
  // when it is sent, so it always carries the newest.
  let lastTotalsAt = Number.NEGATIVE_INFINITY;
  let trailingPush: ReturnType<typeof setTimeout> | undefined;
  let readingTotals = false;
  let pushAfterRead = false;
  let pushAfterDrain = false;

  // Totals follow the first config: none is pushed before a config was sent.
  const requestTotals = (): void => {
    if (pending || lastSent === undefined || connection.signal.aborted || trailingPush !== undefined) return;
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

  // Subscribe before reading the current config, so one published in between is not
  // lost. A publish changes which limits the totals list, so totals follow it.
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

  let current: CurrentConfig | undefined;
  try {
    current = await core.currentConfig();
  } catch (error) {
    log.error({ err: error }, "config stream: reading the current config failed");
    raw.destroy();
    return;
  }
  if (connection.signal.aborted) return;
  // Before the first publish the stream stays open with nothing to send: the first
  // config arrives as soon as one is published.
  if (current) sendConfig(current);
  const publishedMeanwhile = pending;
  pending = undefined;
  for (const published of publishedMeanwhile) sendConfig(published);
  requestTotals();
}
