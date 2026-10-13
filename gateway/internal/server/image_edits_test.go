package server

import (
	"bytes"
	"context"
	"io"
	"maps"
	"mime"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"kaiak/internal/accounting"
	"kaiak/internal/config"
	"kaiak/internal/fakebackend"
	"kaiak/internal/provider"
)

// editCall is an image edit of the eval group's key carrying form, a form body under
// testBoundary, request ID id.
func editCall(form, id string) call {
	return call{method: "POST", path: "/v1/images/edits", key: workloadKey, body: form,
		header: map[string]string{"X-Request-Id": id, "Content-Type": formContentType}}
}

// editPart is a part as mime/multipart reads it: its name, its filename when its
// Content-Disposition names one, its Content-Type when it has one, its bytes.
type editPart struct {
	name        string
	file        bool
	filename    string
	contentType *string
	body        string
}

// formParts reads a multipart/form-data body as mime/multipart does, under the
// boundary contentType names; boundary is that boundary.
func formParts(t *testing.T, contentType string, body []byte) (parts []editPart, boundary string) {
	t.Helper()
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil || mediaType != "multipart/form-data" || params["boundary"] == "" {
		t.Fatalf("Content-Type %q: not a form with a boundary (%v)", contentType, err)
	}
	reader := multipart.NewReader(bytes.NewReader(body), params["boundary"])
	for {
		part, err := reader.NextRawPart()
		if err == io.EOF {
			return parts, params["boundary"]
		}
		if err != nil {
			t.Fatalf("form: %v", err)
		}
		data, err := io.ReadAll(part)
		if err != nil {
			t.Fatalf("part: %v", err)
		}
		_, disposition, err := mime.ParseMediaType(part.Header.Get("Content-Disposition"))
		if err != nil {
			t.Fatalf("Content-Disposition: %v", err)
		}
		p := editPart{name: disposition["name"], body: string(data)}
		p.filename, p.file = disposition["filename"]
		if values, ok := part.Header["Content-Type"]; ok {
			p.contentType = &values[0]
		}
		parts = append(parts, p)
	}
}

