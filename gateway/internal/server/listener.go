package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"kaiak/internal/logattr"
)

// readHeaderTimeout bounds how long a client may take to send request headers, so
// idle half-open connections cannot pile up.
const readHeaderTimeout = 30 * time.Second

// maxHeaderBytes bounds a request's headers (431 above it; net/http allows 4 KiB of
// slack). Client API and admin requests carry a key and a few short headers; the
// net/http default of 1 MiB would let every unauthenticated connection make the
// gateway buffer that much.
const maxHeaderBytes = 64 << 10

// ClientTimeouts bound a client's progress on a listener (docs/specs/GATEWAY.md,
// Lifecycle: client timeouts). None bounds a whole response: a stream runs as long
// as the backend generates and the client reads.
type ClientTimeouts struct {
	// Idle: a keep-alive connection waiting for its next request is closed after it.
	Idle time.Duration
	// BodyRead: a request's body must have arrived within it of the handler taking
	// the request. It also bounds the server's disposal of a body the handler left
	// unread (an early refusal): until it runs out the answer waits for that body.
	BodyRead time.Duration
	// Write: each write to the client (a stream event, a flush, a piece of a body)
	// must complete within it; a client that stops reading is taken as gone.
	Write time.Duration
}

// DefaultClientTimeouts are the client timeouts the gateway runs with unless the
// environment sets others.
var DefaultClientTimeouts = ClientTimeouts{Idle: 120 * time.Second, BodyRead: 60 * time.Second, Write: 60 * time.Second}

// Listener is one bound HTTP listener (API or admin).
type Listener struct {
	name   string
	ln     net.Listener
	server *http.Server
	logger *slog.Logger
	// cancel cancels every request context the listener hands out, with a cause.
	cancel context.CancelCauseFunc
	// stopped: stopAccepting closed ln, so Serve's accept error is the planned stop.
	stopped atomic.Bool
}

// Listen binds addr and prepares to serve handler on it, bounding clients by
// timeouts. Binding happens here, so an unusable address fails startup before
// anything serves.
func Listen(name, addr string, handler http.Handler, timeouts ClientTimeouts, logger *slog.Logger) (*Listener, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("%s listener: %w", name, err)
	}
	// server.address and server.port: the bound address split, as the HTTP
	// convention has them.
	host, _, _ := net.SplitHostPort(ln.Addr().String()) // a TCP listener's address always splits
	logger.Info("listening", "kaiak.listener.name", name, "server.address", host,
		"server.port", ln.Addr().(*net.TCPAddr).Port)
	base, cancel := context.WithCancelCause(context.Background())
	return &Listener{
		name: name,
		ln:   ln,
		server: &http.Server{
			Handler:           withClientDeadlines(handler, timeouts),
			ReadHeaderTimeout: readHeaderTimeout,
			MaxHeaderBytes:    maxHeaderBytes,
			IdleTimeout:       timeouts.Idle,
			ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
			BaseContext:       func(net.Listener) context.Context { return base },
		},
		logger: logger,
		cancel: cancel,
	}, nil
}

// LimitConnections caps the connections the listener keeps open at max
// (KAIAK_MAX_CONNECTIONS; docs/specs/GATEWAY.md, Lifecycle: client timeouts): a
// connection accepted beyond it is closed at once, before a byte is read, and refused
// is called. Idle keep-alive connections count until they close. Called before
// Serve; max 0 leaves the listener uncapped.
func (l *Listener) LimitConnections(max int64, refused func()) {
	if max > 0 {
		l.ln = &cappedListener{Listener: l.ln, max: max, refused: refused}
	}
}

// cappedListener closes accepted connections while max are open. Closing at accept
// is the cheapest refusal: no goroutine, no read, no write that a client could stall.
type cappedListener struct {
	net.Listener
	max     int64
	open    atomic.Int64
	refused func()
}

func (c *cappedListener) Accept() (net.Conn, error) {
	for {
		conn, err := c.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if c.open.Add(1) <= c.max {
			return &countedConn{Conn: conn, open: &c.open}, nil
		}
		c.open.Add(-1)
		_ = conn.Close() // refused: nothing was read or written
		c.refused()
	}
}

// countedConn gives its place back when it is closed, once however often Close is
// called.
type countedConn struct {
	net.Conn
	open *atomic.Int64
	once sync.Once
}

func (c *countedConn) Close() error {
	c.once.Do(func() { c.open.Add(-1) })
	return c.Conn.Close()
}

// CloseWrite passes net/http's half-close through: it closes a connection after an
// answer by shutting its write side first, so the client reads the answer before
// the close; without it the close could reset the connection with the answer unread.
func (c *countedConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return c.Conn.Close()
}

// Addr is the bound address (the real port when addr asked for port 0).
func (l *Listener) Addr() net.Addr { return l.ln.Addr() }

// Close releases a listener that will not Serve.
func (l *Listener) Close() error { return l.ln.Close() }

