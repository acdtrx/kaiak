package server

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"mime"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"

	"kaiak/internal/provider"
)

// testBoundary is the boundary the form tests write their bodies under.
const testBoundary = "b0und"

// rawPart is a part as a test writes it: its header lines and its bytes.
type rawPart struct {
	headers []string
	body    string
}

// formOf is a form body of parts under testBoundary, delimited as clients write one.
func formOf(parts ...rawPart) string {
	var s strings.Builder
	for _, p := range parts {
		s.WriteString("--" + testBoundary + "\r\n")
		for _, h := range p.headers {
			s.WriteString(h + "\r\n")
		}
		s.WriteString("\r\n" + p.body + "\r\n")
	}
	s.WriteString("--" + testBoundary + "--\r\n")
	return s.String()
}

// textPart is a text part named name holding value.
func textPart(name, value string) rawPart {
	return rawPart{[]string{`Content-Disposition: form-data; name="` + name + `"`}, value}
}

// filePart is a file part named name, of filename and contentType, holding data.
func filePart(name, filename, contentType, data string) rawPart {
	return rawPart{[]string{`Content-Disposition: form-data; name="` + name + `"; filename="` + filename + `"`,
		"Content-Type: " + contentType}, data}
}

// wantPart is a part a test expects: contentType nil when the part has none.
type wantPart struct {
	name        string
	file        bool
	filename    string
	contentType *string
	body        string
}

func ptr[T any](v T) *T { return &v }

// expectParts fails the test unless got are want, in order.
func expectParts(t *testing.T, got []provider.FormPart, want []wantPart) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%d parts, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		g := got[i]
		if g.Name != w.name || g.File != w.file || g.Filename != w.filename || string(g.Body) != w.body {
			t.Errorf("part %d: name %q, file %v, filename %q, body %q; want %q, %v, %q, %q",
				i, g.Name, g.File, g.Filename, g.Body, w.name, w.file, w.filename, w.body)
		}
		if (g.ContentType == nil) != (w.contentType == nil) || w.contentType != nil && string(g.ContentType) != *w.contentType {
			t.Errorf("part %d: content type %q (nil %v), want %v", i, g.ContentType, g.ContentType == nil, w.contentType)
		}
	}
}

// binaryBytes holds every byte value and the pieces of a delimiter that are not one: the
// boundary without the CRLF before it, after a lone CR or LF, followed by a letter, by
// one dash, or cut short; dashes and CRLFs of their own.
var binaryBytes = func() string {
	var all [256]byte
	for i := range all {
		all[i] = byte(i)
	}
	return string(all[:]) + "--" + testBoundary + "\r\n" + "\n--" + testBoundary + "\r\n" + "\r--" + testBoundary + "\r\n" +
		"\r\n--" + testBoundary + "x\r\n" + "\r\n--" + testBoundary + "-x" + "\r\n--" + testBoundary[:3] + "\r\n" +
		"\r\n--\r\n--" + "\r\n\r\n" + testBoundary
}()

