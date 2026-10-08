// Package fakebackend is a model server for tests. It serves chat completions,
// completions, embeddings, Anthropic Messages and OpenAI Responses with their token
// counting, and the models list (the gateway's probe) under /v1/ (the OpenAI and Anthropic layout),
// /openai/v1/ (Azure OpenAI's) and /anthropic/v1/ (Claude in Foundry's), and rerank
// under /v1/ alone (vLLM's and llama-server's: neither Azure API has one), in vLLM's
// answer shape or llama-server's (SetRerankShape); it answers as scripted by the test —
// a path it has no route for too (SetNoRoute) — and records every request it receives.
// Test tooling only: nothing in the gateway binary imports it; cmd/fakebackend runs it
// as a process for the e2e test and the live-test kit.
package fakebackend

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"time"
)

// Reply scripts how the backend answers. The zero Reply is a normal, successful
// answer: streamed when the request asks for a stream, with usage reported the way
// OpenAI reports it.
type Reply struct {
	// Status, when set and not 200, makes the backend answer with that status and
	// Body (default: an OpenAI-shaped error) instead of a completion. Body alone is
	// answered with 200 (e.g. a JSON body cut short).
	Status int
	Body   string
	// Header adds response headers.
	Header map[string]string
	// RequireHeader, when set, makes the backend answer 401 to a request whose
	// RequireHeader header is not exactly RequireValue (a backend credential check).
	RequireHeader string
	RequireValue  string

	// Chunks are the pieces of generated text, one per stream event; the non-stream
	// answer joins them. Default: DefaultChunks.
	Chunks []string
	// Usage overrides the reported token counts. Default: 7 prompt tokens, one
	// completion token per chunk.
	Usage *Usage
	// CachedTokens, when Usage is not set, reports that many of the default 7 prompt
	// tokens as read from the cache (a prefix cache answering a repeated prompt).
	CachedTokens int
	// OmitUsage: never report usage — no usage field in a non-stream answer, no
	// usage chunk in a stream even when the request asks for one.
	OmitUsage bool

	// Before, when set, is a fault before the answer's body (see BeforeFault).
	Before BeforeFault
	// Pace, when set, makes a stream wait for a value from it before each event
	// after the first, so a test controls when each event is sent.
	Pace <-chan struct{}
	// PingFirst makes a stream open with a ": ping" comment block before its first
	// data event (a backend keeping the connection alive before its first token);
	// with Pace, the first data event then waits for a Pace value too.
	PingFirst bool
	// Fault, when set, breaks a stream (see StreamFault).
	Fault *StreamFault
	// EventDelay, when set, makes a stream wait that long before each event after
	// the first (a slow model).
	EventDelay time.Duration
	// HonorMaxTokens: generate at most the request's max_completion_tokens,
	// max_tokens or max_output_tokens chunks (the smallest when several are set), one
	// token each, and finish with "length" ("max_tokens" in Messages, an incomplete
	// response in Responses) when that cut the answer short — as a real model server
	// does.
	HonorMaxTokens bool
}

// BeforeFault is a fault before the answer's body; the zero value is none.
type BeforeFault int

const (
	// StallFirstByte: send nothing and wait until the request is cancelled.
	StallFirstByte BeforeFault = iota + 1
	// CutBody: answer Status (default 200) with its headers and a JSON
	// Content-Length whose body never comes — the connection drops before the first
	// body byte.
	CutBody
	// StallBody: answer Status (default 200) and its headers, then wait until the
	// request is cancelled.
	StallBody
)

// StreamFault breaks a stream after At text events. At 0 the fault comes before the
// stream's first event — before a Messages or Responses stream's opening events too.
// A stream with fewer than At text events is not broken.
type StreamFault struct {
	At   int
	Kind StreamFaultKind
	// Code, with ErrorEvent, is the error event's Anthropic error type (Messages) or
	// code (Responses) in place of overloaded_error and server_error.
	Code string
	// PingEvery, with Hang, makes the hanging stream send a ": ping" comment block
	// that often while it waits (a keep-alive with no data).
	PingEvery time.Duration
}

// StreamFaultKind is what a StreamFault does.
type StreamFaultKind int

const (
	// Hang: wait until the request is cancelled.
	Hang StreamFaultKind = iota + 1
	// Cut: drop the connection.
	Cut
	// End: end the response cleanly — no finish_reason, no usage, no [DONE] (a
	// generator that died); a Messages stream ends without message_delta and
	// message_stop, a Responses stream without response.completed.
	End
	// ErrorEvent: send the API's error event (Anthropic's overloaded_error, a
	// Responses error) and end there. Messages and Responses streams only.
	ErrorEvent
)