// expectRebuilt fails the test unless the backend's request is the client's form
// rebuilt: sent with its exact length under a boundary of the gateway's, every part
// in order with its name, filename, content type and bytes, model's value model.
func expectRebuilt(t *testing.T, sent *fakebackend.Request, client, model string) {
	t.Helper()
	got, boundary := formParts(t, sent.Header.Get("Content-Type"), sent.Body)
	want, _ := formParts(t, formContentType, []byte(client))
	if boundary == testBoundary {
		t.Error("the backend got the client's boundary, want one of the gateway's")
	}
	if length := sent.Header.Get("Content-Length"); length != strconv.Itoa(len(sent.Body)) {
		t.Errorf("Content-Length %q, the body is %d bytes", length, len(sent.Body))
	}
	for i := range want {
		if want[i].name == "model" {
			want[i].body = model
		}
	}
	if len(got) != len(want) {
		t.Fatalf("the backend got %d parts, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		g := got[i]
		if g.name != w.name || g.file != w.file || g.filename != w.filename || g.body != w.body ||
			(g.contentType == nil) != (w.contentType == nil) || w.contentType != nil && *g.contentType != *w.contentType {
			t.Errorf("part %d: %+v, want %+v", i, g, w)
		}
	}
}

// pngData is an image file part's bytes: a PNG signature, then every byte value, CR
// and LF among them.
var pngData = func() string {
	var b strings.Builder
	b.WriteString("\x89PNG\r\n\x1a\n")
	for c := range 256 {
		b.WriteByte(byte(c))
	}
	return b.String()
}()

// An image edit passes the whole pipeline to a vllm-omni deployment: its form rebuilt
// with the deployment's model — every other part as the client wrote it, an unknown
// one, OpenAI's mask and a stream part saying false included — answered with the
// backend's JSON as sent, never a stream; settled with the answer's images, the prompt
// and the three input images estimated and flagged, no output — the token limits
// reserving that estimate alone; operation generate_content and output type image on
// the request line and the usage metrics, never timed to a first token
// (docs/specs/GATEWAY.md, Client API → owned parts: image edits; Request pipeline →
// Uploads; Limits → The input estimate; Accounting → Media usage; Observability).
func TestImageEditsThroughThePipeline(t *testing.T) {
	g := newTestGateway(t)
	withSpeechModels(t, g, func(doc string) string {
		return replaceOnce(t, doc, `"research": { "capture": false }`, `"research": { "capture": false, "limits": [{ "type": "tokens_per_minute", "value": 100000 }] }`)
	})
	g.backend.SetReply(fakebackend.Reply{ImageBytes: 2000})
	const prompt = "a red fox in snow"
	form := formOf(textPart("model", "painter"), textPart("prompt", prompt),
		filePart("image", "fox one.png", "image/png", pngData), filePart("image", "fox ünï.png", "image/png", pngData),
		textPart("url[]", "data:image/png;base64,iVBORw0KGgo="), filePart("mask", "m.png", "image/png", pngData),
		textPart("n", "2"), textPart("size", "512x512"), textPart("stream", "False"), textPart("response_format", "b64_json"),
		textPart("negative_prompt", "blur"), textPart("x_custom", "kept"))
	w := do(t, g.h, editCall(form, "edits-1"))
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("%d %s: %.300s", w.Code, w.Header().Get("Content-Type"), w.Body.String())
	}
	if images := answeredImages(t, w.Body.Bytes()); len(images) != 2 || len(images[1]) != 2000 ||
		!strings.HasPrefix(string(images[0]), "\x89PNG") {
		t.Errorf("%d images: want two PNG images of 2000 bytes", len(images))
	}
	if w.Header().Get("X-Accel-Buffering") != "" {
		t.Error("an image edit's answer was flushed as it arrived")
	}

	reqs := g.backend.Requests()
	if len(reqs) != 1 || reqs[0].Path != "/v1/images/edits" {
		t.Fatalf("backend requests %+v, want one to /v1/images/edits", reqs)
	}
	expectRebuilt(t, reqs[0], form, "Qwen/Qwen-Image")

	rec := onlyRecord(t, g)
	estimate := accounting.EstimateUploadInput(int64(len(prompt)), 3).Total
	expectUnits(t, rec, imageUnits(estimate, 2), true, false)
	if estimate != accounting.EstimateTokens(int64(len(prompt)))+3*accounting.InlineMediaTokens {
		t.Errorf("estimate %d, want the prompt's bytes and three media items", estimate)
	}
	if got := w.Header().Get("x-ratelimit-remaining-tokens"); got != strconv.FormatInt(100000-estimate, 10) {
		t.Errorf("remaining tokens at admission %s, want the input estimate of %d reserved alone", got, estimate)
	}
	if rec.Operation != "generate_content" || rec.OutputType != "image" || rec.CostNanoUSD != 0 {
		t.Errorf("record operation %q, output type %q, cost %d; want generate_content, image, 0", rec.Operation,
			rec.OutputType, rec.CostNanoUSD)
	}
	line := logLine(t, g, "edits-1")
	for _, want := range []string{`"gen_ai.operation.name":"generate_content"`, `"gen_ai.output.type":"image"`,
		`"gen_ai.request.stream":false`, `"kaiak.backend.type":"vllm-omni"`, `"kaiak.usage.images":2`, `"kaiak.usage.estimated":true`} {
		if !strings.Contains(line, want) {
			t.Errorf("log line lacks %s: %s", want, line)
		}
	}
	usage := `{kaiak_key_group="eval",kaiak_key_root_group="research",kaiak_key_id="k-eval",gen_ai_request_model="painter",` +
		`gen_ai_operation_name="generate_content",gen_ai_output_type="image",gen_ai_provider_name="vllm-omni",kaiak_usage_status="complete"}`
	text := g.metricsText()
	expectMetricLines(t, text,
		`http_server_request_duration_seconds_count{http_request_method="POST",url_scheme="http",http_route="/v1/images/edits",`+
			`http_response_status_code="200",gen_ai_request_model="painter"} 1`,
		`kaiak_usage_records_total`+usage+` 1`,
		`kaiak_usage_images_total`+usage+` 2`)
	if strings.Contains(text, `kaiak_time_to_first_token_seconds_count{gen_ai_request_model="painter"`) {
		t.Errorf("an image edit was timed to a first token")
	}
}

