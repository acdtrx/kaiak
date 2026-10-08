// Package schemacheck checks decoded JSON documents against the structure the
// protocol's JSON Schemas (protocol/schema/) express, written out in code because the
// standard library has no JSON Schema validator. A Checker walks the generic JSON tree,
// so every issue carries its exact path, and reports every violation (as
// kaiak-control's validator does). The packages owning each document build their
// walkers from these parts and read the document through one pipeline: Validate
// (syntax, repeated members, the walker), their own rules, then DecodeTyped. The
// shared fixtures in protocol/fixtures keep each walker and its schema in agreement.
package schemacheck

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// JSWhitespace is the set JavaScript's \s matches, for character classes. The schemas'
// patterns are ECMAScript regexes; Go's \s is ASCII-only and omits \v, so the set is
// spelled out.
const JSWhitespace = `\t\n\v\f\r \x{00a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}`

// MaxSafeInteger is the largest integer a JavaScript number holds exactly (2^53 − 1):
// the bound the message schemas put on integer fields, so kaiak-control reads them
// without rounding.
const MaxSafeInteger = 1<<53 - 1

// Codes for the rejections of Validate's stages. A schema violation has no per-rule
// code: CodeSchema plus the walker's own message (docs/specs/CONTROL-PROTOCOL.md,
// Config and Messages). The owners of each document add codes for their own rules.
const (
	// CodeSyntax: the document is not a single JSON value.
	CodeSyntax = "syntax"
	// CodeDuplicateMember: an object in the document names the same member twice
	// (JSON leaves the meaning open; decoders disagree on which one wins).
	CodeDuplicateMember = "duplicate-member"
	// CodeSchema: the document breaks its schema.
	CodeSchema = "schema"
)

// Issue is one reason a document was rejected.
type Issue struct {
	Code string
	// Path is a JSON Pointer (RFC 6901) into the document; "" is the root.
	Path    string
	Message string
}

func (i Issue) String() string {
	path := i.Path
	if path == "" {
		path = "/"
	}
	return fmt.Sprintf("%s: %s [%s]", path, i.Message, i.Code)
}

// ValidationError rejects a document and lists every issue of the stage that failed
// (syntax, then duplicate members, then schema, then the owner's rules).
type ValidationError struct {
	// Subject names the document, e.g. "config" or "usage ack".
	Subject string
	Issues  []Issue
}

func (e *ValidationError) Error() string {
	parts := make([]string, len(e.Issues))
	for i, issue := range e.Issues {
		parts[i] = issue.String()
	}
	return e.Subject + " rejected: " + strings.Join(parts, "; ")
}

// Codes returns the distinct issue codes, sorted.
func (e *ValidationError) Codes() []string {
	codes := make([]string, 0, len(e.Issues))
	for _, issue := range e.Issues {
		codes = append(codes, issue.Code)
	}
	slices.Sort(codes)
	return slices.Compact(codes)
}

// Validate reads data as one document of subject and returns its generic tree
// (numbers as json.Number): data must be exactly one JSON value, no object in it may
// name a member twice, and walk — the document's schema walker — must find no issue.
// A rejection is a *ValidationError listing the issues of the first stage that failed.
//
// A repeated member is refused at any depth before the walker runs: JSON leaves its
// meaning open, and Go's decoders disagree on it — the generic tree keeps the last
// member, a typed decode merges both into a map — so a walker checking the tree would
// not see what the typed decode builds.
func Validate(subject string, data []byte, walk func(tree any) []Issue) (any, error) {
	reject := func(issues ...Issue) (any, error) {
		return nil, &ValidationError{Subject: subject, Issues: issues}
	}
	tree, err := decodeTree(data)
	if err != nil {
		return reject(Issue{Code: CodeSyntax, Message: err.Error()})
	}
	if path, found := FirstDuplicateMember(data); found {
		return reject(Issue{Code: CodeDuplicateMember, Path: path, Message: "appears more than once in its object"})
	}
	if issues := walk(tree); len(issues) > 0 {
		return reject(issues...)
	}
	return tree, nil
}

// decodeTree parses data as exactly one JSON value, keeping numbers as json.Number.
func decodeTree(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var tree any
	if err := dec.Decode(&tree); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("unexpected data after the JSON document")
	}
	return tree, nil
}

