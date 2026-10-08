// Cross-reference rules the JSON Schema cannot express. The rule codes are part of the
// contract (docs/specs/CONTROL-PROTOCOL.md, Config): the gateway reports the same code
// for the same fixture. Runs only on a document that passed the schema.

import { isRealDate, isRealTimestamp } from "../calendar/index.ts";
import { pointer } from "../schemas/index.ts";
import type { ValidationIssue } from "../schemas/index.ts";

import { MAX_GROUP_DEPTH, ancestries } from "./tree.ts";
import type { Ancestry } from "./tree.ts";
import { ALL_MODELS } from "./types.ts";
import type { Config, Group, Limit, LimitType, Model } from "./types.ts";

export type SemanticRuleCode =
  | "key-group-unknown"
  | "key-hash-duplicate"
  | "group-parent-unknown"
  | "group-cycle"
  | "group-depth-exceeded"
  | "counters-exceeded"
  | "deployment-backend-unknown"
  | "allowed-model-unknown"
  | "allowed-models-wildcard-mixed"
  | "limit-duplicate"
  | "output-limit-default-above-ceiling"
  | "output-limit-above-context"
  | "reasoning-efforts-without-reasoning"
  | "price-dates-not-increasing"
  | "price-tier-first-not-zero"
  | "price-tiers-not-increasing"
  | "date-invalid"
  | "timestamp-invalid";

// The most counters a config may allocate on every gateway: an hour and a month counter
// for global and every group, limited or not, and one per effective per-minute limit
// (docs/specs/CONTROL-PROTOCOL.md, Config → The group tree: Counters are bounded).
const MAX_COUNTERS = 50_000;

export function checkSemantics(config: Config): ValidationIssue[] {
  const issues: ValidationIssue[] = [];
  const report = (code: SemanticRuleCode, path: string, message: string): void => {
    issues.push({ code, message: path === "" ? message : `${path}: ${message}`, path });
  };
  const modelNames = new Set(Object.keys(config.models));

  for (const [name, model] of Object.entries(config.models)) {
    checkModel(config, name, model, report);
  }

  const checkAllowedModels = (allowed: readonly string[], path: string): void => {
    if (allowed.includes(ALL_MODELS) && allowed.length > 1) {
      report("allowed-models-wildcard-mixed", path, `"${ALL_MODELS}" must stand alone`);
    }
    allowed.forEach((model, index) => {
      if (model !== ALL_MODELS && !modelNames.has(model)) {
        report("allowed-model-unknown", pointer(path, index), `model "${model}" is not defined`);
      }
    });
  };

  const checkLimits = (limits: readonly Limit[] | undefined, path: string): void => {
    const seen = new Map<string, number>();
    (limits ?? []).forEach((limit, index) => {
      const first = seen.get(limit.type);
      if (first !== undefined) {
        report("limit-duplicate", pointer(path, index), `same type as ${pointer(path, first)}`);
        return;
      }
      seen.set(limit.type, index);
    });
  };

  checkLimits(config.global.limits, "/global/limits");

  const groups = config.groups ?? {};
  const tree = ancestries(groups);
  for (const [id, group] of Object.entries(groups)) {
    const path = pointer("/groups", id);
    const ancestry = tree.get(id);
    if (ancestry !== undefined) checkAncestry(ancestry, path, report);
    if (group.allowed_models) checkAllowedModels(group.allowed_models, pointer(path, "allowed_models"));
    checkLimits(group.limits, pointer(path, "limits"));
    const defaults = group.child_defaults;
    if (defaults?.allowed_models) {
      checkAllowedModels(defaults.allowed_models, pointer(path, "child_defaults", "allowed_models"));
    }
    checkLimits(defaults?.limits, pointer(path, "child_defaults", "limits"));
  }

  const counters = countCounters(config);
  if (counters > MAX_COUNTERS) {
    report(
      "counters-exceeded",
      "",
      `${counters} counters (two for global and for every group, one per per-minute limit): a config allocates at most ${MAX_COUNTERS}`,
    );
  }

  const keyByHash = new Map<string, string>();
  for (const [id, key] of Object.entries(config.keys)) {
    const path = pointer("/keys", id);
    if (!Object.hasOwn(groups, key.group)) {
      report("key-group-unknown", pointer(path, "group"), `group "${key.group}" is not defined`);
    }
    const other = keyByHash.get(key.hash);
    if (other !== undefined) {
      report("key-hash-duplicate", pointer(path, "hash"), `same hash as key "${other}"`);
    } else {
      keyByHash.set(key.hash, id);
    }
    if (key.expires_at !== undefined && !isRealTimestamp(key.expires_at)) {
      report("timestamp-invalid", pointer(path, "expires_at"), `"${key.expires_at}" is not a real instant`);
    }
  }

  return issues;
}

type Report = (code: SemanticRuleCode, path: string, message: string) => void;

