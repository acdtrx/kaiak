package provider

import (
	"context"
	"net/http"
	"strings"

	"kaiak/internal/config"
)

// vLLMOmni is the module for vllm-omni backends: vLLM-Omni (vllm serve <model>
// --omni), vLLM's server with speech and image generation, one model per process. No
// tier is billed there, so the client's service_tier passes untouched and none is
// added.
type vLLMOmni struct {
	backend    *config.Backend
	client     *http.Client
	credential string
}

// vLLMOmniEndpoints: the media endpoints alone — speech, the speech batch and image
// generation, each listed here once the client API serves it. vLLM-Omni serves
// vLLM's routes too, but its chat answers carry media the gateway would count only as
// tokens (images on a diffusion model, speech on an Omni model with a talker stage),
// so chat, completions, embeddings, Messages, Responses and rerank are not served: a
// request for them to a model whose deployments are all vllm-omni is
// endpoint_not_served (docs/specs/GATEWAY.md, Providers → Endpoint support).
var vLLMOmniEndpoints = []Endpoint{AudioSpeech}

// speechHeaders are the speech answer's headers relayed to the client: the word
// timing a request asks for with word_timestamps, which vLLM-Omni sends only there
// (docs/specs/GATEWAY.md, Providers: headers). They hold the input's words: never
// logged.
var speechHeaders = []string{"X-Word-Timestamps", "X-Word-Timestamps-Omitted"}

func newVLLMOmni(b *config.Backend, client *http.Client, credential string) backendModule {
	return &vLLMOmni{backend: b, client: client, credential: credential}
}

// url joins the base URL, which already ends in the API version path
// (docs/specs/GATEWAY.md, Base URLs), and the endpoint path.
func (m *vLLMOmni) url(path string) string { return m.backend.BaseURL + "/" + path }

// header carries the credential (vLLM-Omni's --api-key) as a bearer token; a backend
// without one gets none.
func (m *vLLMOmni) header() http.Header {
	return bearer(m.credential)
}

// Send implements Provider. Every endpoint vllm-omni serves is a core endpoint:
// vLLM-Omni registers its speech, speech batch and image routes on every server,
// whatever model it loaded, so its answer to a path it does not have can only mean a
// wrong base_url (docs/specs/GATEWAY.md, Providers → An endpoint missing from a
// server). Speech and the speech batch run vLLM's model check, whose 404 names the
// model ("The model `…` does not exist."). A speech answer relays its word timing
// (speechHeaders).
func (m *vLLMOmni) Send(ctx context.Context, req *Request) (Response, error) {
	call := wireCall{
		backend: m.backend, client: m.client, url: m.url(req.Endpoint.Path()), header: m.header(),
		missingModel: missingModelNamedOrCoded("model_not_found"), unknownPath: m.unknownPath, core: true,
	}
	if req.Endpoint == AudioSpeech {
		call.relayed = speechHeaders
	}
	return sendWire(ctx, req, call)
}

// unknownPath: vLLM-Omni is vLLM's FastAPI app, which answers a path it has no route
// for with the framework's {"detail": "Not Found"}.
func (m *vLLMOmni) unknownPath(answer []byte) bool {
	return fastAPIDetail(answer, "Not Found")
}

// imageModelMismatch is the reading of a 400 on image generation: the route compares
// the request's model with the server's first served name and answers "Model
// mismatch: request specifies '<model>' but server is running '<name>'." — the
// deployment's model missing there when the name it quotes first is the deployment's
// backend-side model, which the gateway wrote into the request; no other request text
// reaches that message (docs/specs/GATEWAY.md, Providers → Wrong model on a host).
func (m *vLLMOmni) imageModelMismatch(answer []byte, model string) bool {
	e, ok := readErrorAnswer(answer)
	return ok && model != "" && strings.HasPrefix(e.Message, "Model mismatch: request specifies '"+model+"'")
}

// noDiffusionStage is the reading of a 503 on image generation: a model with no
// diffusion stage — a speech model — answers "No diffusion stage found in multi-stage
// pipeline.", the endpoint missing from its server, not the server failing
// (docs/specs/GATEWAY.md, Providers → An endpoint missing from a server).
func (m *vLLMOmni) noDiffusionStage(answer []byte) bool {
	e, ok := readErrorAnswer(answer)
	return ok && e.Message == "No diffusion stage found in multi-stage pipeline."
}

// probe reads the models list, which names what requests carry: vLLM's list for a
// model with a text stage, a list of the served names for a pure-diffusion one — both
// OpenAI's shape.
func (m *vLLMOmni) probe(ctx context.Context) (func(string) bool, error) {
	body, err := fetchModelsList(ctx, m.backend, m.client, m.url("models"), m.header(), versionPathHint)
	if err != nil {
		return nil, err
	}
	return listedModels(m.backend, body)
}
