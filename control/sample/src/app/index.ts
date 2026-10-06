// The sample control plane's app: a Fastify instance with the kaiak-control plugin
// mounted over a core on the in-memory store, fed by the watched config file, and the
// read-only status page. Built
// here with injected options; a thin main reads the environment and listens.

import Fastify from "fastify";
import type { FastifyBaseLogger, FastifyInstance } from "fastify";
import { controlProtocolPlugin, createControlPlane, createMemoryStore } from "kaiak-control";
import type { ControlPlane, ExpirySweepRun, ListenerEvent } from "kaiak-control";

import { createConfigFile } from "../config-file/index.ts";
import type { ConfigFile, ReloadRun } from "../config-file/index.ts";
import type { LoggerOptions } from "../logging/index.ts";
import { registerStatusPage } from "../page/index.ts";

export interface SampleAppOptions {
  // Path of the config file to serve.
  configFile: string;
  // The bearer token gateways present.
  token: string;
  // Fastify's logger option.
  logger: LoggerOptions;
  // Quiet time after the last change to the file before it is reloaded. Default 200.
  configDebounceMs?: number;
}

export interface SampleApp {
  app: FastifyInstance;
  controlPlane: ControlPlane;
  configFile: ConfigFile;
}

export const STARTUP_TRIGGER = "startup";

// The store is in memory and serves one process: nothing survives a restart.
const IN_MEMORY_WARNING =
  "state is kept in memory: budgets, usage totals and usage batch de-duplication reset when this process restarts — the sample is not a billing system";

// Builds the app. The config file is first read when the app becomes ready (before it
// listens), and watched from then until it closes. A file that is missing or invalid
// at startup does not stop the app: gateways get 503 config-unavailable until the file
// is fixed.
export function createSampleApp(options: SampleAppOptions): SampleApp {
  const app = Fastify({ logger: options.logger });
  const log = app.log;
  const controlPlane = createControlPlane({
    store: createMemoryStore(),
    token: options.token,
    onListenerError: (error: unknown, event: ListenerEvent) =>
      log.error({ err: error, event: event.type }, "control-plane listener failed"),
    onExpirySweep: (run) => logExpirySweep(log, run),
  });
  const page = registerStatusPage(app, { controlPlane, configFile: () => configFile.state() });
  const configFile = createConfigFile({
    path: options.configFile,
    controlPlane,
    onReload: (run) => {
      logReload(log, run);
      page.configFileChanged();
    },
    onWatchError: (error) => log.error({ err: error }, "config file watch failed; reload by restarting"),
    ...(options.configDebounceMs !== undefined && { debounceMs: options.configDebounceMs }),
  });

  app.register(controlProtocolPlugin, { controlPlane });
  // Watching starts before the first read, so an edit between the two is not missed
  // (on macOS the watch goes live a moment later; see ConfigFile.startWatching).
  app.addHook("onReady", async () => {
    log.warn(IN_MEMORY_WARNING);
    configFile.startWatching();
    await configFile.reload(STARTUP_TRIGGER);
  });
  app.addHook("onClose", async () => {
    configFile.stopWatching();
  });

  return { app, controlPlane, configFile };
}

function logReload(log: FastifyBaseLogger, run: ReloadRun): void {
  if (run.ok && run.outcome === "published") {
    log.info({ trigger: run.trigger, version: run.version }, `config file published as version ${run.version}`);
  } else if (run.ok) {
    log.info({ trigger: run.trigger, version: run.version }, `config file unchanged; version ${run.version} stays`);
  } else {
    log.error({ trigger: run.trigger, rejection: run.error }, `config file rejected (${run.error.code}): ${run.error.message}; current version stays`);
  }
}

// Sweeps run every few seconds: only those that changed something, or failed, are
// worth a line.
function logExpirySweep(log: FastifyBaseLogger, run: ExpirySweepRun): void {
  if (!run.ok) {
    log.error({ err: run.error, trigger: run.trigger }, "gateway expiry sweep failed");
  } else if (run.expired.length > 0 || run.forgotten.length > 0 || run.batchCursorsDropped.length > 0) {
    log.info(
      {
        trigger: run.trigger,
        expired: run.expired,
        forgotten: run.forgotten,
        batch_cursors_dropped: run.batchCursorsDropped,
      },
      "gateway expiry sweep",
    );
  }
}
