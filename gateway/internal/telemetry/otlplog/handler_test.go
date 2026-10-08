package otlplog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"os"
	"reflect"
	"testing"
	"time"

	"kaiak/internal/telemetry/otlp"
)

// idleExporter is an exporter that never sends on its own (no tick, no full batch
// at test sizes), so a test can read its queue.
func idleExporter(t *testing.T, vars map[string]string) *Exporter {
	t.Helper()
	if vars == nil {
		vars = map[string]string{}
	}
	if vars["OTEL_EXPORTER_OTLP_LOGS_ENDPOINT"] == "" {
		vars["OTEL_EXPORTER_OTLP_LOGS_ENDPOINT"] = "http://127.0.0.1:1/v1/logs"
	}
	s := mustSettings(t, vars)
	e := newExporter(s, otlp.Service{Version: "1.2.3", InstanceID: "gw-1"},
		slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil)), options{tick: make(chan time.Time)})
	t.Cleanup(func() { e.Shutdown(context.Background()) })
	return e
}

func queued(e *Exporter) []record {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]record(nil), e.queue...)
}

// noTime drops the time from a handler's output so two runs compare equal.
func noTime(groups []string, a slog.Attr) slog.Attr {
	if len(groups) == 0 && a.Key == slog.TimeKey {
		return slog.Attr{}
	}
	return a
}

func TestHandlerPassesEveryRecordToNext(t *testing.T) {
	logAll := func(l *slog.Logger) {
		l.Info("plain", "a", 1)
		l.With("kaiak.key.id", "k1").WithGroup("g").With("x", true).Warn("grouped", "y", 2.5)
		l.Debug("below the threshold")
		l.Error("failed", "exception.message", errors.New("boom"))
	}
	var want, got bytes.Buffer
	logAll(slog.New(slog.NewJSONHandler(&want, &slog.HandlerOptions{ReplaceAttr: noTime})))
	e := idleExporter(t, nil)
	logAll(slog.New(e.Handler(slog.NewJSONHandler(&got, &slog.HandlerOptions{ReplaceAttr: noTime}))))
	if got.String() != want.String() {
		t.Fatalf("next handler wrote\n%s\nwant\n%s", got.String(), want.String())
	}
	var messages []string
	for _, r := range queued(e) {
		messages = append(messages, r.message)
	}
	if want := []string{"plain", "grouped", "failed"}; !reflect.DeepEqual(messages, want) {
		t.Fatalf("queued %q, want %q: the next handler's level threshold applies", messages, want)
	}
}

type jsonOnly struct{ A int }

type named string

type errWithJSON struct{}

func (errWithJSON) Error() string                { return "plain text" }
func (errWithJSON) MarshalJSON() ([]byte, error) { return []byte(`{"code":7}`), nil }

type lazy struct{ v string }

func (l lazy) LogValue() slog.Value { return slog.StringValue("resolved " + l.v) }

