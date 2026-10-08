package provider

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"
)

// Request bodies are edited by splicing: the object's members are indexed by byte
// offset and only the values of owned keys are replaced (or a member inserted). Every
// other byte of the client's body — unknown fields, their values, key order,
// whitespace — reaches the backend unchanged. Decoding into Go values and encoding
// again would reorder keys and could round numbers, so it is never done.

// member is one member of a JSON object: its key (decoded) and the byte span of its
// value.
type member struct {
	key        string
	start, end int
}

// objectIndex locates the members of a JSON object held in a byte slice.
type objectIndex struct {
	members []member
	// close is the offset of the closing brace.
	close int
}

// indexObject indexes data, which must hold exactly one JSON object. Keys are matched
// exactly after decoding, as the inbound stage reads them.
func indexObject(data []byte) (objectIndex, error) {
	var idx objectIndex
	dec := json.NewDecoder(bytes.NewReader(data))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return idx, errors.New("not a JSON object")
	}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return idx, err
		}
		key, _ := tok.(string) // inside an object, More() true means a key comes next
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return idx, err
		}
		end := int(dec.InputOffset())
		idx.members = append(idx.members, member{key: key, start: end - len(value), end: end})
	}
	if _, err := dec.Token(); err != nil {
		return idx, err
	}
	idx.close = int(dec.InputOffset()) - 1
	if _, err := dec.Token(); err == nil {
		return idx, errors.New("data after the JSON object")
	}
	return idx, nil
}

// memberEdit changes one member of an object: set returns the member's new value
// given its current one (nil when the object has no such member). A nil value for an
// absent member leaves it absent.
type memberEdit struct {
	key string
	set func(current []byte) ([]byte, error)
}

// setValue is a memberEdit that gives key a fixed value, replacing or adding it.
func setValue(key string, value []byte) memberEdit {
	return memberEdit{key: key, set: func([]byte) ([]byte, error) { return value, nil }}
}

// editObject applies edits to the JSON object in data and returns the result: each
// edited key's one member is replaced, or added after the last member. An edited key
// present several times is an error — a backend could read either occurrence, and
// editing every one would let a body of repeats grow by one edit per repeat. The
// inbound stage refuses a repeated top-level member before any edit (400
// duplicate_member); a nested object the gateway edits (stream_options) is refused here.
func editObject(data []byte, edits ...memberEdit) ([]byte, error) {
	idx, err := indexObject(data)
	if err != nil {
		return nil, err
	}
	type splice struct {
		start, end int
		text       []byte
	}
	var splices []splice
	for _, e := range edits {
		found := false
		for _, m := range idx.members {
			if m.key != e.key {
				continue
			}
			if found {
				return nil, fmt.Errorf("%s: member appears more than once", e.key)
			}
			found = true
			value, err := e.set(data[m.start:m.end])
			if err != nil {
				return nil, fmt.Errorf("%s: %w", e.key, err)
			}
			splices = append(splices, splice{m.start, m.end, value})
		}
		if found {
			continue
		}
		value, err := e.set(nil)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", e.key, err)
		}
		if value == nil {
			continue
		}
		key, _ := json.Marshal(e.key) // a string always encodes
		text := slices.Concat(key, []byte(":"), value)
		at := idx.close
		if n := len(idx.members); n > 0 {
			at = idx.members[n-1].end
			text = slices.Concat([]byte(","), text)
		}
		splices = append(splices, splice{at, at, text})
	}
	if len(splices) == 0 {
		return data, nil
	}
	// Splices never overlap (each member is edited by one edit at most, inserts go
	// after the last member): order them by position, inserts at the same offset in
	// edit order.
	slices.SortStableFunc(splices, func(a, b splice) int { return a.start - b.start })
	out := make([]byte, 0, len(data)+64)
	pos := 0
	for _, s := range splices {
		out = append(out, data[pos:s.start]...)
		out = append(out, s.text...)
		pos = s.end
	}
	return append(out, data[pos:]...), nil
}

