package provider

import (
	"context"
	"net/http"

	"kaiak/internal/config"
)

// anthropic is the module for anthropic backends: Anthropic's API, which serves
// Messages only and bills fast mode, US-only inference, 1-hour cache writes and
// Priority Tier capacity above the standard rates the price table holds.
type anthropic struct {
	backend    *config.Backend
	client     *http.Client
	credential string
}

// anthropicEndpoints: Messages and its token counting.
var anthropicEndpoints = []Endpoint{Messages, MessagesCountTokens}

func newAnthropic(b *config.Backend, client *http.Client, credential string) backendModule {
	return &anthropic{backend: b, client: client, credential: credential}
}

// anthropicVersion is the anthropic-version header both Anthropic types send: the
// API's one version (docs/specs/GATEWAY.md, Base URLs).
const anthropicVersion = "2023-06-01"

// url joins the base URL, which already ends in the API version path
// (docs/specs/GATEWAY.md, Base URLs), and the endpoint path.
func (m *anthropic) url(path string) string { return m.backend.BaseURL + "/" + path }

// header carries the credential as Anthropic's x-api-key header, and the API
// version. The schema requires api_key_env; a credential the registry withholds
// (Registry.credential) sends no key header.
func (m *anthropic) header() http.Header {
	h := make(http.Header)
	if m.credential != "" {
		h.Set("X-Api-Key", m.credential)
	}
	h.Set("Anthropic-Version", anthropicVersion)
	return h
}

// Send implements Provider. A Messages request asking for a price option is refused
// before sending; every other one runs on the standard tier: service_tier
// "standard_only" replaces the client's, since the API's default ("auto") draws on
// Priority Tier capacity where the organization has a commitment, billed outside the
// price table (docs/specs/GATEWAY.md, Providers → Standard price on Anthropic types).
// A missing model is a not_found_error whose message begins "model:".
func (m *anthropic) Send(ctx context.Context, req *Request) (Response, error) {
	var edits []memberEdit
	if req.Endpoint == Messages {
		if err := refusePriceOptions(req.Body); err != nil {
			return nil, err
		}
		edits = append(edits, setValue("service_tier", []byte(`"standard_only"`)))
	}
	body, stripUsage, err := passthroughBody(req, edits...)
	if err != nil {
		return nil, editError(err)
	}
	return sendWire(ctx, req, wireCall{
		backend: m.backend, client: m.client, url: m.url(req.Endpoint.path()), header: m.header(),
		body: body, stripUsage: stripUsage, missingModel: anthropicModelMissing, unknownPath: m.unknownPath,
		core: req.Endpoint == Messages,
	})
}

// unknownPath: Anthropic's API answers a route it does not have with a
// not_found_error whose message is "Not Found" (from the documentation, not verified
// live).
func (m *anthropic) unknownPath(answer []byte) bool {
	e, ok := readErrorAnswer(answer)
	return ok && e.Type == "not_found_error" && e.Message == "Not Found"
}

// probe reads the models list, which names what requests carry. The list is paged,
// 20 by default; one page of 1000 holds every model.
func (m *anthropic) probe(ctx context.Context) (func(string) bool, error) {
	body, err := fetchModelsList(ctx, m.backend, m.client, m.url("models?limit=1000"), m.header(), versionPathHint)
	if err != nil {
		return nil, err
	}
	return listedModels(m.backend, body)
}
