// What a totals window is (docs/specs/CONTROL-PROTOCOL.md, Messages → Totals): the
// limit types the control plane counts, and a window's identity — its scope and type.

import type { LimitType } from "../config/index.ts";

import type { TotalsLimitType } from "./types.ts";

// The limit types whose windows the control plane counts (totals.schema.json's window
// type); the per-minute ones are each gateway's own.
export const COUNTED_TYPES: readonly TotalsLimitType[] = ["tokens_per_hour", "usd_per_month"];

export function isCountedType(type: LimitType): type is TotalsLimitType {
  return (COUNTED_TYPES as readonly LimitType[]).includes(type);
}

// A window's identity: its group (undefined for global) and type. A totals message
// lists each at most once, and the gateway keeps its counts by it.
export function scopeTypeKey(group: string | undefined, type: TotalsLimitType): string {
  return JSON.stringify([group ?? null, type]);
}