// An upload's parts as clients send them: names, filenames (quoted, escaped, RFC 2231,
// empty), content types or none, other headers skipped, empty parts in either shape,
// binary bytes holding pieces of the boundary, a preamble and an epilogue skipped,
// header names in any case and values without the spaces around them — each part's
// bytes a slice of the client's body.
func TestFormParts(t *testing.T) {
	for _, c := range []struct {
		name string
		body string
		want []wantPart
	}{
		{"text and file parts", formOf(textPart("model", "pub"), filePart("image[]", "cat.png", "image/png", "\x89PNG\r\n"),
			filePart("image[]", "", "application/octet-stream", "")),
			[]wantPart{{name: "model", body: "pub"}, {name: "image[]", file: true, filename: "cat.png", contentType: ptr("image/png"), body: "\x89PNG\r\n"},
				{name: "image[]", file: true, contentType: ptr("application/octet-stream")}}},
		{"binary bytes holding pieces of the boundary", formOf(filePart("file", "a.wav", "audio/wav", binaryBytes), textPart("model", "pub")),
			[]wantPart{{name: "file", file: true, filename: "a.wav", contentType: ptr("audio/wav"), body: binaryBytes}, {name: "model", body: "pub"}}},
		{"escaped, encoded and unquoted names", formOf(
			rawPart{[]string{`Content-Disposition: form-data; name="a\"b\\c"; filename="C:\\dir\\x.wav"`}, "1"},
			rawPart{[]string{`Content-Disposition: form-data; name=plain; filename*=UTF-8''%C3%A9t%C3%A9.wav`}, "2"},
			rawPart{[]string{`Content-Disposition: form-data; name="résumé"`}, "3"}),
			[]wantPart{{name: `a"b\c`, file: true, filename: `C:\dir\x.wav`, body: "1"}, {name: "plain", file: true, filename: "été.wav", body: "2"},
				{name: "résumé", body: "3"}}},
		{"header names in any case, values trimmed, other headers skipped", formOf(
			rawPart{[]string{"content-disposition:\tForm-Data; name=\"a\"  ", "X-Other: anything", "CONTENT-TYPE:   text/plain; charset=utf-8 \t"}, "v"},
			rawPart{[]string{`Content-Disposition: form-data; name="b"`, "Content-Type:"}, "w"}),
			[]wantPart{{name: "a", contentType: ptr("text/plain; charset=utf-8"), body: "v"}, {name: "b", contentType: ptr(""), body: "w"}}},
		{"an empty part, its bytes absent", "--" + testBoundary + "\r\nContent-Disposition: form-data; name=\"a\"\r\n\r\n--" + testBoundary +
			"\r\nContent-Disposition: form-data; name=\"b\"\r\n\r\n--" + testBoundary + "--",
			[]wantPart{{name: "a"}, {name: "b"}}},
		{"a preamble and an epilogue", "a preamble\r\nof lines --" + testBoundary + "\r\n\r\n" + formOf(textPart("model", "pub")) + "an epilogue\r\n--" + testBoundary + "\r\nignored",
			[]wantPart{{name: "model", body: "pub"}}},
		{"the closing boundary ending the body", strings.TrimSuffix(formOf(textPart("model", "pub")), "\r\n"),
			[]wantPart{{name: "model", body: "pub"}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			body := []byte(c.body)
			parts, apiErr := readForm(body, testBoundary, []string{"image[]"})
			if apiErr != nil {
				t.Fatalf("refused: %s %s", apiErr.code, apiErr.message)
			}
			expectParts(t, parts, c.want)
			for i, part := range parts {
				if len(part.Body) > 0 && !within(part.Body, body) {
					t.Errorf("part %d's bytes are a copy, not a slice of the body", i)
				}
			}
			expectReadAsMime(t, body, testBoundary, parts)
		})
	}
}

// within reports whether s lies in whole's memory.
func within(s, whole []byte) bool {
	return cap(s) > 0 && cap(whole) > 0 && &whole[:cap(whole)][cap(whole)-1] == &s[:cap(s)][cap(s)-1]
}

// The bounds, at their edge: a part's headers of 8 KiB, their blank line included; a
// text part of 1 MiB, a file part of more; 1000 parts; a preamble of 4 KiB. One more
// of each is refused.
func TestFormBoundsAtTheirEdge(t *testing.T) {
	disposition := `Content-Disposition: form-data; name="a"`
	headerBlock := func(size int) rawPart {
		pad := size - len(disposition) - len("\r\nX-Pad: \r\n\r\n")
		return rawPart{[]string{disposition, "X-Pad: " + strings.Repeat("x", pad)}, "v"}
	}
	manyParts := func(n int) string {
		parts := make([]rawPart, n)
		for i := range parts {
			parts[i] = textPart(fmt.Sprint("p", i), "")
		}
		return formOf(parts...)
	}
	preamble := func(size int) string {
		return strings.Repeat("x", size-2) + "\r\n" + formOf(textPart("a", "v"))
	}
	for _, c := range []struct {
		name     string
		body     string
		accepted bool
	}{
		{"headers of 8 KiB", formOf(headerBlock(8 << 10)), true},
		{"headers over 8 KiB", formOf(headerBlock(8<<10 + 1)), false},
		{"a text part of 1 MiB", formOf(textPart("a", strings.Repeat("x", 1<<20))), true},
		{"a text part over 1 MiB", formOf(textPart("a", strings.Repeat("x", 1<<20+1))), false},
		{"a file part over 1 MiB", formOf(filePart("a", "f", "audio/wav", strings.Repeat("x", 2<<20))), true},
		{"1000 parts", manyParts(1000), true},
		{"1001 parts", manyParts(1001), false},
		{"a preamble of 4 KiB", preamble(4 << 10), true},
		{"a preamble over 4 KiB", preamble(4<<10 + 1), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			parts, apiErr := readForm([]byte(c.body), testBoundary, nil)
			switch {
			case c.accepted && apiErr != nil:
				t.Fatalf("refused: %s", apiErr.message)
			case !c.accepted && (apiErr == nil || apiErr.code != "invalid_body"):
				t.Fatalf("got %v, want invalid_body", apiErr)
			case c.accepted:
				expectReadAsMime(t, []byte(c.body), testBoundary, parts)
			}
		})
	}
}