// DecodeTyped decodes a tree that Validate accepted into T, strictly: unknown fields
// are errors, so a schema field the Go types lack fails loudly instead of being
// dropped. Integer fields may be written with a fraction or exponent (4096.0, 1e3) —
// JSON Schema counts those as integers — so they are rewritten as plain integers
// first, in place: the tree is not to be read afterwards. A failure means the walker
// accepted what the types cannot hold — the two disagree — and is a *ValidationError
// with CodeSchema.
func DecodeTyped[T any](subject string, tree any) (T, error) {
	var v T
	data, err := json.Marshal(plainIntegers(tree))
	if err == nil {
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.DisallowUnknownFields()
		err = dec.Decode(&v)
	}
	if err != nil {
		return v, &ValidationError{Subject: subject, Issues: []Issue{{Code: CodeSchema, Message: err.Error()}}}
	}
	return v, nil
}

func plainIntegers(v any) any {
	switch v := v.(type) {
	case map[string]any:
		for k, item := range v {
			v[k] = plainIntegers(item)
		}
	case []any:
		for i, item := range v {
			v[i] = plainIntegers(item)
		}
	case json.Number:
		if f := NumberValue(v); IsInteger(f) && strings.ContainsAny(string(v), ".eE") {
			return json.Number(strconv.FormatFloat(f, 'f', -1, 64))
		}
	}
	return v
}

