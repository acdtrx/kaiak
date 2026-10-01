package provider

import (
	"context"
	"net/http"

	"kaiak/internal/config"
)

// llamaServer is the module for llama-server backends: llama.cpp's server, through
// its OpenAI-compatible API. No tier is billed there, so the client's service_tier
// passes untouched and none is added.
type llamaServer struct {
	backend    *config.Backend
	client     *http.Client
	credential string
}

// url joins the base URL, which already ends in the API version path
// (docs/specs/GATEWAY.md, Base URLs), and the endpoint path.
func (m *llamaServer) url(path string) string { return m.backend.BaseURL + "/" + path }

// header carries the credential (llama-server's --api-key) as a bearer token; a
// backend without one gets none.
func (m *llamaServer) header() http.Header {
	h := make(http.Header)
	if m.credential != "" {
		h.Set("Authorization", "Bearer "+m.credential)
	}
	return h
}

// Send implements Provider.
func (m *llamaServer) Send(ctx context.Context, req *Request) (Response, error) {
	body, stripUsage, err := passthroughBody(req)
	if err != nil {
		return nil, editError(err)
	}
	return sendWire(ctx, req, wireCall{
		backend: m.backend, client: m.client, url: m.url(req.Endpoint.path()), header: m.header(),
		body: body, stripUsage: stripUsage, missingModelCodes: []string{"model_not_found"}, unknownPath: m.unknownPath,
	})
}

// unknownPath: llama-server answers a path it does not have with
// {"error": {"message": "File Not Found", "type": "not_found_error", "code": 404}}.
// Its HTTP layer gives every 404 that body, so in router mode a model it does not
// have reads the same: the deployment's failure either way.
func (m *llamaServer) unknownPath(answer []byte) bool {
	e, ok := readErrorAnswer(answer)
	return ok && e.Type == "not_found_error" && e.Message == "File Not Found"
}

// probe reads the models list, which names what requests carry: the model file's
// path, or its --alias (docs/specs/GATEWAY.md, Backend model names).
func (m *llamaServer) probe(ctx context.Context) (func(string) bool, error) {
	body, err := fetchModelsList(ctx, m.backend, m.client, m.url("models"), m.header(), versionPathHint)
	if err != nil {
		return nil, err
	}
	return listedModels(m.backend, body)
}
