package provider

import (
	"context"
	"encoding/json"
	"net/http"

	"kaiak/internal/config"
)

// vLLM is the module for vllm backends: vLLM's OpenAI-compatible server. No tier is
// billed there, so the client's service_tier passes untouched and none is added.
type vLLM struct {
	backend    *config.Backend
	client     *http.Client
	credential string
}

// vLLMEndpoints: vLLM serves the OpenAI endpoints, Messages with its token counting,
// Responses — not responses/input_tokens (not in 0.30.0: a POST there lands on
// GET /v1/responses/{id} and answers 405) — and rerank.
var vLLMEndpoints = []Endpoint{ChatCompletions, Completions, Embeddings, Messages, MessagesCountTokens, Responses, Rerank}

func newVLLM(b *config.Backend, client *http.Client, credential string) backendModule {
	return &vLLM{backend: b, client: client, credential: credential}
}

// url joins the base URL, which already ends in the API version path
// (docs/specs/GATEWAY.md, Base URLs), and the endpoint path.
func (m *vLLM) url(path string) string { return m.backend.BaseURL + "/" + path }

// header carries the credential (vLLM's --api-key) as a bearer token; a backend
// without one gets none.
func (m *vLLM) header() http.Header {
	return bearer(m.credential)
}

// Send implements Provider. vllm has no core endpoints: vLLM creates its routes from
// the loaded model's tasks — no chat route on a reranker or embedding server, no
// embeddings or rerank route on a chat server — so its answer to a path it does not
// have names the model, not the URL, on every endpoint: the endpoint missing, not a
// wrong base_url (docs/specs/GATEWAY.md, Providers → An endpoint missing from a
// server).
func (m *vLLM) Send(ctx context.Context, req *Request) (Response, error) {
	return sendWire(ctx, req, wireCall{
		backend: m.backend, client: m.client, url: m.url(req.Endpoint.Path()), header: m.header(),
		missingModel: missingModelNamedOrCoded("model_not_found"), unknownPath: m.unknownPath,
	})
}

// unknownPath: vLLM's web framework (FastAPI) answers a path it has no route for
// with {"detail": "Not Found"}, and a POST to a route it has for another method only
// with {"detail": "Method Not Allowed"} (a 405); vLLM's own 404s carry the OpenAI or
// the Anthropic error shape.
func (m *vLLM) unknownPath(answer []byte) bool {
	var a struct {
		Detail *string `json:"detail"`
	}
	return json.Unmarshal(answer, &a) == nil && a.Detail != nil && (*a.Detail == "Not Found" || *a.Detail == "Method Not Allowed")
}

// probe reads the models list, which names what requests carry (vLLM's
// --served-model-name).
func (m *vLLM) probe(ctx context.Context) (func(string) bool, error) {
	body, err := fetchModelsList(ctx, m.backend, m.client, m.url("models"), m.header(), versionPathHint)
	if err != nil {
		return nil, err
	}
	return listedModels(m.backend, body)
}
