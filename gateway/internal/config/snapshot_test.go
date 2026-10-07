package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func parseFixture(t *testing.T, name string) *Snapshot {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(fixturesDir, "valid", name))
	if err != nil {
		t.Fatal(err)
	}
	s, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// Tiers resolve in order, each with its own units; the largest threshold the schema
// allows resolves exactly.
func TestPriceTiersResolve(t *testing.T) {
	s := parseFixture(t, "price-tiers.json")
	tiers := s.Models["gpt-5.4"].Prices[1].Tiers
	if len(tiers) != 2 || tiers[0].AboveInputTokens != 0 || tiers[1].AboveInputTokens != 272000 ||
		tiers[1].USDPerMillion[UnitTokensIn] != 5 || tiers[1].USDPerMillion[UnitTokensOut] != 22.5 {
		t.Errorf("gpt-5.4 tiers = %+v", tiers)
	}
	s = parseFixture(t, "price-brackets.json")
	eight := s.Models["eight-brackets"].Prices[0].Tiers
	if len(eight) != 8 || eight[7].AboveInputTokens != 1<<53-1 {
		t.Fatalf("eight-brackets tiers = %+v", eight)
	}
	if units := eight[2].USDPerMillion; len(units) != 1 || units[UnitTokensCached] != 0 {
		t.Errorf("third tier units = %v, want tokens_cached alone", units)
	}
}

func mustReject(t *testing.T, doc string) *ValidationError {
	t.Helper()
	_, err := Parse([]byte(doc))
	var invalid *ValidationError
	if !errors.As(err, &invalid) {
		t.Fatalf("want a *ValidationError, got %v", err)
	}
	return invalid
}

// minimalDoc is protocol/fixtures/config/valid/minimal.json with one field replaceable.
func minimalDoc(backend string) string {
	return `{
  "format_version": 5,
  "global": {},
  "backends": { "local": ` + backend + ` },
  "models": {
    "llama": {
      "deployments": [{ "backend": "local", "model": "llama" }],
      "metadata": {
        "context_length": 8192.0,
        "capabilities": { "streaming": true, "tools": false, "vision": false, "reasoning": false }
      }
    }
  },
  "groups": { "me": {} },
  "keys": {
    "k-me": { "hash": "sha256:59f8f6709d858b919a10541cd215a0de7b4299bbc6c11a2e0bf4ca03ec6df717", "group": "me" }
  }
}`
}

func TestDefaultsAppliedForOmittedFields(t *testing.T) {
	s := parseFixture(t, "minimal.json")
	if s.MaxRequestBodyBytes != 4<<20 {
		t.Errorf("MaxRequestBodyBytes = %d, want 4 MiB", s.MaxRequestBodyBytes)
	}
	if !s.KeyIDLabel {
		t.Error("KeyIDLabel = false, want the default true")
	}
	if !s.GroupLabel {
		t.Error("GroupLabel = false, want the default true")
	}
	if s.ControlOutageGrace != 15*time.Minute {
		t.Errorf("ControlOutageGrace = %v, want the default 15m", s.ControlOutageGrace)
	}
	if s.MaxN != 8 {
		t.Errorf("MaxN = %d, want the default 8", s.MaxN)
	}
	if s.MaxSequencesPerRequest != 16 || s.MaxEmbeddingInputs != 2048 {
		t.Errorf("MaxSequencesPerRequest = %d, MaxEmbeddingInputs = %d, want the defaults 16 and 2048",
			s.MaxSequencesPerRequest, s.MaxEmbeddingInputs)
	}
	if s.MaxConcurrentRequestsPerKey != 16 {
		t.Errorf("MaxConcurrentRequestsPerKey = %d, want the default 16", s.MaxConcurrentRequestsPerKey)
	}
	b := s.Backends["local"]
	if b.ConnectTimeout != 5*time.Second || b.FirstEventTimeout != 60*time.Second ||
		b.ResponseTimeout != 30*time.Minute || b.StallTimeout != 120*time.Second {
		t.Errorf("timeouts = %v / %v / %v / %v, want 5s / 1m / 30m / 2m",
			b.ConnectTimeout, b.FirstEventTimeout, b.ResponseTimeout, b.StallTimeout)
	}
	if m := s.Models["llama"]; m.OutputLimit != nil || len(m.Prices) != 0 {
		t.Errorf("model without output_limit or prices resolved to %+v / %+v", m.OutputLimit, m.Prices)
	}
	if b.MaxInFlight != 0 {
		t.Errorf("MaxInFlight = %d, want 0 (no cap)", b.MaxInFlight)
	}
	m := s.Models["llama"]
	if m.Queue != (Queue{Size: 100, Timeout: 30 * time.Second}) || m.MaxAttempts != 3 {
		t.Errorf("queue %+v, attempts %d, want 100 / 30s and 3", m.Queue, m.MaxAttempts)
	}
	if s.Circuit != (Circuit{FailureThreshold: 5, ProbeInterval: 10 * time.Second}) {
		t.Errorf("Circuit = %+v, want 5 / 10s", s.Circuit)
	}
}

