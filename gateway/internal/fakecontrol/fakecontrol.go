// Package fakecontrol is a control plane for tests: it serves the config stream of
// docs/specs/CONTROL-PROTOCOL.md — the current config the test publishes, then totals,
// then every change — takes usage batches (de-duplicated by batch ID as the protocol
// settles, so a test can check exactly-once counting) and status reports, runs the
// request checks, records every request, and lets the test script stream events, go
// down, restart, answer with another protocol version, or fail usage batches (an error
// answer, an ack dropped after counting, or an ack naming another batch). Its totals
// are scripted: the test sets the windows and the live-gateway count, and the server
// keeps each instance's counted_through, one entry per epoch, as kaiak-control does; it
// does not aggregate
// usage itself; each stream's first totals list every scripted window, later ones only
// the windows that changed for that stream (one it got that the script dropped is
// listed at "0" while still in its window), as kaiak-control sends them. Test tooling only: nothing in the gateway binary imports it. It speaks
// raw JSON and imports nothing from the gateway, so the control package's own tests
// can use it.
package fakecontrol

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"sync"
	"time"
)

// Server is one fake control plane on a local port.
type Server struct {
	srv   *httptest.Server
	token string

	mu       sync.Mutex
	protocol string // the Kaiak-Protocol value answered; "" omits the header
	down     bool
	// config is the current config, on one line (the text sent and hashed); nil
	// before the first Publish and after Restart.
	config  []byte
	streams map[*Stream]struct{}
	// opened counts the streams opened so far; each stream's Seq is its count.
	opened    int
	requests  []Request
	connected chan *Stream

	// Usage intake: the last counted batch's sequence per instance and epoch, the
	// counted batches, the scripted faults (queued, then the persistent one).
	lastBatch   map[string]map[string]int64
	counted     []UsageBatch
	usageFaults []UsageFault
	usageFault  *UsageFault
	// Totals: the scripted windows (a JSON array) and live-gateway count, and whether
	// changes push totals to the open streams.
	windows    []byte
	live       int64
	pushTotals bool
	// holdConnectTotals leaves the totals out after a stream's first config (a control
	// plane slow to send them).
	holdConnectTotals bool
	usageEvents       chan UsageEvent
	statuses          [][]byte
	statusEvents      chan []byte
}

// BatchID identifies a usage batch.
type BatchID struct {
	Instance string `json:"instance"`
	Epoch    string `json:"epoch"`
	Sequence int64  `json:"sequence"`
}

// UsageBatch is a usage batch as received; records stay raw JSON.
type UsageBatch struct {
	Batch   BatchID           `json:"batch"`
	Records []json.RawMessage `json:"records"`
}

// Usage batch outcomes, as UsageEvent reports them.
const (
	// OutcomeCounted: counted and acknowledged.
	OutcomeCounted = "counted"
	// OutcomeDuplicate: counted before (a resend); acknowledged, not counted.
	OutcomeDuplicate = "duplicate"
	// OutcomeAckDropped: counted (or a duplicate), then the connection dropped before
	// the ack, as a lost ack.
	OutcomeAckDropped = "ack-dropped"
	// OutcomeRefused: answered with an error; nothing counted.
	OutcomeRefused = "refused"
	// OutcomeAckOther: answered with an ack naming another batch; nothing counted.
	OutcomeAckOther = "ack-other"
)

// UsageEvent is one POST /v1/usage as the server took it.
type UsageEvent struct {
	Batch   BatchID
	Records int
	Outcome string
	// Status and Code are the error answer of a refused batch.
	Status int
	Code   string
	// Body is the request body as received.
	Body []byte
}

// UsageFault scripts the answer to a usage batch.
type UsageFault struct {
	// Status and Code answer with this error instead of taking the batch (e.g. 400
	// usage-batch-invalid, 500 internal-error).
	Status int
	Code   string
	// DropAck takes the batch as usual (counted, or a duplicate), then drops the
	// connection instead of answering: the gateway sees a lost ack.
	DropAck bool
	// AckOther answers with an ack naming another batch (the next sequence of the
	// same epoch) without taking the batch, as a broken proxy or cache would.
	AckOther bool
}

