// The group tree's shape (docs/specs/CONTROL-PROTOCOL.md, Config → The group tree):
// where following a group's parents leads.

import type { Group } from "./types.ts";

// At most 8 levels; a top-level group is level 1.
export const MAX_GROUP_DEPTH = 8;

// Where a group's parents lead:
// - rooted: to a top-level group; depth is the group's level;
// - parent-unknown: this group's own parent (missing) has no entry;
// - cycle: back to this group;
// - below-broken: to a group whose parent is unknown or that is on a cycle.
export type Ancestry =
  | { kind: "rooted"; depth: number }
  | { kind: "parent-unknown"; missing: string }
  | { kind: "cycle" }
  | { kind: "below-broken" };

const BELOW_BROKEN: Ancestry = { kind: "below-broken" };
const CYCLE: Ancestry = { kind: "cycle" };

// Every group's ancestry, keyed by group ID. Each group's parents are followed once:
// a walk up stops at the first group already settled, so a long chain or cycle costs
// time in proportion to its length.
export function ancestries(groups: Readonly<Record<string, Group>>): Map<string, Ancestry> {
  const settled = new Map<string, Ancestry>();
  for (const id of Object.keys(groups)) {
    if (settled.has(id)) continue;
    // Walk up from id until a settled group, a top-level group, an unknown parent or
    // a group already on this walk; then settle the walk from its top down.
    const walk: string[] = [];
    const onWalk = new Map<string, number>();
    // The depth of the group above the walk's top; undefined when that group is broken.
    let above: number | undefined;
    for (let current = id; ; ) {
      const known = settled.get(current);
      if (known !== undefined) {
        above = known.kind === "rooted" ? known.depth : undefined;
        break;
      }
      const at = onWalk.get(current);
      if (at !== undefined) {
        for (const member of walk.splice(at)) settled.set(member, CYCLE);
        above = undefined;
        break;
      }
      onWalk.set(current, walk.length);
      walk.push(current);
      const parent = groups[current]?.parent;
      if (parent === undefined) {
        above = 0;
        break;
      }
      if (!Object.hasOwn(groups, parent)) {
        walk.pop();
        settled.set(current, { kind: "parent-unknown", missing: parent });
        above = undefined;
        break;
      }
      current = parent;
    }
    for (let i = walk.length - 1; i >= 0; i--) {
      const member = walk[i] as string;
      if (above === undefined) {
        settled.set(member, BELOW_BROKEN);
        continue;
      }
      above += 1;
      settled.set(member, { kind: "rooted", depth: above });
    }
  }
  return settled;
}

// A rooted group's path: every group from its top-level group down to it.
export function pathOf(groups: Readonly<Record<string, Group>>, id: string): string[] {
  const upward: string[] = [];
  for (let current: string | undefined = id; current !== undefined; current = groups[current]?.parent) {
    upward.push(current);
  }
  return upward.reverse();
}
