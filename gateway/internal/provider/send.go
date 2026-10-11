package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"time"

	"kaiak/internal/config"
	"kaiak/internal/netfail"
)

// The wire core: the request, response and error handling every backend module shares
// (openai.go, azure_openai.go, vllm.go, vllm_omni.go, llama_server.go,
// openai_compatible.go, anthropic.go, azure_anthropic.go). The core is not a provider
// and knows no backend type: a module prepares the upstream request — URL, credential
// (bearer, apiKey or its own header), its own body edits, the error codes that mean a
// missing model, its server's 404 to a path it does not have, its server's answers of
// other statuses that say the same or the model missing (answerReading) — and hands it
// to sendWire, which applies the edits every request gets (passthroughBody); its probe
// reads the models list with fetchModelsList.

// wireCall is one upstream request a backend module prepared.
type wireCall struct {
	backend *config.Backend
	client  *http.Client
	url     string
	// header holds the module's credential; sendWire adds the headers every request
	// carries (wireHeaders).
	header http.Header
	// edits are the module's own edits of the client's body, applied with the ones
	// every request gets (passthroughBody).
	edits []memberEdit
	// missingModel reports whether a 404 answer (its first maxPeekedAnswer bytes) says
	// the deployment's backend-side model does not exist there.
	missingModel func(answer []byte, model string) bool
	// unknownPath reports whether a 404 answer (its first maxPeekedAnswer bytes) is the
	// server's answer to a path it does not have: the request reached the server but
	// no endpoint.
	unknownPath func(answer []byte) bool
	// readings are the module's readings of the endpoint's answers of statuses beyond
	// the ones read for every module (401, 403, 404), one per status: its server's own
	// words for the deployment's model missing or a path it does not have. An answer
	// of a status none reads is relayed, or answered as the backend's 5xx, as it came.
	readings []answerReading
	// relayed are the response headers the module relays to the client for its
	// server's answer, beside the ones every answer relays (clientHeaders).
	relayed []string
}

// answerReading reads an answer of status — its first maxPeekedAnswer bytes — as code
// (CodeModelMissing or CodePathMissing) when read says so; model is the deployment's
// backend-side model. Any other answer of status is relayed, or answered as the
// backend's 5xx, as it came.
type answerReading struct {
	status int
	code   Code
	read   func(answer []byte, model string) bool
}

