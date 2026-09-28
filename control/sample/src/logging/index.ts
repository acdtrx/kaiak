// Logger settings for the sample's Fastify app (docs/TECH-STACK.md, Logging): plain
// JSON lines in production; pino-pretty, compact and single-line, for development.

import type { FastifyServerOptions } from "fastify";

import type { LogFormat } from "../settings/index.ts";

export type LoggerOptions = NonNullable<FastifyServerOptions["logger"]>;

export function loggerOptions(format: LogFormat): LoggerOptions {
  if (format === "json") return true;
  return {
    transport: {
      target: "pino-pretty",
      options: {
        translateTime: "SYS:HH:MM:ss.l",
        singleLine: true,
        ignore: "pid,hostname,reqId,req.host,req.remoteAddress,req.remotePort",
      },
    },
  };
}
