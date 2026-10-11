package server

import (
	"bytes"
	"fmt"
	"mime"
	"slices"
	"strings"
	"unicode/utf8"

	"kaiak/internal/provider"
)

// An upload's body, multipart/form-data (docs/specs/GATEWAY.md, Request pipeline →
// Uploads), is read by a scanner over the held body: each part's headers are parsed,
// and its bytes stay a slice of the client's — never a copy, so an upload costs its
// size once, in the upload budget. The grammar is RFC 7578's, over RFC 2046's
// multipart body, held strictly — lines end in CRLF, a boundary line ends at its
// boundary, a header is never folded — so what lenient readers would each guess at
// differently is refused, and every body the scanner takes reads as the same parts in
// mime/multipart (the differential tests hold it to that). The parts are held to what
// the backends' own readers take as written: names and filenames in UTF-8 with no
// backslash or quote, no empty filename, text parts in UTF-8.

const (
	// maxFormParts caps a body's parts, files and fields together, within Starlette's
	// limits — 1000 fields and 1000 files, which vLLM and vLLM-Omni apply — so the
	// gateway refuses no form they would take; maxTextPartBytes is Starlette's limit on
	// a field.
	maxFormParts     = 1000
	maxTextPartBytes = 1 << 20
	// maxPartHeaderBytes bounds a part's header block, its blank line included: a
	// name, a filename and a content type take a few hundred bytes.
	maxPartHeaderBytes = 8 << 10
	// maxPreambleBytes bounds what comes before the first boundary line. Clients send
	// nothing there, and it is ignored; the bound keeps it within the line
	// mime/multipart reads a preamble by, so a body the scanner takes it takes too.
	maxPreambleBytes = 4 << 10
	// maxBoundaryLength is RFC 2046's longest boundary.
	maxBoundaryLength = 70
)

var crlf = []byte("\r\n")

// uploadBoundary reads an upload's Content-Type: multipart/form-data with a boundary
// RFC 2046 allows (validBoundary), else 400 unsupported_content_type.
func uploadBoundary(contentType string) (string, *apiError) {
	kind, params, err := mime.ParseMediaType(contentType)
	if err != nil || kind != "multipart/form-data" || !validBoundary(params["boundary"]) {
		return "", errUnsupportedContentType()
	}
	return params["boundary"], nil
}

// validBoundary reports whether b is a boundary as RFC 2046 has it: 1 to 70 of its
// characters — letters, digits and '()+_,-./:=? and the space — not ending in a space.
func validBoundary(b string) bool {
	if b == "" || len(b) > maxBoundaryLength || b[len(b)-1] == ' ' {
		return false
	}
	for i := 0; i < len(b); i++ {
		c := b[i]
		if !('a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || strings.IndexByte("'()+_,-./:=? ", c) >= 0) {
			return false
		}
	}
	return true
}

// readForm reads body, a multipart/form-data body under boundary, into its parts, in
// order. A preamble and an epilogue are skipped. lists are the names of the parts
// that may repeat; any other name repeated is refused, 400 duplicate_member — the
// gateway and the backend could read different occurrences. A body breaking the
// grammar is refused, 400 invalid_body, its reason never quoting the body; so is a
// text part that is not UTF-8 — llama-server fails on one (500), a failure a client
// could cause at will.
func readForm(body []byte, boundary string, lists []string) ([]provider.FormPart, *apiError) {
	delimiter := []byte("\r\n--" + boundary)
	pos, apiErr := firstPart(body, delimiter[2:])
	if apiErr != nil {
		return nil, apiErr
	}
	var parts []provider.FormPart
	for {
		if len(parts) == maxFormParts {
			return nil, errInvalidForm(fmt.Sprintf("it holds more than %d parts", maxFormParts))
		}
		part, start, apiErr := readPartHeaders(body, pos)
		if apiErr != nil {
			return nil, apiErr
		}
		end, next, apiErr := partEnd(body, start, delimiter)
		if apiErr != nil {
			return nil, apiErr
		}
		part.Body = body[start:end]
		if !part.File && len(part.Body) > maxTextPartBytes {
			return nil, errInvalidForm(fmt.Sprintf("a text part is over %d bytes", maxTextPartBytes))
		}
		if !part.File && !utf8.Valid(part.Body) {
			return nil, errInvalidForm("a text part is not UTF-8")
		}
		parts = append(parts, part)
		if next < 0 {
			break
		}
		pos = next
	}
	seen := make(map[string]bool, len(parts))
	for _, part := range parts {
		if slices.Contains(lists, part.Name) {
			continue
		}
		if seen[part.Name] {
			return nil, errDuplicateMember(part.Name)
		}
		seen[part.Name] = true
	}
	return parts, nil
}