// Usage is a token report. CachedTokens, CacheWriteTokens and ReasoningTokens, when
// set, are reported in prompt_tokens_details and completion_tokens_details, as OpenAI
// and Azure OpenAI do. A Messages answer reports them as Anthropic does: input_tokens
// is PromptTokens less the cached and written tokens, which are
// cache_read_input_tokens and cache_creation_input_tokens; reasoning is not reported
// apart. A Responses answer reports PromptTokens as input_tokens, with the cached and
// written tokens in input_tokens_details and reasoning in output_tokens_details.
type Usage struct {
	PromptTokens     int
	CompletionTokens int
	CachedTokens     int
	CacheWriteTokens int
	ReasoningTokens  int
}

// DefaultChunks is the generated text when a Reply sets no Chunks.
var DefaultChunks = []string{"Hello", " from", " the", " fake"}

// Request is one request the backend received.
type Request struct {
	Method string
	// Path is the URL path, e.g. "/v1/chat/completions".
	Path   string
	Header http.Header
	Body   []byte
	// canceled is closed when the backend saw the request cancelled while it was
	// waiting (stall, hang or pace).
	canceled chan struct{}
}

// Canceled is closed once the backend observed the request's cancellation (the
// gateway closed the connection or cancelled the request) while waiting.
func (r *Request) Canceled() <-chan struct{} { return r.canceled }

// Backend is a running fake backend.
type Backend struct {
	server *httptest.Server
	// done is closed by Close, releasing handlers that wait.
	done chan struct{}

	mu       sync.Mutex
	reply    Reply
	queued   []Reply
	received []*Request
	arrivals chan *Request
	// modelsStatus is the models list's answer status (0 = 200); models are the
	// model IDs it lists (nil: DefaultModels); modelsRequests are the requests for
	// it, kept apart from received.
	modelsStatus   int
	models         []string
	modelsRequests []*Request
	// rerankShape is the server whose rerank answer the backend sends.
	rerankShape RerankShape
	// noRoute is the body of the 404 answering a path the backend has no route for
	// ("": an OpenAI-shaped error); unrouted are the endpoints it has no route for.
	noRoute  string
	unrouted []string
}

// maxQueuedArrivals bounds requests queued for NextRequest; a test that never reads
// them still gets every request recorded in Requests.
const maxQueuedArrivals = 256

// New starts a fake backend on a loopback port. Close it when done.
func New() *Backend {
	b := &Backend{done: make(chan struct{}), arrivals: make(chan *Request, maxQueuedArrivals)}
	b.server = httptest.NewServer(http.HandlerFunc(b.serve))
	return b
}

// NewAt starts a fake backend listening on addr (host:port; port 0 picks a free
// one). Close it when done.
func NewAt(addr string) (*Backend, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	b := &Backend{done: make(chan struct{}), arrivals: make(chan *Request, maxQueuedArrivals)}
	b.server = httptest.NewUnstartedServer(http.HandlerFunc(b.serve))
	_ = b.server.Listener.Close() // the loopback listener httptest opened, replaced by ln
	b.server.Listener = ln
	b.server.Start()
	return b, nil
}

// URL is the backend's root URL (no path): an azure-openai or azure-anthropic
// base_url is URL(); every other type's is URL() + "/v1".
func (b *Backend) URL() string { return b.server.URL }

// Close stops the backend, cancelling any request still running.
func (b *Backend) Close() {
	close(b.done)
	b.server.CloseClientConnections()
	b.server.Close()
}

// SetReply scripts the answer to every request from now on.
func (b *Backend) SetReply(r Reply) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.reply = r
}

// QueueReplies scripts the answers to the next len(rs) requests, in order; the
// requests after them get SetReply's answer.
func (b *Backend) QueueReplies(rs ...Reply) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.queued = append(b.queued, rs...)
}

// DefaultModels is what the models list lists until SetModels says otherwise.
var DefaultModels = []string{"fake"}

// SetModels scripts the model IDs the models list lists from now on (nil:
// DefaultModels). The completion endpoints serve any model name regardless.
func (b *Backend) SetModels(ids ...string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.models = ids
}

