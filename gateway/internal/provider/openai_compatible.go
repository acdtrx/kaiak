package provider

import (
	"context"
	"net/http"

	"kaiak/internal/config"
)

// openAICompatible is the module for openai-compatible backends: vLLM, llama-server,
// SGLang, OpenAI. It is the wire core with the OpenAI API's own conventions.
type openAICompatible struct {
	backend    *config.Backend
	client     *http.Client
	credential string
}

// url joins the base URL, which already ends in the API version path
// (docs/specs/GATEWAY.md, Base URLs), and the endpoint path.
func (m *openAICompatible) url(path string) string { return m.backend.BaseURL + "/" + path }

// header carries the credential as a bearer token; a backend without one gets none.
func (m *openAICompatible) header() http.Header {
	h := make(http.Header)
	if m.credential != "" {
		h.Set("Authorization", "Bearer "+m.credential)
	}
	return h
}

// Send implements Provider.
func (m *openAICompatible) Send(ctx context.Context, req *Request) (Response, error) {
	body, stripUsage, err := passthroughBody(req, standardServiceTier(req.Endpoint))
	if err != nil {
		return nil, editError(err)
	}
	return sendWire(ctx, req, wireCall{
		backend: m.backend, client: m.client, url: m.url(req.Endpoint.path()), header: m.header(),
		body: body, stripUsage: stripUsage, missingModelCodes: []string{"model_not_found"},
	})
}

// probe reads the models list, which names what requests carry.
func (m *openAICompatible) probe(ctx context.Context) (func(string) bool, error) {
	body, err := fetchModelsList(ctx, m.backend, m.client, m.url("models"), m.header())
	if err != nil {
		return nil, err
	}
	return listedModels(m.backend, body)
}
