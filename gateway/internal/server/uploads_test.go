package server

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"kaiak/internal/config"
	"kaiak/internal/provider"
)

// uploadEndpoint is an upload endpoint row of the tests' own, its format standing in
// for a real one's, so the upload machinery is tested apart from any format: image[]
// may repeat, and its read records the parts it is handed.
func uploadEndpoint() (*endpoint, *[]provider.FormPart) {
	read := new([]provider.FormPart)
	ep := &endpoint{body: true, api: provider.ImagesGenerations, operation: "generate_content", outputType: "image",
		upload: &uploadFormat{lists: []string{"image[]"}, read: func(_ *request, parts []provider.FormPart) *apiError {
			*read = parts
			return nil
		}}}
	return ep, read
}

// testUploads are upload budget and read deadline of size bytes and the default
// timeout.
func testUploads(size int64) Uploads {
	return Uploads{Budget: NewBodyBudget(size), ReadTimeout: DefaultUploadReadTimeout}
}

// uploadSnapshot is the test config with max_upload_bytes set (0 = the default).
func uploadSnapshot(t *testing.T, maxUpload int64) *config.Snapshot {
	t.Helper()
	return testSnapshotWith(t, "http://127.0.0.1:1", func(doc string) string {
		if maxUpload == 0 {
			return doc
		}
		return replaceOnce(t, doc, `"max_request_body_bytes": 1024 }`,
			`"max_request_body_bytes": 1024, "max_upload_bytes": `+strconv.FormatInt(maxUpload, 10)+` }`)
	})
}

// uploadRequest is a POST of body to the upload endpoint, its Content-Type contentType
// ("" = none).
func uploadRequest(body io.Reader, contentType string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/v1/images/edits", body)
	if contentType != "" {
		r.Header.Set("Content-Type", contentType)
	}
	return r
}

// formContentType is the Content-Type of a form under testBoundary.
const formContentType = "multipart/form-data; boundary=" + testBoundary

// readTestUpload runs the inbound stage for r on the upload endpoint ep, under
// snapshot, with the body budget bodies and uploads. The caller runs the request's
// finishers (finish) to give its budget back.
func readTestUpload(ep *endpoint, r *http.Request, snapshot *config.Snapshot, bodies *BodyBudget, uploads Uploads) (*request,
	*httptest.ResponseRecorder, *apiError) {
	w := httptest.NewRecorder()
	rq := &request{w: &statusWriter{ResponseWriter: w}, r: r, endpoint: ep, start: time.Now(), snapshot: snapshot}
	return rq, w, readInbound(context.Background(), rq, bodies, uploads)
}

// unreadBody fails the test if the body is read.
type unreadBody struct{ t *testing.T }

func (b unreadBody) Read([]byte) (int, error) {
	b.t.Error("the body was read")
	return 0, io.EOF
}

// An upload endpoint takes multipart/form-data with a boundary alone: no content type,
// JSON (OpenAI's JSON form of image edits), a form without a boundary are refused, 400
// unsupported_content_type, before the body is read — nothing taken from either budget.
func TestUploadContentTypeIsCheckedBeforeTheBody(t *testing.T) {
	ep, read := uploadEndpoint()
	bodies, uploads := NewBodyBudget(DefaultBodyMemory), testUploads(DefaultUploadMemory)
	for _, contentType := range []string{"", "application/json", "multipart/form-data", "multipart/form-data; boundary="} {
		rq, _, apiErr := readTestUpload(ep, uploadRequest(unreadBody{t}, contentType), uploadSnapshot(t, 0), bodies, uploads)
		if apiErr == nil || apiErr.status != http.StatusBadRequest || apiErr.code != "unsupported_content_type" {
			t.Errorf("Content-Type %q: %v, want 400 unsupported_content_type", contentType, apiErr)
		}
		rq.finish()
	}
	if *read != nil || bodies.inUse() != 0 || uploads.Budget.inUse() != 0 {
		t.Errorf("format read %v; budgets in use %d and %d, want nothing", *read, bodies.inUse(), uploads.Budget.inUse())
	}
}

