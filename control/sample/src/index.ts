// Entry of the sample control plane package, a thin app on kaiak-control. The process
// entries are main.ts (the server), keygen-cli.ts (the keygen command) and verify-cli.ts
// (the verify command).

export { createSampleApp, STARTUP_TRIGGER } from "./app/index.ts";
export type * from "./app/index.ts";

export { createConfigFile, WATCH_TRIGGER } from "./config-file/index.ts";
export type * from "./config-file/index.ts";

export { DEFAULT_LISTEN, readSettings } from "./settings/index.ts";
export type * from "./settings/index.ts";

export { loggerOptions } from "./logging/index.ts";
export type * from "./logging/index.ts";

export { formatKey, runKeygen } from "./keygen/index.ts";
export type * from "./keygen/index.ts";

export { explain, runVerify } from "./verify/index.ts";
export type { VerifyResult } from "./verify/index.ts";