// firstPart finds the form's first boundary line — dash, the boundary after its two
// dashes, at the body's start or at a line's after a preamble ending in CRLF — and
// returns where the first part's headers begin. The first line to start with dash is
// that line, or the body is refused: a reader skipping it would find another.
func firstPart(body, dash []byte) (int, *apiError) {
	start := 0
	if !bytes.HasPrefix(body, dash) {
		window := body[:min(len(body), maxPreambleBytes+len(dash))]
		i := bytes.Index(window, append([]byte("\n"), dash...))
		if i < 0 {
			return 0, errInvalidForm(fmt.Sprintf("no boundary line opens it within its first %d bytes", maxPreambleBytes))
		}
		start = i + 1
		if !bytes.HasSuffix(body[:start], crlf) {
			return 0, errInvalidForm("its preamble does not end with CRLF")
		}
	}
	rest := body[start+len(dash):]
	switch {
	case bytes.HasPrefix(rest, crlf):
		return start + len(dash) + len(crlf), nil
	case bytes.HasPrefix(rest, []byte("--")):
		return 0, errInvalidForm("it holds no part")
	}
	return 0, errInvalidForm("a boundary line does not end at its boundary")
}

// readPartHeaders reads the headers of the part beginning at pos, up to the blank line
// ending them, and returns the part as they describe it and where its bytes begin. Its
// Content-Disposition (readDisposition) and Content-Type are read, each once; a
// Content-Transfer-Encoding is refused — RFC 7578 deprecates it, and readers disagree
// on decoding it; any other header is skipped, and the rebuilt body leaves it out.
func readPartHeaders(body []byte, pos int) (provider.FormPart, int, *apiError) {
	var part provider.FormPart
	disposition := false
	limit := min(len(body), pos+maxPartHeaderBytes)
	for at := pos; ; {
		eol := bytes.Index(body[at:limit], crlf)
		if eol < 0 {
			if limit == len(body) {
				return part, 0, errInvalidForm("it ends before its closing boundary")
			}
			return part, 0, errInvalidForm(fmt.Sprintf("a part's headers are over %d bytes", maxPartHeaderBytes))
		}
		line := body[at : at+eol]
		at += eol + len(crlf)
		if len(line) == 0 {
			if !disposition {
				return part, 0, errInvalidForm("a part has no Content-Disposition")
			}
			return part, at, nil
		}
		name, value, ok := partHeader(line)
		switch {
		case !ok:
			return part, 0, errInvalidForm("a part header is malformed")
		case bytes.EqualFold(name, []byte("Content-Disposition")):
			if disposition {
				return part, 0, errInvalidForm("a part has two Content-Disposition headers")
			}
			disposition = true
			if apiErr := readDisposition(value, &part); apiErr != nil {
				return part, 0, apiErr
			}
		case bytes.EqualFold(name, []byte("Content-Type")):
			if part.ContentType != nil {
				return part, 0, errInvalidForm("a part has two Content-Type headers")
			}
			part.ContentType = value
		case bytes.EqualFold(name, []byte("Content-Transfer-Encoding")):
			return part, 0, errInvalidForm("a part has a Content-Transfer-Encoding")
		}
	}
}

// partHeader splits a header line into its name and its value, without the spaces and
// tabs around it: RFC 7230's header field — a token, a colon, a value of visible
// characters, spaces and tabs. A line folding the one before it (starting with a space
// or a tab, which RFC 7230 deprecates) has no name, and is refused. value is never nil.
func partHeader(line []byte) (name, value []byte, ok bool) {
	colon := bytes.IndexByte(line, ':')
	if colon <= 0 {
		return nil, nil, false
	}
	name = line[:colon]
	for _, c := range name {
		if !('a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0) {
			return nil, nil, false
		}
	}
	value = line[colon+1:]
	for len(value) > 0 && (value[0] == ' ' || value[0] == '\t') {
		value = value[1:]
	}
	for len(value) > 0 && (value[len(value)-1] == ' ' || value[len(value)-1] == '\t') {
		value = value[:len(value)-1]
	}
	for _, c := range value {
		if c < ' ' && c != '\t' || c == 0x7f {
			return nil, nil, false
		}
	}
	return name, value, true
}