// FirstDuplicateMember scans data, one JSON value, for an object naming a member twice
// at any depth, and returns the JSON Pointer of the first repeat. Names compare after
// decoding ("a" and "\u0061" are one name), as every decoder reads them. It reads
// tokens and holds only the names of the objects open at the current position, so its
// memory is bounded by the input's size; the caller bounds that (and has checked the
// syntax, which also bounds the nesting). Data that is not valid JSON is scanned up to
// its first error.
func FirstDuplicateMember(data []byte) (string, bool) {
	type container struct {
		path   string
		object bool
		names  map[string]struct{}
		// wantName: inside an object, the next token is a member name.
		wantName bool
		name     string
		index    int
	}
	var open []*container
	// valueDone moves the innermost container past the value just read.
	valueDone := func() {
		if len(open) == 0 {
			return
		}
		c := open[len(open)-1]
		if c.object {
			c.wantName = true
		} else {
			c.index++
		}
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	for {
		tok, err := dec.Token()
		if err != nil {
			return "", false // the end of the value (io.EOF), or invalid JSON
		}
		var inner *container
		if len(open) > 0 {
			inner = open[len(open)-1]
		}
		if inner != nil && inner.object && inner.wantName {
			if tok == json.Delim('}') {
				open = open[:len(open)-1]
				valueDone()
				continue
			}
			name, _ := tok.(string) // inside an object a name comes next
			if _, repeated := inner.names[name]; repeated {
				return Pointer(inner.path, name), true
			}
			inner.names[name] = struct{}{}
			inner.name, inner.wantName = name, false
			continue
		}
		switch tok {
		case json.Delim('{'), json.Delim('['):
			path := ""
			switch {
			case inner == nil:
			case inner.object:
				path = Pointer(inner.path, inner.name)
			default:
				path = Pointer(inner.path, inner.index)
			}
			c := &container{path: path, object: tok == json.Delim('{'), wantName: true}
			if c.object {
				c.names = make(map[string]struct{})
			}
			open = append(open, c)
		case json.Delim(']'):
			open = open[:len(open)-1]
			valueDone()
		default:
			valueDone()
		}
	}
}

// Field describes one member of an object: whether it is required, and the check its
// value must pass.
type Field struct {
	Required bool
	Check    func(v any, path string)
}

// Checker collects issues as a walker runs its checks. The zero value is ready.
type Checker struct {
	issues []Issue
}

// Issues returns every issue found so far.
func (c *Checker) Issues() []Issue { return c.issues }

// Fail records a schema issue at path.
func (c *Checker) Fail(path, message string) {
	c.issues = append(c.issues, Issue{Code: CodeSchema, Path: path, Message: message})
}

// Object checks v is an object holding the required fields and no unknown ones, runs
// each present field's check, and returns the object (nil if v is not one).
func (c *Checker) Object(v any, path string, fields map[string]Field) map[string]any {
	m, ok := v.(map[string]any)
	if !ok {
		c.Fail(path, "must be an object")
		return nil
	}
	for _, name := range slices.Sorted(maps.Keys(fields)) {
		f := fields[name]
		value, present := m[name]
		if !present {
			if f.Required {
				c.Fail(path, "missing required field "+strconv.Quote(name))
			}
			continue
		}
		f.Check(value, Pointer(path, name))
	}
	for _, name := range slices.Sorted(maps.Keys(m)) {
		if _, known := fields[name]; !known {
			c.Fail(Pointer(path, name), "unknown field")
		}
	}
	return m
}

// CollectionOf checks an object keyed by names that pass validKey (keyWhat describes
// them), each entry checked by each.
func (c *Checker) CollectionOf(validKey func(string) bool, keyWhat string, each func(v any, path string)) func(v any, path string) {
	return func(v any, path string) {
		m, ok := v.(map[string]any)
		if !ok {
			c.Fail(path, "must be an object keyed by ID")
			return
		}
		for _, name := range slices.Sorted(maps.Keys(m)) {
			p := Pointer(path, name)
			if !validKey(name) {
				c.Fail(p, "key must be "+keyWhat)
			}
			each(m[name], p)
		}
	}
}

// ArrayOf checks an array of at least minItems items (maxItems 0 = no maximum), each
// checked by each; unique rejects items equal to an earlier one.
func (c *Checker) ArrayOf(minItems, maxItems int, unique bool, each func(v any, path string)) func(v any, path string) {
	return func(v any, path string) {
		items, ok := v.([]any)
		if !ok {
			c.Fail(path, "must be an array")
			return
		}
		if len(items) < minItems {
			c.Fail(path, "must have at least "+strconv.Itoa(minItems)+" item(s)")
		}
		if maxItems > 0 && len(items) > maxItems {
			c.Fail(path, "must have at most "+strconv.Itoa(maxItems)+" items")
		}
		seen := make(map[string]int, len(items))
		for i, item := range items {
			each(item, Pointer(path, i))
			if !unique {
				continue
			}
			identity := canonicalJSON(item)
			if first, dup := seen[identity]; dup {
				c.Fail(Pointer(path, i), "duplicates item "+strconv.Itoa(first))
				continue
			}
			seen[identity] = i
		}
	}
}

// StringMatching checks a string matching re; what describes it.
func (c *Checker) StringMatching(re *regexp.Regexp, what string) func(v any, path string) {
	return func(v any, path string) {
		s, ok := v.(string)
		if !ok {
			c.Fail(path, "must be a string")
			return
		}
		if !re.MatchString(s) {
			c.Fail(path, "must be "+what)
		}
	}
}

// StringWhere checks a string that valid accepts; what describes it.
func (c *Checker) StringWhere(valid func(string) bool, what string) func(v any, path string) {
	return func(v any, path string) {
		s, ok := v.(string)
		if !ok {
			c.Fail(path, "must be a string")
			return
		}
		if !valid(s) {
			c.Fail(path, "must be "+what)
		}
	}
}

// Enum checks a string that is one of values.
func (c *Checker) Enum(values []string) func(v any, path string) {
	return func(v any, path string) {
		s, ok := v.(string)
		if !ok || !slices.Contains(values, s) {
			c.Fail(path, "must be one of "+quoteAll(values))
		}
	}
}

// Const checks the integer want.
func (c *Checker) Const(want float64) func(v any, path string) {
	return func(v any, path string) {
		if n, ok := v.(json.Number); !ok || NumberValue(n) != want {
			c.Fail(path, "must be "+strconv.FormatFloat(want, 'f', -1, 64))
		}
	}
}

// Boolean checks a boolean.
func (c *Checker) Boolean(v any, path string) {
	if _, ok := v.(bool); !ok {
		c.Fail(path, "must be a boolean")
	}
}

// NumberAtLeast checks a number of at least minimum.
func (c *Checker) NumberAtLeast(minimum float64) func(v any, path string) {
	return func(v any, path string) {
		n, ok := v.(json.Number)
		if !ok {
			c.Fail(path, "must be a number")
			return
		}
		if value := NumberValue(n); math.IsNaN(value) || value < minimum {
			c.Fail(path, "must be at least "+strconv.FormatFloat(minimum, 'f', -1, 64))
		}
	}
}

// NumberBetween checks a number in [minimum, maximum].
func (c *Checker) NumberBetween(minimum, maximum float64) func(v any, path string) {
	return func(v any, path string) {
		n, ok := v.(json.Number)
		if !ok {
			c.Fail(path, "must be a number")
			return
		}
		if value := NumberValue(n); math.IsNaN(value) || value < minimum || value > maximum {
			c.Fail(path, "must be between "+strconv.FormatFloat(minimum, 'f', -1, 64)+
				" and "+strconv.FormatFloat(maximum, 'f', -1, 64))
		}
	}
}

// IntegerAtLeast checks an integer of at least minimum and at most MaxSafeInteger: the
// schemas bound every integer so that kaiak-control reads it exactly.
func (c *Checker) IntegerAtLeast(minimum float64) func(v any, path string) {
	return c.IntegerBetween(minimum, MaxSafeInteger)
}

// IntegerBetween checks an integer in [minimum, maximum].
func (c *Checker) IntegerBetween(minimum, maximum float64) func(v any, path string) {
	return func(v any, path string) {
		n, ok := v.(json.Number)
		if !ok {
			c.Fail(path, "must be an integer")
			return
		}
		value := NumberValue(n)
		switch {
		case !IsInteger(value):
			c.Fail(path, "must be an integer")
		case value < minimum || value > maximum:
			c.Fail(path, "must be between "+strconv.FormatFloat(minimum, 'f', -1, 64)+
				" and "+strconv.FormatFloat(maximum, 'f', -1, 64))
		}
	}
}

// Nullable accepts null, or a value passing check.
func Nullable(check func(v any, path string)) func(v any, path string) {
	return func(v any, path string) {
		if v == nil {
			return
		}
		check(v, path)
	}
}

// NumberValue reads a JSON number the way JavaScript does (as a float64). The decoder
// has already checked the syntax; a value out of float64 range reads as NaN, which
// every check rejects.
func NumberValue(n json.Number) float64 {
	f, err := strconv.ParseFloat(string(n), 64)
	if err != nil {
		return math.NaN()
	}
	return f
}

// IsInteger reports whether f is a whole number (4096.0 included, as JSON Schema
// counts it).
func IsInteger(f float64) bool {
	return !math.IsNaN(f) && !math.IsInf(f, 0) && math.Trunc(f) == f
}

// Pointer builds a JSON Pointer (RFC 6901) from a base pointer and further segments.
func Pointer(base string, segments ...any) string {
	var b strings.Builder
	b.WriteString(base)
	for _, segment := range segments {
		b.WriteByte('/')
		s := fmt.Sprint(segment)
		s = strings.ReplaceAll(s, "~", "~0")
		s = strings.ReplaceAll(s, "/", "~1")
		b.WriteString(s)
	}
	return b.String()
}

// IsRealDate: a schema pattern has fixed the shape (YYYY-MM-DD); this checks the day
// exists.
func IsRealDate(value string) bool {
	year, errYear := strconv.Atoi(value[0:4])
	month, errMonth := strconv.Atoi(value[5:7])
	day, errDay := strconv.Atoi(value[8:10])
	if errYear != nil || errMonth != nil || errDay != nil {
		return false
	}
	return isRealCalendarDay(year, month, day)
}

// IsRealTimestamp: a schema pattern has fixed the shape (YYYY-MM-DDTHH:MM:SS[.fraction]Z);
// this checks every field is in range. Leap seconds (:60) are rejected.
func IsRealTimestamp(value string) bool {
	if !IsRealDate(value[0:10]) {
		return false
	}
	hour, errHour := strconv.Atoi(value[11:13])
	minute, errMinute := strconv.Atoi(value[14:16])
	second, errSecond := strconv.Atoi(value[17:19])
	if errHour != nil || errMinute != nil || errSecond != nil {
		return false
	}
	return hour <= 23 && minute <= 59 && second <= 59
}

// isRealCalendarDay uses the proleptic Gregorian calendar, as RFC 3339 does.
func isRealCalendarDay(year, month, day int) bool {
	daysInMonth := [12]int{31, 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31}
	if month < 1 || month > 12 || day < 1 {
		return false
	}
	leap := (year%4 == 0 && year%100 != 0) || year%400 == 0
	if month == 2 && leap {
		return day <= 29
	}
	return day <= daysInMonth[month-1]
}

// canonicalJSON gives equal JSON values equal strings (object keys sorted), for
// uniqueItems. Values checked for uniqueness are strings or objects of strings, where
// text equality is JSON equality.
func canonicalJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

func quoteAll(values []string) string {
	b := []byte{}
	for i, v := range values {
		if i > 0 {
			b = append(b, ", "...)
		}
		b = strconv.AppendQuote(b, v)
	}
	return string(b)
}
