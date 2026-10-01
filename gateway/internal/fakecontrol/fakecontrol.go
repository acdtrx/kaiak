// Package fakecontrol is a control plane for tests: it serves the config snapshot and
// the config stream of docs/specs/CONTROL-PROTOCOL.md from configs the test
// publishes, takes usage batches (de-duplicated by batch ID as the protocol settles, so
// a test can check exactly-once counting) and status reports, runs the request checks,
// records every request, and lets the test script stream events, go down, restart,
// answer with another protocol version, or fail usage batches (an error answer, an
// ack dropped after counting, or an ack naming another batch). Its totals are
// scripted: the test sets the windows and the live-gateway count, and the server
// keeps the revision (a random control-plane ID, new on Restart, and a sequence
// bumped by every change) and each instance's counted_through, as kaiak-control
// does; it does not aggregate usage itself. Test tooling only: nothing in the gateway
// binary imports it. It speaks raw JSON and imports nothing from the gateway, so the
// control package's own tests can use it.
package fakecontrol

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"sync"
)

// Server is one fake control plane on a local port.
type Server struct {
	srv   *httptest.Server
	token string

	mu       sync.Mutex
	protocol string // the Kaiak-Protocol value answered; "" omits the header
	down     bool
	// configs[i] is version i+1, counted in configEpoch: the store's epoch, new on
	// Restart (an in-memory store starts over).
	configs     [][]byte
	configEpoch string
	streams     map[*Stream]struct{}
	requests    []Request
	connected   chan *Stream

	// Usage intake: the last counted batch per instance, the counted batches, the
	// scripted faults (queued, then the persistent one).
	lastBatch   map[string]BatchID
	counted     []UsageBatch
	usageFaults []UsageFault
	usageFault  *UsageFault
	// Totals: the revision (this process's ID and its sequence), the scripted windows
	// (a JSON array) and live-gateway count, and whether changes push totals to the
	// open streams.
	controlPlane string
	sequence     int64
	windows      []byte
	live         int64
	pushTotals   bool
	// holdConnectTotals leaves the totals out after a stream's replay (a control plane
	// slow to send them).
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
	// usage-batch-invalid, 503 config-unavailable, 500 internal-error).
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
	// Since and SinceEpoch are the version the gateway resumed from and its epoch;
	// Instance is its Kaiak-Instance.
	Since      int64
	SinceEpoch string
	Instance   string
	// epoch is the server's config epoch when the stream opened.
	epoch  string
	server *Server
	frames chan []byte
	close  chan struct{}
	once   sync.Once
	done   chan struct{}
}

// protocolVersion is the Kaiak-Protocol value the fake speaks
// (docs/specs/CONTROL-PROTOCOL.md).
const protocolVersion = "3"

// connectedBuffer bounds the streams Connected holds for a test that does not read
// them; further ones are not announced.
const connectedBuffer = 64

// New starts a server that accepts token.
func New(token string) *Server {
	s := &Server{token: token, protocol: protocolVersion, streams: map[*Stream]struct{}{},
		connected: make(chan *Stream, connectedBuffer), lastBatch: map[string]BatchID{},
		controlPlane: newControlPlaneID(), configEpoch: newControlPlaneID(), windows: []byte("[]"), live: 1,
		usageEvents: make(chan UsageEvent, eventsBuffer), statusEvents: make(chan []byte, eventsBuffer)}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/config", s.serveConfig)
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

// Publish stores config as the next version, pushes it to every open stream (then
// the totals, with PushTotalsOnChange) and returns its version.
func (s *Server) Publish(config []byte) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.configs = append(s.configs, bytes.Clone(config))
	version := int64(len(s.configs))
	frame := configFrame(s.configEpoch, version, config)
	for st := range s.streams {
		st.enqueue(frame)
	}
	s.totalsChangedLocked()
	return version
}

