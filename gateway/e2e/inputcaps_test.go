package e2e

// Input caps through the built binary: a request body repeating a top-level member,
// the per-request sequence and embeddings-input caps (global.max_sequences_per_request,
// global.max_embedding_inputs, at their defaults), and the API listener's connection
// cap (KAIAK_MAX_CONNECTIONS). The server tests prove each mechanism; these prove the
// binary applies them before anything reaches a backend.

import (
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kaiak/internal/fakebackend"
)

func TestRequestInputCapsThroughTheBinary(t *testing.T) {
	backend := fakebackend.New()
	defer backend.Close()
	dir := t.TempDir()
	configFile := filepath.Join(dir, "config.json")
	evalKey, evalHash := newKey()
	_, annHash := newKey()
	writeJSON(t, configFile, testConfig(backend.URL(), evalHash, annHash, ""))
	g := startGateway(t, configFile, "")

	// refused posts body raw (json.RawMessage keeps a repeated member) and wants a 400
	// with code and param.
	refused := func(what, path, body, code, param string) {
		t.Helper()
		r := g.post(t, path, evalKey, "", json.RawMessage(body))
		e, _ := r.json(t)["error"].(map[string]any)
		if r.StatusCode != http.StatusBadRequest || e["code"] != code || e["param"] != param {
			t.Errorf("%s: %d %s, want 400 %s on %s", what, r.StatusCode, r.body, code, param)
		}
	}
	prompts := func(n int) string { return strings.TrimSuffix(strings.Repeat(`"x",`, n), ",") }
	refused("model twice", "/v1/chat/completions",
		`{"model":"chat","messages":[{"role":"user","content":"hi"}],"model":"chat"}`, "duplicate_member", "model")
	refused("17 prompts", "/v1/completions", `{"model":"chat","prompt":[`+prompts(17)+`]}`, "invalid_value", "prompt")
	refused("2049 embedding inputs", "/v1/embeddings", `{"model":"embed","input":[`+prompts(2049)+`]}`, "invalid_value", "input")
	if n := len(backend.Requests()); n != 0 {
		t.Errorf("backend received %d requests for refused ones", n)
	}

	// At the caps the requests pass.
	if r := g.post(t, "/v1/completions", evalKey, "", json.RawMessage(`{"model":"chat","prompt":[`+prompts(16)+`]}`)); r.StatusCode != http.StatusOK {
		t.Errorf("16 prompts: %d %s", r.StatusCode, r.body)
	}
	if r := g.post(t, "/v1/embeddings", evalKey, "", json.RawMessage(`{"model":"embed","input":[`+prompts(2048)+`]}`)); r.StatusCode != http.StatusOK {
		t.Errorf("2048 embedding inputs: %d %s", r.StatusCode, r.body)
	}
	g.stop(t)
}

func TestConnectionCapThroughTheBinary(t *testing.T) {
	backend := fakebackend.New()
	defer backend.Close()
	dir := t.TempDir()
	configFile := filepath.Join(dir, "config.json")
	evalKey, evalHash := newKey()
	_, annHash := newKey()
	writeJSON(t, configFile, testConfig(backend.URL(), evalHash, annHash, ""))
	g := startGatewayEnv(t, append(gatewayEnv(configFile, ""), "KAIAK_MAX_CONNECTIONS=1"))
	if v := g.metric(t, "kaiak_connections_refused_total"); v != 0 {
		t.Fatalf("refused connections at start: %v", v)
	}

	// One connection, kept open after its request (keep-alive), fills the cap.
	held := dialAPI(t, g)
	_ = held.SetDeadline(time.Now().Add(waitLimit))
	if _, err := held.Write([]byte("GET /v1/models HTTP/1.1\r\nHost: kaiak\r\nAuthorization: Bearer " + evalKey + "\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	head := make([]byte, 12)
	if _, err := io.ReadFull(held, head); err != nil || string(head) != "HTTP/1.1 200" {
		t.Fatalf("the connection within the cap: %q, %v", head, err)
	}

	// The next is closed at once with nothing sent; the admin listener is not capped.
	if answer := readUntilClosed(t, dialAPI(t, g)); answer != "" {
		t.Errorf("connection beyond the cap answered %q, want a close with nothing sent", answer)
	}
	g.waitMetric(t, "the refusal counted", "kaiak_connections_refused_total", func(v float64) bool { return v == 1 })
	g.stop(t)
}