// What the gateway refuses on /v1/images/edits it refuses before routing, in OpenAI's
// error shape, naming the part at fault and never echoing its value: more images than
// max_images_per_request; an n that is not a whole number in decimal; a
// response_format other than b64_json; a stream part other than false; lora, whatever
// it holds; url, url[], mask_image and reference_image given as text that is not a
// data: URL — a URL, a path, another scheme; an owned text part sent as a file; a
// single-valued part named twice; no model; OpenAI's JSON form of edits; a model whose
// endpoints list leaves images_edits out (docs/specs/GATEWAY.md, Client API → owned
// parts: image edits; errors; Request pipeline → Uploads).
func TestImageEditsRefusals(t *testing.T) {
	g := newTestGateway(t)
	withSpeechModels(t, g, func(doc string) string {
		return replaceOnce(t, doc, `"painter": {`, `"painter-gen": { "deployments": [{ "backend": "omni", "model": "Qwen/Qwen-Image" }],
      "endpoints": ["images_generations"],
      "metadata": { "context_length": 8192,
        "capabilities": { "streaming": true, "tools": false, "vision": false, "reasoning": false } } },
    "painter": {`)
	})
	edit := func(parts ...rawPart) string {
		return formOf(append([]rawPart{textPart("model", "painter"), textPart("prompt", "a fox"),
			filePart("image", "a.png", "image/png", "\x89PNG")}, parts...)...)
	}
	cases := []struct {
		name, form, code, param string
	}{
		{"more images than the cap", edit(textPart("n", "11")), "invalid_value", "n"},
		{"n a fraction", edit(textPart("n", "2.5")), "invalid_type", "n"},
		{"n a word", edit(textPart("n", "two")), "invalid_type", "n"},
		{"n empty", edit(textPart("n", "")), "invalid_type", "n"},
		{"n spaced", edit(textPart("n", " 2")), "invalid_type", "n"},
		{"n in hex", edit(textPart("n", "0x2")), "invalid_type", "n"},
		{"n with an underscore", edit(textPart("n", "1_0")), "invalid_type", "n"},
		{"n past int64", edit(textPart("n", "99999999999999999999")), "invalid_type", "n"},
		{"n a file", edit(filePart("n", "n.txt", "text/plain", "2")), "invalid_type", "n"},
		{"url format", edit(textPart("response_format", "url")), "response_format_unsupported", "response_format"},
		{"file format", edit(textPart("response_format", "file")), "response_format_unsupported", "response_format"},
		{"another format", edit(textPart("response_format", "B64_JSON")), "response_format_unsupported", "response_format"},
		{"response_format a file", edit(filePart("response_format", "f.txt", "text/plain", "b64_json")), "invalid_type",
			"response_format"},
		{"lora", edit(textPart("lora", `{"name":"style","path":"/srv/adapters/style","scale":1}`)), "external_reference_unsupported",
			"lora"},
		{"lora empty", edit(textPart("lora", "")), "external_reference_unsupported", "lora"},
		{"lora a file", edit(filePart("lora", "l.json", "application/json", `{"path":"/srv/adapters/style"}`)),
			"external_reference_unsupported", "lora"},
		{"stream a file", edit(filePart("stream", "s.txt", "text/plain", "false")), "invalid_type", "stream"},
		{"n named twice", edit(textPart("n", "1"), textPart("n", "9")), "duplicate_member", "n"},
		{"prompt named twice", edit(textPart("prompt", "a cat")), "duplicate_member", "prompt"},
		{"mask_image named twice", edit(filePart("mask_image", "m.png", "image/png", "x"), filePart("mask_image", "n.png",
			"image/png", "y")), "duplicate_member", "mask_image"},
		{"model missing", formOf(textPart("prompt", "a fox"), filePart("image", "a.png", "image/png", "x")),
			"missing_required_parameter", "model"},
		{"not listed on a vllm-omni model", formOf(textPart("model", "painter-gen"), textPart("prompt", "a fox"),
			filePart("image", "a.png", "image/png", "x")), "endpoint_not_served", ""},
		{"not on vllm-omni", formOf(textPart("model", "open"), textPart("prompt", "a fox"),
			filePart("image", "a.png", "image/png", "x")), "endpoint_not_served", ""},
	}
	for _, value := range []string{"true", "TRUE", "1", "on", "yes", ""} {
		cases = append(cases, struct{ name, form, code, param string }{"stream " + strconv.Quote(value),
			edit(textPart("stream", value)), "invalid_value", "stream"})
	}
	for _, name := range []string{"url", "url[]", "mask_image", "reference_image"} {
		for _, value := range []string{"https://example.com/srv/a.png", "http://10.0.0.1/a.png", "/srv/images/a.png",
			"file:///srv/images/a.png", "s3://bucket/a.png", " data:image/png;base64,iVBO"} {
			cases = append(cases, struct{ name, form, code, param string }{name + " " + value,
				edit(textPart(name, value)), "external_reference_unsupported", name})
		}
	}
	cases = append(cases, struct{ name, form, code, param string }{"a url[] after a data URL",
		edit(textPart("url[]", "data:image/png;base64,iVBO"), textPart("url[]", "https://example.com/srv/b.png")),
		"external_reference_unsupported", "url[]"})
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := do(t, g.h, editCall(c.form, "refused"))
			expectError(t, w, http.StatusBadRequest, c.code)
			if _, _, param := openAIError(t, w); (param == nil && c.param != "") || (param != nil && *param != c.param) {
				t.Errorf("param %v, want %q: %s", param, c.param, w.Body.String())
			}
			msg := errorMessage(t, w)
			for _, value := range []string{"/srv/", "example.com", "10.0.0.1", "bucket", "iVBO", "B64_JSON", "0x2", "1_0", "two"} {
				if strings.Contains(msg, value) {
					t.Errorf("message %q echoes the value", msg)
				}
			}
			if c.code == "endpoint_not_served" && errorMessage(t, w) != notListed("/v1/images/edits") {
				t.Errorf("message %q, want the list's refusal", errorMessage(t, w))
			}
		})
	}

	// OpenAI's JSON form of edits is refused before its body is read.
	w := do(t, g.h, call{method: "POST", path: "/v1/images/edits", key: workloadKey,
		body:   `{"model":"painter","prompt":"a fox","images":[{"image_url":"data:image/png;base64,iVBO"}]}`,
		header: map[string]string{"Content-Type": "application/json"}})
	expectError(t, w, http.StatusBadRequest, "unsupported_content_type")

	if n := len(g.backend.Requests()); n != 0 {
		t.Errorf("%d requests reached the backend, want none", n)
	}
	if n := len(g.usage.all()); n != 0 {
		t.Errorf("%d usage records, want none: nothing was routed", n)
	}

	// The cap is the config's.
	g = newTestGateway(t)
	withSpeechModels(t, g, withGlobal(t, `"max_images_per_request": 2`))
	w = do(t, g.h, editCall(edit(textPart("n", "3")), "over"))
	expectError(t, w, http.StatusBadRequest, "invalid_value")
	if msg := errorMessage(t, w); !strings.Contains(msg, "'n' asks for 3 images; the gateway's maximum is 2") {
		t.Errorf("message %q", msg)
	}
	if w := do(t, g.h, editCall(edit(textPart("n", "2")), "at")); w.Code != http.StatusOK {
		t.Errorf("two images under a cap of 2: %d %s", w.Code, w.Body.String())
	}
}

