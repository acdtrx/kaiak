package provider

import (
	"encoding/json"
	"strings"
	"testing"

	"kaiak/internal/config"
)

func TestEditObjectSplicesOnlyOwnedValues(t *testing.T) {
	cases := []struct {
		name, in, want string
		edits          []memberEdit
	}{
		{
			name:  "replace keeps layout and neighbours",
			in:    "{ \"a\" : [1, 2.50],\n \"model\":\"x\" ,\"n\": 1e400 }",
			want:  "{ \"a\" : [1, 2.50],\n \"model\":\"y\" ,\"n\": 1e400 }",
			edits: []memberEdit{setValue("model", []byte(`"y"`))},
		},
		{
			name:  "number values keep their exact bytes",
			in:    `{"model":"x","big":123456789012345678901234567890,"f":0.1000000000000000000001}`,
			want:  `{"model":"y","big":123456789012345678901234567890,"f":0.1000000000000000000001}`,
			edits: []memberEdit{setValue("model", []byte(`"y"`))},
		},
		{
			name:  "insert after the last member",
			in:    "{\"model\":\"x\", \"z\":{\"k\":[]}\n}",
			want:  "{\"model\":\"x\", \"z\":{\"k\":[]},\"stream_options\":{\"include_usage\":true}\n}",
			edits: []memberEdit{{key: "stream_options", set: setIncludeUsage}},
		},
		{
			name:  "insert into an empty object",
			in:    `{ }`,
			want:  `{ "k":1}`,
			edits: []memberEdit{setValue("k", []byte(`1`))},
		},
		{
			name:  "null stream_options becomes an object",
			in:    `{"stream_options":null,"model":"x"}`,
			want:  `{"stream_options":{"include_usage":true},"model":"x"}`,
			edits: []memberEdit{{key: "stream_options", set: setIncludeUsage}},
		},
		{
			name:  "nested edit keeps sibling options",
			in:    `{"stream_options":{"continuous_usage_stats":true, "include_usage":false}}`,
			want:  `{"stream_options":{"continuous_usage_stats":true, "include_usage":true}}`,
			edits: []memberEdit{{key: "stream_options", set: setIncludeUsage}},
		},
		{
			name:  "nested insert into an empty options object",
			in:    `{"stream_options":{}}`,
			want:  `{"stream_options":{"include_usage":true}}`,
			edits: []memberEdit{{key: "stream_options", set: setIncludeUsage}},
		},
		{
			name:  "keys match exactly after decoding",
			in:    `{"Model":"a","model":"b"}`,
			want:  `{"Model":"a","model":"y"}`,
			edits: []memberEdit{setValue("model", []byte(`"y"`))},
		},
		{
			name:  "same value gives the same bytes",
			in:    "{\"model\": \"x\",\n\"b\":true}",
			want:  "{\"model\": \"x\",\n\"b\":true}",
			edits: []memberEdit{setValue("model", []byte(`"x"`))},
		},
		{
			name: "replace and insert together",
			in:   `{"model":"x","stream":true}`,
			want: `{"model":"y","stream":true,"stream_options":{"include_usage":true}}`,
			edits: []memberEdit{setValue("model", []byte(`"y"`)),
				{key: "stream_options", set: setIncludeUsage}},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := editObject([]byte(c.in), c.edits...)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != c.want {
				t.Errorf("got  %s\nwant %s", got, c.want)
			}
			if !json.Valid(got) {
				t.Error("result is not valid JSON")
			}
		})
	}
}

func TestEditObjectRefusesARepeatedEditedKey(t *testing.T) {
	for _, in := range []string{`{"model":"a","x":1,"model":"b"}`, `{"model":"a","mod\u0065l":"b"}`} {
		if got, err := editObject([]byte(in), setValue("model", []byte(`"y"`))); err == nil {
			t.Errorf("%s edited to %s", in, got)
		}
	}
	// A repeat of a key no edit touches is the backend's business.
	got, err := editObject([]byte(`{"model":"a","x":1,"x":2}`), setValue("model", []byte(`"y"`)))
	if err != nil || string(got) != `{"model":"y","x":1,"x":2}` {
		t.Errorf("got %s, %v", got, err)
	}
}

