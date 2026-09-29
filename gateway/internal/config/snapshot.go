package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"maps"
	"math"
	"slices"
	"time"

	"kaiak/internal/schemacheck"
)

// Documented defaults for fields a config document may omit (docs/specs/GATEWAY.md;
// the schema's "default" keywords). Applied at load time, so the rest of the gateway
// never sees an unset value.
const (
	DefaultConnectTimeout      = 5 * time.Second
	DefaultFirstEventTimeout   = 60 * time.Second
	DefaultResponseTimeout     = 30 * time.Minute
	DefaultStallTimeout        = 120 * time.Second
	DefaultMaxRequestBodyBytes = 4 << 20
	DefaultKeyIDLabel          = true
	DefaultGroupLabel          = true
	DefaultControlOutageGrace  = 15 * time.Minute
	DefaultMaxN                = 8
	// DefaultMaxSequencesPerRequest: twice the default max_n — n at its ceiling on
	// two prompts, or 16 single-sequence prompts.
	DefaultMaxSequencesPerRequest = 16
	// DefaultMaxEmbeddingInputs is OpenAI's own limit on an embeddings request's inputs.
	DefaultMaxEmbeddingInputs  = 2048
	DefaultMaxConcurrentPerKey = 16
	DefaultQueueSize           = 100
	DefaultQueueTimeout        = 30 * time.Second
	DefaultMaxAttempts         = 3
	DefaultFailureThreshold    = 5
	DefaultProbeInterval       = 10 * time.Second
)

// Bounds the schema sets on the reliability settings.
const (
	// MaxAttemptsCeiling is the most attempts one client request may be given.
	MaxAttemptsCeiling = 10
	// MinProbeIntervalMS keeps probes of an open circuit from hammering its backend.
	MinProbeIntervalMS = 100
	// MaxGroupDepth is the most levels the group tree holds; a top-level group is
	// level 1.
	MaxGroupDepth = 8
	// MaxEffectiveLimits is the most effective limits global and every group may add
	// up to: each is a counter on every gateway, and child_defaults multiply them.
	MaxEffectiveLimits = 50_000
)

type BackendType string

const (
	BackendOpenAICompatible BackendType = "openai-compatible"
	BackendAzureOpenAI      BackendType = "azure-openai"
)

type LimitType string

const (
	LimitRequestsPerMinute LimitType = "requests_per_minute"
	LimitTokensPerMinute   LimitType = "tokens_per_minute"
	LimitTokensPerHour     LimitType = "tokens_per_hour"
	LimitUSDPerMonth       LimitType = "usd_per_month"
)

// Unit is a usage unit: what usage records, prices and limits count
// (docs/specs/CONTROL-PROTOCOL.md, Prices). The token units priced are disjoint:
// tokens_in is uncached input, tokens_cached cached input, tokens_out all output
// including reasoning. tokens_reasoning is the reasoning share of tokens_out, recorded
// for visibility and never priced.
type Unit string

const (
	UnitTokensIn        Unit = "tokens_in"
	UnitTokensCached    Unit = "tokens_cached"
	UnitTokensOut       Unit = "tokens_out"
	UnitTokensReasoning Unit = "tokens_reasoning"
)

// Version identifies a config a control plane published: its version number and the
// epoch of the control-plane store it counts in (docs/specs/CONTROL-PROTOCOL.md,
// Config versions). Two configs are the same one only when both are equal.
type Version struct {
	Epoch  string
	Number int64
}