// readDisposition reads a part's Content-Disposition into part: form-data, with a
// name, and a filename for a file part — its parameters decoded as mime/multipart
// decodes them. What the rebuilt body could not carry as read, or a backend would read
// otherwise, is refused:
//   - a control character in a name or a filename: a header cannot hold it;
//   - a backslash or a quote: a quoted value carries one only escaped, and the
//     backends' readers do not read the escape as mime/multipart's writer writes it —
//     python-multipart (vLLM, vLLM-Omni) ends a name ending in a backslash at the
//     escaped quote and reads the filename after it as other parameters, a name among
//     them, so a file part would reach the backend as another field; cpp-httplib
//     (llama-server) keeps the backslashes;
//   - a name or a filename that is not UTF-8: llama-server fails on one (500), a
//     failure a client could cause at will;
//   - an empty filename: cpp-httplib reads a part with one as a text field, so a
//     prompt sent that way passes the gateway's reading as a file — uncounted in the
//     input estimate, past the text part's cap — and a file sent that way is no file
//     to llama-server.
func readDisposition(value []byte, part *provider.FormPart) *apiError {
	kind, params, err := mime.ParseMediaType(string(value))
	switch {
	case err != nil:
		return errInvalidForm("a part's Content-Disposition cannot be read")
	case kind != "form-data":
		return errInvalidForm("a part's Content-Disposition is not form-data")
	}
	name := params["name"]
	if name == "" {
		return errInvalidForm("a part has no name")
	}
	filename, file := params["filename"]
	switch {
	case hasControl(name) || hasControl(filename):
		return errInvalidForm("a part's name or filename holds a control character")
	case strings.ContainsAny(name, `\"`) || strings.ContainsAny(filename, `\"`):
		return errInvalidForm("a part's name or filename holds a backslash or a quote")
	case !utf8.ValidString(name) || !utf8.ValidString(filename):
		return errInvalidForm("a part's name or filename is not UTF-8")
	case file && filename == "":
		return errInvalidForm("a part's filename is empty")
	}
	part.Name, part.File, part.Filename = name, file, filename
	return nil
}

// hasControl reports whether s holds an ASCII control character.
func hasControl(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < ' ' || s[i] == 0x7f {
			return true
		}
	}
	return false
}

// partEnd finds where the part whose bytes begin at start ends: at the first
// delimiter — a CRLF and the boundary after two dashes — that a boundary line
// follows. The search begins at the CRLF ending the part's headers, which also begins
// the delimiter of a part with no bytes. next is where the next part's headers begin,
// -1 when the delimiter closed the form (an epilogue after it is skipped). The
// delimiter followed by a space, a tab, a lone CR or LF, or nothing, is refused:
// readers disagree on whether it ends the part.
func partEnd(body []byte, start int, delimiter []byte) (end, next int, apiErr *apiError) {
	for from := start - len(crlf); ; {
		i := bytes.Index(body[from:], delimiter)
		if i < 0 {
			return 0, 0, errInvalidForm("it ends before its closing boundary")
		}
		at := from + i
		rest := body[at+len(delimiter):]
		switch {
		case bytes.HasPrefix(rest, crlf):
			return max(at, start), at + len(delimiter) + len(crlf), nil
		case bytes.HasPrefix(rest, []byte("--")):
			if after := rest[2:]; len(after) > 0 && !bytes.HasPrefix(after, crlf) {
				return 0, 0, errInvalidForm("a boundary line does not end at its boundary")
			}
			return max(at, start), -1, nil
		case len(rest) == 0:
			return 0, 0, errInvalidForm("it ends before its closing boundary")
		case rest[0] == ' ' || rest[0] == '\t' || rest[0] == '\r' || rest[0] == '\n':
			return 0, 0, errInvalidForm("a boundary line does not end at its boundary")
		}
		// The boundary is the start of a longer word: the part's own bytes.
		from = at + len(delimiter)
	}
}
