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

// azureOpenAIEndpoints: Azure's v1 API serves the OpenAI endpoints and Responses, but
// not responses/input_tokens (it answers 404 there).
var azureOpenAIEndpoints = []Endpoint{ChatCompletions, Completions, Embeddings, Responses}

func newAzureOpenAI(b *config.Backend, client *http.Client, credential string) backendModule {
	return &azureOpenAI{backend: b, client: client, credential: credential}
}

// url joins the resource endpoint, the /openai/v1 prefix and the endpoint path
// (docs/specs/GATEWAY.md, Base URLs).
func (m *azureOpenAI) url(path string) string { return m.backend.BaseURL + "/openai/v1/" + path }

// header carries the credential as Azure's api-key header.
func (m *azureOpenAI) header() http.Header {
	return apiKey(m.credential)
}

// Send implements Provider. A missing deployment is DeploymentNotFound; the v1 API
// being OpenAI's shape, model_not_found is read as the same.
func (m *azureOpenAI) Send(ctx context.Context, req *Request) (Response, error) {
	return sendWire(ctx, req, wireCall{
		backend: m.backend, client: m.client, url: m.url(req.Endpoint.path()), header: m.header(),
		edits: []memberEdit{standardServiceTier(req.Endpoint)}, missingModel: missingModelCoded("DeploymentNotFound", "model_not_found"), unknownPath: m.unknownPath, core: openAICore(req.Endpoint),
	})
}

// unknownPath: an Azure resource answers a path it does not have with
// {"error": {"code": "404", "message": "Resource not found"}}.
func (m *azureOpenAI) unknownPath(answer []byte) bool {
	e, ok := readErrorAnswer(answer)
	return ok && e.Message == "Resource not found"
}

// azurePathHint is the base_url hint for azure-openai: the module adds the API's
// path itself (docs/specs/GATEWAY.md, Base URLs).
const azurePathHint = "base_url should be the resource endpoint with no path, e.g. https://<resource>.openai.azure.com: the gateway adds /openai/v1"

// probe checks that the resource answers. Its models list names models, not the
// deployment names requests carry, so it cannot tell whether a deployment exists
// (serves is nil): the trial request decides.
func (m *azureOpenAI) probe(ctx context.Context) (func(string) bool, error) {
	_, err := fetchModelsList(ctx, m.backend, m.client, m.url("models"), m.header(), azurePathHint)
	return nil, err
}