// An upload is read into its parts, its model from its model part, and handed to its
// format; its body is held in the upload budget — never the body budget, and under
// the upload cap, not the JSON one (the test config caps JSON bodies at 1024 bytes) —
// until the request ends.
func TestUploadIsReadIntoItsParts(t *testing.T) {
	ep, read := uploadEndpoint()
	bodies, uploads := NewBodyBudget(DefaultBodyMemory), testUploads(DefaultUploadMemory)
	image := strings.Repeat("\x89PNG", 1000)
	body := formOf(textPart("model", "open"), filePart("image[]", "a.png", "image/png", image),
		filePart("image[]", "b.png", "image/png", image), textPart("prompt", "a fox"))
	rq, _, apiErr := readTestUpload(ep, uploadRequest(strings.NewReader(body), formContentType), uploadSnapshot(t, 0), bodies, uploads)
	if apiErr != nil {
		t.Fatalf("refused: %s %s", apiErr.code, apiErr.message)
	}
	if rq.model != "open" || rq.inbound.Sequences != 1 || rq.inbound.Stream || string(rq.body) != body {
		t.Errorf("model %q, sequences %d, stream %v, body held %v; want open, 1, false, the body", rq.model, rq.inbound.Sequences,
			rq.inbound.Stream, string(rq.body) == body)
	}
	expectParts(t, rq.form, []wantPart{{name: "model", body: "open"},
		{name: "image[]", file: true, filename: "a.png", contentType: ptr("image/png"), body: image},
		{name: "image[]", file: true, filename: "b.png", contentType: ptr("image/png"), body: image}, {name: "prompt", body: "a fox"}})
	if len(*read) != 4 || &(*read)[0] != &rq.form[0] {
		t.Errorf("the format read %d parts, want the request's 4", len(*read))
	}
	if got := uploads.Budget.inUse(); got != int64(len(body)) || bodies.inUse() != 0 {
		t.Errorf("upload budget in use %d, body budget %d; want %d and 0", got, bodies.inUse(), len(body))
	}
	rq.finish()
	if uploads.Budget.inUse() != 0 || rq.body != nil || rq.form != nil {
		t.Errorf("upload budget in use %d, body and parts held %v; want them given back", uploads.Budget.inUse(), rq.body != nil || rq.form != nil)
	}
}

// Every upload names its model in a text part, once: absent or empty is 400
// missing_required_parameter, a file part 400 invalid_type, a repeat 400
// duplicate_member — each naming model. A form breaking the grammar is 400
// invalid_body.
func TestUploadModelPart(t *testing.T) {
	ep, _ := uploadEndpoint()
	for _, c := range []struct {
		name, body, code, param string
	}{
		{"no model", formOf(textPart("prompt", "a fox")), "missing_required_parameter", "model"},
		{"an empty model", formOf(textPart("model", "")), "missing_required_parameter", "model"},
		{"a model file", formOf(filePart("model", "m.txt", "text/plain", "open")), "invalid_type", "model"},
		{"two models", formOf(textPart("model", "open"), textPart("model", "secret")), "duplicate_member", "model"},
		{"a broken form", strings.TrimSuffix(formOf(textPart("model", "open")), "--\r\n"), "invalid_body", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			uploads := testUploads(DefaultUploadMemory)
			rq, _, apiErr := readTestUpload(ep, uploadRequest(strings.NewReader(c.body), formContentType), uploadSnapshot(t, 0),
				NewBodyBudget(DefaultBodyMemory), uploads)
			if apiErr == nil || apiErr.status != http.StatusBadRequest || apiErr.code != c.code || apiErr.param != c.param {
				t.Fatalf("%v, want 400 %s param %q", apiErr, c.code, c.param)
			}
			rq.finish()
			if uploads.Budget.inUse() != 0 {
				t.Errorf("upload budget in use %d after the refusal, want 0", uploads.Budget.inUse())
			}
		})
	}
}

// The upload cap is max_upload_bytes, or the whole upload budget when smaller: a body
// declaring more is refused, 413 request_too_large naming the limit, before it is read
// (the test's body fails it if read); a body of unknown length past it, while it is
// read.
func TestUploadCap(t *testing.T) {
	ep, _ := uploadEndpoint()
	over := formOf(textPart("model", "open"), filePart("file", "a.wav", "audio/wav", strings.Repeat("x", 2000)))
	for _, c := range []struct {
		name      string
		maxUpload int64
		budget    int64
		body      io.Reader
		declared  int64
		limit     string
	}{
		{"declared over max_upload_bytes", 2000, DefaultUploadMemory, unreadBody{t}, int64(len(over)), "2000"},
		{"declared over the whole budget", 1 << 20, 1500, unreadBody{t}, int64(len(over)), "1500"},
		{"of unknown length over max_upload_bytes", 2000, DefaultUploadMemory, io.MultiReader(strings.NewReader(over)), -1, "2000"},
	} {
		t.Run(c.name, func(t *testing.T) {
			uploads := testUploads(c.budget)
			r := uploadRequest(c.body, formContentType)
			r.ContentLength = c.declared
			rq, _, apiErr := readTestUpload(ep, r, uploadSnapshot(t, c.maxUpload), NewBodyBudget(DefaultBodyMemory), uploads)
			if apiErr == nil || apiErr.status != http.StatusRequestEntityTooLarge || apiErr.code != "request_too_large" ||
				!strings.Contains(apiErr.message, "limit of "+c.limit+" bytes") {
				t.Fatalf("%v, want 413 request_too_large at %s bytes", apiErr, c.limit)
			}
			rq.finish()
			if uploads.Budget.inUse() != 0 {
				t.Errorf("upload budget in use %d after the refusal, want 0", uploads.Budget.inUse())
			}
		})
	}
	// Within the cap, it is read.
	rq, _, apiErr := readTestUpload(ep, uploadRequest(strings.NewReader(over), formContentType), uploadSnapshot(t, int64(len(over))),
		NewBodyBudget(DefaultBodyMemory), testUploads(DefaultUploadMemory))
	if apiErr != nil {
		t.Errorf("an upload at the cap: %s", apiErr.message)
	}
	rq.finish()
}

