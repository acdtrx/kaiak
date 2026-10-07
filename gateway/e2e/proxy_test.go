//go:build crosshalf

package e2e

// The proxy between the gateways and the sample control plane in the cross-half test:
// its address stays put while the sample behind it stops and starts again on another
// port, it can lose one usage ack — the batch reaches the control plane and is
// counted, the answer never reaches the gateway — and it keeps every status report it
// forwarded with the control plane's answer, and every usage record of the batches the
// control plane accepted: what the sample received, as sent.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"sync"
	"testing"
	"time"

	"kaiak/internal/accounting"
	"kaiak/internal/control"
)

type controlProxy struct {
	server    *httptest.Server
	transport *http.Transport
	forward   *httputil.ReverseProxy

	mu       sync.Mutex
	upstream *url.URL // nil: answers 502, as a load balancer with no backend does
	dropFor  string   // instance whose next usage answer is dropped; empty for none
	dropped  chan int // the status of each dropped answer

	statuses []forwardedStatus
	records  map[string]accounting.UsageRecord // by request ID, from accepted batches
	changes  *changes                          // notified on every forwarded status and accepted batch
}

// forwardedStatus is one POST /v1/status the proxy forwarded, and the answer.
type forwardedStatus struct {
	instance string
	body     []byte
	answer   int
}

type upstreamKey struct{}

// newControlProxy starts the proxy with no upstream; it is closed at test cleanup.
func newControlProxy(t *testing.T) *controlProxy {
	t.Helper()
	p := &controlProxy{transport: &http.Transport{}, dropped: make(chan int, 1),
		records: make(map[string]accounting.UsageRecord), changes: newChanges()}
	p.forward = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(pr.In.Context().Value(upstreamKey{}).(*url.URL))
		},
		Transport:     p.transport,
		FlushInterval: -1, // the config stream is SSE: every event goes through at once
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			w.WriteHeader(http.StatusBadGateway)
		},
	}
	p.server = httptest.NewServer(p)
	t.Cleanup(func() {
		p.server.CloseClientConnections()
		p.server.Close()
		p.transport.CloseIdleConnections()
	})
	return p
}

func (p *controlProxy) URL() string { return p.server.URL }

// setUpstream points the proxy at the control plane's base URL; empty for none.
func (p *controlProxy) setUpstream(t *testing.T, raw string) {
	t.Helper()
	var u *url.URL
	if raw != "" {
		var err error
		if u, err = url.Parse(raw); err != nil {
			t.Fatal(err)
		}
	}
	p.mu.Lock()
	p.upstream = u
	p.mu.Unlock()
	p.transport.CloseIdleConnections()
}

// dropNextUsageAnswer makes the proxy forward instance's next POST /v1/usage and
// close the gateway's connection instead of relaying the answer.
func (p *controlProxy) dropNextUsageAnswer(instance string) {
	p.mu.Lock()
	p.dropFor = instance
	p.mu.Unlock()
}

func (p *controlProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	upstream := p.upstream
	drop := upstream != nil && p.dropFor != "" && r.Method == http.MethodPost &&
		r.URL.Path == "/v1/usage" && r.Header.Get("Kaiak-Instance") == p.dropFor
	if drop {
		p.dropFor = ""
	}
	p.mu.Unlock()
	if upstream == nil {
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	r = r.WithContext(context.WithValue(r.Context(), upstreamKey{}, upstream))
	if r.Method == http.MethodPost && r.URL.Path == "/v1/status" {
		p.forwardStatus(w, r)
		return
	}
	if !drop && r.Method == http.MethodPost && r.URL.Path == "/v1/usage" {
		p.forwardUsage(w, r)
		return
	}
	if !drop {
		p.forward.ServeHTTP(w, r)
		return
	}

	out := r.Clone(r.Context())
	out.RequestURI = ""
	out.Host = ""
	out.URL = upstream.ResolveReference(&url.URL{Path: r.URL.Path})
	resp, err := p.transport.RoundTrip(out)
	if err != nil {
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	conn, _, err := http.NewResponseController(w).Hijack()
	if err != nil {
		panic(err) // an HTTP/1.1 test server always hijacks
	}
	conn.Close()
	p.dropped <- resp.StatusCode
}

// forwardStatus forwards a status report and keeps it with the answer's status.
func (p *controlProxy) forwardStatus(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	rec := &answerRecorder{ResponseWriter: w}
	p.forward.ServeHTTP(rec, r)
	p.mu.Lock()
	p.statuses = append(p.statuses, forwardedStatus{instance: r.Header.Get("Kaiak-Instance"), body: body, answer: rec.status})
	p.mu.Unlock()
	p.changes.notify()
}

// forwardUsage forwards a usage batch and, when the control plane accepted it (2xx),
// keeps its records.
func (p *controlProxy) forwardUsage(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	rec := &answerRecorder{ResponseWriter: w}
	p.forward.ServeHTTP(rec, r)
	var batch control.UsageBatch
	if rec.status < 200 || rec.status > 299 || json.Unmarshal(body, &batch) != nil {
		return
	}
	p.mu.Lock()
	for _, record := range batch.Records {
		p.records[record.RequestID] = record
	}
	p.mu.Unlock()
	p.changes.notify()
}

// record is the usage record the control plane accepted for a request ID, if any.
func (p *controlProxy) record(requestID string) (accounting.UsageRecord, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	record, ok := p.records[requestID]
	return record, ok
}

// waitRecord returns the usage record of requestID once a batch holding it was
// accepted; it waits up to limit.
func (p *controlProxy) waitRecord(t *testing.T, requestID string, limit time.Duration) accounting.UsageRecord {
	t.Helper()
	var record accounting.UsageRecord
	p.changes.wait(t, "an accepted usage record for request "+requestID, limit, func() bool {
		var ok bool
		record, ok = p.record(requestID)
		return ok
	})
	return record
}

// statusMark is the number of status reports forwarded so far: waitStatus from it
// sees only the reports after it.
func (p *controlProxy) statusMark() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.statuses)
}

// waitStatus returns the first status report from instance, from the mark on,
// accepted by the control plane (2xx) and accepted by match; it waits up to limit.
func (p *controlProxy) waitStatus(t *testing.T, what, instance string, mark int, limit time.Duration, match func(control.Status) bool) control.Status {
	t.Helper()
	var found control.Status
	next := mark
	p.changes.wait(t, "an accepted status report from "+instance+" showing "+what, limit, func() bool {
		p.mu.Lock()
		seen := p.statuses[next:]
		next = len(p.statuses)
		p.mu.Unlock()
		for _, fs := range seen {
			if fs.instance != instance || fs.answer < 200 || fs.answer > 299 {
				continue
			}
			var st control.Status
			if err := json.Unmarshal(fs.body, &st); err != nil {
				t.Fatalf("%s's status report is not a status: %v\n%s", instance, err, fs.body)
			}
			if match(st) {
				found = st
				return true
			}
		}
		return false
	})
	return found
}

// answerRecorder notes the status a handler answers with.
type answerRecorder struct {
	http.ResponseWriter
	status int
}

func (a *answerRecorder) WriteHeader(code int) {
	if a.status == 0 {
		a.status = code
	}
	a.ResponseWriter.WriteHeader(code)
}

func (a *answerRecorder) Write(b []byte) (int, error) {
	if a.status == 0 {
		a.status = http.StatusOK
	}
	return a.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the connection's writer.
func (a *answerRecorder) Unwrap() http.ResponseWriter { return a.ResponseWriter }
