// Public entry of kaiak-control: everything the library exposes is exported here.

export const libraryName = "kaiak-control";

export type { ValidationIssue } from "./schemas/index.ts";

export { BACKEND_TYPES, resolveScopes, validateConfig } from "./config/index.ts";
export type * from "./config/index.ts";

export {
  validateConfigSnapshot,
  validateGatewayStatus,
  validateResync,
  validateTotals,
  validateUsageAck,
  validateUsageBatch,
  validateUsageRecord,
} from "./messages/index.ts";
export type * from "./messages/index.ts";

export { createControlPlane } from "./control-plane/index.ts";
export type * from "./control-plane/index.ts";

export type * from "./config-versions/index.ts";

export type * from "./usage/index.ts";

export type * from "./gateways/index.ts";

export { createMemoryStore } from "./storage/index.ts";
export type * from "./storage/index.ts";

export { createKey, hashKey, isConfigId } from "./keys/index.ts";
export type * from "./keys/index.ts";

export { INSTANCE_HEADER, PROTOCOL_HEADER, PROTOCOL_VERSION, errorBody } from "./protocol/index.ts";
export type * from "./protocol/index.ts";

export { verifyBackend } from "./backend-verify/index.ts";
export type * from "./backend-verify/index.ts";

export { controlProtocolPlugin } from "./fastify/index.ts";
export type * from "./fastify/index.ts";