func TestEditObjectRejectsNonObjects(t *testing.T) {
	for _, in := range []string{`[1]`, `"s"`, `{"a":`, `{"a":1} {}`, ``} {
		if _, err := editObject([]byte(in), setValue("model", []byte(`"y"`))); err == nil {
			t.Errorf("%q accepted", in)
		}
	}
}

func TestPassthroughBodyEdits(t *testing.T) {
	deployment := config.Deployment{Model: `org/m"q`}
	cases := []struct {
		name       string
		req        Request
		want       string
		stripUsage bool
	}{
		{"non-stream: model only", Request{Body: []byte(`{"model":"pub","x":[1]}`)},
			`{"model":"org/m\"q","x":[1]}`, false},
		{"stream without usage: include_usage added", Request{Body: []byte(`{"model":"pub","stream":true}`), Stream: true},
			`{"model":"org/m\"q","stream":true,"stream_options":{"include_usage":true}}`, true},
		{"stream with usage: untouched options", Request{Body: []byte(`{"model":"pub","stream":true,"stream_options":{"include_usage":true}}`),
			Stream: true, IncludeUsage: true},
			`{"model":"org/m\"q","stream":true,"stream_options":{"include_usage":true}}`, false},
		{"pipeline params: replaced where present, added in order where absent", Request{
			Body:   []byte(`{"model":"pub","max_tokens":9000,"x":1}`),
			Params: []Param{{"max_tokens", []byte("1024")}, {"top_k", []byte("[1,2]")}, {"temperature", []byte("0.2")}}},
			`{"model":"org/m\"q","max_tokens":1024,"x":1,"top_k":[1,2],"temperature":0.2}`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.req.Deployment = deployment
			got, strip, err := passthroughBody(&c.req)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != c.want || strip != c.stripUsage {
				t.Errorf("got %s strip=%v, want %s strip=%v", got, strip, c.want, c.stripUsage)
			}
		})
	}
}

// A chat or Responses request always names the standard tier: an absent one is
// "auto", the deployment's or project's own setting. Completions and embeddings get
// "default" only in place of a tier the client sent: OpenAI refuses parameters an
// endpoint does not define. Responses token counting keeps the client's.
func TestStandardServiceTier(t *testing.T) {
	cases := []struct {
		endpoint Endpoint
		body     string
		want     string
	}{
		{ChatCompletions, `{"model":"pub","messages":[]}`, `{"model":"d","messages":[],"service_tier":"default"}`},
		{ChatCompletions, `{"model":"pub","service_tier":"priority","messages":[]}`, `{"model":"d","service_tier":"default","messages":[]}`},
		{Completions, `{"model":"pub","prompt":"a"}`, `{"model":"d","prompt":"a"}`},
		{Completions, `{"model":"pub","prompt":"a","service_tier":"auto"}`, `{"model":"d","prompt":"a","service_tier":"default"}`},
		{Embeddings, `{"model":"pub","input":"a"}`, `{"model":"d","input":"a"}`},
		{Embeddings, `{"model":"pub","input":"a","service_tier":"flex"}`, `{"model":"d","input":"a","service_tier":"default"}`},
		{Responses, `{"model":"pub","input":"a"}`, `{"model":"d","input":"a","service_tier":"default","store":false}`},
		{Responses, `{"model":"pub","input":"a","service_tier":"priority"}`, `{"model":"d","input":"a","service_tier":"default","store":false}`},
		{ResponsesInputTokens, `{"model":"pub","input":"a"}`, `{"model":"d","input":"a"}`},
		{ResponsesInputTokens, `{"model":"pub","input":"a","service_tier":"priority"}`, `{"model":"d","input":"a","service_tier":"priority"}`},
	}
	for _, c := range cases {
		req := &Request{Endpoint: c.endpoint, Deployment: config.Deployment{Model: "d"}, Body: []byte(c.body)}
		got, _, err := passthroughBody(req, standardServiceTier(c.endpoint))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != c.want {
			t.Errorf("endpoint %d: got %s, want %s", c.endpoint, got, c.want)
		}
	}
}

