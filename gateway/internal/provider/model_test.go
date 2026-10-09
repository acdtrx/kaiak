package provider

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"kaiak/internal/config"
	"kaiak/internal/sse"
)

func TestModelRewriterEditsOnlyTopLevelModelStrings(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"chat answer", `{"id":"x","object":"chat.completion","created":1,"model":"Qwen/Qwen3-32B","choices":[]}`,
			`{"id":"x","object":"chat.completion","created":1,"model":"pub","choices":[]}`},
		{"layout kept", "{ \"model\" :\n  \"a\\\"b\\\\\" , \"x\": 1 }", "{ \"model\" :\n  \"pub\" , \"x\": 1 }"},
		{"after data (embeddings)", `{"object":"list","data":[{"embedding":[0.1,-2e-3],"model":"inner"}],"model":"m","usage":{}}`,
			`{"object":"list","data":[{"embedding":[0.1,-2e-3],"model":"inner"}],"model":"pub","usage":{}}`},
		{"nested model untouched", `{"a":{"model":"x"},"b":[{"model":"y"}]}`, `{"a":{"model":"x"},"b":[{"model":"y"}]}`},
		{"model as a value untouched", `{"k":"model","v":["model",":"]}`, `{"k":"model","v":["model",":"]}`},
		{"escaped key matches", `{"mod\u0065l":"x"}`, `{"mod\u0065l":"pub"}`},
		{"other case does not", `{"Model":"x"}`, `{"Model":"x"}`},
		{"non-string value untouched", `{"model":null,"n":{"model":1}}`, `{"model":null,"n":{"model":1}}`},
		{"every duplicate", `{"model":"a","x":"model","model":"b"}`, `{"model":"pub","x":"model","model":"pub"}`},
		{"key with braces in strings", `{"s":"{\"model\":\"q\"}","model":"a"}`, `{"s":"{\"model\":\"q\"}","model":"pub"}`},
		{"top-level array", `["model","x"]`, `["model","x"]`},
		{"error body", `{"error":{"message":"m","model":"x"}}`, `{"error":{"message":"m","model":"x"}}`},
		{"not json", `plain text "model": "x"`, `plain text "model": "x"`},
		{"long key", `{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa":"x","model":"y"}`,
			`{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa":"x","model":"pub"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Any split of the input into pieces gives the same output.
			for cut := 0; cut <= len(c.in); cut++ {
				m := newModelRewriter([]byte(`"pub"`))
				out := m.rewrite(nil, []byte(c.in[:cut]))
				out = m.rewrite(out, []byte(c.in[cut:]))
				if string(out) != c.want {
					t.Fatalf("cut at %d:\ngot  %s\nwant %s", cut, out, c.want)
				}
			}
			m := newModelRewriter([]byte(`"pub"`))
			var out []byte
			for i := range len(c.in) {
				out = m.rewrite(out, []byte{c.in[i]})
			}
			if string(out) != c.want {
				t.Errorf("byte by byte:\ngot  %s\nwant %s", out, c.want)
			}
		})
	}
}

// With a nested member, the model of that member's object is rewritten too — a
// Messages message_start's message.model — and no other nested one.
func TestNestedModelRewriter(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"message_start", `{"type":"message_start","message":{"id":"m","model":"back","usage":{"model":"x"}}}`,
			`{"type":"message_start","message":{"id":"m","model":"pub","usage":{"model":"x"}}}`},
		{"top level too", `{"model":"a","message":{"model":"b"}}`, `{"model":"pub","message":{"model":"pub"}}`},
		{"other members untouched", `{"other":{"model":"x"},"message":{"content":[{"model":"y"}],"model":"z"}}`,
			`{"other":{"model":"x"},"message":{"content":[{"model":"y"}],"model":"pub"}}`},
		{"nested not an object", `{"message":"model","x":{"model":"y"}}`, `{"message":"model","x":{"model":"y"}}`},
		{"nested key as a value", `{"k":"message","v":{"model":"y"}}`, `{"k":"message","v":{"model":"y"}}`},
		{"escaped nested key", `{"m\u0065ssage":{"model":"b"}}`, `{"m\u0065ssage":{"model":"pub"}}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for cut := 0; cut <= len(c.in); cut++ {
				m := newNestedModelRewriter([]byte(`"pub"`), "message")
				out := m.rewrite(nil, []byte(c.in[:cut]))
				out = m.rewrite(out, []byte(c.in[cut:]))
				if string(out) != c.want {
					t.Fatalf("cut at %d:\ngot  %s\nwant %s", cut, out, c.want)
				}
			}
		})
	}
}

