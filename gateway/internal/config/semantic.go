package config

import (
	"fmt"
	"maps"
	"slices"

	"kaiak/internal/schemacheck"
)

// Cross-reference rules the schema cannot express, each with its contract code
// (docs/specs/CONTROL-PROTOCOL.md, Config). kaiak-control checks the same rules and
// reports the same code for the same document. Runs only on a document that passed
// checkSchema, and reports every violation.

// allModels in an allowed_models list stands for every model; it must stand alone.
const allModels = "*"

type semanticCheck struct {
	doc    *document
	issues []Issue
}

func checkSemantics(doc *document) []Issue {
	c := &semanticCheck{doc: doc}

	for _, name := range slices.Sorted(maps.Keys(doc.Models)) {
		c.model(name, doc.Models[name])
	}

	c.limits(doc.Global.Limits, "/global/limits")

	for _, id := range slices.Sorted(maps.Keys(doc.Groups)) {
		group := doc.Groups[id]
		path := schemacheck.Pointer("/groups", id)
		if group.AllowedModels != nil {
			c.allowedModels(group.AllowedModels, schemacheck.Pointer(path, "allowed_models"))
		}
		c.limits(group.Limits, schemacheck.Pointer(path, "limits"))
		if defaults := group.ChildDefaults; defaults != nil {
			if defaults.AllowedModels != nil {
				c.allowedModels(defaults.AllowedModels, schemacheck.Pointer(path, "child_defaults", "allowed_models"))
			}
			c.limits(defaults.Limits, schemacheck.Pointer(path, "child_defaults", "limits"))
		}
	}
	c.tree()
	c.counters()

	keyByHash := make(map[string]string, len(doc.Keys))
	for _, id := range slices.Sorted(maps.Keys(doc.Keys)) {
		key := doc.Keys[id]
		path := schemacheck.Pointer("/keys", id)
		if _, ok := doc.Groups[key.Group]; !ok {
			c.report(CodeKeyGroupUnknown, schemacheck.Pointer(path, "group"), fmt.Sprintf("group %q is not defined", key.Group))
		}
		if other, dup := keyByHash[key.Hash]; dup {
			c.report(CodeKeyHashDuplicate, schemacheck.Pointer(path, "hash"), fmt.Sprintf("same hash as key %q", other))
		} else {
			keyByHash[key.Hash] = id
		}
		if key.ExpiresAt != "" && !schemacheck.IsRealTimestamp(key.ExpiresAt) {
			c.report(CodeTimestampInvalid, schemacheck.Pointer(path, "expires_at"), fmt.Sprintf("%q is not a real instant", key.ExpiresAt))
		}
	}

	return c.issues
}

func (c *semanticCheck) report(code, path, message string) {
	c.issues = append(c.issues, Issue{Code: code, Path: path, Message: message})
}

func (c *semanticCheck) model(name string, model modelDoc) {
	path := schemacheck.Pointer("/models", name)

	for i, deployment := range model.Deployments {
		if _, ok := c.doc.Backends[deployment.Backend]; !ok {
			c.report(CodeDeploymentBackendUnknown, schemacheck.Pointer(path, "deployments", i, "backend"),
				fmt.Sprintf("backend %q is not defined", deployment.Backend))
		}
	}

	metadata := model.Metadata
	if len(metadata.ReasoningEfforts) > 0 && !metadata.Capabilities.Reasoning {
		c.report(CodeReasoningEffortsWithoutReasoning, schemacheck.Pointer(path, "metadata", "reasoning_efforts"),
			"reasoning efforts are listed but capabilities.reasoning is false")
	}
	if limit := model.OutputLimit; limit != nil {
		if limit.Default > limit.Ceiling {
			c.report(CodeOutputLimitDefaultAboveCeiling, schemacheck.Pointer(path, "output_limit"),
				fmt.Sprintf("default %v is above ceiling %v", limit.Default, limit.Ceiling))
		}
		if limit.Ceiling > metadata.ContextLength {
			c.report(CodeOutputLimitAboveContext, schemacheck.Pointer(path, "output_limit", "ceiling"),
				fmt.Sprintf("ceiling %v is above context_length %v", limit.Ceiling, metadata.ContextLength))
		}
	}

	previous := ""
	for i, price := range model.Prices {
		c.priceTiers(price.Tiers, schemacheck.Pointer(path, "prices", i, "tiers"))
		datePath := schemacheck.Pointer(path, "prices", i, "effective_from")
		if !schemacheck.IsRealDate(price.EffectiveFrom) {
			c.report(CodeDateInvalid, datePath, fmt.Sprintf("%q is not a real date", price.EffectiveFrom))
			continue
		}
		// YYYY-MM-DD strings order the same way as the dates they name.
		if previous != "" && price.EffectiveFrom <= previous {
			c.report(CodePriceDatesNotIncreasing, datePath, fmt.Sprintf("%s does not follow %s", price.EffectiveFrom, previous))
		}
		previous = price.EffectiveFrom
	}
}

