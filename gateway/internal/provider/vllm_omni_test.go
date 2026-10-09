package provider

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"kaiak/internal/config"
	"kaiak/internal/fakebackend"
)

// vLLMOmniModelMissing is vLLM's model check's answer, which vLLM-Omni's speech and
// speech batch run (vLLM 0.30.0, _check_model).
const vLLMOmniModelMissing = `{"error":{"message":"The model ` + "`backend-model`" +
	` does not exist.","type":"NotFoundError","param":"model","code":404}}`

// The vllm-omni module sends to base_url plus the endpoint path with the credential as
// a bearer token (none without api_key_env), and its probe reads the models list
// there (docs/specs/GATEWAY.md, Providers → Base URLs, credentials, Model metadata);
// a models list answering 404 carries the version-path hint.
func TestVLLMOmniURLCredentialAndProbe(t *testing.T) {
	fb := fakebackend.New()
	defer fb.Close()
	fb.SetModels("backend-model")
	r := moduleRegistry()
	for _, withKey := range []bool{true, false} {
		b := &config.Backend{ID: "omni", Type: config.BackendVLLMOmni, BaseURL: fb.URL() + "/v1", ConnectTimeout: time.Second}
		want := ""
		if withKey {
			b.APIKeyEnv = "KEY"
			want = "Bearer " + testCredential
		}
		m, ok := r.module(b).(*vLLMOmni)
		if !ok {
			t.Fatalf("vllm-omni's module is %T", r.module(b))
		}
		for _, path := range []string{"audio/speech", "audio/speech/batch", "images/generations"} {
			if got := m.url(path); got != fb.URL()+"/v1/"+path {
				t.Errorf("url(%s) = %s, want %s/v1/%s", path, got, fb.URL(), path)
			}
		}
		if got := m.header(); got.Get("Authorization") != want || len(got) > 1 || (want == "" && len(got) != 0) {
			t.Errorf("with key %v: header %v, want Authorization %q alone", withKey, got, want)
		}

		before := len(fb.ModelsRequests())
		serves, err := r.Probe(context.Background(), b)
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		probes := fb.ModelsRequests()[before:]
		if len(probes) != 1 || probes[0].Method != http.MethodGet || probes[0].Path != "/v1/models" {
			t.Fatalf("probe requests %v, want one GET /v1/models", probes)
		}
		if got := probes[0].Header.Get("Authorization"); got != want {
			t.Errorf("with key %v: probe Authorization %q, want %q", withKey, got, want)
		}
		if !serves("backend-model") || serves("other-model") {
			t.Errorf("serves(listed) %v, serves(unlisted) %v, want true, false", serves("backend-model"), serves("other-model"))
		}
	}

	b := &config.Backend{ID: "omni", Type: config.BackendVLLMOmni, BaseURL: fb.URL(), ConnectTimeout: time.Second}
	fb.SetModelsStatus(http.StatusNotFound)
	_, err := r.Probe(context.Background(), b)
	var perr *pathMissingError
	if !errors.As(err, &perr) || perr.url != fb.URL()+"/models" || perr.hint != versionPathHint {
		t.Errorf("probe of a 404 models list = %#v, want a pathMissingError with the version-path hint", err)
	}
}

