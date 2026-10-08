package provider

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"

	"kaiak/internal/netfail"
	"kaiak/internal/sse"
)

// The upstream response the core hands out: the headers relayed, the stall and
// response timers, completeness, the hidden usage chunk, the model name rewritten,
// error events relayed.

// clientHeaders are the backend response headers relayed to the client. The list is
// an allowlist: hop-by-hop headers, backend request IDs (the gateway's own
// X-Request-Id is authoritative), cookies, server identification and backend
// rate-limit headers stay behind. Retry-After-Ms is Azure's finer Retry-After.
var clientHeaders = []string{"Content-Type", "Retry-After", "Retry-After-Ms"}

// upstreamResponse is a response read through the wire core.
type upstreamResponse struct {
	status int
	header http.Header
	stream bool

	body   io.ReadCloser
	ctx    context.Context
	cancel context.CancelCauseFunc
	events *sse.Reader
	buf    []byte
	// backendID names the backend in error messages.
	backendID string
	// stall, for a streaming request, runs only while a read after the first event
	// waits for the backend: time spent writing to the client is not the backend's
	// silence. Only data events are progress: silence is the waiting summed since the
	// last one, across comment and keep-alive blocks. responseTimer, for a
	// non-streaming request, runs from the send until Close.
	stall           *time.Timer
	stallTimeout    time.Duration
	silence         time.Duration
	responseTimer   *time.Timer
	responseTimeout time.Duration
	// format reads a stream's events (docs/specs/GATEWAY.md, Providers: complete
	// responses); errorEvent: a successful stream carried an error event, relayed with
	// the gateway's message in place of the backend's, after which the stream ends
	// incomplete.
	format     streamFormat
	errorEvent *ErrorEvent
	// stripUsage marks the usage-only chunk Hidden.
	stripUsage bool
	// publicModel (encoded JSON) replaces the backend's model name in every stream
	// chunk; bodyModel does so in a JSON body, nil for other bodies.
	publicModel []byte
	bodyModel   *modelRewriter

	// peeked are the first event and the comment blocks before it, read by Send;
	// pending is an error to return once the events read before it have been handed
	// out.
	peeked  []Event
	pending error
}

// errResponseClosed is the cancellation cause once the caller closed the response.
var errResponseClosed = errors.New("response closed")

// maxSSEBlockBytes bounds one event block read from a backend, so a backend that never
// ends an event cannot make the gateway buffer without limit.
const maxSSEBlockBytes = 16 << 20

// pieceSize is how much of a non-stream body is read per event.
const pieceSize = 32 << 10

func newUpstreamResponse(ctx context.Context, cancel context.CancelCauseFunc, resp *http.Response, format Format,
	stripUsage bool, publicModel []byte) *upstreamResponse {
	r := &upstreamResponse{
		status:      resp.StatusCode,
		header:      make(http.Header),
		body:        resp.Body,
		ctx:         ctx,
		cancel:      cancel,
		stripUsage:  stripUsage,
		publicModel: publicModel,
	}
	for _, name := range clientHeaders {
		if values := resp.Header.Values(name); len(values) > 0 {
			r.header[name] = values
		}
	}
	mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	r.stream = mediaType == "text/event-stream"
	switch {
	case r.stream:
		r.events = sse.NewReader(resp.Body, maxSSEBlockBytes)
		r.format = newStreamFormat(format)
	default:
		r.buf = make([]byte, pieceSize)
		if mediaType == "application/json" || strings.HasSuffix(mediaType, "+json") {
			r.bodyModel = newModelRewriter(publicModel)
		}
	}
	return r
}

func (r *upstreamResponse) Status() int         { return r.status }
func (r *upstreamResponse) Header() http.Header { return r.header }
func (r *upstreamResponse) Stream() bool        { return r.stream }

func (r *upstreamResponse) Next() (Event, error) {
	if len(r.peeked) > 0 {
		ev := r.peeked[0]
		r.peeked = r.peeked[1:]
		return ev, nil
	}
	if r.pending != nil {
		return Event{}, r.end(r.pending)
	}
	return r.read()
}

// read reads the next event from the backend. For a streaming request the stall
// timer runs while it waits, for what is left of the stall timeout since the last
// data event: keep-alive comments are relayed but do not reset it (a backend
// pinging while it serves nothing has stalled). After an error event nothing more is
// read: the stream ends there, incomplete.
func (r *upstreamResponse) read() (Event, error) {
	if r.errorEvent != nil {
		return Event{}, &ErrorEventEnd{Event: r.errorEvent, backend: r.backendID}
	}
	var waitStart time.Time
	if r.stall != nil {
		waitStart = time.Now()
		r.stall.Reset(max(r.stallTimeout-r.silence, time.Nanosecond))
	}
	ev, data, err := r.readEvent()
	if r.stall != nil {
		if !r.stall.Stop() && err == nil {
			// The stall timer fired as the event arrived: the request is cancelled.
			err = context.Cause(r.ctx)
		}
		if data {
			r.silence = 0
		} else {
			r.silence += time.Since(waitStart)
		}
	}
	if err != nil {
		return Event{}, r.end(err)
	}
	return ev, nil
}

