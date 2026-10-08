package otlp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"kaiak/internal/telemetry/fakeotlp"
)

// body is an export request the fake collector decodes; the client posts it as is.
const body = `{"resourceLogs":[]}`

// harness is a client posting to a fake collector, its retry waits ending at once,
// each recorded.
type harness struct {
	c   *Client
	col *fakeotlp.Collector

	mu    sync.Mutex
	waits []time.Duration
}

// newHarness is a logs client posting to col; vars add to the endpoint.
func newHarness(t *testing.T, col *fakeotlp.Collector, vars map[string]string) *harness {
	t.Helper()
	return newSignalHarness(t, Logs, col, vars)
}

func newSignalHarness(t *testing.T, sig Signal, col *fakeotlp.Collector, vars map[string]string) *harness {
	t.Helper()
	if vars == nil {
		vars = map[string]string{}
	}
	vars[sig.info().variables.endpoint] = col.URL + "/" + sig.info().path
	s, err := ReadSettings(sig, envOf(vars))
	if err != nil || s == nil {
		t.Fatalf("ReadSettings = %v, %v; want settings", s, err)
	}
	h := &harness{col: col, c: NewClient(s, Service{Version: "1.2.3", InstanceID: "gw-1"})}
	h.c.after = func(d time.Duration) <-chan time.Time {
		h.mu.Lock()
		h.waits = append(h.waits, d)
		h.mu.Unlock()
		ch := make(chan time.Time, 1)
		ch <- time.Time{}
		return ch
	}
	t.Cleanup(h.c.CloseIdleConnections)
	return h
}

func (h *harness) export() Outcome {
	return h.c.Export(context.Background(), []byte(body))
}

func (h *harness) recordedWaits() []time.Duration {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]time.Duration(nil), h.waits...)
}

// wantDelivered fails the test unless o delivered the body whole.
func wantDelivered(t *testing.T, o Outcome) {
	t.Helper()
	if o.Err != nil || o.Rejected != 0 || o.RejectErr != nil {
		t.Fatalf("outcome %+v, want delivered whole", o)
	}
}

// wantFailed fails the test unless o failed the body, with status as its status.
func wantFailed(t *testing.T, o Outcome, status int) {
	t.Helper()
	if o.Err == nil || o.Status != status {
		t.Fatalf("outcome %+v, want failed with status %d", o, status)
	}
}

func TestExportRequest(t *testing.T) {
	col := fakeotlp.New(t, nil)
	h := newHarness(t, col, map[string]string{
		"OTEL_EXPORTER_OTLP_HEADERS": "authorization=Bearer%20abc,x-scope-orgid=tenant-1",
	})
	o := h.export()
	wantDelivered(t, o)
	if o.Status != http.StatusOK {
		t.Fatalf("status %d, want 200", o.Status)
	}
	r := col.Next(t)
	for name, want := range map[string]string{
		"Content-Type":  "application/json",
		"User-Agent":    "kaiak/1.2.3",
		"Authorization": "Bearer abc",
		"X-Scope-Orgid": "tenant-1",
	} {
		if got := r.Header.Get(name); got != want {
			t.Errorf("header %s = %q, want %q", name, got, want)
		}
	}
}

func TestResource(t *testing.T) {
	s, err := ReadSettings(Logs, envOf(map[string]string{
		"OTEL_EXPORTER_OTLP_ENDPOINT": "http://c:4318",
		"OTEL_SERVICE_NAME":           "kaiak-eu",
		"OTEL_RESOURCE_ATTRIBUTES":    "deployment.environment.name=prod,service.version=0.0.0,service.instance.id=other,k8s.pod.name=gw%2C1",
	}))
	if err != nil {
		t.Fatal(err)
	}
	got, err := json.Marshal(NewClient(s, Service{Version: "1.2.3", InstanceID: "gw-1"}).Resource())
	if err != nil {
		t.Fatal(err)
	}
	want := `{"attributes":[` +
		`{"key":"service.name","value":{"stringValue":"kaiak-eu"}},` +
		`{"key":"service.version","value":{"stringValue":"1.2.3"}},` +
		`{"key":"service.instance.id","value":{"stringValue":"gw-1"}},` +
		`{"key":"deployment.environment.name","value":{"stringValue":"prod"}},` +
		`{"key":"k8s.pod.name","value":{"stringValue":"gw,1"}}]}`
	if string(got) != want {
		t.Fatalf("resource\n%s\nwant\n%s", got, want)
	}
}

