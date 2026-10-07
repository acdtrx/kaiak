package e2e

// The harness: builds the kaiak binary once, runs it as a subprocess with a config
// file and environment, and reads its JSON log lines — the log is
// how the test learns the bound ports and waits for events (config applied, request
// settled, draining), never fixed sleeps.

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"kaiak/internal/auth"
)

// kaiakBin is the binary TestMain built.
var kaiakBin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "kaiak-e2e-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2e:", err)
		os.Exit(1)
	}
	code := func() int {
		defer os.RemoveAll(dir)
		kaiakBin = filepath.Join(dir, "kaiak")
		args := []string{"build", "-o", kaiakBin}
		if raceEnabled {
			args = append(args, "-race")
		}
		build := exec.Command("go", append(args, "./cmd/kaiak")...)
		build.Dir = ".." // the gateway module root
		build.Stdout, build.Stderr = os.Stderr, os.Stderr
		if err := build.Run(); err != nil {
			fmt.Fprintln(os.Stderr, "e2e: building kaiak:", err)
			return 1
		}
		return m.Run()
	}()
	os.Exit(code)
}

// waitLimit bounds every wait on the gateway: generous for a slow CI machine, far
// longer than anything should take.
const waitLimit = 15 * time.Second

// changes wakes the waits on state another goroutine updates: it is notified after
// every change, and ended once no more will come.
type changes struct {
	mu    sync.Mutex
	next  chan struct{} // closed and replaced on every notify
	ended string        // what ended the changes; "" while more may come
}

func newChanges() *changes { return &changes{next: make(chan struct{})} }

// notify wakes every wait to look at the state again.
func (c *changes) notify() {
	c.mu.Lock()
	defer c.mu.Unlock()
	close(c.next)
	c.next = make(chan struct{})
}

// end wakes every wait a last time; why says what ended the changes.
func (c *changes) end(why string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ended = why
	close(c.next)
	c.next = make(chan struct{})
}

// wait calls check now and after every change until it returns true, failing the
// test once the changes end or limit passes first.
func (c *changes) wait(t *testing.T, what string, limit time.Duration, check func() bool) {
	t.Helper()
	deadline := time.NewTimer(limit)
	defer deadline.Stop()
	for {
		c.mu.Lock()
		next, ended := c.next, c.ended // before check: a change after it wakes this wait
		c.mu.Unlock()
		switch {
		case check():
			return
		case ended != "":
			t.Fatalf("%s before %s", ended, what)
		}
		select {
		case <-next:
		case <-deadline.C:
			t.Fatalf("%s not seen within %s", what, limit)
		}
	}
}

// logLines collects a process's log, one JSON object per line.
type logLines struct {
	mu      sync.Mutex
	raw     []string
	lines   []map[string]any
	changes *changes // notified on every new line, ended at EOF
}

func newLogLines() *logLines { return &logLines{changes: newChanges()} }

func (l *logLines) read(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		var entry map[string]any
		_ = json.Unmarshal([]byte(line), &entry) // non-JSON lines (a race report) stay in raw
		l.mu.Lock()
		l.raw = append(l.raw, line)
		if entry != nil {
			l.lines = append(l.lines, entry)
		}
		l.mu.Unlock()
		l.changes.notify()
	}
	l.changes.end("the process exited")
}

// wait returns the first line matching match, waiting for it up to waitLimit.
func (l *logLines) wait(t *testing.T, what string, match func(map[string]any) bool) map[string]any {
	t.Helper()
	var found map[string]any
	l.changes.wait(t, what, waitLimit, func() bool {
		found, _ = l.find(match)
		return found != nil
	})
	return found
}

// waitCount waits, up to limit, for the n-th line matching match.
func (l *logLines) waitCount(t *testing.T, what string, n int, limit time.Duration, match func(map[string]any) bool) {
	t.Helper()
	l.changes.wait(t, fmt.Sprintf("%s (%d lines)", what, n), limit, func() bool { return l.count(match) >= n })
}

// find returns the first line matching match logged so far, without waiting.
func (l *logLines) find(match func(map[string]any) bool) (map[string]any, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, entry := range l.lines {
		if match(entry) {
			return entry, true
		}
	}
	return nil, false
}