func TestAttributeKinds(t *testing.T) {
	at := time.Date(2026, 10, 5, 12, 0, 0, 500_000_000, time.UTC)
	for _, tc := range []struct {
		name string
		attr slog.Attr
		want value
	}{
		{"string", slog.String("k", "v"), value{kind: kindString, str: "v"}},
		{"empty string", slog.String("k", ""), value{kind: kindString}},
		{"int", slog.Int("k", -42), value{kind: kindInt, integer: -42}},
		{"int64", slog.Int64("k", math.MaxInt64), value{kind: kindInt, integer: math.MaxInt64}},
		{"uint64 that fits", slog.Uint64("k", 7), value{kind: kindInt, integer: 7}},
		{"uint64 above int64", slog.Uint64("k", math.MaxUint64), value{kind: kindString, str: "18446744073709551615"}},
		{"float", slog.Float64("k", 0.012345), value{kind: kindDouble, double: 0.012345}},
		{"bool", slog.Bool("k", true), value{kind: kindBool, boolean: true}},
		{"duration: nanoseconds, as the JSON handler", slog.Duration("k", 1500*time.Millisecond), value{kind: kindInt, integer: 1_500_000_000}},
		{"time: RFC 3339", slog.Time("k", at), value{kind: kindString, str: "2026-10-05T12:00:00.5Z"}},
		{"string array", slog.Any("k", []string{"a", "b"}), value{kind: kindStrings, strs: []string{"a", "b"}}},
		{"error: its message", slog.Any("k", errors.New("dial tcp: refused")), value{kind: kindString, str: "dial tcp: refused"}},
		{"error that marshals: its JSON", slog.Any("k", errWithJSON{}), value{kind: kindString, str: `{"code":7}`}},
		{"struct: its JSON", slog.Any("k", jsonOnly{A: 1}), value{kind: kindString, str: `{"A":1}`}},
		{"named string: its text", slog.Any("k", named("x")), value{kind: kindString, str: "x"}},
		{"int array: its JSON", slog.Any("k", []int{1, 2}), value{kind: kindString, str: "[1,2]"}},
		{"nil: null", slog.Any("k", nil), value{kind: kindString, str: "null"}},
		{"unmarshalable: the JSON handler's error text", slog.Any("k", func() {}), value{kind: kindString, str: "!ERROR:json: unsupported type: func()"}},
		{"log valuer: resolved", slog.Any("k", lazy{"v"}), value{kind: kindString, str: "resolved v"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := idleExporter(t, nil)
			slog.New(e.Handler(discard())).LogAttrs(context.Background(), slog.LevelInfo, "m", tc.attr)
			got := queued(e)[0].attrs
			if want := []attr{{key: "k", value: tc.want}}; !reflect.DeepEqual(got, want) {
				t.Fatalf("attrs %+v, want %+v", got, want)
			}
		})
	}
}

func TestGroupsAreFlattened(t *testing.T) {
	e := idleExporter(t, nil)
	l := slog.New(e.Handler(discard())).
		With("top", 1).
		WithGroup("g").
		With("a", 2).
		WithGroup("").
		WithGroup("h")
	l.Info("m",
		"b", 3,
		slog.Group("nested", slog.Group("deeper", "c", 4)),
		slog.Group("empty"),
		slog.Group("", "inlined", 5),
		slog.Attr{},
	)
	var keys []string
	for _, a := range queued(e)[0].attrs {
		keys = append(keys, a.key)
	}
	want := []string{"top", "g.a", "g.h.b", "g.h.nested.deeper.c", "g.h.inlined"}
	if !reflect.DeepEqual(keys, want) {
		t.Fatalf("keys %q, want %q", keys, want)
	}
}

func TestWithAttrsAndWithGroupReachNext(t *testing.T) {
	var got bytes.Buffer
	e := idleExporter(t, nil)
	slog.New(e.Handler(slog.NewJSONHandler(&got, &slog.HandlerOptions{ReplaceAttr: noTime}))).
		With("a", 1).WithGroup("g").With("b", 2).Info("m", "c", 3)
	if want := `{"level":"INFO","msg":"m","a":1,"g":{"b":2,"c":3}}` + "\n"; got.String() != want {
		t.Fatalf("next wrote %s, want %s", got.String(), want)
	}
}

func TestRecordIsCopiedAtHandle(t *testing.T) {
	e := idleExporter(t, nil)
	l := slog.New(e.Handler(discard()))
	codes := []string{"a", "b"}
	withCodes := l.With("kaiak.tried", codes)
	withCodes.Info("m", "kaiak.config.issue_codes", codes)
	codes[0] = "changed"
	withCodes.Info("later")
	recs := queued(e)
	for i, r := range recs {
		for _, a := range r.attrs {
			if a.value.strs[0] != "a" {
				t.Fatalf("record %d: %s = %q: the caller's slice was shared", i, a.key, a.value.strs)
			}
		}
	}
}

func TestSeverity(t *testing.T) {
	for level, want := range map[slog.Level]int{
		slog.LevelDebug:     5,
		slog.LevelInfo:      9,
		slog.LevelWarn:      13,
		slog.LevelError:     17,
		slog.LevelWarn + 2:  15,
		slog.LevelDebug - 9: 1,
		slog.LevelError + 9: 24,
	} {
		if got := severityNumber(level); got != want {
			t.Errorf("severityNumber(%s) = %d, want %d", level, got, want)
		}
	}
}

