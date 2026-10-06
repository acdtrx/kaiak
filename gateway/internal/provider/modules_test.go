package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"kaiak/internal/config"
	"kaiak/internal/fakebackend"
)

// moduleCase is one backend type as a test reaches it on the fake backend, which
// serves the OpenAI layout under /v1/ and Azure's under /openai/v1/.
type moduleCase struct {
	typ config.BackendType
	// prefix is the path every request and the probe start with.
	prefix string
	// forcesTier: the module keeps every request on the standard service tier.
	forcesTier bool
	// credentialHeader carries the credential; keyRequired: the schema refuses the
	// type without api_key_env.
	credentialHeader, credentialValue string
	keyRequired                       bool
}

const testCredential = "module-secret"

var moduleCases = []moduleCase{
	{typ: config.BackendOpenAI, prefix: "/v1/", forcesTier: true,
		credentialHeader: "Authorization", credentialValue: "Bearer " + testCredential, keyRequired: true},
	{typ: config.BackendAzureOpenAI, prefix: "/openai/v1/", forcesTier: true,
		credentialHeader: "Api-Key", credentialValue: testCredential, keyRequired: true},
	{typ: config.BackendVLLM, prefix: "/v1/",
		credentialHeader: "Authorization", credentialValue: "Bearer " + testCredential},
	{typ: config.BackendLlamaServer, prefix: "/v1/",
		credentialHeader: "Authorization", credentialValue: "Bearer " + testCredential},
	{typ: config.BackendOpenAICompatible, prefix: "/v1/",
		credentialHeader: "Authorization", credentialValue: "Bearer " + testCredential},
}

// backend is a backend of the case's type on fb, with api_key_env "KEY" or none.
func (c moduleCase) backend(fb *fakebackend.Backend, withKey bool) *config.Backend {
	b := &config.Backend{ID: string(c.typ), Type: c.typ, BaseURL: fb.URL() + "/v1", ConnectTimeout: time.Second,
		FirstEventTimeout: time.Minute, ResponseTimeout: time.Minute, StallTimeout: time.Minute}
	if c.typ == config.BackendAzureOpenAI {
		b.BaseURL = fb.URL()
	}
	if withKey {
		b.APIKeyEnv = "KEY"
	}
	return b
}

func moduleRegistry() *Registry {
	return NewRegistry(func(name string) (string, bool) {
		if name == "KEY" {
			return testCredential, true
		}
		return "", false
	})
}

// sendThrough sends body to endpoint through b's module, reads the answer whole and
// returns the request the fake backend received.
func sendThrough(t *testing.T, fb *fakebackend.Backend, r *Registry, b *config.Backend, endpoint Endpoint, body string) *fakebackend.Request {
	t.Helper()
	before := len(fb.Requests())
	resp, err := r.For(b).Send(context.Background(), &Request{Endpoint: endpoint,
		Deployment: config.Deployment{Backend: b, Model: "backend-model"}, Body: []byte(body), RequestID: "r",
		PublicModel: "pub"})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	for {
		if _, err := resp.Next(); err != nil {
			break
		}
	}
	resp.Close()
	if resp.Status() != http.StatusOK {
		t.Fatalf("status %d", resp.Status())
	}
	got := fb.Requests()
	if len(got) != before+1 {
		t.Fatalf("%d requests reached the backend, want 1", len(got)-before)
	}
	return got[before]
}

