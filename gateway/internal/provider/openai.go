package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/http/httptrace"
	"strings"
	"time"

	"kaiak/internal/config"
	"kaiak/internal/sse"
)

// openAIFormat is the provider for backends speaking the OpenAI wire format:
// openai-compatible (vLLM, llama-server, SGLang, OpenAI) and azure-openai (Azure's
// /openai/v1/ API). The two differ only in URL layout and credential header.
type openAIFormat struct {
	backend    *config.Backend
	client     *http.Client
	credential string
}

// apiURL joins b's base URL and path, a path below the OpenAI API's version prefix
// (docs/specs/GATEWAY.md, Base URLs): an openai-compatible base URL already ends in
// the API version path; an azure-openai one is the resource endpoint.
func apiURL(b *config.Backend, path string) string {
	if b.Type == config.BackendAzureOpenAI {
		return b.BaseURL + "/openai/v1/" + path
	}
	return b.BaseURL + "/" + path
}

func (p *openAIFormat) url(e Endpoint) string { return apiURL(p.backend, e.path()) }

// Send implements Provider. Besides *Error and the context's error, it returns a plain
// error when the upstream request cannot be built from req — a gateway fault, since
// the inbound stage has validated the body.
func (p *openAIFormat) Send(ctx context.Context, req *Request) (Response, error) {
	body, stripUsage, err := passthroughBody(req)
	if err != nil {
		return nil, fmt.Errorf("edit request body: %w", err)
	}

	// The edited body is dropped once Send returns: the first event is in, so the
	// transport will not send it again. req is not captured by anything that outlives
	// Send (the trace below lives as long as the response), so the client's body is
	// released with the pipeline's copy.
	upstreamBody := newUpstreamBody(body)
	body = nil
	defer upstreamBody.release()

	upstreamCtx, cancel := context.WithCancelCause(ctx)
	if sent := req.Sent; sent != nil {
		upstreamCtx = httptrace.WithClientTrace(upstreamCtx, &httptrace.ClientTrace{
			WroteRequest: func(info httptrace.WroteRequestInfo) {
				if info.Err == nil {
					sent()
				}
			},
		})
	}
	// A stream's first-event timer stops at the first event, where the stall timer
	// takes over; a non-stream request's response timer runs until the body ends.
	limit, limitCause := p.backend.FirstEventTimeout, errFirstEventTimeout
	if !req.Stream {
		limit, limitCause = p.backend.ResponseTimeout, errResponseTimeout
	}
	timer := time.AfterFunc(limit, func() { cancel(limitCause) })
	// fail classifies an error seen before the first event and releases the request.
	fail := func(err error) error {
		timer.Stop()
		cause := context.Cause(upstreamCtx)
		cancel(nil)
		switch {
		case ctx.Err() != nil:
			return ctx.Err()
		case errors.Is(cause, errFirstEventTimeout):
			return &Error{Code: CodeTimeout, Err: fmt.Errorf("backend %s: no first event within %s", p.backend.ID, limit)}
		case errors.Is(cause, errResponseTimeout):
			return &Error{Code: CodeResponseTimeout, Err: fmt.Errorf("backend %s: no response within %s", p.backend.ID, limit)}
		}
		return &Error{Code: CodeUnavailable, Err: fmt.Errorf("backend %s: %w", p.backend.ID, err)}
	}

	upstream, err := http.NewRequestWithContext(upstreamCtx, http.MethodPost, p.url(req.Endpoint), upstreamBody.reader())
	if err != nil {
		timer.Stop()
		cancel(nil)
		return nil, fmt.Errorf("build upstream request: %w", err)
	}
	upstream.ContentLength = upstreamBody.size
	upstream.GetBody = upstreamBody.getBody
	p.setHeaders(upstream.Header, req)

	resp, err := p.client.Do(upstream)
	if err != nil {
		return nil, fail(err)
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		_ = resp.Body.Close() // the backend's refusal body is not relayed
		timer.Stop()
		cancel(nil)
		return nil, &Error{Code: CodeAuthFailed, Err: fmt.Errorf("backend %s answered %d", p.backend.ID, resp.StatusCode)}
	}
	if resp.StatusCode == http.StatusNotFound {
		if modelMissing(resp, req.Deployment.Model) {
			_ = resp.Body.Close() // the backend's answer names its model: not relayed
			timer.Stop()
			cancel(nil)
			return nil, &Error{Code: CodeModelMissing, Err: fmt.Errorf("backend %s answered 404: model %q does not exist there",
				p.backend.ID, req.Deployment.Model)}
		}
	}

	publicModel, _ := json.Marshal(req.PublicModel) // a string always encodes
	r := newUpstreamResponse(upstreamCtx, cancel, resp, stripUsage, publicModel)
	r.backendID = p.backend.ID
	first, err := r.read()
	if err != nil && !errors.Is(err, io.EOF) {
		if !r.succeeded() && ctx.Err() == nil {
			// An error answer broken before its first byte (the connection lost, a
			// timeout) is still that answer: its status came with the headers, and
			// accounting, retries and the circuit go by it. The break is the first
			// Next.
			timer.Stop()
			r.pending = err
			return r, nil
		}
		err = fail(err)
		r.Close()
		return nil, err
	}
	if req.Stream {
		if !timer.Stop() {
			// The timer fired as the first event arrived: the request is cancelled.
			err = fail(errFirstEventTimeout)
			r.Close()
			return nil, err
		}
		r.stallTimeout = p.backend.StallTimeout
		r.stall = time.AfterFunc(r.stallTimeout, func() { cancel(errStalled) })
		r.stall.Stop()
	} else {
		r.responseTimer, r.responseTimeout = timer, limit
	}
	if err != nil {
		r.pending = err
	} else {
		r.peeked = &first
	}
	return r, nil
}

