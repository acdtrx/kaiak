package server

import (
	"bufio"
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"kaiak/internal/fakebackend"
)

// backendModel returns the model name a backend request carried.
func backendModel(t *testing.T, r *fakebackend.Request) string {
	t.Helper()
	var body struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(r.Body, &body); err != nil {
		t.Fatal(err)
	}
	return body.Model
}

// openStream starts a streamed chat request to pair on srv and waits until its first
// event arrived: the request is then in flight on its deployment. The returned cancel
// disconnects the client.
func openStream(t *testing.T, g *testGateway, srv *httptest.Server) (deploymentModel string, cancel func()) {
	t.Helper()
	ctx, stop := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, "POST", srv.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"pair","stream":true,"messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+workloadKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	readEvent(t, bufio.NewReader(resp.Body))
	reqs := g.backend.Requests()
	return backendModel(t, reqs[len(reqs)-1]), func() { stop(); resp.Body.Close() }
}

func inFlight(g *testGateway) map[string]int { return g.router.InFlightByBackend() }

func TestLoadGoesToTheLeastBusyDeployment(t *testing.T) {
	g := newTestGateway(t)
	g.backend.SetReply(fakebackend.Reply{HangAfter: 1})
	srv := httptest.NewServer(g.h)
	t.Cleanup(srv.Close)

	var got []string
	var cancels []func()
	for range 3 {
		model, cancel := openStream(t, g, srv)
		got = append(got, model)
		cancels = append(cancels, cancel)
	}
	// Idle deployments take turns; the third request finds them tied at one each.
	if strings.Join(got, " ") != "pair-a pair-b pair-a" {
		t.Errorf("streams went to %v, want pair-a pair-b pair-a", got)
	}
	if want := map[string]int{"local": 2, "local-b": 1}; !maps.Equal(inFlight(g), want) {
		t.Errorf("in flight %v, want %v", inFlight(g), want)
	}
	// With pair-a busier, pair-b takes the next request.
	model, cancel := openStream(t, g, srv)
	cancels = append(cancels, cancel)
	if model != "pair-b" {
		t.Errorf("fourth stream went to %s, want pair-b", model)
	}

	// Client disconnects release every count. Close waits for every handler to end.
	for _, cancel := range cancels {
		cancel()
	}
	srv.Close()
	if n := inFlight(g); len(n) != 0 {
		t.Errorf("in flight %v after every client left, want none", n)
	}
}

func TestInFlightIsReleasedWhateverTheOutcome(t *testing.T) {
	g := newTestGateway(t)
	for _, c := range []struct {
		name  string
		model string
		reply fakebackend.Reply
	}{
		{"success", "pair", fakebackend.Reply{}},
		{"backend error", "pair", fakebackend.Reply{Status: 500}},
		{"backend refuses credentials", "pair", fakebackend.Reply{Status: 401}},
		{"backend unreachable", "down", fakebackend.Reply{}},
	} {
		g.backend.SetReply(c.reply)
		do(t, g.h, call{method: "POST", path: "/v1/chat/completions", key: workloadKey,
			body: `{"model":"` + c.model + `","stream":true}`})
		if n := inFlight(g); len(n) != 0 {
			t.Errorf("%s: in flight %v after the request, want none", c.name, n)
		}
	}
}

func TestInFlightSurvivesAConfigSwap(t *testing.T) {
	g := newTestGateway(t)
	g.backend.SetReply(fakebackend.Reply{HangAfter: 1})
	srv := httptest.NewServer(g.h)
	t.Cleanup(srv.Close)

	model, cancel := openStream(t, g, srv) // pair-a, running
	if model != "pair-a" {
		t.Fatalf("first stream went to %s", model)
	}
	g.backend.SetReply(fakebackend.Reply{})
	send := func() string {
		do(t, g.h, call{method: "POST", path: "/v1/chat/completions", key: workloadKey, body: `{"model":"pair"}`})
		reqs := g.backend.Requests()
		return backendModel(t, reqs[len(reqs)-1])
	}
	if got := send(); got != "pair-b" {
		t.Fatalf("second request went to %s, want pair-b", got)
	}

	// A reload builds new deployment values; the running stream still counts on
	// pair-a, so pair-b wins although it is pair-a's turn.
	g.holder.Swap(testSnapshot(t, g.backend.URL()))
	if want := map[string]int{"local": 1}; !maps.Equal(inFlight(g), want) {
		t.Errorf("in flight after reload %v, want %v", inFlight(g), want)
	}
	if got := send(); got != "pair-b" {
		t.Errorf("after reload the request went to %s, want pair-b (pair-a still busy)", got)
	}

	cancel()
	srv.Close()
	if n := inFlight(g); len(n) != 0 {
		t.Errorf("in flight %v after the stream ended, want none", n)
	}
}