// What an image edit's owned parts allow passes untouched, rebuilt as the client wrote
// it: url, url[], mask_image and reference_image as data: URLs, the scheme in any
// case, or — mask_image and reference_image — as file parts; image and image[] file
// parts, never inspected, image[] and url[] repeated; a stream part saying false in
// any case; parts the gateway does not own, extra_params among them (vLLM-Omni's edits
// take none) (docs/specs/GATEWAY.md, Client API → owned parts: image edits).
func TestImageEditsPassWhatTheyAllow(t *testing.T) {
	g := newTestGateway(t)
	withSpeechModels(t, g)
	for _, form := range []string{
		formOf(textPart("model", "painter"), textPart("prompt", "a fox"), textPart("url", "data:image/png;base64,iVBO"),
			textPart("mask_image", "DATA:image/png;base64,iVBO"), textPart("reference_image", "Data:image/png;base64,iVBO"),
			textPart("stream", "false")),
		formOf(textPart("model", "painter"), textPart("prompt", "a fox"), filePart("image[]", "a.png", "image/png", pngData),
			filePart("image[]", "b.png", "image/webp", "RIFF"), textPart("url[]", "data:image/png;base64,iVBO"),
			textPart("url[]", "data:image/jpeg;base64,/9j/"), filePart("mask_image", "m.png", "image/png", pngData),
			filePart("reference_image", "r.png", "", "x"), textPart("stream", "FALSE"),
			textPart("extra_params", `{"control_image":"/srv/a.png"}`), textPart("layers", "2")),
	} {
		if w := do(t, g.h, editCall(form, "pass")); w.Code != http.StatusOK || w.Header().Get("Content-Type") != "application/json" {
			t.Errorf("%q: %d %s", form, w.Code, w.Body.String())
			continue
		}
		reqs := g.backend.Requests()
		expectRebuilt(t, reqs[len(reqs)-1], form, "Qwen/Qwen-Image")
	}
}

