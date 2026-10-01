package provider

import (
	"context"
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
		body: body, stripUsage: stripUsage, missingModelCodes: []string{"model_not_found"},
	})
}

// probe reads the models list, which names what requests carry (vLLM's
// --served-model-name).
func (m *vLLM) probe(ctx context.Context) (func(string) bool, error) {
	body, err := fetchModelsList(ctx, m.backend, m.client, m.url("models"), m.header())
	if err != nil {
		return nil, err
	}
	return listedModels(m.backend, body)
}
