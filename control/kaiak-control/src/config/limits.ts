// Limits and allowed models as they apply (docs/specs/CONTROL-PROTOCOL.md, Config →
// The group tree; GATEWAY.md, Limits): every scope a config defines — global and each
// group — with its effective limits and the models its keys may use. The same
// resolution as the gateway's config snapshot; the shared resolution fixtures hold
// both to it.

import { MAX_GROUP_DEPTH, ancestries, pathOf } from "./tree.ts";
import type { Config, Group, Limit } from "./types.ts";

// One scope as a config resolves it.
export interface ResolvedScope {
  // The group; absent for global.
  group?: string;
  // The group and its ancestors, top-level first; empty for global.
  path: string[];
  // Effective limits: the parent's child_defaults.limits in their order, each
  // replaced in place by the group's limit of the same type, then the group's other
  // limits in their order. Each limit as written in config.
  limits: Limit[];
  // The models the group's keys may use across the whole path, sorted by code point;
  // absent when no level on the path restricts (always absent for global).
  allowed_models?: string[];
}

// Every scope of a valid config: global first, then each group in the order
// Object.entries gives — the document's member order, except that integer-like group
// IDs ("7", "42") come first, in ascending numeric order. A scope with no limits is
// still listed.
export function resolveScopes(config: Config): ResolvedScope[] {
  const groups = config.groups ?? {};
  const tree = ancestries(groups);
  const scopes: ResolvedScope[] = [{ path: [], limits: config.global.limits ?? [] }];
  for (const [id, group] of Object.entries(groups)) {
    const ancestry = tree.get(id);
    if (ancestry?.kind !== "rooted" || ancestry.depth > MAX_GROUP_DEPTH) {
      throw Object.assign(new Error(`group "${id}" is not in a valid tree; resolve only a valid config`), {
        code: "config-invalid",
      });
    }
    const parent = group.parent === undefined ? undefined : groups[group.parent];
    const path = pathOf(groups, id);
    const allowed = allowedAlong(path, groups);
    scopes.push({
      group: id,
      path,
      limits: mergeLimits(parent?.child_defaults?.limits ?? [], group.limits ?? []),
      ...(allowed !== undefined && { allowed_models: allowed }),
    });
  }
  return scopes;
}

// A group's own limit replaces the default of the same type, in its place; defaults
// not replaced still apply; the group's other limits follow.
export function mergeLimits(defaults: readonly Limit[], own: readonly Limit[]): Limit[] {
  const byType = new Map(own.map((limit) => [limit.type, limit]));
  const merged = defaults.map((limit) => {
    const replacement = byType.get(limit.type);
    if (!replacement) return limit;
    byType.delete(limit.type);
    return replacement;
  });
  return [...merged, ...byType.values()];
}

const ALL_MODELS = "*";

// The intersection of every restricting level's list along the path; undefined when
// no level restricts. A level's list is its own allowed_models, else its parent's
// child_defaults.allowed_models; none, or ["*"], restricts nothing.
function allowedAlong(path: readonly string[], groups: Readonly<Record<string, Group>>): string[] | undefined {
  let allowed: Set<string> | undefined;
  for (const id of path) {
    const group = groups[id];
    const parent = group?.parent === undefined ? undefined : groups[group.parent];
    const level = group?.allowed_models ?? parent?.child_defaults?.allowed_models;
    if (level === undefined || (level.length === 1 && level[0] === ALL_MODELS)) continue;
    const listed = new Set(level);
    allowed = allowed === undefined ? listed : new Set([...allowed].filter((model) => listed.has(model)));
  }
  // UTF-8 byte order is code-point order.
  return allowed && [...allowed].sort((a, b) => Buffer.compare(Buffer.from(a), Buffer.from(b)));
}