// What breaks the grammar is refused, 400 invalid_body, the message never quoting the
// body; a repeated single-value part, 400 duplicate_member naming it.
func TestFormRefusals(t *testing.T) {
	model := textPart("model", "pub")
	b := testBoundary
	for _, c := range []struct {
		name, body, code, param string
	}{
		{"no name", formOf(rawPart{[]string{"Content-Disposition: form-data"}, "v"}), "invalid_body", ""},
		{"an empty name", formOf(rawPart{[]string{`Content-Disposition: form-data; name=""; filename="secret.wav"`}, "v"}), "invalid_body", ""},
		{"no Content-Disposition", formOf(rawPart{[]string{"Content-Type: text/plain"}, "v"}), "invalid_body", ""},
		{"no headers", formOf(rawPart{nil, "v"}), "invalid_body", ""},
		{"not form-data", formOf(rawPart{[]string{`Content-Disposition: attachment; name="a"`}, "v"}), "invalid_body", ""},
		{"an unreadable disposition", formOf(rawPart{[]string{`Content-Disposition: form-data; name="a"; name="b"`}, "v"}), "invalid_body", ""},
		{"two Content-Disposition headers", formOf(rawPart{[]string{`Content-Disposition: form-data; name="a"`, `content-disposition: form-data; name="b"`}, "v"}), "invalid_body", ""},
		{"two Content-Type headers", formOf(rawPart{[]string{`Content-Disposition: form-data; name="a"`, "Content-Type: a/b", "Content-Type: c/d"}, "v"}), "invalid_body", ""},
		{"a Content-Transfer-Encoding", formOf(rawPart{[]string{`Content-Disposition: form-data; name="a"`, "Content-Transfer-Encoding: quoted-printable"}, "v"}), "invalid_body", ""},
		{"a binary Content-Transfer-Encoding", formOf(rawPart{[]string{`Content-Disposition: form-data; name="a"`, "content-transfer-encoding: binary"}, "v"}), "invalid_body", ""},
		{"a folded header", formOf(rawPart{[]string{`Content-Disposition: form-data;`, ` name="a"`}, "v"}), "invalid_body", ""},
		{"a header without a colon", formOf(rawPart{[]string{`Content-Disposition: form-data; name="a"`, "X-Other"}, "v"}), "invalid_body", ""},
		{"a space before the colon", formOf(rawPart{[]string{`Content-Disposition : form-data; name="a"`}, "v"}), "invalid_body", ""},
		{"a control character in a header", formOf(rawPart{[]string{`Content-Disposition: form-data; name="a"`, "X-Other: a\x00b"}, "v"}), "invalid_body", ""},
		{"a line feed in a name", formOf(rawPart{[]string{`Content-Disposition: form-data; name*=UTF-8''a%0Ab`}, "v"}), "invalid_body", ""},
		{"a control character in a filename", formOf(rawPart{[]string{`Content-Disposition: form-data; name="a"; filename*=UTF-8''x%01.wav`}, "v"}), "invalid_body", ""},
		{"lines ending in LF", strings.ReplaceAll(formOf(model), "\r\n", "\n"), "invalid_body", ""},
		{"a header line ending in LF", "--" + b + "\r\nContent-Disposition: form-data; name=\"a\"\nX: y\r\n\r\nv\r\n--" + b + "--\r\n", "invalid_body", ""},
		{"no closing boundary", strings.TrimSuffix(formOf(model), "--"+b+"--\r\n"), "invalid_body", ""},
		{"cut after a delimiter", strings.TrimSuffix(formOf(model), "--\r\n"), "invalid_body", ""},
		{"cut after a dash", strings.TrimSuffix(formOf(model), "-\r\n"), "invalid_body", ""},
		{"cut within the headers", "--" + b + "\r\nContent-Disposition: form-data; na", "invalid_body", ""},
		{"no blank line after the headers", "--" + b + "\r\nContent-Disposition: form-data; name=\"a\"\r\n", "invalid_body", ""},
		{"padding after a boundary", strings.Replace(formOf(model, model), "--"+b+"\r\n", "--"+b+" \r\n", 1), "invalid_body", ""},
		{"padding after a later boundary", strings.Replace(formOf(textPart("a", "v"), model), "\r\n--"+b+"\r\n", "\r\n--"+b+"\t\r\n", 1), "invalid_body", ""},
		{"padding after the closing boundary", strings.Replace(formOf(model), b+"--", b+"-- ", 1), "invalid_body", ""},
		{"a lone CR after a boundary", strings.Replace(formOf(textPart("a", "v"), model), "\r\n--"+b+"\r\n", "\r\n--"+b+"\rX\r\n", 1), "invalid_body", ""},
		{"a lone LF after a boundary", strings.Replace(formOf(textPart("a", "v"), model), "\r\n--"+b+"\r\n", "\r\n--"+b+"\n", 1), "invalid_body", ""},
		{"more after the closing boundary", strings.Replace(formOf(model), b+"--", b+"--more", 1), "invalid_body", ""},
		{"no part", "--" + b + "--\r\n", "invalid_body", ""},
		{"no boundary", "just bytes", "invalid_body", ""},
		{"an empty body", "", "invalid_body", ""},
		{"a preamble ending in LF", "preamble\n" + formOf(model), "invalid_body", ""},
		{"a preamble line starting with the boundary", "--" + b + "x\r\n" + formOf(model), "invalid_body", ""},
		{"a repeated single-value part", formOf(model, textPart("prompt", "a"), textPart("prompt", "b")), "duplicate_member", "prompt"},
		{"a repeated model", formOf(model, model), "duplicate_member", "model"},
		{"names compared byte for byte", formOf(textPart("Prompt", "a"), textPart("prompt", "b"), textPart("Prompt", "c")), "duplicate_member", "Prompt"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, apiErr := readForm([]byte(c.body), b, []string{"image[]"})
			if apiErr == nil {
				t.Fatal("accepted")
			}
			if apiErr.status != http.StatusBadRequest || apiErr.code != c.code || apiErr.param != c.param {
				t.Errorf("%d %s param %q, want 400 %s param %q (%s)", apiErr.status, apiErr.code, apiErr.param, c.code, c.param, apiErr.message)
			}
			if strings.Contains(apiErr.message, "secret") {
				t.Errorf("the message quotes the body: %s", apiErr.message)
			}
		})
	}
	// A list part repeats.
	parts, apiErr := readForm([]byte(formOf(model, filePart("image[]", "a", "image/png", "1"), filePart("image[]", "b", "image/png", "2"))), b,
		[]string{"image[]"})
	if apiErr != nil || len(parts) != 3 {
		t.Errorf("a repeated list part: %d parts, %v; want 3, accepted", len(parts), apiErr)
	}
}

