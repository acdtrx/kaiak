package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"kaiak/internal/config"
)

// recorded is one request a wireServer received.
type recorded struct {
	method, path, query string
	header              http.Header
	body                []byte
}

// wireServer answers every request with the status and body last set (as JSON), and
// records what it received.
type wireServer struct {
	srv *httptest.Server

	mu     sync.Mutex
	status int
	answer string
	got    []recorded
}

func newWireServer(t *testing.T) *wireServer {
	t.Helper()
	s := &wireServer{status: http.StatusOK, answer: `{"id":"msg_1","type":"message","model":"backend-model"}`}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.got = append(s.got, recorded{method: r.Method, path: r.URL.Path, query: r.URL.RawQuery, header: r.Header.Clone(), body: body})
		status, answer := s.status, s.answer
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, answer)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

// set makes the server answer status with body from now on.
func (s *wireServer) set(status int, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status, s.answer = status, body
}

// requests returns what the server has received so far.
func (s *wireServer) requests() []recorded {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]recorded(nil), s.got...)
}

// wireBackend is a backend of type typ on s, with api_key_env "KEY": the base URL an
// Anthropic or OpenAI client would use (with /v1), or the resource endpoint for the
// Azure types.
func wireBackend(s *wireServer, typ config.BackendType) *config.Backend {
	base := s.srv.URL + "/v1"
	if typ == config.BackendAzureOpenAI || typ == config.BackendAzureAnthropic {
		base = s.srv.URL
	}
	return &config.Backend{ID: string(typ), Type: typ, BaseURL: base, APIKeyEnv: "KEY", ConnectTimeout: time.Second,
		FirstEventTimeout: time.Minute, ResponseTimeout: time.Minute, StallTimeout: time.Minute}
}

// sendTo sends body to endpoint through b's module for backend-side model
// "backend-model".
func sendTo(r *Registry, b *config.Backend, endpoint Endpoint, body string) (Response, error) {
	return r.For(b).Send(context.Background(), &Request{Endpoint: endpoint,
		Deployment: config.Deployment{Backend: b, Model: "backend-model"}, Body: []byte(body), RequestID: "r",
		PublicModel: "pub"})
}

// members decodes a JSON object's members.
func members(t *testing.T, body []byte) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("body %s: %v", body, err)
	}
	return m
}

const messagesBody = `{"model":"pub","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`

// The two Anthropic types on the wire (docs/specs/GATEWAY.md, Providers): their URL
// layouts, the credential header and anthropic-version, the model name, and the
// service tier — anthropic forces standard_only on Messages, azure-anthropic passes
// the client's untouched; neither touches token counting's.
func TestAnthropicTypesOnTheWire(t *testing.T) {
	cases := []struct {
		typ              config.BackendType
		prefix           string
		credentialHeader string
		forcesTier       bool
	}{
		{config.BackendAnthropic, "/v1/", "X-Api-Key", true},
		{config.BackendAzureAnthropic, "/anthropic/v1/", "Api-Key", false},
	}
	r := moduleRegistry()
	for _, c := range cases {
		t.Run(string(c.typ), func(t *testing.T) {
			s := newWireServer(t)
			b := wireBackend(s, c.typ)
			for _, e := range []struct {
				endpoint Endpoint
				path     string
				tier     bool // the endpoint gets the forced tier
			}{{Messages, "messages", true}, {MessagesCountTokens, "messages/count_tokens", false}} {
				for _, clientTier := range []string{"", `"auto"`} {
					body := messagesBody
					if clientTier != "" {
						body = strings.Replace(body, `{"model":"pub"`, `{"model":"pub","service_tier":`+clientTier, 1)
					}
					resp, err := sendTo(r, b, e.endpoint, body)
					if err != nil {
						t.Fatalf("%s: %v", e.path, err)
					}
					readAll(resp)
					got := s.requests()[len(s.requests())-1]
					if got.method != http.MethodPost || got.path != c.prefix+e.path {
						t.Errorf("%s %s, want POST %s", got.method, got.path, c.prefix+e.path)
					}
					if got.header.Get(c.credentialHeader) != testCredential || got.header.Get("Authorization") != "" {
						t.Errorf("%s: credential headers %v", e.path, got.header)
					}
					if v := got.header.Get("Anthropic-Version"); v != "2023-06-01" {
						t.Errorf("%s: anthropic-version %q", e.path, v)
					}
					m := members(t, got.body)
					if string(m["model"]) != `"backend-model"` {
						t.Errorf("%s: model %s", e.path, m["model"])
					}
					want := clientTier
					if c.forcesTier && e.tier {
						want = `"standard_only"`
					}
					if tier := string(m["service_tier"]); tier != want {
						t.Errorf("%s with client tier %q: service_tier %q, want %q", e.path, clientTier, tier, want)
					}
				}
			}
		})
	}
}

