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

	"kaiak/internal/fixturetest"
	"kaiak/internal/schemacheck"
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

func mustReject(t *testing.T, doc string) *schemacheck.ValidationError {
	t.Helper()
	_, err := Parse([]byte(doc))
	var invalid *schemacheck.ValidationError
	if !errors.As(err, &invalid) {
		t.Fatalf("want a *schemacheck.ValidationError, got %v", err)
	}
	return invalid
}

// minimalDoc is protocol/fixtures/config/valid/minimal.json with one field replaceable.
func minimalDoc(backend string) string {
	return `{
  "protocol_version": 8,
  "global": {},
  "backends": { "local": ` + backend + ` },
  "models": {
    "llama": {
      "deployments": [{ "backend": "local", "model": "llama" }],
      "endpoints": ["chat_completions"],
      "metadata": {
        "context_length": 8192.0,
        "capabilities": { "streaming": true, "tools": false, "vision": false, "reasoning": false }
      }
    }
  },
  "groups": { "me": { "capture": false } },
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
	if s.MaxUploadBytes != 25<<20 {
		t.Errorf("MaxUploadBytes = %d, want 25 MiB", s.MaxUploadBytes)
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
	if s.MaxSequencesPerRequest != 16 || s.MaxEmbeddingInputs != 2048 || s.MaxRerankDocuments != 1000 {
		t.Errorf("MaxSequencesPerRequest = %d, MaxEmbeddingInputs = %d, MaxRerankDocuments = %d, want the defaults 16, 2048 and 1000",
			s.MaxSequencesPerRequest, s.MaxEmbeddingInputs, s.MaxRerankDocuments)
	}
	if s.MaxImagesPerRequest != 10 || s.MaxSpeechBatchItems != 32 {
		t.Errorf("MaxImagesPerRequest = %d, MaxSpeechBatchItems = %d, want the defaults 10 and 32",
			s.MaxImagesPerRequest, s.MaxSpeechBatchItems)
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

// The status reports the configured cap as written: no saturation below the
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
	if s.MaxUploadBytes != 52428800 {
		t.Errorf("MaxUploadBytes = %d, want 50 MiB", s.MaxUploadBytes)
	}
	if s.ControlOutageGrace != 10*time.Minute {
		t.Errorf("ControlOutageGrace = %v, want 10m", s.ControlOutageGrace)
	}
	if s.MaxN != 4 {
		t.Errorf("MaxN = %d, want 4", s.MaxN)
	}
	if s.MaxSequencesPerRequest != 32 || s.MaxEmbeddingInputs != 512 || s.MaxRerankDocuments != 250 {
		t.Errorf("MaxSequencesPerRequest = %d, MaxEmbeddingInputs = %d, MaxRerankDocuments = %d, want 32, 512 and 250",
			s.MaxSequencesPerRequest, s.MaxEmbeddingInputs, s.MaxRerankDocuments)
	}
	if s.MaxImagesPerRequest != 6 || s.MaxSpeechBatchItems != 16 {
		t.Errorf("MaxImagesPerRequest = %d, MaxSpeechBatchItems = %d, want 6 and 16",
			s.MaxImagesPerRequest, s.MaxSpeechBatchItems)
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
	if want := []string{"bge-m3", "gpt-4.1", "gpt-4.1-mini", "qwen-image", "qwen3-32b", "qwen3-tts"}; !slices.Equal(s.ModelNames, want) {
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

// A model's endpoints list resolves as written, and the model serves exactly what it
// names.
func TestModelEndpointsResolve(t *testing.T) {
	s := parseFixture(t, "model-endpoints.json")
	for name, want := range map[string][]EndpointName{
		"image": {EndpointImagesGenerations, EndpointImagesEdits},
		"asr":   {EndpointAudioTranscriptions},
		"chat": {EndpointChatCompletions, EndpointCompletions, EndpointMessages, EndpointMessagesCountTokens,
			EndpointResponses},
	} {
		m := s.Models[name]
		if !slices.Equal(m.Endpoints, want) {
			t.Errorf("%s endpoints = %v, want %v", name, m.Endpoints, want)
		}
		for _, e := range endpointNames {
			if got := m.Admits(EndpointName(e)); got != slices.Contains(want, EndpointName(e)) {
				t.Errorf("%s admits %s: %v, want %v", name, e, got, !got)
			}
		}
	}
}

// The backend-type rules: a model on one type reports each listed endpoint the type
// does not serve, at its index; a model whose deployments' types differ reports that
// once, at the backend of the first deployment whose type differs from the first
// one's, and its list is not judged; a model with a deployment on an unknown backend
// reports that alone.
func TestBackendTypeIssues(t *testing.T) {
	doc := minimalDoc(`{ "type": "vllm", "base_url": "http://x/v1" }`)
	doc = strings.Replace(doc, `"backends": {`, `"backends": {
    "local-b": { "type": "vllm", "base_url": "http://y/v1" },
    "omni": { "type": "vllm-omni", "base_url": "http://omni/v1" },`, 1)
	oneType := strings.Replace(doc, `"deployments": [{ "backend": "local", "model": "llama" }],
      "endpoints": ["chat_completions"],`,
		`"deployments": [{ "backend": "local", "model": "llama" }, { "backend": "local-b", "model": "llama" }],
      "endpoints": ["rerank", "responses_input_tokens", "audio_speech", "messages_count_tokens", "completions"],`, 1)
	issues := func(doc string) []string {
		var got []string
		for _, issue := range mustReject(t, doc).Issues {
			got = append(got, issue.Code+" "+issue.Path)
		}
		return got
	}
	if got, want := issues(oneType), []string{"endpoint-not-served /models/llama/endpoints/1",
		"endpoint-not-served /models/llama/endpoints/2"}; !slices.Equal(got, want) {
		t.Errorf("one type: issues %v, want %v", got, want)
	}

	typesDiffer := strings.Replace(oneType, `{ "backend": "local-b", "model": "llama" }`,
		`{ "backend": "local-b", "model": "llama" }, { "backend": "omni", "model": "llama" }, { "backend": "local", "model": "other" }`, 1)
	if got, want := issues(typesDiffer), []string{"deployment-types-differ /models/llama/deployments/2/backend"}; !slices.Equal(got, want) {
		t.Errorf("types differ: issues %v, want %v", got, want)
	}

	unknown := strings.Replace(typesDiffer, `{ "backend": "local-b", "model": "llama" }`, `{ "backend": "ghost", "model": "llama" }`, 1)
	if got, want := issues(unknown), []string{"deployment-backend-unknown /models/llama/deployments/1/backend"}; !slices.Equal(got, want) {
		t.Errorf("unknown backend: issues %v, want %v", got, want)
	}
}

// The shared fixtures of the backend-type rules report at the paths the contract
// names, the types differing once per model, a model on an unknown backend that rule
// alone (docs/specs/CONTROL-PROTOCOL.md, Config → semantic rules).
func TestBackendTypeFixturePaths(t *testing.T) {
	for file, want := range map[string][]string{
		"deployment-types-differ.json": {"deployment-types-differ /models/chat/deployments/2/backend"},
		"endpoints-types-differ.json":  {"deployment-types-differ /models/chat/deployments/1/backend"},
		"endpoints-not-served.json": {"endpoint-not-served /models/image/endpoints/1",
			"endpoint-not-served /models/reranker/endpoints/1"},
		"endpoints-deployment-backend-unknown.json": {"deployment-backend-unknown /models/tts/deployments/1/backend"},
	} {
		t.Run(file, func(t *testing.T) {
			_, err := Parse(fixturetest.Read(t, filepath.Join(fixturesDir, "invalid", file)))
			invalid, ok := errors.AsType[*schemacheck.ValidationError](err)
			if !ok {
				t.Fatalf("want a validation error, got %v", err)
			}
			var got []string
			for _, issue := range invalid.Issues {
				got = append(got, issue.Code+" "+issue.Path)
			}
			if !slices.Equal(got, want) {
				t.Errorf("issues %v, want %v", got, want)
			}
		})
	}
}

// keyByHash is the snapshot's key with hash, failing the test when there is none.
func keyByHash(t *testing.T, s *Snapshot, hash string) *Key {
	t.Helper()
	k, ok := s.KeyByHash(hash)
	if !ok {
		t.Fatalf("no key with hash %s", hash)
	}
	return k
}

func TestKeysResolveToTheirGroup(t *testing.T) {
	s := parseFixture(t, "full.json")

	k, ok := s.KeyByHash("sha256:f256ba1b01a5a25759658ea228c77040e681593f15f8b4b48f184a811741c514")
	if !ok || k.ID != "k-support-bot" {
		t.Fatalf("KeyByHash = %+v, %v", k, ok)
	}
	if k.Group == nil || k.Group.ID != "support-bot" || k.Group != s.Groups["support-bot"] {
		t.Errorf("group = %+v", k.Group)
	}
	if !slices.Equal(k.Group.PathIDs, []string{"support", "support-bot"}) {
		t.Errorf("path = %v", k.Group.PathIDs)
	}
	if want := time.Date(2027, 3, 31, 23, 59, 59, 0, time.UTC); !k.ExpiresAt.Equal(want) {
		t.Errorf("ExpiresAt = %v", k.ExpiresAt)
	}

	bob := keyByHash(t, s, "sha256:caa32f96ff6009dfa680f8f85bfe87cf698ea2999e5676aaee65a2b5eb62cddd")
	if bob.ID != "k-bob" || bob.Group != s.Groups["bob"] || !slices.Equal(s.Groups["users"].PathIDs, []string{"users"}) {
		t.Errorf("k-bob group = %+v", bob.Group)
	}
	if want := time.Date(2026, 12, 31, 12, 0, 0, 5e8, time.UTC); !bob.ExpiresAt.Equal(want) {
		t.Errorf("fractional ExpiresAt = %v", bob.ExpiresAt)
	}
	bobOld := keyByHash(t, s, "sha256:63e40536c95622c5e768300e6a9030385f72f1ce4d5bf7a249cb2dd49d1615ef")
	aliceLaptop := keyByHash(t, s, "sha256:59f8f6709d858b919a10541cd215a0de7b4299bbc6c11a2e0bf4ca03ec6df717")
	if bobOld.ID != "k-bob-old" || aliceLaptop.ID != "k-alice-laptop" {
		t.Fatalf("keys by hash: %s, %s", bobOld.ID, aliceLaptop.ID)
	}
	if !bobOld.Disabled || !aliceLaptop.ExpiresAt.IsZero() {
		t.Error("disabled flag or zero expiry not resolved")
	}
	if _, ok := s.KeyByHash("sha256:" + strings.Repeat("0", 64)); ok {
		t.Error("unknown hash found")
	}
}

func TestWildcardExpandsToEveryModel(t *testing.T) {
	s := parseFixture(t, "full.json")
	eval := s.Groups["eval-pipeline"].AllowedModels
	if !slices.Equal(eval.Names(), s.ModelNames) || !eval.Allows("bge-m3") || eval.Allows("*") {
		t.Errorf(`"*" resolved to names=%v`, eval.Names())
	}
	support := s.Groups["support-bot"].AllowedModels
	if !slices.Equal(support.Names(), []string{"bge-m3", "gpt-4.1-mini"}) || support.Allows("gpt-4.1") {
		t.Errorf("explicit list resolved to names=%v", support.Names())
	}
}

func TestChildDefaultsMergeUnderEachChild(t *testing.T) {
	s := parseFixture(t, "full.json")
	defaults := []Limit{
		{Type: LimitRequestsPerMinute, Value: 30},
		{Type: LimitTokensPerHour, Value: 500000},
		{Type: LimitUSDPerMonth, Value: 20},
		{Type: LimitAudioSecondsOutPerHour, Value: 600},
	}

	// The users group itself gets nothing from its own child_defaults.
	users := s.Groups["users"]
	if !slices.Equal(users.AllowedModels.Names(), s.ModelNames) || len(users.Limits) != 0 {
		t.Errorf("users: allowed %v, limits %+v", users.AllowedModels.Names(), users.Limits)
	}

	// carol: an empty entry takes everything from the defaults.
	carol := s.Groups["carol"]
	if !slices.Equal(carol.AllowedModels.Names(), []string{"bge-m3", "gpt-4.1-mini", "qwen3-32b", "qwen3-tts"}) {
		t.Errorf("carol allowed = %v", carol.AllowedModels.Names())
	}
	assertLimits(t, "carol", carol.Limits, defaults)

	// alice: allowed_models ["*"] replaces the default list; her usd and audio limits
	// replace the default ones in place; the others still apply.
	alice := s.Groups["alice"]
	if !slices.Equal(alice.AllowedModels.Names(), s.ModelNames) || !alice.AllowedModels.Allows("gpt-4.1") {
		t.Errorf("alice allowed = %v", alice.AllowedModels.Names())
	}
	assertLimits(t, "alice", alice.Limits, []Limit{defaults[0], defaults[1], {Type: LimitUSDPerMonth, Value: 200},
		{Type: LimitAudioSecondsOutPerHour, Value: 3600}})

	// bob: no allowed_models, so the default list applies.
	bob := s.Groups["bob"]
	if bob.AllowedModels.Allows("gpt-4.1") || !bob.AllowedModels.Allows("qwen3-32b") {
		t.Errorf("bob allowed = %v", bob.AllowedModels.Names())
	}
	assertLimits(t, "bob", bob.Limits, []Limit{defaults[0], {Type: LimitTokensPerHour, Value: 2000000}, defaults[2], defaults[3]})
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
	if len(locked.AllowedModels.Names()) != 0 || locked.AllowedModels.Allows("small") {
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
		{"not JSON", `{"protocol_version": 8`, schemacheck.CodeSyntax},
		{"trailing data", valid + ` {}`, schemacheck.CodeSyntax},
		{"null field", minimalDoc(`{ "type": "openai-compatible", "base_url": "http://x/v1", "api_key_env": null }`), schemacheck.CodeSchema},
		{"null collection", strings.Replace(valid, `"groups": { "me": { "capture": false } }`, `"groups": null`, 1), schemacheck.CodeSchema},
		{"fractional timeout", minimalDoc(`{ "type": "openai-compatible", "base_url": "http://x/v1", "connect_timeout_ms": 1.5 }`), schemacheck.CodeSchema},
		// The schema's patterns are ECMAScript: \s includes \v and Unicode spaces.
		{"vertical tab in base_url", minimalDoc(`{ "type": "openai-compatible", "base_url": "http://x/v\u000b1" }`), schemacheck.CodeSchema},
		{"no-break space in base_url", minimalDoc(`{ "type": "openai-compatible", "base_url": "http://x y/v1" }`), schemacheck.CodeSchema},
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
		`"groups": { "me": { "capture": false } }`, `"groups": {
    "me": { "capture": false },
    "a": { "parent": "b", "capture": false }, "b": { "parent": "a", "capture": false },
    "below-cycle": { "parent": "a", "capture": false },
    "orphan": { "parent": "nowhere", "capture": false }, "below-orphan": { "parent": "orphan", "capture": false },
    "l1": { "capture": false }, "l2": { "parent": "l1", "capture": false }, "l3": { "parent": "l2", "capture": false },
    "l4": { "parent": "l3", "capture": false }, "l5": { "parent": "l4", "capture": false },
    "l6": { "parent": "l5", "capture": false }, "l7": { "parent": "l6", "capture": false },
    "l8": { "parent": "l7", "capture": false }, "l9": { "parent": "l8", "capture": false },
    "l10": { "parent": "l9", "capture": false }
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

// The counter count: five for global and for every group — one per counted type, the
// hour counts of tokens, images, audio generated and audio processed and the USD
// month — limited or not, and one per effective per-minute limit, the media ones
// included — a parent's per-minute defaults count once per direct child that has no
// own limit of the type; hour and month limits add none; a group whose parent has no
// entry counts its own limits alone.
func TestCountCounters(t *testing.T) {
	if got := countersPerScope(); got != 5 {
		t.Fatalf("counters per scope = %d, want 5", got)
	}
	rpm := limitDoc{Type: string(LimitRequestsPerMinute), Value: 1}
	tpm := limitDoc{Type: string(LimitTokensPerMinute), Value: 1}
	tph := limitDoc{Type: string(LimitTokensPerHour), Value: 1}
	ipm := limitDoc{Type: string(LimitImagesPerMinute), Value: 1}
	aph := limitDoc{Type: string(LimitAudioSecondsOutPerHour), Value: 1}
	apm := limitDoc{Type: string(LimitAudioSecondsInPerMinute), Value: 1}
	usd := limitDoc{Type: string(LimitUSDPerMonth), Value: 2}
	doc := &document{
		Global: globalDoc{Limits: []limitDoc{rpm, usd, ipm}},
		Groups: map[string]groupDoc{
			"users":    {Limits: []limitDoc{rpm}, ChildDefaults: &childDefaultsDoc{Limits: []limitDoc{rpm, tph, apm}}},
			"plain":    {Parent: "users"},
			"override": {Parent: "users", Limits: []limitDoc{{Type: string(LimitRequestsPerMinute), Value: 9}, aph}},
			"extra":    {Parent: "users", Limits: []limitDoc{tpm, usd}},
			"orphan":   {Parent: "nowhere", Limits: []limitDoc{rpm, tph}},
			"below":    {Parent: "plain"},
			"empty":    {},
		},
	}
	// global 7 + users 6 + plain 7 + override 7 + extra 8 + orphan 6 + below 5 + empty 5.
	if got := countCounters(doc); got != 51 {
		t.Errorf("count = %d, want 51", got)
	}
}

// The bound counts groups without limits, and is reported once, at the document root.
func TestCountersExceededAtRoot(t *testing.T) {
	groups := map[string]groupDoc{}
	for i := range MaxCounters/countersPerScope() - 1 {
		groups[fmt.Sprintf("u%d", i)] = groupDoc{}
	}
	doc := &document{Groups: groups}
	if issues := checkSemantics(doc); len(issues) != 0 {
		t.Fatalf("at the bound: %v", issues)
	}
	doc.Global.Limits = []limitDoc{{Type: string(LimitRequestsPerMinute), Value: 1}}
	issues := checkSemantics(doc)
	if len(issues) != 1 || issues[0].Code != CodeCountersExceeded || issues[0].Path != "" ||
		!strings.HasPrefix(issues[0].Message, "50001 counters (5 for global and for every group") {
		t.Errorf("one over the bound: %v, want one %s at \"\" counting 50001 at five per scope", issues, CodeCountersExceeded)
	}
}