// An upload's Content-Type: multipart/form-data with a boundary of 1 to 70 of RFC 2046's
// characters, not ending in a space; anything else is 400 unsupported_content_type.
func TestUploadBoundary(t *testing.T) {
	long := strings.Repeat("a", 70)
	for _, c := range []struct {
		contentType, boundary string
	}{
		{"multipart/form-data; boundary=" + testBoundary, testBoundary},
		{"Multipart/Form-Data; Boundary=abc", "abc"},
		{`multipart/form-data; boundary="----WebKitFormBoundary7MA4YWxkTrZu0gW"`, "----WebKitFormBoundary7MA4YWxkTrZu0gW"},
		{`multipart/form-data; charset=utf-8; boundary="a'()+_,-./:=? b"`, "a'()+_,-./:=? b"},
		{"multipart/form-data; boundary=" + long, long},
		{"", ""},
		{"application/json", ""},
		{"multipart/form-data", ""},
		{"multipart/form-data; boundary=", ""},
		{`multipart/form-data; boundary=""`, ""},
		{"multipart/form-data; boundary=" + long + "a", ""},
		{`multipart/form-data; boundary="ab "`, ""},
		{`multipart/form-data; boundary="a@b"`, ""},
		{"multipart/mixed; boundary=abc", ""},
		{"application/x-www-form-urlencoded", ""},
		{"multipart/form-data; boundary=a; boundary=b", ""},
	} {
		boundary, apiErr := uploadBoundary(c.contentType)
		switch {
		case c.boundary != "" && (apiErr != nil || boundary != c.boundary):
			t.Errorf("%q: %q, %v; want boundary %q", c.contentType, boundary, apiErr, c.boundary)
		case c.boundary == "" && (apiErr == nil || apiErr.status != http.StatusBadRequest || apiErr.code != "unsupported_content_type"):
			t.Errorf("%q: %q, %v; want 400 unsupported_content_type", c.contentType, boundary, apiErr)
		}
	}
}