// TestEncodedBatchMatchesFixture checks the encoding against testdata/batch.json,
// written by hand from the OTLP JSON rules: lowerCamelCase keys, int64 and fixed64
// as decimal strings, the severity as an integer, AnyValue's one-of keys.
func TestEncodedBatchMatchesFixture(t *testing.T) {
	e := idleExporter(t, map[string]string{
		"OTEL_SERVICE_NAME":        "kaiak-eu",
		"OTEL_RESOURCE_ATTRIBUTES": "deployment.environment.name=prod,service.version=0.0.0,k8s.pod.name=gw%2C1",
	})
	h := e.Handler(discard())
	at := func(sec, nsec int) time.Time { return time.Date(2026, 10, 5, 12, 0, sec, nsec, time.UTC) }

	r1 := slog.NewRecord(at(0, 123456789), slog.LevelInfo, "request", 0)
	r1.AddAttrs(
		slog.String("kaiak.request.id", "req-1"),
		slog.Int("http.response.status_code", 200),
		slog.Float64("kaiak.request.duration", 0.012345),
		slog.Bool("gen_ai.request.stream", true),
		slog.Any("kaiak.config.issue_codes", []string{"a", "b"}),
		slog.Any("exception.message", errors.New("dial tcp: connection refused")),
		slog.Group("kaiak.limit", "used", 5),
	)
	r2 := slog.NewRecord(at(1, 0), slog.LevelWarn, "circuit opened", 0)
	r2.AddAttrs(
		slog.Uint64("kaiak.big", math.MaxUint64),
		slog.Time("kaiak.at", at(0, 500_000_000)),
		slog.Duration("kaiak.wait", 1500*time.Millisecond),
		slog.Float64("kaiak.ratio", math.NaN()),
	)
	r3 := slog.NewRecord(at(1, 1), slog.LevelError, "kaiak stopped with an error", 0)
	for _, r := range []slog.Record{r1, r2, r3} {
		if err := h.Handle(context.Background(), r); err != nil {
			t.Fatal(err)
		}
	}

	got, err := encodeBatch(e.client.Resource(), queued(e))
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile("testdata/batch.json")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decodeJSON(t, got), decodeJSON(t, want)) {
		t.Fatalf("encoded batch\n%s\nwant (testdata/batch.json)\n%s", got, want)
	}
}

// decodeJSON decodes keeping numbers as written, so "200" and 200 differ.
func decodeJSON(t *testing.T, b []byte) any {
	t.Helper()
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		t.Fatalf("decode %s: %v", b, err)
	}
	return v
}

// discard is an enabled next handler that writes nothing (slog.DiscardHandler is
// disabled, so a logger would never reach the exporting handler).
func discard() slog.Handler { return slog.NewJSONHandler(io.Discard, nil) }

// nilDerefErr's Error dereferences its receiver: a typed nil panics.
type nilDerefErr struct{ msg string }

func (e *nilDerefErr) Error() string { return e.msg }

type panicsInError struct{}

func (panicsInError) Error() string { panic("error text unavailable") }

type panicsInJSON struct{}

func (panicsInJSON) MarshalJSON() ([]byte, error) { panic(errors.New("cannot marshal")) }

// A value whose rendering panics is written as slog's handlers write it — the
// JSON handler beside it is the reference — and the caller goes on.
func TestPanickingValuesAreRenderedAsSlogDoes(t *testing.T) {
	var typedNil *nilDerefErr
	for _, tc := range []struct {
		name  string
		value any
		want  string
	}{
		{"typed-nil error", error(typedNil), "<nil>"},
		{"panicking Error", panicsInError{}, "!PANIC: error text unavailable"},
		{"panicking MarshalJSON", panicsInJSON{}, "!PANIC: cannot marshal"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := idleExporter(t, nil)
			var stderr bytes.Buffer
			slog.New(e.Handler(slog.NewJSONHandler(&stderr, nil))).Info("m", "k", tc.value)
			var line map[string]any
			if err := json.Unmarshal(stderr.Bytes(), &line); err != nil {
				t.Fatalf("stderr %q: %v", stderr.String(), err)
			}
			if line["k"] != tc.want {
				t.Fatalf("the JSON handler wrote %q, want %q", line["k"], tc.want)
			}
			got := queued(e)[0].attrs
			if want := []attr{{key: "k", value: value{kind: kindString, str: tc.want}}}; !reflect.DeepEqual(got, want) {
				t.Fatalf("attrs %+v, want %+v", got, want)
			}
		})
	}
}
