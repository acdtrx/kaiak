package e2e

// Inline media and limit refusals through the built binary (the daily-operations
// review's D1 and D6): the input estimate and the refusal's log line as an operator
// meets them, over a real HTTP body of screenshot size.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"kaiak/internal/fakebackend"
)

// screenshot is an inline PNG as a client sends it: a data URL carrying n bytes of
// base64.
func screenshot(n int) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	return "data:image/png;base64," + strings.Repeat(alphabet, n/len(alphabet)+1)[:n]
}

// A 1.4 MB screenshot on a 32k-context model under a 100k tokens/min group limit
// counts as one media item (1000 tokens): admitted, with the whole default output
// sent to the backend. A text prompt larger than the limit is refused, and its log
// line and metric name the limit that refused it.
func TestInlineImageKeepsItsDefaultOutput(t *testing.T) {
	backend := fakebackend.New()
	defer backend.Close()
	g, key := startReliability(t, backend.URL(), backend.URL(), func(cfg map[string]any) {
		model := reliableChatModel()
		model["deployments"] = []any{map[string]any{"backend": "a", "model": reliableModel}}
		model["metadata"].(map[string]any)["context_length"] = 32768
		model["output_limit"] = map[string]any{"default": 4096, "ceiling": 8192}
		cfg["models"] = map[string]any{"vision": model}
		cfg["groups"].(map[string]any)["w"].(map[string]any)["limits"] = []any{
			map[string]any{"type": "tokens_per_minute", "value": 100000},
		}
	})

	image := map[string]any{"model": "vision", "messages": []any{map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "text", "text": "What is on this screen?"},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": screenshot(1400 << 10)}},
	}}}}
	if r := g.post(t, "/v1/chat/completions", key, "screenshot", image); r.StatusCode != http.StatusOK {
		t.Fatalf("screenshot: %d %s, want 200", r.StatusCode, r.body)
	}
	g.settled(t, "screenshot")
	reqs := backend.Requests()
	if len(reqs) != 1 {
		t.Fatalf("backend got %d requests, want 1", len(reqs))
	}
	var sent map[string]any
	if err := json.Unmarshal(reqs[0].Body, &sent); err != nil {
		t.Fatal(err)
	}
	if sent["max_completion_tokens"] != 4096.0 {
		t.Errorf("max_completion_tokens sent = %v, want the whole default 4096", sent["max_completion_tokens"])
	}

	// About 100k text tokens plus the 4096 default: more than the limit holds.
	text := chatBody("vision", false, nil)
	text["messages"] = []any{map[string]any{"role": "user", "content": strings.Repeat("word ", 80000)}}
	r := g.post(t, "/v1/chat/completions", key, "too-large", text)
	if r.StatusCode != http.StatusTooManyRequests || r.errorCode(t) != "rate_limit_exceeded" {
		t.Fatalf("too-large: %d %s, want 429 rate_limit_exceeded", r.StatusCode, r.body)
	}
	line := g.settled(t, "too-large")
	for field, want := range map[string]any{"kaiak.limit.scope": "group", "kaiak.limit.id": "w",
		"kaiak.limit.type": "tokens_per_minute", "kaiak.limit.configured": 100000.0, "kaiak.key.group": "w"} {
		if line[field] != want {
			t.Errorf("log line %s = %v, want %v", field, line[field], want)
		}
	}
	if requested, _ := line["kaiak.limit.requested"].(float64); requested <= 100000 {
		t.Errorf("log line requested = %v, want above the limit", line["kaiak.limit.requested"])
	}
	if got := g.metric(t, `kaiak_limit_rejections_total{scope_kind="group",type="tokens_per_minute"}`); got != 1 {
		t.Errorf("limit rejections = %v, want 1", got)
	}
	if got := len(backend.Requests()); got != 1 {
		t.Errorf("backend got %d requests, want only the screenshot", got)
	}
	g.stop(t)
}
