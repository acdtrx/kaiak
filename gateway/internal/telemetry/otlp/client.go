// Package otlp is the OTLP/HTTP connection every signal's exporter shares
// (docs/specs/GATEWAY.md, Configuration sources: OTLP export; Observability → OTLP
// log export and OTLP metric export): the OTEL_* settings per signal, the resource,
// the JSON encoding's common messages, and one export — a body posted to the
// collector with the delivery rules (no redirects, what counts as delivered,
// retries with backoff and Retry-After within the timeout) and its outcome in the
// gateway's own words. It knows nothing of batching: each signal's exporter builds
// its bodies its own way and hands them over one at a time.
package otlp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"net/http"
	"strconv"
	"time"

	"kaiak/internal/netfail"
)

// Delivery is fixed, not configurable.
const (
	// maxResponseSize bounds a collector's answer, the OTLP specification's bound.
	maxResponseSize = 4 << 20
	firstBackoff    = 500 * time.Millisecond
	maxBackoff      = 5 * time.Second
)

// Client is one signal's connection to the collector its settings name. It is safe
// for concurrent use.
type Client struct {
	signal    signalInfo
	endpoint  string
	headers   []header
	timeout   time.Duration
	userAgent string
	resource  Resource
	http      *http.Client
	// after waits out a retry's delay; tests replace it.
	after func(time.Duration) <-chan time.Time
}

// NewClient is the connection s describes, under a resource made of s and svc.
func NewClient(s *Settings, svc Service) *Client {
	return &Client{
		signal:    s.signal.info(),
		endpoint:  s.endpoint.String(),
		headers:   s.headers,
		timeout:   s.timeout,
		userAgent: "kaiak/" + svc.Version,
		resource:  newResource(s, svc),
		http: &http.Client{
			Transport: http.DefaultTransport.(*http.Transport).Clone(),
			// A redirect is not followed: Go would resend the body and every
			// configured header to its target (docs/specs/GATEWAY.md, OTLP log
			// export → Delivery: no redirects). The 3xx fails the export.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		after: time.After,
	}
}

// Resource is the resource the signal's export requests carry.
func (c *Client) Resource() Resource {
	return c.resource
}

// CloseIdleConnections closes the connections kept for the next export.
func (c *Client) CloseIdleConnections() {
	c.http.CloseIdleConnections()
}

// Outcome is what came of one export. Its errors are in the gateway's own words,
// never text the collector sent (docs/specs/GATEWAY.md, Observability → Logs: no
// remote text): an auth proxy may echo the credential in its answer.
type Outcome struct {
	// Status is the collector's last answer; 0 when it gave none.
	Status int
	// Err is nil when the body was delivered.
	Err error
	// ErrorType is a failed export's class, its error.type on the exporters' own
	// counts (docs/specs/GATEWAY.md, Observability → Exporters' own counts): the
	// status that ended it as a string, ErrorMalformedResponse, ErrorTimeout, or a
	// failed connection's class; "" when the body was delivered.
	ErrorType string
	// Rejected and RejectErr are a delivered body's partial success: the items the
	// collector refused, as the signal's response counts them (rejectedLogRecords,
	// rejectedDataPoints), and their description; 0 and nil when it refused none.
	Rejected  uint64
	RejectErr error

	retry bool
	// retryAfter is the collector's Retry-After; below 0 when it sent none.
	retryAfter time.Duration
}

// Export posts body, retrying as the OTLP specification has it until the
// settings' timeout, or ctx, ends.
func (c *Client) Export(ctx context.Context, body []byte) Outcome {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	for attempt := 0; ; attempt++ {
		o := c.post(ctx, body)
		if o.Err == nil || !o.retry {
			return o
		}
		// The collector's Retry-After when it is longer than the backoff: one of 0
		// (or a date already past) would retry back to back.
		wait := max(o.retryAfter, backoff(attempt))
		if deadline, _ := ctx.Deadline(); wait >= time.Until(deadline) {
			return o
		}
		select {
		case <-c.after(wait):
		case <-ctx.Done():
			return o
		}
	}
}

func (c *Client) post(ctx context.Context, body []byte) Outcome {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return Outcome{Err: err, ErrorType: connectionErrorType(err)}
	}
	for _, h := range c.headers {
		req.Header.Add(h.name, h.value)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", c.userAgent)
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return Outcome{Err: fmt.Errorf("export cut short: %w", ctx.Err()), ErrorType: ErrorTimeout}
		}
		return Outcome{Err: errors.New("no answer from the collector: " + netfail.Class(err)),
			ErrorType: connectionErrorType(err), retry: true, retryAfter: -1}
	}
	defer resp.Body.Close()
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize+1))
	o := Outcome{Status: resp.StatusCode, retryAfter: -1}
	answered := "collector answered " + strconv.Itoa(resp.StatusCode)
	if text := http.StatusText(resp.StatusCode); text != "" {
		answered += " " + text
	}
	delivered := resp.StatusCode >= 200 && resp.StatusCode < 300
	if len(data) > maxResponseSize {
		o.Err = errors.New(answered + " with a body above 4 MiB")
		o.ErrorType = statusErrorType(resp.StatusCode)
		if delivered {
			o.ErrorType = ErrorMalformedResponse
		}
		return o
	}
	switch {
	case delivered:
		// What counts as delivered: an empty body or the signal's export response.
		// Anything else fails the export unretried — the collector may have taken
		// some of it, and a retry would duplicate those items.
		if readErr != nil {
			o.Err = errors.New(answered + ", its answer unreadable: " + netfail.Class(readErr))
			// An answer that could not be read is not the signal's response, unless
			// the export's time ran out while reading it.
			o.ErrorType = ErrorMalformedResponse
			if ctx.Err() != nil {
				o.ErrorType = ErrorTimeout
			}
			return o
		}
		rejected, ok := readExportResponse(c.signal, data)
		if !ok {
			o.Err = errors.New(answered + " with a body that is not an " + c.signal.response)
			o.ErrorType = ErrorMalformedResponse
			return o
		}
		if rejected > 0 {
			o.Rejected, o.RejectErr = rejected, fmt.Errorf("collector rejected %d %s", rejected, c.signal.items)
		}
		return o
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		o.Err = errors.New(answered + ": redirects are not followed")
		o.ErrorType = statusErrorType(resp.StatusCode)
		return o
	}
	// A failure's body, a Status, is not decoded: its message is the collector's
	// text.
	o.Err = errors.New(answered)
	o.ErrorType = statusErrorType(resp.StatusCode)
	switch resp.StatusCode {
	case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		o.retry = true
		o.retryAfter = retryAfter(resp.Header.Get("Retry-After"), time.Now())
	}
	return o
}

