package server

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"kaiak/internal/accounting"
	"kaiak/internal/config"
	"kaiak/internal/fakebackend"
)

// settledRecord waits for the next record a real-server request settles.
func settledRecord(t *testing.T, g *testGateway) accounting.UsageRecord {
	t.Helper()
	select {
	case r := <-g.usage.settled:
		return r
	case <-time.After(waitTimeout):
		t.Fatal("no usage record settled")
		return accounting.UsageRecord{}
	}
}

// onlyRecord returns the one record settled so far.
func onlyRecord(t *testing.T, g *testGateway) accounting.UsageRecord {
	t.Helper()
	records := g.usage.all()
	if len(records) != 1 {
		t.Fatalf("%d usage records, want 1: %+v", len(records), records)
	}
	return records[0]
}

func units(in, cached, out, reasoning int64) accounting.Units {
	return accounting.Units{
		config.UnitTokensIn: in, config.UnitTokensCached: cached,
		config.UnitTokensOut: out, config.UnitTokensReasoning: reasoning,
	}
}

func expectUnits(t *testing.T, r accounting.UsageRecord, want accounting.Units, estimated, partial bool) {
	t.Helper()
	if !maps.Equal(r.Units, want) {
		t.Errorf("units %v, want %v", r.Units, want)
	}
	if r.Estimated != estimated || r.Partial != partial {
		t.Errorf("estimated=%v partial=%v, want %v %v", r.Estimated, r.Partial, estimated, partial)
	}
}

// The fake backend's default text, as the estimate counts it.
const fakeContent = "Hello from the fake"

func TestNonStreamUsageIsRecordedExactlyAndPriced(t *testing.T) {
	g := newTestGateway(t)
	g.backend.SetReply(fakebackend.Reply{Usage: &fakebackend.Usage{
		PromptTokens: 100, CachedTokens: 40, CompletionTokens: 10, ReasoningTokens: 4}})
	w := do(t, g.h, call{method: "POST", path: "/v1/chat/completions", key: workloadKey,
		body: `{"model":"pair","messages":[]}`, header: map[string]string{"X-Request-Id": "acct-1"}})
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	r := onlyRecord(t, g)
	expectUnits(t, r, units(60, 40, 10, 4), false, false)
	// No tokens_cached price: cached input is charged at the input price.
	// 60×1 + 40×1 + 10×2 = 120 micro-dollars.
	if r.CostNanoUSD != 120_000 {
		t.Errorf("cost %d nano-USD, want 120000", r.CostNanoUSD)
	}
	if r.RequestID != "acct-1" || r.GatewayInstance != "gw-test" || r.KeyID != "k-eval" || r.Model != "pair" {
		t.Errorf("record identity %+v", r)
	}
	if !slices.Equal(r.Groups, []string{"research", "eval"}) {
		t.Errorf("groups %v, want the key's path", r.Groups)
	}
	if r.Deployment != (accounting.Deployment{Backend: "local", Model: "pair-a"}) {
		t.Errorf("deployment %+v", r.Deployment)
	}
	if len(r.RecordID) != 32 || r.GatewayTime.Location() != time.UTC || r.GatewayTime.IsZero() {
		t.Errorf("record ID %q, time %v", r.RecordID, r.GatewayTime)
	}
	logs := g.logText()
	for _, want := range []string{`"tokens_in":60`, `"tokens_cached":40`, `"tokens_out":10`,
		`"tokens_reasoning":4`, `"cost_usd":0.00012`, `"estimated":false`, `"partial":false`} {
		if !strings.Contains(logs, want) {
			t.Errorf("log misses %s:\n%s", want, logs)
		}
	}
}

func TestStreamUsageCountsTheHiddenUsageChunk(t *testing.T) {
	g := newTestGateway(t)
	g.backend.SetReply(fakebackend.Reply{Usage: &fakebackend.Usage{PromptTokens: 12, CompletionTokens: 30}})
	w := do(t, g.h, call{method: "POST", path: "/v1/chat/completions", key: userKey,
		body: `{"model":"open","stream":true}`})
	if strings.Contains(w.Body.String(), `"prompt_tokens"`) {
		t.Fatalf("client saw the usage chunk it did not ask for:\n%s", w.Body.String())
	}
	r := onlyRecord(t, g)
	expectUnits(t, r, units(12, 0, 30, 0), false, false)
	if r.CostNanoUSD != 0 || !slices.Equal(r.Groups, []string{"users", "ann"}) {
		t.Errorf("unpriced model: cost %d, groups %v", r.CostNanoUSD, r.Groups)
	}
}

