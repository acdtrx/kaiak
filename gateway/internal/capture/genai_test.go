package capture

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"kaiak/internal/provider"
)

// The shared pieces of the conventions' format, which every API's mapping reads
// through (docs/specs/GATEWAY.md, Request capture → Content: one message format).

// A base64 data: URL is read into its MIME type and its base64, as sent — its scheme
// and marker in any case; any other URL is not one.
func TestDataURL(t *testing.T) {
	for _, c := range []struct {
		url, mime, content string
		ok                 bool
	}{
		{"data:image/png;base64,iVBORw0K", "image/png", "iVBORw0K", true},
		{"DATA:Image/PNG;BASE64,iVBORw0K", "image/png", "iVBORw0K", true},
		{"data:image/png;name=cat.png;base64,iVBO", "image/png", "iVBO", true},
		{"data:text/plain; charset=utf-8;base64,aGk=", "text/plain", "aGk=", true},
		{"data:;base64,aGk=", "", "aGk=", true},
		{"data:image;base64,aGk=", "", "aGk=", true},
		{"data:image/png;base64,", "image/png", "", true},
		{"data:image/png;base64,a,b", "image/png", "a,b", true},
		{"data:text/plain,hello", "", "", false},
		{"data:image/png;base64", "", "", false},
		{"data:", "", "", false},
		{"dat", "", "", false},
		{"https://example.com/a.png", "", "", false},
		{"", "", "", false},
	} {
		mime, content, ok := dataURL(c.url)
		if mime != c.mime || content != c.content || ok != c.ok {
			t.Errorf("%q: %q %q %v, want %q %q %v", c.url, mime, content, ok, c.mime, c.content, c.ok)
		}
	}
}

// A MIME type is carried as its type and subtype, lowercase, without parameters; its
// modality is image, audio or video by its type, any other a document — and with no
// MIME type, the kind the API names.
func TestMIMETypeAndModality(t *testing.T) {
	for _, c := range []struct {
		in, mime, modality string
	}{
		{"image/png", "image/png", modalityImage},
		{" Image/WEBP ; q=1", "image/webp", modalityImage},
		{"audio/wav", "audio/wav", modalityAudio},
		{"video/mp4", "video/mp4", modalityVideo},
		{"application/pdf", "application/pdf", modalityDocument},
		{"text/plain", "text/plain", modalityDocument},
		{"image", "", "kind"},
		{"image/", "", "kind"},
		{"/png", "", "kind"},
		{"", "", "kind"},
	} {
		mime := mimeType(c.in)
		if got := modality(mime, "kind"); mime != c.mime || got != c.modality {
			t.Errorf("%q: MIME type %q, modality %q; want %q, %q", c.in, mime, got, c.mime, c.modality)
		}
	}
}

// Media by URL is a blob for a base64 data: URL, else a uri of the API's kind.
func TestMediaByURL(t *testing.T) {
	for _, c := range []struct {
		url  string
		want part
	}{
		{"data:audio/mpeg;base64,SUQz", newBlobPart(modalityAudio, "audio/mpeg", "SUQz")},
		{"data:;base64,SUQz", newBlobPart(modalityImage, "", "SUQz")},
		{"data:text/plain,hi", newURIPart(modalityImage, "", "data:text/plain,hi")},
		{"https://example.com/a.png", newURIPart(modalityImage, "", "https://example.com/a.png")},
	} {
		if got := newMediaByURL(c.url, modalityImage); got != c.want {
			t.Errorf("%q: %#v, want %#v", c.url, got, c.want)
		}
	}
}

// A tool call's arguments are the JSON a string holds when it is valid JSON, else the
// string; any other value is itself; none when absent or null. Written, the JSON is
// compact and the string unescaped.
func TestToolArguments(t *testing.T) {
	for _, c := range []struct {
		raw, want string
	}{
		{`"{\"city\": \"Oslo\"}"`, `{"city":"Oslo"}`},
		{`"[1, 2]"`, `[1,2]`},
		{`"42"`, `42`},
		{`"{\"city\": \"Os"`, `"{\"city\": \"Os"`},
		{`""`, `""`},
		{`"not json"`, `"not json"`},
		{`"<&>"`, `"<&>"`},
		{`{"city":"Oslo"}`, `{"city":"Oslo"}`},
		{`7`, `7`},
		{`null`, ``},
		{``, ``},
	} {
		if got := encoded(t, toolArguments(json.RawMessage(c.raw))); got != c.want {
			t.Errorf("%s: %s, want %s", c.raw, got, c.want)
		}
	}
}

