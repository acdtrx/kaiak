// Package netfail names the class of a failed exchange with a remote party in the
// gateway's own words (docs/specs/GATEWAY.md, Observability → Logs: no remote text).
// Go builds some transport errors from the bytes it received — a malformed header
// line is quoted whole, credentials included — so a log line carries the class,
// never the error's text.
package netfail

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/textproto"
	"syscall"
)

// Class is err's class: timed out, cancelled, name not resolved, connection
// refused, host unreachable, connection closed, TLS failure, malformed response —
// or, for anything else, connection failed.
func Class(err error) string {
	var netErr net.Error
	var dnsErr *net.DNSError
	var protoErr textproto.ProtocolError
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &netErr) && netErr.Timeout():
		return "timed out"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.As(err, &dnsErr):
		return "name not resolved"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection refused"
	case errors.Is(err, syscall.EHOSTUNREACH), errors.Is(err, syscall.ENETUNREACH):
		return "host unreachable"
	case errors.Is(err, syscall.ECONNRESET), errors.Is(err, syscall.EPIPE),
		errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return "connection closed"
	case isTLS(err):
		return "TLS failure"
	case errors.As(err, &protoErr):
		return "malformed response"
	}
	return "connection failed"
}

// isTLS reports a handshake or certificate failure.
func isTLS(err error) bool {
	var (
		header     tls.RecordHeaderError
		alert      tls.AlertError
		verify     *tls.CertificateVerificationError
		authority  x509.UnknownAuthorityError
		hostname   x509.HostnameError
		invalid    x509.CertificateInvalidError
		systemRoot x509.SystemRootsError
	)
	return errors.As(err, &header) || errors.As(err, &alert) || errors.As(err, &verify) ||
		errors.As(err, &authority) || errors.As(err, &hostname) || errors.As(err, &invalid) ||
		errors.As(err, &systemRoot)
}
