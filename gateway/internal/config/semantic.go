package config

import (
	"fmt"
	"maps"
	"slices"
	"strings"

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
	c.effectiveLimits()

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

// effectiveLimits checks the effective limits of global and every group add up to at
// most MaxEffectiveLimits.
func (c *semanticCheck) effectiveLimits() {
	if total := countEffectiveLimits(c.doc); total > MaxEffectiveLimits {
		c.report(CodeEffectiveLimitsExceeded, "",
			fmt.Sprintf("%d effective limits: global and the groups hold at most %d", total, MaxEffectiveLimits))
	}
}

// countEffectiveLimits counts the effective limits of global and every group without
// building them: a group's count is what mergeLimits makes of its parent's
// child_defaults limits and its own — every default, plus each own limit no default
// takes (a default takes the first own limit of its identity). Only the direct parent
// is read, so the count holds whatever tree() finds; a group whose parent has no entry
// counts its own limits alone.
func countEffectiveLimits(doc *document) int {
	total := len(doc.Global.Limits)
	// defaultIdentities is each parent's child_defaults limit identities, built once.
	defaultIdentities := map[string]map[string]bool{}
	for _, group := range doc.Groups {
		total += len(group.Limits)
		parent, ok := doc.Groups[group.Parent]
		if !ok || parent.ChildDefaults == nil || len(parent.ChildDefaults.Limits) == 0 {
			continue
		}
		defaults := parent.ChildDefaults.Limits
		identities, built := defaultIdentities[group.Parent]
		if !built {
			identities = make(map[string]bool, len(defaults))
			for _, d := range defaults {
				identities[limitIdentity(d.Type, d.Models)] = true
			}
			defaultIdentities[group.Parent] = identities
		}
		total += len(defaults)
		taken := make(map[string]bool, len(group.Limits))
		for _, own := range group.Limits {
			if identity := limitIdentity(own.Type, own.Models); identities[identity] && !taken[identity] {
				taken[identity] = true
				total--
			}
		}
	}
	return total
}

// limits checks one limit list.
func (c *semanticCheck) limits(limits []limitDoc, path string) {
	seen := make(map[string]int, len(limits))
	for i, limit := range limits {
		limitPath := schemacheck.Pointer(path, i)
		for j, name := range limit.Models {
			if _, ok := c.doc.Models[name]; !ok {
				c.report(CodeLimitModelUnknown, schemacheck.Pointer(limitPath, "models", j), fmt.Sprintf("model %q is not defined", name))
			}
		}
		identity := limitIdentity(limit.Type, limit.Models)
		if first, dup := seen[identity]; dup {
			c.report(CodeLimitDuplicate, limitPath, "same type and model set as "+schemacheck.Pointer(path, first))
			continue
		}
		seen[identity] = i
	}
}

// limitIdentity: two limits in one list collide when they have the same type and
// cover the same set of models; "all models" (nil) is its own set.
func limitIdentity(limitType string, models []string) string {
	if models == nil {
		return limitType + "\n" + allModels
	}
	sorted := slices.Clone(models)
	slices.Sort(sorted)
	return limitType + "\n" + strings.Join(sorted, "\n")
}
