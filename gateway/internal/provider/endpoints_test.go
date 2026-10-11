package provider

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"kaiak/internal/config"
)

// llamaServerNotSupported is llama-server's answer to rerank when it was not started
// with --reranking (tools/server/server-common.cpp, format_error_response; b11513): a
// 501, a backend 5xx as any server error.
const llamaServerNotSupported = `{"error":{"code":501,"message":"This server does not support reranking. ` +
	"Start it with `--reranking`" + `","type":"not_supported_error"}}`

// One "not found" reading on every type and every endpoint it serves
// (docs/specs/GATEWAY.md, Providers → A path the server does not have): each module's
// signature of a path its server does not have — vLLM's 404 and its 405, where a POST
// lands on a route for another method — is upstream_path_missing, whose error names
// the URL and never the backend's text. Every other answer is relayed for the pipeline
// to answer: a 405 on the other types, llama-server's 501 for an endpoint it was not
// started for and vLLM-Omni's 503 for an image request to a model with no diffusion
// stage — plain 5xx, as any server error.
func TestPathMissingAnswersByModule(t *testing.T) {
	// relayed: the answer is not read; the provider returns it.
	const relayed = Code("")
	const methodNotAllowed = `{"detail":"Method Not Allowed"}`
	const noDiffusion = `{"error":{"message":"No diffusion stage found in multi-stage pipeline.","type":"Service Unavailable",` +
		`"param":null,"code":503}}`
	cases := []struct {
		name   string
		typ    config.BackendType
		status int
		answer string
		want   Code
	}{
		{"route missing", config.BackendVLLM, http.StatusNotFound, unknownPathAnswers["vllm"], CodePathMissing},
		{"method not allowed", config.BackendVLLM, http.StatusMethodNotAllowed, methodNotAllowed, CodePathMissing},
		{"not supported", config.BackendVLLM, http.StatusNotImplemented, llamaServerNotSupported, relayed},
		{"route missing", config.BackendVLLMOmni, http.StatusNotFound, unknownPathAnswers["vllm"], CodePathMissing},
		{"method not allowed", config.BackendVLLMOmni, http.StatusMethodNotAllowed, methodNotAllowed, relayed},
		{"no diffusion stage", config.BackendVLLMOmni, http.StatusServiceUnavailable, noDiffusion, relayed},
		{"file not found", config.BackendLlamaServer, http.StatusNotFound, unknownPathAnswers["llama-server"], CodePathMissing},
		{"not supported", config.BackendLlamaServer, http.StatusNotImplemented, llamaServerNotSupported, relayed},
		{"method not allowed", config.BackendLlamaServer, http.StatusMethodNotAllowed, methodNotAllowed, relayed},
		{"page not found", config.BackendOpenAICompatible, http.StatusNotFound, unknownPathAnswers["go"], CodePathMissing},
		{"not supported", config.BackendOpenAICompatible, http.StatusNotImplemented, llamaServerNotSupported, relayed},
		{"method not allowed", config.BackendOpenAICompatible, http.StatusMethodNotAllowed, methodNotAllowed, relayed},
		{"invalid URL", config.BackendOpenAI, http.StatusNotFound,
			`{"error":{"message":"Invalid URL (POST /v1/responses)","type":"invalid_request_error","param":null,"code":null}}`,
			CodePathMissing},
		{"resource not found", config.BackendAzureOpenAI, http.StatusNotFound, unknownPathAnswers["azure-openai"], CodePathMissing},
		{"not found", config.BackendAnthropic, http.StatusNotFound,
			`{"type":"error","error":{"type":"not_found_error","message":"Not Found"}}`, CodePathMissing},
		{"resource not found", config.BackendAzureAnthropic, http.StatusNotFound, unknownPathAnswers["azure-openai"],
			CodePathMissing},
	}
	r := moduleRegistry()
	for _, c := range cases {
		s := newWireServer(t)
		s.set(c.status, c.answer)
		for e := range Endpoint(len(endpoints)) {
			if !c.typ.Serves(e.Name()) {
				continue
			}
			t.Run(string(c.typ)+"/"+c.name+"/"+e.Path(), func(t *testing.T) {
				resp, err := sendTo(r, wireBackend(s, c.typ), e, `{"model":"pub"}`)
				if c.want == relayed {
					if err != nil {
						t.Fatalf("%v, want the %d relayed", err, c.status)
					}
					if got := readAll(resp); resp.Status() != c.status || got != c.answer {
						t.Errorf("relayed %d %s, want %d %s", resp.Status(), got, c.status, c.answer)
					}
					return
				}
				perr, ok := errors.AsType[*Error](err)
				if !ok || perr.Code != c.want {
					t.Fatalf("%v, want %s", err, c.want)
				}
				if !strings.Contains(err.Error(), s.srv.URL) || !strings.Contains(err.Error(), e.Path()) {
					t.Errorf("error %q: want the URL named", err)
				}
				for _, text := range []string{"Not Found", "Not Allowed", "Invalid URL", "not found", "page"} {
					if strings.Contains(err.Error(), text) {
						t.Errorf("error %q carries the backend's text", err)
					}
				}
			})
		}
	}
}

// Messages and Responses always report usage: a stream to them gets no
// stream_options edit, and nothing is hidden from the client. A Responses request gets
// store: false and nothing else. Rerank has no stream: its stream member passes
// untouched, and the model is its one edit. Speech's stream is its mode's, passed
// untouched, and speech reports usage unasked — the whole clip in its headers, the SSE
// stream in speech.audio.done: the model is its one edit beside its output limit. The
// speech batch has no stream, and reports each item's usage unasked. Image generation
// has no stream and reports no usage: the model is its one edit.
func TestNoUsageEditOutsideTheOpenAIFormat(t *testing.T) {
	for e, want := range map[Endpoint]string{
		Messages:          `{"model":"m","stream":true}`,
		Responses:         `{"model":"m","stream":true,"store":false}`,
		Rerank:            `{"model":"m","stream":true}`,
		AudioSpeech:       `{"model":"m","stream":true}`,
		AudioSpeechBatch:  `{"model":"m","stream":true}`,
		ImagesGenerations: `{"model":"m","stream":true}`,
	} {
		body, stripUsage, err := passthroughBody(&Request{Endpoint: e, Stream: true,
			Deployment: config.Deployment{Model: "m"}, Body: []byte(`{"model":"pub","stream":true}`)})
		if err != nil || stripUsage || string(body) != want {
			t.Errorf("%s: %s, strip %v, %v", e.Path(), body, stripUsage, err)
		}
	}
}