// Restart forgets every published version, the last counted batch of each instance
// and the totals, starts a new config epoch and a new control-plane ID for the
// revision, and ends the open streams, as a control plane with an in-memory store does
// when it restarts: versions start again at 1, in a new epoch. Counted keeps what was
// counted before, for the test.
func (s *Server) Restart() {
	s.mu.Lock()
	s.configs, s.configEpoch = nil, newControlPlaneID()
	s.lastBatch = map[string]BatchID{}
	s.controlPlane, s.sequence, s.windows = newControlPlaneID(), 0, []byte("[]")
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

// ConfigEpoch returns the epoch the published versions count in.
func (s *Server) ConfigEpoch() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.configEpoch
}

// Revision returns the current totals revision: the control-plane ID and sequence.
func (s *Server) Revision() (controlPlane string, sequence int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.controlPlane, s.sequence
}

// Totals returns the current totals as instance gets them (its counted_through).
func (s *Server) Totals(instance string) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.totalsLocked(instance)
}

// PushCurrentTotals sends the current totals to every open stream, each with its
// instance's counted_through.
func (s *Server) PushCurrentTotals() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pushCurrentLocked()
}

// totalsChangedLocked bumps the revision and, with PushTotalsOnChange, pushes the
// totals.
func (s *Server) totalsChangedLocked() {
	s.sequence++
	if s.pushTotals {
		s.pushCurrentLocked()
	}
}

func (s *Server) pushCurrentLocked() {
	if len(s.configs) == 0 {
		return // no config: no totals, as kaiak-control
	}
	for st := range s.streams {
		st.enqueue(eventFrame("totals", "", s.totalsLocked(st.Instance)))
	}
}

// totalsLocked is the totals message instance gets: the revision, the current config
// (epoch and version), the live-gateway count, the instance's last counted batch and the windows.
func (s *Server) totalsLocked(instance string) []byte {
	counted := []byte("null")
	if last, ok := s.lastBatch[instance]; ok {
		counted = fmt.Appendf(nil, `{"epoch":%q,"sequence":%d}`, last.Epoch, last.Sequence)
	}
	return fmt.Appendf(nil, `{"revision":{"control_plane":%q,"sequence":%d},"config_epoch":%q,"config_version":%d,"live_gateways":%d,"counted_through":%s,"windows":%s}`,
		s.controlPlane, s.sequence, s.configEpoch, len(s.configs), s.live, counted, s.windows)
}

func newControlPlaneID() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never returns an error
	return hex.EncodeToString(b[:])
}

// PushTotals sends a totals event with data to every open stream.
func (s *Server) PushTotals(data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	frame := eventFrame("totals", "", data)
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

// Gets returns the GET requests received so far (snapshot and stream).
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

// Counted returns the batches counted so far, in the order counted.
func (s *Server) Counted() []UsageBatch {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]UsageBatch(nil), s.counted...)
}