// Uploads take their own budget: an upload that does not fit in what the uploads in
// flight leave is refused at once, 503 server_busy with Retry-After: 1, its body
// unread, the body budget untouched — and a full upload budget leaves chat served.
func TestUploadBudgetIsApartFromTheBodyBudget(t *testing.T) {
	ep, _ := uploadEndpoint()
	bodies, uploads := NewBodyBudget(DefaultBodyMemory), testUploads(10000)
	if !uploads.Budget.take(9000) {
		t.Fatal("take")
	}
	body := formOf(textPart("model", "open"), filePart("file", "a.wav", "audio/wav", strings.Repeat("x", 2000)))
	rq, w, apiErr := readTestUpload(ep, uploadRequest(strings.NewReader(body), formContentType), uploadSnapshot(t, 0), bodies, uploads)
	if apiErr == nil || apiErr.status != http.StatusServiceUnavailable || apiErr.code != "server_busy" || w.Header().Get("Retry-After") != "1" {
		t.Fatalf("%v, Retry-After %q; want 503 server_busy, 1", apiErr, w.Header().Get("Retry-After"))
	}
	rq.finish()
	if uploads.Budget.inUse() != 9000 || bodies.inUse() != 0 {
		t.Errorf("budgets in use: uploads %d, bodies %d; want the others' 9000 and 0", uploads.Budget.inUse(), bodies.inUse())
	}

	g := newTestGateway(t)
	if !g.uploads.Budget.take(DefaultUploadMemory) {
		t.Fatal("take")
	}
	if w := do(t, g.h, call{method: "POST", path: "/v1/chat/completions", key: workloadKey, body: chatBody}); w.Code != http.StatusOK {
		t.Errorf("chat with the upload budget spent: status %d, want 200: %s", w.Code, w.Body.String())
	}
	g.uploads.Budget.give(DefaultUploadMemory)
}

// serveUpload serves the upload endpoint's inbound stage on a real listener bounded by
// timeouts, its uploads read within uploadRead; it answers how many parts it read, or
// its refusal.
func serveUpload(t *testing.T, timeouts ClientTimeouts, uploadRead time.Duration) string {
	t.Helper()
	ep, _ := uploadEndpoint()
	snapshot := uploadSnapshot(t, 0)
	bodies, uploads := NewBodyBudget(DefaultBodyMemory), Uploads{Budget: NewBodyBudget(DefaultUploadMemory), ReadTimeout: uploadRead}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rq := &request{w: &statusWriter{ResponseWriter: w}, r: r, endpoint: ep, start: time.Now(), snapshot: snapshot}
		defer rq.finish()
		if apiErr := readInbound(r.Context(), rq, bodies, uploads); apiErr != nil {
			writeError(rq.w, apiErr, shapeOpenAI)
			return
		}
		_, _ = fmt.Fprintf(rq.w, "%d parts", len(rq.form))
	})
	l, err := Listen("api", "127.0.0.1:0", handler, timeouts, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = l.Serve() }()
	t.Cleanup(l.cut)
	return l.Addr().String()
}

// An upload's body arrives within the upload read deadline, in place of the body-read
// deadline: a body slower than the body-read deadline is read when it arrives within
// the upload's; one stalled past the upload's is answered 400 invalid_body then, long
// before the body-read deadline.
func TestUploadReadDeadline(t *testing.T) {
	body := formOf(textPart("model", "open"), filePart("file", "a.wav", "audio/wav", strings.Repeat("x", 1000)))
	head := "POST /v1/images/edits HTTP/1.1\r\nHost: kaiak\r\nContent-Type: " + formContentType +
		"\r\nContent-Length: " + strconv.Itoa(len(body)) + "\r\n\r\n"

	t.Run("longer than the body-read deadline", func(t *testing.T) {
		conn := dial(t, serveUpload(t, shortTimeouts, 3*time.Second))
		if _, err := io.WriteString(conn, head+body[:len(body)/2]); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * shortTimeouts.BodyRead) // the client is slow
		if _, err := io.WriteString(conn, body[len(body)/2:]); err != nil {
			t.Fatal(err)
		}
		answer, _ := readAnswer(t, conn, 3*time.Second)
		if !strings.HasPrefix(answer, "HTTP/1.1 200") || !strings.HasSuffix(answer, "2 parts") {
			t.Errorf("answer %q, want 200 and the 2 parts read", answer)
		}
	})
	t.Run("shorter than the body-read deadline", func(t *testing.T) {
		timeouts := shortTimeouts
		timeouts.BodyRead = 10 * time.Second
		conn := dial(t, serveUpload(t, timeouts, 300*time.Millisecond))
		if _, err := io.WriteString(conn, head+body[:len(body)/2]); err != nil {
			t.Fatal(err)
		}
		answer, closed := readAnswer(t, conn, 3*time.Second)
		if !closed || !strings.HasPrefix(answer, "HTTP/1.1 400") || !strings.Contains(answer, "invalid_body") {
			t.Errorf("answer %q (closed %v), want 400 invalid_body within the upload read deadline", answer, closed)
		}
	})
}