// eventsBuffer bounds the usage and status events held for a test that does not read
// them; further ones are not announced (the records of counted batches stay).
const eventsBuffer = 4096

// Request is one request the server received.
type Request struct {
	Method string
	Path   string
	Query  string
	Header http.Header
}

// Stream is one open config stream. The test adds events to it; the server writes
// them in order.
type Stream struct {
	// Instance is the gateway's Kaiak-Instance; Seq counts the streams opened on the
	// server, from 1.
	Instance string
	Seq      int
	// gotConfig: the stream has been sent a config, so totals may follow (kaiak-control
	// sends none before). Guarded by the server's mu.
	gotConfig bool
	// sent is each window this stream was last sent, by scope and type; nil before
	// its first totals (which are complete). Guarded by the server's mu.
	sent   map[string]totalsWindow
	server *Server
	frames chan []byte
	close  chan struct{}
	once   sync.Once
	done   chan struct{}
}

// protocolVersion is the Kaiak-Protocol value the fake speaks
// (docs/specs/CONTROL-PROTOCOL.md).
const protocolVersion = "5"

// connectedBuffer bounds the streams Connected holds for a test that does not read
// them; further ones are not announced.
const connectedBuffer = 64

// New starts a server that accepts token.
func New(token string) *Server {
	s := &Server{token: token, protocol: protocolVersion, streams: map[*Stream]struct{}{},
		connected: make(chan *Stream, connectedBuffer), lastBatch: map[string]map[string]int64{},
		windows: []byte("[]"), live: 1,
		usageEvents: make(chan UsageEvent, eventsBuffer), statusEvents: make(chan []byte, eventsBuffer)}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/stream", s.serveStream)
	mux.HandleFunc("POST /v1/usage", s.serveUsage)
	mux.HandleFunc("POST /v1/status", s.serveStatus)
	s.srv = httptest.NewServer(s.checked(mux))
	return s
}

// URL is the server's base URL (no trailing slash).
func (s *Server) URL() string { return s.srv.URL }

// Close ends every open stream and stops the server: later connections are refused.
func (s *Server) Close() {
	s.closeStreams()
	s.srv.Close()
}

// Publish makes config the current config, sends it to every open stream — then the
// totals to a stream that had none yet (unless HoldTotalsOnConnect), and to every
// stream with PushTotalsOnChange — and returns its hash.
func (s *Server) Publish(config []byte) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.config = compact(config)
	frame := eventFrame("config", configEvent(s.config))
	for st := range s.streams {
		st.enqueue(frame)
		if !st.gotConfig {
			st.gotConfig = true
			if !s.holdConnectTotals && !s.pushTotals {
				st.enqueue(eventFrame("totals", s.streamTotalsLocked(st)))
			}
		}
	}
	s.totalsChangedLocked()
	return configHash(config)
}

// Restart forgets the current config, the last counted batch of each instance and the
// totals, and ends the open streams, as a control plane with an in-memory store does
// when it restarts: nothing is published until the next Publish. CountedRecords keeps what
// was counted before, for the test.
func (s *Server) Restart() {
	s.mu.Lock()
	s.config = nil
	s.lastBatch = map[string]map[string]int64{}
	s.windows = []byte("[]")
	s.mu.Unlock()
	s.closeStreams()
}

// PushTotalsOnChange makes every change to the totals — a publish, a counted batch,
// SetWindows, SetLiveGateways — push them to every open stream, as kaiak-control does
// (without its once-a-second pacing). Off by default: changes then reach a stream only
// through PushTotals and PushCurrentTotals. Either way a stream gets the totals right
// after its replay (CONTROL-PROTOCOL.md, Config stream), unless HoldTotalsOnConnect.
func (s *Server) PushTotalsOnChange() {
	s.mu.Lock()
	s.pushTotals = true
	s.mu.Unlock()
}

// HoldTotalsOnConnect makes new streams skip the totals that follow the replay (true):
// a control plane whose totals are delayed. PushTotals and PushCurrentTotals still
// reach the open streams.
func (s *Server) HoldTotalsOnConnect(hold bool) {
	s.mu.Lock()
	s.holdConnectTotals = hold
	s.mu.Unlock()
}

