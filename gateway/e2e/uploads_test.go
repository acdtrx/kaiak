package e2e

// The upload scenarios' shared parts (docs/specs/GATEWAY.md, Request pipeline →
// Uploads): a form as a client writes it — Go's mime/multipart writer — sent to the
// gateway, and the form a backend received read back as its server reads one, to
// compare with what the client sent.

import (
	"bytes"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strconv"
	"testing"

	"kaiak/internal/fakebackend"
)

// formPart is one part of an upload: a text part, or — with a filename — a file part
// with its content type ("": none sent).
type formPart struct {
	name        string
	file        bool
	filename    string
	contentType string
	data        string
}

func textPart(name, value string) formPart { return formPart{name: name, data: value} }

func filePart(name, filename, contentType, data string) formPart {
	return formPart{name: name, file: true, filename: filename, contentType: contentType, data: data}
}

// writeForm writes parts in order as a client does, under a boundary of
// mime/multipart's writer: the body and its Content-Type.
func writeForm(t *testing.T, parts ...formPart) (contentType string, body []byte) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for _, p := range parts {
		header := textproto.MIMEHeader{}
		if p.file {
			header.Set("Content-Disposition", multipart.FileContentDisposition(p.name, p.filename))
			if p.contentType != "" {
				header.Set("Content-Type", p.contentType)
			}
		} else {
			header.Set("Content-Disposition", mime.FormatMediaType("form-data", map[string]string{"name": p.name}))
		}
		pw, err := w.CreatePart(header)
		if err == nil {
			_, err = io.WriteString(pw, p.data)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return w.FormDataContentType(), buf.Bytes()
}

// postForm sends an upload to the API with key and request ID (either may be empty):
// body under contentType ("": none sent), its length declared.
func (g *gateway) postForm(t *testing.T, path, key, requestID, contentType string, body []byte) *response {
	t.Helper()
	return g.postUpload(t, path, key, requestID, contentType, bytes.NewReader(body))
}

// postUpload sends body to the API as postForm does; a body of a reader type
// net/http cannot size goes out chunked, its length undeclared.
func (g *gateway) postUpload(t *testing.T, path, key, requestID, contentType string, body io.Reader) *response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, g.api+path, body)
	if err != nil {
		t.Fatal(err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	if requestID != "" {
		req.Header.Set("X-Request-Id", requestID)
	}
	return do(t, req)
}

// readForm reads a form as a backend's server does (mime/multipart), under the
// boundary contentType names: its parts in order, and that boundary.
func readForm(t *testing.T, contentType string, body []byte) (parts []formPart, boundary string) {
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
		p := formPart{name: disposition["name"], contentType: part.Header.Get("Content-Type"), data: string(data)}
		p.filename, p.file = disposition["filename"]
		parts = append(parts, p)
	}
}

// wantRebuilt fails the test unless the backend received the client's form rebuilt
// (Request pipeline → Uploads, the rebuilt body): every part in order with its name,
// filename, content type and bytes, model's value the deployment's model, under a
// boundary of the gateway's, sent with its exact length.
func wantRebuilt(t *testing.T, got *fakebackend.Request, clientType string, client []byte, model string) {
	t.Helper()
	sent, boundary := readForm(t, got.Header.Get("Content-Type"), got.Body)
	want, clientBoundary := readForm(t, clientType, client)
	if boundary == clientBoundary {
		t.Error("the backend got the client's boundary, want one of the gateway's")
	}
	if length := got.Header.Get("Content-Length"); length != strconv.Itoa(len(got.Body)) {
		t.Errorf("Content-Length %q, the body is %d bytes", length, len(got.Body))
	}
	for i := range want {
		if want[i].name == "model" {
			want[i].data = model
		}
	}
	if len(sent) != len(want) {
		t.Fatalf("the backend got %d parts, want %d", len(sent), len(want))
	}
	for i := range want {
		if sent[i] != want[i] {
			t.Errorf("part %d: %s %q (file %v, content type %q), %d bytes; want %s %q (file %v, content type %q), %d bytes",
				i, sent[i].name, sent[i].filename, sent[i].file, sent[i].contentType, len(sent[i].data),
				want[i].name, want[i].filename, want[i].file, want[i].contentType, len(want[i].data))
		}
	}
}

// wantRefusal fails the test unless r is a refusal in OpenAI's error shape with status
// and code; it returns the error object.
func wantRefusal(t *testing.T, r *response, status int, code string) map[string]any {
	t.Helper()
	e, _ := r.json(t)["error"].(map[string]any)
	if r.StatusCode != status || e["code"] != code {
		t.Fatalf("%d %.300s, want %d %s", r.StatusCode, r.body, status, code)
	}
	return e
}

// pngImage is an input image as a client uploads it: a PNG signature, then every byte
// value — CR, LF and a boundary's dashes among them — which the backend must get as
// sent.
var pngImage = func() string {
	b := []byte(pngSignature)
	for c := range 256 {
		b = append(b, byte(c))
	}
	return string(append(b, "\r\n--\r\n"...))
}()
