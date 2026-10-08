package control

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// What a control URL answers can carry anything — a proxy
// or a misdirected URL echoing the bearer token. The client's failures reach log
// lines in its own words (docs/specs/GATEWAY.md, Logs: no remote text): the
// Kaiak-Protocol header as absent, invalid or the number parsed; an error code only in
// the protocol's code shape; a transport failure as its class.

// remoteClient is a client of url, logging to logs.
func remoteClient(url string, logs *syncBuffer) *Client {
	return &Client{opts: Options{Token: testToken, Instance: testInstance}, base: url, http: http.DefaultClient,
		logger: slog.New(slog.NewJSONHandler(logs, nil))}
}

// fetchFailureLog is the line the client logs when opening the stream at answer fails.
func fetchFailureLog(t *testing.T, answer http.HandlerFunc) string {
	t.Helper()
	remote := httptest.NewServer(answer)
	defer remote.Close()
	var logs syncBuffer
	c := remoteClient(remote.URL, &logs)
	resp, err := c.openStream(context.Background())
	if err == nil {
		resp.Body.Close()
		t.Fatal("the stream opened")
	}
	c.logFetchFailure("config not received", err)
	return logs.String()
}

// echoAuthorization stands for the request's Authorization header, answered back.
const echoAuthorization = "<the request's Authorization>"

func TestProtocolMismatchLogsNoHeaderValue(t *testing.T) {
	for _, c := range []struct {
		name   string
		values []string
		want   string
	}{
		{"the token echoed", []string{echoAuthorization}, "control plane answered 200 with an invalid Kaiak-Protocol, gateway speaks 5"},
		{"absent", nil, "control plane answered 200 without Kaiak-Protocol, gateway speaks 5"},
		{"another version", []string{"4"}, "control plane answered 200 with Kaiak-Protocol 4, gateway speaks 5"},
		{"two values", []string{"5", "5"}, "control plane answered 200 with an invalid Kaiak-Protocol, gateway speaks 5"},
		{"not canonical", []string{"05"}, "control plane answered 200 with an invalid Kaiak-Protocol, gateway speaks 5"},
	} {
		t.Run(c.name, func(t *testing.T) {
			out := fetchFailureLog(t, func(w http.ResponseWriter, r *http.Request) {
				for _, v := range c.values {
					if v == echoAuthorization {
						v = r.Header.Get("Authorization")
					}
					w.Header().Add(headerProtocol, v)
				}
			})
			if strings.Contains(out, testToken) {
				t.Errorf("the header's value reached the log: %s", out)
			}
			if !strings.Contains(out, `"level":"ERROR"`) || !strings.Contains(out, c.want) {
				t.Errorf("log %s, want an error line saying %q", out, c.want)
			}
		})
	}
}

func TestErrorCodesAreLoggedOnlyInTheirShape(t *testing.T) {
	long := strings.Repeat("a", 65)
	for _, c := range []struct {
		code, want string
	}{
		{"unauthorized", "control plane answered 401 unauthorized"},
		{"protocol-version-mismatch", "control plane answered 401 protocol-version-mismatch"},
		{"Bearer " + testToken, `control plane answered 401"`},
		{"Unauthorized", `control plane answered 401"`},
		{"no--code", `control plane answered 401"`},
		{long, `control plane answered 401"`},
		{strings.Repeat("a", 64), "control plane answered 401 " + strings.Repeat("a", 64)},
	} {
		t.Run(c.code, func(t *testing.T) {
			out := fetchFailureLog(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set(headerProtocol, protocolHeaderValue)
				w.WriteHeader(http.StatusUnauthorized)
				fmt.Fprintf(w, `{"error":%q,"detail":"token %s refused"}`, c.code, testToken)
			})
			if strings.Contains(out, testToken) || strings.Contains(out, long) {
				t.Errorf("the answer's text reached the log: %s", out)
			}
			if !strings.Contains(out, c.want) {
				t.Errorf("log %s, want %q", out, c.want)
			}
		})
	}
}

func TestTransportFailuresAreLoggedAsTheirClass(t *testing.T) {
	out := fetchFailureLog(t, func(w http.ResponseWriter, r *http.Request) {
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		defer conn.Close()
		// A header line Go cannot parse: its error quotes the line.
		fmt.Fprintf(rw, "HTTP/1.1 200 OK\r\n%s\r\n\r\n", r.Header.Get("Authorization"))
		rw.Flush()
	})
	if strings.Contains(out, testToken) {
		t.Errorf("the answer's bytes reached the log: %s", out)
	}
	if !strings.Contains(out, `"exception.message":"open config stream: malformed response"`) {
		t.Errorf("log %s, want the failure's class", out)
	}
}
