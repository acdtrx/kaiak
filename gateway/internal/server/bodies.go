package server

import (
	"errors"
	"io"
	"net/http"
	"sync"
)

// DefaultBodyMemory is the body budget when KAIAK_BODY_MEMORY_BYTES is unset: 128
// bodies at the default 4 MiB cap, thousands of ordinary chat requests (a few KiB to
// a few hundred KiB each), and a bounded share of a container sized at 1 GiB or more.
const DefaultBodyMemory int64 = 512 << 20

// BodyBudget bounds the request body bytes the gateway holds at once, across every
// request (docs/specs/GATEWAY.md, Request pipeline: request bodies). A request takes
// its share step by step as its body arrives, each step before its buffer is made,
// and gives it back when the body is dropped: once the response starts relaying (no retry can use the body after that)
// or when the request ends. A request that finds the budget spent is refused at once
// with 503 server_busy; nothing waits for room, so nothing can deadlock on it.
type BodyBudget struct {
	limit int64

	mu   sync.Mutex
	used int64
}

// NewBodyBudget returns a budget of limit bytes (above 0).
func NewBodyBudget(limit int64) *BodyBudget { return &BodyBudget{limit: limit} }

// Limit is the budget's size: also the largest body any request may send.
func (b *BodyBudget) Limit() int64 { return b.limit }

// InUse is the bytes currently taken.
func (b *BodyBudget) InUse() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.used
}

// take takes n bytes; false, taking nothing, when fewer are left.
func (b *BodyBudget) take(n int64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if n > b.limit-b.used {
		return false
	}
	b.used += n
	return true
}

// give returns n bytes taken earlier.
func (b *BodyBudget) give(n int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.used -= n
}

// bodyStep is the first size of a body's buffer. The buffer then doubles as the body
// arrives, taking the budget for each growth before it is made, so a request holds
// about what its client has sent — never more than bodyStep before a byte arrives.
// Small enough that idle connections declaring large bodies hold next to nothing;
// the doublings cost little (about one extra copy of the body in all).
const bodyStep = 4 << 10

// readBody reads the request body, at most limit bytes, holding its share of the
// budget for the request (released by releaseBody). The buffer grows as the body
// arrives, whether or not a Content-Length is declared — the declared length only
// caps it: a client could declare a large body and send nothing, and taking the
// declared length up front let a few such connections spend the whole budget.
func (rq *request) readBody(bodies *BodyBudget, limit int64) ([]byte, *apiError) {
	rq.bodies = bodies
	src := http.MaxBytesReader(rq.w, rq.r.Body, limit)
	size := limit
	declared := rq.r.ContentLength >= 0
	if declared {
		size = min(rq.r.ContentLength, limit)
	}
	body := make([]byte, 0)
	for {
		if len(body) == cap(body) {
			if int64(cap(body)) == size {
				// Reading on to the end marks the body read in full, so the
				// connection can serve the next request (the listener closes it
				// after an early answer); a body of unknown length at the limit
				// must end here too.
				if err := expectEnd(src); err != nil {
					return nil, bodyReadError(err, limit)
				}
				return body, nil
			}
			grow := min(max(2*int64(cap(body)), bodyStep), size)
			if !rq.holdBody(grow - int64(cap(body))) {
				return nil, rq.serverBusy()
			}
			grown := make([]byte, len(body), grow)
			copy(grown, body)
			body = grown
		}
		n, err := src.Read(body[len(body):cap(body)])
		body = body[:len(body)+n]
		if errors.Is(err, io.EOF) {
			if declared && int64(len(body)) < size {
				// net/http reports a body cut short as io.ErrUnexpectedEOF; this
				// guards any other reader.
				return nil, errReadBody()
			}
			return body, nil
		}
		if err != nil {
			return nil, bodyReadError(err, limit)
		}
	}
}

// expectEnd reads the one more byte a complete body does not have.
func expectEnd(src io.Reader) error {
	var probe [1]byte
	_, err := io.ReadFull(src, probe[:])
	switch {
	case errors.Is(err, io.EOF):
		return nil
	case err == nil:
		return errors.New("request body longer than declared")
	}
	return err
}

func bodyReadError(err error, limit int64) *apiError {
	if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
		return errBodyTooLarge(limit)
	}
	return errReadBody()
}

// holdBody takes n more bytes of the budget for the request's body.
func (rq *request) holdBody(n int64) bool {
	if !rq.bodies.take(n) {
		return false
	}
	rq.bodyHeld += n
	return true
}

// serverBusy is the answer of a request that found the body budget spent.
func (rq *request) serverBusy() *apiError {
	rq.w.Header().Set("Retry-After", "1")
	return errServerBusy()
}

// releaseBody drops the request's body and gives its share of the budget back. It
// runs once the response starts relaying, and again (doing nothing more) as the
// request ends.
func (rq *request) releaseBody() {
	rq.body = nil
	if rq.bodyHeld > 0 {
		rq.bodies.give(rq.bodyHeld)
		rq.bodyHeld = 0
	}
}
