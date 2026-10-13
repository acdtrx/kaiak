package otlp

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// problemsHarness is a Problems writing to a buffer, on a clock the test moves.
type problemsHarness struct {
	p       *Problems
	out     bytes.Buffer
	now     time.Time
	dropped atomic.Uint64
}

func newProblemsHarness() *problemsHarness {
	h := &problemsHarness{now: time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)}
	h.p = NewProblems(slog.New(slog.NewJSONHandler(&h.out, nil)), ProblemLine{
		Message: "test export failing", Failed: "test.failed", Dropped: "test.dropped",
	}, h.dropped.Load, func() time.Time { return h.now })
	return h
}

// lines decodes the lines written so far.
func (h *problemsHarness) lines(t *testing.T) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(h.out.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("line %q: %v", line, err)
		}
		if m["msg"] != "test export failing" || m["level"] != "WARN" {
			t.Fatalf("line %q: want a WARN `test export failing`", line)
		}
		delete(m, "time")
		delete(m, "msg")
		delete(m, "level")
		out = append(out, m)
	}
	return out
}

func TestProblemsReportAtMostOnceAMinute(t *testing.T) {
	h := newProblemsHarness()
	h.p.Report()
	if got := h.lines(t); len(got) != 0 {
		t.Fatalf("lines %v, want none: no problem yet", got)
	}

	h.p.Fail(3, "503", 503, errors.New("collector answered 503 Service Unavailable"))
	h.p.Report()
	h.now = h.now.Add(59 * time.Second)
	h.p.Fail(2, "timeout", 0, errors.New("export cut short"))
	h.dropped.Add(4)
	h.p.Report()
	want := []map[string]any{
		{"test.failed": float64(3), "test.dropped": float64(0), "http.response.status_code": float64(503),
			"exception.message": "collector answered 503 Service Unavailable"},
	}
	if got := h.lines(t); !reflect.DeepEqual(got, want) {
		t.Fatalf("lines %v, want %v: the first at once, then none within the minute", got, want)
	}

	h.now = h.now.Add(time.Second)
	h.p.Report()
	// The last failure had no status: none is written.
	want = append(want, map[string]any{"test.failed": float64(2), "test.dropped": float64(4), "exception.message": "export cut short"})
	if got := h.lines(t); !reflect.DeepEqual(got, want) {
		t.Fatalf("lines %v, want %v: a minute on, what happened since", got, want)
	}

	h.now = h.now.Add(time.Hour)
	h.p.Report()
	if got := h.lines(t); len(got) != 2 {
		t.Fatalf("lines %v, want no new line without new problems", got)
	}
	if got, want := h.p.Failed(), map[string]uint64{"503": 3, "timeout": 2}; !reflect.DeepEqual(got, want) {
		t.Fatalf("failed %v, want %v", got, want)
	}
}

// The line at exit is never held back: drops at exit are never silent.
func TestProblemsReportNowIsNeverHeldBack(t *testing.T) {
	h := newProblemsHarness()
	h.p.Fail(1, "400", 400, errors.New("collector answered 400 Bad Request"))
	h.p.Report()
	h.dropped.Add(5)
	h.p.ReportNow()
	h.p.ReportNow()
	got := h.lines(t)
	if len(got) != 2 || got[1]["test.failed"] != float64(0) || got[1]["test.dropped"] != float64(5) {
		t.Fatalf("lines %v, want two: the failure, then the drops within the same minute", got)
	}
	if _, ok := got[1]["http.response.status_code"]; ok {
		t.Fatalf("line %v carries a status: nothing failed since the previous line", got[1])
	}
}

func TestProblemsFailedIsACopy(t *testing.T) {
	h := newProblemsHarness()
	if got := h.p.Failed(); got != nil {
		t.Fatalf("failed %v, want nil before any failure", got)
	}
	h.p.Fail(2, "rejected", 200, errors.New("collector rejected 2 spans"))
	h.p.Failed()["rejected"] = 99
	if got := h.p.Failed(); got["rejected"] != 2 {
		t.Fatalf("failed %v, want rejected 2: Failed hands out a copy", got)
	}
}
