// A stored window's identity: its scope (a group, absent for global), type and start.

import type { WindowKey } from "./types.ts";

export function windowKeyOf({ group, type, windowStart }: WindowKey): string {
  return JSON.stringify([group ?? null, type, windowStart]);
}
