// The sample's read-only status page: GET / renders the control plane's state —
// gateways, config, totals against limits, recent usage — and GET /events keeps it
// current. No authentication: the page is for local and demo use, and it holds
// nothing secret (key IDs, never keys or hashes; never the gateway token).

import type { FastifyInstance } from "fastify";
import type { ControlPlane } from "kaiak-control";

import type { ConfigFileState } from "../config-file/index.ts";

import { CONTENT_SECURITY_POLICY, pageDocument } from "./document.ts";
import { createPageFeed } from "./feed.ts";
import { SECTION_IDS, renderSection } from "./sections.ts";
import type { PageSources } from "./sections.ts";

export interface StatusPageOptions {
  controlPlane: Pick<
    ControlPlane,
    "currentConfig" | "gateways" | "totals" | "recentRecords" | "onConfigPublished" | "onTotalsChanged" | "onGatewaysChanged"
  >;
  // The config file's latest runs, read at every render of the config section.
  configFile: () => ConfigFileState;
  // Milliseconds since the epoch. Default Date.now.
  clock?: () => number;
  // At most one push of changed sections per this many milliseconds. Default 1000.
  pushIntervalMs?: number;
  // Milliseconds between heartbeat comments on a page stream. Default 15000.
  heartbeatIntervalMs?: number;
  // A page stream whose browser has not read for this long is ended. Default 30000.
  stalledStreamTimeoutMs?: number;
  // Most page streams open at once; one more is refused (503). Default 32.
  maxStreams?: number;
}

export interface StatusPage {
  // Tells the page the config file had a run (the core announces publishes itself;
  // rejections and unchanged runs only the config file sees).
  configFileChanged(): void;
}

const DEFAULT_PUSH_INTERVAL_MS = 1_000;
const DEFAULT_HEARTBEAT_INTERVAL_MS = 15_000;
const DEFAULT_STALLED_STREAM_TIMEOUT_MS = 30_000;
// Page streams share the port with the gateways' config streams and take no key, so
// their number is bounded; a demo page has a handful of viewers.
const DEFAULT_MAX_STREAMS = 32;

export function registerStatusPage(app: FastifyInstance, options: StatusPageOptions): StatusPage {
  const sources: PageSources = { core: options.controlPlane, configFile: options.configFile, clock: options.clock ?? Date.now };
  const feed = createPageFeed({
    sources,
    core: options.controlPlane,
    pushIntervalMs: options.pushIntervalMs ?? DEFAULT_PUSH_INTERVAL_MS,
    heartbeatIntervalMs: options.heartbeatIntervalMs ?? DEFAULT_HEARTBEAT_INTERVAL_MS,
    stalledStreamTimeoutMs: options.stalledStreamTimeoutMs ?? DEFAULT_STALLED_STREAM_TIMEOUT_MS,
    maxStreams: options.maxStreams ?? DEFAULT_MAX_STREAMS,
    log: app.log,
  });

  app.get("/", async (_request, reply) => {
    const rendered = await Promise.all(SECTION_IDS.map(async (id) => [id, await renderSection(id, sources)] as const));
    return reply
      .header("content-security-policy", CONTENT_SECURITY_POLICY)
      .header("cache-control", "no-store")
      .header("x-content-type-options", "nosniff")
      .header("referrer-policy", "no-referrer")
      .type("text/html; charset=utf-8")
      .send(pageDocument(new Map(rendered)));
  });

  // A HEAD request would open a stream with no body to carry it.
  app.get("/events", { exposeHeadRoute: false }, async (_request, reply) => {
    if (!feed.stream(reply)) {
      return reply
        .code(503)
        .header("retry-after", "10")
        .send({ error: "page-streams-full", detail: `the page serves at most ${options.maxStreams ?? DEFAULT_MAX_STREAMS} page streams at once` });
    }
    // Hijacked: the feed writes the stream.
    return undefined;
  });

  // A page stream never ends by itself and would hold the server open.
  app.addHook("preClose", async () => {
    feed.endAll();
  });

  return { configFileChanged: () => feed.sectionsChanged(["config"]) };
}