func TestEmbeddingsCountPromptTokensOnly(t *testing.T) {
	g := newTestGateway(t)
	g.backend.SetReply(fakebackend.Reply{Usage: &fakebackend.Usage{PromptTokens: 9, CompletionTokens: 99}})
	do(t, g.h, call{method: "POST", path: "/v1/embeddings", key: userKey, body: `{"model":"open","input":"x"}`})
	expectUnits(t, onlyRecord(t, g), units(9, 0, 0, 0), false, false)
}

func TestMissingUsageIsEstimated(t *testing.T) {
	for _, c := range []struct{ name, body string }{
		{"non-stream", `{"model":"open","messages":[{"role":"user","content":"hi there"}]}`},
		{"stream", `{"model":"open","stream":true,"stream_options":{"include_usage":true}}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			g := newTestGateway(t)
			g.backend.SetReply(fakebackend.Reply{OmitUsage: true})
			do(t, g.h, call{method: "POST", path: "/v1/chat/completions", key: userKey, body: c.body})
			// Input: the client's body; output: the generated text only, ~4 bytes a token.
			in := int64(len(c.body)+3) / 4
			expectUnits(t, onlyRecord(t, g), units(in, 0, (int64(len(fakeContent))+3)/4, 0), true, false)
		})
	}
}

func TestClientDisconnectRecordsPartialUsage(t *testing.T) {
	g := newTestGateway(t)
	g.backend.SetReply(fakebackend.Reply{HangAfter: 2})
	url := serveGateway(t, g)
	body := `{"model":"open","stream":true}`
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resp := streamRequest(t, ctx, url, body)
	rd := bufio.NewReader(resp.Body)
	readEvent(t, rd)
	readEvent(t, rd)
	cancel()
	resp.Body.Close()

	// Two chunks seen, "Hello" and " from": 10 bytes.
	expectUnits(t, settledRecord(t, g), units(int64(len(body)+3)/4, 0, 3, 0), true, true)
}

// D1 (H6): the client leaving after its request reached the backend, before the
// first event, bills the prompt the backend has: the input estimated from the body,
// no output, flagged estimated and partial.
func TestClientGoneBeforeTheFirstEventBillsTheSentPrompt(t *testing.T) {
	g := newTestGateway(t)
	g.backend.SetReply(fakebackend.Reply{StallBeforeFirstByte: true})
	url := serveGateway(t, g)
	body := `{"model":"open","stream":true,"messages":[{"role":"user","content":"a prompt the backend received"}]}`
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

	expectUnits(t, settledRecord(t, g), units(int64(len(body)+3)/4, 0, 0, 0), true, true)
}

func TestBackendCutMidStreamRecordsPartialUsage(t *testing.T) {
	g := newTestGateway(t)
	g.backend.SetReply(fakebackend.Reply{CutAfter: 2})
	url := serveGateway(t, g)
	body := `{"model":"open","stream":true}`
	resp := streamRequest(t, context.Background(), url, body)
	_, _ = io.ReadAll(resp.Body)
	resp.Body.Close()

	expectUnits(t, settledRecord(t, g), units(int64(len(body)+3)/4, 0, 3, 0), true, true)
	if logs := g.logText(); !strings.Contains(logs, `"estimated":true,"partial":true`) {
		t.Errorf("log misses the flags:\n%s", logs)
	}
}

func TestRequestsWithoutABackendAnswerRecordNoUnits(t *testing.T) {
	g := newTestGateway(t)
	w := do(t, g.h, call{method: "POST", path: "/v1/chat/completions", key: workloadKey, body: `{"model":"down"}`})
	expectError(t, w, http.StatusBadGateway, "upstream_unavailable")
	r := onlyRecord(t, g)
	expectUnits(t, r, units(0, 0, 0, 0), false, true)
	if r.Deployment != (accounting.Deployment{Backend: "down", Model: "down"}) {
		t.Errorf("deployment %+v", r.Deployment)
	}
}

func TestBackendErrorStatusRecordsNoUnits(t *testing.T) {
	g := newTestGateway(t)
	g.backend.SetReply(fakebackend.Reply{Status: http.StatusBadRequest})
	w := do(t, g.h, call{method: "POST", path: "/v1/chat/completions", key: workloadKey, body: `{"model":"pair"}`})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status %d", w.Code)
	}
	r := onlyRecord(t, g)
	expectUnits(t, r, units(0, 0, 0, 0), false, false)
	if r.CostNanoUSD != 0 {
		t.Errorf("cost %d for a refused request", r.CostNanoUSD)
	}
}

func TestOneRecordPerRoutedRequestAndNoneBeforeRouting(t *testing.T) {
	g := newTestGateway(t)
	refused := []call{
		{method: "POST", path: "/v1/chat/completions", body: `{"model":"open"}`},                           // 401
		{method: "POST", path: "/v1/chat/completions", key: userKey, body: `{"model":"secret"}`},           // 404
		{method: "POST", path: "/v1/chat/completions", key: userKey, body: `[]`},                           // 400
		{method: "POST", path: "/v1/chat/completions", key: userKey, body: strings.Repeat(" ", bodyCap+1)}, // 413
		{method: "GET", path: "/v1/models", key: userKey},                                                  // not routed
		{method: "GET", path: "/v1/models/open/props", key: userKey},                                       // not routed
		{method: "POST", path: "/v1/nothing", key: userKey, body: `{}`},                                    // 404 unknown_url
		{method: "GET", path: "/v1/chat/completions", key: userKey},                                        // 405
	}
	for _, c := range refused {
		do(t, g.h, c)
	}
	if n := len(g.usage.all()); n != 0 {
		t.Fatalf("%d records for requests refused before routing", n)
	}

	routed := []call{
		{method: "POST", path: "/v1/chat/completions", key: userKey, body: `{"model":"open"}`},
		{method: "POST", path: "/v1/chat/completions", key: userKey, body: `{"model":"open","stream":true}`},
		{method: "POST", path: "/v1/completions", key: userKey, body: `{"model":"open","prompt":"x"}`},
		{method: "POST", path: "/v1/embeddings", key: userKey, body: `{"model":"open","input":"x"}`},
		{method: "POST", path: "/v1/chat/completions", key: workloadKey, body: `{"model":"down"}`},
	}
	for i, c := range routed {
		c.header = map[string]string{"X-Request-Id": "routed-" + string(rune('a'+i))}
		do(t, g.h, c)
	}
	var ids []string
	for _, r := range g.usage.all() {
		ids = append(ids, r.RequestID)
	}
	if want := []string{"routed-a", "routed-b", "routed-c", "routed-d", "routed-e"}; !slices.Equal(ids, want) {
		t.Errorf("records for %v, want one each for %v", ids, want)
	}
}

func TestUsageRecordCarriesNothingSensitive(t *testing.T) {
	g := newTestGateway(t)
	g.backend.SetReply(fakebackend.Reply{Chunks: []string{"secret answer"}})
	do(t, g.h, call{method: "POST", path: "/v1/chat/completions", key: workloadKey,
		body: `{"model":"pair","messages":[{"role":"user","content":"private prompt"}]}`})
	data, err := json.Marshal(onlyRecord(t, g))
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	want := []string{"cost_nano_usd", "deployment", "estimated", "gateway_instance", "gateway_time", "groups", "key_id",
		"model", "partial", "record_id", "request_id", "units"}
	if got := slices.Sorted(maps.Keys(fields)); !slices.Equal(got, want) {
		t.Errorf("record fields %v, want %v", got, want)
	}
	for _, secret := range []string{workloadKey, hashOf(workloadKey), localBackendKey, "private prompt", "secret answer"} {
		if strings.Contains(string(data), secret) {
			t.Errorf("record carries %q: %s", secret, data)
		}
	}
	if !strings.Contains(string(data), `"units":{"tokens_cached":0,"tokens_in":7,"tokens_out":1,"tokens_reasoning":0}`) {
		t.Errorf("units encoding: %s", data)
	}
}