// setIncludeUsage returns a stream_options value with include_usage true: the
// client's object with that one member edited or added, or a new object when the
// client sent none (absent or null).
func setIncludeUsage(current []byte) ([]byte, error) {
	if current == nil || string(current) == "null" {
		return []byte(`{"include_usage":true}`), nil
	}
	return editObject(current, setValue("include_usage", []byte("true")))
}

// passthroughBody applies the gateway's owned edits to the client's body: the model
// name becomes the deployment's, the module's own edits (extra) and the pipeline's
// parameters are set, then the format's (streamFormat.requestEdits). stripUsage is
// true when a format edit asked for the usage-only chunk the client did not, so it
// must not reach the client.
func passthroughBody(req *Request, extra ...memberEdit) (body []byte, stripUsage bool, err error) {
	model, _ := json.Marshal(req.Deployment.Model) // a string always encodes
	edits := append([]memberEdit{setValue("model", model)}, extra...)
	for _, p := range req.Params {
		edits = append(edits, setValue(p.Key, p.Value))
	}
	formatEdits, stripUsage := newStreamFormat(req.Endpoint.Format()).requestEdits(req)
	body, err = editObject(req.Body, append(edits, formatEdits...)...)
	return body, stripUsage, err
}

// standardServiceTier is the edit that keeps a request on standard processing, for
// the modules whose backends bill by tier (openai.go, azure_openai.go;
// docs/specs/GATEWAY.md, Providers → Service tier): prices are standard-tier rates,
// and a priority request is billed about twice what its record would say. A chat
// completions or Responses request always carries service_tier "default" — an absent
// tier means "auto", which follows the deployment's or project's own setting. On
// completions and embeddings a client's tier becomes "default" and an absent one
// stays absent: OpenAI refuses parameters an endpoint does not define. Responses
// token counting is left as the client sent it: nothing is generated or billed there.
func standardServiceTier(e Endpoint) memberEdit {
	return memberEdit{key: "service_tier", set: func(current []byte) ([]byte, error) {
		switch {
		case e == ResponsesInputTokens:
			return current, nil
		case current == nil && e != ChatCompletions && e != Responses:
			return nil, nil
		}
		return []byte(`"default"`), nil
	}}
}

// upstreamBody is the edited request body an upstream request sends. The transport
// may read it more than once (GetBody: a pooled connection found dead as the request
// was written) until the response arrives; release then drops it, so a response that
// streams for minutes holds no copy of the prompt. A reader handed out before release
// keeps the bytes only until the transport has read or closed it.
type upstreamBody struct {
	size int64

	mu   sync.Mutex
	data []byte
}

// errBodyReleased: the transport asked to send the body again after the response
// arrived.
var errBodyReleased = errors.New("request body already released")

func newUpstreamBody(data []byte) *upstreamBody {
	return &upstreamBody{size: int64(len(data)), data: data}
}

// reader returns a reader over the body.
func (b *upstreamBody) reader() *upstreamBodyReader {
	b.mu.Lock()
	defer b.mu.Unlock()
	return &upstreamBodyReader{data: b.data}
}

// getBody is the upstream request's GetBody.
func (b *upstreamBody) getBody() (io.ReadCloser, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.data == nil {
		return nil, errBodyReleased
	}
	return &upstreamBodyReader{data: b.data}, nil
}

// release drops the body.
func (b *upstreamBody) release() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.data = nil
}

// upstreamBodyReader reads an upstream body once and drops its bytes as soon as the
// last one is read, or when it is closed. The transport may close it from another
// goroutine than the one reading.
type upstreamBodyReader struct {
	mu   sync.Mutex
	data []byte
	off  int
}

func (r *upstreamBodyReader) Read(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.off >= len(r.data) {
		r.data = nil
		return 0, io.EOF
	}
	n := copy(p, r.data[r.off:])
	r.off += n
	if r.off == len(r.data) {
		r.data = nil
	}
	return n, nil
}

func (r *upstreamBodyReader) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.data = nil
	return nil
}

// startsWith reports whether a JSON value, past leading whitespace, begins with c.
func startsWith(raw []byte, c byte) bool {
	raw = bytes.TrimLeft(raw, " \t\r\n")
	return len(raw) > 0 && raw[0] == c
}