// SetWindows sets the windows every totals message carries (a JSON array, one line)
// and counts as a change to the totals.
func (s *Server) SetWindows(windows []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.windows = compact(windows)
	s.totalsChangedLocked()
}

// SetLiveGateways sets the live-gateway count every totals message carries (1 by
// default) and counts as a change to the totals.
func (s *Server) SetLiveGateways(n int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.live = n
	s.totalsChangedLocked()
}

// PushCurrentTotals sends the current totals to every open stream, each with its
// instance's counted_through.
func (s *Server) PushCurrentTotals() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pushCurrentLocked()
}

// totalsChangedLocked pushes the totals with PushTotalsOnChange.
func (s *Server) totalsChangedLocked() {
	if s.pushTotals {
		s.pushCurrentLocked()
	}
}

// pushCurrentLocked sends the totals to every open stream that has been sent a config:
// kaiak-control sends no totals before.
func (s *Server) pushCurrentLocked() {
	for st := range s.streams {
		if st.gotConfig {
			st.enqueue(eventFrame("totals", s.streamTotalsLocked(st)))
		}
	}
}

// totalsLocked is the totals message instance gets with windows: the live-gateway
// count, the instance's last counted batch of each epoch, and the windows.
func (s *Server) totalsLocked(instance string, windows []byte) []byte {
	counted := []byte("[")
	for _, epoch := range slices.Sorted(maps.Keys(s.lastBatch[instance])) {
		if len(counted) > 1 {
			counted = append(counted, ',')
		}
		counted = fmt.Appendf(counted, `{"epoch":%q,"sequence":%d}`, epoch, s.lastBatch[instance][epoch])
	}
	counted = append(counted, ']')
	return fmt.Appendf(nil, `{"live_gateways":%d,"counted_through":%s,"windows":%s}`, s.live, counted, windows)
}

// totalsWindow is one scripted window: its scope and type, start and amount.
type totalsWindow struct {
	Group       string `json:"group,omitempty"`
	Type        string `json:"type"`
	WindowStart string `json:"window_start"`
	Used        string `json:"used"`
}

func (w totalsWindow) key() string { return w.Group + "\n" + w.Type }

// current reports whether the window is still the current one of its type at now: a
// window that ended with its hour or month is never listed again (CONTROL-PROTOCOL.md,
// Messages → Totals).
func (w totalsWindow) current(now time.Time) bool {
	start, err := time.Parse(time.RFC3339, w.WindowStart)
	if err != nil {
		return false
	}
	now = now.UTC()
	if w.Type == "usd_per_month" {
		return start.Equal(time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC))
	}
	return start.Equal(now.Truncate(time.Hour))
}

// streamTotalsLocked is the next totals message for st: complete for its first, then
// the windows that changed since its last — a window it was sent that the script no
// longer has, still in its window, listed at "0"; one that ended, not listed — and it
// remembers what it sent.
func (s *Server) streamTotalsLocked(st *Stream) []byte {
	var scripted []totalsWindow
	if err := json.Unmarshal(s.windows, &scripted); err != nil {
		panic(fmt.Sprintf("fakecontrol: windows are not a totals window list: %v", err))
	}
	now := make(map[string]totalsWindow, len(scripted))
	for _, w := range scripted {
		now[w.key()] = w
	}
	listed := scripted
	if st.sent != nil {
		listed = nil
		for _, w := range scripted {
			if st.sent[w.key()] != w {
				listed = append(listed, w)
			}
		}
		for _, key := range slices.Sorted(maps.Keys(st.sent)) {
			if _, ok := now[key]; !ok && st.sent[key].current(time.Now()) {
				gone := st.sent[key]
				gone.Used = "0"
				listed = append(listed, gone)
				now[key] = gone
			}
		}
	}
	st.sent = now
	windows, err := json.Marshal(append([]totalsWindow{}, listed...))
	if err != nil {
		panic(err)
	}
	return s.totalsLocked(st.Instance, windows)
}

// PushTotals sends a totals event with data to every open stream, as it is: the
// streams' own changes-only bookkeeping does not see it.
func (s *Server) PushTotals(data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	frame := eventFrame("totals", data)
	for st := range s.streams {
		st.enqueue(frame)
	}
}