// Every module through the same service-tier cases (docs/specs/GATEWAY.md, Providers →
// Service tier): openai and azure-openai keep requests on the standard tier — chat and
// Responses always carry "default", completions and embeddings only in place of a tier
// the client sent, Responses token counting keeps the client's; vllm, llama-server and
// openai-compatible pass the client's member untouched and add none.
func TestServiceTierByModule(t *testing.T) {
	tierCases := []struct {
		name     string
		endpoint Endpoint
		body     string
		// client is the client's service_tier member ("" for none); forced is what a
		// tier-forcing module sends ("" for none).
		client, forced string
	}{
		{"chat without a tier", ChatCompletions, `{"model":"pub","messages":[{"role":"user","content":"hi"}]}`, "", `"default"`},
		{"chat with priority", ChatCompletions, `{"model":"pub","service_tier": "priority","messages":[{"role":"user","content":"hi"}]}`, `"priority"`, `"default"`},
		{"embeddings without a tier", Embeddings, `{"model":"pub","input":"a"}`, "", ""},
		{"embeddings with flex", Embeddings, `{"model":"pub","service_tier":"flex","input":"a"}`, `"flex"`, `"default"`},
		{"responses without a tier", Responses, `{"model":"pub","input":"a"}`, "", `"default"`},
		{"responses with priority", Responses, `{"model":"pub","service_tier":"priority","input":"a"}`, `"priority"`, `"default"`},
		{"input_tokens with priority", ResponsesInputTokens, `{"model":"pub","service_tier":"priority","input":"a"}`, `"priority"`, `"priority"`},
	}
	fb := fakebackend.New()
	defer fb.Close()
	r := moduleRegistry()
	for _, m := range moduleCases {
		b := m.backend(fb, true)
		for _, tc := range tierCases {
			t.Run(string(m.typ)+"/"+tc.name, func(t *testing.T) {
				got := sendThrough(t, fb, r, b, tc.endpoint, tc.body)
				var members map[string]json.RawMessage
				if err := json.Unmarshal(got.Body, &members); err != nil {
					t.Fatalf("backend body %s: %v", got.Body, err)
				}
				want := tc.client
				if m.forcesTier {
					want = tc.forced
				}
				tier, sent := members["service_tier"]
				switch {
				case want == "" && sent:
					t.Errorf("service_tier %s sent, want none: %s", tier, got.Body)
				case want != "" && string(tier) != want:
					t.Errorf("service_tier %s, want %s: %s", tier, want, got.Body)
				}
			})
		}
	}
}

// Each module's URL layout, credential header and probe, against the fake backend; a
// type that allows no api_key_env sends no credential header without one.
func TestModuleURLCredentialAndProbe(t *testing.T) {
	endpoints := []struct {
		endpoint Endpoint
		path     string
		body     string
	}{
		{ChatCompletions, "chat/completions", `{"model":"pub","messages":[{"role":"user","content":"hi"}]}`},
		{Completions, "completions", `{"model":"pub","prompt":"hi"}`},
		{Embeddings, "embeddings", `{"model":"pub","input":"a"}`},
	}
	fb := fakebackend.New()
	defer fb.Close()
	fb.SetModels("backend-model")
	r := moduleRegistry()
	credentialHeaders := []string{"Authorization", "Api-Key"}
	for _, m := range moduleCases {
		for _, withKey := range []bool{true, false} {
			if !withKey && m.keyRequired {
				continue
			}
			name := string(m.typ) + "/with key"
			if !withKey {
				name = string(m.typ) + "/without key"
			}
			t.Run(name, func(t *testing.T) {
				b := m.backend(fb, withKey)
				// checkHeaders: the module's credential header alone, or none.
				checkHeaders := func(what string, h http.Header) {
					for _, name := range credentialHeaders {
						want := ""
						if withKey && name == m.credentialHeader {
							want = m.credentialValue
						}
						if got := h.Get(name); got != want {
							t.Errorf("%s: %s %q, want %q", what, name, got, want)
						}
					}
				}
				for _, e := range endpoints {
					got := sendThrough(t, fb, r, b, e.endpoint, e.body)
					if got.Path != m.prefix+e.path {
						t.Errorf("request to %s, want %s", got.Path, m.prefix+e.path)
					}
					checkHeaders(e.path, got.Header)
				}

				before := len(fb.ModelsRequests())
				serves, err := r.Probe(context.Background(), b)
				if err != nil {
					t.Fatalf("probe: %v", err)
				}
				probes := fb.ModelsRequests()[before:]
				if len(probes) != 1 || probes[0].Method != http.MethodGet || probes[0].Path != m.prefix+"models" {
					t.Fatalf("probe requests %v, want one GET %smodels", probes, m.prefix)
				}
				checkHeaders("probe", probes[0].Header)
				// Azure's list names models, not deployments: every name counts as served.
				wantOther := m.typ == config.BackendAzureOpenAI
				if !serves("backend-model") || serves("other-model") != wantOther {
					t.Errorf("serves(listed) %v, serves(unlisted) %v, want true, %v",
						serves("backend-model"), serves("other-model"), wantOther)
				}
			})
		}
	}
}
