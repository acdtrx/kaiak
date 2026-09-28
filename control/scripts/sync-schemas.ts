// Copies the protocol's JSON Schemas (protocol/schema/, the source of truth) into
// kaiak-control/schema/, where the package reads them, so the package works outside
// the repository (an extracted package, the sample image). `npm run sync-schemas`
// after a schema change; `npm test` fails while the copy differs.

import { copyFileSync, mkdirSync, readdirSync, readFileSync, rmSync } from "node:fs";
import path from "node:path";

const SUFFIX = ".schema.json";
export const SOURCE_DIR = path.resolve(import.meta.dirname, "../../protocol/schema");
export const COPY_DIR = path.resolve(import.meta.dirname, "../kaiak-control/schema");

function schemaFiles(dir: string): string[] {
  return readdirSync(dir)
    .filter((name) => name.endsWith(SUFFIX))
    .sort();
}

// The schema files missing from, extra in, or different in the copy, by name.
export function schemaDrift(source = SOURCE_DIR, copy = COPY_DIR): string[] {
  const wanted = schemaFiles(source);
  const held = new Set(schemaFiles(copy));
  const drift = wanted.filter((name) => !held.has(name) || !readFileSync(path.join(source, name)).equals(readFileSync(path.join(copy, name))));
  for (const name of held) if (!wanted.includes(name)) drift.push(name);
  return drift.sort();
}

export function syncSchemas(source = SOURCE_DIR, copy = COPY_DIR): void {
  mkdirSync(copy, { recursive: true });
  const wanted = new Set(schemaFiles(source));
  for (const name of schemaFiles(copy)) if (!wanted.has(name)) rmSync(path.join(copy, name));
  for (const name of wanted) copyFileSync(path.join(source, name), path.join(copy, name));
}

if (import.meta.main) {
  syncSchemas();
  console.log(`schemas copied to ${path.relative(process.cwd(), COPY_DIR) || "."}`);
}