// Snapshot is one validated config, resolved for serving: references are pointers,
// defaults applied, each group's effective allowed models and limits derived along its
// path.
// A Snapshot is immutable once Parse returns it: readers share it across goroutines
// and must not modify anything reachable from it. A request takes the current
// snapshot once (Holder.Current) and uses it until it finishes.
type Snapshot struct {
	// Version is the control plane's identity of the config: zero in file mode.
	Version             Version
	MaxRequestBodyBytes int64
	// KeyIDLabel: label usage metrics with the key ID; GroupLabel: with the key's
	// group, key_group (the top-level group labels them either way).
	KeyIDLabel bool
	GroupLabel bool
	// ControlOutageGrace: in control-plane mode, how long models covered by a USD
	// limit keep serving without contact with the control plane.
	ControlOutageGrace time.Duration
	// MaxN is the most sequences a request may ask for per prompt: n, and a
	// completion's best_of.
	MaxN int64
	// MaxSequencesPerRequest is the most sequences one chat or completions request may
	// ask the backend to generate: n (or best_of, when larger) × a completion's prompts.
	MaxSequencesPerRequest int64
	// MaxEmbeddingInputs is the most inputs one embeddings request may carry.
	MaxEmbeddingInputs int64
	// MaxConcurrentRequestsPerKey is the most requests one key may have in flight on
	// this gateway at once.
	MaxConcurrentRequestsPerKey int64
	// Circuit is the circuit-breaker setting every deployment shares.
	Circuit      Circuit
	GlobalLimits []Limit
	Backends     map[string]*Backend
	Models       map[string]*Model
	// ModelNames lists every public model name, sorted.
	ModelNames []string
	// Groups by group ID: the whole tree.
	Groups map[string]*Group
	// Keys by key ID; KeyByHash looks one up by hash.
	Keys       map[string]*Key
	keysByHash map[string]*Key
}

// KeyByHash returns the key whose hash is hash ("sha256:" + lowercase hex). Disabled
// and expired keys are returned too; deciding what they may do is the caller's job.
func (s *Snapshot) KeyByHash(hash string) (*Key, bool) {
	key, ok := s.keysByHash[hash]
	return key, ok
}

type Backend struct {
	ID      string
	Type    BackendType
	BaseURL string
	// APIKeyEnv names the environment variable holding the API key; "" = none.
	APIKeyEnv      string
	ConnectTimeout time.Duration
	// FirstEventTimeout bounds a streaming request until its first event;
	// ResponseTimeout a non-streaming request's whole response; StallTimeout the
	// silence between a stream's events after the first (docs/specs/GATEWAY.md,
	// Routing and reliability: timeouts).
	FirstEventTimeout time.Duration
	ResponseTimeout   time.Duration
	StallTimeout      time.Duration
	// MaxInFlight caps the requests in flight on the backend; 0 = no cap.
	MaxInFlight int64
}

// Queue is a model's queue setting: global.queue with the model's override applied.
type Queue struct {
	// Size is the most requests waiting; 0 = never queue.
	Size    int
	Timeout time.Duration
}

type Circuit struct {
	// FailureThreshold consecutive failures open a deployment's circuit.
	FailureThreshold int
	ProbeInterval    time.Duration
}

type Model struct {
	// Name is the public model name clients send.
	Name             string
	Deployments      []Deployment
	ContextLength    int64
	Capabilities     Capabilities
	ReasoningEfforts []string
	// Defaults maps request parameter → JSON value, exactly as written in the config.
	Defaults map[string]json.RawMessage
	// OutputLimit is nil when the model declares none (the gateway sets nothing).
	OutputLimit *OutputLimit
	// Prices in strictly increasing EffectiveFrom order; empty = unpriced.
	Prices []Price
	Queue  Queue
	// MaxAttempts is the attempts one client request gets, the first included.
	MaxAttempts int
}

type Deployment struct {
	Backend *Backend
	// Model is the model name on that backend.
	Model string
}

type Capabilities struct {
	Streaming bool
	Tools     bool
	Vision    bool
	Reasoning bool
}

type OutputLimit struct {
	Default int64
	Ceiling int64
}

type Price struct {
	// EffectiveFrom is midnight UTC of the day the price takes effect.
	EffectiveFrom time.Time
	// Tiers are the entry's prices by the request's input size (tokens_in +
	// tokens_cached): at least one, the first at 0, thresholds strictly increasing.
	Tiers []PriceTier
}

