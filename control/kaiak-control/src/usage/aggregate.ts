// Which windows a usage record counts toward, and by how much
// (docs/specs/CONTROL-PROTOCOL.md, Usage intake → Counted toward): the window of every
// counted type — tokens, images, audio generated and audio processed per hour, USD per
// month — of global and of every group the record's path lists, whatever limits the
// config sets — so counting never depends on the config.

import { COUNTED_TYPES } from "../messages/index.ts";
import type { TotalsLimitType, UsageRecord } from "../messages/index.ts";
import { windowKeyOf } from "../storage/index.ts";
import type { WindowKey, WindowStarts, WindowTotal } from "../storage/index.ts";

import { recordWindowStart } from "./windows.ts";

// What a batch of records adds to each window, one entry per window with a non-zero
// amount: global and each group on a record's path, each record in its own window
// when that is the current or previous one, else the current one (recordWindowStart).
export function batchAdditions(records: readonly UsageRecord[], windows: WindowStarts): WindowTotal[] {
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

// What the gateway counts against the limit, each type in its own unit. Tokens: those
// that load the backend — plain input, input written to the cache and output (reasoning
// is inside tokens_out); input read from the cache does not count, a prefix-cache hit
// costs the backend almost nothing. Images and seconds of audio: their own unit, apart
// from the tokens, so a speech record counts toward both. USD: the cost in nano-USD.
// protocol/fixtures/usage/ holds both halves to it.
function amountFor(type: TotalsLimitType, record: UsageRecord): bigint {
  const { units } = record;
  switch (type) {
    case "tokens_per_hour":
      return BigInt(units.tokens_in) + BigInt(units.tokens_cache_write) + BigInt(units.tokens_out);
    case "images_per_hour":
      return BigInt(units.images);
    case "audio_seconds_out_per_hour":
      return BigInt(units.audio_seconds_out);
    case "audio_seconds_in_per_hour":
      return BigInt(units.audio_seconds_in);
    case "usd_per_month":
      return BigInt(record.cost_nano_usd);
  }
}
