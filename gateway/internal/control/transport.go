package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"regexp"
	"strconv"

	"kaiak/internal/netfail"
)

// Header names and the protocol version as it travels in the header
// (CONTROL-PROTOCOL.md, Shape and Request checks).
const (
	headerProtocol = "Kaiak-Protocol"
	headerInstance = "Kaiak-Instance"
)

var protocolHeaderValue = strconv.Itoa(ProtocolVersion)

// maxMessageBytes bounds one stream event and a usage ack: a config document is far
// smaller.
const maxMessageBytes = 16 << 20

// errProtocolMismatch: the control plane answered without the protocol version this
// gateway speaks. Retrying cannot fix it until one side is upgraded, so it is logged
// at error level; the client still retries on its backoff and the gateway keeps
// serving what it has.
var errProtocolMismatch = errors.New("protocol version mismatch")

// statusError is an answer other than the one the endpoint succeeds with, carrying
// the protocol's error code when the body had one in the protocol's code shape.
type statusError struct {
	status int
	code   string
}

func (e *statusError) Error() string {
	if e.code == "" {
		return fmt.Sprintf("control plane answered %d", e.status)
	}
	return fmt.Sprintf("control plane answered %d %s", e.status, e.code)
}

// refusals says what failureLevel makes of a 4xx answer.
type refusals bool

const (
	// refusalWarns: the boot and the config stream log a refused answer at warning
	// level.
	refusalWarns refusals = false
	// refusalIsError: usage and status deliveries log a refused answer — the token or
	// the endpoint is wrong — at error level.
	refusalIsError refusals = true
)

// failureLevel is the log level of a failed exchange with the control plane: error
// when the failure is one retrying cannot fix — another protocol version, a malformed
// totals event (only the stream reads them), and a 4xx answer when refused says so —
// else warning.
func failureLevel(err error, refused refusals) slog.Level {
	if errors.Is(err, errProtocolMismatch) || errors.Is(err, errMalformedTotals) {
		return slog.LevelError
	}
	if s, ok := errors.AsType[*statusError](err); ok && s.status < 500 && refused == refusalIsError {
		return slog.LevelError
	}
	return slog.LevelWarn
}

// get sends one GET to the control plane with the headers every request carries,
// and checks the answer's protocol version before anything else is read. An answer
// other than 200 is a *statusError.
func (c *Client) get(ctx context.Context, path string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.send(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return nil, &statusError{status: resp.StatusCode, code: errorCode(resp.Body)}
	}
	return resp, nil
}

// post sends one JSON body to the control plane with the headers every request
// carries, and checks the answer's protocol version before anything else is
// read. An answer other than want is a *statusError.
func (c *Client) post(ctx context.Context, path string, body []byte, want int) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.send(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != want {
		defer resp.Body.Close()
		return nil, &statusError{status: resp.StatusCode, code: errorCode(resp.Body)}
	}
	return resp, nil
}

// send adds the token, the protocol version and the instance to req, sends it, and
// refuses a redirect (not followed: defaultHTTPClient) and an answer that does not
// carry the protocol version this gateway speaks. Its errors reach log lines, so they
// carry nothing the answer sent (docs/specs/GATEWAY.md, Logs: no remote text): a
// transport failure is named by its class — Go quotes a header line it cannot parse
// whole — and a mismatched version by protocolSeen.
func (c *Client) send(req *http.Request) (*http.Response, error) {
	req.Header.Set("Authorization", "Bearer "+c.opts.Token)
	req.Header.Set(headerProtocol, protocolHeaderValue)
	req.Header.Set(headerInstance, c.opts.Instance)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, errors.New(netfail.Class(err))
	}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		resp.Body.Close()
		return nil, fmt.Errorf("control plane answered %d, a redirect: redirects are not followed, "+
			"so KAIAK_CONTROL_URL must be the address that answers", resp.StatusCode)
	}
	if got := resp.Header.Values(headerProtocol); len(got) != 1 || got[0] != protocolHeaderValue {
		resp.Body.Close()
		return nil, fmt.Errorf("%w: control plane answered %d %s, gateway speaks %s",
			errProtocolMismatch, resp.StatusCode, protocolSeen(got), protocolHeaderValue)
	}
	return resp, nil
}

// protocolSeen says what a mismatched answer's Kaiak-Protocol values held: none,
// something other than one integer, or the integer — never the value as received,
// which can be anything (a URL answering with the bearer token in it).
func protocolSeen(values []string) string {
	if len(values) == 0 {
		return "without " + headerProtocol
	}
	if n, err := strconv.Atoi(values[0]); len(values) == 1 && err == nil && strconv.Itoa(n) == values[0] {
		return "with " + headerProtocol + " " + values[0]
	}
	return "with an invalid " + headerProtocol
}

// errorCodeShape is the protocol's error code shape (CONTROL-PROTOCOL.md, Shape:
// errors), at most maxErrorCodeChars long.
var errorCodeShape = regexp.MustCompile(`^[a-z]+(-[a-z]+)*$`)

const maxErrorCodeChars = 64

// errorCode reads the { error, detail? } body's code; "" when the body is not one or
// the code is not in the protocol's code shape — an answer that is not the control
// plane's own can carry anything there, and the code reaches log lines.
func errorCode(body io.Reader) string {
	var e struct {
		Error string `json:"error"`
	}
	data, err := io.ReadAll(io.LimitReader(body, 64<<10))
	if err != nil || json.Unmarshal(data, &e) != nil {
		return ""
	}
	if len(e.Error) > maxErrorCodeChars || !errorCodeShape.MatchString(e.Error) {
		return ""
	}
	return e.Error
}

// openStream opens GET /v1/stream: the control plane sends its current config, then
// totals, then every change.
func (c *Client) openStream(ctx context.Context) (*http.Response, error) {
	resp, err := c.get(ctx, "/stream")
	if err != nil {
		return nil, fmt.Errorf("open config stream: %w", err)
	}
	if mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")); mediaType != "text/event-stream" {
		resp.Body.Close()
		return nil, errors.New("open config stream: the answer is not text/event-stream")
	}
	return resp, nil
}
