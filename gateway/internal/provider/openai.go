package provider

import (
	"context"
	"net/http"
	"strings"

	"kaiak/internal/config"
)

// openAI is the module for openai backends: OpenAI's API, which bills by service
// tier, so every request runs on the standard one.
type openAI struct {
	backend    *config.Backend
	client     *http.Client
	credential string
}

// openAIEndpoints: OpenAI's API serves the OpenAI endpoints and Responses with its token
// counting.
var openAIEndpoints = []Endpoint{ChatCompletions, Completions, Embeddings, Responses, ResponsesInputTokens}

func newOpenAI(b *config.Backend, client *http.Client, credential string) backendModule {
	return &openAI{backend: b, client: client, credential: credential}
}

// url joins the base URL, which already ends in the API version path
// (docs/specs/GATEWAY.md, Base URLs), and the endpoint path.
func (m *openAI) url(path string) string { return m.backend.BaseURL + "/" + path }

// header carries the credential as a bearer token. The schema requires api_key_env;
// a credential the registry withholds (Registry.credential) sends no header.
func (m *openAI) header() http.Header {
	h := make(http.Header)
	if m.credential != "" {
		h.Set("Authorization", "Bearer "+m.credential)
	}
	return h
}

// Send implements Provider.
func (m *openAI) Send(ctx context.Context, req *Request) (Response, error) {
	body, stripUsage, err := passthroughBody(req, standardServiceTier(req.Endpoint))
	if err != nil {
		return nil, editError(err)
	}
	return sendWire(ctx, req, wireCall{
		backend: m.backend, client: m.client, url: m.url(req.Endpoint.path()), header: m.header(),
		body: body, stripUsage: stripUsage, missingModelCodes: []string{"model_not_found"}, unknownPath: m.unknownPath, core: openAICore(req.Endpoint),
	})
}

// unknownPath: OpenAI answers a path it does not have with an invalid_request_error
// whose message starts "Invalid URL" ("Invalid URL (POST /chat/completions)").
func (m *openAI) unknownPath(answer []byte) bool {
	e, ok := readErrorAnswer(answer)
	return ok && e.Type == "invalid_request_error" && strings.HasPrefix(e.Message, "Invalid URL")
}

// probe reads the models list, which names what requests carry.
func (m *openAI) probe(ctx context.Context) (func(string) bool, error) {
	body, err := fetchModelsList(ctx, m.backend, m.client, m.url("models"), m.header(), versionPathHint)
	if err != nil {
		return nil, err
	}
	return listedModels(m.backend, body)
}