// maxNotFoundBody is the most of a 404 answer read to tell a missing model from a
// caller's 404; error answers are small.
const maxNotFoundBody = 64 << 10

// modelMissing reads a 404 answer and reports whether it says the deployment's model
// does not exist on the backend: its error message names model as a whole word, or
// its error code is model_not_found (OpenAI) or DeploymentNotFound (Azure). The
// answer's shapes vary by server — {"error": {"message", "code"}} (OpenAI, vLLM),
// {"message", "code"} at the top level (older vLLM), {"error": "<message>"}
// (Ollama). What was read is put back in front of the body, so an answer that is not
// about the model is relayed whole — and a read error is not the model's: the body
// returns it again after what was read.
func modelMissing(resp *http.Response, model string) bool {
	head, err := io.ReadAll(io.LimitReader(resp.Body, maxNotFoundBody))
	resp.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(head), resp.Body), resp.Body}
	if err != nil {
		return false
	}
	var answer struct {
		Error   json.RawMessage `json:"error"`
		Message string          `json:"message"`
		Code    json.RawMessage `json:"code"`
	}
	if json.Unmarshal(head, &answer) != nil {
		return false
	}
	message, code := answer.Message, answer.Code
	var nested struct {
		Message string          `json:"message"`
		Code    json.RawMessage `json:"code"`
	}
	switch {
	case json.Unmarshal(answer.Error, &message) == nil:
	case json.Unmarshal(answer.Error, &nested) == nil:
		message, code = nested.Message, nested.Code
	}
	var codeName string
	if json.Unmarshal(code, &codeName) == nil && (codeName == "model_not_found" || codeName == "DeploymentNotFound") {
		return true
	}
	return namesWord(message, model)
}

// namesWord reports whether text contains word with no model-name character right
// before or after it, so "llama" is not found in "llama-2".
func namesWord(text, word string) bool {
	if word == "" {
		return false
	}
	nameChar := func(c byte) bool {
		return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("._/:-@+", c) >= 0
	}
	for from := 0; ; {
		i := strings.Index(text[from:], word)
		if i < 0 {
			return false
		}
		start, end := from+i, from+i+len(word)
		if (start == 0 || !nameChar(text[start-1])) && (end == len(text) || !nameChar(text[end])) {
			return true
		}
		from = start + 1
	}
}