// readExportResponse reads a 2xx body as the signal's export response: empty, or a
// JSON object whose partialSuccess, when present, is an object with the signal's
// rejected member (rejectedLogRecords, rejectedDataPoints) an integer (or its
// decimal string) and errorMessage a string; members OTLP does not define, the
// other signals' rejected members among them, are ignored. It returns the items
// rejected, and false for a body that is not one.
func readExportResponse(sig signalInfo, data []byte) (uint64, bool) {
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return 0, true
	}
	if data[0] != '{' {
		return 0, false
	}
	var resp struct {
		PartialSuccess *struct {
			// Every signal's member is decoded raw; only sig's is then read.
			RejectedLogRecords json.RawMessage `json:"rejectedLogRecords"`
			RejectedDataPoints json.RawMessage `json:"rejectedDataPoints"`
			// ErrorMessage is decoded only to check its type: the collector's text
			// is never logged.
			ErrorMessage *string `json:"errorMessage"`
		} `json:"partialSuccess"`
	}
	if json.Unmarshal(data, &resp) != nil {
		return 0, false
	}
	if resp.PartialSuccess == nil {
		return 0, true
	}
	var raw json.RawMessage
	switch sig.rejected {
	case "rejectedLogRecords":
		raw = resp.PartialSuccess.RejectedLogRecords
	case "rejectedDataPoints":
		raw = resp.PartialSuccess.RejectedDataPoints
	}
	if raw == nil || string(raw) == "null" {
		return 0, true
	}
	var rejected flexInt
	if json.Unmarshal(raw, &rejected) != nil {
		return 0, false
	}
	if rejected <= 0 {
		return 0, true
	}
	return uint64(rejected), true
}

// flexInt is a JSON int64 written either way protobuf's JSON mapping accepts: a
// decimal string or a number.
type flexInt int64

func (n *flexInt) UnmarshalJSON(b []byte) error {
	s := string(b)
	if unquoted, err := strconv.Unquote(s); err == nil {
		s = unquoted
	}
	v, err := strconv.ParseInt(s, 10, 64)
	*n = flexInt(v)
	return err
}

// retryAfter reads a Retry-After header — seconds or an HTTP date — as the wait
// from now; below 0 when it is absent or unreadable. A wait too long for a
// duration saturates, so that it outlasts every export's timeout rather than read
// as absent.
func retryAfter(h string, now time.Time) time.Duration {
	if h == "" {
		return -1
	}
	if secs, err := strconv.ParseUint(h, 10, 64); err == nil || errors.Is(err, strconv.ErrRange) {
		if secs > uint64(maxWait/time.Second) {
			return maxWait
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(h); err == nil {
		return max(t.Sub(now), 0)
	}
	return -1
}

// maxWait is the longest duration: a Retry-After saturates there.
const maxWait = time.Duration(math.MaxInt64)

// backoff is the wait before retry attempt+1: from 0.5 s, doubling, at most 5 s,
// with jitter over its upper half.
func backoff(attempt int) time.Duration {
	base := maxBackoff
	if attempt < 4 {
		base = min(firstBackoff<<attempt, maxBackoff)
	}
	return base/2 + rand.N(base/2+1)
}

// The error.type values of a failed export that are not a status or a connection's
// class (docs/specs/GATEWAY.md, Observability → Exporters' own counts).
const (
	// ErrorRejected: the items of a partial success the collector refused.
	ErrorRejected = "rejected"
	// ErrorMalformedResponse: an answer that could not be read as the signal's
	// export response.
	ErrorMalformedResponse = "malformed_response"
	// ErrorTimeout: the export's time ran out during an attempt.
	ErrorTimeout = "timeout"
)

// statusErrorType is the error.type of an export the collector's status ended: the
// status as a string, as the HTTP convention has it.
func statusErrorType(status int) string {
	return strconv.Itoa(status)
}

// connectionErrorTypes are netfail's classes as error.type identifiers.
var connectionErrorTypes = map[string]string{
	"timed out":          ErrorTimeout,
	"cancelled":          ErrorTimeout,
	"name not resolved":  "name_not_resolved",
	"connection refused": "connection_refused",
	"host unreachable":   "host_unreachable",
	"connection closed":  "connection_closed",
	"TLS failure":        "tls_failure",
	"malformed response": ErrorMalformedResponse,
}

// connectionErrorType is the error.type of an exchange that failed with err: its
// class (netfail.Class), connection_failed for any other.
func connectionErrorType(err error) string {
	if t, ok := connectionErrorTypes[netfail.Class(err)]; ok {
		return t
	}
	return "connection_failed"
}
