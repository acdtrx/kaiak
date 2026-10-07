// Which windows a usage record counts toward, and by how much
// (docs/specs/CONTROL-PROTOCOL.md, Usage intake → Counted toward): the tokens_per_hour
// and usd_per_month window of global and of every group the record's path lists,
// whatever limits the config sets — so counting never depends on the config.

import type { TotalsLimitType, UsageRecord } from "../messages/index.ts";
import type { CurrentWindows, WindowKey, WindowTotal } from "../storage/index.ts";

import { recordWindowStart } from "./windows.ts";

// The limit types the control plane keeps totals for; the per-minute ones are each
// gateway's own.
const COUNTED_TYPES: readonly TotalsLimitType[] = ["tokens_per_hour", "usd_per_month"];

// Identifies one scope's window of one type.
export function windowKeyOf({ group, type, windowStart }: WindowKey): string {
  return JSON.stringify([group ?? null, type, windowStart]);
}

// What a batch of records adds to each window, one entry per window with a non-zero
// amount: global and each group on a record's path, each record in its own window
// when that is the current or previous one, else the current one (recordWindowStart).
export function batchAdditions(records: readonly UsageRecord[], windows: CurrentWindows): WindowTotal[] {
  const additions = new Map<string, WindowTotal>();
  for (const record of records) {
    const scopes: (string | undefined)[] = [undefined, ...record.groups];
    for (const type of COUNTED_TYPES) {
      const amount = amountFor(type, record);
      if (amount === 0n) continue;
      const windowStart = recordWindowStart(type, record.gateway_time, windows);
      for (const group of scopes) {
        const window: WindowKey = { ...(group !== undefined && { group }), type, windowStart };
        const key = windowKeyOf(window);
        const existing = additions.get(key);
        if (existing) existing.used += amount;
        else additions.set(key, { ...window, used: amount });
      }
    }
  }
  return [...additions.values()];
}

// What the gateway counts against the limit: the cost in nano-USD, or the tokens that
// load the backend — plain input, input written to the cache and output (reasoning is
// inside tokens_out). Input read from the cache does not count: a prefix-cache hit
// costs the backend almost nothing.
function amountFor(type: TotalsLimitType, record: UsageRecord): bigint {
  if (type === "usd_per_month") return BigInt(record.cost_nano_usd);
  const { tokens_in, tokens_cache_write, tokens_out } = record.units;
  return BigInt(tokens_in) + BigInt(tokens_cache_write) + BigInt(tokens_out);
}