func TestRetryOnEachRetryableStatus(t *testing.T) {
	for _, status := range []int{429, 502, 503, 504} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			col := fakeotlp.New(t, func(w http.ResponseWriter, _ *http.Request, n int) {
				if n < 2 {
					w.WriteHeader(status)
					return
				}
				io.WriteString(w, "{}")
			})
			h := newHarness(t, col, nil)
			wantDelivered(t, h.export())
			if n := col.Requests(); n != 3 {
				t.Fatalf("%d requests, want 3 (two retries)", n)
			}
			waits := h.recordedWaits()
			if len(waits) != 2 {
				t.Fatalf("waits %v, want 2", waits)
			}
			// Backoff with jitter over the upper half: 0.5 s, then 1 s.
			for i, base := range []time.Duration{500 * time.Millisecond, time.Second} {
				if waits[i] < base/2 || waits[i] > base {
					t.Errorf("wait %d = %s, want within [%s, %s]", i, waits[i], base/2, base)
				}
			}
		})
	}
}

func TestBackoffDoublesToFiveSeconds(t *testing.T) {
	for attempt, base := range []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second, 4 * time.Second, 5 * time.Second, 5 * time.Second, 5 * time.Second} {
		for range 100 {
			if d := backoff(attempt); d < base/2 || d > base {
				t.Fatalf("backoff(%d) = %s, want within [%s, %s]", attempt, d, base/2, base)
			}
		}
	}
	if d := backoff(1000); d > maxBackoff {
		t.Fatalf("backoff(1000) = %s, want at most %s", d, maxBackoff)
	}
}

func TestRetryOnNetworkError(t *testing.T) {
	col := fakeotlp.New(t, func(w http.ResponseWriter, _ *http.Request, n int) {
		if n == 0 {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Errorf("hijack: %v", err)
				return
			}
			conn.Close()
			return
		}
		io.WriteString(w, "{}")
	})
	h := newHarness(t, col, nil)
	wantDelivered(t, h.export())
	if n := col.Requests(); n != 2 {
		t.Fatalf("%d requests, want 2 (one retry)", n)
	}
}

func TestRetryAfterIsHonoured(t *testing.T) {
	for _, tc := range []struct {
		name     string
		header   func() string
		min, max time.Duration
	}{
		{"seconds", func() string { return "3" }, 3 * time.Second, 3 * time.Second},
		{"zero seconds: the backoff", func() string { return "0" }, 250 * time.Millisecond, 500 * time.Millisecond},
		{"HTTP date", func() string { return time.Now().Add(5 * time.Second).UTC().Format(http.TimeFormat) }, 3 * time.Second, 5 * time.Second},
		{"HTTP date in the past: the backoff", func() string { return time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat) }, 250 * time.Millisecond, 500 * time.Millisecond},
		{"unreadable: backoff", func() string { return "soon" }, 250 * time.Millisecond, 500 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			col := fakeotlp.New(t, func(w http.ResponseWriter, _ *http.Request, n int) {
				if n == 0 {
					w.Header().Set("Retry-After", tc.header())
					w.WriteHeader(http.StatusTooManyRequests)
					return
				}
				io.WriteString(w, "{}")
			})
			h := newHarness(t, col, nil)
			wantDelivered(t, h.export())
			waits := h.recordedWaits()
			if len(waits) != 1 || waits[0] < tc.min || waits[0] > tc.max {
				t.Fatalf("waits %v, want one within [%s, %s]", waits, tc.min, tc.max)
			}
		})
	}
}