// One defect, one code: a group reports an unknown parent only where it names it, a
// cycle for each group on it, and its depth only where its parents reach a top-level
// group — a group below an unknown parent or a cycle reports nothing of its own.
function checkAncestry(ancestry: Ancestry, path: string, report: Report): void {
  const parentPath = pointer(path, "parent");
  switch (ancestry.kind) {
    case "parent-unknown":
      report("group-parent-unknown", parentPath, `group "${ancestry.missing}" is not defined`);
      return;
    case "cycle":
      report("group-cycle", parentPath, "following parents leads back to this group");
      return;
    case "rooted":
      if (ancestry.depth > MAX_GROUP_DEPTH) {
        report("group-depth-exceeded", path, `level ${ancestry.depth}: the tree holds at most ${MAX_GROUP_DEPTH} levels`);
      }
      return;
    case "below-broken":
      return;
  }
}

// The per-minute limit types: each limit of one is a counter of its own; the hour and
// month types count on the two counters every scope has.
const PER_MINUTE_TYPES: ReadonlySet<string> = new Set<LimitType>(["requests_per_minute", "tokens_per_minute"]);

// How many counters a config allocates, counted without merging each group's list: two
// for global and two for every group, plus each scope's effective per-minute limits — a
// group's own plus its parent's child_defaults.limits of a per-minute type none of its
// own limits has. Only the direct parent matters, so the count stands whatever the tree
// rules find; a parent with no entry adds no defaults.
function countCounters(config: Config): number {
  const groups: Readonly<Record<string, Group>> = config.groups ?? {};
  const perMinute = (limits: readonly Limit[] | undefined): Set<string> =>
    new Set((limits ?? []).map((limit) => limit.type).filter((type) => PER_MINUTE_TYPES.has(type)));
  const defaultTypes = new Map<string, Set<string>>();
  const defaultsOf = (parentId: string): Set<string> => {
    let types = defaultTypes.get(parentId);
    if (types === undefined) {
      types = perMinute(groups[parentId]?.child_defaults?.limits);
      defaultTypes.set(parentId, types);
    }
    return types;
  };
  let count = 2 + perMinute(config.global.limits).size;
  for (const group of Object.values(groups)) {
    const own = perMinute(group.limits);
    count += 2 + own.size;
    if (group.parent === undefined || !Object.hasOwn(groups, group.parent)) continue;
    for (const type of defaultsOf(group.parent)) if (!own.has(type)) count += 1;
  }
  return count;
}

function checkModel(config: Config, name: string, model: Model, report: Report): void {
  const path = pointer("/models", name);

  model.deployments.forEach((deployment, index) => {
    if (!Object.hasOwn(config.backends, deployment.backend)) {
      report(
        "deployment-backend-unknown",
        pointer(path, "deployments", index, "backend"),
        `backend "${deployment.backend}" is not defined`,
      );
    }
  });

  const { metadata, output_limit: outputLimit } = model;
  if ((metadata.reasoning_efforts?.length ?? 0) > 0 && !metadata.capabilities.reasoning) {
    report(
      "reasoning-efforts-without-reasoning",
      pointer(path, "metadata", "reasoning_efforts"),
      "reasoning efforts are listed but capabilities.reasoning is false",
    );
  }
  if (outputLimit && outputLimit.default > outputLimit.ceiling) {
    report(
      "output-limit-default-above-ceiling",
      pointer(path, "output_limit"),
      `default ${outputLimit.default} is above ceiling ${outputLimit.ceiling}`,
    );
  }
  if (outputLimit && outputLimit.ceiling > metadata.context_length) {
    report(
      "output-limit-above-context",
      pointer(path, "output_limit", "ceiling"),
      `ceiling ${outputLimit.ceiling} is above context_length ${metadata.context_length}`,
    );
  }

  let previous: string | undefined;
  (model.prices ?? []).forEach((price, index) => {
    let below: number | undefined;
    price.tiers.forEach((tier, tierIndex) => {
      const threshold = tier.above_input_tokens;
      const thresholdPath = pointer(path, "prices", index, "tiers", tierIndex, "above_input_tokens");
      if (below === undefined && threshold !== 0) {
        report("price-tier-first-not-zero", thresholdPath, `the first tier's above_input_tokens is ${threshold}, not 0`);
      }
      if (below !== undefined && threshold <= below) {
        report("price-tiers-not-increasing", thresholdPath, `${threshold} is not above the previous tier's ${below}`);
      }
      below = threshold;
    });
    const datePath = pointer(path, "prices", index, "effective_from");
    if (!isRealDate(price.effective_from)) {
      report("date-invalid", datePath, `"${price.effective_from}" is not a real date`);
      return;
    }
    // YYYY-MM-DD strings order the same way as the dates they name.
    if (previous !== undefined && price.effective_from <= previous) {
      report("price-dates-not-increasing", datePath, `${price.effective_from} does not follow ${previous}`);
    }
    previous = price.effective_from;
  });
}