// PriceTier is one tier of a price entry, complete in itself: nothing is inherited
// from the tier below.
type PriceTier struct {
	// AboveInputTokens: the tier applies to a request whose input size is above it.
	AboveInputTokens int64
	// USDPerMillion holds the units the tier names (tokens_in, tokens_cached,
	// tokens_out). tokens_cached left out is charged at the tier's tokens_in price;
	// tokens_in or tokens_out left out costs 0.
	USDPerMillion map[Unit]float64
}

type Limit struct {
	Type  LimitType
	Value float64
	// Models the limit covers, counted together, sorted; nil = all models.
	Models []string
}

// Covers reports whether the limit counts requests to model.
func (l Limit) Covers(model string) bool {
	return l.Models == nil || slices.Contains(l.Models, model)
}

// Group is one node of the group tree with what its path gives it
// (docs/specs/CONTROL-PROTOCOL.md, Config → The group tree). Labels are not kept: the
// gateway gives them no meaning.
type Group struct {
	ID string
	// Parent is nil for a top-level group.
	Parent *Group
	// Path is the group and every ancestor, top-level first, the group itself last
	// (1 to MaxGroupDepth groups). Shared: do not modify.
	Path []*Group
	// PathIDs are Path's IDs, as usage records carry them. Shared: do not modify.
	PathIDs []string
	// AllowedModels: the models a key in this group may use, every level of the path
	// intersected.
	AllowedModels ModelSet
	// Limits are the group's effective limits: its parent's child_defaults.limits,
	// each replaced in place by the group's own limit of the same identity, then the
	// group's other limits.
	Limits []Limit
}

// Root is the top-level group of g's path (g itself when g is top-level).
func (g *Group) Root() *Group { return g.Path[0] }

type Key struct {
	ID    string
	Hash  string
	Group *Group
	// ExpiresAt is zero when the key does not expire.
	ExpiresAt time.Time
	Disabled  bool
}

// AllowedModels returns the models the key's group path allows.
func (k *Key) AllowedModels() ModelSet { return k.Group.AllowedModels }

// ModelSet is a set of public model names, concrete names only; All records that no
// level restricted it, so it holds every model of the snapshot.
type ModelSet struct {
	all   bool
	names []string
	set   map[string]struct{}
}

func (m ModelSet) Allows(model string) bool {
	_, ok := m.set[model]
	return ok
}

// All reports whether nothing restricted the set: it holds every model.
func (m ModelSet) All() bool { return m.all }

// Names lists the models in the set, sorted. The slice is shared: do not modify it.
func (m ModelSet) Names() []string { return m.names }

// Parse decodes and validates a config document and resolves it into a Snapshot. On
// rejection the error is a *ValidationError listing the issues of the first stage that
// failed: syntax, duplicate members, schema, then semantic rules.
func Parse(data []byte) (*Snapshot, error) {
	tree, err := schemacheck.Decode(data)
	var duplicate *schemacheck.DuplicateMemberError
	switch {
	case errors.As(err, &duplicate):
		return nil, &ValidationError{Issues: []Issue{{Code: CodeDuplicateMember, Path: duplicate.Path,
			Message: "appears more than once in its object"}}}
	case err != nil:
		return nil, &ValidationError{Issues: []Issue{{Code: CodeSyntax, Message: err.Error()}}}
	}
	if issues := checkSchema(tree); len(issues) > 0 {
		return nil, &ValidationError{Issues: issues}
	}
	doc, err := decodeDocument(data)
	if err != nil {
		// checkSchema accepted a document the Go types cannot hold: the two disagree.
		return nil, &ValidationError{Issues: []Issue{{Code: CodeSchema, Message: err.Error()}}}
	}
	if issues := checkSemantics(doc); len(issues) > 0 {
		return nil, &ValidationError{Issues: issues}
	}
	return resolve(doc), nil
}