func TestReliabilitySettings(t *testing.T) {
	s := parseFixture(t, "full.json")
	if s.Circuit != (Circuit{FailureThreshold: 3, ProbeInterval: 5 * time.Second}) {
		t.Errorf("Circuit = %+v", s.Circuit)
	}
	if a, b := s.Backends["vllm-a"], s.Backends["vllm-b"]; a.MaxInFlight != 8 || b.MaxInFlight != 0 {
		t.Errorf("MaxInFlight vllm-a %d, vllm-b %d, want 8 and 0 (no cap)", a.MaxInFlight, b.MaxInFlight)
	}
	for name, want := range map[string]struct {
		queue    Queue
		attempts int
	}{
		"bge-m3":    {Queue{Size: 200, Timeout: time.Minute}, 2},   // global settings
		"qwen3-32b": {Queue{Size: 20, Timeout: time.Minute}, 2},    // size overridden, timeout global
		"gpt-4.1":   {Queue{Size: 0, Timeout: 5 * time.Second}, 1}, // both overridden, no retries
	} {
		m := s.Models[name]
		if m.Queue != want.queue || m.MaxAttempts != want.attempts {
			t.Errorf("%s: queue %+v, attempts %d, want %+v, %d", name, m.Queue, m.MaxAttempts, want.queue, want.attempts)
		}
	}
}

// The status reports the configured cap as written (N-P5): no saturation below the
// schema's 2^53 - 1.
func TestHugeMaxInFlightIsKeptWhole(t *testing.T) {
	s, err := Parse([]byte(minimalDoc(`{ "type": "openai-compatible", "base_url": "http://x/v1", "max_in_flight": 9007199254740991 }`)))
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Backends["local"].MaxInFlight; got != 9007199254740991 {
		t.Errorf("MaxInFlight = %d, want 9007199254740991", got)
	}
}

func TestExplicitValuesOverrideDefaults(t *testing.T) {
	s := parseFixture(t, "full.json")
	if s.MaxRequestBodyBytes != 8388608 {
		t.Errorf("MaxRequestBodyBytes = %d", s.MaxRequestBodyBytes)
	}
	if s.ControlOutageGrace != 10*time.Minute {
		t.Errorf("ControlOutageGrace = %v, want 10m", s.ControlOutageGrace)
	}
	if s.MaxN != 4 {
		t.Errorf("MaxN = %d, want 4", s.MaxN)
	}
	if s.MaxSequencesPerRequest != 32 || s.MaxEmbeddingInputs != 512 {
		t.Errorf("MaxSequencesPerRequest = %d, MaxEmbeddingInputs = %d, want 32 and 512",
			s.MaxSequencesPerRequest, s.MaxEmbeddingInputs)
	}
	if s.MaxConcurrentRequestsPerKey != 32 {
		t.Errorf("MaxConcurrentRequestsPerKey = %d, want 32", s.MaxConcurrentRequestsPerKey)
	}
	if !s.KeyIDLabel || s.GroupLabel {
		t.Errorf("KeyIDLabel, GroupLabel = %v, %v; want true, false", s.KeyIDLabel, s.GroupLabel)
	}
	a := s.Backends["vllm-a"]
	if a.ConnectTimeout != 2*time.Second || a.FirstEventTimeout != 120*time.Second ||
		a.ResponseTimeout != DefaultResponseTimeout || a.StallTimeout != 90*time.Second {
		t.Errorf("vllm-a timeouts = %v / %v / %v / %v", a.ConnectTimeout, a.FirstEventTimeout, a.ResponseTimeout, a.StallTimeout)
	}
	az := s.Backends["azure-westeurope"]
	if az.ConnectTimeout != DefaultConnectTimeout || az.FirstEventTimeout != DefaultFirstEventTimeout ||
		az.ResponseTimeout != 600*time.Second || az.StallTimeout != DefaultStallTimeout {
		t.Errorf("azure timeouts = %v / %v / %v / %v", az.ConnectTimeout, az.FirstEventTimeout, az.ResponseTimeout, az.StallTimeout)
	}
	if az.Type != BackendAzureOpenAI || az.APIKeyEnv != "AZURE_WESTEUROPE_API_KEY" {
		t.Errorf("azure backend = %+v", az)
	}
}