// encoded is v as the content attributes write it: JSON without HTML escaping; ""
// for nil, which they leave out.
func encoded(t *testing.T, v any) string {
	t.Helper()
	if v == nil {
		return ""
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		t.Fatal(err)
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// The content attributes are JSON written as the client sent its text: <, > and &
// unescaped, and non-ASCII as itself; a message with no parts has an empty list.
func TestContentJSONIsNotHTMLEscaped(t *testing.T) {
	attrs := plainAttributes(t, contentAttributes(requestContent{messages: []message{
		newMessage("user", "", []part{newTextPart("if a < b && c > d: «ok»")}),
		newMessage("assistant", "", nil)}}, answerContent{}))
	want := `[{"role":"user","parts":[{"type":"text","content":"if a < b && c > d: «ok»"}]},{"role":"assistant","parts":[]}]`
	if got := attrs["gen_ai.input.messages"]; got != want {
		t.Errorf("input messages\n %s\nwant\n %s", got, want)
	}
}

// The finish reasons are one per generation the answer gave, error — the
// conventions' value for a generation that ended abnormally — for one that gave none;
// none for an answer with no generation.
func TestFinishReasons(t *testing.T) {
	one := []message{newMessage("assistant", "", nil)}
	two := append(slices.Clone(one), one...)
	for _, c := range []struct {
		an   answerContent
		want any
	}{
		{answerContent{messages: two, finishReasons: []string{"", "stop"}}, []string{"error", "stop"}},
		{answerContent{messages: two, finishReasons: []string{"", ""}}, []string{"error", "error"}},
		{answerContent{messages: one, finishReasons: []string{""}}, []string{"error"}},
		{answerContent{messages: one, finishReasons: []string{"length"}}, []string{"length"}},
		{answerContent{}, nil},
	} {
		got := plainAttributes(t, contentAttributes(requestContent{}, c.an))["gen_ai.response.finish_reasons"]
		if !slices.Equal(asStrings(got), asStrings(c.want)) || (got == nil) != (c.want == nil) {
			t.Errorf("finish reasons %v: %v, want %v", c.an.finishReasons, got, c.want)
		}
	}
}

func asStrings(v any) []string {
	l, _ := v.([]string)
	return l
}

// An event stream yields each event's data in order, whatever its line endings and
// comments; an event the stream ends inside is not yielded.
func TestStreamEvents(t *testing.T) {
	for _, c := range []struct {
		name, stream string
		want         []string
	}{
		{"LF", "data: a\n\ndata: b\n\n", []string{"a", "b"}},
		{"CRLF and CR", "data: a\r\n\r\ndata: b\r\r", []string{"a", "b"}},
		{"comments and named events", ": ping\n\nevent: x\ndata: a\n\nid: 1\n\n", []string{"a"}},
		{"data on two lines", "data: a\ndata: b\n\n", []string{"a\nb"}},
		{"cut inside an event", "data: a\n\ndata: {\"b\":", []string{"a"}},
		{"cut before the blank line", "data: a\n\ndata: b\n", []string{"a"}},
		{"empty", "", nil},
	} {
		var got []string
		for data := range streamEvents([]byte(c.stream)) {
			got = append(got, string(data))
		}
		if !slices.Equal(got, c.want) {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
	// A consumer that stops early stops the reading.
	n := 0
	for range streamEvents([]byte(strings.Repeat("data: x\n\n", 5))) {
		n++
		if n == 2 {
			break
		}
	}
	if n != 2 {
		t.Errorf("%d events read, want 2", n)
	}
}

// A tool the mapping does not know keeps its own JSON, named as the conventions
// require of every tool definition: by its own name; else the name its API gives it
// inside (Chat's custom.name), added; else its type, added. A tool whose name is not
// a string is left out.
func TestGenericTool(t *testing.T) {
	for _, c := range []struct{ raw, inner, want string }{
		{`{"type":"bash_20250124","name":"bash"}`, "", `{"type":"bash_20250124","name":"bash"}`},
		{`{"type":"x","name":""}`, "", `{"type":"x","name":""}`},
		{`{"type":"local_shell"}`, "", `{"type":"local_shell","name":"local_shell"}`},
		{`{"type":"custom","custom":{"name":"grep"}}`, "grep", `{"type":"custom","custom":{"name":"grep"},"name":"grep"}`},
		{"{ \"type\" : \"x<&>\" }\n", "", `{"type":"x<&>","name":"x<&>"}`},
		{`{"type":"x","name":5}`, "", ""},
		{`{"type":"x","name":null}`, "grep", ""},
	} {
		m, _ := members([]byte(c.raw))
		tool, ok := genericTool(json.RawMessage(c.raw), m, c.inner)
		got := ""
		if ok {
			got = encoded(t, tool)
		}
		if got != c.want {
			t.Errorf("%s: %s, want %s", c.raw, got, c.want)
		}
	}
}

// A part, block or item the mapping does not know is carried as a generic part —
// unless its type is one of the conventions' own part types, which it is not: read
// as one, it would lack that part's members, so it is left out.
func TestGenericPartNamedLikeTheConventions(t *testing.T) {
	for _, typ := range conventionPartTypes {
		raw := json.RawMessage(`{"type":"` + typ + `","text":"x"}`)
		if got := appendGeneric(nil, raw, typ); len(got) != 0 {
			t.Errorf("%s: %v, want it left out", typ, got)
		}
	}
	for _, typ := range []string{"x_custom", "compaction", "redacted_thinking", "input_audio"} {
		raw := json.RawMessage(`{"type":"` + typ + `"}`)
		if got := appendGeneric(nil, raw, typ); len(got) != 1 || encoded(t, got[0]) != string(raw) {
			t.Errorf("%s: %v, want it carried as sent", typ, got)
		}
	}
}

// No output is made up for a request that failed — its span has an error.type: an
// output message with no parts, a generation cut before its content arrived, is left
// out, and its finish reason, error, kept; one with parts stays: what the client
// received.
func TestNoOutputMadeUpForAFailedRequest(t *testing.T) {
	for _, c := range []struct {
		name     string
		api      provider.Endpoint
		response string
		want     map[string]any
	}{
		{"chat, cut after the role", provider.ChatCompletions,
			events(`{"id":"c","model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}`),
			map[string]any{"gen_ai.response.id": "c", "gen_ai.response.model": "m",
				"gen_ai.response.finish_reasons": []string{"error"}}},
		{"chat, two choices, one cut before its content", provider.ChatCompletions, events(
			`{"id":"c","choices":[{"index":0,"delta":{"role":"assistant","content":""}},{"index":1,"delta":{"role":"assistant","content":"B"}}]}`),
			map[string]any{"gen_ai.response.id": "c", "gen_ai.response.finish_reasons": []string{"error", "error"},
				"gen_ai.output.messages": `[{"role":"assistant","parts":[{"type":"text","content":"B"}]}]`}},
		{"messages, cut after message_start", provider.Messages,
			typedEvents(`{"type":"message_start","message":{"id":"msg","model":"m","content":[]}}`),
			map[string]any{"gen_ai.response.id": "msg", "gen_ai.response.model": "m",
				"gen_ai.response.finish_reasons": []string{"error"}}},
		{"responses, failed", provider.Responses, typedEvents(
			`{"type":"response.created","response":{"id":"r","model":"m","status":"in_progress","output":[]}}`,
			`{"type":"response.failed","response":{"id":"r","model":"m","status":"failed","output":[]}}`),
			map[string]any{"gen_ai.response.id": "r", "gen_ai.response.model": "m",
				"gen_ai.response.finish_reasons": []string{"error"}}},
		{"responses, cut with text", provider.Responses, typedEvents(
			`{"type":"response.created","response":{"id":"r","model":"m","status":"in_progress","output":[]}}`,
			`{"type":"response.output_item.added","item":{"id":"i","type":"message","role":"assistant","content":[]}}`,
			`{"type":"response.output_text.delta","item_id":"i","delta":"Part"}`),
			map[string]any{"gen_ai.response.id": "r", "gen_ai.response.model": "m",
				"gen_ai.response.finish_reasons": []string{"error"},
				"gen_ai.output.messages":         `[{"role":"assistant","parts":[{"type":"text","content":"Part"}]}]`}},
	} {
		got := plainAttributes(t, content(c.api, exchange{response: []byte(c.response), stream: true, status: 200,
			failed: true}))
		expectAttributes(t, c.name, got, c.want)
	}
}