// decodeDocument is the strict typed decode: unknown fields are errors, so a schema
// field the Go types lack fails loudly instead of being dropped. decodeTree has
// already checked that data holds exactly one JSON value.
func decodeDocument(data []byte) (*document, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var doc document
	if err := dec.Decode(&doc); err != nil {
		return nil, err
	}
	return &doc, nil
}

// resolve builds the Snapshot from a document that passed every check.
func resolve(doc *document) *Snapshot {
	s := &Snapshot{
		MaxRequestBodyBytes:         DefaultMaxRequestBodyBytes,
		KeyIDLabel:                  DefaultKeyIDLabel,
		GroupLabel:                  DefaultGroupLabel,
		ControlOutageGrace:          millisecondsOr(doc.Global.ControlOutageGraceMS, DefaultControlOutageGrace),
		MaxN:                        DefaultMaxN,
		MaxSequencesPerRequest:      DefaultMaxSequencesPerRequest,
		MaxEmbeddingInputs:          DefaultMaxEmbeddingInputs,
		MaxConcurrentRequestsPerKey: DefaultMaxConcurrentPerKey,
		Circuit:                     Circuit{FailureThreshold: DefaultFailureThreshold, ProbeInterval: DefaultProbeInterval},
		GlobalLimits:                resolveLimits(doc.Global.Limits),
		Backends:                    make(map[string]*Backend, len(doc.Backends)),
		Models:                      make(map[string]*Model, len(doc.Models)),
		ModelNames:                  slices.Sorted(maps.Keys(doc.Models)),
		Groups:                      make(map[string]*Group, len(doc.Groups)),
		Keys:                        make(map[string]*Key, len(doc.Keys)),
		keysByHash:                  make(map[string]*Key, len(doc.Keys)),
	}
	if doc.Global.MaxRequestBodyBytes != nil {
		s.MaxRequestBodyBytes = int64(*doc.Global.MaxRequestBodyBytes)
	}
	if doc.Global.MaxN != nil {
		s.MaxN = int64(*doc.Global.MaxN)
	}
	if doc.Global.MaxSequences != nil {
		s.MaxSequencesPerRequest = int64(*doc.Global.MaxSequences)
	}
	if doc.Global.MaxEmbeddingInputs != nil {
		s.MaxEmbeddingInputs = int64(*doc.Global.MaxEmbeddingInputs)
	}
	if doc.Global.MaxConcurrentPerKey != nil {
		s.MaxConcurrentRequestsPerKey = int64(*doc.Global.MaxConcurrentPerKey)
	}
	if m := doc.Global.Metrics; m != nil {
		if m.KeyIDLabel != nil {
			s.KeyIDLabel = *m.KeyIDLabel
		}
		if m.GroupLabel != nil {
			s.GroupLabel = *m.GroupLabel
		}
	}
	if c := doc.Global.Circuit; c != nil {
		s.Circuit.FailureThreshold = countOr(c.FailureThreshold, s.Circuit.FailureThreshold)
		s.Circuit.ProbeInterval = millisecondsOr(c.ProbeIntervalMS, s.Circuit.ProbeInterval)
	}
	globalQueue := applyQueue(Queue{Size: DefaultQueueSize, Timeout: DefaultQueueTimeout}, doc.Global.Queue)
	globalAttempts := applyRetries(DefaultMaxAttempts, doc.Global.Retries)

	for id, b := range doc.Backends {
		s.Backends[id] = &Backend{
			ID:                id,
			Type:              BackendType(b.Type),
			BaseURL:           b.BaseURL,
			APIKeyEnv:         b.APIKeyEnv,
			ConnectTimeout:    millisecondsOr(b.ConnectTimeoutMS, DefaultConnectTimeout),
			FirstEventTimeout: millisecondsOr(b.FirstEventTimeoutMS, DefaultFirstEventTimeout),
			ResponseTimeout:   millisecondsOr(b.ResponseTimeoutMS, DefaultResponseTimeout),
			StallTimeout:      millisecondsOr(b.StallTimeoutMS, DefaultStallTimeout),
			MaxInFlight:       maxInFlight(b.MaxInFlight),
		}
	}

	for name, m := range doc.Models {
		model := resolveModel(name, m, s.Backends)
		model.Queue = applyQueue(globalQueue, m.Queue)
		model.MaxAttempts = applyRetries(globalAttempts, m.Retries)
		s.Models[name] = model
	}

	every := allModelSet(s.ModelNames)
	for id := range doc.Groups {
		resolveGroup(id, doc, s, every)
	}

	for id, k := range doc.Keys {
		key := &Key{ID: id, Hash: k.Hash, Group: s.Groups[k.Group], Disabled: k.Disabled}
		if k.ExpiresAt != "" {
			// The semantic rules have checked the timestamp names a real instant.
			key.ExpiresAt, _ = time.Parse(time.RFC3339Nano, k.ExpiresAt)
		}
		s.Keys[id] = key
		s.keysByHash[k.Hash] = key
	}

	return s
}

