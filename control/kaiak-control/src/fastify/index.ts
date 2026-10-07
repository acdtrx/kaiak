// The Fastify adapter for the control protocol (docs/specs/CONTROL-PROTOCOL.md): the
// gateway endpoints as routes over the framework-agnostic core. It registers routes
// only — logging and everything else about the app belong to the host.

import type { FastifyInstance, FastifyPluginAsync, FastifyRequest } from "fastify";

import type { ControlPlane } from "../control-plane/index.ts";
import { PROTOCOL_HEADER, PROTOCOL_VERSION, errorBody } from "../protocol/index.ts";
import type { ErrorBody } from "../protocol/index.ts";

import { streamToGateway } from "./gateway-stream.ts";
import { createTotalsFeed } from "./totals-feed.ts";

export interface ControlProtocolPluginOptions {
  controlPlane: ControlPlane;
  // Where the routes are mounted (Fastify's register option). Default "/v1".
  prefix?: string;
  // Milliseconds between heartbeat comments on an idle stream. Default 15000.
  heartbeatIntervalMs?: number;
  // At most one totals read, and so one totals event per stream, per this many
  // milliseconds. Default 1000.
  totalsPushIntervalMs?: number;
  // A stream whose gateway has not read earlier events for this many milliseconds is
  // ended, so the gateway reconnects. Default 30000.
  stalledStreamTimeoutMs?: number;
}

const DEFAULT_PREFIX = "/v1";
const DEFAULT_HEARTBEAT_INTERVAL_MS = 15_000;
const DEFAULT_TOTALS_PUSH_INTERVAL_MS = 1_000;
const DEFAULT_STALLED_STREAM_TIMEOUT_MS = 30_000;
// A usage batch holds at most 500 records. With every field at its longest (an 8-group
// path of 128-character IDs, a 512-character backend model name) a record is about
// 2.9 KB and a full batch about 0.69 of this limit. Only a backend model name made of
// characters Go's JSON encoder writes as six bytes (<, >, &) takes a batch past it (up
// to about 1.3×); the body is then refused.
const USAGE_BODY_LIMIT_BYTES = 2 * 1024 * 1024;
// A status names each backend, deployment and model once: a few KiB for a large
// deployment.
const STATUS_BODY_LIMIT_BYTES = 64 * 1024;

export const controlProtocolPlugin: FastifyPluginAsync<ControlProtocolPluginOptions> = async (instance, options) => {
  // Fastify has already applied a prefix the host passed; without one the routes go
  // under the default.
  await instance.register(async (routes) => registerGatewayRoutes(routes, options), {
    prefix: options.prefix === undefined ? DEFAULT_PREFIX : "",
  });
};

