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
	"slices"
	"strings"
	"time"

	"kaiak/internal/config"
	"kaiak/internal/netfail"
	"kaiak/internal/sse"
)

// The wire core: the request, response and error handling every backend module shares
// (openai.go, azure_openai.go, vllm.go, llama_server.go, openai_compatible.go,
// anthropic.go, azure_anthropic.go). The core is not a provider and knows no backend
// type: a module prepares the upstream request — URL, credential, body edits, the
// error codes that mean a missing model, its server's answer to a path it does not
// have, whether the endpoint is one of its type's core ones — and hands it to
// sendWire; its probe reads the models list with fetchModelsList.

// wireCall is one upstream request a backend module prepared.
type wireCall struct {
	backend *config.Backend
	client  *http.Client
	url     string
	// header holds the module's credential; sendWire adds the headers every request
	// carries (wireHeaders).
	header http.Header
	// body is the edited client body (passthroughBody); stripUsage marks the
	// usage-only chunk that edit asked for as hidden.
	body       []byte
	stripUsage bool
	// missingModelCodes are the error codes by which the backend says, in a 404, that
	// the model does not exist there.
	missingModelCodes []string
	// unknownPath reports whether a 404 answer (its first maxNotFoundBody bytes) is
	// the server's answer to a path it does not have: the request reached the
	// server but no endpoint.
	unknownPath func(answer []byte) bool
	// core: the endpoint is one of the type's core endpoints, where a path the server
	// does not have means a wrong base_url. On any other endpoint the type serves, it
	// means the server's version predates the endpoint (CodeEndpointMissing).
	core bool
}

// openAICore reports whether e is a core endpoint of the OpenAI-format types: one of
// OpenAI's three (docs/specs/GATEWAY.md, Providers: an endpoint missing from a
// server).
func openAICore(e Endpoint) bool { return e.Format() == FormatOpenAI }

// sendWire sends call upstream for req and waits for the first event of the response,
// as Provider.Send says. Besides *Error and the context's error, it returns a plain
// error when the upstream request cannot be built — a gateway fault.
func sendWire(ctx context.Context, req *Request, call wireCall) (Response, error) {
	// The edited body is dropped once sendWire returns: the first event is in, so the
	// transport will not send it again. req is not captured by anything that outlives
	// the send (the trace below lives as long as the response), so the client's body is
	// released with the pipeline's copy.
	upstreamBody := newUpstreamBody(call.body)
	call.body = nil
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
	limit, limitCause := call.backend.FirstEventTimeout, errFirstEventTimeout
	if !req.Stream {
		limit, limitCause = call.backend.ResponseTimeout, errResponseTimeout
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
			return &Error{Code: CodeTimeout, Err: fmt.Errorf("backend %s: no first event within %s", call.backend.ID, limit)}
		case errors.Is(cause, errResponseTimeout):
			return &Error{Code: CodeResponseTimeout, Err: fmt.Errorf("backend %s: no response within %s", call.backend.ID, limit)}
		}
		return &Error{Code: CodeUnavailable, Err: fmt.Errorf("backend %s: %w", call.backend.ID, err)}
	}

	upstream, err := http.NewRequestWithContext(upstreamCtx, http.MethodPost, call.url, upstreamBody.reader())
	if err != nil {
		timer.Stop()
		cancel(nil)
		return nil, fmt.Errorf("build upstream request: %w", err)
	}
	upstream.ContentLength = upstreamBody.size
	upstream.GetBody = upstreamBody.getBody
	upstream.Header = call.header.Clone()
	wireHeaders(upstream.Header, req)

	resp, err := call.client.Do(upstream)
	if err != nil {
		return nil, fail(errors.New(netfail.Class(err)))
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		_ = resp.Body.Close() // the backend's refusal body is not relayed
		timer.Stop()
		cancel(nil)
		return nil, &Error{Code: CodeAuthFailed, Err: fmt.Errorf("backend %s answered %d", call.backend.ID, resp.StatusCode)}
	}
	// A 405 is read too beyond the core endpoints: vLLM answers one where a POST
	// lands on a route it has for another method only.
	if resp.StatusCode == http.StatusNotFound || (resp.StatusCode == http.StatusMethodNotAllowed && !call.core) {
		// A missing model is read first: a server may answer it in the same shape as
		// an unknown path.
		var deploymentErr *Error
		switch answer, ok := readNotFound(resp); {
		case ok && resp.StatusCode == http.StatusNotFound && modelMissing(answer, req.Deployment.Model, call.missingModelCodes):
			deploymentErr = &Error{Code: CodeModelMissing, Err: fmt.Errorf("backend %s answered 404: model %q does not exist there",
				call.backend.ID, req.Deployment.Model)}
		case ok && call.unknownPath(answer) && call.core:
			deploymentErr = &Error{Code: CodePathMissing, Err: fmt.Errorf("backend %s answered 404: no endpoint at %s (check its base_url)",
				call.backend.ID, call.url)}
		case ok && call.unknownPath(answer):
			deploymentErr = &Error{Code: CodeEndpointMissing, Err: fmt.Errorf("backend %s answered %d: its server has no %s endpoint (an older version?)",
				call.backend.ID, resp.StatusCode, req.Endpoint.path())}
		}
		if deploymentErr != nil {
			_ = resp.Body.Close() // the backend's answer is about its deployment: not relayed
			timer.Stop()
			cancel(nil)
			return nil, deploymentErr
		}
	}

	publicModel, _ := json.Marshal(req.PublicModel) // a string always encodes
	r := newUpstreamResponse(upstreamCtx, cancel, resp, req.Endpoint.Format(), call.stripUsage, publicModel)
	r.backendID = call.backend.ID
	first, err := r.read()
	if err == nil && r.errorEvent {
		// The backend gave up before anything reached the client: a failure the
		// attempt loop may retry, as a 5xx is (docs/specs/GATEWAY.md, Providers:
		// complete responses).
		timer.Stop()
		r.Close()
		return nil, &Error{Code: CodeErrorEvent, Err: fmt.Errorf("backend %s: its stream began with an error event", call.backend.ID)}
	}
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
		r.stallTimeout = call.backend.StallTimeout
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