func resolveModel(name string, m modelDoc, backends map[string]*Backend) *Model {
	model := &Model{
		Name:          name,
		Deployments:   make([]Deployment, len(m.Deployments)),
		ContextLength: int64(m.Metadata.ContextLength),
		Capabilities: Capabilities{
			Streaming: m.Metadata.Capabilities.Streaming,
			Tools:     m.Metadata.Capabilities.Tools,
			Vision:    m.Metadata.Capabilities.Vision,
			Reasoning: m.Metadata.Capabilities.Reasoning,
		},
		ReasoningEfforts: m.Metadata.ReasoningEfforts,
		Defaults:         m.Defaults,
		Prices:           make([]Price, len(m.Prices)),
	}
	for i, d := range m.Deployments {
		model.Deployments[i] = Deployment{Backend: backends[d.Backend], Model: d.Model}
	}
	if m.OutputLimit != nil {
		model.OutputLimit = &OutputLimit{Default: int64(m.OutputLimit.Default), Ceiling: int64(m.OutputLimit.Ceiling)}
	}
	for i, p := range m.Prices {
		// The semantic rules have checked the date names a real day.
		from, _ := time.Parse(time.DateOnly, p.EffectiveFrom)
		tiers := make([]PriceTier, len(p.Tiers))
		for j, tier := range p.Tiers {
			units := make(map[Unit]float64, len(tier.USDPerMillion))
			for unit, usd := range tier.USDPerMillion {
				units[Unit(unit)] = usd
			}
			tiers[j] = PriceTier{AboveInputTokens: int64(tier.AboveInputTokens), USDPerMillion: units}
		}
		model.Prices[i] = Price{EffectiveFrom: from, Tiers: tiers}
	}
	return model
}

func resolveLimits(docs []limitDoc) []Limit {
	limits := make([]Limit, len(docs))
	for i, l := range docs {
		var models []string
		if l.Models != nil {
			models = slices.Clone(l.Models)
			slices.Sort(models)
		}
		limits[i] = Limit{Type: LimitType(l.Type), Value: l.Value, Models: models}
	}
	return limits
}

// resolveGroup resolves the group id, its ancestors first, into s.Groups and returns
// it; every is the snapshot's every-model set. The semantic rules have checked the
// parents form a tree.
func resolveGroup(id string, doc *document, s *Snapshot, every ModelSet) *Group {
	if g, done := s.Groups[id]; done {
		return g
	}
	entry := doc.Groups[id]
	g := &Group{ID: id}
	var defaults childDefaultsDoc
	allowed := every
	if entry.Parent != "" {
		g.Parent = resolveGroup(entry.Parent, doc, s, every)
		if d := doc.Groups[entry.Parent].ChildDefaults; d != nil {
			defaults = *d
		}
		g.Path = slices.Clone(g.Parent.Path)
		allowed = g.Parent.AllowedModels
	}
	g.Path = append(g.Path, g)
	g.PathIDs = make([]string, len(g.Path))
	for i, member := range g.Path {
		g.PathIDs[i] = member.ID
	}

	level := entry.AllowedModels
	if level == nil {
		level = defaults.AllowedModels
	}
	if level != nil {
		allowed = allowed.restrict(level)
	}
	g.AllowedModels = allowed
	g.Limits = mergeLimits(resolveLimits(defaults.Limits), resolveLimits(entry.Limits))
	s.Groups[id] = g
	return g
}

