package provider

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"kaiak/internal/config"
	"kaiak/internal/fakebackend"
)

// Each server's answer to a path it does not have (docs/specs/GATEWAY.md, Providers:
// wrong path to a host), as the step that settled them recorded it.
var unknownPathAnswers = map[string]string{
	"openai":       `{"error":{"message":"Invalid URL (POST /chat/completions)","type":"invalid_request_error","param":null,"code":null}}`,
	"azure-openai": `{"error":{"code":"404","message":"Resource not found"}}`,
	"vllm":         `{"detail":"Not Found"}`,
	"llama-server": `{"error":{"message":"File Not Found","type":"not_found_error","code":404}}`,
	// What servers of no known type answer below their API: Go's, Express's and
	// nginx's pages, and an empty 404.
	"go":    "404 page not found\n",
	"html":  "<!DOCTYPE html>\n<html><body><pre>Cannot POST /chat/completions</pre></body></html>",
	"nginx": "<html>\r\n<head><title>404 Not Found</title></head>\r\n<body>\r\n<center><h1>404 Not Found</h1></center>\r\n</body>\r\n</html>\r\n",
	"empty": "",
}

// unknownPathOf lists, per module, the answers it reads as an unknown path; every
// other one is the caller's 404. openai-compatible knows no server: any 404 not in
// the OpenAI error shape is one.
var unknownPathOf = map[config.BackendType][]string{
	config.BackendOpenAI:           {"openai"},
	config.BackendAzureOpenAI:      {"azure-openai"},
	config.BackendVLLM:             {"vllm"},
	config.BackendLlamaServer:      {"llama-server"},
	config.BackendOpenAICompatible: {"vllm", "go", "html", "nginx", "empty"},
}

// notFoundServer answers every request 404 with the body last set, as
// application/json when it starts with "{", as HTML otherwise.
func notFoundServer(t *testing.T) (*httptest.Server, func(body string)) {
	t.Helper()
	var body atomic.Value
	body.Store("")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		current := body.Load().(string)
		if strings.HasPrefix(current, "{") {
			w.Header().Set("Content-Type", "application/json")
		} else {
			w.Header().Set("Content-Type", "text/html")
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, current)
	}))
	t.Cleanup(srv.Close)
	return srv, func(b string) { body.Store(b) }
}

// sendFor sends a chat request for backend-side model "backend-model" through b's
// module.
func sendFor(r *Registry, b *config.Backend) (Response, error) {
	return r.For(b).Send(context.Background(), &Request{Endpoint: ChatCompletions,
		Deployment: config.Deployment{Backend: b, Model: "backend-model"}, Body: []byte(`{"model":"pub"}`),
		RequestID: "r", PublicModel: "pub"})
}

// readAll reads a response's events whole and closes it.
func readAll(resp Response) string {
	defer resp.Close()
	var got []byte
	for {
		ev, err := resp.Next()
		if err != nil {
			return string(got)
		}
		got = append(got, ev.Data...)
	}
}

// Per module: its server's unknown-path answer is upstream_path_missing, whose error
// names the URL and not the backend's text; any other server's answer, a caller's
// 404 and a missing model behave as they did — relayed whole, or
// upstream_model_missing, which is read first.
func TestUnknownPathByModule(t *testing.T) {
	srv, answer := notFoundServer(t)
	r := moduleRegistry()
	for _, m := range moduleCases {
		t.Run(string(m.typ), func(t *testing.T) {
			b := &config.Backend{ID: "b", Type: m.typ, BaseURL: srv.URL + "/v1", APIKeyEnv: "KEY",
				ConnectTimeout: time.Second, FirstEventTimeout: time.Minute, ResponseTimeout: time.Minute,
				StallTimeout: time.Minute}
			for name, body := range unknownPathAnswers {
				answer(body)
				resp, err := sendFor(r, b)
				var perr *Error
				switch isUnknown := slices.Contains(unknownPathOf[m.typ], name); {
				case isUnknown && (!errors.As(err, &perr) || perr.Code != CodePathMissing):
					t.Errorf("%s answer = %v, want upstream_path_missing", name, err)
				case isUnknown && !strings.Contains(err.Error(), srv.URL+"/v1"):
					t.Errorf("%s answer: error %q does not name the URL", name, err)
				case isUnknown && body != "" && strings.Contains(err.Error(), strings.TrimSpace(body)):
					t.Errorf("%s answer: error %q carries the backend's text", name, err)
				case !isUnknown && err != nil:
					t.Errorf("%s answer = %v, want it relayed", name, err)
				case !isUnknown:
					if got := readAll(resp); resp.Status() != http.StatusNotFound || got != body {
						t.Errorf("%s answer relayed %d %q, want 404 %q", name, resp.Status(), got, body)
					}
				}
			}

			// A caller's 404 in the module's own error shape is relayed.
			const callers = `{"error":{"message":"No such adapter: foo","type":"invalid_request_error","param":"model","code":null}}`
			answer(callers)
			resp, err := sendFor(r, b)
			if err != nil {
				t.Fatalf("caller's 404 = %v, want it relayed", err)
			}
			if got := readAll(resp); got != callers {
				t.Errorf("caller's 404 relayed as %q", got)
			}

			// A missing model is read first, whatever shape it comes in — on a
			// self-hosted type vLLM's older one, which is not the OpenAI error shape; on
			// a cloud type its API's code (the message alone is not read there).
			missing := `{"object":"error","message":"The model ` + "`backend-model`" + ` does not exist.","type":"NotFoundError","code":404}`
			switch m.typ {
			case config.BackendOpenAI:
				missing = `{"error":{"message":"The model 'backend-model' does not exist","type":"invalid_request_error","param":"model","code":"model_not_found"}}`
			case config.BackendAzureOpenAI:
				missing = `{"error":{"code":"DeploymentNotFound","message":"The API deployment for this resource does not exist."}}`
			}
			answer(missing)
			_, err = sendFor(r, b)
			var perr *Error
			if !errors.As(err, &perr) || perr.Code != CodeModelMissing {
				t.Errorf("missing model = %v, want upstream_model_missing", err)
			}
		})
	}
}

// A models list answering 404 is a *pathMissingError carrying the module's hint for
// base_url (the config-apply check warns with it); other failures are not.
func TestProbeOfAMissingModelsList(t *testing.T) {
	fb := fakebackend.New()
	defer fb.Close()
	r := moduleRegistry()
	for _, m := range moduleCases {
		t.Run(string(m.typ), func(t *testing.T) {
			b := m.backend(fb, true)
			fb.SetModelsStatus(http.StatusNotFound)
			_, err := r.Probe(context.Background(), b)
			var perr *pathMissingError
			if !errors.As(err, &perr) || perr.backend != b.ID || perr.url != fb.URL()+m.prefix+"models" {
				t.Fatalf("probe of a 404 models list = %#v, want a pathMissingError for %s", err, b.BaseURL)
			}
			want := versionPathHint
			if m.typ == config.BackendAzureOpenAI {
				want = azurePathHint
			}
			if perr.hint != want {
				t.Errorf("hint %q, want %q", perr.hint, want)
			}
			if strings.Contains(err.Error(), testCredential) {
				t.Errorf("error %q names the credential", err)
			}

			fb.SetModelsStatus(http.StatusServiceUnavailable)
			if _, err := r.Probe(context.Background(), b); errors.As(err, &perr) {
				t.Errorf("probe of a 503 models list = %v, want no pathMissingError", err)
			}
			fb.SetModelsStatus(0)
		})
	}
}