function registerGatewayRoutes(routes: FastifyInstance, options: ControlProtocolPluginOptions): void {
  const {
    controlPlane,
    heartbeatIntervalMs = DEFAULT_HEARTBEAT_INTERVAL_MS,
    totalsPushIntervalMs = DEFAULT_TOTALS_PUSH_INTERVAL_MS,
    stalledStreamTimeoutMs = DEFAULT_STALLED_STREAM_TIMEOUT_MS,
  } = options;
  // Ends each open stream; run when the app closes, since a stream never ends by itself
  // and would hold the server open.
  const openStreams = new Set<() => void>();
  // The instance each request's checks accepted, for routes whose body names it too.
  // Kept here rather than as a request decoration, whose type would be declared on
  // every FastifyRequest of the host app.
  const checkedInstances = new WeakMap<FastifyRequest, string>();
  const instanceOf = (request: FastifyRequest): string => {
    const instance = checkedInstances.get(request);
    if (instance === undefined) {
      throw Object.assign(new Error("a gateway route ran without the request checks"), { code: "instance-unchecked" });
    }
    return instance;
  };

  // Every response carries the protocol version; every request passes the checks
  // before its route runs. The hook covers every answer under the plugin's prefix —
  // the routes', Fastify's own refusals (bodies too large or malformed) and the
  // not-found handler below. A URL Fastify cannot decode is refused before any
  // prefix applies, by the host's app; gateways never send one.
  routes.addHook("onRequest", async (request, reply) => {
    reply.header(PROTOCOL_HEADER, String(PROTOCOL_VERSION));
    const check = controlPlane.checkGatewayRequest(request.headers);
    if (check.ok) {
      checkedInstances.set(request, check.instance);
      return;
    }
    return reply.code(check.error.status).send(errorBody(check.error));
  });

  // One totals read per push serves every stream of this core.
  const totals = createTotalsFeed({ core: controlPlane, intervalMs: totalsPushIntervalMs, log: routes.log });
  // A config this core could not read after a change, retries included, may be missing
  // from its streams: every stream ends, and the gateways reconnect and read the
  // current config on connect.
  const unsubscribeDeliveryFailed = controlPlane.onDeliveryFailed((error) => {
    routes.log.error(
      { err: error, streams: openStreams.size },
      "reading the current config after a change failed; ending every gateway stream",
    );
    for (const end of openStreams) end();
  });
  // The core starts with the app: it runs the expiry sweep, without which the live set
  // never shrinks.
  routes.addHook("onReady", async () => {
    await controlPlane.start();
  });
  routes.addHook("preClose", async () => {
    for (const end of openStreams) end();
  });
  routes.addHook("onClose", async () => {
    unsubscribeDeliveryFailed();
    totals.close();
    await controlPlane.stop();
  });

  // Failures keep the protocol's { error, detail } shape. A 4xx is the caller's (what
  // Fastify itself refuses); anything else is ours and is logged, not shown.
  routes.setErrorHandler((error, request, reply) => {
    const status = statusOf(error);
    if (status < 500) {
      const body: ErrorBody = { error: "request-invalid", detail: error instanceof Error ? error.message : String(error) };
      return reply.code(status).send(body);
    }
    request.log.error({ err: error }, "control-protocol request failed");
    const body: ErrorBody = { error: "internal-error" };
    return reply.code(500).send(body);
  });

  // A path or method the protocol does not define, after the request checks.
  routes.setNotFoundHandler(async (request, reply) => {
    const body: ErrorBody = { error: "not-found", detail: `no ${request.method} ${request.url} in the control protocol` };
    return reply.code(404).send(body);
  });

  routes.post("/usage", { bodyLimit: USAGE_BODY_LIMIT_BYTES }, async (request, reply) => {
    const intake = await controlPlane.acceptUsageBatch(instanceOf(request), request.body);
    if (!intake.ok) {
      const body: ErrorBody = { error: intake.error.code, detail: intake.error.message };
      return reply.code(intake.error.status).send(body);
    }
    const { batch } = intake.ack;
    if (intake.outcome === "new-epoch") {
      request.log.info({ batch, previous: intake.previous }, "usage batch starts a new epoch");
    } else if (intake.outcome === "gap") {
      request.log.warn({ batch, previous: intake.previous }, "usage batch sequence skips batches; counting it");
    } else if (intake.outcome === "duplicate") {
      request.log.info({ batch, previous: intake.previous }, "usage batch already counted; acked again");
    }
    return intake.ack;
  });

  // Accepted before any config is published: a starting gateway reports with nothing
  // applied.
  routes.post("/status", { bodyLimit: STATUS_BODY_LIMIT_BYTES }, async (request, reply) => {
    const instance = instanceOf(request);
    const intake = await controlPlane.acceptStatus(instance, request.body);
    if (!intake.ok) {
      const body: ErrorBody = { error: intake.error.code, detail: intake.error.message };
      return reply.code(intake.error.status).send(body);
    }
    if (intake.joined) request.log.info({ instance }, "gateway joined the live set");
    if (intake.conflictStarted) {
      request.log.warn(
        { instance },
        "gateway statuses alternate between two start times: two processes may share this instance name",
      );
    }
    return reply.code(204).send();
  });

  // A HEAD request would open a stream with no body to carry it.
  routes.get("/stream", { exposeHeadRoute: false }, async (request, reply) => {
    await streamToGateway(reply, {
      core: controlPlane,
      totals,
      instance: instanceOf(request),
      heartbeatIntervalMs,
      stalledStreamTimeoutMs,
      openStreams,
      log: request.log,
    });
  });
}

function statusOf(error: unknown): number {
  const status = typeof error === "object" && error !== null && "statusCode" in error ? error.statusCode : undefined;
  return typeof status === "number" && status >= 400 && status < 600 ? status : 500;
}
