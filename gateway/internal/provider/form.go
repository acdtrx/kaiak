package provider

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
)

// FormPart is one part of an upload's body, multipart/form-data, as the inbound stage
// read it (docs/specs/GATEWAY.md, Request pipeline → Uploads). Its byte fields are
// slices of the client's held body, never copies.
type FormPart struct {
	// Name is the part's name, as its Content-Disposition's parameters decode.
	Name string
	// File reports a file part: its Content-Disposition names a filename, Filename —
	// empty when the client named an empty one. A text part names none.
	File     bool
	Filename string
	// ContentType is the part's Content-Type header as the client sent it, without the
	// spaces around it; nil when the part has none.
	ContentType []byte
	// Body is the part's bytes as received.
	Body []byte
}

// The rebuilt upload body (docs/specs/GATEWAY.md, Request pipeline → Uploads): the
// backend gets a form the gateway writes — every part of the client's form in order,
// its bytes as received, under headers of the gateway's making (its name and filename
// in a Content-Disposition, its Content-Type when the client sent one, nothing else),
// the model part's value the deployment's model name, between a fresh boundary — so it
// reads exactly the parts the inbound stage read. The body is written as it is sent:
// only the part headers are made; the parts' bytes are the client's own, pieces of the
// upstream body, so nothing beyond the client's held body is copied, and the body's
// length is known before its first byte is sent.

// formBody is the upload body an attempt sends for req, and its Content-Type. Each
// attempt builds its own, under a boundary of its own: a retry resends the whole form.
func formBody(req *Request) (*upstreamBody, string) {
	boundary := formBoundary()
	model := []byte(req.Deployment.Model)
	// Every part's delimiter and headers, and the closing delimiter, in one buffer;
	// heads[i] is where part i's begin, the last entry where the closing one does.
	var written []byte
	heads := make([]int, 0, len(req.Form)+1)
	for i, part := range req.Form {
		heads = append(heads, len(written))
		if i > 0 {
			written = append(written, "\r\n"...)
		}
		written = append(written, "--"+boundary+"\r\nContent-Disposition: "+formDisposition(part)+"\r\n"...)
		if part.ContentType != nil {
			written = append(append(append(written, "Content-Type: "...), part.ContentType...), "\r\n"...)
		}
		written = append(written, "\r\n"...)
	}
	heads = append(heads, len(written))
	written = append(written, "\r\n--"+boundary+"--\r\n"...)

	pieces := make([][]byte, 0, 2*len(req.Form)+1)
	for i, part := range req.Form {
		value := part.Body
		if part.Name == "model" {
			value = model
		}
		pieces = append(pieces, written[heads[i]:heads[i+1]], value)
	}
	pieces = append(pieces, written[heads[len(req.Form)]:])
	return newUpstreamBody(pieces...), "multipart/form-data; boundary=" + boundary
}

// formDisposition is part's Content-Disposition: its name, and its filename for a file
// part, quoted as mime/multipart's writer quotes them (FileContentDisposition).
func formDisposition(part FormPart) string {
	d := `form-data; name="` + formQuoter.Replace(part.Name) + `"`
	if part.File {
		d += `; filename="` + formQuoter.Replace(part.Filename) + `"`
	}
	return d
}

// formQuoter escapes a quoted parameter value as mime/multipart's writer does: a
// backslash or a quote escaped by a backslash, CR and LF percent-encoded — the inbound
// stage refuses those in a name or filename, so they never reach a header.
var formQuoter = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\r", "%0D", "\n", "%0A")

// formBoundary is a fresh boundary, as mime/multipart's writer makes one: 30 random
// bytes in hex, which no client can have placed in a part.
func formBoundary() string {
	var b [30]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never returns an error
	return hex.EncodeToString(b[:])
}