// probeReadTimeout bounds a probe's wait for the answer once connected; the
// backend's connect timeout bounds the connection before it. The models list is
// cheap: a backend that takes longer is not ready for traffic.
const probeReadTimeout = 5 * time.Second

// maxProbeBody is the most of a probe's answer read, so the connection returns to
// the pool; a longer answer closes it (and is not a models list the probe reads).
const maxProbeBody = 1 << 20

// fetchModelsList asks backend b for its models list at url, with header (the
// module's credential), bounded by b's connect timeout plus probeReadTimeout, and
// returns the answer's body when the status is 2xx. A 404 is a *PathMissingError
// carrying pathHint, the module's word on what base_url should hold. The error may
// name the backend's address, never the credential nor text the backend sent: a
// transport failure is named by its class (netfail).
func fetchModelsList(ctx context.Context, b *config.Backend, client *http.Client, url string, header http.Header,
	pathHint string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, b.ConnectTimeout+probeReadTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("backend %s: build probe: %w", b.ID, err)
	}
	req.Header = header.Clone()
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "kaiak")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("backend %s: %s", b.ID, netfail.Class(err))
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxProbeBody))
	if err != nil {
		return nil, fmt.Errorf("backend %s: read models list: %s", b.ID, netfail.Class(err))
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, &PathMissingError{Backend: b.ID, URL: url, Hint: pathHint}
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("backend %s: models list answered %d", b.ID, resp.StatusCode)
	}
	return body, nil
}

// PathMissingError is a probe's error when the backend answered 404 for its models
// list: the server is up, and the backend's base_url is most likely wrong.
type PathMissingError struct {
	Backend string
	// URL is the models list's address; Hint says what the module expects base_url
	// to hold.
	URL  string
	Hint string
}

func (e *PathMissingError) Error() string {
	return fmt.Sprintf("backend %s: models list answered 404 at %s", e.Backend, e.URL)
}

// BaseURLHint says what the backend's base_url should hold.
func (e *PathMissingError) BaseURLHint() string { return e.Hint }

// versionPathHint is the base_url hint of the modules whose base_url is what an
// OpenAI client would use (docs/specs/GATEWAY.md, Base URLs).
const versionPathHint = "base_url should end in the API version path, e.g. /v1"

