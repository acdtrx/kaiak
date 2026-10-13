package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// maxExportBytes bounds one export request the receiver reads: twice the gateway's
// export limit (16 MiB, docs/specs/GATEWAY.md, Request capture → The export limit).
const maxExportBytes = 32 << 20

// forwardTimeout bounds one forwarded export.
const forwardTimeout = 30 * time.Second

// traceReceiver is the run's own OTLP/HTTP receiver of traces, JSON only — what the
// gateway's trace export sends. The gateway's OTEL_EXPORTER_OTLP_TRACES_ENDPOINT
// points at it, and the capture checks read the spans it received. A gateway has one
// trace destination, so with a forward URL the receiver also posts every export it
// accepts, as received, to that second receiver (a collector, a Langfuse), with the
// forward headers.
type traceReceiver struct {
	server   *http.Server
	endpoint string // the URL the gateway posts to

	forward        string
	forwardHeaders []header
	// require are headers an export must carry, else it is answered 401: the
	// self-test's forward target checks the forward headers arrive.
	require []header
	client  *http.Client
	sends   sync.WaitGroup

	mu    sync.Mutex
	spans []span
	// refused are the exports answered other than 2xx, each why.
	refused []string
	// forwarded counts the exports the forward target answered 2xx, and the spans
	// in them; forwardFailures says why each other forward failed.
	forwarded, forwardedSpans int
	forwardFailures           []string
}

// header is one HTTP header.
type header struct{ name, value string }

// span is one span as the gateway writes it (docs/specs/GATEWAY.md, Observability →
// OTLP trace export): attributes by key, each a plain value — a string, an int64, a
// float64, a bool or a []any of these.
type span struct {
	traceID, spanID, parentSpanID, name string
	kind                                int
	start, end                          string
	attrs                               map[string]any
	status                              *spanStatus
}

type spanStatus struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// startTraceReceiver listens on a free loopback port.
func startTraceReceiver(forward string, forwardHeaders, require []header) (*traceReceiver, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	t := &traceReceiver{endpoint: "http://" + ln.Addr().String() + "/v1/traces", forward: forward,
		forwardHeaders: forwardHeaders, require: require, client: newClient(forwardTimeout)}
	t.server = &http.Server{Handler: http.HandlerFunc(t.serve), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = t.server.Serve(ln) }() // ends with ErrServerClosed at close
	return t, nil
}

// close stops the receiver and waits for the exports it is still forwarding.
func (t *traceReceiver) close() {
	_ = t.server.Close()
	t.sends.Wait()
}

