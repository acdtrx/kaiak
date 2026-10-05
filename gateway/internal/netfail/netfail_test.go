package netfail

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"syscall"
	"testing"
)

func TestClass(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{context.DeadlineExceeded, "timed out"},
		{&net.OpError{Op: "dial", Err: timeoutErr{}}, "timed out"},
		{fmt.Errorf("wrapped: %w", context.Canceled), "cancelled"},
		{&net.DNSError{Err: "no such host", Name: "collector"}, "name not resolved"},
		{&net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}, "connection refused"},
		{&net.OpError{Op: "dial", Err: syscall.EHOSTUNREACH}, "host unreachable"},
		{&net.OpError{Op: "read", Err: syscall.ECONNRESET}, "connection closed"},
		{io.ErrUnexpectedEOF, "connection closed"},
		{fmt.Errorf("x: %w", io.EOF), "connection closed"},
		{&tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}, "TLS failure"},
		{tls.RecordHeaderError{Msg: "first record does not look like a TLS handshake"}, "TLS failure"},
		{fmt.Errorf("broken: %w", textproto.ProtocolError(`malformed MIME header: missing colon: "Bearer x"`)), "malformed response"},
		{errors.New(`malformed HTTP response "Bearer x"`), "connection failed"},
	} {
		if got := Class(tc.err); got != tc.want {
			t.Errorf("Class(%v) = %q, want %q", tc.err, got, tc.want)
		}
	}
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

// The error a real client gets from a header line it cannot parse quotes the line;
// its class does not.
func TestClassOfAMalformedHeaderCarriesNoBytes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		defer conn.Close()
		writeRaw(buf, "HTTP/1.1 200 OK\r\nBearer test-secret\r\n\r\n")
	}))
	defer srv.Close()
	_, err := srv.Client().Get(srv.URL)
	if err == nil || !strings.Contains(err.Error(), "test-secret") {
		t.Fatalf("want Go's error to quote the line, got %v", err)
	}
	if got := Class(err); got != "malformed response" {
		t.Fatalf("Class = %q, want malformed response", got)
	}
}

func writeRaw(w *bufio.ReadWriter, s string) {
	w.WriteString(s)
	w.Flush()
}