// CloseStreams ends every open stream, as a control plane going away does; with
// SetDown(true) the gateways cannot reconnect.
func (s *Server) CloseStreams() { s.closeStreams() }

// SetDown makes the server drop every new connection without an answer (true), or
// serve again (false). Streams already open stay open (CloseStreams ends them).
func (s *Server) SetDown(down bool) {
	s.mu.Lock()
	s.down = down
	s.mu.Unlock()
}

// SetProtocol sets the Kaiak-Protocol value of every answer; "" leaves the header out.
func (s *Server) SetProtocol(v string) {
	s.mu.Lock()
	s.protocol = v
	s.mu.Unlock()
}

// Requests returns the requests received so far.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Request, len(s.requests))
	copy(out, s.requests)
	return out
}

// Gets returns the GET requests received so far (the stream).
func (s *Server) Gets() []Request {
	var out []Request
	for _, r := range s.Requests() {
		if r.Method == http.MethodGet {
			out = append(out, r)
		}
	}
	return out
}

// FailUsage queues faults: each answers one usage batch, in order.
func (s *Server) FailUsage(faults ...UsageFault) {
	s.mu.Lock()
	s.usageFaults = append(s.usageFaults, faults...)
	s.mu.Unlock()
}

// SetUsageFault answers every usage batch with f once the queued faults are used;
// nil takes batches normally again.
func (s *Server) SetUsageFault(f *UsageFault) {
	s.mu.Lock()
	s.usageFault = f
	s.mu.Unlock()
}

// CountedRecords returns the records of every counted batch, in order.
func (s *Server) CountedRecords() []json.RawMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []json.RawMessage
	for _, b := range s.counted {
		out = append(out, b.Records...)
	}
	return out
}

// UsageEvents delivers every usage batch the server took, in order.
func (s *Server) UsageEvents() <-chan UsageEvent { return s.usageEvents }

// Statuses returns the bodies of the status reports accepted so far.
func (s *Server) Statuses() [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([][]byte(nil), s.statuses...)
}

// StatusEvents delivers the body of every status report accepted, in order.
func (s *Server) StatusEvents() <-chan []byte { return s.statusEvents }

// Connected delivers each stream once its first events are queued, in connection
// order.
func (s *Server) Connected() <-chan *Stream { return s.connected }

// Opened is how many streams have been opened so far: the Seq of the latest.
func (s *Server) Opened() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.opened
}

// Send adds an event to the stream.
func (st *Stream) Send(event string, data []byte) { st.enqueue(eventFrame(event, data)) }

// SendConfig adds a config event carrying config and its hash.
func (st *Stream) SendConfig(config []byte) {
	st.enqueue(eventFrame("config", configEvent(config)))
}

// Comment adds a comment line (a heartbeat).
func (st *Stream) Comment(text string) { st.enqueue([]byte(": " + text + "\n\n")) }

// Close ends the stream once the events added before it are written. It leaves the
// server's open streams at once, so a Publish after it never queues on it.
func (st *Stream) Close() {
	st.server.mu.Lock()
	delete(st.server.streams, st)
	st.server.mu.Unlock()
	st.end()
}

func (st *Stream) end() { st.once.Do(func() { close(st.close) }) }

// Done is closed when the stream has ended, by either side.
func (st *Stream) Done() <-chan struct{} { return st.done }

func (st *Stream) enqueue(frame []byte) {
	select {
	case st.frames <- frame:
	case <-st.done:
	}
}

// configHash is a config's config_hash: the lowercase hex SHA-256 of the config as
// sent, on one line.
func configHash(config []byte) string {
	sum := sha256.Sum256(compact(config))
	return hex.EncodeToString(sum[:])
}

// configEvent is the data of a config event carrying config.
func configEvent(config []byte) []byte {
	return fmt.Appendf(nil, `{"config_hash":%q,"config":%s}`, configHash(config), compact(config))
}

func eventFrame(event string, data []byte) []byte {
	return fmt.Appendf(nil, "event: %s\ndata: %s\n\n", event, data)
}

// compact puts a JSON document on one line, as an event's data line needs; a
// document that is not JSON is returned as is (a test sending a broken config).
func compact(doc []byte) []byte {
	var b bytes.Buffer
	if err := json.Compact(&b, doc); err != nil {
		return doc
	}
	return b.Bytes()
}

