package server

import (
	"context"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"kaiak/internal/fakebackend"
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
		setup func(g *testGateway)
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
			setup: func(g *testGateway) {
				withLimits(t, g, "", `[{ "type": "requests_per_minute", "value": 0 }]`)
			},
			c: call{method: "POST", path: "/v1/chat/completions", key: workloadKey, body: `{"model":"open","messages":[]}`},
			want: keys(always, []string{"kaiak.key.id", "kaiak.key.group", "gen_ai.request.model",
				"gen_ai.request.stream", "gen_ai.operation.name", "error.type", "kaiak.limit.scope", "kaiak.limit.group",
				"kaiak.limit.type", "kaiak.limit.enforced", "kaiak.limit.configured", "kaiak.limit.used"}),
			values: map[string]any{"error.type": "rate_limit_exceeded", "http.response.status_code": float64(429),
				"kaiak.limit.scope": "group", "kaiak.limit.group": "eval"}},
		{name: "global limit refusal",
			setup: func(g *testGateway) {
				g.holder.Swap(testSnapshotWith(t, g.backend.URL(), func(doc string) string {
					return strings.Replace(doc, `"global": { `,
						`"global": { "limits": [{ "type": "requests_per_minute", "value": 0 }], `, 1)
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
			setup: func(g *testGateway) {
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
				c.setup(g)
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
