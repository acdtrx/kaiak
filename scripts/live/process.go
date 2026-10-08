package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
)

// startupLimit bounds how long the gateway (or the fake backend) may take to come up,
// and how long a log line may take to appear after its request ended.
const startupLimit = 30 * time.Second

// gateway is a running kaiak binary.
type gateway struct {
	cmd      *exec.Cmd
	logs     *logLines
	api      string // http://host:port
	admin    string
	exited   chan struct{}
	exitErr  error
	stopOnce sync.Once
}

func startGateway(ctx context.Context, o options, ws *workspace, configFile string, extraEnv []string) (*gateway, error) {
	g := &gateway{logs: newLogLines(), exited: make(chan struct{})}
	g.cmd = exec.Command(o.kaiakBin)
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "KAIAK_") {
			g.cmd.Env = append(g.cmd.Env, kv)
		}
	}
	g.cmd.Env = append(g.cmd.Env,
		"KAIAK_CONFIG_FILE="+configFile,
		"KAIAK_LISTEN_ADDR=127.0.0.1:0",
		"KAIAK_ADMIN_ADDR=127.0.0.1:0",
		"KAIAK_LOG_FORMAT=json",
		"KAIAK_INSTANCE_ID=live-"+o.label(),
		"KAIAK_DRAIN_GRACE_MS=0",
		"KAIAK_DRAIN_TIMEOUT_MS=10000")
	g.cmd.Env = append(g.cmd.Env, extraEnv...)

	stderr, err := g.cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	logFile := filepath.Join(ws.dir, o.label()+".kaiak.log")
	log, err := os.Create(logFile)
	if err != nil {
		return nil, err
	}
	if err := g.cmd.Start(); err != nil {
		log.Close()
		return nil, fmt.Errorf("starting the gateway: %w", err)
	}
	var echo io.Writer = io.Discard
	if o.verbose {
		echo = os.Stderr
	}
	readDone := make(chan struct{})
	go func() {
		g.logs.read(io.TeeReader(stderr, io.MultiWriter(log, echo)))
		log.Close()
		close(readDone)
	}()
	go func() {
		<-readDone // Wait closes the pipe: read everything first
		g.exitErr = g.cmd.Wait()
		close(g.exited)
	}()

	if err := g.waitReady(ctx); err != nil {
		g.stop()
		return nil, fmt.Errorf("%w\ngateway log (%s):\n%s", err, logFile, g.logs.text())
	}
	return g, nil
}

// waitReady learns the listener addresses from the log and checks /readyz.
func (g *gateway) waitReady(ctx context.Context) error {
	api, err := g.logs.wait(ctx, msg("listening", "kaiak.listener.name", "api"))
	if err != nil {
		return fmt.Errorf("gateway did not start: %w", err)
	}
	admin, err := g.logs.wait(ctx, msg("listening", "kaiak.listener.name", "admin"))
	if err != nil {
		return fmt.Errorf("gateway did not start: %w", err)
	}
	g.api, g.admin = listenerURL(api), listenerURL(admin)
	// The config is loaded before the listeners bind, so the gateway is ready now.
	status, body, err := httpGet(ctx, newClient(10*time.Second), g.admin+"/readyz", "")
	if err != nil || status != 200 {
		return fmt.Errorf("/readyz = %d %s %v", status, body, err)
	}
	return nil
}

// stop sends SIGTERM and waits for the drain and exit, killing the process if they
// do not end in time. Safe to call more than once.
func (g *gateway) stop() {
	g.stopOnce.Do(func() {
		_ = g.cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-g.exited:
		case <-time.After(20 * time.Second):
			_ = g.cmd.Process.Kill()
			<-g.exited
		}
	})
}

// cleanExit: the gateway drained and exited 0.
func (g *gateway) cleanExit() bool {
	select {
	case <-g.exited:
		return g.exitErr == nil
	default:
		return false
	}
}

// logLines collects the gateway's JSON log lines.
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
		var entry map[string]any
		_ = json.Unmarshal(sc.Bytes(), &entry) // non-JSON lines stay in raw
		l.mu.Lock()
		l.raw = append(l.raw, sc.Text())
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
	l.mu.Unlock()
}

var errLogTimeout = errors.New("not logged in time")

// wait returns the first line matching match, waiting up to startupLimit.
func (l *logLines) wait(ctx context.Context, match func(map[string]any) bool) (map[string]any, error) {
	return l.waitWithin(ctx, startupLimit, match)
}