// closeStreams ends every open stream. They leave the set under the lock, so a
// Publish after it (after Restart) never queues on one.
func (s *Server) closeStreams() {
	s.mu.Lock()
	open := make([]*Stream, 0, len(s.streams))
	for st := range s.streams {
		open = append(open, st)
		delete(s.streams, st)
	}
	s.mu.Unlock()
	for _, st := range open {
		st.end()
	}
}

var instancePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,252}$`)

// checked records the request and runs the request checks (token, protocol version,
// instance) in the protocol's order; every answer carries the protocol header.
func (s *Server) checked(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.requests = append(s.requests, Request{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery,
			Header: r.Header.Clone()})
		down, protocol := s.down, s.protocol
		s.mu.Unlock()
		if down {
			if conn, _, err := http.NewResponseController(w).Hijack(); err == nil {
				_ = conn.Close() // dropping the connection is the point
			}
			return
		}
		if protocol != "" {
			w.Header().Set("Kaiak-Protocol", protocol)
		}
		switch {
		case r.Header.Get("Authorization") != "Bearer "+s.token:
			writeError(w, http.StatusUnauthorized, "unauthorized")
		case len(r.Header.Values("Kaiak-Protocol")) != 1 || r.Header.Get("Kaiak-Protocol") != protocolVersion:
			writeError(w, http.StatusBadRequest, "protocol-version-mismatch")
		case len(r.Header.Values("Kaiak-Instance")) != 1 || !instancePattern.MatchString(r.Header.Get("Kaiak-Instance")):
			writeError(w, http.StatusBadRequest, "instance-invalid")
		default:
			next.ServeHTTP(w, r)
		}
	})
}

func writeError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `{"error":%q}`, code) // a failed write means the gateway left
}

// serveStream sends the current config, then the totals, then every config
// published while the stream is open; with nothing published yet it stays open and
// sends both once a config is published.
func (s *Server) serveStream(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flush := func() { _ = http.NewResponseController(w).Flush() } // fails only once the gateway left

	st := &Stream{Instance: r.Header.Get("Kaiak-Instance"), server: s, frames: make(chan []byte, 256),
		close: make(chan struct{}), done: make(chan struct{})}
	// done closes before the stream leaves the set: a Publish blocked on a full
	// buffer (holding s.mu) must be released first.
	defer func() {
		close(st.done)
		s.mu.Lock()
		delete(s.streams, st)
		s.mu.Unlock()
	}()

	s.mu.Lock()
	s.opened++
	st.Seq = s.opened
	if s.config != nil {
		st.gotConfig = true
		st.frames <- eventFrame("config", configEvent(s.config))
		if !s.holdConnectTotals {
			st.frames <- eventFrame("totals", s.streamTotalsLocked(st))
		}
	}
	s.streams[st] = struct{}{}
	s.mu.Unlock()
	select {
	case s.connected <- st:
	default:
	}
	flush()

	for {
		select {
		case frame := <-st.frames:
			if _, err := w.Write(frame); err != nil {
				return
			}
			flush()
		case <-st.close:
			// Write what was queued before the close.
			for {
				select {
				case frame := <-st.frames:
					if _, err := w.Write(frame); err != nil {
						return
					}
				default:
					flush()
					return
				}
			}
		case <-r.Context().Done():
			return
		}
	}
}

// maxUsageBody and maxStatusBody are the body limits of kaiak-control's plugin.
const (
	maxUsageBody  = 2 << 20
	maxStatusBody = 64 << 10
)

// serveUsage takes a usage batch as kaiak-control's intake does, in its order: the
// batch message (a light check: shape, 1 to 500 records, the records' instance and
// unique record IDs), the instance against the header; then the scripted fault, if
// any; then de-duplication by the last counted batch of the batch's epoch. A batch is counted
// whether or not a config is published.
func (s *Server) serveUsage(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxUsageBody))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "request-invalid")
		return
	}
	var batch UsageBatch
	event := UsageEvent{Body: body}
	refuse := func(status int, code string) {
		event.Outcome, event.Status, event.Code = OutcomeRefused, status, code
		s.announceUsage(event)
		writeError(w, status, code)
	}
	if err := json.Unmarshal(body, &batch); err != nil || batch.Batch.Epoch == "" || batch.Batch.Sequence < 1 ||
		len(batch.Records) == 0 || len(batch.Records) > 500 {
		refuse(http.StatusBadRequest, "usage-batch-invalid")
		return
	}
	event.Batch, event.Records = batch.Batch, len(batch.Records)
	if code := checkRecords(batch); code != "" {
		refuse(http.StatusBadRequest, code)
		return
	}
	if batch.Batch.Instance != r.Header.Get("Kaiak-Instance") {
		refuse(http.StatusBadRequest, "instance-mismatch")
		return
	}

	s.mu.Lock()
	var fault *UsageFault
	if len(s.usageFaults) > 0 {
		fault = &s.usageFaults[0]
		s.usageFaults = s.usageFaults[1:]
	} else {
		fault = s.usageFault
	}
	if fault != nil && fault.AckOther {
		other := batch.Batch
		other.Sequence++
		s.mu.Unlock()
		event.Outcome = OutcomeAckOther
		s.announceUsage(event)
		writeAck(w, other)
		return
	}
	if fault != nil && !fault.DropAck {
		s.mu.Unlock()
		refuse(fault.Status, fault.Code)
		return
	}
	event.Outcome = OutcomeDuplicate
	epochs := s.lastBatch[batch.Batch.Instance]
	if last, seen := epochs[batch.Batch.Epoch]; !seen || batch.Batch.Sequence > last {
		event.Outcome = OutcomeCounted
		if epochs == nil {
			epochs = map[string]int64{}
			s.lastBatch[batch.Batch.Instance] = epochs
		}
		epochs[batch.Batch.Epoch] = batch.Batch.Sequence
		s.counted = append(s.counted, batch)
		s.totalsChangedLocked()
	}
	s.mu.Unlock()

	if fault != nil && fault.DropAck {
		event.Outcome = OutcomeAckDropped
		s.announceUsage(event)
		if conn, _, err := http.NewResponseController(w).Hijack(); err == nil {
			_ = conn.Close() // dropping the connection is the point
		}
		return
	}
	s.announceUsage(event)
	writeAck(w, batch.Batch)
}

// writeAck answers a usage batch with the ack naming batch.
func writeAck(w http.ResponseWriter, batch BatchID) {
	id, _ := json.Marshal(batch) // a struct of strings and an integer always encodes
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"batch":%s}`, id) // a failed write means the gateway left
}

