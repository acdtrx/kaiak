// Command live runs the kaiak gateway against a real backend (vLLM, Azure OpenAI or
// OpenAI) and checks it end to end, printing PASS/FAIL per check. It generates the
// config (with a fresh client key), starts the built kaiak binary, drives it over HTTP
// and reads its log and metrics. With -base-url-2 a second backend of the same kind
// serves the same model: the models get two deployments, and the reliability checks
// run (load spread, the concurrency cap, and with -check-failover a failover the user
// drives by stopping and restarting the second backend). With -embeddings-base-url
// the embeddings model is served by a server of its own (openai-compatible), a third
// backend in the config. -self-test runs every check against the fake backend
// standing in for each kind, and the two-backend checks against two fakes, stopping
// and restarting the second itself, with a third fake as the embeddings server.
//
// Standard library only, and a module of its own: nothing here is part of the
// gateway. Runbook: docs/testing/LIVE-BACKENDS.md.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

// Backend kinds.
const (
	kindVLLM   = "vllm"
	kindAzure  = "azure-openai"
	kindOpenAI = "openai"
)

var allKinds = []string{kindVLLM, kindOpenAI, kindAzure}

// options is everything a run needs, from flags and environment.
type options struct {
	kind            string
	baseURL         string
	baseURL2        string
	model           string
	embeddingsModel string
	// embeddingsBaseURL, when set, is a separate openai-compatible server for the
	// embeddings model, authenticated by embeddingsAPIKeyEnv ("" = no key).
	embeddingsBaseURL   string
	embeddingsAPIKeyEnv string
	apiKeyEnv           string
	maxOutput           int
	ceiling             int
	contextLength       int
	priceIn             float64
	priceOut            float64
	chatDefaults        string
	kaiakBin            string
	requestTimeout      time.Duration
	keep                bool
	verbose             bool

	// Two backends (-base-url-2).
	maxInFlight   int
	checkFailover bool
	failoverWait  time.Duration
	// failoverPace is the gap between the failover check's requests.
	failoverPace time.Duration
	// stopSecond and startSecond stop and restart the second backend; nil in a live
	// run, where the check asks the user to do it.
	stopSecond, startSecond func() error
}

// twoBackends: a second backend serves the model.
func (o options) twoBackends() bool { return o.baseURL2 != "" }

// embeddingsServer: the embeddings model has a server of its own.
func (o options) embeddingsServer() bool { return o.embeddingsBaseURL != "" }

// label names the run's files and gateway instance.
func (o options) label() string {
	if o.twoBackends() {
		return o.kind + "-two"
	}
	return o.kind
}

// defaultAPIKeyEnv is the variable holding the backend key when -api-key-env is not
// given: vLLM usually runs without one.
var defaultAPIKeyEnv = map[string]string{kindVLLM: "", kindOpenAI: "OPENAI_API_KEY", kindAzure: "AZURE_OPENAI_API_KEY"}