// end classifies the error that ended the response: a read broken off by one of the
// response's own timers is ErrStalled or ErrResponseTimeout; the end of a successful
// response that is not complete is ErrIncomplete. Classifying twice changes nothing.
func (r *upstreamResponse) end(err error) error {
	switch cause := context.Cause(r.ctx); {
	case errors.Is(cause, errStalled):
		return fmt.Errorf("backend %s: silent for %s: %w", r.backendID, r.stallTimeout, ErrStalled)
	case errors.Is(cause, errResponseTimeout):
		return fmt.Errorf("backend %s: response not complete within %s: %w", r.backendID, r.responseTimeout, ErrResponseTimeout)
	case errors.Is(err, io.EOF) && !r.complete():
		return fmt.Errorf("backend %s: %w", r.backendID, ErrIncomplete)
	}
	return err
}

// readEvent reads the next stream event or body piece; data reports a stream block
// carrying data (an event, not a comment or keep-alive).
func (r *upstreamResponse) readEvent() (ev Event, data bool, err error) {
	if r.stream {
		block, err := r.events.Next()
		if err != nil {
			return Event{}, false, err // in the gateway's words already (sse.Reader.Next)
		}
		ev = Event{Data: block.Raw}
		if block.HasData {
			ev.Payload = block.Data
			// Every data event is observed — it names the event the model rewrite
			// reads — but only a successful stream ends on an error event.
			seen := r.format.observe(block.Data)
			if r.succeeded() {
				r.errorEvent = seen.errorEvent
			}
			ev.Hidden = r.stripUsage && seen.usageOnly
			switch {
			case r.errorEvent != nil:
				ev.Data = r.relayedErrorEvent(block)
			case !ev.Hidden:
				ev.Data = r.rewriteChunkModel(block)
			}
		}
		return ev, block.HasData, nil
	}
	for {
		n, err := r.body.Read(r.buf)
		if n > 0 {
			var piece []byte
			if r.bodyModel != nil {
				piece = r.bodyModel.rewrite(nil, r.buf[:n])
			} else {
				piece = bytes.Clone(r.buf[:n])
			}
			// A piece can be all backend model name, which is not emitted.
			if len(piece) > 0 {
				r.pending = readFailure(err)
				return Event{Data: piece}, true, nil
			}
		}
		if err != nil {
			return Event{}, false, readFailure(err)
		}
	}
}

// readFailure is a non-stream body read's error as the gateway words it: the end of
// the body as it is; any other — the connection's — as its class (netfail). Go
// builds some of those from the bytes the backend sent (a trailer line it cannot
// parse is quoted whole), and the error reaches log lines (docs/specs/GATEWAY.md,
// Logs: no remote text).
func readFailure(err error) error {
	if err == nil || err == io.EOF {
		return err
	}
	return errors.New(netfail.Class(err))
}

// succeeded reports whether the backend answered with a 2xx status. Completeness
// is checked on successful answers only: an error answer is relayed as it came.
func (r *upstreamResponse) succeeded() bool {
	return r.status >= 200 && r.status < 300
}

// complete reports whether a response that reached its end was whole. A successful
// stream is complete as its format's events say (streamFormat); a successful JSON body
// once its top-level value closed. Other answers are complete when they end.
func (r *upstreamResponse) complete() bool {
	if !r.succeeded() {
		return true
	}
	if r.stream {
		return r.format.complete()
	}
	if r.bodyModel != nil {
		return r.bodyModel.closed()
	}
	return true
}

// rewriteChunkModel returns the block's raw bytes with the chunk's model replaced by
// the public name: the top-level model, and the one a format carries one level down
// (streamFormat.nestedModel). The chunk's JSON is spread over the block's data line
// values (joined by line breaks, which JSON reads as whitespace), so each value is
// edited in place and everything around it is kept.
func (r *upstreamResponse) rewriteChunkModel(block sse.Block) []byte {
	m := newNestedModelRewriter(r.publicModel, r.format.nestedModel())
	out := make([]byte, 0, len(block.Raw)+len(r.publicModel))
	pos := 0
	for _, span := range block.DataSpans {
		out = append(out, block.Raw[pos:span[0]]...)
		out = m.rewrite(out, block.Raw[span[0]:span[1]])
		pos = span[1]
	}
	return append(out, block.Raw[pos:]...)
}

// relayedErrorEvent is an error event block as the client gets it: its payload as its
// format relays one (streamFormat.relayedError), the model name rewritten as in any
// event, in a block of its own with the event's name and ID.
func (r *upstreamResponse) relayedErrorEvent(block sse.Block) []byte {
	payload := r.format.relayedError(block.Data)
	payload = newNestedModelRewriter(r.publicModel, r.format.nestedModel()).rewrite(nil, payload)
	var out []byte
	if block.Event != "" {
		out = append(out, "event: "+block.Event+"\n"...)
	}
	if block.HasID {
		out = append(out, "id: "+block.ID+"\n"...)
	}
	out = append(out, "data: "...)
	out = append(out, payload...)
	return append(out, "\n\n"...)
}

func (r *upstreamResponse) Close() {
	if r.responseTimer != nil {
		r.responseTimer.Stop()
	}
	if r.stall != nil {
		r.stall.Stop()
	}
	r.cancel(errResponseClosed)
	_ = r.body.Close() // closing a response body only releases the connection
}
