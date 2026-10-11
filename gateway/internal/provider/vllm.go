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

func newVLLM(b *config.Backend, client *http.Client, credential string) backendModule {
	return &vLLM{backend: b, client: client, credential: credential}
}

// url joins the base URL, which already ends in the API version path
// (docs/specs/GATEWAY.md, Base URLs), and the endpoint path.
func (m *vLLM) url(path string) string { return m.backend.BaseURL + "/" + path }

// header carries the credential (vLLM's --api-key) as a bearer token; a backend
// without one gets none.
func (m *vLLM) header() http.Header {
	return bearer(m.credential)
}

// Send implements Provider. vLLM creates its routes from the loaded model's tasks —
// no chat route on a reranker or embedding server, no embeddings or rerank route on a
// chat server — so its answer to a path it does not have is as often a list naming an
// endpoint the model's server lacks as a wrong base_url: one reading either way
// (docs/specs/GATEWAY.md, Providers → A path the server does not have).
func (m *vLLM) Send(ctx context.Context, req *Request) (Response, error) {
	return sendWire(ctx, req, wireCall{
		backend: m.backend, client: m.client, url: m.url(req.Endpoint.Path()), header: m.header(),
		missingModel: missingModelNamedOrCoded("model_not_found"), unknownPath: m.unknownPath,
		readings: []answerReading{{status: http.StatusMethodNotAllowed, code: CodePathMissing, read: m.methodNotAllowed}},
	})
}

// unknownPath: vLLM's web framework (FastAPI) answers a path it has no route for
// with {"detail": "Not Found"}; vLLM's own 404s carry the OpenAI or the Anthropic
// error shape.
func (m *vLLM) unknownPath(answer []byte) bool {
	return fastAPIDetail(answer, "Not Found")
}

// methodNotAllowed: FastAPI answers a POST to a route vLLM has for another method only
// with a 405, {"detail": "Method Not Allowed"} — a path the server does not have for
// the request (0.30.0's POST /v1/responses/input_tokens lands on
// GET /v1/responses/{id}).
func (m *vLLM) methodNotAllowed(answer []byte, _ string) bool {
	return fastAPIDetail(answer, "Method Not Allowed")
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
