// Client keys (docs/specs/CONTROL-PROTOCOL.md, Config → key format): generated here,
// shown once, never stored; only the key ID and the hash enter config.

import { createHash, randomBytes } from "node:crypto";

import { definitionChecker } from "../schemas/index.ts";

export interface CreatedKey {
  id: string;
  // The plaintext key: shown once, never stored or logged.
  key: string;
  // What config holds: "sha256:" + 64 lowercase hex.
  hash: string;
}

const KEY_PREFIX = "kaiak-";
const BODY_LENGTH = 43;
const ALPHABET = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789";
// The largest multiple of the alphabet size that fits a byte: bytes at or above it are
// discarded, so every character is equally likely (a plain modulo would favour the
// first 256 % 62 characters).
const ACCEPT_BELOW = 256 - (256 % ALPHABET.length);

const isId = definitionChecker("config.schema.json", "id");

// Whether id has the shape config gives the ID of a backend, group or key.
export function isConfigId(id: string): boolean {
  return isId(id);
}

// Creates a key for the key ID `id`. Throws { code: "key-id-invalid" } when id is not
// a valid config ID.
export function createKey(id: string): CreatedKey {
  if (!isConfigId(id)) {
    throw Object.assign(new Error(`"${id}" is not a valid key ID`), { code: "key-id-invalid" });
  }
  const key = KEY_PREFIX + randomBody();
  return { id, key, hash: hashKey(key) };
}

// The hash config stores for a key: SHA-256 of the whole key string, prefix included.
export function hashKey(key: string): string {
  return `sha256:${createHash("sha256").update(key, "utf8").digest("hex")}`;
}

// 43 characters from the alphabet carry 43 × log2(62) ≈ 256 bits: the entropy of 32
// random bytes.
function randomBody(): string {
  let body = "";
  while (body.length < BODY_LENGTH) {
    for (const byte of randomBytes(BODY_LENGTH)) {
      if (byte >= ACCEPT_BELOW) continue;
      body += ALPHABET.charAt(byte % ALPHABET.length);
      if (body.length === BODY_LENGTH) break;
    }
  }
  return body;
}