// SetModelsStatus scripts the models list's answer from now on: 200 (or 0) lists
// the models (SetModels); any other status answers an OpenAI-shaped error.
func (b *Backend) SetModelsStatus(status int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.modelsStatus = status
}

// SetRerankShape scripts which server's rerank answer the backend sends from now on
// (the zero value: vLLM's).
func (b *Backend) SetRerankShape(shape RerankShape) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rerankShape = shape
}

// SetNoRoute scripts, from now on, how the backend answers a path it has no route
// for — a 404 with body, as its server's web framework answers one (vLLM's FastAPI:
// {"detail":"Not Found"}; "" for the default, an OpenAI-shaped error) — and the
// endpoints it has no route for (paths below the version prefix, as
// "chat/completions"), as a vLLM server serving a reranker has no chat route.
func (b *Backend) SetNoRoute(body string, endpoints ...string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.noRoute, b.unrouted = body, endpoints
}

// ModelsRequests returns every request for the models list so far, in arrival
// order. They are not in Requests or Arrivals.
func (b *Backend) ModelsRequests() []*Request {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]*Request(nil), b.modelsRequests...)
}

// Requests returns every request received so far, in arrival order.
func (b *Backend) Requests() []*Request {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]*Request(nil), b.received...)
}

// Arrivals delivers each request as it arrives, before the backend answers it (up to
// maxQueuedArrivals unread).
func (b *Backend) Arrivals() <-chan *Request { return b.arrivals }

// Endpoints the backend serves, as paths below the version prefix.
const (
	chatPath        = "chat/completions"
	completionPath  = "completions"
	embeddingPath   = "embeddings"
	messagesPath    = "messages"
	countTokensPath = "messages/count_tokens"
	responsesPath   = "responses"
	inputTokensPath = "responses/input_tokens"
	rerankPath      = "rerank"
	modelsPath      = "models"
)

// layouts are the version prefixes the backend serves its endpoints under: OpenAI's
// and Anthropic's, Azure OpenAI's, Claude in Foundry's.
var layouts = []string{"/v1/", "/openai/v1/", "/anthropic/v1/"}

// servedEndpoint is the endpoint a request's path names below one of the layouts; ok
// is false for any other path.
func servedEndpoint(path string) (endpoint string, ok bool) {
	for _, prefix := range layouts {
		if endpoint, ok = strings.CutPrefix(path, prefix); ok {
			return endpoint, true
		}
	}
	return "", false
}