// checkRecords applies the batch rules on records: every record of the batch's
// instance, record IDs unique. It returns the first rule broken, or "".
func checkRecords(batch UsageBatch) string {
	seen := map[string]bool{}
	for _, raw := range batch.Records {
		var rec struct {
			RecordID        string `json:"record_id"`
			GatewayInstance string `json:"gateway_instance"`
		}
		if err := json.Unmarshal(raw, &rec); err != nil || rec.RecordID == "" {
			return "usage-batch-invalid"
		}
		if rec.GatewayInstance != batch.Batch.Instance {
			return "record-instance-mismatch"
		}
		if seen[rec.RecordID] {
			return "record-id-duplicate"
		}
		seen[rec.RecordID] = true
	}
	return ""
}

func (s *Server) announceUsage(e UsageEvent) {
	select {
	case s.usageEvents <- e:
	default:
	}
}

// serveStatus accepts a status report naming the requester's instance: 204.
func (s *Server) serveStatus(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxStatusBody))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "request-invalid")
		return
	}
	var status struct {
		Instance string `json:"instance"`
	}
	if err := json.Unmarshal(body, &status); err != nil {
		writeError(w, http.StatusBadRequest, "status-invalid")
		return
	}
	if status.Instance != r.Header.Get("Kaiak-Instance") {
		writeError(w, http.StatusBadRequest, "instance-mismatch")
		return
	}
	s.mu.Lock()
	s.statuses = append(s.statuses, body)
	s.mu.Unlock()
	select {
	case s.statusEvents <- body:
	default:
	}
	w.WriteHeader(http.StatusNoContent)
}
