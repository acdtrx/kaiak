package provider

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
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
					t.Errorf("%s serves %s: %v, the spec says %q", typ, e.path(), Serves(typ, e), cell)
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

// On an endpoint beyond the type's core ones, the server's answer to a path it does
// not have says its version predates the endpoint: upstream_endpoint_missing, naming
// the endpoint and not the backend's text (docs/specs/GATEWAY.md, Providers: an
// endpoint missing from a server). vLLM's 405 for a POST landing on a GET-only route
// reads the same there, and stays a caller's answer on a core endpoint.
func TestEndpointMissingFromAServer(t *testing.T) {
	cases := []struct {
		typ      config.BackendType
		endpoint Endpoint
		status   int
		answer   string
	}{
		{config.BackendVLLM, Messages, http.StatusNotFound, unknownPathAnswers["vllm"]},
		{config.BackendVLLM, Responses, http.StatusMethodNotAllowed, `{"detail":"Method Not Allowed"}`},
		{config.BackendLlamaServer, MessagesCountTokens, http.StatusNotFound, unknownPathAnswers["llama-server"]},
		{config.BackendOpenAI, Responses, http.StatusNotFound,
			`{"error":{"message":"Invalid URL (POST /v1/responses)","type":"invalid_request_error","param":null,"code":null}}`},
		{config.BackendAzureOpenAI, Responses, http.StatusNotFound, unknownPathAnswers["azure-openai"]},
	}
	r := moduleRegistry()
	for _, c := range cases {
		t.Run(string(c.typ)+"/"+c.endpoint.path(), func(t *testing.T) {
			s := newWireServer(t)
			s.set(c.status, c.answer)
			_, err := sendTo(r, wireBackend(s, c.typ), c.endpoint, `{"model":"pub"}`)
			perr, ok := errors.AsType[*Error](err)
			if !ok || perr.Code != CodeEndpointMissing {
				t.Fatalf("%v, want upstream_endpoint_missing", err)
			}
			if !strings.Contains(err.Error(), c.endpoint.path()) || strings.Contains(err.Error(), "Not Found") ||
				strings.Contains(err.Error(), "Invalid URL") {
				t.Errorf("error %q: want the endpoint named, not the backend's text", err)
			}
		})
	}

	// On a core endpoint a 405 is the caller's: relayed.
	s := newWireServer(t)
	s.set(http.StatusMethodNotAllowed, `{"detail":"Method Not Allowed"}`)
	resp, err := sendTo(r, wireBackend(s, config.BackendVLLM), ChatCompletions, `{"model":"pub"}`)
	if err != nil {
		t.Fatalf("405 on chat: %v, want it relayed", err)
	}
	if readAll(resp); resp.Status() != http.StatusMethodNotAllowed {
		t.Errorf("405 on chat relayed as %d", resp.Status())
	}
}

// Messages and Responses always report usage: a stream to them gets no
// stream_options edit, and nothing is hidden from the client.
func TestNoUsageEditOutsideTheOpenAIFormat(t *testing.T) {
	for _, e := range []Endpoint{Messages, Responses} {
		body, stripUsage, err := passthroughBody(&Request{Endpoint: e, Stream: true,
			Deployment: config.Deployment{Model: "m"}, Body: []byte(`{"model":"pub","stream":true}`)})
		if err != nil || stripUsage || string(body) != `{"model":"m","stream":true}` {
			t.Errorf("%s: %s, strip %v, %v", e.path(), body, stripUsage, err)
		}
	}
}
