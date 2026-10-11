package server

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"kaiak/internal/accounting"
	"kaiak/internal/config"
	"kaiak/internal/fakebackend"
	"kaiak/internal/provider"
)

// The request line's keys are the field table's (docs/specs/GATEWAY.md, Observability:
// Logs → the request line): for each kind of outcome, exactly the attributes the
// table says are present, under the table's names.
func TestRequestLineKeysFollowTheFieldTable(t *testing.T) {
	// Every line: the record's own keys and the attributes present always.
	always := []string{"time", "level", "msg", "kaiak.request.id", "http.request.method", "url.path",
		"http.response.status_code", "kaiak.request.duration"}
	// A client that left before any answer was sent: no status code.
	unanswered := slices.DeleteFunc(slices.Clone(always), func(k string) bool { return k == "http.response.status_code" })
	routed := []string{"kaiak.key.id", "kaiak.key.group", "gen_ai.request.model", "gen_ai.request.stream",
		"gen_ai.operation.name", "kaiak.backend.id", "kaiak.backend.type", "kaiak.deployment.model", "kaiak.attempts"}
	settled := []string{"gen_ai.usage.input_tokens", "gen_ai.usage.cache_read.input_tokens",
		"gen_ai.usage.cache_write.input_tokens", "gen_ai.usage.output_tokens", "gen_ai.usage.reasoning.output_tokens",
		"kaiak.usage.cost_usd", "kaiak.usage.estimated", "kaiak.usage.partial"}
	keys := func(groups ...[]string) []string { return slices.Concat(groups...) }

	cases := []struct {
		name  string
		setup func(t *testing.T, g *testGateway)
		c     call
		want  []string
		// values: fields whose value the case pins.
		values map[string]any
		// gone: the client has left before the request is handled.
		gone bool
	}{
		{name: "success", c: call{method: "POST", path: "/v1/chat/completions", key: workloadKey,
			body: `{"model":"open","messages":[]}`},
			want: keys(always, routed, settled),
			values: map[string]any{"http.request.method": "POST", "gen_ai.operation.name": "chat",
				"kaiak.backend.type": "openai-compatible", "http.response.status_code": float64(200)}},
		{name: "success on azure-openai", c: call{method: "POST", path: "/v1/chat/completions", key: workloadKey,
			body: `{"model":"on-azure","messages":[]}`},
			want:   keys(always, routed, settled, []string{"gen_ai.provider.name"}),
			values: map[string]any{"kaiak.backend.type": "azure-openai", "gen_ai.provider.name": "azure.ai.openai"}},
		{name: "limit refusal",
			setup: func(t *testing.T, g *testGateway) {
				withLimits(t, g, "", `[{ "type": "requests_per_minute", "value": 0 }]`)
			},
			c: call{method: "POST", path: "/v1/chat/completions", key: workloadKey, body: `{"model":"open","messages":[]}`},
			want: keys(always, []string{"kaiak.key.id", "kaiak.key.group", "gen_ai.request.model",
				"gen_ai.request.stream", "gen_ai.operation.name", "error.type", "kaiak.limit.scope", "kaiak.limit.group",
				"kaiak.limit.type", "kaiak.limit.enforced", "kaiak.limit.configured", "kaiak.limit.used"}),
			values: map[string]any{"error.type": "rate_limit_exceeded", "http.response.status_code": float64(429),
				"kaiak.limit.scope": "group", "kaiak.limit.group": "eval"}},
		{name: "global limit refusal",
			setup: func(t *testing.T, g *testGateway) {
				g.holder.Swap(testSnapshotWith(t, g.backend.URL(), func(doc string) string {
					return replaceOnce(t, doc, `"global": { `,
						`"global": { "limits": [{ "type": "requests_per_minute", "value": 0 }], `)
				}))
			},
			c: call{method: "POST", path: "/v1/chat/completions", key: workloadKey, body: `{"model":"open","messages":[]}`},
			want: keys(always, []string{"kaiak.key.id", "kaiak.key.group", "gen_ai.request.model",
				"gen_ai.request.stream", "gen_ai.operation.name", "error.type", "kaiak.limit.scope",
				"kaiak.limit.type", "kaiak.limit.enforced", "kaiak.limit.configured", "kaiak.limit.used"}),
			values: map[string]any{"kaiak.limit.scope": "global"}},
		{name: "401", c: call{method: "POST", path: "/v1/chat/completions", key: expiredKey,
			body: `{"model":"open","messages":[]}`},
			want:   keys(always, []string{"kaiak.key.id", "gen_ai.operation.name", "kaiak.auth.failure", "error.type"}),
			values: map[string]any{"kaiak.auth.failure": "expired_key", "http.response.status_code": float64(401)}},
		{name: "backend error status",
			setup: func(t *testing.T, g *testGateway) {
				g.backend.SetReply(fakebackend.Reply{Status: http.StatusInternalServerError,
					Body: `{"error":{"message":"disk full","type":"server_error","code":"internal"}}`})
			},
			c: call{method: "POST", path: "/v1/chat/completions", key: workloadKey, body: `{"model":"open","messages":[]}`},
			want: keys(always, routed, settled, []string{"kaiak.retry_refused", "error.type",
				"kaiak.upstream.error.code", "kaiak.upstream.error.type"}),
			values: map[string]any{"error.type": "upstream_error", "kaiak.upstream.error.code": "internal",
				"kaiak.upstream.error.type": "server_error"}},
		{name: "backend unreachable", c: call{method: "POST", path: "/v1/chat/completions", key: workloadKey,
			body: `{"model":"down","messages":[]}`},
			want: keys(always, routed, settled, []string{"kaiak.retry_refused", "error.type",
				"kaiak.upstream.error.message"}),
			values: map[string]any{"error.type": "upstream_unavailable"}},
		{name: "unknown method", c: call{method: "BREW", path: "/v1/chat/completions", key: workloadKey},
			want:   keys(always, []string{"http.request.method_original", "error.type"}),
			values: map[string]any{"http.request.method": "_OTHER", "http.request.method_original": "BREW"}},
		{name: "client left before any answer", gone: true,
			c:      call{method: "POST", path: "/v1/chat/completions", key: workloadKey, body: `{"model":"open","messages":[]}`},
			want:   keys(unanswered, routed, settled, []string{"error.type"}),
			values: map[string]any{"error.type": "client_closed"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g := newTestGateway(t)
			if c.setup != nil {
				c.setup(t, g)
			}
			c.c.header = map[string]string{"X-Request-Id": "line"}
			if c.gone {
				r := httptest.NewRequest(c.c.method, c.c.path, strings.NewReader(c.c.body))
				r.Header.Set("Authorization", "Bearer "+c.c.key)
				r.Header.Set("X-Request-Id", "line")
				ctx, leave := context.WithCancel(r.Context())
				leave()
				g.h.ServeHTTP(httptest.NewRecorder(), r.WithContext(ctx))
			} else {
				do(t, g.h, c.c)
			}
			fields := logFields(t, g, "line")
			got := slices.Sorted(maps.Keys(fields))
			want := slices.Sorted(slices.Values(c.want))
			if !slices.Equal(got, want) {
				t.Errorf("keys\n got %v\nwant %v", got, want)
			}
			for k, v := range c.values {
				if fields[k] != v {
					t.Errorf("%s = %v, want %v", k, fields[k], v)
				}
			}
		})
	}
}

