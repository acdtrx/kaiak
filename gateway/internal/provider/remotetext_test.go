package provider

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"kaiak/internal/config"
)

// Go's HTTP client quotes a header line it cannot parse in
// its error, credentials included, and a provider's errors reach the model check's,
// the circuit's and the request's log lines. They name the failure's class instead
// (docs/specs/GATEWAY.md, Logs: no remote text) — for the probe, for a request's send,
// and for a body broken off after its first bytes.
func TestBackendBytesNeverReachAProviderError(t *testing.T) {
	const secret = "provider-secret"
	// echoHeader answers with the request's Authorization as a header line of its
	// own: not a header Go can parse.
	echoHeader := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hijack(t, w, func(rw *bufio.ReadWriter) {
			fmt.Fprintf(rw, "HTTP/1.1 200 OK\r\n%s\r\n\r\n", r.Header.Get("Authorization"))
		})
	}))
	defer echoHeader.Close()
	// echoTrailer answers a chunked body whose trailer is that line: the body breaks
	// after its first bytes, with the same quoted line in the read's error.
	echoTrailer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hijack(t, w, func(rw *bufio.ReadWriter) {
			fmt.Fprintf(rw, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nTransfer-Encoding: chunked\r\n\r\n"+
				"2\r\n{}\r\n0\r\n%s\r\n\r\n", r.Header.Get("Authorization"))
		})
	}))
	defer echoTrailer.Close()

	r := NewRegistry(func(string) (string, bool) { return secret, true })
	defer r.Retain(nil)
	backend := func(url string) *config.Backend {
		return &config.Backend{ID: "local", Type: config.BackendOpenAICompatible, BaseURL: url + "/v1",
			APIKeyEnv: "BACKEND_KEY", ConnectTimeout: time.Second, FirstEventTimeout: 5 * time.Second,
			ResponseTimeout: 5 * time.Second, StallTimeout: 5 * time.Second}
	}
	send := func(b *config.Backend) (Response, error) {
		return r.For(b).Send(context.Background(), &Request{Endpoint: ChatCompletions,
			Deployment: config.Deployment{Backend: b, Model: "m"}, Body: []byte(`{"model":"m","messages":[]}`),
			RequestID: "req-1", PublicModel: "m"})
	}
	check := func(what string, err error, want string) {
		t.Helper()
		switch {
		case err == nil:
			t.Errorf("%s: no error", what)
		case strings.Contains(err.Error(), secret):
			t.Errorf("%s: the error quotes the backend's bytes: %s", what, err)
		case err.Error() != want:
			t.Errorf("%s: %q, want %q", what, err, want)
		}
	}

	_, err := r.Probe(context.Background(), backend(echoHeader.URL))
	check("probe", err, "backend local: malformed response")

	resp, err := send(backend(echoHeader.URL))
	if resp != nil {
		resp.Close()
	}
	var perr *Error
	if err != nil && (!errors.As(err, &perr) || perr.Code != CodeUnavailable) {
		t.Errorf("send: %v, want upstream_unavailable", err)
	}
	check("send", err, "upstream_unavailable: backend local: malformed response")

	resp, err = send(backend(echoTrailer.URL))
	if err != nil {
		t.Fatalf("send to the chunked answer: %v", err)
	}
	defer resp.Close()
	for err == nil {
		_, err = resp.Next()
	}
	if errors.Is(err, io.EOF) {
		t.Fatal("the body ended cleanly; want it broken off by its trailer")
	}
	check("the body's read", err, "malformed response")
}

// hijack takes over w's connection, writes with answer, and closes it.
func hijack(t *testing.T, w http.ResponseWriter, answer func(rw *bufio.ReadWriter)) {
	conn, rw, err := w.(http.Hijacker).Hijack()
	if err != nil {
		t.Errorf("hijack: %v", err)
		return
	}
	defer conn.Close()
	answer(rw)
	rw.Flush()
}
