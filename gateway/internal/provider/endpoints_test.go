package provider

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"kaiak/internal/config"
)

// endpointColumns names the endpoint support table's columns
// (docs/specs/GATEWAY.md, Providers → Endpoint support) by the endpoints they cover.
var endpointColumns = map[string][]Endpoint{
	"chat, completions, embeddings": {ChatCompletions, Completions, Embeddings},
	"messages":                      {Messages},
	"messages_count_tokens":         {MessagesCountTokens},
	"responses":                     {Responses},
	"responses_input_tokens":        {ResponsesInputTokens},
	"rerank":                        {Rerank},
}

// Each type serves exactly what the spec's endpoint support table says, and the
// table names every type.
func TestEndpointSupportFollowsTheSpec(t *testing.T) {
	doc, err := os.ReadFile(filepath.Join("..", "..", "..", "docs", "specs", "GATEWAY.md"))
	if err != nil {
		t.Fatal(err)
	}
	var header []string
	seen := map[config.BackendType]bool{}
	for line := range strings.Lines(string(doc)) {
		line = strings.TrimSpace(line)
		cells := strings.Split(strings.Trim(line, "|"), "|")
		for i := range cells {
			cells[i] = strings.TrimSpace(cells[i])
		}
		switch {
		case strings.HasPrefix(line, "| Type | chat, completions, embeddings |"):
			header = cells
			continue
		case header == nil:
			continue
		case !strings.HasPrefix(line, "|"):
			header = nil
			continue
		case strings.HasPrefix(cells[0], "---"):
			continue
		}
		typ := config.BackendType(strings.Trim(cells[0], "`"))
		seen[typ] = true
		for i, cell := range cells[1:] {
			for _, e := range endpointColumns[header[i+1]] {
				if want := strings.HasPrefix(cell, "yes"); Serves(typ, e) != want {
					t.Errorf("%s serves %s: %v, the spec says %q", typ, e.Path(), Serves(typ, e), cell)
				}
			}
		}
	}
	for typ := range kinds {
		if !seen[typ] {
			t.Errorf("the spec's endpoint support table has no row for %s", typ)
		}
	}
}

// llamaServerNotSupported is llama-server's answer to rerank when it was not started
// with --reranking (tools/server/server-common.cpp, format_error_response; b11513).
const llamaServerNotSupported = `{"error":{"code":501,"message":"This server does not support reranking. ` +
	"Start it with `--reranking`" + `","type":"not_supported_error"}}`

