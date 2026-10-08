package provider

// Rules the types whose core endpoints are OpenAI's three share (openai.go,
// azure_openai.go, llama_server.go, openai_compatible.go).

// openAICore reports whether e is a core endpoint of those types: one of OpenAI's
// three (docs/specs/GATEWAY.md, Providers: an endpoint missing from a server).
func openAICore(e Endpoint) bool { return e.Format() == FormatOpenAI }
