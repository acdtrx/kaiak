package capture

import (
	"bytes"
	"encoding/json"
	"iter"

	"kaiak/internal/sse"
	"kaiak/internal/telemetry/otlptrace"
)

// Reading what clients and backends sent. Every API's mapping reads its JSON through
// these, by exact member name — as the inbound stage reads the owned fields, and as
// backends read a request: encoding/json's struct fields would also match a member
// whose name differs in case — and each value only when it has the type the mapping
// reads, so that content of any other shape is left out, never misread
// (docs/specs/GATEWAY.md, Request capture → Content). A member named twice is its
// last occurrence, as backends read it.

// members is raw's members when it is a JSON object; false for anything else.
func members(raw []byte) (map[string]json.RawMessage, bool) {
	raw = trimSpace(raw)
	if len(raw) == 0 || raw[0] != '{' {
		return nil, false
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return nil, false
	}
	return m, true
}

// elements is raw's elements when it is a JSON array; false for anything else.
func elements(raw json.RawMessage) ([]json.RawMessage, bool) {
	raw = trimSpace(raw)
	if len(raw) == 0 || raw[0] != '[' {
		return nil, false
	}
	var l []json.RawMessage
	if json.Unmarshal(raw, &l) != nil {
		return nil, false
	}
	return l, true
}

// trimSpace is raw without the JSON whitespace before its value: a body or an
// event's data may start with some. A value members or elements return has none.
func trimSpace(raw []byte) []byte { return bytes.TrimLeft(raw, " \t\r\n") }

// stringOf is raw's value when it is a JSON string; false for anything else.
func stringOf(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 || raw[0] != '"' {
		return "", false
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	return s, true
}

// isAbsent reports whether a member is absent or null, which counts as absent.
func isAbsent(raw json.RawMessage) bool {
	return len(raw) == 0 || bytes.Equal(raw, []byte("null"))
}

// integerOf is raw's value when it is an integer, written as one, within int64 — as
// the gateway reads its own integer fields; false for anything else.
func integerOf(raw json.RawMessage) (int64, bool) {
	if isAbsent(raw) {
		return 0, false
	}
	var n int64
	if json.Unmarshal(raw, &n) != nil {
		return 0, false
	}
	return n, true
}

// numberOf is raw's value when it is a number that is a finite float64; false for
// anything else — a number beyond float64's range included.
func numberOf(raw json.RawMessage) (float64, bool) {
	if isAbsent(raw) {
		return 0, false
	}
	var f float64
	if json.Unmarshal(raw, &f) != nil {
		return 0, false
	}
	return f, true
}

// stringsOf is raw's strings when it is a string — a list of one — or a list of
// strings; false for anything else.
func stringsOf(raw json.RawMessage) ([]string, bool) {
	if s, ok := stringOf(raw); ok {
		return []string{s}, true
	}
	l, ok := elements(raw)
	if !ok {
		return nil, false
	}
	out := make([]string, 0, len(l))
	for _, e := range l {
		s, ok := stringOf(e)
		if !ok {
			return nil, false
		}
		out = append(out, s)
	}
	return out, true
}

// parameters are a request's parameter attributes (docs/specs/GATEWAY.md, Request
// capture → Parameters): each added only when its value has the attribute's type,
// anything else left out for the backend to judge.
type parameters []otlptrace.Attribute

// addInteger adds key from the first of values that is an integer: an API with two
// names for one parameter lists them in the order they are read.
func (p *parameters) addInteger(key string, values ...json.RawMessage) {
	for _, raw := range values {
		if n, ok := integerOf(raw); ok {
			*p = append(*p, otlptrace.Int(key, n))
			return
		}
	}
}

// addNumber adds key when raw is a finite number.
func (p *parameters) addNumber(key string, raw json.RawMessage) {
	if f, ok := numberOf(raw); ok {
		*p = append(*p, otlptrace.Float64(key, f))
	}
}

// addStrings adds key when raw is a string or a list of strings.
func (p *parameters) addStrings(key string, raw json.RawMessage) {
	if l, ok := stringsOf(raw); ok {
		*p = append(*p, otlptrace.Strings(key, l))
	}
}

// streamEvents yields the data of each event of an event stream as the client
// received it, in order, to its end — or to where it was cut short: an event the
// stream ends inside was never whole, and is not yielded.
func streamEvents(stream []byte) iter.Seq[[]byte] {
	return func(yield func([]byte) bool) {
		// No block of the stream can be longer than the stream.
		r := sse.NewReader(bytes.NewReader(stream), len(stream)+1)
		for {
			b, err := r.Next()
			if err != nil {
				return
			}
			if b.HasData && !yield(b.Data) {
				return
			}
		}
	}
}