func TestRetryAfterBeyondTheTimeLeftFailsAtOnce(t *testing.T) {
	col := fakeotlp.New(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	h := newHarness(t, col, map[string]string{"OTEL_EXPORTER_OTLP_LOGS_TIMEOUT": "10000"})
	wantFailed(t, h.export(), http.StatusServiceUnavailable)
	if n := col.Requests(); n != 1 {
		t.Fatalf("%d requests, want 1", n)
	}
	if w := h.recordedWaits(); len(w) != 0 {
		t.Fatalf("waited %v, want no wait", w)
	}
}

func TestRetriesEndWithTheTimeout(t *testing.T) {
	col := fakeotlp.New(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.WriteHeader(http.StatusBadGateway)
	})
	// The first backoff (at least 0.25 s) is beyond a 100 ms timeout.
	h := newHarness(t, col, map[string]string{"OTEL_EXPORTER_OTLP_TIMEOUT": "100"})
	wantFailed(t, h.export(), http.StatusBadGateway)
	if n := col.Requests(); n != 1 {
		t.Fatalf("%d requests, want 1", n)
	}
}

func TestNoRetryOnOtherStatuses(t *testing.T) {
	for _, status := range []int{400, 401, 404, 413, 500} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			col := fakeotlp.New(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				io.WriteString(w, `{"code":3,"message":"bad data in record 2"}`)
			})
			h := newHarness(t, col, nil)
			o := h.export()
			wantFailed(t, o, status)
			if n := col.Requests(); n != 1 {
				t.Fatalf("%d requests, want 1", n)
			}
			// The collector's message is never in the outcome (Logs: no remote text).
			if want := fmt.Sprintf("collector answered %d %s", status, http.StatusText(status)); o.Err.Error() != want {
				t.Fatalf("error %q, want %q", o.Err, want)
			}
		})
	}
}

func TestPartialSuccess(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		// rejected is the outcome's count; failed means the export failed whole.
		rejected uint64
		failed   bool
	}{
		{"rejected as a string", `{"partialSuccess":{"rejectedLogRecords":"2","errorMessage":"too large"}}`, 2, false},
		{"rejected as a number", `{"partialSuccess":{"rejectedLogRecords":1}}`, 1, false},
		{"more rejected than sent: as the collector counts", `{"partialSuccess":{"rejectedLogRecords":"9"}}`, 9, false},
		{"a warning only", `{"partialSuccess":{"errorMessage":"deprecated field"}}`, 0, false},
		{"no body", ``, 0, false},
		{"not JSON: unreadable", `ok`, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			col := fakeotlp.New(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
				io.WriteString(w, tc.body)
			})
			h := newHarness(t, col, nil)
			o := h.export()
			if n := col.Requests(); n != 1 {
				t.Fatalf("%d requests, want 1: a partial success is not retried", n)
			}
			if tc.failed {
				wantFailed(t, o, http.StatusOK)
				return
			}
			if o.Err != nil || o.Rejected != tc.rejected || o.Status != http.StatusOK {
				t.Fatalf("outcome %+v, want delivered with %d rejected", o, tc.rejected)
			}
			if tc.rejected == 0 && o.RejectErr != nil {
				t.Fatalf("reject error %v, want none: nothing was rejected", o.RejectErr)
			}
			if want := fmt.Sprintf("collector rejected %d records", tc.rejected); tc.rejected > 0 && (o.RejectErr == nil || o.RejectErr.Error() != want) {
				t.Fatalf("reject error %v, want %q", o.RejectErr, want)
			}
		})
	}
}