// listedModels reads an OpenAI models list and reports, through serves, whether a
// backend-side model name is one of its data[*].id.
func listedModels(b *config.Backend, body []byte) (serves func(model string) bool, err error) {
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &list) != nil || list.Data == nil {
		return nil, fmt.Errorf("backend %s: the answer is not a models list", b.ID)
	}
	ids := make(map[string]bool, len(list.Data))
	for _, m := range list.Data {
		ids[m.ID] = true
	}
	return func(model string) bool { return ids[model] }, nil
}

// maxNotFoundBody is the most of a 404 answer read to tell the deployment's failure
// (a missing model, an unknown path) from a caller's 404; error answers are small.
const maxNotFoundBody = 64 << 10

// readNotFound reads the start of a 404 (or 405) answer, up to maxNotFoundBody bytes, and puts
// what was read back in front of the body, so an answer that is the caller's is
// relayed whole. ok is false when the read failed: such an answer is not the
// deployment's, and the body returns the error again after what was read.
func readNotFound(resp *http.Response) (answer []byte, ok bool) {
	head, err := io.ReadAll(io.LimitReader(resp.Body, maxNotFoundBody))
	resp.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(head), resp.Body), resp.Body}
	return head, err == nil
}

// modelMissing reports whether a 404 answer says the deployment's model does not
// exist on the backend: its error message names model as a whole word, or its error
// code is one of codes, the module's. The answer's shapes vary by server —
// {"error": {"message", "code"}} (OpenAI, vLLM), {"message", "code"} at the top
// level (older vLLM), {"error": "<message>"} (Ollama).
func modelMissing(answer []byte, model string, codes []string) bool {
	var top struct {
		Error   json.RawMessage `json:"error"`
		Message string          `json:"message"`
		Code    json.RawMessage `json:"code"`
	}
	if json.Unmarshal(answer, &top) != nil {
		return false
	}
	message, code := top.Message, top.Code
	var nested struct {
		Message string          `json:"message"`
		Code    json.RawMessage `json:"code"`
	}
	switch {
	case json.Unmarshal(top.Error, &message) == nil:
	case json.Unmarshal(top.Error, &nested) == nil:
		message, code = nested.Message, nested.Code
	}
	var codeName string
	if json.Unmarshal(code, &codeName) == nil && slices.Contains(codes, codeName) {
		return true
	}
	return namesWord(message, model)
}

// errorAnswer is what the core reads of an error answer in the OpenAI format.
type errorAnswer struct {
	Type    string
	Message string
}