func main() {
	var o options
	selfTest := flag.Bool("self-test", false, "run every check against the fake backend standing in for each kind (or only -kind)")
	flag.StringVar(&o.kind, "kind", "", "backend kind: vllm, azure-openai or openai")
	flag.StringVar(&o.baseURL, "base-url", os.Getenv("LIVE_BASE_URL"), "backend base URL (env LIVE_BASE_URL): vLLM http://host:8000/v1; OpenAI https://api.openai.com/v1 (the default); Azure the resource endpoint https://<resource>.openai.azure.com")
	flag.StringVar(&o.baseURL2, "base-url-2", os.Getenv("LIVE_BASE_URL_2"), "a second backend of the same kind serving the same -model (env LIVE_BASE_URL_2): two deployments, and the load-spread, cap and failover checks")
	flag.IntVar(&o.maxInFlight, "max-in-flight", 0, "with -base-url-2: each backend's max_in_flight (0: no cap); adds the capacity check")
	flag.BoolVar(&o.checkFailover, "check-failover", false, "with -base-url-2: the failover check — stop the second backend when asked, start it again when asked")
	flag.DurationVar(&o.failoverWait, "failover-wait", 10*time.Minute, "how long the failover check waits for each step (the backend stopped, the backend back)")
	flag.StringVar(&o.model, "model", os.Getenv("LIVE_MODEL"), "chat model name on the backend (env LIVE_MODEL); for Azure the deployment name")
	flag.StringVar(&o.embeddingsModel, "embeddings-model", os.Getenv("LIVE_EMBEDDINGS_MODEL"), "embeddings model name on the backend, optional (env LIVE_EMBEDDINGS_MODEL)")
	flag.StringVar(&o.embeddingsBaseURL, "embeddings-base-url", os.Getenv("LIVE_EMBEDDINGS_BASE_URL"), "a separate openai-compatible server for -embeddings-model (env LIVE_EMBEDDINGS_BASE_URL), e.g. http://host:8003/v1; default: -base-url serves it")
	flag.StringVar(&o.embeddingsAPIKeyEnv, "embeddings-api-key-env", "", "with -embeddings-base-url: name of the environment variable holding that server's key (default: none)")
	flag.StringVar(&o.apiKeyEnv, "api-key-env", "-", "name of the environment variable holding the backend key (default: none for vllm, OPENAI_API_KEY, AZURE_OPENAI_API_KEY)")
	flag.IntVar(&o.maxOutput, "max-output", 1024, "output-limit default and ceiling of the chat model (raise it for reasoning models)")
	flag.IntVar(&o.ceiling, "ceiling", 16, "output-limit ceiling of the model the ceiling check uses")
	flag.IntVar(&o.contextLength, "context-length", 32768, "declared context length of the chat model")
	flag.Float64Var(&o.priceIn, "price-in", 1, "USD per million input tokens (0 with -price-out 0: unpriced, cost check skipped)")
	flag.Float64Var(&o.priceOut, "price-out", 2, "USD per million output tokens")
	flag.StringVar(&o.chatDefaults, "chat-defaults", "", `extra request defaults for the chat models, a JSON object (e.g. '{"chat_template_kwargs":{"enable_thinking":false}}')`)
	flag.StringVar(&o.kaiakBin, "kaiak", "", "kaiak binary to run (default: built from this repository)")
	flag.DurationVar(&o.requestTimeout, "request-timeout", 2*time.Minute, "time allowed for each request")
	flag.BoolVar(&o.keep, "keep", false, "keep the temporary directory (config, gateway log) and print its path")
	flag.BoolVar(&o.verbose, "v", false, "echo the gateway's log")
	flag.Parse()
	o.failoverPace = time.Second

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	// Output piped into a reader that quits early (| head) must not kill the runner
	// before it stops the gateway and removes its files: writes fail instead.
	signal.Ignore(syscall.SIGPIPE)

	var err error
	if *selfTest {
		err = runSelfTest(ctx, o)
	} else {
		err = runLive(ctx, o)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "live:", err)
		cancel()
		os.Exit(1)
	}
}

// errChecksFailed: the run completed and at least one check failed.
var errChecksFailed = errors.New("checks failed")

// runLive runs the checks against the backend the options name.
func runLive(ctx context.Context, o options) error {
	if err := resolve(&o); err != nil {
		return err
	}
	ws, err := newWorkspace(o.keep)
	if err != nil {
		return err
	}
	defer ws.close()
	if o.kaiakBin == "" {
		if o.kaiakBin, err = ws.build("kaiak", "./cmd/kaiak"); err != nil {
			return err
		}
	}
	return runKind(ctx, o, ws, nil)
}

