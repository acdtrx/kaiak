// Package fakeotlp is an OpenTelemetry collector for tests: it takes OTLP/HTTP JSON
// log exports, answers each as the test scripts, and keeps every export it received,
// decoded. It decodes with its own types, not the gateway's encoder types, so a test
// reading what it received checks the encoder. Test tooling only: nothing in the
// gateway binary imports it.
package fakeotlp

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// Export is an ExportLogsServiceRequest as OTLP/HTTP JSON writes it.
type Export struct {
	ResourceLogs []ResourceLogs `json:"resourceLogs"`
}

// ResourceLogs is one resource's logs, by instrumentation scope.
type ResourceLogs struct {
	Resource struct {
		Attributes []KeyValue `json:"attributes"`
	} `json:"resource"`
	ScopeLogs []ScopeLogs `json:"scopeLogs"`
}

// ScopeLogs is one instrumentation scope's records.
type ScopeLogs struct {
	Scope struct {
		Name string `json:"name"`
	} `json:"scope"`
	LogRecords []Record `json:"logRecords"`
}

// Record is one log record.
type Record struct {
	TimeUnixNano         string     `json:"timeUnixNano"`
	ObservedTimeUnixNano string     `json:"observedTimeUnixNano"`
	SeverityNumber       int        `json:"severityNumber"`
	SeverityText         string     `json:"severityText"`
	Body                 Value      `json:"body"`
	Attributes           []KeyValue `json:"attributes"`
}

// KeyValue is one attribute.
type KeyValue struct {
	Key   string `json:"key"`
	Value Value  `json:"value"`
}

// Value is an AnyValue: exactly one of its fields is set.
type Value struct {
	StringValue *string  `json:"stringValue"`
	IntValue    *string  `json:"intValue"`
	DoubleValue *float64 `json:"doubleValue"`
	BoolValue   *bool    `json:"boolValue"`
	ArrayValue  *struct {
		Values []Value `json:"values"`
	} `json:"arrayValue"`
}

// Received is one export as the collector received it.
type Received struct {
	Header http.Header
	Export Export
	// Status is the status the collector answered: 0 while the answer is pending, or
	// when the answer took the connection over.
	Status int
}

// Records are the export's records, in order, across its resources and scopes.
func (r Received) Records() []Record {
	var out []Record
	for _, rl := range r.Export.ResourceLogs {
		for _, sl := range rl.ScopeLogs {
			out = append(out, sl.LogRecords...)
		}
	}
	return out
}

// Messages are the bodies of the export's records, in order ("" for a body that is
// not a string).
func (r Received) Messages() []string {
	var out []string
	for _, rec := range r.Records() {
		message := ""
		if rec.Body.StringValue != nil {
			message = *rec.Body.StringValue
		}
		out = append(out, message)
	}
	return out
}

// Respond answers export n (from 0). The export is recorded before Respond runs, so
// Next returns it while Respond still waits.
type Respond func(w http.ResponseWriter, r *http.Request, n int)

// AnswerStatus answers each export with the status status gives: 200 with an empty
// ExportLogsServiceResponse, any other with an OTLP Status body. status may block
// until the request's context ends.
func AnswerStatus(status func(n int, r *http.Request) int) Respond {
	return func(w http.ResponseWriter, r *http.Request, n int) {
		code := status(n, r)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		if code == http.StatusOK {
			_, _ = io.WriteString(w, `{}`)
			return
		}
		_, _ = io.WriteString(w, `{"code": 3, "message": "export refused by the test"}`)
	}
}

// Collector is a running fake collector. It closes when the test ends.
type Collector struct {
	// URL is the collector's root URL; it takes exports at any path.
	URL string

	mu       sync.Mutex
	received []Received
	// read counts the exports Next returned.
	read    int
	arrived chan struct{}
}

// nextWait bounds Next's wait for an export.
const nextWait = 15 * time.Second

// New starts a collector answering each export with respond; nil answers 200 with an
// empty ExportLogsServiceResponse. An export that does not decode fails the test and
// is answered 400.
func New(t *testing.T, respond Respond) *Collector {
	t.Helper()
	if respond == nil {
		respond = AnswerStatus(func(int, *http.Request) int { return http.StatusOK })
	}
	c := &Collector{arrived: make(chan struct{}, 1)}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var export Export
		if err := json.NewDecoder(r.Body).Decode(&export); err != nil {
			t.Errorf("collector: export not decoded: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		c.mu.Lock()
		n := len(c.received)
		c.received = append(c.received, Received{Header: r.Header.Clone(), Export: export})
		c.mu.Unlock()
		select {
		case c.arrived <- struct{}{}:
		default:
		}
		sw := &statusWriter{ResponseWriter: w}
		respond(sw, r, n)
		if sw.status == 0 && !sw.hijacked {
			sw.status = http.StatusOK // net/http answers 200 for a handler that wrote nothing
		}
		c.mu.Lock()
		c.received[n].Status = sw.status
		c.mu.Unlock()
	}))
	t.Cleanup(srv.Close)
	c.URL = srv.URL
	return c
}

// Next waits for the next export Next has not returned yet, in arrival order.
func (c *Collector) Next(t *testing.T) Received {
	t.Helper()
	deadline := time.After(nextWait)
	for {
		c.mu.Lock()
		if c.read < len(c.received) {
			r := c.received[c.read]
			c.read++
			c.mu.Unlock()
			return r
		}
		c.mu.Unlock()
		select {
		case <-c.arrived:
		case <-deadline:
			t.Fatalf("no export reached the collector within %s", nextWait)
			return Received{}
		}
	}
}

// None fails the test if an export Next has not returned yet arrived.
func (c *Collector) None(t *testing.T) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.read < len(c.received) {
		t.Fatalf("unexpected export with %q", c.received[c.read].Messages())
	}
}

// Requests is the number of exports received so far.
func (c *Collector) Requests() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.received)
}

// Received returns every export received so far, in arrival order.
func (c *Collector) Received() []Received {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Received(nil), c.received...)
}

// Bodies are the bodies of every record received so far, in arrival order.
func (c *Collector) Bodies() []string {
	var out []string
	for _, r := range c.Received() {
		out = append(out, r.Messages()...)
	}
	return out
}

// statusWriter records the status a Respond answers.
type statusWriter struct {
	http.ResponseWriter
	status   int
	hijacked bool
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(p)
}

// Hijack hands the connection to a Respond that answers it by hand.
func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("fakeotlp: the connection cannot be taken over")
	}
	w.hijacked = true
	return h.Hijack()
}