func TestModelResolution(t *testing.T) {
	s := parseFixture(t, "full.json")
	if want := []string{"bge-m3", "gpt-4.1", "gpt-4.1-mini", "qwen3-32b"}; !slices.Equal(s.ModelNames, want) {
		t.Errorf("ModelNames = %v, want %v", s.ModelNames, want)
	}
	q := s.Models["qwen3-32b"]
	if len(q.Deployments) != 2 || q.Deployments[1].Backend != s.Backends["vllm-b"] || q.Deployments[1].Model != "Qwen/Qwen3-32B" {
		t.Errorf("deployments = %+v", q.Deployments)
	}
	if q.OutputLimit == nil || *q.OutputLimit != (OutputLimit{Default: 4096, Ceiling: 16384}) {
		t.Errorf("output limit = %+v", q.OutputLimit)
	}
	g := s.Models["gpt-4.1"]
	if len(g.Prices) != 2 {
		t.Fatalf("prices = %+v", g.Prices)
	}
	if want := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC); !g.Prices[1].EffectiveFrom.Equal(want) {
		t.Errorf("effective_from = %v, want %v", g.Prices[1].EffectiveFrom, want)
	}
	if len(g.Prices[0].Tiers) != 1 || g.Prices[0].Tiers[0].AboveInputTokens != 0 {
		t.Fatalf("tiers = %+v, want one at 0", g.Prices[0].Tiers)
	}
	if got := g.Prices[0].Tiers[0].USDPerMillion[UnitTokensCached]; got != 0.5 {
		t.Errorf("tokens_cached price = %v", got)
	}
}

func TestKeysResolveToTheirGroup(t *testing.T) {
	s := parseFixture(t, "full.json")

	k, ok := s.KeyByHash("sha256:f256ba1b01a5a25759658ea228c77040e681593f15f8b4b48f184a811741c514")
	if !ok || k.ID != "k-support-bot" {
		t.Fatalf("KeyByHash = %+v, %v", k, ok)
	}
	if k.Group == nil || k.Group.ID != "support-bot" || k.Group.Parent != s.Groups["support"] || k.Group.Root() != s.Groups["support"] {
		t.Errorf("group = %+v", k.Group)
	}
	if !slices.Equal(k.Group.PathIDs, []string{"support", "support-bot"}) || k.Group.Path[1] != k.Group {
		t.Errorf("path = %v", k.Group.PathIDs)
	}
	if want := time.Date(2027, 3, 31, 23, 59, 59, 0, time.UTC); !k.ExpiresAt.Equal(want) {
		t.Errorf("ExpiresAt = %v", k.ExpiresAt)
	}

	bob := s.Keys["k-bob"]
	if bob.Group != s.Groups["bob"] || s.Groups["users"].Parent != nil || s.Groups["users"].Root() != s.Groups["users"] {
		t.Errorf("k-bob group = %+v", bob.Group)
	}
	if want := time.Date(2026, 12, 31, 12, 0, 0, 5e8, time.UTC); !bob.ExpiresAt.Equal(want) {
		t.Errorf("fractional ExpiresAt = %v", bob.ExpiresAt)
	}
	if !s.Keys["k-bob-old"].Disabled || !s.Keys["k-alice-laptop"].ExpiresAt.IsZero() {
		t.Error("disabled flag or zero expiry not resolved")
	}
	if _, ok := s.KeyByHash("sha256:" + strings.Repeat("0", 64)); ok {
		t.Error("unknown hash found")
	}
}

