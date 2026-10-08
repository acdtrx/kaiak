// The keygen command (docs/specs/CONTROL-PROTOCOL.md, Config → key format): mints a
// client key with kaiak-control and prints it once with its ID, its hash and the
// `keys` entry to paste into config. Needs no running server, so file-mode users use
// it too.

import { parseArgs } from "node:util";

import { createKey, isConfigId } from "kaiak-control";
import type { CreatedKey } from "kaiak-control";

export type KeygenResult = { ok: true; output: string; created: CreatedKey } | { ok: false; message: string };

export const USAGE = "usage (from control/): npm run keygen -w sample -- --id <key-id> --group <group-id>";

// Runs the command on its arguments (without node and the script path).
export function runKeygen(args: readonly string[]): KeygenResult {
  let values: { id?: string | undefined; group?: string | undefined };
  try {
    ({ values } = parseArgs({
      args: [...args],
      options: { id: { type: "string" }, group: { type: "string" } },
      strict: true,
      allowPositionals: false,
    }));
  } catch (error) {
    return { ok: false, message: error instanceof Error ? error.message : String(error) };
  }
  const { id, group } = values;
  if (!id) return { ok: false, message: "--id is required: the key ID config will know the key by" };
  if (!group) return { ok: false, message: "--group is required: the group the key belongs to" };
  // Checked before minting: a key refused after it was shown would be shown for nothing.
  if (!isConfigId(group)) return { ok: false, message: `"${group}" is not a valid group ID` };
  let created: CreatedKey;
  try {
    created = createKey(id);
  } catch (error) {
    return { ok: false, message: error instanceof Error ? error.message : String(error) };
  }
  return { ok: true, created, output: formatKey(created, group) };
}

// The key's lines, then the config entry: one line of JSON, a member of `keys`.
function formatKey(created: CreatedKey, group: string): string {
  const entry = `${JSON.stringify(created.id)}: ${JSON.stringify({ hash: created.hash, group })}`;
  return [
    `key:  ${created.key}`,
    `id:   ${created.id}`,
    `hash: ${created.hash}`,
    "",
    "The key is shown only this once and is not stored anywhere: hand it to the client now.",
    'Add this entry to the config\'s "keys":',
    "",
    `  ${entry}`,
    "",
  ].join("\n");
}