// setHeaders builds the upstream request's headers. Nothing from the client's headers
// is forwarded except the request ID, so the client's Authorization (its kaiak key),
// cookies and anything else never reach a backend; the gateway's own credential for
// the backend is set instead.
func (p *openAIFormat) setHeaders(h http.Header, req *Request) {
	h.Set("Content-Type", "application/json")
	if req.Stream {
		h.Set("Accept", "text/event-stream")
	} else {
		h.Set("Accept", "application/json")
	}
	h.Set("User-Agent", "kaiak")
	h.Set("X-Request-Id", req.RequestID)
	setCredential(h, p.backend, p.credential)
}

// setCredential sets the gateway's credential for backend b, if it has one.
func setCredential(h http.Header, b *config.Backend, credential string) {
	if credential == "" {
		return
	}
	if b.Type == config.BackendAzureOpenAI {
		h.Set("Api-Key", credential)
	} else {
		h.Set("Authorization", "Bearer "+credential)
	}
}

// passthroughBody applies the gateway's owned edits to the client's body: the model
// name becomes the deployment's, the service tier standard, the pipeline's parameters
// are set, and a stream gets stream_options.include_usage so the backend reports
// usage. stripUsage is true when the client did not ask for usage, so the usage-only
// chunk that edit adds must not reach it.
func passthroughBody(req *Request) (body []byte, stripUsage bool, err error) {
	model, _ := json.Marshal(req.Deployment.Model) // a string always encodes
	edits := []memberEdit{setValue("model", model), {key: "service_tier", set: standardServiceTier(req)}}
	for _, p := range req.Params {
		edits = append(edits, setValue(p.Key, p.Value))
	}
	if req.Stream && !req.IncludeUsage {
		edits = append(edits, memberEdit{key: "stream_options", set: setIncludeUsage})
		stripUsage = true
	}
	body, err = editObject(req.Body, edits...)
	return body, stripUsage, err
}

// standardServiceTier returns the edit that keeps a request on standard processing
// (docs/specs/GATEWAY.md, Providers → Service tier): prices are standard-tier rates,
// and a priority request would be billed about twice what its record says. A client's
// service_tier becomes "default" on every backend. Azure chat completions also get it
// when the client sent none: there an absent tier means the deployment's own setting,
// which may be priority. Elsewhere an absent tier stays absent — backends that do not
// know the field (vLLM, llama-server, ...) are not sent it.
func standardServiceTier(req *Request) func([]byte) ([]byte, error) {
	return func(current []byte) ([]byte, error) {
		if current == nil && (req.Endpoint != ChatCompletions || req.Deployment.Backend.Type != config.BackendAzureOpenAI) {
			return nil, nil
		}
		return []byte(`"default"`), nil
	}
}

// clientHeaders are the backend response headers relayed to the client. The list is
// an allowlist: hop-by-hop headers, backend request IDs (the gateway's own
// X-Request-Id is authoritative), cookies, server identification and backend
// rate-limit headers stay behind. Retry-After-Ms is Azure's finer Retry-After.
var clientHeaders = []string{"Content-Type", "Retry-After", "Retry-After-Ms"}

// upstreamResponse is an openAIFormat response.
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
	// done: the stream carried [DONE]; choices maps each choice index seen in a
	// stream to whether its finish_reason arrived (docs/specs/GATEWAY.md, Providers:
	// complete responses).
	done    bool
	choices map[int64]bool
	// stripUsage marks the usage-only chunk Hidden.
	stripUsage bool
	// publicModel (encoded JSON) replaces the backend's model name in every stream
	// chunk; bodyModel does so in a JSON body, nil for other bodies.
	publicModel []byte
	bodyModel   *modelRewriter

	// peeked is the first event, read by Send; pending is an error to return once
	// the events read before it have been handed out.
	peeked  *Event
	pending error
}