// count is how many lines so far match match.
func (l *logLines) count(match func(map[string]any) bool) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, entry := range l.lines {
		if match(entry) {
			n++
		}
	}
	return n
}

func (l *logLines) text() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.raw, "\n")
}

// listenerURL is the base URL of the listener a listening line names: its
// server.address and server.port.
func listenerURL(line map[string]any) string {
	return "http://" + net.JoinHostPort(fmt.Sprint(line["server.address"]), fmt.Sprint(line["server.port"]))
}

// msg matches a log line by message and string fields.
func msg(message string, fields ...string) func(map[string]any) bool {
	return func(entry map[string]any) bool {
		if entry["msg"] != message {
			return false
		}
		for i := 0; i+1 < len(fields); i += 2 {
			if fmt.Sprint(entry[fields[i]]) != fields[i+1] {
				return false
			}
		}
		return true
	}
}

// process is one running child process whose log the test reads.
type process struct {
	name   string // "gateway", "sample": names it in failures
	cmd    *exec.Cmd
	logs   *logLines
	exited chan struct{}
	err    error // Wait's result, set before exited closes
}

// start runs cmd with env on top of the test's environment, less the variables that
// must not reach a child: KAIAK_ (the test sets the child's own), OTEL_ (an exporter
// endpoint set for the shell) and npm's INIT_CWD (the sample resolves a relative
// config path against it). Its stdout and stderr are its log. It is killed at test
// cleanup if still running, and its log printed if the test failed.
func (p *process) start(t *testing.T, name string, cmd *exec.Cmd, env []string) {
	t.Helper()
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "KAIAK_") && !strings.HasPrefix(kv, "OTEL_") && !strings.HasPrefix(kv, "INIT_CWD=") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env, env...)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stdout, cmd.Stderr = w, w
	*p = process{name: name, cmd: cmd, logs: newLogLines(), exited: make(chan struct{})}
	err = cmd.Start()
	w.Close() // the child holds its own copy: the log ends when the child exits
	if err != nil {
		r.Close()
		t.Fatal(err)
	}
	readDone := make(chan struct{})
	go func() {
		p.logs.read(r)
		r.Close()
		close(readDone)
	}()
	go func() {
		p.err = cmd.Wait()
		<-readDone
		close(p.exited)
	}()
	t.Cleanup(func() {
		select {
		case <-p.exited:
		default:
			_ = cmd.Process.Kill()
			<-p.exited
		}
		if t.Failed() {
			t.Logf("%s log:\n%s", name, p.logs.text())
		}
	})
}

// signal sends sig to the process.
func (p *process) signal(t *testing.T, sig syscall.Signal) {
	t.Helper()
	if err := p.cmd.Process.Signal(sig); err != nil {
		t.Fatal(err)
	}
}

// stop sends SIGTERM and waits for the process to exit 0.
func (p *process) stop(t *testing.T) {
	t.Helper()
	p.signal(t, syscall.SIGTERM)
	p.waitExit(t)
}

// ended waits up to within for the process to end.
func (p *process) ended(t *testing.T, within time.Duration) {
	t.Helper()
	select {
	case <-p.exited:
	case <-time.After(within):
		t.Fatalf("%s did not exit within %s", p.name, within)
	}
}

// waitExit waits for the process to end and requires exit code 0 and no race report.
func (p *process) waitExit(t *testing.T) {
	t.Helper()
	p.ended(t, waitLimit)
	if p.err != nil {
		t.Fatalf("%s exited with %v", p.name, p.err)
	}
	if strings.Contains(p.logs.text(), "WARNING: DATA RACE") {
		t.Fatalf("the race detector reported a race in the %s", p.name)
	}
}

// exitCode waits up to within for the process to end and returns its exit code (0
// when it succeeded).
func (p *process) exitCode(t *testing.T, within time.Duration) int {
	t.Helper()
	p.ended(t, within)
	var exit *exec.ExitError
	switch {
	case p.err == nil:
		return 0
	case errors.As(p.err, &exit):
		return exit.ExitCode()
	}
	t.Fatalf("%s ended with %v", p.name, p.err)
	return 0
}

