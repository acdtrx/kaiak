package main

import "slices"

// The client API endpoints a kind's backend type serves (docs/specs/GATEWAY.md,
// Providers → Endpoint support), by the gateway's endpoint names. The kit runs each
// API's checks on the kinds that serve it, and checks the gateway refuses the others
// with endpoint_not_served.
const (
	epChat        = "chat_completions"
	epCompletions = "completions"
	epEmbeddings  = "embeddings"
	epMessages    = "messages"
	epCountTokens = "messages_count_tokens"
	epResponses   = "responses"
	epInputTokens = "responses_input_tokens"
)

var kindEndpoints = map[string][]string{
	kindVLLM:           {epChat, epCompletions, epEmbeddings, epMessages, epCountTokens, epResponses},
	kindLlamaServer:    {epChat, epCompletions, epEmbeddings, epMessages, epCountTokens, epResponses, epInputTokens},
	kindOpenAI:         {epChat, epCompletions, epEmbeddings, epResponses, epInputTokens},
	kindAzure:          {epChat, epCompletions, epEmbeddings, epResponses},
	kindAnthropic:      {epMessages, epCountTokens},
	kindAzureAnthropic: {epMessages, epCountTokens},
}

// serves reports whether the run's kind serves the endpoint.
func (o options) serves(endpoint string) bool {
	return slices.Contains(kindEndpoints[o.kind], endpoint)
}

// anthropicKind: a kind serving Claude through Anthropic's API, where the gateway
// holds requests to the standard price.
func anthropicKind(kind string) bool { return kind == kindAnthropic || kind == kindAzureAnthropic }
