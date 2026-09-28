// GET /events: the page's live updates, one server-sent event stream per browser. On
// connect a browser gets every section (a reconnect included, so nothing it shows is
// stale); after that, a section whose state changed is re-rendered once and pushed to
// every browser as `event: section`, data `{"id", "html"}`. Pushes are coalesced — at
// most one per push interval, carrying every section changed since the last — because
// totals change with every usage batch.

import { performance } from "node:perf_hooks";

import type { FastifyBaseLogger, FastifyReply } from "fastify";
import type { ControlPlane } from "kaiak-control";

import { markupText } from "./html.ts";
import { SECTION_IDS, renderSection } from "./sections.ts";
import type { PageSources, SectionId } from "./sections.ts";

export interface PageFeedOptions {
  sources: PageSources;
  core: Pick<ControlPlane, "onConfigPublished" | "onTotalsChanged" | "onGatewaysChanged">;
  pushIntervalMs: number;
  heartbeatIntervalMs: number;
  // A stream whose browser has not read earlier events for this long is ended; the
  // browser reconnects and gets every section again.
  stalledStreamTimeoutMs: number;
  // Most streams open at once; stream refuses one more.
  maxStreams: number;
  log: FastifyBaseLogger;
}

export interface PageFeed {
  // Serves one browser's stream until it leaves or endAll runs; false, with the reply
  // untouched, when maxStreams are open already.
  stream(reply: FastifyReply): boolean;
  // Marks sections changed by something the core does not announce (the config file).
  sectionsChanged(ids: readonly SectionId[]): void;
  // Ends every open stream.
  endAll(): void;
}

interface Browser {
  write(chunk: string): void;
  end(): void;
}

const EVENT_STREAM_HEADERS = {
  "content-type": "text/event-stream; charset=utf-8",
  "cache-control": "no-cache",
  // Keeps proxies that buffer responses (nginx) from holding events back.
  "x-accel-buffering": "no",
};

const HEARTBEAT = ": heartbeat\n\n";

export function createPageFeed(options: PageFeedOptions): PageFeed {
  const { sources, core, pushIntervalMs, heartbeatIntervalMs, stalledStreamTimeoutMs, maxStreams, log } = options;
  // Every open stream; the core is subscribed to only while there is one.
  const open = new Set<Browser>();
  // Streams that have had their connect render and get pushes.
  const receiving = new Set<Browser>();
  // Set once a stream was refused at the cap, so a flood logs once until the count
  // falls below it again.
  let capWarned = false;
  let unsubscribes: (() => void)[] = [];

  // Renders — a connect's and pushes — take turns, so a browser never gets an older
  // render after a newer one.
  let turn: Promise<unknown> = Promise.resolve();
  const inTurn = (run: () => Promise<void>): void => {
    turn = turn.then(run).catch((error: unknown) => {
      // Logged here so one failed turn does not stop the ones after it.
      log.error({ err: error }, "page stream: sending sections failed");
    });
  };

  const dirty = new Set<SectionId>();
  let lastPushAt = Number.NEGATIVE_INFINITY;
  let pushTimer: ReturnType<typeof setTimeout> | undefined;
  let pushQueued = false;

  const renderEvents = async (ids: readonly SectionId[]): Promise<string | undefined> => {
    try {
      const rendered = await Promise.all(ids.map(async (id) => ({ id, html: markupText(await renderSection(id, sources)) })));
      return rendered.map((section) => `event: section\ndata: ${JSON.stringify(section)}\n\n`).join("");
    } catch (error) {
      log.error({ err: error, sections: ids }, "page: rendering sections failed");
      return undefined;
    }
  };

  const queuePush = (): void => {
    pushQueued = true;
    lastPushAt = performance.now();
    inTurn(async () => {
      pushQueued = false;
      // Sections changed while this renders wait for the next push.
      const ids = SECTION_IDS.filter((id) => dirty.has(id));
      dirty.clear();
      if (ids.length === 0 || receiving.size === 0) return;
      const chunk = await renderEvents(ids);
      if (chunk === undefined) return;
      for (const browser of receiving) browser.write(chunk);
    });
  };

  const markChanged = (ids: readonly SectionId[]): void => {
    if (open.size === 0) return;
    for (const id of ids) dirty.add(id);
    if (pushQueued || pushTimer !== undefined) return;
    const wait = lastPushAt + pushIntervalMs - performance.now();
    if (wait <= 0) {
      queuePush();
      return;
    }
    pushTimer = setTimeout(() => {
      pushTimer = undefined;
      queuePush();
    }, wait);
  };

  const subscribe = (): void => {
    unsubscribes = [
      core.onConfigPublished(() => markChanged(["config", "totals", "gateways"])),
      core.onTotalsChanged(() => markChanged(["totals", "usage"])),
      // The totals section shows the live-gateway count.
      core.onGatewaysChanged((change) => markChanged(change.liveChanged ? ["gateways", "totals"] : ["gateways"])),
    ];
  };

  const unsubscribe = (): void => {
    for (const stop of unsubscribes) stop();
    unsubscribes = [];
    clearTimeout(pushTimer);
    pushTimer = undefined;
    dirty.clear();
  };

  return {
    stream(reply) {
      if (open.size >= maxStreams) {
        if (!capWarned) log.warn({ maxStreams }, "page stream refused: the page streams are at their cap");
        capWarned = true;
        return false;
      }
      reply.hijack();
      const raw = reply.raw;
      raw.writeHead(200, EVENT_STREAM_HEADERS);
      raw.flushHeaders();

      // Writes go through Node's own buffer; a socket that takes nothing for the stall
      // timeout ends the stream.
      let stallTimer: ReturnType<typeof setTimeout> | undefined;
      const drained = (): void => {
        clearTimeout(stallTimer);
        stallTimer = undefined;
      };
      const browser: Browser = {
        write(chunk) {
          if (raw.write(chunk) || stallTimer !== undefined) return;
          stallTimer = setTimeout(() => {
            log.warn({ stalledStreamTimeoutMs }, "page stream: the browser has not read for too long; ending the stream");
            raw.destroy();
          }, stalledStreamTimeoutMs);
          raw.once("drain", drained);
        },
        end() {
          raw.end();
        },
      };
      const heartbeat = setInterval(() => {
        if (!raw.writableNeedDrain) browser.write(HEARTBEAT);
      }, heartbeatIntervalMs);

      open.add(browser);
      if (open.size === 1) subscribe();
      const close = (): void => {
        clearInterval(heartbeat);
        clearTimeout(stallTimer);
        receiving.delete(browser);
        open.delete(browser);
        if (open.size < maxStreams) capWarned = false;
        if (open.size === 0) unsubscribe();
      };
      raw.once("close", close);
      if (raw.destroyed) {
        close();
        return true;
      }

      inTurn(async () => {
        if (!open.has(browser)) return;
        const chunk = await renderEvents(SECTION_IDS);
        if (!open.has(browser)) return;
        if (chunk === undefined) {
          raw.destroy();
          return;
        }
        browser.write(chunk);
        receiving.add(browser);
      });
      return true;
    },

    sectionsChanged: markChanged,

    endAll() {
      for (const browser of open) browser.end();
    },
  };
}