// serveModels answers the models list as scripted by SetModelsStatus.
func (b *Backend) serveModels(w http.ResponseWriter, req *Request) {
	b.mu.Lock()
	b.modelsRequests = append(b.modelsRequests, req)
	status := b.modelsStatus
	ids := b.models
	b.mu.Unlock()
	if status != 0 && status != http.StatusOK {
		writeJSON(w, status, errorBody(fmt.Sprintf("scripted failure %d", status)))
		return
	}
	if ids == nil {
		ids = DefaultModels
	}
	data := []any{}
	for _, id := range ids {
		data = append(data, map[string]any{"id": id, "object": "model", "created": created, "owned_by": "fake"})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

func (b *Backend) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	req := &Request{Method: r.Method, Path: r.URL.Path, Header: r.Header.Clone(), Body: body, canceled: make(chan struct{})}
	endpoint, ok := servedEndpoint(r.URL.Path)
	if ok && endpoint == modelsPath && r.Method == http.MethodGet {
		b.serveModels(w, req)
		return
	}

	b.mu.Lock()
	b.received = append(b.received, req)
	reply := b.reply
	rerankShape := b.rerankShape
	noRoute, unrouted := b.noRoute, b.unrouted
	if len(b.queued) > 0 {
		reply = b.queued[0]
		b.queued = b.queued[1:]
	}
	b.mu.Unlock()
	select {
	case b.arrivals <- req:
	default:
	}
	switch endpoint {
	case chatPath, completionPath, embeddingPath, messagesPath, countTokensPath, responsesPath, inputTokensPath:
	case rerankPath:
		ok = strings.HasPrefix(r.URL.Path, "/v1/")
	default:
		ok = false
	}
	if !ok || r.Method != http.MethodPost || slices.Contains(unrouted, endpoint) {
		if noRoute == "" {
			writeJSON(w, http.StatusNotFound, errorBody("unknown path "+r.URL.Path))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, noRoute)
		return
	}
	// A Messages endpoint answers its errors in Anthropic's shape.
	errorAnswer := errorBody
	if endpoint == messagesPath || endpoint == countTokensPath {
		errorAnswer = messagesErrorBody
	}

	if reply.RequireHeader != "" && r.Header.Get(reply.RequireHeader) != reply.RequireValue {
		writeJSON(w, http.StatusUnauthorized, errorAnswer("missing or wrong "+reply.RequireHeader+" header"))
		return
	}
	for name, value := range reply.Header {
		w.Header().Set(name, value)
	}
	if reply.Before == StallFirstByte {
		b.waitForCancel(r, req)
		return
	}
	if reply.Before == CutBody || reply.Before == StallBody {
		status := reply.Status
		if status == 0 {
			status = http.StatusOK
		}
		w.Header().Set("Content-Type", "application/json")
		if reply.Before == CutBody {
			// Nothing of it is written, so net/http drops the connection.
			w.Header().Set("Content-Length", "100")
		}
		w.WriteHeader(status)
		_ = http.NewResponseController(w).Flush()
		if reply.Before == StallBody {
			b.waitForCancel(r, req)
		}
		return
	}
	if (reply.Status != 0 && reply.Status != http.StatusOK) || reply.Body != "" {
		status := reply.Status
		if status == 0 {
			status = http.StatusOK
		}
		if reply.Body == "" {
			writeJSON(w, status, errorAnswer(fmt.Sprintf("scripted failure %d", status)))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, reply.Body)
		return
	}

	var params struct {
		model        string
		stream       bool
		includeUsage bool
		maxTokens    *int
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		writeJSON(w, http.StatusBadRequest, errorAnswer("body is not a JSON object"))
		return
	}
	_ = json.Unmarshal(top["model"], &params.model)
	_ = json.Unmarshal(top["stream"], &params.stream)
	var options map[string]json.RawMessage
	if json.Unmarshal(top["stream_options"], &options) == nil {
		_ = json.Unmarshal(options["include_usage"], &params.includeUsage)
	}
	for _, name := range []string{"max_completion_tokens", "max_tokens", "max_output_tokens"} {
		var n *int
		if json.Unmarshal(top[name], &n) == nil && n != nil && (params.maxTokens == nil || *n < *params.maxTokens) {
			params.maxTokens = n
		}
	}

	chunks := reply.Chunks
	if chunks == nil {
		chunks = DefaultChunks
	}
	finish := "stop"
	if reply.HonorMaxTokens && params.maxTokens != nil && *params.maxTokens >= 0 && *params.maxTokens < len(chunks) {
		chunks = chunks[:*params.maxTokens]
		finish = "length"
	}
	usage := Usage{PromptTokens: 7, CompletionTokens: len(chunks), CachedTokens: reply.CachedTokens}
	if reply.Usage != nil {
		usage = *reply.Usage
	}

	switch {
	case endpoint == countTokensPath:
		writeJSON(w, http.StatusOK, map[string]any{"input_tokens": usage.PromptTokens})
	case endpoint == inputTokensPath:
		writeJSON(w, http.StatusOK, map[string]any{"object": "response.input_tokens", "input_tokens": usage.PromptTokens})
	case endpoint == responsesPath && params.stream:
		b.writeResponsesStream(w, r, req, reply, top, params.model, chunks, finish == "length", usage)
	case endpoint == responsesPath:
		writeResponse(w, top, params.model, strings.Join(chunks, ""), finish == "length", usage, reply.OmitUsage)
	case endpoint == messagesPath && params.stream:
		b.writeMessagesStream(w, r, req, reply, params.model, chunks, finish == "length", usage)
	case endpoint == messagesPath:
		writeMessage(w, params.model, strings.Join(chunks, ""), finish == "length", usage, reply.OmitUsage)
	case endpoint == embeddingPath:
		b.writeEmbeddings(w, params.model, usage, reply.OmitUsage)
	case endpoint == rerankPath:
		// Neither server reads stream on rerank: the answer is a JSON body either way.
		writeRerank(w, rerankShape, top, params.model, usage, reply.OmitUsage)
	case params.stream:
		b.writeStream(w, r, req, reply, endpoint, params.model, chunks, finish, usage, params.includeUsage && !reply.OmitUsage)
	default:
		writeCompletion(w, endpoint, params.model, strings.Join(chunks, ""), finish, usage, reply.OmitUsage)
	}
}

// waitForCancel blocks until the request is cancelled or the backend closes, and
// records a cancellation.
func (b *Backend) waitForCancel(r *http.Request, req *Request) {
	select {
	case <-r.Context().Done():
		close(req.canceled)
	case <-b.done:
	}
}

// pingUntilCancel writes a ": ping" comment block every interval until the request
// is cancelled or the backend closes, and records a cancellation.
func (b *Backend) pingUntilCancel(w http.ResponseWriter, r *http.Request, req *Request, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	flusher := http.NewResponseController(w)
	for {
		select {
		case <-ticker.C:
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil || flusher.Flush() != nil {
				b.waitForCancel(r, req)
				return
			}
		case <-r.Context().Done():
			close(req.canceled)
			return
		case <-b.done:
			return
		}
	}
}

// streamWriter writes one stream's events as a Reply paces them.
type streamWriter struct {
	b       *Backend
	w       http.ResponseWriter
	r       *http.Request
	req     *Request
	reply   Reply
	flusher *http.ResponseController
	sent    int
	// errorEvent is the API's error event carrying code ("": the API's default); nil
	// for an API whose streams have none.
	errorEvent func(code string) []byte
}

// startStream answers 200 with an event stream, opening with a ": ping" comment
// block when the reply asks; ok is false when the client is gone. errorEvent is the
// API's error event (nil: none).
func (b *Backend) startStream(w http.ResponseWriter, r *http.Request, req *Request, reply Reply,
	errorEvent func(code string) []byte) (*streamWriter, bool) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	s := &streamWriter{b: b, w: w, r: r, req: req, reply: reply, flusher: http.NewResponseController(w),
		errorEvent: errorEvent}
	if reply.PingFirst {
		if _, err := io.WriteString(w, ": ping\n\n"); err != nil || s.flusher.Flush() != nil {
			return nil, false
		}
	}
	return s, true
}

