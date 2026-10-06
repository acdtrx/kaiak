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
// and Responses — not responses/input_tokens (not in 0.30.0: a POST there lands on
// GET /v1/responses/{id} and answers 405).
var vLLMEndpoints = []Endpoint{ChatCompletions, Completions, Embeddings, Messages, MessagesCountTokens, Responses}

func newVLLM(b *config.Backend, client *http.Client, credential string) backendModule {
	return &vLLM{backend: b, client: client, credential: credential}
}

// url joins the base URL, which already ends in the API version path
// (docs/specs/GATEWAY.md, Base URLs), and the endpoint path.
func (m *vLLM) url(path string) string { return m.backend.BaseURL + "/" + path }

// header carries the credential (vLLM's --api-key) as a bearer token; a backend
// without one gets none.
func (m *vLLM) header() http.Header {
	h := make(http.Header)
	if m.credential != "" {
		h.Set("Authorization", "Bearer "+m.credential)
	}
	return h
}

// Send implements Provider.
func (m *vLLM) Send(ctx context.Context, req *Request) (Response, error) {
	body, stripUsage, err := passthroughBody(req)
	if err != nil {
		return nil, editError(err)
	}
	return sendWire(ctx, req, wireCall{
		backend: m.backend, client: m.client, url: m.url(req.Endpoint.path()), header: m.header(),
		body: body, stripUsage: stripUsage, missingModel: missingModelNamedOrCoded("model_not_found"), unknownPath: m.unknownPath, core: openAICore(req.Endpoint),
	})
}

// unknownPath: vLLM's web framework (FastAPI) answers a path it has no route for
// with {"detail": "Not Found"}, and a POST to a route it has for another method only
// with {"detail": "Method Not Allowed"} (a 405, read beyond the core endpoints); vLLM's
// own 404s carry the OpenAI or the Anthropic error shape.
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
