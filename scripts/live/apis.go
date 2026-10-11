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
	epRerank      = "rerank"
	// The media endpoints vllm-omni serves, and nothing else serves. It is no kind of
	// its own: its servers join a run of any kind (-speech-base-url, -image-base-url,
	// -edit-base-url).
	epSpeech      = "audio_speech"
	epSpeechBatch = "audio_speech_batch"
	epImages      = "images_generations"
	epEdits       = "images_edits"
	// Speech-to-text, which vllm and llama-server serve.
	epTranscriptions = "audio_transcriptions"
)

// kindEndpoints are the endpoints each kind's backend type serves; none serves
// vllm-omni's media endpoints.
var kindEndpoints = map[string][]string{
	kindVLLM:           {epChat, epCompletions, epEmbeddings, epMessages, epCountTokens, epResponses, epRerank, epTranscriptions},
	kindLlamaServer:    {epChat, epCompletions, epEmbeddings, epMessages, epCountTokens, epResponses, epInputTokens, epRerank, epTranscriptions},
	kindOpenAI:         {epChat, epCompletions, epEmbeddings, epResponses, epInputTokens},
	kindAzure:          {epChat, epCompletions, epEmbeddings, epResponses},
	kindAnthropic:      {epMessages, epCountTokens},
	kindAzureAnthropic: {epMessages, epCountTokens},
}

// serves reports whether the run's kind serves the endpoint.
func (o options) serves(endpoint string) bool {
	return slices.Contains(kindEndpoints[o.kind], endpoint)
}

// chatEndpoints are the endpoints list of the run's chat models: those of the kind's
// endpoints a chat model's server answers — all but embeddings, rerank and
// speech-to-text, which an embedding model, a reranker and a speech recognition model
// serve, each listing its own (docs/specs/GATEWAY.md, Model metadata → Endpoints per
// model).
func chatEndpoints(kind string) []string {
	return slices.DeleteFunc(slices.Clone(kindEndpoints[kind]), func(e string) bool {
		return e == epEmbeddings || e == epRerank || e == epTranscriptions
	})
}

// anthropicKind: a kind serving Claude through Anthropic's API, where the gateway
// holds requests to the standard price.
func anthropicKind(kind string) bool { return kind == kindAnthropic || kind == kindAzureAnthropic }