// Each module's answers saying its server does not serve an endpoint, on every
// endpoint its type serves (docs/specs/GATEWAY.md, Providers → Wrong path to a host,
// An endpoint missing from a server):
//   - vllm has no core endpoints — vLLM creates its routes from the loaded model's
//     tasks — so its route-missing answer, a 404 or a 405 where a POST lands on a
//     route for another method, is upstream_endpoint_missing on every endpoint;
//   - llama-server's File Not Found is upstream_path_missing on OpenAI's three, its
//     core endpoints, and upstream_endpoint_missing elsewhere; its 501
//     not_supported_error is upstream_endpoint_missing on embeddings and rerank, and
//     any other 501 — on another endpoint, of another type — stays the backend's 5xx;
//   - a cloud type's unknown-path answer is the same beyond its core endpoints;
//   - a 501 is the backend's 5xx on every other type, and a 405 is relayed on a core
//     endpoint.
//
// The error names the URL — on vllm a wrong base_url answers this way too — and never
// the backend's text.
func TestWrongEndpointAnswersByModule(t *testing.T) {
	openAIThree := []Endpoint{ChatCompletions, Completions, Embeddings}
	// relayed: the answer is not the deployment's failure; the provider returns it.
	const relayed = Code("")
	everywhere := func(c Code) func(Endpoint) Code { return func(Endpoint) Code { return c } }
	coreOr := func(core, beyond Code) func(Endpoint) Code {
		return func(e Endpoint) Code {
			if slices.Contains(openAIThree, e) {
				return core
			}
			return beyond
		}
	}
	on := func(c Code, these ...Endpoint) func(Endpoint) Code {
		return func(e Endpoint) Code {
			if slices.Contains(these, e) {
				return c
			}
			return relayed
		}
	}
	cases := []struct {
		name   string
		typ    config.BackendType
		status int
		answer string
		want   func(Endpoint) Code
	}{
		{"route missing", config.BackendVLLM, http.StatusNotFound, unknownPathAnswers["vllm"], everywhere(CodeEndpointMissing)},
		{"method not allowed", config.BackendVLLM, http.StatusMethodNotAllowed, `{"detail":"Method Not Allowed"}`,
			everywhere(CodeEndpointMissing)},
		{"not supported", config.BackendVLLM, http.StatusNotImplemented, llamaServerNotSupported, everywhere(relayed)},
		{"file not found", config.BackendLlamaServer, http.StatusNotFound, unknownPathAnswers["llama-server"],
			coreOr(CodePathMissing, CodeEndpointMissing)},
		{"not supported", config.BackendLlamaServer, http.StatusNotImplemented, llamaServerNotSupported,
			on(CodeEndpointMissing, Embeddings, Rerank)},
		{"501 server error", config.BackendLlamaServer, http.StatusNotImplemented,
			`{"error":{"code":501,"message":"This server does not support reranking.","type":"server_error"}}`, everywhere(relayed)},
		{"method not allowed", config.BackendLlamaServer, http.StatusMethodNotAllowed, `{"detail":"Method Not Allowed"}`,
			everywhere(relayed)},
		{"not supported", config.BackendOpenAICompatible, http.StatusNotImplemented, llamaServerNotSupported, everywhere(relayed)},
		{"method not allowed", config.BackendOpenAICompatible, http.StatusMethodNotAllowed, `{"detail":"Method Not Allowed"}`,
			everywhere(relayed)},
		{"invalid URL", config.BackendOpenAI, http.StatusNotFound,
			`{"error":{"message":"Invalid URL (POST /v1/responses)","type":"invalid_request_error","param":null,"code":null}}`,
			coreOr(CodePathMissing, CodeEndpointMissing)},
		{"resource not found", config.BackendAzureOpenAI, http.StatusNotFound, unknownPathAnswers["azure-openai"],
			coreOr(CodePathMissing, CodeEndpointMissing)},
	}
	r := moduleRegistry()
	for _, c := range cases {
		s := newWireServer(t)
		s.set(c.status, c.answer)
		for e := range Endpoint(len(endpoints)) {
			if !Serves(c.typ, e) {
				continue
			}
			t.Run(string(c.typ)+"/"+c.name+"/"+e.Path(), func(t *testing.T) {
				resp, err := sendTo(r, wireBackend(s, c.typ), e, `{"model":"pub"}`)
				want := c.want(e)
				if want == relayed {
					if err != nil {
						t.Fatalf("%v, want the %d relayed", err, c.status)
					}
					if got := readAll(resp); resp.Status() != c.status || got != c.answer {
						t.Errorf("relayed %d %s, want %d %s", resp.Status(), got, c.status, c.answer)
					}
					return
				}
				perr, ok := errors.AsType[*Error](err)
				if !ok || perr.Code != want {
					t.Fatalf("%v, want %s", err, want)
				}
				if !strings.Contains(err.Error(), s.srv.URL) || !strings.Contains(err.Error(), e.Path()) {
					t.Errorf("error %q: want the URL named", err)
				}
				for _, text := range []string{"Not Found", "Not Allowed", "Invalid URL", "not found", "not support"} {
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
// untouched, and the model is its one edit.
func TestNoUsageEditOutsideTheOpenAIFormat(t *testing.T) {
	for e, want := range map[Endpoint]string{
		Messages:  `{"model":"m","stream":true}`,
		Responses: `{"model":"m","stream":true,"store":false}`,
		Rerank:    `{"model":"m","stream":true}`,
	} {
		body, stripUsage, err := passthroughBody(&Request{Endpoint: e, Stream: true,
			Deployment: config.Deployment{Model: "m"}, Body: []byte(`{"model":"pub","stream":true}`)})
		if err != nil || stripUsage || string(body) != want {
			t.Errorf("%s: %s, strip %v, %v", e.Path(), body, stripUsage, err)
		}
	}
}