// CountedRecords returns the records of every counted batch, in order.
func (s *Server) CountedRecords() []json.RawMessage {
	var out []json.RawMessage
	for _, b := range s.Counted() {
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

// Connected delivers each stream once its replay is queued, in connection order.
func (s *Server) Connected() <-chan *Stream { return s.connected }

// Send adds an event to the stream; id may be empty.
func (st *Stream) Send(event, id string, data []byte) { st.enqueue(eventFrame(event, id, data)) }

// SendConfig adds a config event carrying the snapshot of version and config in the
// server's config epoch.
func (st *Stream) SendConfig(version int64, config []byte) {
	st.enqueue(configFrame(st.epoch, version, config))
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

// Snapshot is the answer body of GET /v1/config for version and config in epoch.
func Snapshot(epoch string, version int64, config []byte) []byte {
	return fmt.Appendf(nil, `{"config_epoch":%q,"version":%d,"config":%s}`, epoch, version, compact(config))
}

func configFrame(epoch string, version int64, config []byte) []byte {
	return eventFrame("config", strconv.FormatInt(version, 10), Snapshot(epoch, version, config))
}

func eventFrame(event, id string, data []byte) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "event: %s\n", event)
	if id != "" {
		fmt.Fprintf(&b, "id: %s\n", id)
	}
	fmt.Fprintf(&b, "data: %s\n\n", data)
	return b.Bytes()
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
// Publish after it (a new epoch's first version, after Restart) never queues on one.
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

func (s *Server) serveConfig(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	n := len(s.configs)
	var body []byte
	if n > 0 {
		body = Snapshot(s.configEpoch, int64(n), s.configs[n-1])
	}
	s.mu.Unlock()
	if body == nil {
		writeError(w, http.StatusServiceUnavailable, "config-unavailable")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body) // a failed write means the gateway left
}

var (
	sincePattern = regexp.MustCompile(`^(0|[1-9][0-9]*)$`)
	epochPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)
)

// serveStream resumes from since and its config epoch: every newer version, then each
// one published while the stream is open; resync when since cannot be resumed from
// (another epoch, nothing published, or newer than the current version). History is
// unbounded.
func (s *Server) serveStream(w http.ResponseWriter, r *http.Request) {
	raw, rawEpoch := r.URL.Query()["since"], r.URL.Query()["config_epoch"]
	var since int64
	var err error
	if len(raw) == 1 && sincePattern.MatchString(raw[0]) {
		since, err = strconv.ParseInt(raw[0], 10, 64)
	}
	if len(raw) != 1 || !sincePattern.MatchString(raw[0]) || err != nil ||
		len(rawEpoch) != 1 || !epochPattern.MatchString(rawEpoch[0]) {
		writeError(w, http.StatusBadRequest, "since-invalid")
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flush := func() { _ = http.NewResponseController(w).Flush() } // fails only once the gateway left

	st := &Stream{Since: since, SinceEpoch: rawEpoch[0], Instance: r.Header.Get("Kaiak-Instance"), server: s, frames: make(chan []byte, 256), close: make(chan struct{}), done: make(chan struct{})}
	// done closes before the stream leaves the set: a Publish blocked on a full
	// buffer (holding s.mu) must be released first.
	defer func() {
		close(st.done)
		s.mu.Lock()
		delete(s.streams, st)
		s.mu.Unlock()
	}()

	s.mu.Lock()
	current := int64(len(s.configs))
	st.epoch = s.configEpoch
	if st.SinceEpoch != s.configEpoch || current == 0 || since > current {
		s.mu.Unlock()
		_, _ = w.Write(eventFrame("resync", "", []byte("{}")))
		flush()
		return
	}
	for v := since + 1; v <= current; v++ {
		st.frames <- configFrame(s.configEpoch, v, s.configs[v-1])
	}
	if !s.holdConnectTotals {
		st.frames <- eventFrame("totals", "", s.totalsLocked(st.Instance))
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
// unique record IDs), the instance against the header, a published config; then the
// scripted fault, if any; then de-duplication by the instance's last counted batch.
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
	if len(s.configs) == 0 {
		s.mu.Unlock()
		refuse(http.StatusServiceUnavailable, "config-unavailable")
		return
	}
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
		totals := s.totalsLocked(batch.Batch.Instance)
		s.mu.Unlock()
		event.Outcome = OutcomeAckOther
		s.announceUsage(event)
		writeAck(w, other, totals)
		return
	}
	if fault != nil && !fault.DropAck {
		s.mu.Unlock()
		refuse(fault.Status, fault.Code)
		return
	}
	event.Outcome = OutcomeDuplicate
	last, seen := s.lastBatch[batch.Batch.Instance]
	if !seen || last.Epoch != batch.Batch.Epoch || batch.Batch.Sequence > last.Sequence {
		event.Outcome = OutcomeCounted
		s.lastBatch[batch.Batch.Instance] = batch.Batch
		s.counted = append(s.counted, batch)
		s.totalsChangedLocked()
	}
	totals := s.totalsLocked(batch.Batch.Instance)
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
	writeAck(w, batch.Batch, totals)
}

// writeAck answers a usage batch with the ack naming batch, carrying totals.
func writeAck(w http.ResponseWriter, batch BatchID, totals []byte) {
	id, _ := json.Marshal(batch) // a struct of strings and an integer always encodes
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"batch":%s,"totals":%s}`, id, totals) // a failed write means the gateway left
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