// Price options the gateway does not price are refused before sending on both
// Anthropic types (docs/specs/GATEWAY.md, Providers → Standard price on Anthropic
// types), naming the parameter; token counting bills nothing and is not refused; the
// standard values pass.
func TestAnthropicTypesRefusePriceOptions(t *testing.T) {
	refused := []struct{ body, param string }{
		{`{"model":"pub","speed":"fast","messages":[]}`, "speed"},
		{`{"model":"pub","speed":1,"messages":[]}`, "speed"},
		{`{"model":"pub","inference_geo":"us","messages":[]}`, "inference_geo"},
		{`{"model":"pub","cache_control":{"type":"ephemeral","ttl":"1h"},"messages":[]}`, "cache_control.ttl"},
		{`{"model":"pub","system":[{"type":"text","text":"s"},{"type":"text","text":"t","cache_control":{"type":"ephemeral","ttl":"1h"}}],"messages":[]}`,
			"system[1].cache_control.ttl"},
		{`{"model":"pub","messages":[{"role":"user","content":"a"},{"role":"user","content":[{"type":"text","text":"b"},{"type":"text","text":"c","cache_control":{"type":"ephemeral","ttl":"1h"}}]}]}`,
			"messages[1].content[1].cache_control.ttl"},
		{`{"model":"pub","messages":[],"tools":[{"name":"x","input_schema":{},"cache_control":{"type":"ephemeral","ttl":"1h"}}]}`,
			"tools[0].cache_control.ttl"},
	}
	passed := []string{
		messagesBody,
		` {"model":"pub","speed":"standard","inference_geo":"global","speed_note":"fast","messages":[]}`,
		`{"model":"pub","speed":null,"inference_geo":null,"messages":[]}`,
		`{"model":"pub","system":"plain","cache_control":{"type":"ephemeral"},"messages":[{"role":"user","content":[{"type":"text","text":"c","cache_control":{"type":"ephemeral","ttl":"5m"}}]}]}`,
		// Keys match exactly, as the inbound stage reads them.
		`{"model":"pub","Speed":"fast","messages":[]}`,
	}
	r := moduleRegistry()
	for _, typ := range []config.BackendType{config.BackendAnthropic, config.BackendAzureAnthropic} {
		t.Run(string(typ), func(t *testing.T) {
			s := newWireServer(t)
			b := wireBackend(s, typ)
			for _, c := range refused {
				_, err := sendTo(r, b, Messages, c.body)
				refusal, ok := errors.AsType[*RefusalError](err)
				if !ok || refusal.Code != "price_option_unsupported" || refusal.Param != c.param {
					t.Errorf("%s: %v, want a price_option_unsupported refusal naming %s", c.body, err, c.param)
					continue
				}
				if strings.Contains(refusal.Message, `"fast"`) || strings.Contains(refusal.Message, `"us"`) {
					t.Errorf("refusal message %q echoes the client's value", refusal.Message)
				}
				// Token counting is billed nothing: not refused.
				resp, err := sendTo(r, b, MessagesCountTokens, c.body)
				if err != nil {
					t.Errorf("count_tokens %s: %v", c.body, err)
					continue
				}
				readAll(resp)
			}
			if n := len(s.requests()); n != len(refused) {
				t.Errorf("%d requests reached the backend, want only the %d token counts", n, len(refused))
			}
			for _, body := range passed {
				resp, err := sendTo(r, b, Messages, body)
				if err != nil {
					t.Errorf("%s: %v, want it sent", body, err)
					continue
				}
				readAll(resp)
			}
		})
	}
}