// mergeLimits applies a group's own limits over its parent's child_defaults limits: a
// group's limit replaces the default limit with the same type and model set, in its
// place; defaults not replaced still apply; the group's other limits follow in their
// order.
func mergeLimits(defaults, overrides []Limit) []Limit {
	merged := make([]Limit, 0, len(defaults)+len(overrides))
	// byIdentity is the first override of each identity; a default takes it.
	byIdentity := make(map[string]int, len(overrides))
	for i, o := range overrides {
		id := limitIdentity(string(o.Type), o.Models)
		if _, dup := byIdentity[id]; !dup {
			byIdentity[id] = i
		}
	}
	used := make([]bool, len(overrides))
	for _, d := range defaults {
		limit := d
		if i, ok := byIdentity[limitIdentity(string(d.Type), d.Models)]; ok {
			limit = overrides[i]
			used[i] = true
		}
		merged = append(merged, limit)
	}
	for i, o := range overrides {
		if !used[i] {
			merged = append(merged, o)
		}
	}
	return merged
}

// allModelSet is every model in the snapshot: no level restricts it.
func allModelSet(modelNames []string) ModelSet {
	set := make(map[string]struct{}, len(modelNames))
	for _, name := range modelNames {
		set[name] = struct{}{}
	}
	return ModelSet{all: true, names: modelNames, set: set}
}

// restrict narrows m by one level's allowed_models list (validated; ["*"] restricts
// nothing).
func (m ModelSet) restrict(level []string) ModelSet {
	if slices.Equal(level, []string{allModels}) {
		return m
	}
	names := make([]string, 0, len(level))
	for _, name := range level {
		if m.Allows(name) {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	set := make(map[string]struct{}, len(names))
	for _, name := range names {
		set[name] = struct{}{}
	}
	return ModelSet{names: names, set: set}
}

// applyQueue returns q with the fields o sets replaced; nil o changes nothing.
func applyQueue(q Queue, o *queueDoc) Queue {
	if o != nil {
		q.Size = countOr(o.Size, q.Size)
		q.Timeout = millisecondsOr(o.TimeoutMS, q.Timeout)
	}
	return q
}

// applyRetries returns the attempts o sets, else attempts.
func applyRetries(attempts int, o *retriesDoc) int {
	if o != nil {
		attempts = countOr(o.MaxAttempts, attempts)
	}
	return attempts
}

// countOr converts a validated non-negative integer, saturating at math.MaxInt32 (no
// count in the config is meaningful beyond it); nil is fallback.
func countOr(n *float64, fallback int) int {
	if n == nil {
		return fallback
	}
	if *n >= math.MaxInt32 {
		return math.MaxInt32
	}
	return int(*n)
}

// maxInFlight converts a validated backend cap (at most 2^53 - 1, exact in a float64)
// without saturating: the status reports the configured cap as written; nil is 0, no
// cap.
func maxInFlight(n *float64) int64 {
	if n == nil {
		return 0
	}
	return int64(*n)
}

func millisecondsOr(ms *float64, fallback time.Duration) time.Duration {
	if ms == nil {
		return fallback
	}
	if *ms >= float64(math.MaxInt64/int64(time.Millisecond)) {
		return time.Duration(math.MaxInt64)
	}
	return time.Duration(*ms) * time.Millisecond
}