// A speech batch answer's errored results carry the gateway's text in place of the
// backend's: every error member of each object in the top-level results list, whatever
// it holds that can carry text — a string, an object, an array — and every one of a
// result naming it twice; a null, a number or a boolean passes, and so does an error
// anywhere else. The top-level model is rewritten as in any body, in the same pass;
// everything else passes as sent (docs/specs/GATEWAY.md, Providers: media answers).
func TestSpeechBatchRewriterReplacesEachResultsError(t *testing.T) {
	const bs = `\`
	escapeAll := func(name string) string {
		var b strings.Builder
		for _, r := range name {
			fmt.Fprintf(&b, "%su%04x", bs, r)
		}
		return b.String()
	}
	for _, c := range []struct{ name, in, want string }{
		{"an errored result", `{"id":"b","results":[{"index":0,"status":"success","audio_data":"UklGR","media_type":"audio/wav"},` +
			`{"index":1,"status":"error","error":"CUDA error on gpu-3.internal"}],"total":2,"succeeded":1,"failed":1}`,
			`{"id":"b","results":[{"index":0,"status":"success","audio_data":"UklGR","media_type":"audio/wav"},` +
				`{"index":1,"status":"error","error":"G"}],"total":2,"succeeded":1,"failed":1}`},
		{"every result", `{"results":[{"index":0,"error":"a"},{"index":1,"error":"b"},{"index":2,"error":"c"}]}`,
			`{"results":[{"index":0,"error":"G"},{"index":1,"error":"G"},{"index":2,"error":"G"}]}`},
		{"named twice", `{"results":[{"error":"a","index":0,"error":"b"}]}`, `{"results":[{"error":"G","index":0,"error":"G"}]}`},
		{"an object", `{"results":[{"error":{"message":"host gpu-3","trace":["}]\"{",{"x":[1]}]},"index":0}]}`,
			`{"results":[{"error":"G","index":0}]}`},
		{"an array", `{"results":[{"index":0,"error":["a",{"b":"]"}]}]}`, `{"results":[{"index":0,"error":"G"}]}`},
		{"quotes, braces and escapes in the text", `{"results":[{"error":"a\"]}{,\\","index":0}]}`,
			`{"results":[{"error":"G","index":0}]}`},
		{"null, a number and a boolean pass", `{"results":[{"error":null},{"error":500},{"error":false}]}`,
			`{"results":[{"error":null},{"error":500},{"error":false}]}`},
		{"an escaped name", `{"results":[{"` + bs + `u0065rror":"x"}]}`, `{"results":[{"` + bs + `u0065rror":"G"}]}`},
		{"an escaped list name", `{"r` + bs + `u0065sults":[{"error":"x"}]}`, `{"r` + bs + `u0065sults":[{"error":"G"}]}`},
		{"layout kept", "{ \"results\" :\n [ { \"error\" :\t\"x\" } ,{\"error\":\"y\"}] }",
			"{ \"results\" :\n [ { \"error\" :\t\"G\" } ,{\"error\":\"G\"}] }"},
		{"results named twice", `{"results":[{"error":"a"}],"results":[{"error":"b"}]}`,
			`{"results":[{"error":"G"}],"results":[{"error":"G"}]}`},
		{"the top-level model rewritten, a result's kept", `{"model":"back","results":[{"model":"x","error":"e"}]}`,
			`{"model":"pub","results":[{"model":"x","error":"G"}]}`},
		{"an error elsewhere untouched", `{"error":"t","meta":{"results":[{"error":"m"}]},"other":[{"error":"o"}],` +
			`"results":[{"usage":{"error":"u"},"index":0},[{"error":"n"}],"error"]}`,
			`{"error":"t","meta":{"results":[{"error":"m"}]},"other":[{"error":"o"}],` +
				`"results":[{"usage":{"error":"u"},"index":0},[{"error":"n"}],"error"]}`},
		{"results not a list", `{"results":{"error":"x"}}`, `{"results":{"error":"x"}}`},
		{"error as a value", `{"results":[{"status":"error","x":["error",":"]}]}`, `{"results":[{"status":"error","x":["error",":"]}]}`},
		{"every character escaped", `{"` + escapeAll("results") + `":[{"` + escapeAll("error") + `":"x"}]}`,
			`{"` + escapeAll("results") + `":[{"` + escapeAll("error") + `":"G"}]}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			for cut := 0; cut <= len(c.in); cut++ {
				m := newSpeechBatchRewriter([]byte(`"pub"`), []byte(`"G"`))
				out := m.rewrite(nil, []byte(c.in[:cut]))
				out = m.rewrite(out, []byte(c.in[cut:]))
				if string(out) != c.want {
					t.Fatalf("cut at %d:\ngot  %s\nwant %s", cut, out, c.want)
				}
				if !m.closed() {
					t.Fatalf("cut at %d: the answer did not close", cut)
				}
			}
			m := newSpeechBatchRewriter([]byte(`"pub"`), []byte(`"G"`))
			var out []byte
			for i := range len(c.in) {
				out = m.rewrite(out, []byte{c.in[i]})
			}
			if string(out) != c.want {
				t.Errorf("byte by byte:\ngot  %s\nwant %s", out, c.want)
			}
		})
	}
}