// Serve accepts connections until the listener stops accepting (the drain, or
// Shutdown). Connections already open keep being served after it returns; the error
// is non-nil only when serving failed.
func (l *Listener) Serve() error {
	err := l.server.Serve(l.ln)
	if errors.Is(err, http.ErrServerClosed) || (l.stopped.Load() && errors.Is(err, net.ErrClosed)) {
		return nil
	}
	return fmt.Errorf("%s listener: %w", l.name, err)
}

// Shutdown stops the listener: it stops accepting, closes idle connections and waits
// up to timeout for open requests, then cuts off what is left.
func (l *Listener) Shutdown(timeout time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	// Shutdown's only other error is from closing a listening socket already closed.
	if err := l.server.Shutdown(ctx); errors.Is(err, context.DeadlineExceeded) {
		l.logger.Warn("shutdown timed out; closing open connections", "kaiak.listener.name", l.name,
			logattr.Seconds("kaiak.listener.shutdown_timeout", timeout))
		l.cut()
	}
}

// stopAccepting closes the listening socket: the OS refuses new connections, while
// open connections, idle keep-alive ones included, stay served.
func (l *Listener) stopAccepting() {
	l.stopped.Store(true)
	_ = l.ln.Close() // an error means it was already closed: Serve failed on it, or never ran
}

// cut cuts off every open request: their contexts are cancelled with errDrainCut
// first, so handlers see why before their connections are closed.
func (l *Listener) cut() {
	l.cancel(errDrainCut)
	_ = l.server.Close() // its error is the listening socket's, closed already or now
}

// withClientDeadlines applies the body-read and write deadlines to every request
// handler serves. The body-read deadline is set as a request with a body is taken
// and cleared once the handler read the body (clearBodyDeadline): a read deadline
// left on the connection would also end net/http's background read — which watches
// for the client leaving — and cancel the request while a response streams. A
// request without a body gets none, since that background read is already running.
// The write deadline is renewed before each write and left in place after the last,
// so the server's final flush is bounded too; each request starts with none, so a
// deadline left by the previous request on a keep-alive connection never cuts the
// next.
func withClientDeadlines(handler http.Handler, timeouts ClientTimeouts) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rc := http.NewResponseController(w)
		// Errors mean the writer has no connection deadlines (never so for the
		// server's own); there is nothing to bound then.
		if r.ContentLength != 0 {
			_ = rc.SetReadDeadline(time.Now().Add(timeouts.BodyRead))
		}
		_ = rc.SetWriteDeadline(time.Time{})
		body := &watchedBody{ReadCloser: r.Body}
		r.Body = body
		handler.ServeHTTP(&deadlineWriter{ResponseWriter: w, rc: rc, timeout: timeouts.Write, req: r, body: body}, r)
	})
}

// clearBodyDeadline removes the body-read deadline once the handler has read the
// request body in full. (net/http also clears it when it starts its background read
// at the body's end; this does not rely on that.)
func clearBodyDeadline(w http.ResponseWriter) {
	_ = http.NewResponseController(w).SetReadDeadline(time.Time{})
}

// watchedBody records whether a request body was read to its end.
type watchedBody struct {
	io.ReadCloser
	ended bool
}

func (b *watchedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err == io.EOF {
		b.ended = true
	}
	return n, err
}

// deadlineWriter renews the connection's write deadline before every write and
// flush, and closes the connection after an answer given before the request body
// was read (an early refusal): net/http would otherwise first read and discard that
// body — waiting on a client that may never send it — before the answer leaves.
// Unwrap lets http.ResponseController reach the connection through it.
type deadlineWriter struct {
	http.ResponseWriter
	rc          *http.ResponseController
	timeout     time.Duration
	req         *http.Request
	body        *watchedBody
	wroteHeader bool
}

func (d *deadlineWriter) WriteHeader(status int) {
	if !d.wroteHeader && status >= 200 {
		d.wroteHeader = true
		if d.req.ContentLength != 0 && !d.body.ended {
			d.Header().Set("Connection", "close")
		}
	}
	d.ResponseWriter.WriteHeader(status)
}

func (d *deadlineWriter) Write(p []byte) (int, error) {
	if !d.wroteHeader {
		d.WriteHeader(http.StatusOK)
	}
	_ = d.rc.SetWriteDeadline(time.Now().Add(d.timeout))
	return d.ResponseWriter.Write(p)
}

// FlushError is what http.ResponseController.Flush calls.
func (d *deadlineWriter) FlushError() error {
	if !d.wroteHeader {
		d.WriteHeader(http.StatusOK)
	}
	_ = d.rc.SetWriteDeadline(time.Now().Add(d.timeout))
	return d.rc.Flush()
}

func (d *deadlineWriter) Flush() { _ = d.FlushError() }

func (d *deadlineWriter) Unwrap() http.ResponseWriter { return d.ResponseWriter }