// The partial-success count is read under the signal's own member; another
// signal's is a member OTLP does not define for it, ignored.
func TestPartialSuccessMemberIsTheSignals(t *testing.T) {
	for _, tc := range []struct {
		sig       Signal
		body      string
		rejected  uint64
		rejectErr string
	}{
		{Logs, `{"partialSuccess":{"rejectedLogRecords":"2"}}`, 2, "collector rejected 2 records"},
		{Logs, `{"partialSuccess":{"rejectedDataPoints":"2"}}`, 0, ""},
		{Logs, `{"partialSuccess":{"rejectedDataPoints":true}}`, 0, ""},
		{Metrics, `{"partialSuccess":{"rejectedDataPoints":"3"}}`, 3, "collector rejected 3 data points"},
		{Metrics, `{"partialSuccess":{"rejectedDataPoints":4}}`, 4, "collector rejected 4 data points"},
		{Metrics, `{"partialSuccess":{"rejectedLogRecords":"3"}}`, 0, ""},
	} {
		t.Run(string(tc.sig)+" "+tc.body, func(t *testing.T) {
			col := fakeotlp.New(t, func(w http.ResponseWriter, _ *http.Request, _ int) { io.WriteString(w, tc.body) })
			o := newSignalHarness(t, tc.sig, col, nil).export()
			if o.Err != nil || o.Rejected != tc.rejected {
				t.Fatalf("outcome %+v, want delivered with %d rejected", o, tc.rejected)
			}
			if got := fmt.Sprint(o.RejectErr); tc.rejectErr != "" && got != tc.rejectErr {
				t.Fatalf("reject error %q, want %q", got, tc.rejectErr)
			}
		})
	}
	t.Run("metrics: not an ExportMetricsServiceResponse", func(t *testing.T) {
		col := fakeotlp.New(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
			io.WriteString(w, `{"partialSuccess":{"rejectedDataPoints":"1.5"}}`)
		})
		o := newSignalHarness(t, Metrics, col, nil).export()
		if want := "collector answered 200 OK with a body that is not an ExportMetricsServiceResponse"; o.Err == nil || o.Err.Error() != want {
			t.Fatalf("outcome %+v, want the error %q", o, want)
		}
	})
}

func TestOversizedResponseFailsUnretried(t *testing.T) {
	for _, status := range []int{200, 503} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			col := fakeotlp.New(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
				w.WriteHeader(status)
				w.Write(bytes.Repeat([]byte(" "), maxResponseSize+1))
			})
			h := newHarness(t, col, nil)
			wantFailed(t, h.export(), status)
			if n := col.Requests(); n != 1 {
				t.Fatalf("%d requests, want 1", n)
			}
		})
	}
}

// A redirect is not followed: neither the body nor a configured header reaches the
// target — here another host name, which Go's redirect rule would send a custom
// credential header to — and the export fails at once, with the redirect's status.
func TestRedirectIsNotFollowed(t *testing.T) {
	reached := make(chan string, 10)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached <- r.Header.Get("X-Api-Key")
		io.WriteString(w, "{}")
	}))
	defer target.Close()
	destination := strings.Replace(target.URL, "127.0.0.1", "localhost", 1)
	for _, status := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			col := fakeotlp.New(t, func(w http.ResponseWriter, r *http.Request, _ int) {
				http.Redirect(w, r, destination, status)
			})
			h := newHarness(t, col, map[string]string{"OTEL_EXPORTER_OTLP_HEADERS": "x-api-key=test-secret"})
			o := h.export()
			select {
			case key := <-reached:
				t.Fatalf("the redirect's target was reached (x-api-key %q)", key)
			default:
			}
			if n := col.Requests(); n != 1 {
				t.Fatalf("%d requests, want 1: a redirect is not retried", n)
			}
			wantFailed(t, o, status)
		})
	}
}

// A 302 to a login page answering 200 is not a delivery: nothing reached the
// collector.
func TestRedirectToAPageAnsweringOKIsNotExported(t *testing.T) {
	login := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, "<html><body>Sign in</body></html>")
	}))
	defer login.Close()
	col := fakeotlp.New(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		http.Redirect(w, r, login.URL+"/login", http.StatusFound)
	})
	h := newHarness(t, col, nil)
	wantFailed(t, h.export(), http.StatusFound)
}