// A 1-hour cache write is refused wherever a Messages request can carry a
// cache_control — inside a tool result's content, a document's content source, a
// search result's content, at any depth — and a cache_control or ttl named twice in
// one object is refused, since the gateway and the backend could read different ones
// (the pre-merge review's M2): here the 1-hour one comes second, past the one a
// first-wins reader would stop at.
func TestAnthropicTypesRefuseNestedOneHourCacheWrites(t *testing.T) {
	const oneHour = `"cache_control":{"type":"ephemeral","ttl":"1h"}`
	refused := []struct{ body, code, param string }{
		{`{"model":"pub","messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":[{"type":"text","text":"r",` + oneHour + `}]}]}]}`,
			"price_option_unsupported", "messages[0].content[0].content[0].cache_control.ttl"},
		{`{"model":"pub","messages":[{"role":"user","content":[{"type":"document","source":{"type":"content","content":[{"type":"text","text":"d",` + oneHour + `}]}}]}]}`,
			"price_option_unsupported", "messages[0].content[0].source.content[0].cache_control.ttl"},
		{`{"model":"pub","messages":[{"role":"user","content":[{"type":"search_result","source":"s","title":"t","content":[{"type":"text","text":"x",` + oneHour + `}]}]}]}`,
			"price_option_unsupported", "messages[0].content[0].content[0].cache_control.ttl"},
		{`{"model":"pub","messages":[{"role":"user","content":[{"type":"text","text":"a","cache_control":{"type":"ephemeral"},` + oneHour + `}]}]}`,
			"duplicate_member", "messages[0].content[0].cache_control"},
		{`{"model":"pub","messages":[{"role":"user","content":[{"type":"text","text":"a","cache_control":{"type":"ephemeral","ttl":"5m","ttl":"1h"}}]}]}`,
			"duplicate_member", "messages[0].content[0].cache_control.ttl"},
		// The orders a last-wins reader missed: the 1-hour one first.
		{`{"model":"pub","messages":[{"role":"user","content":[{"type":"text","text":"a",` + oneHour + `,"cache_control":{"type":"ephemeral"}}]}]}`,
			"price_option_unsupported", "messages[0].content[0].cache_control.ttl"},
		{`{"model":"pub","messages":[{"role":"user","content":[{"type":"text","text":"a","cache_control":{"type":"ephemeral","ttl":"1h","ttl":"5m"}}]}]}`,
			"price_option_unsupported", "messages[0].content[0].cache_control.ttl"},
	}
	r := moduleRegistry()
	for _, typ := range []config.BackendType{config.BackendAnthropic, config.BackendAzureAnthropic} {
		t.Run(string(typ), func(t *testing.T) {
			s := newWireServer(t)
			b := wireBackend(s, typ)
			for _, c := range refused {
				_, err := sendTo(r, b, Messages, c.body)
				refusal, ok := errors.AsType[*RefusalError](err)
				if !ok || refusal.Code != c.code || refusal.Param != c.param {
					t.Errorf("%s: %v, want a %s refusal naming %s", c.body, err, c.code, c.param)
				}
			}
			if n := len(s.requests()); n != 0 {
				t.Errorf("%d requests reached the backend, want none", n)
			}
		})
	}
}

// Self-hosted types price nothing: the price options pass to their Messages
// endpoint untouched.
func TestSelfHostedTypesPassPriceOptions(t *testing.T) {
	r := moduleRegistry()
	for _, typ := range []config.BackendType{config.BackendVLLM, config.BackendLlamaServer} {
		s := newWireServer(t)
		b := wireBackend(s, typ)
		body := `{"model":"pub","speed":"fast","inference_geo":"us","service_tier":"auto","cache_control":{"type":"ephemeral","ttl":"1h"},"messages":[]}`
		resp, err := sendTo(r, b, Messages, body)
		if err != nil {
			t.Fatalf("%s: %v", typ, err)
		}
		readAll(resp)
		got := s.requests()[0]
		if got.path != "/v1/messages" {
			t.Errorf("%s: path %s", typ, got.path)
		}
		if want := strings.Replace(body, `"pub"`, `"backend-model"`, 1); string(got.body) != want {
			t.Errorf("%s: sent %s, want %s", typ, got.body, want)
		}
	}
}

// What the Anthropic types read in their backends' 404s: a missing model (the
// message naming it, or Azure's DeploymentNotFound), and the route a server does not
// have — on Messages, the core endpoint, a wrong base_url; on token counting, a server
// without the endpoint. A 529 is relayed as the backend's answer, a 5xx for the
// pipeline.
func TestAnthropicTypesReadTheirBackendsAnswers(t *testing.T) {
	cases := []struct {
		typ                     config.BackendType
		missingModel, wrongPath string
	}{
		{config.BackendAnthropic,
			`{"type":"error","error":{"type":"not_found_error","message":"model: backend-model"}}`,
			`{"type":"error","error":{"type":"not_found_error","message":"Not Found"}}`},
		{config.BackendAzureAnthropic,
			`{"error":{"code":"DeploymentNotFound","message":"The API deployment for this resource does not exist."}}`,
			`{"error":{"code":"404","message":"Resource not found"}}`},
	}
	r := moduleRegistry()
	for _, c := range cases {
		t.Run(string(c.typ), func(t *testing.T) {
			s := newWireServer(t)
			b := wireBackend(s, c.typ)
			code := func(endpoint Endpoint) Code {
				resp, err := sendTo(r, b, endpoint, messagesBody)
				if err == nil {
					readAll(resp)
					return ""
				}
				perr, _ := errors.AsType[*Error](err)
				if perr == nil {
					t.Fatalf("error %v is not a provider error", err)
				}
				return perr.Code
			}
			s.set(http.StatusNotFound, c.missingModel)
			if got := code(Messages); got != CodeModelMissing {
				t.Errorf("missing model: %q, want %s", got, CodeModelMissing)
			}
			s.set(http.StatusNotFound, c.wrongPath)
			if got := code(Messages); got != CodePathMissing {
				t.Errorf("wrong path on messages: %q, want %s", got, CodePathMissing)
			}
			if got := code(MessagesCountTokens); got != CodeEndpointMissing {
				t.Errorf("wrong path on count_tokens: %q, want %s", got, CodeEndpointMissing)
			}
			s.set(529, `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`)
			resp, err := sendTo(r, b, Messages, messagesBody)
			if err != nil {
				t.Fatalf("529: %v, want the answer", err)
			}
			readAll(resp)
			if resp.Status() != 529 {
				t.Errorf("529 answered %d", resp.Status())
			}
		})
	}
}

