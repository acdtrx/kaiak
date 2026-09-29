package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// Public model names in the generated config. All chat names route to the same
// backend model; they differ only in output limit and limits.
const (
	modelChat   = "live-chat"   // output limit -max-output
	modelCapped = "live-capped" // output-limit ceiling -ceiling
	modelRPM    = "live-rpm"    // 1 request per minute for the key's group
	modelEmbed  = "live-embed"  // only with -embeddings-model (whatever its name on the backend)
)

// Backend IDs in the generated config: the -base-url backend, the -base-url-2
// backend when there is one, and the -embeddings-base-url backend when there is one.
const (
	backendFirst  = "live"
	backendSecond = "live-2"
	backendEmbed  = "live-embeddings"
)

// Circuit settings with two backends: a stopped backend is out of rotation after two
// failed requests and probed every second, so the failover check runs in seconds.
const (
	failoverThreshold       = 2
	failoverProbeIntervalMS = 1000
)

// buildConfig is the config document for one run: one backend (two with
// -base-url-2, each deploying the chat models; a third with -embeddings-base-url,
// deploying the embeddings model), the models above, one group holding the one key
// (hash) with every model allowed.
func buildConfig(o options, hash string) ([]byte, error) {
	timeout := o.requestTimeout.Milliseconds()
	backendOf := func(kind, baseURL, apiKeyEnv string) map[string]any {
		b := map[string]any{"base_url": baseURL, "first_event_timeout_ms": timeout,
			"response_timeout_ms": timeout, "stall_timeout_ms": timeout}
		if kind == kindAzure {
			b["type"] = "azure-openai"
		} else {
			b["type"] = "openai-compatible"
		}
		if apiKeyEnv != "" {
			b["api_key_env"] = apiKeyEnv
		}
		return b
	}
	backend := func(baseURL string) map[string]any {
		b := backendOf(o.kind, baseURL, o.apiKeyEnv)
		if o.maxInFlight > 0 {
			b["max_in_flight"] = o.maxInFlight
		}
		return b
	}
	backends := map[string]any{backendFirst: backend(o.baseURL)}
	chatDeployments := []any{map[string]any{"backend": backendFirst, "model": o.model}}
	global := map[string]any{}
	if o.twoBackends() {
		backends[backendSecond] = backend(o.baseURL2)
		chatDeployments = append(chatDeployments, map[string]any{"backend": backendSecond, "model": o.model})
		global["circuit"] = map[string]any{"failure_threshold": failoverThreshold, "probe_interval_ms": failoverProbeIntervalMS}
	}

	var defaults map[string]any
	if o.chatDefaults != "" {
		if err := json.Unmarshal([]byte(o.chatDefaults), &defaults); err != nil || defaults == nil {
			return nil, fmt.Errorf("-chat-defaults is not a JSON object: %s", o.chatDefaults)
		}
	}
	var prices []any
	if o.priceIn != 0 || o.priceOut != 0 {
		prices = []any{map[string]any{"effective_from": "2000-01-01", "tiers": []any{map[string]any{
			"above_input_tokens": 0, "usd_per_million": map[string]any{"tokens_in": o.priceIn, "tokens_out": o.priceOut}}}}}
	}
	chat := func(outputLimit int) map[string]any {
		m := map[string]any{
			"deployments": chatDeployments,
			"metadata": map[string]any{"context_length": o.contextLength,
				"capabilities": map[string]any{"streaming": true, "tools": false, "vision": false, "reasoning": false}},
			"output_limit": map[string]any{"default": outputLimit, "ceiling": outputLimit},
		}
		if defaults != nil {
			m["defaults"] = defaults
		}
		if prices != nil {
			m["prices"] = prices
		}
		return m
	}
	models := map[string]any{
		modelChat:   chat(o.maxOutput),
		modelCapped: chat(o.ceiling),
		modelRPM:    chat(o.ceiling),
	}
	if o.embeddingsModel != "" {
		embedBackend := backendFirst
		if o.embeddingsServer() {
			embedBackend = backendEmbed
			backends[backendEmbed] = backendOf(kindVLLM, o.embeddingsBaseURL, o.embeddingsAPIKeyEnv)
		}
		embed := map[string]any{
			"deployments": []any{map[string]any{"backend": embedBackend, "model": o.embeddingsModel}},
			"metadata": map[string]any{"context_length": 8192,
				"capabilities": map[string]any{"streaming": false, "tools": false, "vision": false, "reasoning": false}},
		}
		if prices != nil {
			embed["prices"] = prices
		}
		models[modelEmbed] = embed
	}

	return json.MarshalIndent(map[string]any{
		"format_version": 3,
		"global":         global,
		"backends":       backends,
		"models":         models,
		"groups": map[string]any{"live": map[string]any{
			"limits": []any{map[string]any{"type": "requests_per_minute", "value": 1, "models": []any{modelRPM}}},
		}},
		"keys": map[string]any{"k-live": map[string]any{"hash": hash, "group": "live"}},
	}, "", "  ")
}

// newKey mints a client key and its config hash, the way the README recipe does.
func newKey() (key, hash string) {
	raw := make([]byte, 24)
	_, _ = rand.Read(raw) // crypto/rand.Read never fails
	key = "kaiak-" + hex.EncodeToString(raw)
	sum := sha256.Sum256([]byte(key))
	return key, "sha256:" + hex.EncodeToString(sum[:])
}

// workspace is the run's temporary directory: built binaries, configs, data
// directories, gateway logs.
type workspace struct {
	dir  string
	keep bool
}

func newWorkspace(keep bool) (*workspace, error) {
	dir, err := os.MkdirTemp("", "kaiak-live-")
	if err != nil {
		return nil, err
	}
	return &workspace{dir: dir, keep: keep}, nil
}

func (w *workspace) close() {
	if w.keep {
		fmt.Println("kept:", w.dir)
		return
	}
	_ = os.RemoveAll(w.dir)
}

// build compiles a main package of the gateway module into the workspace.
func (w *workspace) build(name, pkg string) (string, error) {
	root, err := repoRoot()
	if err != nil {
		return "", err
	}
	out := filepath.Join(w.dir, name)
	cmd := exec.Command("go", "build", "-o", out, pkg)
	cmd.Dir = filepath.Join(root, "gateway")
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("building %s: %w", pkg, err)
	}
	return out, nil
}

func (w *workspace) writeConfig(kind string, data []byte) (string, error) {
	path := filepath.Join(w.dir, kind+".config.json")
	return path, os.WriteFile(path, data, 0o600)
}

// repoRoot finds the repository from the working directory up: the directory
// holding gateway/go.mod.
func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "gateway", "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("run from inside the kaiak repository (no gateway/go.mod above the working directory), or pass -kaiak")
		}
		dir = parent
	}
}
