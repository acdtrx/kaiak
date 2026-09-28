import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import path from "node:path";
import { test } from "node:test";

import { createKey, hashKey, isConfigId } from "./index.ts";

const KEY_FIXTURE = path.resolve(import.meta.dirname, "../../../../protocol/fixtures/keys/example.json");
const KEY_SHAPE = /^kaiak-[A-Za-z0-9]{43}$/;
const HASH_SHAPE = /^sha256:[0-9a-f]{64}$/;

test("the hash of the shared fixture key is the fixture hash (the gateway checks the same pair)", () => {
  const fixture = JSON.parse(readFileSync(KEY_FIXTURE, "utf8")) as { key: string; hash: string };
  assert.match(fixture.key, KEY_SHAPE);
  assert.equal(hashKey(fixture.key), fixture.hash);
});

test("created keys have the key format and hash to their hash", () => {
  const seen = new Set<string>();
  for (let n = 0; n < 2000; n++) {
    const created = createKey("k-test");
    assert.equal(created.id, "k-test");
    assert.match(created.key, KEY_SHAPE);
    assert.match(created.hash, HASH_SHAPE);
    assert.equal(created.hash, hashKey(created.key));
    seen.add(created.key);
  }
  assert.equal(seen.size, 2000, "no repeated keys");
});

test("every body character is drawn evenly from the whole alphabet", () => {
  const counts = new Map<string, number>();
  const keys = 4000;
  for (let n = 0; n < keys; n++) {
    for (const char of createKey("k-test").key.slice("kaiak-".length)) counts.set(char, (counts.get(char) ?? 0) + 1);
  }
  assert.equal(counts.size, 62, "all 62 characters occur");
  // 172,000 characters: about 2774 each, standard deviation about 52. A plain modulo
  // draw would put the first 8 characters about 21% above that; the 12% bound (over six
  // standard deviations) catches it without failing an even draw by chance.
  const expected = (keys * 43) / 62;
  for (const [char, count] of counts) {
    assert.ok(Math.abs(count - expected) < expected * 0.12, `"${char}" drawn ${count} times, expected ~${expected}`);
  }
});

test("a key ID outside the config ID shape is refused", () => {
  for (const id of ["", "-k", "k key", "k/1", "a".repeat(129)]) {
    assert.throws(() => createKey(id), { code: "key-id-invalid" }, JSON.stringify(id));
  }
  assert.equal(createKey("alice@example.com").id, "alice@example.com");
});

test("isConfigId accepts the config ID shape only", () => {
  for (const id of ["", "-k", "k key", "k/1", "a".repeat(129)]) assert.equal(isConfigId(id), false, JSON.stringify(id));
  for (const id of ["k", "alice@example.com", "team.a_1-x", "a".repeat(128)]) assert.equal(isConfigId(id), true, id);
});