// errResponseClosed is the cancellation cause once the caller closed the response.
var errResponseClosed = errors.New("response closed")

// maxSSEBlockBytes bounds one event block read from a backend, so a backend that never
// ends an event cannot make the gateway buffer without limit.
const maxSSEBlockBytes = 16 << 20

// pieceSize is how much of a non-stream body is read per event.
const pieceSize = 32 << 10

func newUpstreamResponse(ctx context.Context, cancel context.CancelCauseFunc, resp *http.Response, stripUsage bool,
	publicModel []byte) *upstreamResponse {
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
	if r.peeked != nil {
		ev := *r.peeked
		r.peeked = nil
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
// pinging while it serves nothing has stalled).
func (r *upstreamResponse) read() (Event, error) {
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
			return Event{}, false, err
		}
		ev = Event{Data: block.Raw}
		if block.HasData {
			ev.Payload = block.Data
			r.observeTerminal(block.Data)
			ev.Hidden = r.stripUsage && isUsageOnlyChunk(block.Data)
			if !ev.Hidden {
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
				r.pending = err
				return Event{Data: piece}, true, nil
			}
		}
		if err != nil {
			return Event{}, false, err
		}
	}
}

// succeeded reports whether the backend answered with a 2xx status. Completeness
// is checked on successful answers only: an error answer is relayed as it came.
func (r *upstreamResponse) succeeded() bool {
	return r.status >= 200 && r.status < 300
}

// observeTerminal reads what a stream chunk says about the stream's end: [DONE], or
// a choice's finish_reason.
func (r *upstreamResponse) observeTerminal(payload []byte) {
	if !r.succeeded() {
		return
	}
	if bytes.Equal(payload, []byte("[DONE]")) {
		r.done = true
		return
	}
	var chunk struct {
		Choices []struct {
			Index        int64           `json:"index"`
			FinishReason json.RawMessage `json:"finish_reason"`
		} `json:"choices"`
	}
	// A chunk that does not decode says nothing about the end.
	if json.Unmarshal(payload, &chunk) != nil {
		return
	}
	for _, c := range chunk.Choices {
		if r.choices == nil {
			r.choices = make(map[int64]bool)
		}
		finished := len(c.FinishReason) > 0 && !bytes.Equal(c.FinishReason, []byte("null"))
		r.choices[c.Index] = r.choices[c.Index] || finished
	}
}

// complete reports whether a response that reached its end was whole. A successful
// stream is complete once it carried [DONE], or once every choice it carried has its
// finish_reason (servers that end without [DONE]); a successful JSON body once its
// top-level value closed. Other answers are complete when they end.
func (r *upstreamResponse) complete() bool {
	if !r.succeeded() {
		return true
	}
	if r.stream {
		if r.done {
			return true
		}
		if len(r.choices) == 0 {
			return false
		}
		for _, finished := range r.choices {
			if !finished {
				return false
			}
		}
		return true
	}
	if r.bodyModel != nil {
		return r.bodyModel.closed()
	}
	return true
}

// rewriteChunkModel returns the block's raw bytes with the chunk's top-level model
// replaced by the public name. The chunk's JSON is spread over the block's data line
// values (joined by line breaks, which JSON reads as whitespace), so each value is
// edited in place and everything around it is kept.
func (r *upstreamResponse) rewriteChunkModel(block sse.Block) []byte {
	m := newModelRewriter(r.publicModel)
	out := make([]byte, 0, len(block.Raw)+len(r.publicModel))
	pos := 0
	for _, span := range block.DataSpans {
		out = append(out, block.Raw[pos:span[0]]...)
		out = m.rewrite(out, block.Raw[span[0]:span[1]])
		pos = span[1]
	}
	return append(out, block.Raw[pos:]...)
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
