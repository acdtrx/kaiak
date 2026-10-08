// GET /v1/stream (docs/specs/CONTROL-PROTOCOL.md, Config stream, Budgets): the current
// config on connect, then the totals — complete — then the current config after every
// change and the totals that changed, as server-sent events written straight to the
// socket. The stream is the gateway's one way to get its config and its totals.

import type { FastifyBaseLogger, FastifyReply } from "fastify";

import type { ConfigRead } from "../config-publishing/index.ts";
import type { ControlPlane } from "../control-plane/index.ts";
import { scopeTypeKey } from "../messages/index.ts";
import type { Totals, TotalsWindow } from "../messages/index.ts";

import type { TotalsFeed } from "./totals-feed.ts";

export interface GatewayStreamOptions {
  core: Pick<ControlPlane, "readConfig" | "onConfigRead">;
  // The core's totals, shared by every stream.
  totals: TotalsFeed;
  // The gateway's checked instance ID (the totals carry its counted_through).
  instance: string;
  heartbeatIntervalMs: number;
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
// leaves, its socket stalls or fails, or the app closes it (when the app stops, or when
// its core could not read the current config after a change).
export async function streamToGateway(reply: FastifyReply, options: GatewayStreamOptions): Promise<void> {
  const { core, totals, instance, heartbeatIntervalMs, stalledStreamTimeoutMs, openStreams, log } = options;

  // The stream bypasses Fastify's reply handling; the headers set so far (the protocol
  // version) go out with the event-stream ones.
  reply.hijack();
  const raw = reply.raw;
  for (const [name, value] of Object.entries(reply.getHeaders())) {
    if (value !== undefined) raw.setHeader(name, value);
  }
  raw.writeHead(200, EVENT_STREAM_HEADERS);
  raw.flushHeaders();

  // Once closed, nothing more is written or read for this stream: closing is the first
  // thing every ending does, before the response ends, so a delivery running in the
  // same turn finds the stream closed.
  let closed = false;
  // The read order of the config last sent and its hash: a config read issued before
  // it is not sent, and a config that is the one last sent is not sent again.
  let lastRead = Number.NEGATIVE_INFINITY;
  let lastHash: string | undefined;

  // Writes go through Node's own buffer, so a client slow to read only delays config
  // events, never loses them. A socket that takes nothing for the stall timeout ends
  // the stream: the gateway reconnects and reads the current config again.
  let stallTimer: ReturnType<typeof setTimeout> | undefined;
  const write = (chunk: string): void => {
    if (closed) return;
    if (raw.write(chunk) || stallTimer !== undefined) return;
    stallTimer = setTimeout(() => {
      log.warn({ stalledStreamTimeoutMs }, "config stream: the gateway has not read for too long; ending the stream");
      close();
      raw.destroy();
    }, stalledStreamTimeoutMs);
    raw.once("drain", drained);
  };

  const sendConfig = ({ published, read }: ConfigRead): void => {
    if (closed || read <= lastRead) return;
    lastRead = read;
    if (published.hash === lastHash) return;
    lastHash = published.hash;
    // The stored text goes out as it is, so config_hash is the hash of what is sent.
    write(`event: config\ndata: {"config_hash":${JSON.stringify(published.hash)},"config":${published.text}}\n\n`);
    sendTotals();
  };

  // Totals follow the first config. The first totals are complete, from a read issued
  // after the stream joined the feed; each later one lists the windows changed since
  // the last one sent, with the instance's cursors and the live count of the latest
  // read. While the socket waits to drain, changes gather
  // here and go out at once on drain, at their newest.
  let complete = true;
  const changed = new Map<string, TotalsWindow>();
  let sentLive: number | undefined;
  let sentCursors: string | undefined;

  const sendTotals = (): void => {
    const state = totals.state;
    if (closed || !state || lastHash === undefined || raw.writableNeedDrain) return;
    if (complete && state.read < membership.firstRead) return;
    const counted_through = state.cursors
      .filter((cursor) => cursor.instance === instance)
      .map(({ epoch, sequence }) => ({ epoch, sequence }));
    const cursors = JSON.stringify(counted_through);
    if (!complete && changed.size === 0 && state.liveGateways === sentLive && cursors === sentCursors) return;
    const message: Totals = {
      live_gateways: state.liveGateways,
      counted_through,
      windows: [...(complete ? state.windows : changed).values()],
    };
    complete = false;
    changed.clear();
    sentLive = state.liveGateways;
    sentCursors = cursors;
    write(`event: totals\ndata: ${JSON.stringify(message)}\n\n`);
  };

  function drained(): void {
    clearTimeout(stallTimer);
    stallTimer = undefined;
    sendTotals();
  }

  // Subscribe before reading the current config, so one published in between is not
  // lost.
  const unsubscribeConfigs = core.onConfigRead(sendConfig);
  const membership = totals.join({
    take(windows) {
      if (!complete) for (const window of windows) changed.set(scopeTypeKey(window.group, window.type), window);
      sendTotals();
    },
  });
  // A heartbeat is skipped while earlier writes wait to drain — the connection is
  // plainly not idle.
  const heartbeat = setInterval(() => {
    if (!raw.writableNeedDrain) write(HEARTBEAT);
  }, heartbeatIntervalMs);

  function close(): void {
    if (closed) return;
    closed = true;
    unsubscribeConfigs();
    membership.leave();
    clearInterval(heartbeat);
    clearTimeout(stallTimer);
    openStreams.delete(end);
  }
  function end(): void {
    close();
    raw.end();
  }
  openStreams.add(end);
  raw.once("close", close);
  // A failing socket ends the stream like a closed one; the error is the connection's,
  // never the process's.
  raw.on("error", (error) => {
    log.warn({ err: error }, "config stream: the connection failed; ending the stream");
    close();
  });
  if (raw.destroyed) {
    close();
    return;
  }

  let current: ConfigRead | undefined;
  try {
    current = await core.readConfig();
  } catch (error) {
    log.error({ err: error }, "config stream: reading the current config failed");
    close();
    raw.destroy();
    return;
  }
  // Before the first publish the stream stays open with nothing to send: the first
  // config arrives as soon as one is published.
  if (current) sendConfig(current);
}
