// The config document contract: structural validation against the shared JSON Schema,
// then the cross-reference rules the schema cannot express.

import { schemaChecker } from "../schemas/index.ts";
import type { ValidationIssue } from "../schemas/index.ts";

import { checkSemantics } from "./semantic.ts";
import type { Config } from "./types.ts";

export { limitCovers, limitIdentity, resolveScopes } from "./limits.ts";
export type { ResolvedScope } from "./limits.ts";
export type { SemanticRuleCode } from "./semantic.ts";
export { BACKEND_TYPES } from "./types.ts";
export type * from "./types.ts";

export type ConfigIssue = ValidationIssue;

export type ConfigValidation = { ok: true; config: Config } | { ok: false; issues: ConfigIssue[] };

const checkSchema = schemaChecker<Config>("config.schema.json");

// Validates a parsed config document. Schema issues carry code "schema"; semantic
// issues carry their rule code. Semantic rules run only once the schema passes.
export function validateConfig(doc: unknown): ConfigValidation {
  const structural = checkSchema(doc);
  if (!structural.value) return { ok: false, issues: structural.issues };
  const issues = checkSemantics(structural.value);
  if (issues.length > 0) return { ok: false, issues };
  return { ok: true, config: structural.value };
}