// gateway is one running kaiak process.
type gateway struct {
	process
	api   string // http://host:port
	admin string
}

// gatewayEnv is the environment every file-mode gateway gets on top of the caller's.
func gatewayEnv(configFile string) []string {
	return append(commonEnv(), "KAIAK_CONFIG_FILE="+configFile)
}

// controlEnv is the environment of a gateway in control-plane mode.
func controlEnv(controlURL, token string) []string {
	return append(commonEnv(), "KAIAK_CONTROL_URL="+controlURL, "KAIAK_CONTROL_TOKEN="+token)
}

// commonEnv is the environment every gateway gets.
func commonEnv() []string {
	return []string{
		"KAIAK_INSTANCE_ID=e2e",
		"KAIAK_LISTEN_ADDR=127.0.0.1:0",
		"KAIAK_ADMIN_ADDR=127.0.0.1:0",
		"KAIAK_LOG_FORMAT=json",
		"KAIAK_DRAIN_GRACE_MS=0",
		"KAIAK_DRAIN_TIMEOUT_MS=10000",
	}
}

// startGateway runs kaiak in file mode and returns once both listeners are bound and
// /readyz answers 200. The process is killed at test cleanup if still running.
func startGateway(t *testing.T, configFile string) *gateway {
	t.Helper()
	return startGatewayEnv(t, gatewayEnv(configFile))
}

// startGatewayEnv runs kaiak with env as startGateway does.
func startGatewayEnv(t *testing.T, env []string) *gateway {
	t.Helper()
	return startGatewayIn(t, "", env)
}

// startGatewayIn runs kaiak with env and working directory dir ("": the test's) as
// startGateway does.
func startGatewayIn(t *testing.T, dir string, env []string) *gateway {
	t.Helper()
	g := startKaiak(t, dir, env)
	g.api = listenerURL(g.logs.wait(t, "the API listener", msg("listening", "kaiak.listener.name", "api")))
	g.admin = listenerURL(g.logs.wait(t, "the admin listener", msg("listening", "kaiak.listener.name", "admin")))
	// Readiness is the config being loaded, which happens before the listeners bind
	// in both modes (control-plane mode exits when it boots without a config), so the
	// first probe answers 200.
	if status, body := g.get(t, "/readyz", ""); status != http.StatusOK {
		t.Fatalf("/readyz = %d %s, want 200", status, body)
	}
	return g
}

// startKaiak starts kaiak with env in working directory dir ("": the test's), reading
// its log, without waiting for it to listen.
func startKaiak(t *testing.T, dir string, env []string) *gateway {
	t.Helper()
	cmd := exec.Command(kaiakBin)
	cmd.Dir = dir
	g := &gateway{}
	g.start(t, "gateway", cmd, env)
	return g
}

// exitError returns the error the gateway logged as it stopped with an error.
func (g *gateway) exitError(t *testing.T) string {
	t.Helper()
	return g.logs.wait(t, "the exit error", msg("kaiak stopped with an error"))["exception.message"].(string)
}

