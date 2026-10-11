package main

// The "not found" checks: a model misconfigured on purpose, so that its requests reach
// a real server on a path the server does not have, and the gateway's one reading of
// that answer (docs/specs/GATEWAY.md, Providers → A path the server does not have):
// 502 upstream_path_missing at once, in one attempt — the model's other deployments
// are identical instances, so it is not retried — the circuit left as it was, no
// usage. Every model lists its endpoints, so a client's request for an endpoint the
// model's server lacks stops at the gateway (endpoint_not_served); only an operator's
// error gets here, and these checks are the one place the kit makes one.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
)

// pathMissingCase is how a run's kind gets its server's answer to a path it does not
// have.
type pathMissingCase int

const (
	// noPathMissing: the cloud kinds — not checked.
	noPathMissing pathMissingCase = iota
	// pathMissingByList: on vllm, a chat model whose list names an endpoint vLLM
	// creates no route for on a model that generates (pathMissingListEndpoint) — the
	// list's error.
	pathMissingByList
	// pathMissingByBaseURL: on llama-server, a backend whose base_url has a wrong path
	// — llama-server registers every route whatever its model or flags, so only a
	// wrong path gets that answer.
	pathMissingByBaseURL
)

// pathMissingListEndpoint is the endpoint the vllm case's list names: vLLM serves
// embeddings only for an embedding model (docs/specs/GATEWAY.md, Model metadata →
// Endpoints per model).
const pathMissingListEndpoint = epEmbeddings

// wrongPath is what a wrong base_url carries after the right one: a path no server
// has, so every request through it, and the models list, answer the server's 404.
const wrongPath = "/wrong-path"

func wrongBaseURL(baseURL string) string { return baseURL + wrongPath }

// pathMissing is the run's case: by list on vllm — unless the -base-url server serves
// -embeddings-model, and so has an embeddings route — by base_url on llama-server,
// none on the cloud kinds.
func (o options) pathMissing() pathMissingCase {
	switch {
	case o.kind == kindVLLM && (o.embeddingsModel == "" || o.embeddingsServer()):
		return pathMissingByList
	case o.kind == kindLlamaServer:
		return pathMissingByBaseURL
	}
	return noPathMissing
}

// omniServer is a vLLM-Omni server given to the run: its URL, its key's variable, its
// model and the endpoint the omni-path-missing check sends it.
type omniServer struct{ baseURL, apiKeyEnv, model, endpoint string }

// omniWrongPathServer is the vLLM-Omni server whose model the omni-path-missing check
// deploys again behind a wrong base_url — vLLM-Omni registers its media routes
// whatever model it loaded, so there too only a wrong path gets the answer: the
// speech server, else the image server, else the edit server; ok is false with none.
func (o options) omniWrongPathServer() (server omniServer, ok bool) {
	switch {
	case o.speechServer():
		return omniServer{o.speechBaseURL, o.speechAPIKeyEnv, o.speechModel, epSpeech}, true
	case o.imageServer():
		return omniServer{o.imageBaseURL, o.imageAPIKeyEnv, o.imageModel, epImages}, true
	case o.editServer():
		return omniServer{o.editBaseURL, o.editAPIKeyEnv, o.editModel, epEdits}, true
	}
	return omniServer{}, false
}

// defaultFailureThreshold is the circuit's default: open after 5 failures in a row
// (docs/specs/GATEWAY.md, Routing and reliability → Reliability settings). A run with
// one backend keeps it; with two, the config sets failoverThreshold.
const defaultFailureThreshold = 5

// pathMissingMessage is the gateway's message for that answer, naming both causes
// (docs/specs/GATEWAY.md, the errors table).
const pathMissingMessage = "The model backend does not have this endpoint: its address is misconfigured, " +
	"or its server does not serve this API for the model."

// pathMissingProbe is one check's misconfigured model: where its deployments are and
// how a request to it is sent.
type pathMissingProbe struct {
	name, model string
	// backends hold the model's deployments, each serving deploymentModel.
	backends        []string
	deploymentModel string
	// wrongBaseURL: the backend's base_url is wrong, so the config-apply check warns
	// that it has no models list there.
	wrongBaseURL bool
	send         func(id string) (*response, error)
}

// pathMissingChecks run the kind's case and, with a vLLM-Omni server, the omni case.
func (r *run) pathMissingChecks() {
	switch r.o.pathMissing() {
	case pathMissingByList:
		backends := []string{backendFirst}
		if r.o.twoBackends() {
			backends = append(backends, backendSecond)
		}
		r.checkPathMissing(pathMissingProbe{name: "path-missing", model: modelPathMissing, backends: backends,
			deploymentModel: r.o.model, send: func(id string) (*response, error) {
				return r.post(r.key, "/v1/embeddings", id, map[string]any{"model": modelPathMissing, "input": []string{"The quick brown fox."}})
			}})
	case pathMissingByBaseURL:
		r.checkPathMissing(pathMissingProbe{name: "path-missing", model: modelPathMissing, backends: []string{backendWrongPath},
			deploymentModel: r.o.model, wrongBaseURL: true, send: func(id string) (*response, error) {
				return r.post(r.key, "/v1/chat/completions", id, r.chatBody(modelPathMissing, false, nil))
			}})
	default:
		if r.o.kind == kindVLLM {
			r.skip("path-missing", "-embeddings-model is served by -base-url, so that server has an embeddings route: "+
				"give it -embeddings-base-url")
		}
	}
	omni, ok := r.o.omniWrongPathServer()
	if !ok {
		return
	}
	r.checkPathMissing(pathMissingProbe{name: "omni-path-missing", model: modelOmniPathMissing,
		backends: []string{backendOmniWrongPath}, deploymentModel: omni.model, wrongBaseURL: true,
		send: func(id string) (*response, error) {
			switch omni.endpoint {
			case epSpeech:
				return r.post(r.key, "/v1/audio/speech", id, speechBody(modelOmniPathMissing, nil))
			case epImages:
				return r.post(r.key, "/v1/images/generations", id, imageBody(modelOmniPathMissing, nil))
			}
			return r.postForm(r.key, "/v1/images/edits", id, editForm(modelOmniPathMissing, nil))
		}})
}