// mimeParts reads body as mime/multipart does, the reference: each part's name, its
// filename as its Content-Disposition names it (FileName would apply filepath.Base),
// its Content-Type and its bytes.
func mimeParts(body []byte, boundary string) ([]provider.FormPart, error) {
	r := multipart.NewReader(bytes.NewReader(body), boundary)
	var parts []provider.FormPart
	for {
		p, err := r.NextPart()
		if errors.Is(err, io.EOF) {
			return parts, nil
		}
		if err != nil {
			return nil, err
		}
		data, err := io.ReadAll(p)
		if err != nil {
			return nil, err
		}
		_, params, _ := mime.ParseMediaType(p.Header.Get("Content-Disposition")) // a part the scanner took has one it reads
		filename, file := params["filename"]
		part := provider.FormPart{Name: p.FormName(), File: file, Filename: filename, Body: data}
		if values, ok := p.Header["Content-Type"]; ok {
			part.ContentType = append([]byte{}, values[0]...)
		}
		parts = append(parts, part)
	}
}

// expectReadAsMime fails the test unless mime/multipart reads body as parts.
func expectReadAsMime(t *testing.T, body []byte, boundary string, parts []provider.FormPart) {
	t.Helper()
	want, err := mimeParts(body, boundary)
	if err != nil {
		t.Fatalf("mime/multipart refuses a body the scanner takes: %v\n%q", err, body)
	}
	if !sameParts(parts, want) {
		t.Fatalf("the scanner reads %+v, mime/multipart %+v\n%q", parts, want, body)
	}
}

func sameParts(a, b []provider.FormPart) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Name != b[i].Name || a[i].File != b[i].File || a[i].Filename != b[i].Filename || !bytes.Equal(a[i].Body, b[i].Body) ||
			(a[i].ContentType == nil) != (b[i].ContentType == nil) || !bytes.Equal(a[i].ContentType, b[i].ContentType) {
			return false
		}
	}
	return true
}

// formPieces are what the random forms are made of: pieces of delimiters, line ends,
// spaces and dashes, in every arrangement a reader could take for a boundary or not.
var formPieces = []string{"\r\n--" + testBoundary, "--" + testBoundary, "\r\n--" + testBoundary + "-", "\r\n--" + testBoundary + "--",
	"\r\n--" + testBoundary + "\r\n", "\r\n", "\n", "\r", " ", "\t", testBoundary, "-", "--", "\r\n--" + testBoundary[:3], "x", ":", ";", `"`}

// randomForm writes a form under testBoundary from rng: random parts — names from a
// small set, so some repeat; text and file parts; content types, other headers, header
// names in any case, spaces around values; bytes holding formPieces — sometimes after
// a preamble, before an epilogue. clean reports a form whose parts hold no piece of a
// delimiter, which the scanner must take as written (want).
func randomForm(rng *rand.Rand) (body []byte, want []wantPart, clean bool) {
	names := []string{"model", "file", "image[]", "prompt", `a"b`, `c\d`, "é", "x y;z"}
	clean = rng.IntN(2) == 0
	var s strings.Builder
	if !clean && rng.IntN(4) == 0 {
		for range rng.IntN(4) {
			s.WriteString(formPieces[rng.IntN(len(formPieces))])
		}
		s.WriteString("\r\n")
	}
	for range 1 + rng.IntN(4) {
		w := wantPart{name: names[rng.IntN(len(names))]}
		s.WriteString("--" + testBoundary + "\r\n")
		header := func(name, value string) {
			if rng.IntN(3) == 0 {
				name = strings.ToLower(name)
			}
			s.WriteString(name + ":" + []string{" ", "", "\t ", "  "}[rng.IntN(4)] + value + []string{"", " ", "\t"}[rng.IntN(3)] + "\r\n")
		}
		disposition := `form-data; name="` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(w.name) + `"`
		if rng.IntN(4) == 0 {
			disposition = "form-data; name*=UTF-8''" + percentEncoded(w.name)
		}
		if rng.IntN(2) == 0 {
			w.file, w.filename = true, []string{"a.wav", "", "C:\\x\\y.png", "ü.mp3"}[rng.IntN(4)]
			disposition += `; filename="` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(w.filename) + `"`
		}
		header("Content-Disposition", disposition)
		if rng.IntN(2) == 0 {
			w.contentType = ptr([]string{"audio/wav", "image/png; q=1", ""}[rng.IntN(3)])
			header("Content-Type", *w.contentType)
		}
		if rng.IntN(3) == 0 {
			header("X-Other", "v")
		}
		s.WriteString("\r\n")
		var data strings.Builder
		for range rng.IntN(6) {
			if clean {
				data.WriteString(strings.Repeat(string(rune('a'+rng.IntN(26))), 1+rng.IntN(20)))
				continue
			}
			data.WriteString(formPieces[rng.IntN(len(formPieces))])
		}
		w.body = data.String()
		s.WriteString(w.body + "\r\n")
		want = append(want, w)
	}
	s.WriteString("--" + testBoundary + "--")
	if rng.IntN(2) == 0 {
		s.WriteString("\r\n")
		if !clean {
			s.WriteString(formPieces[rng.IntN(len(formPieces))])
		}
	}
	body = []byte(s.String())
	if !clean && rng.IntN(2) == 0 {
		// Damage it: a piece put in, a byte changed or the end cut.
		at := rng.IntN(len(body) + 1)
		switch rng.IntN(3) {
		case 0:
			body = append(body[:at:at], append([]byte(formPieces[rng.IntN(len(formPieces))]), body[at:]...)...)
		case 1:
			if at < len(body) {
				body[at] = formPieces[rng.IntN(len(formPieces))][0]
			}
		case 2:
			body = body[:at]
		}
	}
	return body, want, clean
}