// send writes one event — an "event:" line when name is set, then the data — after
// the pace and delay the reply asks for; false when the stream must stop.
func (s *streamWriter) send(name string, payload []byte) bool {
	reply, r, req := s.reply, s.r, s.req
	if (s.sent > 0 || reply.PingFirst) && reply.Pace != nil {
		select {
		case <-reply.Pace:
		case <-r.Context().Done():
			close(req.canceled)
			return false
		case <-s.b.done:
			return false
		}
	}
	if s.sent > 0 && reply.EventDelay > 0 {
		timer := time.NewTimer(reply.EventDelay)
		select {
		case <-timer.C:
		case <-r.Context().Done():
			timer.Stop()
			close(req.canceled)
			return false
		case <-s.b.done:
			timer.Stop()
			return false
		}
	}
	s.sent++
	if name != "" {
		if _, err := fmt.Fprintf(s.w, "event: %s\n", name); err != nil {
			return false
		}
	}
	if _, err := fmt.Fprintf(s.w, "data: %s\n\n", payload); err != nil {
		return false
	}
	return s.flusher.Flush() == nil
}

// interrupt applies the reply's stream fault when i text events have been sent: it
// hangs, cuts the connection, ends the stream cleanly or sends the error event. It
// returns false when the stream ends here. A writer calls it with 0 before its first
// event and with i before text event i; every kind ends the stream, so a fault fires
// once.
func (s *streamWriter) interrupt(i int) bool {
	fault := s.reply.Fault
	if fault == nil || i != fault.At {
		return true
	}
	switch fault.Kind {
	case Hang:
		if fault.PingEvery > 0 {
			s.b.pingUntilCancel(s.w, s.r, s.req, fault.PingEvery)
			return false
		}
		s.b.waitForCancel(s.r, s.req)
	case Cut:
		panic(http.ErrAbortHandler)
	case End:
	case ErrorEvent:
		if s.errorEvent == nil {
			panic("fakebackend: an ErrorEvent fault on a stream with no error event")
		}
		s.send("error", s.errorEvent(fault.Code))
	default:
		panic(fmt.Sprintf("fakebackend: StreamFault with unknown Kind %d", fault.Kind))
	}
	return false
}