// sendWire sends call upstream for req and waits for the first event of the response,
// as Provider.Send says. Besides *Error and the context's error, it returns a plain
// error when the client's body cannot be edited or the upstream request cannot be
// built — a gateway fault.
func sendWire(ctx context.Context, req *Request, call wireCall) (Response, error) {
	body, stripUsage, err := passthroughBody(req, call.edits...)
	if err != nil {
		return nil, editError(err)
	}
	// The edited body is dropped once sendWire returns: the first event is in, so the
	// transport will not send it again. req is not captured by anything that outlives
	// the send (the trace below lives as long as the response), so the client's body is
	// released with the pipeline's copy.
	upstreamBody := newUpstreamBody(body)
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
	// release ends the request when sendWire returns no response.
	release := func() {
		timer.Stop()
		cancel(nil)
	}
	// fail classifies an error seen before the first event and releases the request.
	fail := func(err error) error {
		release()
		cause := context.Cause(upstreamCtx) // a timer's cause, set before release, stays
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
		release()
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
	if failure := deploymentFailure(resp, req, call); failure != nil {
		_ = resp.Body.Close() // the backend's answer is about its deployment: not relayed
		release()
		return nil, failure
	}

	publicModel, _ := json.Marshal(req.PublicModel) // a string always encodes
	r := newUpstreamResponse(upstreamCtx, cancel, resp, req.Endpoint.Format(), call.relayed, stripUsage, publicModel)
	r.backendID = call.backend.ID
	preamble, first, err := r.readFirst()
	if err == nil && r.errorEvent != nil {
		// The backend gave up before anything reached the client: the attempt loop
		// answers it as the HTTP status its kind matches would be (docs/specs/GATEWAY.md,
		// Providers: error events).
		timer.Stop()
		r.Close()
		return nil, &Error{Code: CodeErrorEvent, Event: r.errorEvent,
			Err: fmt.Errorf("backend %s: its stream began with an error event", call.backend.ID)}
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
	r.peeked = preamble
	if err != nil {
		r.pending = err
	} else {
		r.peeked = append(r.peeked, first)
	}
	return r, nil
}

// deploymentFailure is the error for an answer that is not the request's to relay —
// its credential refused (401, 403), its model missing, a path the server does not
// have (a 404, or an answer of another status a module reads: readings) — or nil for
// an answer to relay. The start of a 404 or of an answer of a status the module reads
// is read for it, and put back (peekAnswer).
func deploymentFailure(resp *http.Response, req *Request, call wireCall) *Error {
	status := resp.StatusCode
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return &Error{Code: CodeAuthFailed, Err: fmt.Errorf("backend %s answered %d", call.backend.ID, status)}
	}
	for _, r := range call.readings {
		if r.status != status {
			continue
		}
		if answer, ok := peekAnswer(resp); !ok || !r.read(answer, req.Deployment.Model) {
			return nil
		}
		if r.code == CodeModelMissing {
			return modelMissing(call, req, status)
		}
		return pathMissing(call, status)
	}
	if status != http.StatusNotFound {
		return nil
	}
	// A missing model is read first: a server may answer it in the same shape as an
	// unknown path.
	switch answer, ok := peekAnswer(resp); {
	case ok && call.missingModel(answer, req.Deployment.Model):
		return modelMissing(call, req, status)
	case ok && call.unknownPath(answer):
		return pathMissing(call, status)
	}
	return nil
}

// modelMissing is the error for an answer of status saying the deployment's
// backend-side model does not exist there. It never carries the backend's text, which
// names the server's own model.
func modelMissing(call wireCall, req *Request, status int) *Error {
	return &Error{Code: CodeModelMissing, Err: fmt.Errorf("backend %s answered %d: model %q does not exist there",
		call.backend.ID, status, req.Deployment.Model)}
}

// pathMissing is the error for an answer of status that is the server's answer to a
// path it does not have. It names the URL — the backend's base_url is wrong, or its
// server does not serve the endpoint the model lists — never the backend's text.
func pathMissing(call wireCall, status int) *Error {
	return &Error{Code: CodePathMissing, Err: fmt.Errorf(
		"backend %s answered %d: no endpoint at %s (check its base_url, and that its server serves the endpoint for the model)",
		call.backend.ID, status, call.url)}
}

// maxPreambleBlocks bounds the comment and keep-alive blocks read ahead of a stream's
// first data event; past it the next block is taken as the first event.
const maxPreambleBlocks = 64

// readFirst reads the response's first event: for a stream, its first data event —
// comment and keep-alive blocks before it (up to maxPreambleBlocks) are returned
// apart, to be relayed ahead of it — so the first-event window (its timer, the retry
// of an error event, the headers held back) lasts until the backend has said
// something (docs/specs/GATEWAY.md, Providers: complete responses).
func (r *upstreamResponse) readFirst() (preamble []Event, first Event, err error) {
	for {
		first, err = r.read()
		if err != nil || !r.stream || first.Payload != nil || len(preamble) == maxPreambleBlocks {
			return preamble, first, err
		}
		preamble = append(preamble, first)
	}
}

// editError is the error for a client body the core cannot edit: a gateway fault,
// since the inbound stage validated the body.
func editError(err error) error { return fmt.Errorf("edit request body: %w", err) }

// bearer is a module's credential header as a bearer token; none for no credential
// (a backend without one, or one the registry withholds: Registry.credential).
func bearer(credential string) http.Header {
	h := make(http.Header)
	if credential != "" {
		h.Set("Authorization", "Bearer "+credential)
	}
	return h
}

// apiKey is a module's credential header as Azure's api-key header; none for no
// credential.
func apiKey(credential string) http.Header {
	h := make(http.Header)
	if credential != "" {
		h.Set("Api-Key", credential)
	}
	return h
}

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