// The collector's own text — a Status message, a partial success's errorMessage —
// never reaches the outcome: an auth proxy may echo the credential in it.
func TestCollectorTextIsNeverInTheOutcome(t *testing.T) {
	for _, tc := range []struct {
		name    string
		partial bool
		want    string
	}{
		{"failure status", false, "collector answered 401 Unauthorized"},
		{"partial success", true, "collector rejected 1 records"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			col := fakeotlp.New(t, func(w http.ResponseWriter, r *http.Request, _ int) {
				message := "invalid credential: " + r.Header.Get("Authorization")
				if tc.partial {
					json.NewEncoder(w).Encode(map[string]any{"partialSuccess": map[string]any{
						"rejectedLogRecords": "1", "errorMessage": message,
					}})
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				json.NewEncoder(w).Encode(map[string]any{"code": 16, "message": message})
			})
			h := newHarness(t, col, map[string]string{"OTEL_EXPORTER_OTLP_HEADERS": "authorization=Bearer%20test-secret"})
			o := h.export()
			got := o.Err
			if tc.partial {
				got = o.RejectErr
			}
			if got == nil || got.Error() != tc.want {
				t.Fatalf("outcome %+v, want the error %q: never the collector's text", o, tc.want)
			}
		})
	}
}

// A transport failure is in the gateway's own words: Go's error quotes the bytes it
// could not parse — here a header line carrying a credential.
func TestTransportErrorTextIsNeverInTheOutcome(t *testing.T) {
	col := fakeotlp.New(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		defer conn.Close()
		buf.WriteString("HTTP/1.1 200 OK\r\nBearer test-secret\r\n\r\n")
		buf.Flush()
	})
	// The first backoff (at least 0.25 s) is beyond the timeout: one attempt.
	h := newHarness(t, col, map[string]string{"OTEL_EXPORTER_OTLP_LOGS_TIMEOUT": "100"})
	o := h.export()
	if o.Err == nil {
		t.Fatalf("outcome %+v, want failed", o)
	}
	if msg := o.Err.Error(); strings.Contains(msg, "test-secret") || strings.Contains(msg, "Bearer") {
		t.Fatalf("error %q carries the bytes the collector sent", msg)
	}
	if o.Status != 0 {
		t.Fatalf("status %d, want none: no answer was read", o.Status)
	}
}

// A 2xx counts as delivered only when its body is empty or the signal's export
// response; one that cannot be read, or is something else, fails the export
// unretried: the collector may have taken some of it.
func TestUnreadableAnswerFailsUnretried(t *testing.T) {
	for _, tc := range []struct {
		name    string
		respond func(w http.ResponseWriter)
	}{
		{"truncated partial success", func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Content-Length", "1000")
			io.WriteString(w, `{"partialSuccess":{"rejectedLogRecords":"1"`)
		}},
		{"a login page", func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "text/html")
			io.WriteString(w, "<html><body>Sign in</body></html>")
		}},
		{"not JSON", func(w http.ResponseWriter) { io.WriteString(w, "ok") }},
		{"a JSON array", func(w http.ResponseWriter) { io.WriteString(w, "[]") }},
		{"JSON null", func(w http.ResponseWriter) { io.WriteString(w, "null") }},
		{"partialSuccess not an object", func(w http.ResponseWriter) { io.WriteString(w, `{"partialSuccess":"none"}`) }},
		{"rejectedLogRecords not an integer", func(w http.ResponseWriter) {
			io.WriteString(w, `{"partialSuccess":{"rejectedLogRecords":true}}`)
		}},
		{"rejectedLogRecords a fraction", func(w http.ResponseWriter) {
			io.WriteString(w, `{"partialSuccess":{"rejectedLogRecords":"1.5"}}`)
		}},
		{"errorMessage not a string", func(w http.ResponseWriter) {
			io.WriteString(w, `{"partialSuccess":{"rejectedLogRecords":"1","errorMessage":7}}`)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			col := fakeotlp.New(t, func(w http.ResponseWriter, _ *http.Request, _ int) { tc.respond(w) })
			h := newHarness(t, col, nil)
			o := h.export()
			if n := col.Requests(); n != 1 {
				t.Fatalf("%d requests, want 1: an unreadable answer is not retried", n)
			}
			wantFailed(t, o, http.StatusOK)
		})
	}
}

