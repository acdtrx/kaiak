package config

// The config document as decoded, before resolution. These types follow
// protocol/schema/config.schema.json and are decoded only after the document passed
// checkSchema, so every field has the type the schema requires. Numbers are float64:
// the schema calls a number with no fractional part an integer (4096.0 included), and
// checkSchema has already enforced integrality and range. Pointers mark fields whose
// absence means something (a default applies); a nil slice is an omitted list, an
// empty one an empty list.

type document struct {
	FormatVersion float64               `json:"format_version"`
	Global        globalDoc             `json:"global"`
	Backends      map[string]backendDoc `json:"backends"`
	Models        map[string]modelDoc   `json:"models"`
	Groups        map[string]groupDoc   `json:"groups"`
	Keys          map[string]keyDoc     `json:"keys"`
}

type globalDoc struct {
	Limits               []limitDoc  `json:"limits"`
	MaxRequestBodyBytes  *float64    `json:"max_request_body_bytes"`
	ControlOutageGraceMS *float64    `json:"control_outage_grace_ms"`
	MaxN                 *float64    `json:"max_n"`
	MaxSequences         *float64    `json:"max_sequences_per_request"`
	MaxEmbeddingInputs   *float64    `json:"max_embedding_inputs"`
	MaxConcurrentPerKey  *float64    `json:"max_concurrent_requests_per_key"`
	Queue                *queueDoc   `json:"queue"`
	Retries              *retriesDoc `json:"retries"`
	Circuit              *circuitDoc `json:"circuit"`
	Metrics              *metricsDoc `json:"metrics"`
}

// queueDoc is global.queue or a model's override; a nil field keeps the value it
// overrides.
type queueDoc struct {
	Size      *float64 `json:"size"`
	TimeoutMS *float64 `json:"timeout_ms"`
}

type retriesDoc struct {
	MaxAttempts *float64 `json:"max_attempts"`
}

type circuitDoc struct {
	FailureThreshold *float64 `json:"failure_threshold"`
	ProbeIntervalMS  *float64 `json:"probe_interval_ms"`
}

type metricsDoc struct {
	KeyIDLabel *bool `json:"key_id_label"`
	GroupLabel *bool `json:"group_label"`
}

type backendDoc struct {
	Type                string   `json:"type"`
	BaseURL             string   `json:"base_url"`
	APIKeyEnv           string   `json:"api_key_env"`
	ConnectTimeoutMS    *float64 `json:"connect_timeout_ms"`
	FirstEventTimeoutMS *float64 `json:"first_event_timeout_ms"`
	ResponseTimeoutMS   *float64 `json:"response_timeout_ms"`
	StallTimeoutMS      *float64 `json:"stall_timeout_ms"`
	MaxInFlight         *float64 `json:"max_in_flight"`
}

type modelDoc struct {
	Deployments []deploymentDoc `json:"deployments"`
	Metadata    metadataDoc     `json:"metadata"`
	OutputLimit *outputLimitDoc `json:"output_limit"`
	Queue       *queueDoc       `json:"queue"`
	Retries     *retriesDoc     `json:"retries"`
	Prices      []priceDoc      `json:"prices"`
}

type deploymentDoc struct {
	Backend string `json:"backend"`
	Model   string `json:"model"`
}

type metadataDoc struct {
	ContextLength    float64         `json:"context_length"`
	Capabilities     capabilitiesDoc `json:"capabilities"`
	ReasoningEfforts []string        `json:"reasoning_efforts"`
}

type capabilitiesDoc struct {
	Streaming bool `json:"streaming"`
	Tools     bool `json:"tools"`
	Vision    bool `json:"vision"`
	Reasoning bool `json:"reasoning"`
}

type outputLimitDoc struct {
	Default float64 `json:"default"`
	Ceiling float64 `json:"ceiling"`
}

type priceDoc struct {
	EffectiveFrom string         `json:"effective_from"`
	Tiers         []priceTierDoc `json:"tiers"`
}

type priceTierDoc struct {
	AboveInputTokens float64            `json:"above_input_tokens"`
	USDPerMillion    map[string]float64 `json:"usd_per_million"`
}

// groupDoc: Parent "" is a top-level group (IDs are never empty). A nil
// AllowedModels is an omitted list; Labels are checked by the schema and otherwise
// ignored.
type groupDoc struct {
	Parent        string            `json:"parent"`
	Labels        map[string]string `json:"labels"`
	AllowedModels []string          `json:"allowed_models"`
	Limits        []limitDoc        `json:"limits"`
	ChildDefaults *childDefaultsDoc `json:"child_defaults"`
}

// childDefaultsDoc applies to each direct child of the group that holds it.
type childDefaultsDoc struct {
	AllowedModels []string   `json:"allowed_models"`
	Limits        []limitDoc `json:"limits"`
}

// keyDoc: IDs and timestamps are never empty strings (schema patterns), so "" means
// the field was omitted.
type keyDoc struct {
	Hash      string `json:"hash"`
	Group     string `json:"group"`
	ExpiresAt string `json:"expires_at"`
	Disabled  bool   `json:"disabled"`
}

type limitDoc struct {
	Type  string  `json:"type"`
	Value float64 `json:"value"`
}