func (b *Backend) writeStream(w http.ResponseWriter, r *http.Request, req *Request, reply Reply,
	endpoint, model string, chunks []string, finish string, usage Usage, includeUsage bool) {
	s, ok := b.startStream(w, r, req, reply, nil)
	if !ok {
		return
	}
	send := func(payload []byte) bool { return s.send("", payload) }
	for i, text := range chunks {
		if !s.interrupt(i) {
			return
		}
		if !send(textChunk(endpoint, model, text, i == 0, nil, includeUsage)) {
			return
		}
	}
	if !send(textChunk(endpoint, model, "", len(chunks) == 0, &finish, includeUsage)) {
		return
	}
	if includeUsage {
		payload, _ := json.Marshal(map[string]any{
			"id": "fake-1", "object": chunkObject(endpoint), "created": created, "model": model,
			"choices": []any{}, "usage": usageBody(usage, endpoint),
		})
		if !send(payload) {
			return
		}
	}
	send([]byte("[DONE]"))
}

// created is the fixed creation time on every answer.
const created = 1700000000

func chunkObject(endpoint string) string {
	if endpoint == chatPath {
		return "chat.completion.chunk"
	}
	return "text_completion"
}

// textChunk builds one stream chunk carrying text (or the finish reason). Field order
// follows OpenAI's; like OpenAI, every chunk carries "usage": null when the request
// asks for usage (withUsage).
func textChunk(endpoint, model, text string, first bool, finish *string, withUsage bool) []byte {
	var choice any
	if endpoint == chatPath {
		delta := map[string]string{}
		if first {
			delta["role"] = "assistant"
		}
		if finish == nil {
			delta["content"] = text
		}
		choice = struct {
			Index        int               `json:"index"`
			Delta        map[string]string `json:"delta"`
			FinishReason *string           `json:"finish_reason"`
		}{0, delta, finish}
	} else {
		choice = struct {
			Index        int     `json:"index"`
			Text         string  `json:"text"`
			FinishReason *string `json:"finish_reason"`
		}{0, text, finish}
	}
	chunk := struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int    `json:"created"`
		Model   string `json:"model"`
		Choices []any  `json:"choices"`
	}{"fake-1", chunkObject(endpoint), created, model, []any{choice}}
	data, _ := json.Marshal(chunk)
	if withUsage {
		data = append(data[:len(data)-1], []byte(`,"usage":null}`)...)
	}
	return data
}

func usageBody(u Usage, endpoint string) map[string]any {
	if endpoint == embeddingPath || endpoint == rerankPath {
		return map[string]any{"prompt_tokens": u.PromptTokens, "total_tokens": u.PromptTokens}
	}
	body := map[string]any{"prompt_tokens": u.PromptTokens, "completion_tokens": u.CompletionTokens,
		"total_tokens": u.PromptTokens + u.CompletionTokens}
	promptDetails := map[string]int{}
	if u.CachedTokens > 0 {
		promptDetails["cached_tokens"] = u.CachedTokens
	}
	if u.CacheWriteTokens > 0 {
		promptDetails["cache_write_tokens"] = u.CacheWriteTokens
	}
	if len(promptDetails) > 0 {
		body["prompt_tokens_details"] = promptDetails
	}
	if u.ReasoningTokens > 0 {
		body["completion_tokens_details"] = map[string]int{"reasoning_tokens": u.ReasoningTokens}
	}
	return body
}

func writeCompletion(w http.ResponseWriter, endpoint, model, text, finish string, usage Usage, omitUsage bool) {
	answer := map[string]any{"id": "fake-1", "created": created, "model": model}
	if endpoint == chatPath {
		answer["object"] = "chat.completion"
		answer["choices"] = []any{map[string]any{"index": 0, "finish_reason": finish,
			"message": map[string]string{"role": "assistant", "content": text}}}
	} else {
		answer["object"] = "text_completion"
		answer["choices"] = []any{map[string]any{"index": 0, "finish_reason": finish, "text": text}}
	}
	if !omitUsage {
		answer["usage"] = usageBody(usage, endpoint)
	}
	writeJSON(w, http.StatusOK, answer)
}

func (b *Backend) writeEmbeddings(w http.ResponseWriter, model string, usage Usage, omitUsage bool) {
	answer := map[string]any{
		"object": "list", "model": model,
		"data": []any{map[string]any{"object": "embedding", "index": 0, "embedding": []float64{0.25, -0.5, 0.125}}},
	}
	if !omitUsage {
		answer["usage"] = usageBody(usage, embeddingPath)
	}
	writeJSON(w, http.StatusOK, answer)
}

func errorBody(message string) map[string]any {
	return map[string]any{"error": map[string]any{"message": message, "type": "invalid_request_error", "param": nil, "code": nil}}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	var data bytes.Buffer
	_ = json.NewEncoder(&data).Encode(v) // test values always encode
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(data.Bytes())
}