func TestWildcardExpandsToEveryModel(t *testing.T) {
	s := parseFixture(t, "full.json")
	eval := s.Groups["eval-pipeline"].AllowedModels
	if !eval.All() || !slices.Equal(eval.Names(), s.ModelNames) || !eval.Allows("bge-m3") || eval.Allows("*") {
		t.Errorf(`"*" resolved to all=%v names=%v`, eval.All(), eval.Names())
	}
	support := s.Keys["k-support-bot"].AllowedModels()
	if support.All() || !slices.Equal(support.Names(), []string{"bge-m3", "gpt-4.1-mini"}) || support.Allows("gpt-4.1") {
		t.Errorf("explicit list resolved to all=%v names=%v", support.All(), support.Names())
	}
}

func TestChildDefaultsMergeUnderEachChild(t *testing.T) {
	s := parseFixture(t, "full.json")
	defaults := []Limit{
		{Type: LimitRequestsPerMinute, Value: 30},
		{Type: LimitTokensPerHour, Value: 500000},
		{Type: LimitUSDPerMonth, Value: 20},
	}

	// The users group itself gets nothing from its own child_defaults.
	users := s.Groups["users"]
	if !users.AllowedModels.All() || len(users.Limits) != 0 {
		t.Errorf("users: allowed %v, limits %+v", users.AllowedModels.Names(), users.Limits)
	}

	// carol: an empty entry takes everything from the defaults.
	carol := s.Groups["carol"]
	if !slices.Equal(carol.AllowedModels.Names(), []string{"bge-m3", "gpt-4.1-mini", "qwen3-32b"}) {
		t.Errorf("carol allowed = %v", carol.AllowedModels.Names())
	}
	assertLimits(t, "carol", carol.Limits, defaults)

	// alice: allowed_models ["*"] replaces the default list; her usd limit replaces
	// the default one in place; the others still apply.
	alice := s.Groups["alice"]
	if !alice.AllowedModels.All() || !alice.AllowedModels.Allows("gpt-4.1") {
		t.Errorf("alice allowed = %v", alice.AllowedModels.Names())
	}
	assertLimits(t, "alice", alice.Limits, []Limit{defaults[0], defaults[1], {Type: LimitUSDPerMonth, Value: 200}})

	// bob: no allowed_models, so the default list applies.
	bob := s.Groups["bob"]
	if bob.AllowedModels.Allows("gpt-4.1") || !bob.AllowedModels.Allows("qwen3-32b") {
		t.Errorf("bob allowed = %v", bob.AllowedModels.Names())
	}
	assertLimits(t, "bob", bob.Limits, []Limit{defaults[0], {Type: LimitTokensPerHour, Value: 2000000}, defaults[2]})
}

func TestChildOverrideMatchesOnType(t *testing.T) {
	s := parseFixture(t, "users-child-defaults.json")

	// The requests_per_minute override replaces the default one in place; the
	// tokens_per_hour default still applies.
	power := s.Groups["power-user@example.com"]
	assertLimits(t, "power-user", power.Limits, []Limit{
		{Type: LimitRequestsPerMinute, Value: 100},
		{Type: LimitTokensPerHour, Value: 100000},
	})
	if !slices.Equal(power.AllowedModels.Names(), []string{"large", "small"}) {
		t.Errorf("power-user allowed = %v", power.AllowedModels.Names())
	}

	// An empty allowed_models list replaces the default: the group may use nothing.
	locked := s.Groups["locked-out"]
	if len(locked.AllowedModels.Names()) != 0 || locked.AllowedModels.Allows("small") || locked.AllowedModels.All() {
		t.Errorf("locked-out allowed = %v", locked.AllowedModels.Names())
	}
}

func TestMergeLimitsAppendsOverridesOfAnotherType(t *testing.T) {
	defaults := []Limit{{Type: LimitTokensPerHour, Value: 10}}
	overrides := []Limit{{Type: LimitUSDPerMonth, Value: 99}, {Type: LimitTokensPerHour, Value: 20}}
	assertLimits(t, "merged", mergeLimits(defaults, overrides), []Limit{overrides[1], overrides[0]})
}

func assertLimits(t *testing.T, who string, got, want []Limit) {
	t.Helper()
	if !slices.EqualFunc(got, want, func(a, b Limit) bool {
		return a.Type == b.Type && a.Value == b.Value
	}) {
		t.Errorf("%s limits = %+v, want %+v", who, got, want)
	}
}

