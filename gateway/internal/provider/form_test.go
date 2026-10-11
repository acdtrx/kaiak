package provider

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
	"weak"

	"kaiak/internal/config"
)

// clientForm is a client's upload as the inbound stage hands it over: a held body and
// its parts, slices of it — names and filenames that need quoting, an empty part and
// an empty filename, a Content-Type sent empty, bytes that look like delimiters.
func clientForm() ([]byte, []FormPart) {
	pieces := []string{"pub", "\x89PNG\r\n--" + "\r\n--boundary\r\n\x00\xff", "a fox", "", "RIFF....WAVE"}
	body := []byte(strings.Join(pieces, ""))
	at := 0
	slice := func(i int) []byte {
		s := body[at : at+len(pieces[i]) : at+len(pieces[i])]
		at += len(pieces[i])
		return s
	}
	return body, []FormPart{
		{Name: "model", Body: slice(0)},
		{Name: "image[]", File: true, Filename: `a "b" \c.png`, ContentType: []byte("image/png"), Body: slice(1)},
		{Name: "prompt", ContentType: []byte{}, Body: slice(2)},
		{Name: "é", File: true, Body: slice(3)},
		{Name: `q"\`, File: true, Filename: "été.wav", ContentType: []byte("audio/wav; rate=16000"), Body: slice(4)},
	}
}

// writtenForm is the form mime/multipart's writer writes for parts under boundary,
// the model part holding model: the reference the rebuilt body must equal.
func writtenForm(t *testing.T, boundary string, parts []FormPart, model string) []byte {
	t.Helper()
	var b bytes.Buffer
	w := multipart.NewWriter(&b)
	if err := w.SetBoundary(boundary); err != nil {
		t.Fatal(err)
	}
	for _, p := range parts {
		disposition := multipart.FileContentDisposition(p.Name, p.Filename)
		if !p.File {
			disposition = strings.TrimSuffix(multipart.FileContentDisposition(p.Name, ""), `; filename=""`)
		}
		h := textproto.MIMEHeader{"Content-Disposition": {disposition}}
		if p.ContentType != nil {
			h["Content-Type"] = []string{string(p.ContentType)}
		}
		pw, err := w.CreatePart(h)
		if err != nil {
			t.Fatal(err)
		}
		value := p.Body
		if p.Name == "model" {
			value = []byte(model)
		}
		_, _ = pw.Write(value)
	}
	_ = w.Close()
	return b.Bytes()
}

// readBack reads a rebuilt form with mime/multipart: each part's name, filename,
// Content-Type and bytes.
func readBack(t *testing.T, form []byte, boundary string) []FormPart {
	t.Helper()
	r := multipart.NewReader(bytes.NewReader(form), boundary)
	var parts []FormPart
	for {
		p, err := r.NextPart()
		if errors.Is(err, io.EOF) {
			return parts
		}
		if err != nil {
			t.Fatalf("the rebuilt form does not read: %v", err)
		}
		data, _ := io.ReadAll(p)
		_, params, _ := mime.ParseMediaType(p.Header.Get("Content-Disposition"))
		filename, file := params["filename"]
		part := FormPart{Name: p.FormName(), File: file, Filename: filename, Body: data}
		if v, ok := p.Header["Content-Type"]; ok {
			part.ContentType = append([]byte{}, v[0]...)
		}
		parts = append(parts, part)
	}
}

// boundaryOf is the boundary of a multipart/form-data Content-Type.
func boundaryOf(t *testing.T, contentType string) string {
	t.Helper()
	kind, params, err := mime.ParseMediaType(contentType)
	if err != nil || kind != "multipart/form-data" || params["boundary"] == "" {
		t.Fatalf("Content-Type %q, want multipart/form-data with a boundary", contentType)
	}
	return params["boundary"]
}

// readBody reads an upstream body reader to its end.
func readBody(t *testing.T, r io.Reader) []byte {
	t.Helper()
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// The rebuilt form is the client's parts in order, byte for byte, under the headers
// mime/multipart's writer writes for them — name and filename quoted as it quotes
// them, the client's Content-Type when it sent one, nothing else — the model part's
// value the deployment's model, under a fresh boundary; its length is the bytes it
// sends, from the reader as from GetBody.
func TestFormBodyIsThePartsRebuilt(t *testing.T) {
	_, parts := clientForm()
	req := &Request{Deployment: config.Deployment{Model: "vendor/backend-model"}, Form: parts}
	body, contentType := formBody(req)
	boundary := boundaryOf(t, contentType)
	sent := readBody(t, body.reader())
	if want := writtenForm(t, boundary, parts, "vendor/backend-model"); !bytes.Equal(sent, want) {
		t.Errorf("rebuilt form\n%q\nwant\n%q", sent, want)
	}
	if body.size != int64(len(sent)) {
		t.Errorf("size %d, the body sends %d bytes", body.size, len(sent))
	}
	again, err := body.getBody()
	if err != nil {
		t.Fatal(err)
	}
	if replay := readBody(t, again); !bytes.Equal(replay, sent) {
		t.Error("GetBody sends other bytes than the first reader")
	}

	got := readBack(t, sent, boundary)
	if len(got) != len(parts) {
		t.Fatalf("%d parts read back, want %d", len(got), len(parts))
	}
	for i, p := range parts {
		want := p.Body
		if p.Name == "model" {
			want = []byte("vendor/backend-model")
		}
		g := got[i]
		if g.Name != p.Name || g.File != p.File || g.Filename != p.Filename || !bytes.Equal(g.Body, want) ||
			(g.ContentType == nil) != (p.ContentType == nil) || !bytes.Equal(g.ContentType, p.ContentType) {
			t.Errorf("part %d reads back as %+v, want %+v with body %q", i, g, p, want)
		}
	}

	if _, other := formBody(req); boundaryOf(t, other) == boundary {
		t.Error("two rebuilt forms share a boundary")
	}
}

// The rebuilt form sends the client's bytes, never a copy of them: each part's bytes
// are its own slice of the client's body; only the delimiters and headers are made.
func TestFormBodySendsTheClientsBytes(t *testing.T) {
	client, parts := clientForm()
	body, _ := formBody(&Request{Deployment: config.Deployment{Model: "backend-model"}, Form: parts})
	if len(body.pieces) != 2*len(parts)+1 {
		t.Fatalf("%d pieces, want a head and a body per part and the closing delimiter", len(body.pieces))
	}
	made := 0
	for i, p := range parts {
		piece := body.pieces[2*i+1]
		switch {
		case p.Name == "model":
			made += len(piece)
		case len(p.Body) > 0 && (len(piece) != len(p.Body) || &piece[0] != &p.Body[0]):
			t.Errorf("part %d's bytes are copied, not the client's", i)
		}
		made += len(body.pieces[2*i])
	}
	made += len(body.pieces[len(body.pieces)-1])
	if total := int64(made + len(client) - len(parts[0].Body)); total != body.size {
		t.Errorf("the client's bytes and the made ones total %d, the body is %d", total, body.size)
	}
}

// An upload reaches the backend as a rebuilt form: the Content-Type naming its
// boundary, an exact Content-Length (never chunked), every byte of it. Each attempt
// builds its own, under a boundary of its own.
func TestFormIsSentWithItsLength(t *testing.T) {
	type received struct {
		contentType string
		length      int64
		chunked     bool
		body        []byte
	}
	var mu sync.Mutex
	var got []received
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, received{r.Header.Get("Content-Type"), r.ContentLength, len(r.TransferEncoding) > 0, data})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"text":"ok"}`)
	}))
	defer srv.Close()
	b := &config.Backend{ID: "b", Type: config.BackendVLLM, BaseURL: srv.URL + "/v1", ConnectTimeout: time.Second,
		FirstEventTimeout: time.Minute, ResponseTimeout: time.Minute, StallTimeout: time.Minute}
	client, parts := clientForm()
	for range 2 {
		resp, err := moduleRegistry().For(b).Send(context.Background(), &Request{Endpoint: ImagesGenerations,
			Deployment: config.Deployment{Backend: b, Model: "backend-model"}, Body: client, Form: parts, RequestID: "r", PublicModel: "pub"})
		if err != nil {
			t.Fatal(err)
		}
		if _, end := readEvents(resp); !errors.Is(end, io.EOF) {
			t.Fatalf("answer ended %v", end)
		}
		resp.Close()
	}
	if len(got) != 2 {
		t.Fatalf("the backend got %d requests, want 2", len(got))
	}
	for _, r := range got {
		boundary := boundaryOf(t, r.contentType)
		if r.chunked || r.length != int64(len(r.body)) {
			t.Errorf("Content-Length %d (chunked %v), the body is %d bytes", r.length, r.chunked, len(r.body))
		}
		if want := writtenForm(t, boundary, parts, "backend-model"); !bytes.Equal(r.body, want) {
			t.Errorf("the backend got\n%q\nwant\n%q", r.body, want)
		}
	}
	if got[0].contentType == got[1].contentType {
		t.Error("two attempts sent one boundary")
	}
}

