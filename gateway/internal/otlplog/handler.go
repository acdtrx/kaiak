package otlplog

import (
	"context"
	"encoding/json"
	"log/slog"
	"math"
	"slices"
	"strconv"
	"time"
)

// handler is the exporting slog.Handler: every record goes to next (stderr) as is,
// and a copy goes to the exporter's queue.
type handler struct {
	next slog.Handler
	exp  *Exporter
	// attrs are the WithAttrs attributes, already flattened and copied.
	attrs []attr
	// prefix is the open groups, each followed by "." — the flattened keys' start.
	prefix string
}

func (h *handler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

// Handle queues a copy of r, taken now, then hands r to the next handler. Queueing
// never waits: a full queue drops the copy.
func (h *handler) Handle(ctx context.Context, r slog.Record) error {
	h.exp.enqueue(h.capture(r))
	return h.next.Handle(ctx, r)
}

func (h *handler) WithAttrs(as []slog.Attr) slog.Handler {
	if len(as) == 0 {
		return h
	}
	c := *h
	c.next = h.next.WithAttrs(as)
	c.attrs = slices.Clip(h.attrs)
	for _, a := range as {
		c.attrs = appendAttr(c.attrs, h.prefix, a)
	}
	return &c
}

func (h *handler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	c := *h
	c.next = h.next.WithGroup(name)
	c.prefix = h.prefix + name + "."
	return &c
}

// record is what the sender needs of a log record, copied at Handle time so that
// nothing is shared with the caller.
type record struct {
	time    time.Time
	level   slog.Level
	message string
	attrs   []attr
}

type attr struct {
	key   string
	value value
}

type valueKind uint8

const (
	kindString valueKind = iota
	kindInt
	kindDouble
	kindBool
	kindStrings
)

// value is an attribute value in the types OTLP carries.
type value struct {
	kind    valueKind
	str     string
	integer int64
	double  float64
	boolean bool
	strs    []string
}

func (h *handler) capture(r slog.Record) record {
	rec := record{time: r.Time, level: r.Level, message: r.Message}
	rec.attrs = make([]attr, 0, len(h.attrs)+r.NumAttrs())
	rec.attrs = append(rec.attrs, h.attrs...)
	r.Attrs(func(a slog.Attr) bool {
		rec.attrs = appendAttr(rec.attrs, h.prefix, a)
		return true
	})
	return rec
}

// appendAttr appends a, resolved, to dst under prefix; a group is flattened with
// "." between the names, as slog's own handlers treat groups: an empty group is
// left out, a group without a key is inlined, and an empty attribute is ignored.
func appendAttr(dst []attr, prefix string, a slog.Attr) []attr {
	v := a.Value.Resolve()
	if v.Kind() == slog.KindGroup {
		inner := prefix
		if a.Key != "" {
			inner = prefix + a.Key + "."
		}
		for _, ga := range v.Group() {
			dst = appendAttr(dst, inner, ga)
		}
		return dst
	}
	if a.Key == "" && v.Equal(slog.Value{}) {
		return dst
	}
	return append(dst, attr{key: prefix + a.Key, value: convert(v)})
}

// convert maps a resolved slog value to an OTLP-typed one (docs/specs/GATEWAY.md,
// Observability → OTLP log export: record mapping). Where OTLP has no type for a
// value, it carries the text the JSON handler writes for it.
func convert(v slog.Value) value {
	switch v.Kind() {
	case slog.KindString:
		return value{kind: kindString, str: v.String()}
	case slog.KindInt64:
		return value{kind: kindInt, integer: v.Int64()}
	case slog.KindUint64:
		if u := v.Uint64(); u <= math.MaxInt64 {
			return value{kind: kindInt, integer: int64(u)}
		}
		return value{kind: kindString, str: strconv.FormatUint(v.Uint64(), 10)}
	case slog.KindFloat64:
		return value{kind: kindDouble, double: v.Float64()}
	case slog.KindBool:
		return value{kind: kindBool, boolean: v.Bool()}
	case slog.KindDuration:
		// The JSON handler writes a duration as integer nanoseconds.
		return value{kind: kindInt, integer: int64(v.Duration())}
	case slog.KindTime:
		return value{kind: kindString, str: v.Time().Format(time.RFC3339Nano)}
	}
	a := v.Any()
	if strs, ok := a.([]string); ok {
		return value{kind: kindStrings, strs: slices.Clone(strs)}
	}
	if err, ok := a.(error); ok {
		if _, marshals := a.(json.Marshaler); !marshals {
			return value{kind: kindString, str: err.Error()}
		}
	}
	return value{kind: kindString, str: jsonText(a)}
}

// jsonText is the text the JSON handler writes for a: a JSON string's contents, or
// the JSON itself for anything else.
func jsonText(a any) string {
	b, err := json.Marshal(a)
	if err != nil {
		return "!ERROR:" + err.Error()
	}
	var s string
	if b[0] == '"' && json.Unmarshal(b, &s) == nil {
		return s
	}
	return string(b)
}
