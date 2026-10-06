// The sample control plane's process entry: settings from the environment, the app,
// the listener, and a graceful close on SIGTERM or SIGINT. Settings that are missing
// or invalid stop it with exit status 1 before anything starts.

import { createSampleApp } from "./app/index.ts";
import { loggerOptions } from "./logging/index.ts";
import { readSettings } from "./settings/index.ts";
import type { Settings } from "./settings/index.ts";

let settings: Settings;
try {
  settings = readSettings(process.env, process.cwd());
} catch (error) {
  process.stderr.write(`kaiak-sample: ${error instanceof Error ? error.message : String(error)}\n`);
  process.exit(1);
}

const { app, replicas } = createSampleApp({
  configFile: settings.configFile,
  token: settings.token,
  logger: loggerOptions(settings.logFormat),
  protocolReplicas: settings.protocolPorts.length,
});

let closing = false;
const close = (signal: NodeJS.Signals): void => {
  if (closing) return;
  closing = true;
  app.log.info({ signal }, "closing");
  // kaiak-control's hooks end the gateway streams and stop the expiry sweep; the app's stop
  // the config file watcher.
  Promise.all([app.close(), ...replicas.map((replica) => replica.app.close())]).then(
    () => process.exit(0),
    (error: unknown) => {
      app.log.error({ err: error }, "close failed");
      process.exit(1);
    },
  );
};
process.on("SIGTERM", close);
process.on("SIGINT", close);

try {
  const url = await app.listen({ host: settings.listen.host, port: settings.listen.port });
  // One line naming the bound address, for scripts waiting on the server.
  app.log.info({ url, configFile: settings.configFile }, `sample control plane listening on ${url}`);
  for (const [i, port] of settings.protocolPorts.entries()) {
    const replica = replicas[i]!;
    const replicaUrl = await replica.app.listen({ host: settings.listen.host, port });
    replica.app.log.info({ url: replicaUrl, replica: i + 1 }, `sample protocol replica ${i + 1} listening on ${replicaUrl}`);
  }
} catch (error) {
  app.log.error({ err: error }, "startup failed");
  process.exit(1);
}
