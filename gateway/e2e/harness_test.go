package e2e

// The harness: builds the kaiak binary once, runs it as a subprocess with a config
// file, a data directory and environment, and reads its JSON log lines — the log is
// how the test learns the bound ports and waits for events (config applied, request
// settled, draining), never fixed sleeps.

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/sha256"
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

// logLines collects the gateway's log, one JSON object per line.
type logLines struct {
	mu      sync.Mutex
	raw     []string
	lines   []map[string]any
	changed chan struct{} // closed and replaced on every new line and at EOF
	eof     bool
}

func newLogLines() *logLines { return &logLines{changed: make(chan struct{})} }

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
		close(l.changed)
		l.changed = make(chan struct{})
		l.mu.Unlock()
	}
	l.mu.Lock()
	l.eof = true
	close(l.changed)
	l.changed = make(chan struct{})
	l.mu.Unlock()
}

// wait returns the first line matching match, waiting for it up to waitLimit.
func (l *logLines) wait(t *testing.T, what string, match func(map[string]any) bool) map[string]any {
	t.Helper()
	deadline := time.NewTimer(waitLimit)
	defer deadline.Stop()
	for {
		l.mu.Lock()
		for _, entry := range l.lines {
			if match(entry) {
				l.mu.Unlock()
				return entry
			}
		}
		changed, eof := l.changed, l.eof
		l.mu.Unlock()
		if eof {
			t.Fatalf("gateway exited before logging %s", what)
		}
		select {
		case <-changed:
		case <-deadline.C:
			t.Fatalf("gateway did not log %s within %s", what, waitLimit)
		}
	}
}

// waitCount waits, up to limit, for the n-th line matching match.
func (l *logLines) waitCount(t *testing.T, what string, n int, limit time.Duration, match func(map[string]any) bool) {
	t.Helper()
	deadline := time.NewTimer(limit)
	defer deadline.Stop()
	for {
		l.mu.Lock()
		seen := 0
		for _, entry := range l.lines {
			if match(entry) {
				seen++
			}
		}
		changed, eof := l.changed, l.eof
		l.mu.Unlock()
		switch {
		case seen >= n:
			return
		case eof:
			t.Fatalf("process exited before logging %s", what)
		}
		select {
		case <-changed:
		case <-deadline.C:
			t.Fatalf("%s not logged within %s (%d of %d)", what, limit, seen, n)
		}
	}
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

// gateway is one running kaiak process.
type gateway struct {
	cmd    *exec.Cmd
	logs   *logLines
	api    string // http://host:port
	admin  string
	exited chan struct{}
	err    error // Wait's result, set before exited closes
}

// gatewayEnv is the environment every file-mode gateway gets on top of the caller's.
func gatewayEnv(configFile, dataDir string) []string {
	return append(commonEnv(dataDir), "KAIAK_CONFIG_FILE="+configFile)
}

// controlEnv is the environment of a gateway in control-plane mode.
func controlEnv(controlURL, token, dataDir string) []string {
	return append(commonEnv(dataDir), "KAIAK_CONTROL_URL="+controlURL, "KAIAK_CONTROL_TOKEN="+token)
}

// commonEnv is the environment every gateway gets; dataDir "" leaves the data
// directory unset (nothing written).
func commonEnv(dataDir string) []string {
	env := []string{
		"KAIAK_INSTANCE_ID=e2e",
		"KAIAK_LISTEN_ADDR=127.0.0.1:0",
		"KAIAK_ADMIN_ADDR=127.0.0.1:0",
		"KAIAK_LOG_FORMAT=json",
		"KAIAK_DRAIN_GRACE_MS=0",
		"KAIAK_DRAIN_TIMEOUT_MS=10000",
	}
	if dataDir != "" {
		env = append(env, "KAIAK_DATA_DIR="+dataDir)
	}
	return env
}

// startGateway runs kaiak in file mode and returns once both listeners are bound and
// /readyz answers 200. The process is killed at test cleanup if still running.
func startGateway(t *testing.T, configFile, dataDir string) *gateway {
	t.Helper()
	return startGatewayEnv(t, gatewayEnv(configFile, dataDir))
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
	g := startProcess(t, dir, env)
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

// startProcess starts kaiak with env (and none of the test process's KAIAK_ or OTEL_
// variables: an exporter endpoint set for the shell must not reach the gateways) in
// working directory dir ("": the test's), reading its log. The process is killed at
// test cleanup if still running.
func startProcess(t *testing.T, dir string, env []string) *gateway {
	t.Helper()
	cmd := exec.Command(kaiakBin)
	cmd.Dir = dir
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "KAIAK_") && !strings.HasPrefix(kv, "OTEL_") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env, env...)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	g := &gateway{cmd: cmd, logs: newLogLines(), exited: make(chan struct{})}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	readDone := make(chan struct{})
	go func() {
		g.logs.read(stderr)
		close(readDone)
	}()
	go func() {
		<-readDone // Wait closes the pipe: read everything first
		g.err = cmd.Wait()
		close(g.exited)
	}()
	t.Cleanup(func() {
		select {
		case <-g.exited:
		default:
			_ = cmd.Process.Kill()
			<-g.exited
		}
		if t.Failed() {
			t.Logf("gateway log:\n%s", g.logs.text())
		}
	})
	return g
}

// signal sends sig to the gateway process.
func (g *gateway) signal(t *testing.T, sig syscall.Signal) {
	t.Helper()
	if err := g.cmd.Process.Signal(sig); err != nil {
		t.Fatal(err)
	}
}

// stop sends SIGTERM and waits for the process to exit 0.
func (g *gateway) stop(t *testing.T) {
	t.Helper()
	g.signal(t, syscall.SIGTERM)
	g.waitExit(t)
}

// waitExit waits for the process to end and requires exit code 0 and no race report.
func (g *gateway) waitExit(t *testing.T) {
	t.Helper()
	select {
	case <-g.exited:
	case <-time.After(waitLimit):
		t.Fatalf("gateway did not exit within %s", waitLimit)
	}
	if g.err != nil {
		t.Fatalf("gateway exited with %v", g.err)
	}
	if strings.Contains(g.logs.text(), "WARNING: DATA RACE") {
		t.Fatal("the race detector reported a race in the gateway")
	}
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

// metricPoll is how often waitMetric reads /metrics.
const metricPoll = 50 * time.Millisecond

// waitMetric reads one series until match accepts its value (absent is not
// accepted), up to waitLimit: state inside another process has no event to wait on.
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
	deadline := time.Now().Add(limit)
	for {
		if v, ok := g.metricValue(t, series); ok && match(v) {
			return v
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: %s never matched", what, series)
		}
		time.Sleep(metricPoll)
	}
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
	sum := sha256.Sum256([]byte(key))
	return key, "sha256:" + hex.EncodeToString(sum[:])
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