// checkPathMissing sends the model one request more than its deployments' failures
// could take before their circuits opened, were the answer a failure: each is
// answered 502 upstream_path_missing with the gateway's message, in one attempt, on
// one of the model's deployments, its record of zero usage, not estimated (the
// request was routed, so it has one); then no circuit of the model's
// deployments opened (no log line, each closed in /metrics), the attempts are counted
// path_missing, and — for a wrong base_url — the config-apply check warned that the
// backend has no models list there, the operator's sign of it.
func (r *run) checkPathMissing(p pathMissingProbe) {
	threshold := defaultFailureThreshold
	if r.o.twoBackends() {
		threshold = failoverThreshold
	}
	n := threshold*len(p.backends) + 1
	for i := range n {
		id := fmt.Sprintf("live-%s-%d", p.name, i)
		resp, err := p.send(id)
		if err != nil {
			r.fail(p.name, "%v", err)
			return
		}
		if problem := openAIError(resp, http.StatusBadGateway, "upstream_path_missing"); problem != "" {
			r.fail(p.name, "request %d to %s: %s%s", i+1, p.model, problem, r.upstreamHint(id))
			return
		}
		var e struct {
			Error struct{ Message string }
		}
		_ = json.Unmarshal(resp.body, &e)
		if e.Error.Message != pathMissingMessage {
			r.fail(p.name, "message %q, want %q", e.Error.Message, pathMissingMessage)
			return
		}
		line, err := r.gw.logs.wait(r.ctx, msg("request", "kaiak.request.id", id))
		switch {
		case err != nil:
			r.fail(p.name, "no log line for request %s: %v", id, err)
			return
		case attempts(line) != 1:
			r.fail(p.name, "request %d took %d attempts (kaiak.tried %v): the answer must not be retried", i+1, attempts(line),
				line["kaiak.tried"])
			return
		case !slices.Contains(p.backends, fmt.Sprint(line["kaiak.backend.id"])):
			r.fail(p.name, "request %d served by backend %v, want one of %v", i+1, line["kaiak.backend.id"], p.backends)
			return
		case line["kaiak.usage.estimated"] != false || line["gen_ai.usage.input_tokens"] != 0.0 ||
			line["gen_ai.usage.output_tokens"] != 0.0:
			r.fail(p.name, "request %d's record: in %v, out %v, estimated %v; want zero usage, not estimated: nothing was processed",
				i+1, line["gen_ai.usage.input_tokens"], line["gen_ai.usage.output_tokens"], line["kaiak.usage.estimated"])
			return
		}
	}

	text, ok := r.metricsText(p.name)
	if !ok {
		return
	}
	deployment := `kaiak_deployment_model="` + p.deploymentModel + `"`
	var counted float64
	for _, b := range p.backends {
		backend := `kaiak_backend_id="` + b + `"`
		if _, opened := r.gw.logs.find(msg("circuit opened", "kaiak.backend.id", b, "kaiak.deployment.model", p.deploymentModel)); opened {
			r.fail(p.name, "the circuit of %s on %s opened: the answer must leave it as it was", p.deploymentModel, b)
			return
		}
		if closed := sumSeries(text, "kaiak_circuit_state{", backend, deployment, `kaiak_circuit_state="closed"`); closed != 1 {
			r.fail(p.name, "kaiak_circuit_state closed for %s on %s = %v, want 1", p.deploymentModel, b, closed)
			return
		}
		counted += sumSeries(text, "kaiak_upstream_attempts_total{", backend, deployment, `kaiak_attempt_outcome="path_missing"`)
	}
	if counted != float64(n) {
		r.fail(p.name, `kaiak_upstream_attempts_total{kaiak_attempt_outcome="path_missing"} = %v for %s, want %d`, counted, p.model, n)
		return
	}
	warned := ""
	if p.wrongBaseURL {
		line, err := r.gw.logs.wait(r.ctx, msg("the backend has no models list at its base_url", "kaiak.backend.id", p.backends[0]))
		if err != nil {
			r.fail(p.name, "no config-apply warning that %s has no models list at its base_url: %v", p.backends[0], err)
			return
		}
		warned = fmt.Sprintf("; warned at config apply: %s has no models list at %v", p.backends[0], line["kaiak.backend.base_url"])
	}
	r.pass(p.name, fmt.Sprintf("%d requests to %s → 502 upstream_path_missing, one attempt each, zero usage; circuit of %s closed "+
		"(threshold %d), %d path_missing attempts%s", n, p.model, strings.Join(p.backends, ", "), threshold, n, warned))
}
