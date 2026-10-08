package accounting

import (
	"bytes"
	"encoding/json"
)

// A non-stream response body is relayed in pieces as it arrives and may be large
// (embeddings), so it is never collected whole to be decoded: memberScanner reads the
// JSON bytes as they pass, tracking just enough structure — nesting, strings,
// top-level keys — to keep the raw bytes of the top-level members the meter names:
// "usage" and the response format's content member ("choices" for OpenAI, "content"
// for Messages, "output" for Responses; a rerank answer generates none). Values are
// read from where each format puts them, never searched for by what they look like.

// maxScanKey bounds the bytes of a top-level key kept for comparison: the keys wanted
// are short, so a longer key cannot match even with every character escaped.
const maxScanKey = 64

type scanPhase int

const (
	scanStart      scanPhase = iota // before the outermost value
	scanKey                         // expecting a key or the object's end
	scanKeyString                   // inside a key
	scanColon                       // after a key
	scanValue                       // expecting a member's value
	scanInValue                     // inside a member's value
	scanAfterValue                  // expecting "," or the object's end
	scanDone                        // the object ended, or the body is not an object
)

// memberCapture is one wanted member's raw value bytes.
type memberCapture struct {
	limit int
	value []byte
	// size counts every byte of the value, kept or not.
	size int
	// complete: the value ended within the body and every byte was kept.
	complete bool
}

// memberScanner captures the raw values of chosen top-level members of one JSON
// object fed in pieces of any size. A member that repeats keeps its last value, as
// encoding/json does.
type memberScanner struct {
	captures map[string]*memberCapture

	phase   scanPhase
	key     []byte
	escaped bool
	// current is the capture the value being read goes to; nil when unwanted.
	current *memberCapture
	// Inside a value: container nesting, string state, and whether it is a bare
	// scalar (number, true, false, null), which ends at the first byte after it.
	depth    int
	inString bool
	scalar   bool
}

// newMemberScanner returns a scanner keeping the members named in limits, each up to
// its limit in bytes.
func newMemberScanner(limits map[string]int) *memberScanner {
	s := &memberScanner{captures: make(map[string]*memberCapture, len(limits))}
	for name, limit := range limits {
		s.captures[name] = &memberCapture{limit: limit}
	}
	return s
}

func (s *memberScanner) feed(piece []byte) {
	for _, c := range piece {
		s.step(c)
	}
}

// member returns the wanted member's value when it was read whole.
func (s *memberScanner) member(name string) ([]byte, bool) {
	c := s.captures[name]
	if c == nil || !c.complete {
		return nil, false
	}
	return c.value, true
}

// memberSize returns how many bytes the member's value took, kept or not (0 when it
// never appeared).
func (s *memberScanner) memberSize(name string) int {
	if c := s.captures[name]; c != nil {
		return c.size
	}
	return 0
}

func isJSONSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

func (s *memberScanner) step(c byte) {
	switch s.phase {
	case scanStart:
		switch {
		case isJSONSpace(c):
		case c == '{':
			s.phase = scanKey
		default:
			s.phase = scanDone
		}
	case scanKey:
		switch {
		case isJSONSpace(c):
		case c == '"':
			s.phase = scanKeyString
			s.key = s.key[:0]
			s.escaped = false
		default:
			s.phase = scanDone
		}
	case scanKeyString:
		switch {
		case s.escaped:
			s.escaped = false
		case c == '\\':
			s.escaped = true
		case c == '"':
			s.phase = scanColon
			return
		}
		if len(s.key) <= maxScanKey {
			s.key = append(s.key, c)
		}
	case scanColon:
		switch {
		case isJSONSpace(c):
		case c == ':':
			s.phase = scanValue
			s.current = s.wanted()
		default:
			s.phase = scanDone
		}
	case scanValue:
		if isJSONSpace(c) {
			return
		}
		s.phase = scanInValue
		s.depth = 0
		s.inString = false
		s.escaped = false
		s.scalar = c != '{' && c != '[' && c != '"'
		if s.current != nil {
			s.current.value = s.current.value[:0]
			s.current.size = 0
			s.current.complete = false
		}
		s.valueByte(c)
	case scanInValue:
		s.valueByte(c)
	case scanAfterValue:
		switch {
		case isJSONSpace(c):
		case c == ',':
			s.phase = scanKey
		default:
			s.phase = scanDone
		}
	}
}

// valueByte reads one byte of a member's value.
func (s *memberScanner) valueByte(c byte) {
	if s.scalar {
		if isJSONSpace(c) || c == ',' || c == '}' {
			s.endValue()
			s.step(c)
			return
		}
		s.keep(c)
		return
	}
	s.keep(c)
	if s.inString {
		switch {
		case s.escaped:
			s.escaped = false
		case c == '\\':
			s.escaped = true
		case c == '"':
			s.inString = false
			if s.depth == 0 {
				s.endValue()
			}
		}
		return
	}
	switch c {
	case '"':
		s.inString = true
	case '{', '[':
		s.depth++
	case '}', ']':
		s.depth--
		if s.depth == 0 {
			s.endValue()
		}
	}
}

func (s *memberScanner) keep(c byte) {
	if s.current == nil {
		return
	}
	s.current.size++
	if len(s.current.value) < s.current.limit {
		s.current.value = append(s.current.value, c)
	}
}

func (s *memberScanner) endValue() {
	if s.current != nil {
		s.current.complete = s.current.size == len(s.current.value)
		s.current = nil
	}
	s.phase = scanAfterValue
}

// wanted returns the capture for the key just read, if it is one of the wanted
// members. Keys match exactly after decoding.
func (s *memberScanner) wanted() *memberCapture {
	if len(s.key) > maxScanKey {
		return nil
	}
	if bytes.IndexByte(s.key, '\\') < 0 {
		return s.captures[string(s.key)]
	}
	var key string
	quoted := append(append([]byte{'"'}, s.key...), '"')
	if json.Unmarshal(quoted, &key) != nil {
		return nil
	}
	return s.captures[key]
}