// resolve checks the options and fills per-kind defaults.
func resolve(o *options) error {
	if _, ok := defaultAPIKeyEnv[o.kind]; !ok {
		return fmt.Errorf("-kind %q: want one of %s", o.kind, strings.Join(allKinds, ", "))
	}
	if o.apiKeyEnv == "-" {
		o.apiKeyEnv = defaultAPIKeyEnv[o.kind]
	}
	if o.kind == kindAzure && o.apiKeyEnv == "" {
		return errors.New("azure-openai needs -api-key-env: Azure authenticates with an api-key header")
	}
	if o.apiKeyEnv != "" && os.Getenv(o.apiKeyEnv) == "" {
		return fmt.Errorf("%s is not set: export the backend key there, or name another variable with -api-key-env", o.apiKeyEnv)
	}
	if o.baseURL == "" && o.kind == kindOpenAI {
		o.baseURL = "https://api.openai.com/v1"
	}
	if o.baseURL == "" {
		return errors.New("no backend URL: set -base-url or LIVE_BASE_URL")
	}
	o.baseURL = normalizeBaseURL(o.kind, o.baseURL)
	if o.twoBackends() {
		o.baseURL2 = normalizeBaseURL(o.kind, o.baseURL2)
		if o.baseURL2 == o.baseURL {
			return errors.New("-base-url-2 is -base-url: the second backend must be another process (another host or port)")
		}
	}
	if !o.twoBackends() && (o.maxInFlight != 0 || o.checkFailover) {
		return errors.New("-max-in-flight and -check-failover need -base-url-2")
	}
	if o.maxInFlight < 0 {
		return errors.New("-max-in-flight: want 0 (no cap) or more")
	}
	if o.embeddingsServer() {
		if o.embeddingsModel == "" {
			return errors.New("-embeddings-base-url needs -embeddings-model: the model name on that server")
		}
		o.embeddingsBaseURL = normalizeBaseURL(kindVLLM, o.embeddingsBaseURL)
	}
	if o.embeddingsAPIKeyEnv != "" {
		if !o.embeddingsServer() {
			return errors.New("-embeddings-api-key-env needs -embeddings-base-url")
		}
		if os.Getenv(o.embeddingsAPIKeyEnv) == "" {
			return fmt.Errorf("%s is not set: export the embeddings server's key there", o.embeddingsAPIKeyEnv)
		}
	}
	if o.model == "" {
		return errors.New("no model: set -model or LIVE_MODEL (for Azure, the deployment name)")
	}
	if o.maxOutput < 1 || o.ceiling < 1 || o.maxOutput > o.contextLength || o.ceiling > o.contextLength {
		return errors.New("-max-output and -ceiling must be at least 1 and at most -context-length")
	}
	return nil
}

// normalizeBaseURL trims a trailing slash and, for Azure, a trailing /openai/v1,
// with a note about URLs that look wrong for the kind.
func normalizeBaseURL(kind, u string) string {
	u = strings.TrimRight(u, "/")
	switch {
	case kind == kindAzure && strings.HasSuffix(u, "/openai/v1"):
		u = strings.TrimSuffix(u, "/openai/v1")
		fmt.Printf("note: Azure base URL is the resource endpoint; using %s (the gateway appends /openai/v1/)\n", u)
	case kind != kindAzure && !strings.HasSuffix(u, "/v1"):
		fmt.Printf("note: %s base URL %s does not end in /v1; an OpenAI client's base URL usually does\n", kind, u)
	}
	return u
}

// runKind generates the config, starts the gateway, runs the checks and prints the
// report. extraEnv is added to the gateway's environment (the self-test's fake key).
func runKind(ctx context.Context, o options, ws *workspace, extraEnv []string) error {
	key, hash := newKey()
	cfg, err := buildConfig(o, hash)
	if err != nil {
		return err
	}
	configFile, err := ws.writeConfig(o.label(), cfg)
	if err != nil {
		return err
	}
	fmt.Printf("== %s: %s", o.kind, o.baseURL)
	if o.twoBackends() {
		fmt.Printf(" and %s", o.baseURL2)
	}
	fmt.Printf(", model %s", o.model)
	if o.embeddingsModel != "" {
		fmt.Printf(", embeddings %s", o.embeddingsModel)
		if o.embeddingsServer() {
			fmt.Printf(" on %s", o.embeddingsBaseURL)
		}
	}
	fmt.Println()

	gw, err := startGateway(ctx, o, ws, configFile, extraEnv)
	if err != nil {
		return err
	}
	defer gw.stop()

	r := &run{ctx: ctx, o: o, gw: gw, key: key, client: newClient(o.requestTimeout)}
	r.checks()
	gw.stop()
	if !gw.cleanExit() {
		r.fail("gateway-exit", "the gateway did not drain and exit 0 on SIGTERM (see its log)")
	}
	return r.report()
}