// An export response without rejections delivers the body, whatever members OTLP
// does not define it carries.
func TestAnswersThatDeliver(t *testing.T) {
	for _, body := range []string{``, "\n", `{}`, `{"partialSuccess":null}`, `{"partialSuccess":{}}`,
		`{"partialSuccess":{"rejectedLogRecords":null,"errorMessage":null}}`, `{"extra":[1,{"x":2}]}`,
		`{"partialSuccess":{"rejectedLogRecords":"0","errorMessage":"","extra":true}}`} {
		t.Run(body, func(t *testing.T) {
			col := fakeotlp.New(t, func(w http.ResponseWriter, _ *http.Request, _ int) { io.WriteString(w, body) })
			h := newHarness(t, col, nil)
			wantDelivered(t, h.export())
		})
	}
}

// A Retry-After beyond any export's timeout — two days, or more than a duration
// holds — fails the export without a retry: it saturates, never reads as absent.
func TestLongRetryAfterFailsTheExport(t *testing.T) {
	for _, header := range []string{"172800", "99999999999999999999"} {
		t.Run(header, func(t *testing.T) {
			col := fakeotlp.New(t, func(w http.ResponseWriter, _ *http.Request, n int) {
				if n == 0 {
					w.Header().Set("Retry-After", header)
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				io.WriteString(w, "{}")
			})
			h := newHarness(t, col, nil)
			o := h.export()
			if n := col.Requests(); n != 1 {
				t.Fatalf("%d requests (waits %v), want 1: the export fails without a retry", n, h.recordedWaits())
			}
			wantFailed(t, o, http.StatusServiceUnavailable)
			if got := retryAfter(header, time.Now()); got < 172800*time.Second {
				t.Fatalf("retryAfter(%q) = %v, want at least two days", header, got)
			}
		})
	}
}

// Retry-After: 0, or a date already past, still waits the backoff: the wait is
// max(Retry-After, backoff).
func TestRetryAfterBelowTheBackoffWaitsTheBackoff(t *testing.T) {
	for name, header := range map[string]func() string{
		"zero":        func() string { return "0" },
		"a past date": func() string { return time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat) },
	} {
		t.Run(name, func(t *testing.T) {
			col := fakeotlp.New(t, func(w http.ResponseWriter, _ *http.Request, n int) {
				if n < 3 {
					w.Header().Set("Retry-After", header())
					w.WriteHeader(http.StatusTooManyRequests)
					return
				}
				io.WriteString(w, "{}")
			})
			h := newHarness(t, col, nil)
			wantDelivered(t, h.export())
			waits := h.recordedWaits()
			if len(waits) != 3 {
				t.Fatalf("waits %v, want 3", waits)
			}
			for i, base := range []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second} {
				if waits[i] < base/2 || waits[i] > base {
					t.Errorf("wait %d = %s, want the backoff, within [%s, %s]", i, waits[i], base/2, base)
				}
			}
		})
	}
}

// An export cut short by its context fails at once, without a retry.
func TestExportCutShortByItsContext(t *testing.T) {
	arrived := make(chan struct{}, 1)
	col := fakeotlp.New(t, func(_ http.ResponseWriter, r *http.Request, _ int) {
		arrived <- struct{}{}
		<-r.Context().Done()
	})
	h := newHarness(t, col, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-arrived
		cancel()
	}()
	o := h.c.Export(ctx, []byte(body))
	if o.Err == nil || !strings.HasPrefix(o.Err.Error(), "export cut short") || o.Status != 0 {
		t.Fatalf("outcome %+v, want cut short", o)
	}
	if n := col.Requests(); n != 1 {
		t.Fatalf("%d requests, want 1", n)
	}
}

func TestDoubleSpecialValues(t *testing.T) {
	for f, want := range map[float64]string{math.Inf(1): `"Infinity"`, math.Inf(-1): `"-Infinity"`, 1e21: `1e+21`, 0.5: `0.5`} {
		got, err := json.Marshal(Double(f))
		if err != nil || string(got) != want {
			t.Errorf("Double(%v) = %s, %v; want %s", f, got, err, want)
		}
	}
}