// A form takes no member edit: a parameter set on an upload is a gateway fault, a
// plain error, and nothing is sent.
func TestFormTakesNoMemberEdits(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("the request was sent")
	}))
	defer srv.Close()
	b := &config.Backend{ID: "b", Type: config.BackendVLLM, BaseURL: srv.URL + "/v1", ConnectTimeout: time.Second,
		FirstEventTimeout: time.Minute, ResponseTimeout: time.Minute, StallTimeout: time.Minute}
	client, parts := clientForm()
	_, err := moduleRegistry().For(b).Send(context.Background(), &Request{Endpoint: ImagesGenerations,
		Deployment: config.Deployment{Backend: b, Model: "backend-model"}, Body: client, Form: parts, RequestID: "r",
		Params: []Param{{Key: "max_tokens", Value: []byte("1")}}})
	var perr *Error
	if err == nil || errors.As(err, &perr) {
		t.Errorf("error %v, want a gateway fault", err)
	}
}

// Once Send has returned, the client's upload is not reachable from the response:
// neither through the parts nor through the rebuilt form, whose pieces are its bytes.
func TestSendKeepsNoFormOnceTheFirstEventIsIn(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"text":`)
		w.(http.Flusher).Flush()
		<-r.Context().Done() // the answer stays open until the gateway closes it
	}))
	defer srv.Close()
	b := &config.Backend{ID: "b", Type: config.BackendVLLM, BaseURL: srv.URL + "/v1", ConnectTimeout: time.Second,
		FirstEventTimeout: time.Minute, ResponseTimeout: time.Minute, StallTimeout: time.Minute}
	transport := &capturingTransport{next: newClient(time.Second).Transport}
	p := &vLLM{backend: b, client: &http.Client{Transport: transport}}

	send := func() (Response, weak.Pointer[byte]) {
		client, parts := clientForm()
		resp, err := p.Send(context.Background(), &Request{Endpoint: ImagesGenerations,
			Deployment: config.Deployment{Backend: b, Model: "backend-model"}, Body: client, Form: parts, RequestID: "r", PublicModel: "pub"})
		if err != nil {
			t.Fatal(err)
		}
		return resp, weak.Make(&client[0])
	}
	resp, clientBody := send()
	defer resp.Close()

	runtime.GC()
	if clientBody.Value() != nil {
		t.Error("the client's upload is still reachable from the open response")
	}
	transport.mu.Lock()
	upstream := transport.reqs[0]
	transport.mu.Unlock()
	if again, err := upstream.GetBody(); err == nil {
		again.Close()
		t.Error("the rebuilt form can still be replayed from the upstream request")
	}
	runtime.KeepAlive(resp)
}