// An edit reserves the images its n asks for — absent 1, vLLM-Omni's default; below 1
// one, for the backend to refuse — its meter is told the same, it is never a stream,
// and its input estimate is its prompt's text and each input image: an image or
// image[] file part, a url or url[] data URL — not a mask, a reference image or an
// image sent as text (docs/specs/GATEWAY.md, Client API → owned parts: image edits;
// Limits → Check and reserve, The input estimate).
func TestImageEditsReserveTheirN(t *testing.T) {
	ep := bodyEndpoint(provider.ImagesEdits)
	for n, want := range map[string]int64{"": 1, "0": 1, "-3": 1, "+2": 2, "1": 1, "4": 4, "010": 10} {
		parts := []rawPart{textPart("model", "painter"), textPart("prompt", "a red fox"), filePart("image", "a.png", "image/png", "x")}
		if n != "" {
			parts = append(parts, textPart("n", n))
		}
		uploads := testUploads(DefaultUploadMemory)
		rq, _, apiErr := readTestUpload(ep, uploadRequest(strings.NewReader(formOf(parts...)), formContentType), uploadSnapshot(t, 0),
			NewBodyBudget(DefaultBodyMemory), uploads)
		if apiErr != nil {
			t.Fatalf("n %q: refused %+v", n, apiErr)
		}
		if got := rq.inbound.Media; !maps.Equal(got, map[config.Measure]int64{config.MeasureImages: want}) ||
			rq.inbound.mediaRequest().Images != want || rq.inbound.Stream {
			t.Errorf("n %q: media %v, the meter told %d, stream %v; want %d images, not a stream", n, got,
				rq.inbound.mediaRequest().Images, rq.inbound.Stream, want)
		}
		rq.finish()
	}

	for _, c := range []struct {
		name   string
		parts  []rawPart
		inputs int64
	}{
		{"no input image", nil, 0},
		{"image files", []rawPart{filePart("image", "a.png", "image/png", "x"), filePart("image", "b.png", "image/png", "y")}, 2},
		{"image[] files and url[] data", []rawPart{filePart("image[]", "a.png", "image/png", "x"),
			textPart("url[]", "data:image/png;base64,AA"), textPart("url[]", "data:image/png;base64,BB")}, 3},
		{"a url data URL", []rawPart{textPart("url", "data:image/png;base64,AA")}, 1},
		{"masks and references are not input images", []rawPart{filePart("mask_image", "m.png", "image/png", "x"),
			textPart("reference_image", "data:image/png;base64,AA"), filePart("mask", "m.png", "image/png", "x")}, 0},
		{"an image as text is not one", []rawPart{textPart("image", "data:image/png;base64,AA"), filePart("url", "u.png", "image/png", "x")}, 0},
	} {
		const prompt = "a red fox in the snow at dusk"
		parts := append([]rawPart{textPart("model", "painter"), textPart("prompt", prompt)}, c.parts...)
		rq, _, apiErr := readTestUpload(ep, uploadRequest(strings.NewReader(formOf(parts...)), formContentType), uploadSnapshot(t, 0),
			NewBodyBudget(DefaultBodyMemory), testUploads(DefaultUploadMemory))
		if apiErr != nil {
			t.Fatalf("%s: refused %+v", c.name, apiErr)
		}
		if want := accounting.EstimateUploadInput(int64(len(prompt)), c.inputs); rq.input != want {
			t.Errorf("%s: estimate %+v, want %+v: the prompt and %d input images", c.name, rq.input, want, c.inputs)
		}
		rq.finish()
	}
}