// percentEncoded is s with every byte but letters and digits percent-encoded, as an
// RFC 2231 value.
func percentEncoded(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if c := s[i]; 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' {
			b.WriteByte(c)
			continue
		}
		fmt.Fprintf(&b, "%%%02X", s[i])
	}
	return b.String()
}

// Differential: every body the scanner takes reads as the same parts in
// mime/multipart — over random forms, clean and messy, damaged or not. A clean form
// is taken as written, repeats aside.
func TestFormReadsAsMimeMultipart(t *testing.T) {
	rng := rand.New(rand.NewPCG(7, 11))
	taken, messyTaken := 0, 0
	const forms = 20000
	for range forms {
		body, want, clean := randomForm(rng)
		parts, apiErr := readForm(body, testBoundary, names(want))
		if clean {
			if apiErr != nil {
				t.Fatalf("a clean form refused: %s\n%q", apiErr.message, body)
			}
			expectParts(t, parts, want)
		}
		if apiErr != nil {
			continue
		}
		taken++
		if !clean {
			messyTaken++
		}
		expectReadAsMime(t, body, testBoundary, parts)
	}
	// The test means something only if messy forms are taken too.
	if messyTaken < forms/20 {
		t.Errorf("%d forms taken, %d of them messy: too few to compare", taken, messyTaken)
	}
}

// names are the names of parts: every one may repeat, so the differential is not
// cut short by a repeat.
func names(parts []wantPart) []string {
	var n []string
	for _, p := range parts {
		n = append(n, p.name)
	}
	return n
}

// The differential as a fuzz target: the seeds run with the tests, and `go test
// -fuzz FuzzFormReadsAsMimeMultipart` searches further.
func FuzzFormReadsAsMimeMultipart(f *testing.F) {
	f.Add([]byte(formOf(textPart("model", "pub"), filePart("file", "a.wav", "audio/wav", binaryBytes))))
	f.Add([]byte("pre\r\n" + formOf(textPart("a", "")) + "post"))
	f.Add([]byte("--" + testBoundary + "\r\nContent-Disposition: form-data; name=a\r\n\r\n--" + testBoundary + "--"))
	rng := rand.New(rand.NewPCG(1, 2))
	for range 20 {
		body, _, _ := randomForm(rng)
		f.Add(body)
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		if len(body) > 1<<20 {
			t.Skip()
		}
		parts, apiErr := readForm(body, testBoundary, nil)
		if apiErr != nil && apiErr.code == "duplicate_member" {
			parts, apiErr = readForm(body, testBoundary, allNames(body))
		}
		if apiErr != nil {
			return
		}
		expectReadAsMime(t, body, testBoundary, parts)
	})
}

// allNames lets every name a body's parts could have repeat: the fuzz target compares
// parts, and a repeat is a refusal of its own.
func allNames(body []byte) []string {
	parts, err := mimeParts(body, testBoundary)
	if err != nil {
		return nil
	}
	var n []string
	for _, p := range parts {
		n = append(n, p.Name)
	}
	return n
}