// waitWithin returns the first line matching match, waiting up to limit.
func (l *logLines) waitWithin(ctx context.Context, limit time.Duration, match func(map[string]any) bool) (map[string]any, error) {
	deadline := time.NewTimer(limit)
	defer deadline.Stop()
	for {
		l.mu.Lock()
		for _, entry := range l.lines {
			if match(entry) {
				l.mu.Unlock()
				return entry, nil
			}
		}
		changed, eof := l.changed, l.eof
		l.mu.Unlock()
		if eof {
			return nil, errors.New("the gateway exited")
		}
		select {
		case <-changed:
		case <-deadline.C:
			return nil, errLogTimeout
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// find returns the first line matching match so far, without waiting.
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

// msg matches a log line by message and fields.
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

// runSelfTest runs every check against the fake backend standing in for each kind
// (or only o.kind) — with a second fake as the reranker server, with a key, where the
// kind serves rerank — then, when vllm is among them, the two-backend checks against
// two fakes, the failover check stopping and restarting the second: the kit's own
// acceptance test, no real backend needed.
func runSelfTest(ctx context.Context, o options) error {
	// The reranker server is the self-test's own fake, whatever the flags or the
	// environment name.
	o.rerankBaseURL, o.rerankModel, o.rerankAPIKeyEnv = "", "", ""
	kinds := allKinds
	if o.kind != "" {
		if _, ok := defaultAPIKeyEnv[o.kind]; !ok {
			return fmt.Errorf("-kind %q: want one of %s", o.kind, strings.Join(allKinds, ", "))
		}
		kinds = []string{o.kind}
	}
	ws, err := newWorkspace(o.keep)
	if err != nil {
		return err
	}
	defer ws.close()
	fakeBin, err := ws.build("fakebackend", "./internal/fakebackend/cmd/fakebackend")
	if err != nil {
		return err
	}
	if o.kaiakBin == "" {
		if o.kaiakBin, err = ws.build("kaiak", "./cmd/kaiak"); err != nil {
			return err
		}
	}

	var failed []string
	for _, kind := range kinds {
		k := o
		k.kind = kind
		k.model, k.embeddingsModel = selfTestChatModel, selfTestEmbedModel
		if !k.serves(epEmbeddings) {
			k.embeddingsModel = ""
		}
		k.apiKeyEnv = defaultAPIKeyEnv[kind]
		k.requestTimeout = 10 * time.Second
		secret, _ := newKey()
		auth := map[string]string{kindVLLM: "none", kindLlamaServer: "none", kindOpenAI: "bearer", kindAzure: "api-key",
			kindAnthropic: "x-api-key", kindAzureAnthropic: "api-key"}[kind]
		fake, root, err := startFake(ctx, fakeBin, "127.0.0.1:0", auth, secret,
			[]string{selfTestChatModel, selfTestEmbedModel}, fakeFlags(kind, false)...)
		if err != nil {
			return err
		}
		k.baseURL = root + "/v1"
		if _, azure := azureLayouts[kind]; azure {
			k.baseURL = root
		}
		var extraEnv []string
		if k.apiKeyEnv != "" {
			extraEnv = []string{k.apiKeyEnv + "=" + secret}
		}
		var reranker *exec.Cmd
		if k.serves(epRerank) {
			const rerankKeyEnv = "LIVE_SELF_TEST_RERANK_KEY"
			rerankSecret, _ := newKey()
			var rootRerank string
			reranker, rootRerank, err = startFake(ctx, fakeBin, "127.0.0.1:0", "bearer", rerankSecret,
				[]string{selfTestRerankModel}, fakeFlags(kind, true)...)
			if err != nil {
				stopFake(fake)
				return err
			}
			k.rerankBaseURL, k.rerankModel, k.rerankAPIKeyEnv = rootRerank+"/v1", selfTestRerankModel, rerankKeyEnv
			extraEnv = append(extraEnv, rerankKeyEnv+"="+rerankSecret)
		}
		err = runKind(ctx, k, ws, extraEnv)
		stopFake(fake)
		stopFake(reranker)
		switch {
		case errors.Is(err, errChecksFailed):
			failed = append(failed, kind)
		case err != nil:
			return err
		}
	}
	if slices.Contains(kinds, kindVLLM) {
		kinds = append(kinds, kindVLLM+" with two backends")
		switch err := selfTestTwoBackends(ctx, o, ws, fakeBin); {
		case errors.Is(err, errChecksFailed):
			failed = append(failed, kindVLLM+" with two backends")
		case err != nil:
			return err
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("self-test failed for %s", strings.Join(failed, ", "))
	}
	fmt.Printf("self-test passed for %s\n", strings.Join(kinds, ", "))
	return nil
}

// selfTestTwoBackends runs the vllm checks against two fakes serving the same model,
// each capped at one request, with the failover check: the second fake is stopped,
// then started again on its address. A third fake is the embeddings server, its
// model a path-style name the way llama-server lists a model file.
func selfTestTwoBackends(ctx context.Context, o options, ws *workspace, fakeBin string) error {
	o.kind, o.apiKeyEnv = kindVLLM, ""
	o.model, o.embeddingsModel = selfTestChatModel, selfTestEmbedPath
	o.requestTimeout = 10 * time.Second
	o.maxInFlight, o.checkFailover = 1, true
	o.failoverWait, o.failoverPace = startupLimit, 100*time.Millisecond

	chat := []string{selfTestChatModel}
	first, root, err := startFake(ctx, fakeBin, "127.0.0.1:0", "none", "", chat, fakeFlags(kindVLLM, false)...)
	if err != nil {
		return err
	}
	defer stopFake(first)
	second, root2, err := startFake(ctx, fakeBin, "127.0.0.1:0", "none", "", chat, fakeFlags(kindVLLM, false)...)
	if err != nil {
		return err
	}
	defer func() { stopFake(second) }()
	embedSecret, _ := newKey()
	embeddings, rootEmbed, err := startFake(ctx, fakeBin, "127.0.0.1:0", "bearer", embedSecret, []string{selfTestEmbedPath})
	if err != nil {
		return err
	}
	defer stopFake(embeddings)
	const embedKeyEnv = "LIVE_SELF_TEST_EMBEDDINGS_KEY"
	o.embeddingsBaseURL, o.embeddingsAPIKeyEnv = rootEmbed+"/v1", embedKeyEnv
	o.baseURL, o.baseURL2 = root+"/v1", root2+"/v1"
	o.stopSecond = func() error {
		stopFake(second)
		return nil
	}
	o.startSecond = func() error {
		var err error
		second, _, err = startFake(ctx, fakeBin, strings.TrimPrefix(root2, "http://"), "none", "", chat, fakeFlags(kindVLLM, false)...)
		return err
	}
	return runKind(ctx, o, ws, []string{embedKeyEnv + "=" + embedSecret})
}

// stopFake stops a fake backend process and waits for it; a stopped one is left as
// it is.
func stopFake(cmd *exec.Cmd) {
	if cmd == nil || cmd.ProcessState != nil {
		return
	}
	_ = cmd.Process.Signal(syscall.SIGTERM)
	_ = cmd.Wait()
}

// The model names the self-test deploys; its fakes list them (the gateway's probe
// requires a deployment's model in the backend's models list).
const (
	selfTestChatModel   = "fake-chat"
	selfTestEmbedModel  = "fake-embed"
	selfTestRerankModel = "fake-rerank"
	// selfTestEmbedPath is the embeddings server's model: a path, not a valid public
	// name, so the kit must expose it under its own.
	selfTestEmbedPath = "/models/fake-embed-q8_0.gguf"
)

// selfTestCachedTokens is how many prompt tokens the fakes report read from the cache,
// so the cache check sees a prefix cache answering.
const selfTestCachedTokens = "3"

// selfTestRerankContext is the reranker fakes' context, in words: the oversize check's
// document is longer.
const selfTestRerankContext = "32768"

// fakeFlags are the fake backend's flags playing kind's server for the run's chat
// model, or with reranker for the reranker: rerank in that server's shape, and the
// endpoints its model lacks answered as the server answers them — vLLM creates no
// route for them, llama-server answers rerank with 501 unless started with
// --reranking. Other kinds: none.
func fakeFlags(kind string, reranker bool) []string {
	switch {
	case kind == kindVLLM && reranker:
		return []string{"-rerank-shape", "vllm", "-rerank-context", selfTestRerankContext,
			"-no-route", "chat/completions,completions,embeddings,messages,messages/count_tokens,responses"}
	case kind == kindVLLM:
		return []string{"-no-route", "rerank"}
	case kind == kindLlamaServer && reranker:
		return []string{"-rerank-shape", "llama-server", "-rerank-context", selfTestRerankContext}
	case kind == kindLlamaServer:
		return []string{"-not-supported", "rerank"}
	}
	return nil
}

// startFake runs the fake backend on addr (port 0: a free one), listing models, with
// flags added, and returns its root URL.
func startFake(ctx context.Context, bin, addr, auth, key string, models []string, flags ...string) (*exec.Cmd, string, error) {
	args := append([]string{"-addr", addr, "-auth", auth, "-key", key, "-quiet",
		"-cached-tokens", selfTestCachedTokens, "-models", strings.Join(models, ",")}, flags...)
	cmd := exec.Command(bin, args...)
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, "", err
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, "", err
	}
	line := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(out)
		if sc.Scan() {
			line <- sc.Text()
		}
		close(line)
		_, _ = io.Copy(io.Discard, out)
	}()
	select {
	case l, ok := <-line:
		root, found := strings.CutPrefix(l, "listening ")
		if ok && found {
			return cmd, root, nil
		}
	case <-time.After(startupLimit):
	case <-ctx.Done():
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	return nil, "", errors.New("the fake backend did not start")
}
