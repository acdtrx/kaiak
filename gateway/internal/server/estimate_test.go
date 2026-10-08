package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"kaiak/internal/accounting"
	"kaiak/internal/fakebackend"
)

// base64Image is an inline image as a client sends it: a data URL whose payload is n
// bytes of base64.
func base64Image(n int) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	return "data:image/png;base64," + strings.Repeat(alphabet, n/len(alphabet)+1)[:n]
}

// imageChat is a streamed chat body for model with one text part and one image part.
func imageChat(model, text, imageURL string) string {
	return `{"model":"` + model + `","stream":true,"messages":[{"role":"user","content":[` +
		`{"type":"text","text":"` + text + `"},` +
		`{"type":"image_url","image_url":{"url":"` + imageURL + `","detail":"low"}}]}]}`
}

// withLargeBodies raises the test config's body cap to 4 MiB and sets model pair's
// output limit default (ceiling 16384) and context length.
func withLargeBodies(t *testing.T, def, context int, extra func(string) string) func(string) string {
	return func(doc string) string {
		doc = replaceOnce(t, doc, `"max_request_body_bytes": 1024 }`, `"max_request_body_bytes": 4194304 }`)
		doc = replaceOnce(t, doc, `"context_length": 32768`, `"context_length": `+strconv.Itoa(context))
		doc = replaceOnce(t, doc, `"output_limit": { "default": 256, "ceiling": 1024 }`,
			`"output_limit": { "default": `+strconv.Itoa(def)+`, "ceiling": 16384 }`)
		if extra != nil {
			doc = extra(doc)
		}
		return doc
	}
}

// injectedLimit is the output limit the backend received under key.
func injectedLimit(t *testing.T, g *testGateway, key string) int64 {
	t.Helper()
	reqs := g.backend.Requests()
	if len(reqs) != 1 {
		t.Fatalf("backend got %d requests, want 1", len(reqs))
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(reqs[0].Body, &body); err != nil {
		t.Fatal(err)
	}
	n, err := strconv.ParseInt(string(body[key]), 10, 64)
	if err != nil {
		t.Fatalf("%s = %s", key, body[key])
	}
	return n
}

// A 450 KB inline photo on a 32k-context model counts as one image (1000
// tokens), not ~115k text tokens: the injected default stays whole and the
// reservation is the text around it plus 1000 plus the output limit.
func TestInlineImageCountsAsOneMediaItem(t *testing.T) {
	const tpm = 1_000_000
	g := newTestGateway(t)
	g.holder.Swap(testSnapshotWith(t, g.backend.URL(), withLargeBodies(t, 4096, 32768, func(doc string) string {
		return replaceOnce(t, doc, `"research": {}`, `"research": { "limits": [{ "type": "tokens_per_minute", "value": 1000000 }] }`)
	})))
	image := base64Image(450 << 10)
	body := imageChat("pair", "what is in this photo?", image)
	w := do(t, g.h, call{method: "POST", path: "/v1/chat/completions", key: workloadKey, body: body})
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if got := injectedLimit(t, g, "max_completion_tokens"); got != 4096 {
		t.Errorf("injected default %d, want 4096", got)
	}
	remaining, _ := strconv.ParseInt(w.Header().Get("x-ratelimit-remaining-tokens"), 10, 64)
	reserved := tpm - remaining
	text := accounting.EstimateTokens(int64(len(body) - len(image)))
	if reserved < text-2+accounting.InlineMediaTokens+4096 || reserved > text+accounting.InlineMediaTokens+4096 {
		t.Errorf("reserved %d, want about %d (text) + %d (image) + 4096", reserved, text, accounting.InlineMediaTokens)
	}
}

// A 1.4 MB PNG under a 100k tokens/minute limit is admitted — its estimate is
// one image, not 350k tokens of base64.
func TestLargeInlineImageFitsATokenLimit(t *testing.T) {
	g := newTestGateway(t)
	g.holder.Swap(testSnapshotWith(t, g.backend.URL(), withLargeBodies(t, 16384, 32768, func(doc string) string {
		return replaceOnce(t, doc, `"research": {}`, `"research": { "limits": [{ "type": "tokens_per_minute", "value": 100000 }] }`)
	})))
	body := imageChat("pair", "describe", base64Image(1_399_000))
	w := do(t, g.h, call{method: "POST", path: "/v1/chat/completions", key: workloadKey, body: body})
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
}

// A completion batch's default output is fitted to its largest prompt, not
// to the whole batch — 16 prompts of 12 KB each leave each sequence room for the
// default of 16384 in a 32k context.
func TestBatchDefaultOutputFitsThePrompt(t *testing.T) {
	g := newTestGateway(t)
	g.holder.Swap(testSnapshotWith(t, g.backend.URL(), withLargeBodies(t, 16384, 32768, nil)))
	prompts := make([]string, 16)
	for i := range prompts {
		prompts[i] = `"` + strings.Repeat("word ", 12000/5) + `"`
	}
	body := `{"model":"pair","prompt":[` + strings.Join(prompts, ",") + `]}`
	w := do(t, g.h, call{method: "POST", path: "/v1/completions", key: workloadKey, body: body})
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if got := injectedLimit(t, g, "max_tokens"); got != 16384 {
		t.Errorf("injected default %d per prompt, want 16384", got)
	}
}

// A streamed image request the client cancels after it reached the backend is
// billed its estimated input — the text plus one image, not ~350k tokens of base64.
func TestCancelledImageRequestEstimatesOneMediaItem(t *testing.T) {
	g := newTestGateway(t)
	g.holder.Swap(testSnapshotWith(t, g.backend.URL(), withLargeBodies(t, 256, 32768, func(doc string) string {
		return replaceOnce(t, doc, `"allowed_models": ["open", "Org/open-7b"]`, `"allowed_models": ["open", "pair"]`)
	})))
	g.backend.SetReply(fakebackend.Reply{Before: fakebackend.StallFirstByte})
	url := serveGateway(t, g)
	image := base64Image(1_399_000)
	body := imageChat("pair", "describe", image)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		req, _ := http.NewRequestWithContext(ctx, "POST", url+"/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+userKey)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
		}
	}()
	arrived := <-g.backend.Arrivals()
	cancel()
	<-done
	<-arrived.Canceled()

	record := settledRecord(t, g)
	in := record.Units["tokens_in"]
	text := accounting.EstimateTokens(int64(len(body) - len(image)))
	if in < text-2+accounting.InlineMediaTokens || in > text+accounting.InlineMediaTokens || !record.Estimated || !record.Partial {
		t.Errorf("record %+v, want tokens_in about %d (text) + %d (image), estimated and partial", record, text, accounting.InlineMediaTokens)
	}
}