func TestIntegerWrittenWithFractionIsAnInteger(t *testing.T) {
	// JSON Schema (and kaiak-control) treat 8192.0 as an integer.
	s, err := Parse([]byte(minimalDoc(`{ "type": "openai-compatible", "base_url": "http://x/v1", "connect_timeout_ms": 1500.0 }`)))
	if err != nil {
		t.Fatal(err)
	}
	if s.Models["llama"].ContextLength != 8192 || s.Backends["local"].ConnectTimeout != 1500*time.Millisecond {
		t.Errorf("context %d, timeout %v", s.Models["llama"].ContextLength, s.Backends["local"].ConnectTimeout)
	}
}

func TestRejectionsOutsideTheFixtures(t *testing.T) {
	valid := minimalDoc(`{ "type": "openai-compatible", "base_url": "http://x/v1" }`)
	for _, tc := range []struct {
		name, doc, code string
	}{
		{"not JSON", `{"format_version": 1`, CodeSyntax},
		{"trailing data", valid + ` {}`, CodeSyntax},
		{"null field", minimalDoc(`{ "type": "openai-compatible", "base_url": "http://x/v1", "api_key_env": null }`), CodeSchema},
		{"null collection", strings.Replace(valid, `"groups": { "me": {} }`, `"groups": null`, 1), CodeSchema},
		{"fractional timeout", minimalDoc(`{ "type": "openai-compatible", "base_url": "http://x/v1", "connect_timeout_ms": 1.5 }`), CodeSchema},
		// The schema's patterns are ECMAScript: \s includes \v and Unicode spaces.
		{"vertical tab in base_url", minimalDoc(`{ "type": "openai-compatible", "base_url": "http://x/v\u000b1" }`), CodeSchema},
		{"no-break space in base_url", minimalDoc(`{ "type": "openai-compatible", "base_url": "http://x y/v1" }`), CodeSchema},
	} {
		t.Run(tc.name, func(t *testing.T) {
			invalid := mustReject(t, tc.doc)
			if codes := invalid.Codes(); !slices.Equal(codes, []string{tc.code}) {
				t.Errorf("codes %v, want [%s]: %v", codes, tc.code, invalid)
			}
		})
	}
}

func TestSchemaIssuesCarryPaths(t *testing.T) {
	invalid := mustReject(t, minimalDoc(`{ "type": "openai-compatible", "base_url": "http://x/v1", "api_key": "secret", "connect_timeout_ms": "5s" }`))
	var paths []string
	for _, issue := range invalid.Issues {
		paths = append(paths, issue.Path)
	}
	want := []string{"/backends/local/connect_timeout_ms", "/backends/local/api_key"}
	if !slices.Equal(paths, want) {
		t.Errorf("paths = %v, want %v (every issue reported, each with its path)", paths, want)
	}
}

// The tree rules report each defect where it is, once per affected group: every group
// on a cycle, the group naming an unknown parent, and each group past the depth limit;
// groups below a cycle or an unknown parent report nothing of their own.
func TestGroupTreeIssues(t *testing.T) {
	doc := strings.Replace(minimalDoc(`{ "type": "openai-compatible", "base_url": "http://x/v1" }`),
		`"groups": { "me": {} }`, `"groups": {
    "me": {},
    "a": { "parent": "b" }, "b": { "parent": "a" }, "below-cycle": { "parent": "a" },
    "orphan": { "parent": "nowhere" }, "below-orphan": { "parent": "orphan" },
    "l1": {}, "l2": { "parent": "l1" }, "l3": { "parent": "l2" }, "l4": { "parent": "l3" },
    "l5": { "parent": "l4" }, "l6": { "parent": "l5" }, "l7": { "parent": "l6" },
    "l8": { "parent": "l7" }, "l9": { "parent": "l8" }, "l10": { "parent": "l9" }
  }`, 1)
	invalid := mustReject(t, doc)
	var got []string
	for _, issue := range invalid.Issues {
		got = append(got, issue.Code+" "+issue.Path)
	}
	slices.Sort(got)
	want := []string{
		"group-cycle /groups/a/parent",
		"group-cycle /groups/b/parent",
		"group-depth-exceeded /groups/l10",
		"group-depth-exceeded /groups/l9",
		"group-parent-unknown /groups/orphan/parent",
	}
	if !slices.Equal(got, want) {
		t.Errorf("issues\n %v\nwant\n %v", got, want)
	}
}