// priceTiers checks a price entry's tiers start at 0 and their thresholds increase.
func (c *semanticCheck) priceTiers(tiers []priceTierDoc, path string) {
	for j, tier := range tiers {
		thresholdPath := schemacheck.Pointer(path, j, "above_input_tokens")
		if j == 0 {
			if tier.AboveInputTokens != 0 {
				c.report(CodePriceTierFirstNotZero, thresholdPath,
					fmt.Sprintf("the first tier starts above %d tokens, not 0", int64(tier.AboveInputTokens)))
			}
			continue
		}
		if previous := tiers[j-1].AboveInputTokens; tier.AboveInputTokens <= previous {
			c.report(CodePriceTiersNotIncreasing, thresholdPath,
				fmt.Sprintf("%d is not above the previous tier's %d", int64(tier.AboveInputTokens), int64(previous)))
		}
	}
}

func (c *semanticCheck) allowedModels(allowed []string, path string) {
	if slices.Contains(allowed, allModels) && len(allowed) > 1 {
		c.report(CodeAllowedModelsWildcardMixed, path, fmt.Sprintf("%q must stand alone", allModels))
	}
	for i, name := range allowed {
		if name == allModels {
			continue
		}
		if _, ok := c.doc.Models[name]; !ok {
			c.report(CodeAllowedModelUnknown, schemacheck.Pointer(path, i), fmt.Sprintf("model %q is not defined", name))
		}
	}
}

// tree checks the groups' parents form a tree of at most MaxGroupDepth levels:
// group-parent-unknown where a parent has no entry, group-cycle for each group on a
// cycle, group-depth-exceeded for each group deeper than MaxGroupDepth. Depth is judged
// only where the parents reach a top-level group, so a group below an unknown parent
// or a cycle reports those rules alone. Each group's parents are followed once.
func (c *semanticCheck) tree() {
	groups := c.doc.Groups
	const broken = -1 // below an unknown parent or a cycle, or on a cycle
	depth := make(map[string]int, len(groups))
	for _, id := range slices.Sorted(maps.Keys(groups)) {
		if _, done := depth[id]; done {
			continue
		}
		// Walk up from id until a group whose depth is known, a top-level group, an
		// unknown parent or a group already on this walk.
		var walk []string
		onWalk := make(map[string]int)
		above := 0 // depth of the group above the walk's top; broken if it has none
		for current := id; ; {
			if d, done := depth[current]; done {
				above = d
				break
			}
			if at, seen := onWalk[current]; seen {
				for _, member := range walk[at:] {
					c.report(CodeGroupCycle, schemacheck.Pointer("/groups", member, "parent"),
						fmt.Sprintf("following parents from %q leads back to it", member))
				}
				above = broken
				break
			}
			onWalk[current] = len(walk)
			walk = append(walk, current)
			parent := groups[current].Parent
			if parent == "" {
				above = 0
				break
			}
			if _, ok := groups[parent]; !ok {
				c.report(CodeGroupParentUnknown, schemacheck.Pointer("/groups", current, "parent"),
					fmt.Sprintf("group %q is not defined", parent))
				above = broken
				break
			}
			current = parent
		}
		for i := len(walk) - 1; i >= 0; i-- {
			member := walk[i]
			if above == broken {
				depth[member] = broken
				continue
			}
			above++
			depth[member] = above
			if above > MaxGroupDepth {
				c.report(CodeGroupDepthExceeded, schemacheck.Pointer("/groups", member),
					fmt.Sprintf("level %d: the tree holds at most %d levels", above, MaxGroupDepth))
			}
		}
	}
}

// counters checks the counters the config allocates on every gateway come to at most
// MaxCounters.
func (c *semanticCheck) counters() {
	if total := countCounters(c.doc); total > MaxCounters {
		c.report(CodeCountersExceeded, "",
			fmt.Sprintf("%d counters (two for global and for every group, one per per-minute limit): a config allocates at most %d",
				total, MaxCounters))
	}
}

// countCounters counts the counters a config allocates without building them: one per
// counted limit type (the hour and month counts) for global and every group, limited
// or not, and one per effective per-minute limit — a group's own per-minute types,
// plus each type its parent's child_defaults give that it has no own limit of. Only
// the direct parent is read, so the count holds whatever tree() finds; a group whose
// parent has no entry counts its own limits alone.
func countCounters(doc *document) int {
	perScope := 0
	for _, t := range LimitTypes() {
		if t.Counted() {
			perScope++
		}
	}
	perMinute := func(limits []limitDoc) map[string]bool {
		types := map[string]bool{}
		for _, l := range limits {
			if !LimitType(l.Type).Counted() {
				types[l.Type] = true
			}
		}
		return types
	}
	// defaults is each parent's child_defaults per-minute types, built once.
	defaults := map[string]map[string]bool{}
	total := perScope + len(perMinute(doc.Global.Limits))
	for _, group := range doc.Groups {
		own := perMinute(group.Limits)
		total += perScope + len(own)
		parent, ok := doc.Groups[group.Parent]
		if !ok || parent.ChildDefaults == nil {
			continue
		}
		types, built := defaults[group.Parent]
		if !built {
			types = perMinute(parent.ChildDefaults.Limits)
			defaults[group.Parent] = types
		}
		for typ := range types {
			if !own[typ] {
				total++
			}
		}
	}
	return total
}

// limits checks one limit list.
func (c *semanticCheck) limits(limits []limitDoc, path string) {
	seen := make(map[string]int, len(limits))
	for i, limit := range limits {
		if first, dup := seen[limit.Type]; dup {
			c.report(CodeLimitDuplicate, schemacheck.Pointer(path, i), "same type as "+schemacheck.Pointer(path, first))
			continue
		}
		seen[limit.Type] = i
	}
}