// What the vllm-omni module reads in a backend's answer (docs/specs/GATEWAY.md,
// Providers → Wrong model on a host, Wrong path to a host, An endpoint missing from a
// server). Every endpoint it serves is a core endpoint, and Send reads whatever
// endpoint it is sent as one:
//   - FastAPI's {"detail": "Not Found"} is upstream_path_missing, its error naming the
//     URL and not the backend's text; any other server's unknown-path answer is
//     relayed;
//   - vLLM's model check's 404 naming the deployment's model is
//     upstream_model_missing, in both of vLLM's shapes; one naming another model is
//     relayed;
//   - a 405 and a 501 are relayed: there is no endpoint beyond the core ones, and no
//     mode a 501 says the server was not started in.
func TestVLLMOmniAnswerReadings(t *testing.T) {
	// relayed: the answer is not the deployment's failure; the provider returns it.
	const relayed = Code("")
	type answer struct {
		name   string
		status int
		body   string
		want   Code
	}
	answers := []answer{
		{"route missing", http.StatusNotFound, `{"detail":"Not Found"}`, CodePathMissing},
		{"model missing", http.StatusNotFound, vLLMOmniModelMissing, CodeModelMissing},
		{"model missing, older vLLM", http.StatusNotFound,
			`{"object":"error","message":"The model ` + "`backend-model`" + ` does not exist.","type":"NotFoundError","code":404}`,
			CodeModelMissing},
		{"another model missing", http.StatusNotFound,
			`{"error":{"message":"The model ` + "`backend-model-2`" + ` does not exist.","type":"NotFoundError","param":"model","code":404}}`,
			relayed},
		{"caller's 404", http.StatusNotFound,
			`{"error":{"message":"No such voice: x","type":"NotFoundError","param":null,"code":404}}`, relayed},
		{"method not allowed", http.StatusMethodNotAllowed, `{"detail":"Method Not Allowed"}`, relayed},
		{"not supported", http.StatusNotImplemented, llamaServerNotSupported, relayed},
	}
	for name, body := range unknownPathAnswers {
		if name != "vllm" {
			answers = append(answers, answer{"unknown path from " + name, http.StatusNotFound, body, relayed})
		}
	}
	r := moduleRegistry()
	s := newWireServer(t)
	b := wireBackend(s, config.BackendVLLMOmni)
	for _, a := range answers {
		s.set(a.status, a.body)
		for e := range Endpoint(len(endpoints)) {
			t.Run(a.name+"/"+e.Path(), func(t *testing.T) {
				before := len(s.requests())
				resp, err := sendTo(r, b, e, `{"model":"pub"}`)
				sent := s.requests()[before:]
				if len(sent) != 1 || sent[0].path != "/v1/"+e.Path() ||
					sent[0].header.Get("Authorization") != "Bearer "+testCredential {
					t.Fatalf("sent %+v, want one request to /v1/%s with the bearer credential", sent, e.Path())
				}
				if a.want == relayed {
					if err != nil {
						t.Fatalf("%v, want the %d relayed", err, a.status)
					}
					if got := readAll(resp); resp.Status() != a.status || got != a.body {
						t.Errorf("relayed %d %s, want %d %s", resp.Status(), got, a.status, a.body)
					}
					return
				}
				perr, ok := errors.AsType[*Error](err)
				if !ok || perr.Code != a.want {
					t.Fatalf("%v, want %s", err, a.want)
				}
				if a.want == CodePathMissing && !strings.Contains(err.Error(), s.srv.URL+"/v1/"+e.Path()) {
					t.Errorf("error %q: want the URL named", err)
				}
				for _, text := range []string{"Not Found", "does not exist."} {
					if strings.Contains(err.Error(), text) {
						t.Errorf("error %q carries the backend's text", err)
					}
				}
			})
		}
	}
}

// Image generation's answers the module reads by their message (docs/specs/GATEWAY.md,
// Providers → Wrong model on a host, An endpoint missing from a server): its 400
// "Model mismatch" quoting the deployment's model first is the model missing — one
// quoting another model, or the deployment's only as the server's name, is not; its
// 503 "No diffusion stage found in multi-stage pipeline." is the endpoint missing, and
// no other message is.
func TestVLLMOmniImageReadings(t *testing.T) {
	m := &vLLMOmni{}
	mismatch := func(requested, running string) string {
		return `{"error":{"message":"Model mismatch: request specifies '` + requested + `' but server is running '` +
			running + `'.","type":"Bad Request","code":400}}`
	}
	for _, c := range []struct {
		answer, model string
		want          bool
	}{
		{mismatch("Qwen/Qwen-Image", "Qwen/Qwen3-TTS"), "Qwen/Qwen-Image", true},
		{mismatch("Qwen/Qwen-Image-2", "Qwen/Qwen3-TTS"), "Qwen/Qwen-Image", false},
		{mismatch("other", "Qwen/Qwen-Image"), "Qwen/Qwen-Image", false},
		{mismatch("<path>", "Qwen/Qwen3-TTS"), "/models/qwen/image.safetensors", false},
		{`{"error":{"message":"Qwen/Qwen-Image: n must be at most 10","type":"Bad Request","code":400}}`, "Qwen/Qwen-Image", false},
		{mismatch("", "x"), "", false},
	} {
		if got := m.imageModelMismatch([]byte(c.answer), c.model); got != c.want {
			t.Errorf("model %q, %s: model missing %v, want %v", c.model, c.answer, got, c.want)
		}
	}

	for answer, want := range map[string]bool{
		`{"error":{"message":"No diffusion stage found in multi-stage pipeline.","type":"Service Unavailable","code":503}}`: true,
		`{"error":{"message":"Engine is overloaded.","type":"Service Unavailable","code":503}}`:                             false,
		`{"error":{"message":"No diffusion stage found","type":"Service Unavailable","code":503}}`:                          false,
		`Service Unavailable`: false,
	} {
		if got := m.noDiffusionStage([]byte(answer)); got != want {
			t.Errorf("%s: endpoint missing %v, want %v", answer, got, want)
		}
	}
}
