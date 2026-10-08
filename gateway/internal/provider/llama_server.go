package provider

import (
	"context"
	"net/http"

	"kaiak/internal/config"
)

// llamaServer is the module for llama-server backends: llama.cpp's server, through
// its OpenAI-compatible API. No tier is billed there, so the client's service_tier
// passes untouched and none is added.
type llamaServer struct {
	backend    *config.Backend
	client     *http.Client
	credential string
}

// llamaServerEndpoints: llama-server serves every endpoint (build b9917; rerank on
// b11513).
var llamaServerEndpoints = []Endpoint{ChatCompletions, Completions, Embeddings, Messages, MessagesCountTokens, Responses,
	ResponsesInputTokens, Rerank}

func newLlamaServer(b *config.Backend, client *http.Client, credential string) backendModule {
	return &llamaServer{backend: b, client: client, credential: credential}
}

// url joins the base URL, which already ends in the API version path
// (docs/specs/GATEWAY.md, Base URLs), and the endpoint path.
func (m *llamaServer) url(path string) string { return m.backend.BaseURL + "/" + path }

// header carries the credential (llama-server's --api-key) as a bearer token; a
// backend without one gets none.
func (m *llamaServer) header() http.Header {
	return bearer(m.credential)
}

// Send implements Provider.
func (m *llamaServer) Send(ctx context.Context, req *Request) (Response, error) {
	return sendWire(ctx, req, wireCall{
		backend: m.backend, client: m.client, url: m.url(req.Endpoint.Path()), header: m.header(),
		missingModel: missingModelNamedOrCoded("model_not_found"), unknownPath: m.unknownPath, core: openAICore(req.Endpoint),
		modeMissing: m.modeMissing(req.Endpoint),
	})
}

// unknownPath: llama-server answers a path it does not have with
// {"error": {"message": "File Not Found", "type": "not_found_error", "code": 404}}.
// Its HTTP layer gives every 404 that body, so in router mode a model it does not
// have reads the same: on the core endpoints, the deployment's failure either way.
func (m *llamaServer) unknownPath(answer []byte) bool {
	e, ok := readErrorAnswer(answer)
	return ok && e.Type == "not_found_error" && e.Message == "File Not Found"
}

// modeMissing is the reading of a 501 on endpoint e: llama-server answers embeddings
// when not started with --embeddings, and rerank when not started with --embeddings
// and pooling rank, with a 501 of type not_supported_error ("This server does not
// support reranking. Start it with --reranking") — the endpoint missing; the message
// is not read. Its other 501s are on routes the gateway does not serve, so on any
// other endpoint a 501 stays a backend 5xx (docs/specs/GATEWAY.md, Providers → An
// endpoint missing from a server).
func (m *llamaServer) modeMissing(e Endpoint) func(answer []byte) bool {
	if e != Embeddings && e != Rerank {
		return nil
	}
	return func(answer []byte) bool {
		a, ok := readErrorAnswer(answer)
		return ok && a.Type == "not_supported_error"
	}
}

// probe reads the models list, which names what requests carry: the model file's
// path, or its --alias (docs/specs/GATEWAY.md, Backend model names).
func (m *llamaServer) probe(ctx context.Context) (func(string) bool, error) {
	body, err := fetchModelsList(ctx, m.backend, m.client, m.url("models"), m.header(), versionPathHint)
	if err != nil {
		return nil, err
	}
	return listedModels(m.backend, body)
}