// The tier rules report each offending threshold where it is, in every price entry: a
// first tier above 0, and each tier not above the one before (equal or lower).
func TestPriceTierIssues(t *testing.T) {
	doc := strings.Replace(minimalDoc(`{ "type": "openai-compatible", "base_url": "http://x/v1" }`),
		`"metadata": {`, `"prices": [
        { "effective_from": "2026-01-01", "tiers": [
          { "above_input_tokens": 0, "usd_per_million": { "tokens_in": 1 } },
          { "above_input_tokens": 1000, "usd_per_million": { "tokens_in": 2 } },
          { "above_input_tokens": 1000, "usd_per_million": { "tokens_in": 3 } },
          { "above_input_tokens": 999, "usd_per_million": { "tokens_in": 4 } } ] },
        { "effective_from": "2026-02-01", "tiers": [
          { "above_input_tokens": 5, "usd_per_million": { "tokens_in": 1 } } ] } ],
      "metadata": {`, 1)
	invalid := mustReject(t, doc)
	var got []string
	for _, issue := range invalid.Issues {
		got = append(got, issue.Code+" "+issue.Path)
	}
	slices.Sort(got)
	want := []string{
		"price-tier-first-not-zero /models/llama/prices/1/tiers/0/above_input_tokens",
		"price-tiers-not-increasing /models/llama/prices/0/tiers/2/above_input_tokens",
		"price-tiers-not-increasing /models/llama/prices/0/tiers/3/above_input_tokens",
	}
	if !slices.Equal(got, want) {
		t.Errorf("issues\n %v\nwant\n %v", got, want)
	}
}

// The counter count: two for global and for every group, limited or not, and one per
// effective per-minute limit — a parent's per-minute defaults count once per direct
// child that has no own limit of the type; hour and month limits add none; a group
// whose parent has no entry counts its own limits alone.
func TestCountCounters(t *testing.T) {
	rpm := limitDoc{Type: string(LimitRequestsPerMinute), Value: 1}
	tpm := limitDoc{Type: string(LimitTokensPerMinute), Value: 1}
	tph := limitDoc{Type: string(LimitTokensPerHour), Value: 1}
	usd := limitDoc{Type: string(LimitUSDPerMonth), Value: 2}
	doc := &document{
		Global: globalDoc{Limits: []limitDoc{rpm, usd}},
		Groups: map[string]groupDoc{
			"users":    {Limits: []limitDoc{rpm}, ChildDefaults: &childDefaultsDoc{Limits: []limitDoc{rpm, tph}}},
			"plain":    {Parent: "users"},
			"override": {Parent: "users", Limits: []limitDoc{{Type: string(LimitRequestsPerMinute), Value: 9}}},
			"extra":    {Parent: "users", Limits: []limitDoc{tpm, usd}},
			"orphan":   {Parent: "nowhere", Limits: []limitDoc{rpm, tph}},
			"below":    {Parent: "plain"},
			"empty":    {},
		},
	}
	// global 3 + users 3 + plain 3 + override 3 + extra 4 + orphan 3 + below 2 + empty 2.
	if got := countCounters(doc); got != 23 {
		t.Errorf("count = %d, want 23", got)
	}
}

// The bound counts groups without limits, and is reported once, at the document root.
func TestCountersExceededAtRoot(t *testing.T) {
	groups := map[string]groupDoc{}
	for i := range MaxCounters/2 - 1 {
		groups[fmt.Sprintf("u%d", i)] = groupDoc{}
	}
	doc := &document{Groups: groups}
	if issues := checkSemantics(doc); len(issues) != 0 {
		t.Fatalf("at the bound: %v", issues)
	}
	doc.Global.Limits = []limitDoc{{Type: string(LimitRequestsPerMinute), Value: 1}}
	issues := checkSemantics(doc)
	if len(issues) != 1 || issues[0].Code != CodeCountersExceeded || issues[0].Path != "" {
		t.Errorf("one over the bound: %v, want one %s at \"\"", issues, CodeCountersExceeded)
	}
}
