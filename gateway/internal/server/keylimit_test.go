package server

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"kaiak/internal/fakebackend"
)

// withKeyLimit applies the test config with global.max_concurrent_requests_per_key
// n, on top of edit (nil = none), as cmd/kaiak applies a config: swapped in, and its
// caps handed to routing.
func withKeyLimit(t *testing.T, g *testGateway, n int, edit func(t *testing.T, doc string) string) {
	t.Helper()
	g.apply(t, func(doc string) string {
		if edit != nil {
			doc = edit(t, doc)
		}
		return replaceOnce(t, doc, `"global": { `, `"global": { "max_concurrent_requests_per_key": `+strconv.Itoa(n)+`, `)
	})
}

// keysInFlight is keyID's requests in flight as the per-key limit counts them.
func keysInFlight(g *testGateway, keyID string) int64 {
	return g.h.(*API).keys.count(keyID)
}

// startStreams starts n stalled chat streams with key on url, cancelled by the
// returned function, and waits until each reached the backend.
func startStreams(t *testing.T, g *testGateway, url, key, model string, n int) context.CancelFunc {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	for range n {
		go func() {
			req, _ := http.NewRequestWithContext(ctx, "POST", url+"/v1/chat/completions",
				strings.NewReader(`{"model":"`+model+`","stream":true,"messages":[]}`))
			req.Header.Set("Authorization", "Bearer "+key)
			if resp, err := http.DefaultClient.Do(req); err == nil {
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
		}()
	}
	for range n {
		select {
		case <-g.backend.Arrivals():
		case <-time.After(waitTimeout):
			t.Fatal("a stream never reached the backend")
		}
	}
	return cancel
}

// E10: a key has at most max_concurrent_requests_per_key requests in flight on a
// gateway. Past it every request of that key — a model endpoint too — is refused at
// once, 429 concurrency_limit_exceeded with Retry-After: 1, before its body is read
// or anything reaches a backend; other keys are unaffected, and the key's slots come
// back as its requests end.
func TestPerKeyConcurrencyLimit(t *testing.T) {
	g := newTestGateway(t)
	withKeyLimit(t, g, 2, nil)
	g.backend.SetReply(fakebackend.Reply{StallBeforeFirstByte: true})
	url := serveGateway(t, g)
	cancel := startStreams(t, g, url, workloadKey, "open", 2)
	if n := keysInFlight(g, "k-eval"); n != 2 {
		t.Fatalf("k-eval in flight %d, want 2", n)
	}

	for _, c := range []call{
		{method: "POST", path: "/v1/chat/completions", key: workloadKey, body: chatBody},
		{method: "GET", path: "/v1/models", key: workloadKey},
	} {
		w := do(t, g.h, c)
		expectError(t, w, http.StatusTooManyRequests, "concurrency_limit_exceeded")
		if typ, _, param := openAIError(t, w); typ != "requests" || param != nil {
			t.Errorf("%s: type %q, param %v; want requests, null", c.path, typ, param)
		}
		if got := w.Header().Get("Retry-After"); got != "1" {
			t.Errorf("%s: Retry-After %q, want 1", c.path, got)
		}
		if !strings.Contains(w.Body.String(), "2 requests in flight") {
			t.Errorf("%s: message does not name the limit: %s", c.path, w.Body.String())
		}
	}
	if n := len(g.backend.Requests()); n != 2 {
		t.Errorf("backend got %d requests, want the 2 streams only", n)
	}
	if w := do(t, g.h, call{method: "GET", path: "/v1/models", key: userKey}); w.Code != http.StatusOK {
		t.Errorf("another key: status %d, want 200: %s", w.Code, w.Body.String())
	}
	// The caller's own doing: the class of the gateway's other per-caller limits.
	expectMetricLines(t, g.metricsText(), `kaiak_errors_total{class="rate_limited"} 2`)
	if !strings.Contains(g.logText(), `"error.type":"concurrency_limit_exceeded"`) {
		t.Errorf("log line misses the code:\n%s", g.logText())
	}

	cancel()
	waitIdle(t, g)
	if n := keysInFlight(g, "k-eval"); n != 0 {
		t.Fatalf("k-eval in flight %d once its requests ended, want 0", n)
	}
	g.backend.SetReply(fakebackend.Reply{})
	if w := do(t, g.h, call{method: "POST", path: "/v1/chat/completions", key: workloadKey, body: chatBody}); w.Code != http.StatusOK {
		t.Errorf("status %d once the slots came back, want 200: %s", w.Code, w.Body.String())
	}
}

// E10: a request's slot is given back however the request ends — the finisher
// mechanism, which also runs when the handler panics to cut a broken-off response.
// With a limit of 1, each ending is followed by a request of the same key that must
// be admitted.
func TestPerKeySlotIsReleasedOnEveryPath(t *testing.T) {
	post := func(g *testGateway, body string) int {
		return do(t, g.h, call{method: "POST", path: "/v1/chat/completions", key: workloadKey, body: body}).Code
	}
	for _, c := range []struct {
		name string
		// edit (nil = none) is applied to the config under the per-key limit.
		edit func(t *testing.T, doc string) string
		// run ends one request of k-eval (or more, the last one refused) a given way
		// and reports its status (0 when the client saw no answer).
		run  func(t *testing.T, g *testGateway) int
		want int
	}{
		{name: "success", want: 200, run: func(t *testing.T, g *testGateway) int { return post(g, chatBody) }},
		{name: "invalid json", want: 400, run: func(t *testing.T, g *testGateway) int { return post(g, `{`) }},
		{name: "body too large", want: 413, run: func(t *testing.T, g *testGateway) int {
			return post(g, paddedChat(t, "open", false, 2000))
		}},
		{name: "model not found", want: 404, run: func(t *testing.T, g *testGateway) int {
			return post(g, `{"model":"nope","messages":[]}`)
		}},
		{name: "upstream unavailable", want: 502, run: func(t *testing.T, g *testGateway) int {
			return post(g, `{"model":"down","messages":[]}`)
		}},
		{name: "rate limited", want: 429,
			edit: func(t *testing.T, doc string) string {
				return replaceOnce(t, doc, `"eval": { "parent": "research",`,
					`"eval": { "parent": "research", "limits": [{ "type": "requests_per_minute", "value": 1 }],`)
			},
			run: func(t *testing.T, g *testGateway) int {
				if code := post(g, chatBody); code != 200 {
					t.Fatalf("first request: status %d", code)
				}
				return post(g, chatBody)
			}},
		{name: "queue full", want: 429, edit: cappedDoc, run: func(t *testing.T, g *testGateway) int {
			// Another key holds local's one slot; Org/open-7b has no queue.
			g.backend.SetReply(fakebackend.Reply{StallBeforeFirstByte: true})
			defer startStreams(t, g, serveGateway(t, g), userKey, "open", 1)()
			return post(g, `{"model":"Org/open-7b","messages":[]}`)
		}},
		{name: "client disconnect", want: 0, run: func(t *testing.T, g *testGateway) int {
			g.backend.SetReply(fakebackend.Reply{StallBeforeFirstByte: true})
			startStreams(t, g, serveGateway(t, g), workloadKey, "open", 1)()
			return 0
		}},
		{name: "broken-off stream (handler panic)", want: 200, run: func(t *testing.T, g *testGateway) int {
			g.backend.SetReply(fakebackend.Reply{CutAfter: 1})
			resp := workloadStream(t, serveGateway(t, g), `{"model":"open","stream":true,"messages":[]}`)
			defer resp.Body.Close()
			if _, err := io.Copy(io.Discard, resp.Body); err == nil {
				t.Error("the broken-off stream ended cleanly")
			}
			return resp.StatusCode
		}},
		{name: "drain cut", want: 0, run: func(t *testing.T, g *testGateway) int {
			g.backend.SetReply(fakebackend.Reply{StallBeforeFirstByte: true})
			d := newDrainable(t, g)
			startStreams(t, g, d.url, workloadKey, "open", 1)
			g.drain.begin(DrainTimes{}, d.logger)
			g.drain.refuse(d.l, d.logger)
			g.drain.finish(d.l, 20*time.Millisecond, nil, d.logger)
			// A drained gateway admits nothing more: the next request would end at
			// admission, so the check below is on the count alone.
			return 0
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			g := newTestGateway(t)
			withKeyLimit(t, g, 1, c.edit)
			if got := c.run(t, g); got != c.want {
				t.Errorf("status %d, want %d", got, c.want)
			}
			waitIdle(t, g)
			if n := keysInFlight(g, "k-eval"); n != 0 {
				t.Fatalf("k-eval in flight %d after the request ended, want 0", n)
			}
			if g.drain.Draining() {
				return
			}
			g.backend.SetReply(fakebackend.Reply{})
			if w := do(t, g.h, call{method: "GET", path: "/v1/models", key: workloadKey}); w.Code != http.StatusOK {
				t.Errorf("the next request: status %d, want 200: %s", w.Code, w.Body.String())
			}
		})
	}
}