// get sends a GET with key (may be empty): admin paths to the admin listener,
// everything else to the API.
func (g *gateway) get(t *testing.T, path, key string) (int, []byte) {
	t.Helper()
	base := g.api
	if path == "/readyz" || path == "/healthz" || path == "/metrics" {
		base = g.admin
	}
	req, err := http.NewRequest(http.MethodGet, base+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp := do(t, req)
	return resp.StatusCode, resp.body
}

// post sends a JSON body to the API with key and request ID (either may be empty).
func (g *gateway) post(t *testing.T, path, key, requestID string, body any) *response {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, g.api+path, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	if requestID != "" {
		req.Header.Set("X-Request-Id", requestID)
	}
	return do(t, req)
}

// postMessages sends a Messages request the way Anthropic's SDKs do: the key in
// x-api-key, an anthropic-version header.
func (g *gateway) postMessages(t *testing.T, path, key, requestID string, body any) *response {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, g.api+path, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Key", key)
	req.Header.Set("Anthropic-Version", "2023-06-01")
	req.Header.Set("X-Request-Id", requestID)
	return do(t, req)
}

// settled waits for the log line of the request with id — written as the request's
// last act, after its usage record and limit reservation settled.
func (g *gateway) settled(t *testing.T, id string) map[string]any {
	t.Helper()
	return g.logs.wait(t, "request "+id, msg("request", "kaiak.request.id", id))
}

// metric returns the value of one series in the admin /metrics text (name plus
// labels exactly as exposed), failing when it is absent.
func (g *gateway) metric(t *testing.T, series string) float64 {
	t.Helper()
	v, ok := g.metricValue(t, series)
	if !ok {
		t.Fatalf("metric %s not exposed", series)
	}
	return v
}

// metricValue returns the value of one series, and false when it is absent.
func (g *gateway) metricValue(t *testing.T, series string) (float64, bool) {
	t.Helper()
	status, body := g.get(t, "/metrics", "")
	if status != http.StatusOK {
		t.Fatalf("/metrics = %d", status)
	}
	for line := range strings.SplitSeq(string(body), "\n") {
		if value, ok := strings.CutPrefix(line, series+" "); ok {
			v, err := strconv.ParseFloat(value, 64)
			if err != nil {
				t.Fatalf("metric %s: %v", series, err)
			}
			return v, true
		}
	}
	return 0, false
}

// pollEvery is how often poll looks again.
const pollEvery = 50 * time.Millisecond

// poll calls check until it returns true, up to limit: state inside another process
// with no event to wait on.
func poll(limit time.Duration, check func() bool) bool {
	deadline := time.Now().Add(limit)
	for !check() {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(pollEvery)
	}
	return true
}

// pollUntil polls check, failing the test when it is not true within limit.
func pollUntil(t *testing.T, what string, limit time.Duration, check func() bool) {
	t.Helper()
	if !poll(limit, check) {
		t.Fatalf("%s: not seen within %s", what, limit)
	}
}

// waitMetric polls one series until match accepts its value (absent is not
// accepted), up to waitLimit.
func (g *gateway) waitMetric(t *testing.T, what, series string, match func(float64) bool) float64 {
	t.Helper()
	return g.waitMetricWithin(t, what, series, waitLimit, match)
}

// recoverLimit bounds the waits after the control plane returns: a gateway's
// reconnect delay grew during the outage, up to the 30 s backoff cap.
const recoverLimit = 40 * time.Second

// waitMetricWithin is waitMetric with its own bound.
func (g *gateway) waitMetricWithin(t *testing.T, what, series string, limit time.Duration, match func(float64) bool) float64 {
	t.Helper()
	var v float64
	if !poll(limit, func() bool {
		var ok bool
		v, ok = g.metricValue(t, series)
		return ok && match(v)
	}) {
		t.Fatalf("%s: %s never matched", what, series)
	}
	return v
}

// response is a finished HTTP exchange, body read.
type response struct {
	*http.Response
	body []byte
}

func do(t *testing.T, req *http.Request) *response {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return &response{Response: resp, body: body}
}

// json decodes the body into a generic map.
func (r *response) json(t *testing.T) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal(r.body, &v); err != nil {
		t.Fatalf("body is not a JSON object: %v\n%s", err, r.body)
	}
	return v
}

// errorCode is the OpenAI error body's code.
func (r *response) errorCode(t *testing.T) string {
	t.Helper()
	e, _ := r.json(t)["error"].(map[string]any)
	return fmt.Sprint(e["code"])
}

// events splits an SSE body into its data payloads.
func events(body []byte) []string {
	var out []string
	for block := range strings.SplitSeq(strings.ReplaceAll(string(body), "\r\n", "\n"), "\n\n") {
		for line := range strings.SplitSeq(block, "\n") {
			if data, ok := strings.CutPrefix(line, "data: "); ok {
				out = append(out, data)
			}
		}
	}
	return out
}

// newKey mints a client key and its config hash.
func newKey() (key, hash string) {
	raw := make([]byte, 24)
	_, _ = rand.Read(raw) // crypto/rand.Read never fails
	key = "kaiak-" + hex.EncodeToString(raw)
	return key, auth.KeyHash(key)
}

// writeJSON writes v to path as JSON.
func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// isConnRefused reports whether err is a refused TCP connection.
func isConnRefused(err error) bool { return errors.Is(err, syscall.ECONNREFUSED) }