// readErrorAnswer reads an answer in the OpenAI error shape: {"error": {...}} (its
// type and message, where they are strings) or {"error": "<text>"} (the text as its
// message). ok is false for any other body — plain text, HTML, other JSON.
func readErrorAnswer(answer []byte) (e errorAnswer, ok bool) {
	var top struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(answer, &top) != nil {
		return errorAnswer{}, false
	}
	if json.Unmarshal(top.Error, &e.Message) == nil {
		return e, true
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(top.Error, &fields) != nil || fields == nil {
		return errorAnswer{}, false
	}
	_ = json.Unmarshal(fields["type"], &e.Type) // a type that is not a string reads as none
	_ = json.Unmarshal(fields["message"], &e.Message)
	return e, true
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

// editError is the error for a client body the core cannot edit: a gateway fault,
// since the inbound stage validated the body.
func editError(err error) error { return fmt.Errorf("edit request body: %w", err) }

// wireHeaders sets the headers every upstream request carries beside the module's
// credential. Nothing from the client's headers is forwarded except the request ID,
// so the client's Authorization (its kaiak key), cookies and anything else never reach
// a backend.
func wireHeaders(h http.Header, req *Request) {
	h.Set("Content-Type", "application/json")
	if req.Stream {
		h.Set("Accept", "text/event-stream")
	} else {
		h.Set("Accept", "application/json")
	}
	h.Set("User-Agent", "kaiak")
	h.Set("X-Request-Id", req.RequestID)
}

// passthroughBody applies the gateway's owned edits to the client's body: the model
// name becomes the deployment's, the module's own edits (extra) and the pipeline's
// parameters are set, a Responses request gets store: false (the gateway serves
// Responses stateless and lets no backend keep a conversation: docs/specs/GATEWAY.md,
// Client API → Responses is stateless), and an OpenAI-format stream gets
// stream_options.include_usage so the backend reports usage — Messages and Responses
// always report it. stripUsage is true when the client did not ask for usage, so the
// usage-only chunk that edit adds must not reach it.
func passthroughBody(req *Request, extra ...memberEdit) (body []byte, stripUsage bool, err error) {
	model, _ := json.Marshal(req.Deployment.Model) // a string always encodes
	edits := append([]memberEdit{setValue("model", model)}, extra...)
	for _, p := range req.Params {
		edits = append(edits, setValue(p.Key, p.Value))
	}
	if req.Endpoint == Responses {
		edits = append(edits, setValue("store", []byte("false")))
	}
	if req.Stream && !req.IncludeUsage && req.Endpoint.Format() == FormatOpenAI {
		edits = append(edits, memberEdit{key: "stream_options", set: setIncludeUsage})
		stripUsage = true
	}
	body, err = editObject(req.Body, edits...)
	return body, stripUsage, err
}

// standardServiceTier is the edit that keeps a request on standard processing, for
// the modules whose backends bill by tier (openai.go, azure_openai.go;
// docs/specs/GATEWAY.md, Providers → Service tier): prices are standard-tier rates,
// and a priority request is billed about twice what its record would say. A chat
// completions or Responses request always carries service_tier "default" — an absent
// tier means "auto", which follows the deployment's or project's own setting. On
// completions and embeddings a client's tier becomes "default" and an absent one
// stays absent: OpenAI refuses parameters an endpoint does not define. Responses
// token counting is left as the client sent it: nothing is generated or billed there.
func standardServiceTier(e Endpoint) memberEdit {
	return memberEdit{key: "service_tier", set: func(current []byte) ([]byte, error) {
		switch {
		case e == ResponsesInputTokens:
			return current, nil
		case current == nil && e != ChatCompletions && e != Responses:
			return nil, nil
		}
		return []byte(`"default"`), nil
	}}
}

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
	// ending reads a stream's events for its end (docs/specs/GATEWAY.md, Providers:
	// complete responses); errorEvent: a successful stream carried an error event,
	// relayed as the backend sent it, after which the stream ends incomplete.
	ending     streamEnd
	errorEvent bool
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
		r.ending = newStreamEnd(format)
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
// pinging while it serves nothing has stalled). After an error event nothing more is
// read: the stream ends there, incomplete.
func (r *upstreamResponse) read() (Event, error) {
	if r.errorEvent {
		return Event{}, fmt.Errorf("backend %s: the stream carried an error event: %w", r.backendID, ErrIncomplete)
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
			return Event{}, false, readFailure(err)
		}
		ev = Event{Data: block.Raw}
		if block.HasData {
			ev.Payload = block.Data
			r.errorEvent = r.succeeded() && r.ending.observe(block.Data)
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
				r.pending = readFailure(err)
				return Event{Data: piece}, true, nil
			}
		}
		if err != nil {
			return Event{}, false, readFailure(err)
		}
	}
}

// readFailure is a body read's error as the gateway words it: the end of the body
// and the event stream's own failures (ended inside an event, a block past the size
// limit) as they are; any other — the connection's — as its class (netfail). Go
// builds some of those from the bytes the backend sent (a trailer line it cannot
// parse is quoted whole), and the error reaches log lines (docs/specs/GATEWAY.md,
// Logs: no remote text).
func readFailure(err error) error {
	if err == nil || err == io.EOF || errors.Is(err, sse.ErrTruncated) || errors.Is(err, sse.ErrTooLarge) {
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
// stream is complete as its format's events say (streamEnd); a successful JSON body
// once its top-level value closed. Other answers are complete when they end.
func (r *upstreamResponse) complete() bool {
	if !r.succeeded() {
		return true
	}
	if r.stream {
		return r.ending.complete()
	}
	if r.bodyModel != nil {
		return r.bodyModel.closed()
	}
	return true
}

// rewriteChunkModel returns the block's raw bytes with the chunk's model replaced by
// the public name: the top-level model, and the one a format carries one level down
// (streamEnd.nestedModel). The chunk's JSON is spread over the block's data line
// values (joined by line breaks, which JSON reads as whitespace), so each value is
// edited in place and everything around it is kept.
func (r *upstreamResponse) rewriteChunkModel(block sse.Block) []byte {
	m := newNestedModelRewriter(r.publicModel, r.ending.nestedModel())
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