// The image limits reserve an edit's n before it runs and settle to the answer's
// images: a layered answer, more than n, takes the window past its value and the next
// edit is refused, 429 images. Chat and speech, which count no images, pass
// (docs/specs/GATEWAY.md, Limits → Check and reserve, Settle).
func TestImageEditLimits(t *testing.T) {
	g := newTestGateway(t)
	withSpeechModels(t, g, func(doc string) string {
		return replaceOnce(t, doc, `"research": { "capture": false }`, `"research": { "capture": false, "limits": [{ "type": "images_per_minute", "value": 5 }] }`)
	})
	edit := func(parts ...rawPart) string {
		return formOf(append([]rawPart{textPart("model", "painter"), textPart("prompt", "a fox"),
			filePart("image", "a.png", "image/png", "x")}, parts...)...)
	}
	if w := do(t, g.h, editCall(edit(textPart("n", "3")), "three")); w.Code != http.StatusOK {
		t.Fatalf("3 images under 5: %d %s", w.Code, w.Body.String())
	}
	if used := counterUsed(t, g, "research", config.LimitImagesPerMinute); used != 3 {
		t.Errorf("window holds %d images, want 3", used)
	}
	w := do(t, g.h, editCall(edit(textPart("n", "3")), "refused"))
	expectError(t, w, http.StatusTooManyRequests, "rate_limit_exceeded")
	if typ, _, _ := openAIError(t, w); typ != "images" || w.Header().Get("Retry-After") == "" {
		t.Errorf("type %q, Retry-After %q: want images and a wait", typ, w.Header().Get("Retry-After"))
	}
	if !strings.Contains(errorMessage(t, w), "(used 3, requested 3)") {
		t.Errorf("message %q", errorMessage(t, w))
	}

	// A layered answer: n 1 reserved, three layers settled.
	if w := do(t, g.h, editCall(edit(textPart("n", "1"), textPart("layers", "3")), "layered")); w.Code != http.StatusOK ||
		len(answeredImages(t, w.Body.Bytes())) != 3 {
		t.Fatalf("a layered answer under 2 left: %d %.200s, want 200 and three images", w.Code, w.Body.String())
	}
	if used := counterUsed(t, g, "research", config.LimitImagesPerMinute); used != 6 {
		t.Errorf("window holds %d images, want 6: the layered answer's 3, past the limit", used)
	}
	if rec := g.usage.all(); len(rec) != 2 || rec[1].Units[config.UnitImages] != 3 {
		t.Errorf("records %+v, want the layered answer's settled at 3 images", rec)
	}
	expectError(t, do(t, g.h, editCall(edit(), "full")), http.StatusTooManyRequests, "rate_limit_exceeded")
	expectError(t, do(t, g.h, imagesCall(`{"model":"painter","prompt":"a fox"}`, "generation")), http.StatusTooManyRequests,
		"rate_limit_exceeded")
	for _, c := range []call{{method: "POST", path: "/v1/chat/completions", key: workloadKey, body: chatBody},
		speechCall(`{"model":"tts","input":"hi"}`, "speech")} {
		if w := do(t, g.h, c); w.Code != http.StatusOK {
			t.Errorf("%s refused by the image limit: %d %s", c.path, w.Code, w.Body.String())
		}
	}
}

