// The protocol's JSON Schemas, read at runtime from the package's own schema/ — a
// byte-for-byte copy of protocol/schema/ (control/scripts/sync-schemas.ts; npm test
// fails on drift), so the package needs no repository around it and the validators
// check the same files as the gateway's fixtures. Every schema file loads into one
// validator instance, so the cross-file $refs between them (by their $id) resolve.

import { readdirSync, readFileSync } from "node:fs";
import path from "node:path";

import { Ajv2020 } from "ajv/dist/2020.js";
import type { ErrorObject, ValidateFunction } from "ajv/dist/2020.js";

// One reason a document was rejected. code is "schema" for a schema violation, or the
// semantic rule's code (docs/specs/CONTROL-PROTOCOL.md).
export interface ValidationIssue {
  code: string;
  message: string;
  // JSON Pointer into the document ("" is the root).
  path: string;
}

export type SchemaCheck<T> = { value: T; issues: [] } | { value?: never; issues: ValidationIssue[] };

const SCHEMA_DIR = path.resolve(import.meta.dirname, "../../schema");
const SCHEMA_SUFFIX = ".schema.json";

// The loaded schemas, and each file's $id.
let loaded: { ajv: Ajv2020; idByFile: Map<string, string> } | undefined;

function schemaSet(): { ajv: Ajv2020; idByFile: Map<string, string> } {
  if (loaded) return loaded;
  const idByFile = new Map<string, string>();
  const ajv = new Ajv2020({ allErrors: true, strictTypes: true, strictTuples: true, allowUnionTypes: true });
  for (const file of readdirSync(SCHEMA_DIR).filter((name) => name.endsWith(SCHEMA_SUFFIX))) {
    const schema: unknown = JSON.parse(readFileSync(path.join(SCHEMA_DIR, file), "utf8"));
    if (typeof schema !== "object" || schema === null || typeof (schema as { $id?: unknown }).$id !== "string") {
      throw Object.assign(new Error(`${file} is not a JSON Schema object with an $id`), { code: "schema-unreadable" });
    }
    const { $id } = schema as { $id: string };
    ajv.addSchema(schema);
    idByFile.set(file, $id);
  }
  loaded = { ajv, idByFile };
  return loaded;
}

// Returns a check of documents against one schema file (e.g. "config.schema.json"),
// compiled on first use.
export function schemaChecker<T>(file: string): (doc: unknown) => SchemaCheck<T> {
  let validate: ValidateFunction<T> | undefined;
  return (doc) => {
    validate ??= compile<T>(file);
    if (validate(doc)) return { value: doc, issues: [] };
    return { issues: (validate.errors ?? []).map(toIssue) };
  };
}

// Returns a check of single values against one of a schema file's $defs (e.g. the
// "id" definition of "config.schema.json"), so a value checked outside a document
// meets the same pattern the document's schema applies. Compiled on first use.
export function definitionChecker(file: string, definition: string): (value: unknown) => boolean {
  let validate: ValidateFunction | undefined;
  return (value) => {
    validate ??= compile(file, `#/$defs/${definition}`);
    return validate(value);
  };
}

function compile<T>(file: string, fragment = ""): ValidateFunction<T> {
  const { ajv, idByFile } = schemaSet();
  const id = idByFile.get(file);
  const validate = id === undefined ? undefined : ajv.getSchema<T>(`${id}${fragment}`);
  if (!validate) {
    throw Object.assign(new Error(`no schema ${file}${fragment} in ${SCHEMA_DIR}`), { code: "schema-unreadable" });
  }
  return validate;
}

function toIssue(error: ErrorObject): ValidationIssue {
  const extra = typeof error.params["additionalProperty"] === "string" ? `: ${error.params["additionalProperty"]}` : "";
  return {
    code: "schema",
    message: `${error.instancePath || "/"} ${error.message ?? "is invalid"}${extra}`,
    path: error.instancePath,
  };
}

// Builds a JSON Pointer (RFC 6901) from a base pointer and further segments.
export function pointer(base: string, ...segments: (string | number)[]): string {
  return segments.reduce<string>(
    (acc, segment) => `${acc}/${String(segment).replaceAll("~", "~0").replaceAll("/", "~1")}`,
    base,
  );
}
