package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"time"

	"kaiak/internal/provider"
)

// relayResponse writes the backend's answer to the request's last attempt — its
// status, headers and events — to the client.
// Stream events are flushed one by one as they arrive; nothing is buffered beyond one
// event. The first data event of a stream, or a body's first bytes, under a status
// below 400 tells the circuit breaker the response started. Every event, hidden or not, passes through here: the meter reads each one
// before it is written, so what the backend sent counts even when the client is gone.
// The request body is dropped first: once a response relays no retry can use it, and
// the attempts' meters took its size when they opened. A backend 5xx is answered by
// the gateway instead (answerBackendFault), and so is a 4xx whose body broke before
// its first byte (nothing of it can be relayed).
func relayResponse(ctx context.Context, rq *request, resp provider.Response) {
	rq.releaseBody()
	at := rq.lastAttempt()
	if resp.Status() >= 500 {
		rq.answerBackendFault(at, resp)
		return
	}
	if resp.Status() >= 400 {
		ev, err := resp.Next()
		if err != nil && !errors.Is(err, io.EOF) && ctx.Err() == nil {
			rq.answerBackendFault(at, &peekedResponse{Response: resp, err: err})
			return
		}
		resp = &peekedResponse{Response: resp, ev: ev, err: err}
	}
	header := rq.w.Header()
	for name, values := range resp.Header() {
		header[name] = values
	}
	if resp.Stream() {
		// nginx-style proxies in front of the gateway buffer responses unless told
		// not to, which would hold stream events back.
		header.Set("X-Accel-Buffering", "no")
	}
	at.status = resp.Status()
	defer func() { rq.relayDone = time.Now() }()
	// A relayed error answer's code and type go to the log line, read from its
	// first backendErrorReadMax bytes as they pass.
	var errText []byte
	if resp.Status() >= 400 {
		defer func() { at.errorCode, at.errorType = backendErrorFields(errText) }()
	}
	rq.w.WriteHeader(resp.Status())
	flusher := http.NewResponseController(rq.w)
	started := false
	for {
		ev, err := resp.Next()
		if errors.Is(err, io.EOF) {
			return
		}
		if err != nil {
			if ctx.Err() != nil {
				rq.endRelayCanceled(ctx)
				return
			}
			rq.relayEnd = upstreamRelayEnd(err)
			at.err = err
			rq.abort = true
			return
		}
		at.meter.Observe(ev)
		if resp.Status() >= 400 && len(errText) < backendErrorReadMax {
			errText = append(errText, ev.Data...)
		}
		if !started && (!resp.Stream() || ev.Payload != nil) {
			started = true
			// The response started — a stream's first data event (a comment block
			// proves the connection, not generation), a body's first bytes: a
			// half-open trial is decided now, not at the relay's end
			// (docs/specs/GATEWAY.md, Routing and reliability: half-open).
			if resp.Status() < 400 {
				at.slot.ResponseStarted()
			}
		}
		if rq.firstContent.IsZero() && resp.Stream() && at.meter.StreamContentSeen() {
			// Time to first token is the answering attempt's, from its send: an
			// earlier attempt's failure is not this backend's latency. Published now,
			// while the stream may run on for minutes.
			rq.firstContent = time.Now()
			rq.ttft = rq.firstContent.Sub(at.start)
			rq.ops.ObserveTimeToFirstToken(rq.model, at.deployment.Backend.ID, rq.ttft)
		}
		if ev.Hidden {
			continue
		}
		if _, err := rq.w.Write(ev.Data); err != nil {
			rq.endRelayCanceled(ctx)
			return
		}
		if resp.Stream() {
			if err := flusher.Flush(); err != nil {
				rq.endRelayCanceled(ctx)
				return
			}
		}
	}
}

// peekedResponse hands out an event already read from its response (or the error
// read instead) before the rest.
type peekedResponse struct {
	provider.Response
	ev     provider.Event
	err    error
	served bool
}

func (p *peekedResponse) Next() (provider.Event, error) {
	if !p.served {
		p.served = true
		return p.ev, p.err
	}
	return p.Response.Next()
}

// answerBackendFault answers attempt at's backend 5xx with the gateway's upstream_error under
// the backend's status and its Retry-After headers: the fault is the backend's, and
// its text may name backend internals (hosts, devices, stack traces) that are not
// the caller's business. Its error code and type go to the log line; its message
// never does — a backend can echo request content in it. A 4xx whose body broke
// before its first byte gets the same answer: the refusal is known (the status),
// its text is not.
func (rq *request) answerBackendFault(at *attempt, resp provider.Response) {
	at.status = resp.Status()
	defer func() { rq.relayDone = time.Now() }()
	var text []byte
	for len(text) < backendErrorReadMax {
		ev, err := resp.Next()
		if err != nil {
			break
		}
		at.meter.Observe(ev)
		text = append(text, ev.Data...)
	}
	at.errorCode, at.errorType = backendErrorFields(text)
	header := rq.w.Header()
	for name, values := range resp.Header() {
		if name != "Content-Type" {
			header[name] = values
		}
	}
	rq.failure = errUpstreamFault(resp.Status())
	if resp.Status() == provider.StatusOverloaded {
		rq.failure = errUpstreamOverloaded(provider.StatusOverloaded)
	}
	writeError(rq.w, rq.failure, rq.errorShape())
}

// Why a relayed response stopped early (the log line's kaiak.relay_end;
// docs/specs/GATEWAY.md, Providers: upstream failures).
const (
	relayClientClosed       = "client_closed"
	relayShutdown           = "shutdown"
	relayUpstreamFailed     = "upstream_failed"     // the connection to the backend broke
	relayUpstreamStalled    = "upstream_stalled"    // a stream silent for the stall timeout
	relayUpstreamIncomplete = "upstream_incomplete" // a successful response ended before it was complete
	relayUpstreamTimeout    = "upstream_timeout"    // a non-stream response past its response timeout
)

// upstreamRelayEnd is the kaiak.relay_end of a relay the backend side broke off with err.
func upstreamRelayEnd(err error) string {
	switch {
	case errors.Is(err, provider.ErrStalled):
		return relayUpstreamStalled
	case errors.Is(err, provider.ErrIncomplete):
		return relayUpstreamIncomplete
	case errors.Is(err, provider.ErrResponseTimeout):
		return relayUpstreamTimeout
	}
	return relayUpstreamFailed
}

// endRelayCanceled ends a relay whose client side went away: the drain cut it off
// ("shutdown": the connection is cut, so the truncated response cannot look
// complete), or the client left ("client_closed").
func (rq *request) endRelayCanceled(ctx context.Context) {
	if cutOff(ctx) {
		rq.relayEnd = relayShutdown
		rq.abort = true
		return
	}
	rq.relayEnd = relayClientClosed
}
