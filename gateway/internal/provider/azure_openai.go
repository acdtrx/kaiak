package provider

import (
	"context"
	"net/http"

	"kaiak/internal/config"
)

// azureOpenAI is the module for azure-openai backends: Azure's OpenAI-compatible
// /openai/v1/ API, addressed by resource endpoint, authenticated by api-key, with
// deployment names where OpenAI has model names.
type azureOpenAI struct {
	backend    *config.Backend
	client     *http.Client
	credential string
}

// url joins the resource endpoint, the /openai/v1 prefix and the endpoint path
// (docs/specs/GATEWAY.md, Base URLs).
func (m *azureOpenAI) url(path string) string { return m.backend.BaseURL + "/openai/v1/" + path }

// header carries the credential as Azure's api-key header.
func (m *azureOpenAI) header() http.Header {
	h := make(http.Header)
	if m.credential != "" {
		h.Set("Api-Key", m.credential)
	}
	return h
}

// Send implements Provider. A missing deployment is DeploymentNotFound; the v1 API
// being OpenAI's shape, model_not_found is read as the same.
func (m *azureOpenAI) Send(ctx context.Context, req *Request) (Response, error) {
	body, stripUsage, err := passthroughBody(req, standardServiceTier(req.Endpoint))
	if err != nil {
		return nil, editError(err)
	}
	return sendWire(ctx, req, wireCall{
		backend: m.backend, client: m.client, url: m.url(req.Endpoint.path()), header: m.header(),
		body: body, stripUsage: stripUsage, missingModelCodes: []string{"DeploymentNotFound", "model_not_found"},
	})
}

// probe checks that the resource answers. Its models list names models, not the
// deployment names requests carry, so it cannot say whether a deployment exists:
// every name counts as served, and the trial request decides.
func (m *azureOpenAI) probe(ctx context.Context) (func(string) bool, error) {
	if _, err := fetchModelsList(ctx, m.backend, m.client, m.url("models"), m.header()); err != nil {
		return nil, err
	}
	return func(string) bool { return true }, nil
}
