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
	key, ok := scannedName(s.key)
	if !ok {
		return nil
	}
	return s.captures[key]
}

// scannedName is a member name a one-pass reader read — its bytes between the quotes,
// as sent, kept up to maxScanKey+1 bytes — decoded, as every JSON decoder reads it;
// ok is false for a name longer than any a reader looks for, or one that does not
// decode.
func scannedName(key []byte) (string, bool) {
	if len(key) > maxScanKey {
		return "", false
	}
	if bytes.IndexByte(key, '\\') < 0 {
		return string(key), true
	}
	var name string
	quoted := append(append([]byte{'"'}, key...), '"')
	if json.Unmarshal(quoted, &name) != nil {
		return "", false
	}
	return name, true
}

// valueSkip follows one JSON value of a body read in one pass as it passes — its
// nesting and strings, a string's characters a run at a time, so a value of many MiB
// (base64 media) costs one pass and nothing held — keeping its bytes into keep, up to
// keep's limit, when keep is set. A scalar (number, true, false, null) ends at the
// first byte after it.
type valueSkip struct {
	depth                     int
	inString, escaped, scalar bool
	keep                      *memberCapture
}

// start begins the value whose first byte is c, which run reads; keep, unless nil,
// keeps the value.
func (s *valueSkip) start(c byte, keep *memberCapture) {
	*s = valueSkip{scalar: c != '{' && c != '[' && c != '"', keep: keep}
	if keep != nil {
		keep.value, keep.size, keep.complete = keep.value[:0], 0, false
	}
}

// run reads the value from p and returns what follows it, and whether the value
// ended: a scalar's end is the byte after it, which is not its own.
func (s *valueSkip) run(p []byte) (rest []byte, ended bool) {
	for len(p) > 0 {
		if s.inString && !s.escaped {
			n := stringRun(p)
			s.keepBytes(p[:n])
			if p = p[n:]; len(p) == 0 {
				return p, false
			}
		}
		c := p[0]
		switch {
		case s.scalar:
			if isJSONSpace(c) || c == ',' || c == '}' || c == ']' {
				s.end()
				return p, true
			}
		case s.inString:
			switch {
			case s.escaped:
				s.escaped = false
			case c == '\\':
				s.escaped = true
			case c == '"':
				s.inString = false
				ended = s.depth == 0
			}
		default:
			switch c {
			case '"':
				s.inString = true
			case '{', '[':
				s.depth++
			case '}', ']':
				s.depth--
				ended = s.depth == 0
			}
		}
		s.keepBytes(p[:1])
		p = p[1:]
		if ended {
			s.end()
			return p, true
		}
	}
	return p, false
}

// keepBytes keeps b, the value's next bytes, when the value is kept: up to its limit,
// every byte counted.
func (s *valueSkip) keepBytes(b []byte) {
	k := s.keep
	if k == nil {
		return
	}
	k.size += len(b)
	if room := k.limit - len(k.value); room > 0 {
		k.value = append(k.value, b[:min(room, len(b))]...)
	}
}

// end ends the value: a kept one is complete when every byte was kept.
func (s *valueSkip) end() {
	if s.keep != nil {
		s.keep.complete = s.keep.size == len(s.keep.value)
		s.keep = nil
	}
}

// stringRun is how many bytes of p, inside a JSON string, are its characters before
// the next quote or backslash.
func stringRun(p []byte) int {
	end := bytes.IndexByte(p, '"')
	if end < 0 {
		end = len(p)
	}
	if escape := bytes.IndexByte(p[:end], '\\'); escape >= 0 {
		return escape
	}
	return end
}
