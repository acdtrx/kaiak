package provider

import (
	"bytes"
	"encoding/json"
)

// Responses carry the public model name the client asked for, not the backend's
// (docs/specs/GATEWAY.md, Relaying). The name is edited the way request bodies are:
// only the bytes of the "model" string value change, everything else passes as the
// backend sent it.
//
// A non-stream body is relayed in pieces as it arrives and may be large (embeddings),
// so it is never collected to be decoded whole: modelRewriter scans the JSON bytes as
// they pass, tracking just enough structure — nesting, strings, the keys of the
// objects it watches — to find the "model" members. Each stream chunk is one small
// JSON object and goes through a fresh modelRewriter.

// maxKeyCapture bounds the bytes of a key kept for comparison. "model" is at most 30
// bytes even with every character escaped (m…), and the nested member names
// are shorter still; longer keys cannot match.
const maxKeyCapture = 32

// modelRewriter replaces the string value of every top-level "model" member of one JSON
// object with a fixed string, fed the object's bytes in pieces of any size — and, when
// nested is set, the "model" members of the object that is the top-level member
// nested's value (a Messages message_start's message, a Responses event's response).
// Keys match exactly after decoding, as request keys do. A non-string model value,
// "model" keys anywhere else, and any input that is not a JSON object pass unchanged.
type modelRewriter struct {
	// replacement is the new value, an encoded JSON string.
	replacement []byte
	// nested names the top-level member whose object is watched too; "" for none.
	nested string

	depth int
	// opened: the outermost object or array began.
	opened bool
	// topIsObject: the outermost value is an object — its depth-1 strings in key
	// position are keys. An outermost array holds no keys to rewrite.
	topIsObject bool
	// inNested: the depth-2 object open now is nested's value, whose keys are watched.
	inNested bool
	inString bool
	escaped  bool
	// wantKey: in a watched object, the next string is a key (after "{" or ",").
	wantKey bool
	// capturing: the current string is a watched object's key, collected into key.
	capturing bool
	key       []byte
	// state tracks a matched key through its colon to its value.
	state rewriteState
	// skipping: inside the backend's model string, which is not emitted.
	skipping bool
}

type rewriteState int

const (
	stateNone        rewriteState = iota
	stateWantColon                // the key "model" just ended
	stateWantValue                // its colon passed
	stateNestedColon              // the top-level key nested just ended
	stateNestedValue              // its colon passed: an object here is watched
)

func newModelRewriter(replacement []byte) *modelRewriter {
	return &modelRewriter{replacement: replacement}
}

// newNestedModelRewriter is newModelRewriter that also rewrites the model of the
// object that is the top-level member nested's value.
func newNestedModelRewriter(replacement []byte, nested string) *modelRewriter {
	return &modelRewriter{replacement: replacement, nested: nested}
}

// watched reports whether the keys of the object open at the current depth are
// compared: the top-level object's, and nested's object's.
func (m *modelRewriter) watched() bool {
	return (m.depth == 1 && m.topIsObject) || (m.depth == 2 && m.inNested)
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
					switch {
					case m.keyIs("model"):
						m.state = stateWantColon
					case m.depth == 1 && m.nested != "" && m.keyIs(m.nested):
						m.state = stateNestedColon
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
			switch m.state {
			case stateWantColon:
				m.state = stateWantValue
				continue
			case stateNestedColon:
				m.state = stateNestedValue
				continue
			}
		case '"':
			m.inString = true
			if m.wantKey && m.watched() {
				m.capturing = true
				m.key = m.key[:0]
			}
		case '{', '[':
			if m.depth == 0 {
				m.topIsObject = c == '{'
				m.opened = true
			}
			m.depth++
			if m.depth == 2 && c == '{' && m.state == stateNestedValue {
				m.inNested = true
			}
			m.wantKey = c == '{' && m.watched()
		case '}', ']':
			if m.depth == 2 {
				m.inNested = false
			}
			m.depth--
		case ',':
			m.wantKey = m.watched()
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

// keyIs reports whether the captured key, still JSON-escaped, decodes to name.
func (m *modelRewriter) keyIs(name string) bool {
	if len(m.key) > maxKeyCapture {
		return false
	}
	if bytes.IndexByte(m.key, '\\') < 0 {
		return string(m.key) == name
	}
	var key string
	quoted := append(append([]byte{'"'}, m.key...), '"')
	return json.Unmarshal(quoted, &key) == nil && key == name
}
