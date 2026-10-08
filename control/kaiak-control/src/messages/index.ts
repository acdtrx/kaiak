// The control-protocol messages (docs/specs/CONTROL-PROTOCOL.md, Messages): each is
// validated against its shared JSON Schema, then the rules the schema cannot express.
// One validator per message; the stream's event names map to them as
// config → validateConfigEvent, totals → validateTotals.

import { validateConfig } from "../config/index.ts";
import { schemaChecker } from "../schemas/index.ts";
import type { SchemaCheck, ValidationIssue } from "../schemas/index.ts";

import { checkGatewayStatus, checkTotals, checkUsageBatch, checkUsageRecord } from "./semantic.ts";
import type { ConfigEvent, GatewayStatus, Totals, UsageAck, UsageBatch, UsageRecord } from "./types.ts";

export type { MessageRuleCode } from "./semantic.ts";
export { COUNTED_TYPES, isCountedType, scopeTypeKey } from "./totals.ts";
export type * from "./types.ts";

export type MessageValidation<T> = { ok: true; message: T } | { ok: false; issues: ValidationIssue[] };

// Runs a schema check, then the message's own rules on a document that passed it.
function validateWith<T>(check: (doc: unknown) => SchemaCheck<T>, rules: (message: T) => ValidationIssue[]) {
  return (doc: unknown): MessageValidation<T> => {
    const structural = check(doc);
    if (!structural.value) return { ok: false, issues: structural.issues };
    const issues = rules(structural.value);
    if (issues.length > 0) return { ok: false, issues };
    return { ok: true, message: structural.value };
  };
}

const noRules = (): ValidationIssue[] => [];

export const validateUsageRecord = validateWith(
  schemaChecker<UsageRecord>("usage-record.schema.json"),
  (record) => checkUsageRecord(record),
);

const CONFIG_PATH = "/config";

// The config inside must also pass the config semantic rules; their issues keep their
// codes, with paths under /config.
export const validateConfigEvent = validateWith(
  schemaChecker<ConfigEvent>("config-event.schema.json"),
  (event) => {
    const result = validateConfig(event.config);
    if (result.ok) return [];
    return result.issues.map((issue) => ({
      ...issue,
      path: `${CONFIG_PATH}${issue.path}`,
      message: `${CONFIG_PATH}${issue.message}`,
    }));
  },
);

export const validateTotals = validateWith(schemaChecker<Totals>("totals.schema.json"), (totals) =>
  checkTotals(totals),
);

export const validateUsageBatch = validateWith(schemaChecker<UsageBatch>("usage-batch.schema.json"), checkUsageBatch);

export const validateUsageAck = validateWith(schemaChecker<UsageAck>("usage-ack.schema.json"), noRules);

export const validateGatewayStatus = validateWith(
  schemaChecker<GatewayStatus>("status.schema.json"),
  checkGatewayStatus,
);