func TestIsUsageOnlyChunk(t *testing.T) {
	cases := map[string]bool{
		`{"id":"1","choices":[],"usage":{"prompt_tokens":1}}`:             true,
		`{"id":"1","choices":[{"delta":{}}],"usage":{"prompt_tokens":1}}`: false,
		`{"id":"1","choices":[{"delta":{"content":"x"}}],"usage":null}`:   false,
		`{"id":"1","choices":[]}`:                                         false,
		`{"id":"1","Choices":[],"usage":{"prompt_tokens":1}}`:             false,
		`{"id":"1","choices":null,"usage":{"prompt_tokens":1}}`:           false,
		`[DONE]`: false,
		`{"id":"1","choices":[],"usage":{"prompt_tokens":1},"extra":{"a":[1]}}`: true,
	}
	for payload, want := range cases {
		if got := isUsageOnlyChunk([]byte(payload)); got != want {
			t.Errorf("%s: got %v, want %v", payload, got, want)
		}
	}
}

// The independent audit's finding 2: every occurrence of a repeated owned key was
// replaced, so 10 000 short "model" members became 10 000 long deployment names —
// 43.6× the client's body. The inbound stage refuses a repeated top-level member; the
// editor refuses one too, so the rewritten body never grows by more than one edit per
// owned field.
func TestPassthroughRefusesARepeatedOwnedKey(t *testing.T) {
	raw := []byte(`{` + strings.Repeat(`"model":"m",`, 10000) + `"messages":[]}`)
	req := &Request{Body: raw, Deployment: config.Deployment{Model: strings.Repeat("x", 512)}}
	if edited, _, err := passthroughBody(req, standardServiceTier(req.Endpoint)); err == nil {
		t.Fatalf("accepted: %d client bytes became %d", len(raw), len(edited))
	}
}

// The rewritten body is at most the client's body plus one full member per edit: each
// owned field is replaced or added once.
func TestRewrittenBodyIsBoundedByOneEditPerOwnedField(t *testing.T) {
	longModel := strings.Repeat("x", 512)
	params := []Param{{"max_tokens", []byte("1024")}, {"top_k", []byte("[1,2]")}, {"temperature", []byte("0.2")}}
	for _, body := range []string{
		`{"model":"m","messages":[]}`,
		`{"model":"m","max_tokens":1,"top_k":0,"temperature":1,"stream":true,"stream_options":{"include_usage":false}}`,
		`{"messages":[{"role":"user","content":"hi"}],"stream":true}`,
	} {
		req := &Request{Body: []byte(body), Deployment: config.Deployment{Model: longModel}, Params: params,
			Stream: strings.Contains(body, `"stream":true`)}
		edited, _, err := passthroughBody(req, standardServiceTier(req.Endpoint))
		if err != nil {
			t.Fatalf("%s: %v", body, err)
		}
		// Each edit adds at most `,"key":value` — and include_usage at most a new
		// stream_options object.
		bound := len(body) + len(`,"model":""`) + len(longModel) + len(`,"service_tier":"default"`) +
			len(`,"stream_options":{"include_usage":true}`)
		for _, p := range params {
			bound += len(`,"":`) + len(p.Key) + len(p.Value)
		}
		if len(edited) > bound {
			t.Errorf("%s: rewritten to %d bytes, bound %d", body, len(edited), bound)
		}
	}
}
