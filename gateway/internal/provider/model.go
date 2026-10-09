package provider

import (
	"bytes"
	"encoding/json"
)

// Responses carry the public model name the client asked for, not the backend's
// (docs/specs/GATEWAY.md, Relaying). The name is edited the way request bodies are:
// only the bytes of the "model" string value change, everything else passes as the
// backend sent it. A speech batch answer's errored results carry the gateway's text
// in place of the backend's, edited the same way in the same pass.
//
// A non-stream body is relayed in pieces as it arrives and may be large (embeddings,
// a speech batch's audio), so it is never collected to be decoded whole: modelRewriter
// scans the JSON bytes as they pass, tracking just enough structure — nesting,
// strings, the keys of the objects it watches — to find the members it edits. Each
// stream chunk is one small JSON object and goes through a fresh modelRewriter.

// maxKeyCapture bounds the bytes of a key kept for comparison. "model" is at most 30
// bytes even with every character escaped (\u006d…), and the other member names
// watched ("message", "response", "results", "error") at most 48; longer keys cannot
// match.
const maxKeyCapture = 48

// modelRewriter replaces the string value of every top-level "model" member of one JSON
// object with a fixed string, fed the object's bytes in pieces of any size — and, when
// nested is set, the "model" members of the object that is the top-level member
// nested's value (a Messages message_start's message, a Responses event's response).
// When list is set, it also replaces the value of every member named listMember in the
// objects that are elements of an array that is a top-level member named list (a
// speech batch's results[].error) — a string, object or array, which can carry text;
// null, a number or a boolean carries none and passes. Keys match exactly after
// decoding, as request keys do, and every member of a watched name is edited — one
// named twice included. A non-string model value, keys anywhere else, and any input
// that is not a JSON object pass unchanged.
type modelRewriter struct {
	// replacement is the new value, an encoded JSON string.
	replacement []byte
	// nested names the top-level member whose object is watched too; "" for none.
	nested string
	// list names the top-level member whose array's object elements are watched for
	// listMember, whose value becomes listReplacement (an encoded JSON string); ""
	// for none.
	list, listMember string
	listReplacement  []byte

	depth int
	// opened: the outermost object or array began.
	opened bool
	// topIsObject: the outermost value is an object — its depth-1 strings in key
	// position are keys. An outermost array holds no keys to rewrite.
	topIsObject bool
	// inNested: the depth-2 object open now is nested's value, whose keys are watched.
	inNested bool
	// inList: the depth-2 array open now is list's value; inElement: the depth-3
	// object open now is one of its elements, whose keys are watched.
	inList, inElement bool

	inString bool
	escaped  bool
	// wantKey: in a watched object, the next string is a key (after "{" or ",").
	wantKey bool
	// capturing: the current string is a watched object's key, collected into key.
	capturing bool
	key       []byte
	// state tracks a matched key through its colon to its value.
	state rewriteState
	// skipping: inside the backend's string being replaced, which is not emitted.
	skipping bool
	// skipNest is the nesting inside the backend's object or array being replaced,
	// which is not emitted (0: none); skipInString: inside a string of it.
	skipNest     int
	skipInString bool
}

type rewriteState int

const (
	stateNone        rewriteState = iota
	stateWantColon                // the key "model" just ended
	stateWantValue                // its colon passed
	stateNestedColon              // the top-level key nested just ended
	stateNestedValue              // its colon passed: an object here is watched
	stateListColon                // the top-level key list just ended
	stateListValue                // its colon passed: an array here is watched
	stateMemberColon              // an element's key listMember just ended
	stateMemberValue              // its colon passed: its value is replaced
)

func newModelRewriter(replacement []byte) *modelRewriter {
	return &modelRewriter{replacement: replacement}
}

// newNestedModelRewriter is newModelRewriter that also rewrites the model of the
// object that is the top-level member nested's value.
func newNestedModelRewriter(replacement []byte, nested string) *modelRewriter {
	return &modelRewriter{replacement: replacement, nested: nested}
}

// newSpeechBatchRewriter is newModelRewriter that also replaces each speech batch
// result's error — every error member of the objects in the top-level results list —
// with itemError, an encoded JSON string.
func newSpeechBatchRewriter(replacement, itemError []byte) *modelRewriter {
	return &modelRewriter{replacement: replacement, list: "results", listMember: "error", listReplacement: itemError}
}

// watched reports whether the keys of the object open at the current depth are
// compared: the top-level object's, nested's object's, and the list's elements'.
func (m *modelRewriter) watched() bool {
	return (m.depth == 1 && m.topIsObject) || (m.depth == 2 && m.inNested) || (m.depth == 3 && m.inElement)
}

// rewrite appends piece, edited, to out and returns the result.
func (m *modelRewriter) rewrite(out, piece []byte) []byte {
	for _, c := range piece {
		if m.skipNest > 0 {
			m.skipContainer(c)
			continue
		}
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
					case m.depth <= 2 && m.keyIs("model"):
						m.state = stateWantColon
					case m.depth == 1 && m.nested != "" && m.keyIs(m.nested):
						m.state = stateNestedColon
					case m.depth == 1 && m.list != "" && m.keyIs(m.list):
						m.state = stateListColon
					case m.depth == 3 && m.keyIs(m.listMember):
						m.state = stateMemberColon
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
		if m.state == stateMemberValue && (c == '"' || c == '{' || c == '[') {
			out = append(out, m.listReplacement...)
			m.skipping, m.skipNest = c == '"', 0
			if c != '"' {
				m.skipNest = 1
			}
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
			case stateListColon:
				m.state = stateListValue
				continue
			case stateMemberColon:
				m.state = stateMemberValue
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
			switch {
			case m.depth == 2 && c == '{' && m.state == stateNestedValue:
				m.inNested = true
			case m.depth == 2 && c == '[' && m.state == stateListValue:
				m.inList = true
			case m.depth == 3 && c == '{' && m.inList:
				m.inElement = true
			}
			m.wantKey = c == '{' && m.watched()
		case '}', ']':
			switch m.depth {
			case 2:
				m.inNested, m.inList = false, false
			case 3:
				m.inElement = false
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

// skipContainer reads one byte of the backend's object or array being replaced,
// emitting nothing, until it closes.
func (m *modelRewriter) skipContainer(c byte) {
	switch {
	case m.skipInString:
		switch {
		case m.escaped:
			m.escaped = false
		case c == '\\':
			m.escaped = true
		case c == '"':
			m.skipInString = false
		}
	case c == '"':
		m.skipInString = true
	case c == '{' || c == '[':
		m.skipNest++
	case c == '}' || c == ']':
		m.skipNest--
	}
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