// An edit's answer cut short after its first bytes settles the larger of the images
// relayed and the n asked for, flagged partial and estimated; the client's connection
// is cut (docs/specs/GATEWAY.md, Accounting → Partial).
func TestImageEditsCutShort(t *testing.T) {
	g := newTestGateway(t)
	withSpeechModels(t, g)
	g.backend.SetReply(fakebackend.Reply{Fault: &fakebackend.StreamFault{At: 0, Kind: fakebackend.Cut}})
	const prompt = "a fox"
	form := formOf(textPart("model", "painter"), textPart("prompt", prompt), filePart("image", "a.png", "image/png", "x"),
		textPart("n", "3"))
	req, err := http.NewRequestWithContext(context.Background(), "POST", serveGateway(t, g)+"/v1/images/edits",
		strings.NewReader(form))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+workloadKey)
	req.Header.Set("Content-Type", formContentType)
	if resp, err := http.DefaultClient.Do(req); err == nil {
		_, err = io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || err == nil {
			t.Errorf("status %d, read error %v: want the answer cut short", resp.StatusCode, err)
		}
	}
	expectUnits(t, settledRecord(t, g), imageUnits(accounting.EstimateUploadInput(int64(len(prompt)), 1).Total, 3), true, true)
}

// vLLM-Omni's 400 "Model mismatch" on an edit, naming the deployment's model, is the
// model missing there: 502 upstream_model_missing, the backend's text not relayed, no
// images settled (docs/specs/GATEWAY.md, Providers → Wrong model on a host).
func TestImageEditsModelMismatch(t *testing.T) {
	g := newTestGateway(t)
	withSpeechModels(t, g)
	g.backend.SetImageServedModel("Qwen/Qwen-Image-Edit-2511")
	w := do(t, g.h, editCall(formOf(textPart("model", "painter"), textPart("prompt", "a fox"),
		filePart("image", "a.png", "image/png", "x")), "stale"))
	expectError(t, w, http.StatusBadGateway, "upstream_model_missing")
	if strings.Contains(w.Body.String(), "mismatch") || strings.Contains(w.Body.String(), "Qwen") {
		t.Errorf("the backend's answer reached the client: %s", w.Body.String())
	}
	expectUnits(t, onlyRecord(t, g), imageUnits(0, 0), false, true)
}

// Nothing of an edit's prompt, its filenames, its images, its data URLs, a LoRA path,
// a URL refused or the backend's error text reaches the log (docs/specs/GATEWAY.md,
// Request pipeline → Uploads; Observability: logs).
func TestImageEditsLogsHoldNoContent(t *testing.T) {
	g := newTestGateway(t)
	withSpeechModels(t, g)
	secretImage := "\x89PNG pomegranate-pixels"
	base := []rawPart{textPart("model", "painter"), textPart("prompt", "zanzibar quokka"), textPart("negative_prompt", "whisper marmalade"),
		filePart("image", "tangerine-holiday.png", "image/png", secretImage), textPart("url[]", "data:image/png;base64,bWFuZ29zdGVlbg==")}
	if w := do(t, g.h, editCall(formOf(base...), "secret-1")); w.Code != http.StatusOK {
		t.Fatalf("%d %.200s", w.Code, w.Body.String())
	}
	do(t, g.h, editCall(formOf(append(base, textPart("lora", `{"path":"/srv/persimmon"}`))...), "secret-2"))
	do(t, g.h, editCall(formOf(append(base, textPart("mask_image", "https://example.com/lychee.png"))...), "secret-3"))
	do(t, g.h, editCall(formOf(append(base, filePart("image[]", "durian-trip.png", "image/png", "x"), textPart("n", "1"),
		textPart("n", "2"))...), "secret-4"))
	g.backend.SetImageServedModel("Other/Model")
	do(t, g.h, editCall(formOf(base...), "secret-5"))
	logs := g.logText()
	for _, secret := range []string{"zanzibar", "quokka", "marmalade", "tangerine", "pomegranate", "bWFuZ29zdGVlbg", "persimmon",
		"lychee", "durian", "iVBORw0KGgo", "mismatch"} {
		if strings.Contains(logs, secret) {
			t.Errorf("the log holds %q:\n%s", secret, logs)
		}
	}
}
