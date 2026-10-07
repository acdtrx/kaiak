// Test support for kaiak-control's suites: the shared fixtures in protocol/fixtures/,
// which the gateway's suite runs too — a valid fixture passes both halves, an invalid
// one fails both, and a semantic fixture fails with the rule code its cases.json entry
// names. Test tooling only: not in the package's exports, imported by tests alone.

import assert from "node:assert/strict";
import { readdirSync, readFileSync } from "node:fs";
import path from "node:path";
import { test } from "node:test";

const FIXTURES = path.resolve(import.meta.dirname, "../../../../protocol/fixtures");

// The file of a fixture directory that says what each fixture there breaks; not a
// fixture itself.
export const CASES_FILE = "cases.json";

// The path of protocol/fixtures/<parts>.
export function fixturePath(...parts: string[]): string {
  return path.join(FIXTURES, ...parts);
}

export function readJson(file: string): unknown {
  return JSON.parse(readFileSync(file, "utf8"));
}

// The parsed content of protocol/fixtures/<parts>.
export function fixture(...parts: string[]): unknown {
  return readJson(fixturePath(...parts));
}

// The fixtures in dir: its .json files but the cases file, sorted.
export function fixtureFiles(dir: string): string[] {
  return readdirSync(dir)
    .filter((name) => name.endsWith(".json") && name !== CASES_FILE)
    .sort();
}

// A cases.json entry of an invalid fixture: a schema case breaks the schema and names
// no code; a semantic case breaks the rule its code names.
export interface InvalidCase {
  kind: "schema" | "semantic";
  code?: string;
  reason: string;
}

export function readCases(dir: string): Map<string, InvalidCase> {
  const raw = readJson(path.join(dir, CASES_FILE));
  assert.ok(typeof raw === "object" && raw !== null && !Array.isArray(raw), `${CASES_FILE} is an object`);
  const cases = new Map<string, InvalidCase>();
  for (const [file, entry] of Object.entries(raw)) {
    assert.ok(typeof entry === "object" && entry !== null, `${file}: entry is an object`);
    const { kind, code, reason } = entry as Record<string, unknown>;
    assert.ok(kind === "schema" || kind === "semantic", `${file}: kind is schema or semantic`);
    assert.equal(typeof reason, "string", `${file}: reason is a string`);
    if (kind === "semantic") assert.equal(typeof code, "string", `${file}: semantic case names its code`);
    if (kind === "schema") assert.equal(code, undefined, `${file}: schema case names no code`);
    cases.set(file, { kind, reason: String(reason), ...(typeof code === "string" ? { code } : {}) });
  }
  return cases;
}

// A validator as the fixture runners call it: a document in, its issues out.
export type FixtureValidator = (doc: unknown) => { ok: true } | { ok: false; issues: readonly { code: string }[] };

// One test per fixture in dir, named prefix + file: validate accepts it.
export function testValidFixtures(dir: string, validate: FixtureValidator, prefix = ""): void {
  for (const file of fixtureFiles(dir)) {
    test(`${prefix}${file}`, () => {
      const result = validate(readJson(path.join(dir, file)));
      assert.deepEqual(result.ok ? [] : result.issues, []);
    });
  }
}

// A test that dir's cases file has an entry for each fixture and no other, then one
// test per fixture, named prefix + file + its reason: validate refuses it with exactly
// the schema code (a schema case) or the case's code (a semantic one) — a semantic
// fixture breaks one rule, so no other code may appear.
export function testInvalidFixtures(dir: string, validate: FixtureValidator, prefix = ""): void {
  const cases = readCases(dir);

  test(`every invalid fixture has a ${CASES_FILE} entry and every entry has a fixture`, () => {
    assert.deepEqual([...cases.keys()].sort(), fixtureFiles(dir));
  });

  for (const file of fixtureFiles(dir)) {
    const expected = cases.get(file);
    if (!expected) continue;
    test(`${prefix}${file}: ${expected.reason}`, () => {
      const result = validate(readJson(path.join(dir, file)));
      assert.equal(result.ok, false, "the document is rejected");
      if (result.ok) return;
      const codes = [...new Set(result.issues.map((issue) => issue.code))];
      assert.deepEqual(codes, [expected.kind === "semantic" ? expected.code : "schema"]);
    });
  }
}