// The model name nested in a format's envelope events is rewritten only there: an
// unknown event passes untouched.
func TestUnknownEventsKeepTheirNestedModel(t *testing.T) {
	for _, format := range []string{"messages", "responses"} {
		t.Run(format, func(t *testing.T) {
			unknown := "event: vendor.extension\ndata: {\"type\":\"vendor.extension\",\"message\":{\"model\":\"payload-value\"},\"response\":{\"model\":\"payload-value\"}}\n\n"
			var out string
			var err error
			if format == "messages" {
				out, err = relayMessages(t, config.BackendVLLM, "text/event-stream", []byte(unknown+"data: {\"type\":\"message_stop\"}\n\n"), true)
			} else {
				out, err = relayResponses(t, config.BackendVLLM, "text/event-stream", []byte(unknown+"data: {\"type\":\"response.completed\"}\n\n"), true)
			}
			if !errors.Is(err, io.EOF) {
				t.Fatal(err)
			}
			if !strings.HasPrefix(out, unknown) {
				t.Errorf("unknown event's nested payload was rewritten: %s", out)
			}
		})
	}
}

func TestChunkModelRewriteKeepsTheEventFraming(t *testing.T) {
	r := &upstreamResponse{publicModel: []byte(`"pub"`), format: &openAIStream{}}
	for _, c := range []struct{ in, want string }{
		{"data: {\"id\":1,\"model\":\"back\"}\n\n", "data: {\"id\":1,\"model\":\"pub\"}\n\n"},
		{"event: x\r\ndata:{\"model\":\r\ndata:  \"back\",\"a\":1}\r\nid: 3\r\n\r\n",
			// The reader leaves the final LF of CRLF to the next block.
			"event: x\r\ndata:{\"model\":\r\ndata:  \"pub\",\"a\":1}\r\nid: 3\r\n\r"},
		{"data: [DONE]\n\n", "data: [DONE]\n\n"},
	} {
		block, err := sse.NewReader(strings.NewReader(c.in), maxSSEBlockBytes).Next()
		if err != nil {
			t.Fatal(err)
		}
		if got := string(r.rewriteChunkModel(block)); got != c.want {
			t.Errorf("got %q, want %q", got, c.want)
		}
	}
}
