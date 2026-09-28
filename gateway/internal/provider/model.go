package provider

import (
	"bytes"
	"encoding/json"
)

// Responses carry the public model name the client asked for, not the backend's
// (docs/specs/GATEWAY.md, Relaying). The name is edited the way request bodies are:
// only the bytes of the top-level "model" string value change, everything else passes
// as the backend sent it.
//
// A non-stream body is relayed in pieces as it arrives and may be large (embeddings),
// so it is never collected to be decoded whole: modelRewriter scans the JSON bytes as
// they pass, tracking just enough structure — nesting, strings, top-level keys — to
// find the top-level "model" members. Each stream chunk is one small JSON object and
// goes through a fresh modelRewriter.

// maxKeyCapture bounds the bytes of a top-level key kept for comparison. "model" is at
// most 30 bytes even with every character escaped (m…); longer keys cannot match.
const maxKeyCapture = 32

// modelRewriter replaces the string value of every top-level "model" member of one JSON
// object with a fixed string, fed the object's bytes in pieces of any size. Keys match
// exactly after decoding, as request keys do. A non-string model value, nested "model"
// keys, and any input that is not a JSON object pass unchanged.
type modelRewriter struct {
	// replacement is the new value, an encoded JSON string.
	replacement []byte

	depth int
	// opened: the outermost object or array began.
	opened bool
	// topIsObject: the outermost value is an object — its depth-1 strings in key
	// position are keys. An outermost array holds no keys to rewrite.
	topIsObject bool
	inString    bool
	escaped     bool
	// wantKey: at depth 1 the next string is a key (after "{" or ",").
	wantKey bool
	// capturing: the current string is a top-level key, collected into key.
	capturing bool
	key       []byte
	// state tracks a matched "model" key through its colon to its value.
	state rewriteState
	// skipping: inside the backend's model string, which is not emitted.
	skipping bool
}

type rewriteState int

const (
	stateNone      rewriteState = iota
	stateWantColon              // the key "model" just ended
	stateWantValue              // its colon passed
)

func newModelRewriter(replacement []byte) *modelRewriter {
	return &modelRewriter{replacement: replacement}
}

// rewrite appends piece, edited, to out and returns the result.
func (m *modelRewriter) rewrite(out, piece []byte) []byte {
	for _, c := range piece {
		if m.skipping {
			switch {
			case m.escaped:
				m.escaped = false
			case c == '\\':
				m.escaped = true
			case c == '"':
				m.skipping = false
			}
			continue
		}
		if m.inString {
			out = append(out, c)
			switch {
			case m.escaped:
				m.escaped = false
			case c == '\\':
				m.escaped = true
			case c == '"':
				m.inString = false
				if m.capturing {
					m.capturing = false
					if m.keyIsModel() {
						m.state = stateWantColon
					}
				}
				continue
			}
			if m.capturing && len(m.key) <= maxKeyCapture {
				m.key = append(m.key, c)
			}
			continue
		}
		if m.state == stateWantValue && c == '"' {
			out = append(out, m.replacement...)
			m.skipping = true
			m.state = stateNone
			continue
		}
		out = append(out, c)
		switch c {
		case ' ', '\t', '\n', '\r':
			continue
		case ':':
			if m.state == stateWantColon {
				m.state = stateWantValue
				continue
			}
		case '"':
			m.inString = true
			if m.depth == 1 && m.topIsObject && m.wantKey {
				m.capturing = true
				m.key = m.key[:0]
			}
		case '{', '[':
			if m.depth == 0 {
				m.topIsObject = c == '{'
				m.opened = true
			}
			m.depth++
			m.wantKey = m.depth == 1 && c == '{'
		case '}', ']':
			m.depth--
		case ',':
			m.wantKey = m.depth == 1
		}
		m.state = stateNone
		if c != ',' && c != '{' {
			m.wantKey = false
		}
	}
	return out
}

// closed reports whether the input so far holds a whole JSON object or array: its
// outermost value began and closed again.
func (m *modelRewriter) closed() bool {
	return m.opened && m.depth <= 0 && !m.inString && !m.skipping
}

// keyIsModel reports whether the captured key, still JSON-escaped, decodes to "model".
func (m *modelRewriter) keyIsModel() bool {
	if len(m.key) > maxKeyCapture {
		return false
	}
	if bytes.IndexByte(m.key, '\\') < 0 {
		return string(m.key) == "model"
	}
	var key string
	quoted := append(append([]byte{'"'}, m.key...), '"')
	return json.Unmarshal(quoted, &key) == nil && key == "model"
}
