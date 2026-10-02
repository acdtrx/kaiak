// Which limit windows a usage record counts toward, and by how much
// (docs/specs/CONTROL-PROTOCOL.md, Usage intake): global and every group the record's
// path lists that the config in force defines, the hour and month limits of those
// scopes, those whose model set covers the record's model.

import { limitCovers, resolveScopes } from "../config/index.ts";
import type { Config, Limit } from "../config/index.ts";
import type { TotalsLimitType, UsageRecord } from "../messages/index.ts";
import type { CurrentWindows, WindowKey, WindowTotal } from "../storage/index.ts";

import { recordWindowStart } from "./windows.ts";

// A limit the control plane counts, with the identity its totals are kept by.
export interface CountedLimit {
  // Absent for a global limit.
  group?: string;
  type: TotalsLimitType;
  // Sorted; absent = all models.
  models?: string[];
  limit: Limit;
}

// One config's counted limits: all of them, global's, and by group.
export interface CountedLimits {
  all: CountedLimit[];
  global: CountedLimit[];
  byGroup: Map<string, CountedLimit[]>;
}

export function countedLimitsOf(config: Config): CountedLimits {
  const all: CountedLimit[] = [];
  let global: CountedLimit[] = [];
  const byGroup = new Map<string, CountedLimit[]>();
  for (const { group, limits } of resolveScopes(config)) {
    const counted: CountedLimit[] = [];
    for (const limit of limits) {
      if (limit.type !== "tokens_per_hour" && limit.type !== "usd_per_month") continue;
      counted.push({
        ...(group !== undefined && { group }),
        type: limit.type,
        ...(limit.models !== undefined && { models: [...limit.models].sort() }),
        limit,
      });
    }
    all.push(...counted);
    if (group === undefined) global = counted;
    else byGroup.set(group, counted);
  }
  return { all, global, byGroup };
}

// Identifies one limit's window; equal model sets give equal keys (they are sorted).
export function windowKeyOf({ group, type, models, windowStart }: WindowKey): string {
  return JSON.stringify([group ?? null, type, models ?? null, windowStart]);
}

// What a batch of records adds to each window, one entry per window with a non-zero
// amount: each record in its own window when that is the current or previous one,
// else the current one (recordWindowStart). A listed group the config no longer
// defines (deleted after the gateway settled the record) is skipped; the listed
// groups that remain and global still count.
export function batchAdditions(records: readonly UsageRecord[], limits: CountedLimits, windows: CurrentWindows): WindowTotal[] {
  const additions = new Map<string, WindowTotal>();
  for (const record of records) {
    const scopes = [limits.global, ...record.groups.map((group) => limits.byGroup.get(group) ?? [])];
    for (const counted of scopes.flat()) {
      if (!limitCovers(counted.limit, record.model)) continue;
      const amount = amountFor(counted.type, record);
      if (amount === 0n) continue;
      const window: WindowKey = {
        ...(counted.group !== undefined && { group: counted.group }),
        type: counted.type,
        ...(counted.models !== undefined && { models: counted.models }),
        windowStart: recordWindowStart(counted.type, record.gateway_time, windows),
      };
      const key = windowKeyOf(window);
      const existing = additions.get(key);
      if (existing) existing.used += amount;
      else additions.set(key, { ...window, used: amount });
    }
  }
  return [...additions.values()];
}

// What the gateway counts against the limit: every token the backend handled (reasoning
// is inside tokens_out), or the cost in nano-USD.
function amountFor(type: TotalsLimitType, record: UsageRecord): bigint {
  if (type === "usd_per_month") return BigInt(record.cost_nano_usd);
  const { tokens_in, tokens_cached, tokens_cache_write, tokens_out } = record.units;
  return BigInt(tokens_in) + BigInt(tokens_cached) + BigInt(tokens_cache_write) + BigInt(tokens_out);
}

// A limit that keeps its spend across a config change (docs/specs/CONTROL-PROTOCOL.md,
// Budgets → Model-set edits): a new limit identity whose group (or global) and type
// match limits the previous config had and the new one dropped — its model set
// changed.
export interface Carry {
  to: CountedLimit;
  // Shared by every new limit of the same group and type.
  from: readonly CountedLimit[];
}

// The carry-overs from one config's counted limits to the next's. Each identity is
// computed once and the dropped limits are grouped by group and type, so the cost is
// linear in the number of limits (a child_defaults edit on a group with many children
// makes as many new identities).
export function carryOvers(previous: CountedLimits, next: CountedLimits): Carry[] {
  const identity = (limit: CountedLimit): string =>
    JSON.stringify([limit.group ?? null, limit.type, limit.models ?? null]);
  const scopeAndType = (limit: CountedLimit): string => JSON.stringify([limit.group ?? null, limit.type]);
  const nextIdentities = next.all.map(identity);
  const after = new Set(nextIdentities);
  const before = new Set<string>();
  const dropped = new Map<string, CountedLimit[]>();
  for (const limit of previous.all) {
    const id = identity(limit);
    before.add(id);
    if (after.has(id)) continue;
    const key = scopeAndType(limit);
    const same = dropped.get(key);
    if (same) same.push(limit);
    else dropped.set(key, [limit]);
  }
  const carries: Carry[] = [];
  next.all.forEach((limit, index) => {
    if (before.has(nextIdentities[index] as string)) return;
    const from = dropped.get(scopeAndType(limit));
    if (from) carries.push({ to: limit, from });
  });
  return carries;
}