// anthropic probes its models list (paged: one page of up to 1000) with its
// credential and anthropic-version; azure-anthropic has none and sends nothing,
// every name counting as served.
func TestAnthropicTypesProbe(t *testing.T) {
	r := moduleRegistry()
	s := newWireServer(t)
	s.set(http.StatusOK, `{"data":[{"type":"model","id":"backend-model"}],"has_more":false}`)
	serves, err := r.Probe(context.Background(), wireBackend(s, config.BackendAnthropic))
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	got := s.requests()
	if len(got) != 1 || got[0].method != http.MethodGet || got[0].path != "/v1/models" || got[0].query != "limit=1000" {
		t.Fatalf("probe requests %+v, want one GET /v1/models?limit=1000", got)
	}
	if got[0].header.Get("X-Api-Key") != testCredential || got[0].header.Get("Anthropic-Version") != "2023-06-01" {
		t.Errorf("probe headers %v", got[0].header)
	}
	if !serves("backend-model") || serves("other") {
		t.Errorf("serves(listed) %v, serves(unlisted) %v", serves("backend-model"), serves("other"))
	}

	s = newWireServer(t)
	serves, err = r.Probe(context.Background(), wireBackend(s, config.BackendAzureAnthropic))
	if err != nil || !serves("anything") {
		t.Errorf("azure-anthropic probe: %v, serves %v", err, serves != nil && serves("anything"))
	}
	if n := len(s.requests()); n != 0 {
		t.Errorf("azure-anthropic probe sent %d requests, want none", n)
	}
	if ListsModels(config.BackendAzureAnthropic) || !ListsModels(config.BackendAnthropic) {
		t.Error("ListsModels: azure-anthropic must have none, anthropic one")
	}
}

// On the cloud types a missing model is read from the answer's structured fields
// only — OpenAI's and Azure's error code, Anthropic's not_found_error whose message
// begins "model:" — never from a message that merely names the model: a request can
// make these backends echo an ID it chose (a Responses item_reference, a file_id),
// and a client naming the backend model there must not open the deployment's circuit
// (the pre-merge review's M1). Such a 404 is the caller's, relayed.
func TestCloudTypesReadAMissingModelByItsFieldsOnly(t *testing.T) {
	openAIEcho := `{"error":{"message":"Item with id 'backend-model' not found.","type":"invalid_request_error","param":"input","code":null}}`
	anthropicEcho := `{"type":"error","error":{"type":"not_found_error","message":"File not found: backend-model"}}`
	r := moduleRegistry()
	for _, c := range []struct {
		typ      config.BackendType
		endpoint Endpoint
		body     string
		echo     string
	}{
		{config.BackendOpenAI, Responses, `{"model":"pub","input":"hi"}`, openAIEcho},
		{config.BackendAzureOpenAI, Responses, `{"model":"pub","input":"hi"}`, openAIEcho},
		{config.BackendAnthropic, Messages, messagesBody, anthropicEcho},
		{config.BackendAzureAnthropic, Messages, messagesBody, anthropicEcho},
	} {
		t.Run(string(c.typ), func(t *testing.T) {
			s := newWireServer(t)
			s.set(http.StatusNotFound, c.echo)
			resp, err := sendTo(r, wireBackend(s, c.typ), c.endpoint, c.body)
			if err != nil {
				t.Fatalf("an echoed ID naming the model = %v, want the 404 relayed", err)
			}
			if got := readAll(resp); resp.Status() != http.StatusNotFound || got != c.echo {
				t.Errorf("relayed %d %q, want 404 %q", resp.Status(), got, c.echo)
			}
		})
	}
}