// A media endpoint's request line carries its gen_ai.output.type, and its settled
// usage the media units under their metrics' names — each only when not 0, summed over
// the request's records; a line without media carries none of them.
func TestRequestLineCarriesTheOutputTypeAndMediaUsage(t *testing.T) {
	media := &endpoint{body: true, api: provider.ChatCompletions, operation: "generate_content",
		outputType: "speech"}
	line := func(ep *endpoint, records ...accounting.UsageRecord) map[string]any {
		var buf bytes.Buffer
		a := &API{logger: slog.New(slog.NewJSONHandler(&buf, nil))}
		rq := &request{r: httptest.NewRequest(http.MethodPost, "/v1/x", nil),
			w: &statusWriter{ResponseWriter: httptest.NewRecorder(), status: http.StatusOK}, endpoint: ep,
			id: "req-1", start: time.Now(), records: records}
		if len(records) > 0 {
			rq.usage = &records[len(records)-1]
		}
		a.logRequest(rq)
		var fields map[string]any
		if err := json.Unmarshal(buf.Bytes(), &fields); err != nil {
			t.Fatal(err)
		}
		return fields
	}
	record := func(images, audioOut int64) accounting.UsageRecord {
		rec := accounting.UsageRecord{Units: units(10, 0, 0, 40, 0)}
		rec.Units[config.UnitImages], rec.Units[config.UnitAudioSecondsOut] = images, audioOut
		return rec
	}

	fields := line(media, record(0, 3), record(0, 4))
	if fields["gen_ai.output.type"] != "speech" || fields["gen_ai.operation.name"] != "generate_content" {
		t.Errorf("output type %v, operation %v; want speech, generate_content", fields["gen_ai.output.type"],
			fields["gen_ai.operation.name"])
	}
	if fields["kaiak.usage.audio.output.duration"] != float64(7) {
		t.Errorf("audio generated %v, want the records' 7 s summed", fields["kaiak.usage.audio.output.duration"])
	}
	for _, absent := range []string{"kaiak.usage.images", "kaiak.usage.audio.input.duration"} {
		if v, ok := fields[absent]; ok {
			t.Errorf("%s = %v on a line whose records carry none", absent, v)
		}
	}
	if fields := line(media, record(4, 0)); fields["kaiak.usage.images"] != float64(4) {
		t.Errorf("images %v, want 4", fields["kaiak.usage.images"])
	}

	fields = line(bodyEndpoint(provider.ChatCompletions), record(0, 0))
	for _, absent := range []string{"gen_ai.output.type", "kaiak.usage.images", "kaiak.usage.audio.output.duration",
		"kaiak.usage.audio.input.duration"} {
		if v, ok := fields[absent]; ok {
			t.Errorf("chat line carries %s = %v", absent, v)
		}
	}
}
