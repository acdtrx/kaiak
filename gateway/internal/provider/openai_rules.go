package provider

// Rules the OpenAI-format types share (openai.go, azure_openai.go, vllm.go,
// llama_server.go, openai_compatible.go).

// openAICore reports whether e is a core endpoint of the OpenAI-format types: one of
// OpenAI's three (docs/specs/GATEWAY.md, Providers: an endpoint missing from a
// server).
func openAICore(e Endpoint) bool { return e.Format() == FormatOpenAI }