func TestOutputLimitIsTheOneParameterSet(t *testing.T) {
	g := newTestGateway(t)
	cases := []struct {
		name, path, body, want string
	}{
		{"output-limit default filled, nothing else added", "/v1/chat/completions",
			`{"model":"pair","messages":[]}`,
			`{"model":"pair-a","messages":[],"max_completion_tokens":256}`},
		{"client values pass as sent, null included", "/v1/chat/completions",
			`{"model":"pair","temperature":0.9,"top_k":null,"max_tokens":100}`,
			`{"model":"pair-b","temperature":0.9,"top_k":null,"max_tokens":100}`},
		{"ceiling lowers the key the client used", "/v1/chat/completions",
			`{"model":"pair","max_tokens":5000,"temperature":1}`,
			`{"model":"pair-a","max_tokens":1024,"temperature":1}`},
		{"both keys set: each checked against the ceiling", "/v1/chat/completions",
			`{"model":"pair","max_completion_tokens":4096,"max_tokens":2048}`,
			`{"model":"pair-b","max_completion_tokens":1024,"max_tokens":1024}`},
		{"a key within the ceiling passes as sent (negative ones are refused: TestNegativeOutputLimitIsRefused)", "/v1/chat/completions",
			`{"model":"pair","max_tokens":500}`,
			`{"model":"pair-a","max_tokens":500}`},
		{"completions default under max_tokens", "/v1/completions",
			`{"model":"pair","prompt":"x"}`,
			`{"model":"pair-b","prompt":"x","max_tokens":256}`},
		{"embeddings: no output limit", "/v1/embeddings",
			`{"model":"pair","input":"x","max_tokens":5000}`,
			`{"model":"pair-a","input":"x","max_tokens":5000}`},
		{"no limit declared: untouched (within the context)", "/v1/chat/completions",
			`{"model":"open","max_tokens":8000}`,
			`{"model":"open","max_tokens":8000}`},
	}
	for _, c := range cases {
		w := do(t, g.h, call{method: "POST", path: c.path, key: workloadKey, body: c.body})
		if w.Code != http.StatusOK {
			t.Fatalf("%s: status %d: %s", c.name, w.Code, w.Body.String())
		}
		reqs := g.backend.Requests()
		if got := string(reqs[len(reqs)-1].Body); got != c.want {
			t.Errorf("%s:\nbackend got %s\nwant        %s", c.name, got, c.want)
		}
	}
}

func TestEffectiveOutputLimit(t *testing.T) {
	g := newTestGateway(t)
	snapshot := g.holder.Current()
	cases := []struct {
		name string
		ep   endpoint
		body string
		want int64 // -1: none
	}{
		{"default", endpointChatCompletions, `{"model":"pair"}`, 256},
		{"client value under the ceiling", endpointChatCompletions, `{"model":"pair","max_tokens":10}`, 10},
		{"lowered", endpointChatCompletions, `{"model":"pair","max_completion_tokens":9000}`, 1024},
		{"both set: the larger", endpointChatCompletions, `{"model":"pair","max_completion_tokens":20,"max_tokens":30}`, 30},
		{"completions ignores max_completion_tokens", endpointCompletions, `{"model":"pair","max_completion_tokens":20}`, 256},
		{"embeddings", endpointEmbeddings, `{"model":"pair","max_tokens":20}`, -1},
		{"no limit declared, client value", endpointChatCompletions, `{"model":"open","max_tokens":77}`, 77},
		{"no limit declared, none sent", endpointChatCompletions, `{"model":"open"}`, -1},
	}
	for _, c := range cases {
		rq := &request{endpoint: c.ep, snapshot: snapshot, body: []byte(c.body)}
		if err := parseOwnedFields(rq); err != nil {
			t.Fatal(err)
		}
		if err := applyModelParams(context.Background(), rq); err != nil {
			t.Fatal(err)
		}
		got := int64(-1)
		if rq.outputLimit != nil {
			got = *rq.outputLimit
		}
		if got != c.want {
			t.Errorf("%s: effective output limit %d, want %d", c.name, got, c.want)
		}
	}
}

func TestStreamCarriesThePublicModelName(t *testing.T) {
	g := newTestGateway(t)
	w := do(t, g.h, call{method: "POST", path: "/v1/chat/completions", key: workloadKey,
		body: `{"model":"pair","stream":true,"stream_options":{"include_usage":true}}`})
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	if v := w.Header().Get("X-Accel-Buffering"); v != "no" {
		t.Errorf("stream X-Accel-Buffering %q, want no", v)
	}
	direct := directStream(t, g.backend, onlyRequest(t, g.backend).Body)
	if !strings.Contains(direct, `"model":"pair-a"`) {
		t.Fatalf("backend stream carries no backend model name:\n%s", direct)
	}
	// Every chunk, usage chunk included, names the public model; nothing else differs.
	if want := strings.ReplaceAll(direct, `"model":"pair-a"`, `"model":"pair"`); w.Body.String() != want {
		t.Errorf("client stream\n%s\nwant\n%s", w.Body.String(), want)
	}
}

