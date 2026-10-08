package provider

import (
	"context"
	"net/http"

	"kaiak/internal/config"
)

// azureAnthropic is the module for azure-anthropic backends: Claude in Microsoft
// Foundry, Anthropic's Messages API on an Azure resource, addressed by resource
// endpoint, authenticated by api-key, with Foundry deployment names where Anthropic
// has model names. It has no models list and no Priority Tier.
type azureAnthropic struct {
	backend    *config.Backend
	client     *http.Client
	credential string
}

// azureAnthropicEndpoints: Messages and its token counting.
var azureAnthropicEndpoints = []Endpoint{Messages, MessagesCountTokens}

func newAzureAnthropic(b *config.Backend, client *http.Client, credential string) backendModule {
	return &azureAnthropic{backend: b, client: client, credential: credential}
}

// url joins the resource endpoint, the /anthropic/v1 prefix and the endpoint path
// (docs/specs/GATEWAY.md, Base URLs).
func (m *azureAnthropic) url(path string) string { return m.backend.BaseURL + "/anthropic/v1/" + path }

// header carries the credential as Azure's api-key header, and the API version.
func (m *azureAnthropic) header() http.Header {
	h := apiKey(m.credential)
	h.Set("Anthropic-Version", anthropicVersion)
	return h
}

// Send implements Provider. A Messages request asking for a price option is refused
// before sending; the client's service_tier passes untouched, as Foundry has no
// Priority Tier (docs/specs/GATEWAY.md, Providers → Standard price on Anthropic
// types). A missing deployment is Azure's DeploymentNotFound, or Anthropic's
// not_found_error whose message begins "model:".
func (m *azureAnthropic) Send(ctx context.Context, req *Request) (Response, error) {
	if req.Endpoint == Messages {
		if err := refusePriceOptions(req.Body); err != nil {
			return nil, err
		}
	}
	return sendWire(ctx, req, wireCall{
		backend: m.backend, client: m.client, url: m.url(req.Endpoint.path()), header: m.header(),
		missingModel: azureAnthropicModelMissing, unknownPath: m.unknownPath,
		core: req.Endpoint == Messages,
	})
}

// azureAnthropicModelMissing: Foundry answers a deployment it does not have in Azure's
// shape (DeploymentNotFound) or Anthropic's.
func azureAnthropicModelMissing(answer []byte, model string) bool {
	return missingModelCoded("DeploymentNotFound")(answer, model) || anthropicModelMissing(answer, model)
}

// unknownPath: an Azure resource answers a path it does not have with
// {"error": {"code": "404", "message": "Resource not found"}} (as for azure-openai; not
// verified live on Foundry).
func (m *azureAnthropic) unknownPath(answer []byte) bool {
	e, ok := readErrorAnswer(answer)
	return ok && e.Message == "Resource not found"
}

// probe sends nothing: Foundry offers no models list, so it cannot tell which models
// it serves (serves is nil) and an open circuit turns half-open after each probe
// interval, the half-open trial deciding (docs/specs/GATEWAY.md, Providers → Probe and
// model check for the new types).
func (m *azureAnthropic) probe(context.Context) (func(string) bool, error) {
	return nil, nil
}
