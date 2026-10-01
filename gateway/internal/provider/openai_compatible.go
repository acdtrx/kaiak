package provider

import (
	"context"
	"net/http"

	"kaiak/internal/config"
)

// openAICompatible is the module for openai-compatible backends: any server speaking
// the OpenAI format that has no type of its own (SGLang, …). It is the wire core with
// the OpenAI API's conventions and no server's own rules: the client's service_tier
// passes untouched and none is added.
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
	body, stripUsage, err := passthroughBody(req)
	if err != nil {
		return nil, editError(err)
	}
	return sendWire(ctx, req, wireCall{
		backend: m.backend, client: m.client, url: m.url(req.Endpoint.path()), header: m.header(),
		body: body, stripUsage: stripUsage, missingModelCodes: []string{"model_not_found"}, unknownPath: m.unknownPath,
	})
}

// unknownPath: the server is not known, but one speaking the OpenAI format answers
// in its error shape; a 404 in any other — a web framework's or a proxy's page, plain
// text, other JSON — came from below the API, at a path it does not serve.
func (m *openAICompatible) unknownPath(answer []byte) bool {
	_, ok := readErrorAnswer(answer)
	return !ok
}

// probe reads the models list, which names what requests carry.
func (m *openAICompatible) probe(ctx context.Context) (func(string) bool, error) {
	body, err := fetchModelsList(ctx, m.backend, m.client, m.url("models"), m.header(), versionPathHint)
	if err != nil {
		return nil, err
	}
	return listedModels(m.backend, body)
}