func (t *traceReceiver) serve(w http.ResponseWriter, r *http.Request) {
	refuse := func(status int, why string) {
		t.mu.Lock()
		t.refused = append(t.refused, fmt.Sprintf("%d: %s", status, why))
		t.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, `{"code": 3, "message": "refused by the live-test kit's receiver"}`)
	}
	if r.Method != http.MethodPost || r.URL.Path != "/v1/traces" {
		refuse(http.StatusNotFound, r.Method+" "+r.URL.Path)
		return
	}
	for _, h := range t.require {
		if r.Header.Get(h.name) != h.value {
			refuse(http.StatusUnauthorized, "no "+h.name+" header, or another value")
			return
		}
	}
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		refuse(http.StatusUnsupportedMediaType, "Content-Type "+ct)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxExportBytes+1))
	switch {
	case err != nil:
		refuse(http.StatusBadRequest, err.Error())
		return
	case len(body) > maxExportBytes:
		refuse(http.StatusRequestEntityTooLarge, "an export over 32 MiB")
		return
	}
	spans, err := decodeSpans(body)
	if err != nil {
		refuse(http.StatusBadRequest, err.Error())
		return
	}
	t.mu.Lock()
	t.spans = append(t.spans, spans...)
	t.mu.Unlock()
	if t.forward != "" {
		t.sends.Add(1)
		go t.forwardExport(body, len(spans))
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{}`)
}

// forwardExport posts one export, as received, to the forward URL with the forward
// headers, and records the outcome. A response body is not read into the report: it
// is the remote's text.
func (t *traceReceiver) forwardExport(body []byte, spans int) {
	defer t.sends.Done()
	ctx, cancel := context.WithTimeout(context.Background(), forwardTimeout)
	defer cancel()
	failure := ""
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.forward, bytes.NewReader(body))
	if err == nil {
		req.Header.Set("Content-Type", "application/json")
		for _, h := range t.forwardHeaders {
			req.Header.Set(h.name, h.value)
		}
		var resp *http.Response
		if resp, err = t.client.Do(req); err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode/100 != 2 {
				failure = fmt.Sprintf("answered %d", resp.StatusCode)
			}
		}
	}
	if err != nil {
		failure = err.Error()
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if failure != "" {
		t.forwardFailures = append(t.forwardFailures, failure)
		return
	}
	t.forwarded++
	t.forwardedSpans += spans
}

// received are the spans received so far, in arrival order.
func (t *traceReceiver) received() []span {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]span(nil), t.spans...)
}

// refusals are the exports answered other than 2xx, each why.
func (t *traceReceiver) refusals() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.refused...)
}

// forwardResults are the exports forwarded and answered 2xx, the spans in them, and
// why each other forward failed.
func (t *traceReceiver) forwardResults() (exports, spans int, failures []string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.forwarded, t.forwardedSpans, append([]string(nil), t.forwardFailures...)
}

// spanOf is a span received for a request ID, and how many were received for it.
func (t *traceReceiver) spanOf(requestID string) (s span, n int) {
	for _, sp := range t.received() {
		if sp.attrs["kaiak.request.id"] == requestID {
			s, n = sp, n+1
		}
	}
	return s, n
}

// The OTLP/HTTP JSON trace export, as far as the checks read it.
type (
	otlpTraces struct {
		ResourceSpans []struct {
			ScopeSpans []struct {
				Spans []otlpSpan `json:"spans"`
			} `json:"scopeSpans"`
		} `json:"resourceSpans"`
	}
	otlpSpan struct {
		TraceID           string      `json:"traceId"`
		SpanID            string      `json:"spanId"`
		ParentSpanID      string      `json:"parentSpanId"`
		Name              string      `json:"name"`
		Kind              int         `json:"kind"`
		StartTimeUnixNano string      `json:"startTimeUnixNano"`
		EndTimeUnixNano   string      `json:"endTimeUnixNano"`
		Attributes        []otlpKV    `json:"attributes"`
		Status            *spanStatus `json:"status"`
	}
	otlpKV struct {
		Key   string    `json:"key"`
		Value otlpValue `json:"value"`
	}
	otlpValue struct {
		StringValue *string  `json:"stringValue"`
		IntValue    *string  `json:"intValue"`
		DoubleValue *float64 `json:"doubleValue"`
		BoolValue   *bool    `json:"boolValue"`
		ArrayValue  *struct {
			Values []otlpValue `json:"values"`
		} `json:"arrayValue"`
	}
)

// decodeSpans reads an export's spans. An export that is not OTLP's JSON, an
// attribute twice on a span, or a value with no kind or more than one is an error.
func decodeSpans(body []byte) ([]span, error) {
	var export otlpTraces
	if err := json.Unmarshal(body, &export); err != nil {
		return nil, fmt.Errorf("not an OTLP JSON trace export: %v", err)
	}
	var out []span
	for _, rs := range export.ResourceSpans {
		for _, ss := range rs.ScopeSpans {
			for _, s := range ss.Spans {
				attrs := make(map[string]any, len(s.Attributes))
				for _, kv := range s.Attributes {
					if _, dup := attrs[kv.Key]; dup {
						return nil, fmt.Errorf("span %q: attribute %s twice", s.Name, kv.Key)
					}
					v, err := kv.Value.plain()
					if err != nil {
						return nil, fmt.Errorf("span %q: attribute %s: %v", s.Name, kv.Key, err)
					}
					attrs[kv.Key] = v
				}
				out = append(out, span{traceID: s.TraceID, spanID: s.SpanID, parentSpanID: s.ParentSpanID, name: s.Name,
					kind: s.Kind, start: s.StartTimeUnixNano, end: s.EndTimeUnixNano, attrs: attrs, status: s.Status})
			}
		}
	}
	return out, nil
}

// plain is an attribute value as a plain value: an intValue's decimal string read as
// an int64.
func (v otlpValue) plain() (any, error) {
	var out []any
	if v.StringValue != nil {
		out = append(out, *v.StringValue)
	}
	if v.IntValue != nil {
		n, err := strconv.ParseInt(*v.IntValue, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("intValue %q is not a decimal integer", *v.IntValue)
		}
		out = append(out, n)
	}
	if v.DoubleValue != nil {
		out = append(out, *v.DoubleValue)
	}
	if v.BoolValue != nil {
		out = append(out, *v.BoolValue)
	}
	if v.ArrayValue != nil {
		values := []any{}
		for _, e := range v.ArrayValue.Values {
			p, err := e.plain()
			if err != nil {
				return nil, err
			}
			values = append(values, p)
		}
		out = append(out, values)
	}
	if len(out) != 1 {
		return nil, fmt.Errorf("a value with %d kinds set, want exactly one", len(out))
	}
	return out[0], nil
}

// parseForwardHeaders reads the forward headers from variable name's value, in the
// OTLP header format: key=value pairs separated by commas, values percent-decoded —
// as OTEL_EXPORTER_OTLP_TRACES_HEADERS takes them. Errors name the entry by its
// position, never its text: the values are credentials.
func parseForwardHeaders(name, v string) ([]header, error) {
	var headers []header
	for i, member := range strings.Split(v, ",") {
		member = strings.TrimSpace(member)
		if member == "" {
			continue
		}
		key, raw, ok := strings.Cut(member, "=")
		key = strings.TrimSpace(key)
		value, err := url.PathUnescape(strings.TrimSpace(raw))
		if !ok || key == "" || err != nil {
			return nil, fmt.Errorf("%s: entry %d: want key=value, the value percent-encoded", name, i+1)
		}
		headers = append(headers, header{http.CanonicalHeaderKey(key), value})
	}
	if len(headers) == 0 {
		return nil, errors.New(name + " holds no header")
	}
	return headers, nil
}
