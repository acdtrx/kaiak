package provider

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"kaiak/internal/config"
)

// A rerank request reaches a vllm or llama-server backend at /v1/rerank with the model
// edit alone — every other byte as the client sent it, stream included — and its
// answer is relayed as the backend sent it, the top-level model the public name
// (docs/specs/GATEWAY.md, Providers: passthrough edits; rerank answers are relayed as
// the backend sends them).
func TestRerankPassesThroughWithTheModelEditAlone(t *testing.T) {
	const body = `{"model":"pub", "query":"q","documents":["a",{"content":[]}],"top_n":1,"stream":"yes","return_documents":false}`
	const answer = `{"id":"rerank-1","model":"backend-model","usage":{"prompt_tokens":9,"total_tokens":9},` +
		`"results":[{"index":0,"document":{"text":"a","model":"backend-model"},"relevance_score":0.9}]}`
	for _, typ := range []config.BackendType{config.BackendVLLM, config.BackendLlamaServer} {
		t.Run(string(typ), func(t *testing.T) {
			s := newWireServer(t)
			s.set(http.StatusOK, answer)
			resp, err := sendTo(moduleRegistry(), wireBackend(s, typ), Rerank, body)
			if err != nil {
				t.Fatal(err)
			}
			if resp.Stream() {
				t.Error("a rerank answer read as a stream")
			}
			want := strings.Replace(answer, `"model":"backend-model"`, `"model":"pub"`, 1)
			if got := readAll(resp); got != want {
				t.Errorf("answer\n %s\nwant\n %s", got, want)
			}
			got := s.requests()
			if len(got) != 1 || got[0].path != "/v1/rerank" {
				t.Fatalf("requests %+v, want one to /v1/rerank", got)
			}
			if sent, want := string(got[0].body), strings.Replace(body, `"pub"`, `"backend-model"`, 1); sent != want {
				t.Errorf("sent\n %s\nwant\n %s", sent, want)
			}
		})
	}
}

// A rerank answer is a JSON body, complete once its top-level value closed; an event
// stream answering a rerank request is no rerank answer and never complete
// (docs/specs/GATEWAY.md, Providers: complete responses).
func TestRerankAnswerCompleteness(t *testing.T) {
	for _, c := range []struct {
		name, contentType, body string
		complete                bool
	}{
		{"whole body", "application/json", `{"model":"m","results":[]}`, true},
		{"body cut short", "application/json", `{"model":"m","results":[`, false},
		{"an event stream", "text/event-stream", "data: {\"results\":[]}\n\ndata: [DONE]\n\n", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", c.contentType)
				_, _ = io.WriteString(w, c.body)
			}))
			defer srv.Close()
			b := &config.Backend{ID: "b", Type: config.BackendVLLM, BaseURL: srv.URL + "/v1",
				ConnectTimeout: time.Second, FirstEventTimeout: time.Minute, ResponseTimeout: time.Minute, StallTimeout: time.Minute}
			resp, err := NewRegistry(func(string) (string, bool) { return "", false }).For(b).Send(context.Background(),
				&Request{Endpoint: Rerank, Deployment: config.Deployment{Backend: b, Model: "m"},
					Body: []byte(`{"model":"m","query":"q","documents":["a"]}`), RequestID: "r", PublicModel: "m"})
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Close()
			for err == nil {
				_, err = resp.Next()
			}
			if got := errors.Is(err, io.EOF); got != c.complete {
				t.Errorf("end %v, want complete %v", err, c.complete)
			}
			if !c.complete && !errors.Is(err, ErrIncomplete) {
				t.Errorf("end %v, want ErrIncomplete", err)
			}
		})
	}
}
