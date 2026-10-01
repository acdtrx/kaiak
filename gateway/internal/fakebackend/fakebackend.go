// Package fakebackend is an OpenAI-compatible model server for tests. It serves chat
// completions, completions, embeddings and the models list (the gateway's probe)
// under /v1/ (the OpenAI layout) and /openai/v1/ (Azure layout), answers as
// scripted by the test, and records every request it receives. Test tooling only: nothing in the gateway binary imports it;
// cmd/fakebackend runs it as a process for the e2e test and the live-test kit.
package fakebackend

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
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
	// OmitUsage: never report usage — no usage field in a non-stream answer, no
	// usage chunk in a stream even when the request asks for one.
	OmitUsage bool

	// StallBeforeFirstByte: send nothing and wait until the request is cancelled.
	StallBeforeFirstByte bool
	// CutBeforeBody: answer Status (default 200) with its headers and a JSON
	// Content-Length whose body never comes — the connection drops before the first
	// body byte. StallBeforeBody: answer the status and headers, then wait until the
	// request is cancelled.
	CutBeforeBody   bool
	StallBeforeBody bool
	// Pace, when set, makes a stream wait for a value from it before each event
	// after the first, so a test controls when each event is sent.
	Pace <-chan struct{}
	// HangAfter > 0: a stream sends that many text events, then waits until the
	// request is cancelled.
	HangAfter int
	// PingEvery, with HangAfter, makes the hanging stream send a ": ping" comment
	// block that often while it waits (a keep-alive with no data).
	PingEvery time.Duration
	// PingFirst makes a stream open with a ": ping" comment block before its first
	// data event (a backend keeping the connection alive before its first token);
	// with Pace, the first data event then waits for a Pace value too.
	PingFirst bool
	// CutAfter > 0: a stream sends that many text events, then drops the connection.
	CutAfter int
	// EndAfter > 0: a stream sends that many text events, then ends the response
	// cleanly — no finish_reason, no usage, no [DONE] (a generator that died).
	EndAfter int
	// EventDelay, when set, makes a stream wait that long before each event after
	// the first (a slow model).
	EventDelay time.Duration
	// HonorMaxTokens: generate at most the request's max_completion_tokens or
	// max_tokens chunks (the smaller when both are set), one token each, and finish
	// with "length" when that cut the answer short — as a real model server does.
	HonorMaxTokens bool
}

// Usage is a token report. CachedTokens and ReasoningTokens, when set, are reported
// in prompt_tokens_details and completion_tokens_details, as OpenAI does.
type Usage struct {
	PromptTokens     int
	CompletionTokens int
	CachedTokens     int
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

// URL is the backend's root URL (no path): an azure-openai base_url is URL(); every
// other type's is URL() + "/v1".
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
	chatPath       = "chat/completions"
	completionPath = "completions"
	embeddingPath  = "embeddings"
	modelsPath     = "models"
)

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
	endpoint, ok := strings.CutPrefix(r.URL.Path, "/v1/")
	if !ok {
		endpoint, ok = strings.CutPrefix(r.URL.Path, "/openai/v1/")
	}
	if ok && endpoint == modelsPath && r.Method == http.MethodGet {
		b.serveModels(w, req)
		return
	}

	b.mu.Lock()
	b.received = append(b.received, req)
	reply := b.reply
	if len(b.queued) > 0 {
		reply = b.queued[0]
		b.queued = b.queued[1:]
	}
	b.mu.Unlock()
	select {
	case b.arrivals <- req:
	default:
	}
	if !ok || r.Method != http.MethodPost || (endpoint != chatPath && endpoint != completionPath && endpoint != embeddingPath) {
		writeJSON(w, http.StatusNotFound, errorBody("unknown path "+r.URL.Path))
		return
	}

	if reply.RequireHeader != "" && r.Header.Get(reply.RequireHeader) != reply.RequireValue {
		writeJSON(w, http.StatusUnauthorized, errorBody("missing or wrong "+reply.RequireHeader+" header"))
		return
	}
	for name, value := range reply.Header {
		w.Header().Set(name, value)
	}
	if reply.StallBeforeFirstByte {
		b.waitForCancel(r, req)
		return
	}
	if reply.CutBeforeBody || reply.StallBeforeBody {
		status := reply.Status
		if status == 0 {
			status = http.StatusOK
		}
		w.Header().Set("Content-Type", "application/json")
		if reply.CutBeforeBody {
			// Nothing of it is written, so net/http drops the connection.
			w.Header().Set("Content-Length", "100")
		}
		w.WriteHeader(status)
		_ = http.NewResponseController(w).Flush()
		if reply.StallBeforeBody {
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
			writeJSON(w, status, errorBody(fmt.Sprintf("scripted failure %d", status)))
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
		writeJSON(w, http.StatusBadRequest, errorBody("body is not a JSON object"))
		return
	}
	_ = json.Unmarshal(top["model"], &params.model)
	_ = json.Unmarshal(top["stream"], &params.stream)
	var options map[string]json.RawMessage
	if json.Unmarshal(top["stream_options"], &options) == nil {
		_ = json.Unmarshal(options["include_usage"], &params.includeUsage)
	}
	for _, name := range []string{"max_completion_tokens", "max_tokens"} {
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
	usage := Usage{PromptTokens: 7, CompletionTokens: len(chunks)}
	if reply.Usage != nil {
		usage = *reply.Usage
	}

	switch {
	case endpoint == embeddingPath:
		b.writeEmbeddings(w, params.model, usage, reply.OmitUsage)
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

func (b *Backend) writeStream(w http.ResponseWriter, r *http.Request, req *Request, reply Reply,
	endpoint, model string, chunks []string, finish string, usage Usage, includeUsage bool) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flusher := http.NewResponseController(w)
	if reply.PingFirst {
		if _, err := io.WriteString(w, ": ping\n\n"); err != nil || flusher.Flush() != nil {
			return
		}
	}
	sent := 0
	send := func(payload []byte) bool {
		if (sent > 0 || reply.PingFirst) && reply.Pace != nil {
			select {
			case <-reply.Pace:
			case <-r.Context().Done():
				close(req.canceled)
				return false
			case <-b.done:
				return false
			}
		}
		if sent > 0 && reply.EventDelay > 0 {
			timer := time.NewTimer(reply.EventDelay)
			select {
			case <-timer.C:
			case <-r.Context().Done():
				timer.Stop()
				close(req.canceled)
				return false
			case <-b.done:
				timer.Stop()
				return false
			}
		}
		sent++
		if _, err := fmt.Fprintf(w, "data: %s\n\n", payload); err != nil {
			return false
		}
		return flusher.Flush() == nil
	}

	for i, text := range chunks {
		if reply.HangAfter > 0 && i == reply.HangAfter {
			if reply.PingEvery > 0 {
				b.pingUntilCancel(w, r, req, reply.PingEvery)
				return
			}
			b.waitForCancel(r, req)
			return
		}
		if reply.CutAfter > 0 && i == reply.CutAfter {
			panic(http.ErrAbortHandler)
		}
		if reply.EndAfter > 0 && i == reply.EndAfter {
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
	if endpoint == embeddingPath {
		return map[string]any{"prompt_tokens": u.PromptTokens, "total_tokens": u.PromptTokens}
	}
	body := map[string]any{"prompt_tokens": u.PromptTokens, "completion_tokens": u.CompletionTokens,
		"total_tokens": u.PromptTokens + u.CompletionTokens}
	if u.CachedTokens > 0 {
		body["prompt_tokens_details"] = map[string]int{"cached_tokens": u.CachedTokens}
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