func TestModelListDependsOnTheKeyGroup(t *testing.T) {
	g := newTestGateway(t)
	ids := func(key string) string {
		w := do(t, g.h, call{method: "GET", path: "/v1/models", key: key})
		if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "application/json" {
			t.Fatalf("status %d, content type %q", w.Code, w.Header().Get("Content-Type"))
		}
		var list struct {
			Object string `json:"object"`
			Data   []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || list.Object != "list" {
			t.Fatalf("list %s (%v)", w.Body.String(), err)
		}
		var names []string
		for _, m := range list.Data {
			names = append(names, m.ID)
		}
		return strings.Join(names, " ")
	}
	if got := ids(userKey); got != "Org/open-7b open" {
		t.Errorf("user key lists %q", got)
	}
	if got := ids(workloadKey); got != "Org/open-7b down on-azure open pair renamed secret slow" {
		t.Errorf("workload key lists %q", got)
	}
}

func TestModelEntryAndProps(t *testing.T) {
	g := newTestGateway(t)
	entry := `{"id":"pair","object":"model","created":0,"owned_by":"kaiak","context_length":32768,` +
		`"capabilities":{"streaming":true,"tools":true,"vision":false,"reasoning":true},"reasoning_efforts":["low","high"],` +
		`"endpoints":["chat_completions","completions","embeddings"]`
	w := do(t, g.h, call{method: "GET", path: "/v1/models/pair", key: workloadKey})
	if got := strings.TrimSpace(w.Body.String()); w.Code != http.StatusOK || got != entry+"}" {
		t.Errorf("entry %d %s\nwant %s}", w.Code, got, entry)
	}
	w = do(t, g.h, call{method: "GET", path: "/v1/models/pair/props", key: workloadKey})
	want := entry + `,"output_limit":{"default":256,"ceiling":1024}}`
	if got := strings.TrimSpace(w.Body.String()); w.Code != http.StatusOK || got != want {
		t.Errorf("props %d %s\nwant %s", w.Code, got, want)
	}
	w = do(t, g.h, call{method: "GET", path: "/v1/models/Org/open-7b/props", key: userKey})
	if !strings.Contains(w.Body.String(), `"reasoning_efforts":[],"endpoints":["chat_completions","completions","embeddings"],"output_limit":null}`) {
		t.Errorf("props without declarations %s", w.Body.String())
	}
	// Not allowed for this key: the same 404 as an unknown model.
	w = do(t, g.h, call{method: "GET", path: "/v1/models/pair/props", key: userKey})
	expectError(t, w, http.StatusNotFound, "model_not_found")
}

// An injected output-limit default leaves room for the prompt: at most the context
// length minus the input estimate, never below 256 (and never above the default); a
// value the client sent is left as it is (L11).
func TestInjectedOutputDefaultFitsTheContext(t *testing.T) {
	g := newTestGateway(t)
	s := testSnapshotWith(t, g.backend.URL(), func(doc string) string {
		doc = replaceOnce(t, doc, `"context_length": 32768`, `"context_length": 400`)
		return replaceOnce(t, doc, `"output_limit": { "default": 256, "ceiling": 1024 }`,
			`"output_limit": { "default": 300, "ceiling": 400 }`)
	})
	// padded is a chat body of n bytes.
	padded := func(n int, extra string) string {
		head := `{"model":"pair"` + extra + `,"messages":[{"role":"user","content":"`
		return head + strings.Repeat("x", n-len(head)-len(`"}]}`)) + `"}]}`
	}
	for _, c := range []struct {
		name  string
		body  string
		param string // the output-limit parameter set; "" for none
		want  int64
	}{
		{"room for the default", padded(80, ""), "max_completion_tokens=300", 300},
		{"a long prompt lowers it", padded(500, ""), "max_completion_tokens=275", 275}, // 400 − 125
		{"never below 256", padded(1000, ""), "max_completion_tokens=256", 256},        // 400 − 250 = 150
		{"a client value is untouched", padded(1000, `,"max_tokens":300`), "", 300},
	} {
		rq := &request{endpoint: endpointChatCompletions, snapshot: s, body: []byte(c.body)}
		if err := parseOwnedFields(rq); err != nil {
			t.Fatal(err)
		}
		if err := applyModelParams(context.Background(), rq); err != nil {
			t.Fatal(err)
		}
		param := ""
		for _, p := range rq.params {
			if p.Key == "max_completion_tokens" || p.Key == "max_tokens" {
				param = p.Key + "=" + string(p.Value)
			}
		}
		if param != c.param || rq.outputLimit == nil || *rq.outputLimit != c.want {
			t.Errorf("%s: parameter %q, effective %v; want %q, %d", c.name, param, rq.outputLimit, c.param, c.want)
		}
	}
}
