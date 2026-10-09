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
	modelRPM    = "live-rpm"    // the rate-limit checks' model, sent with the metered key
	modelEmbed  = "live-embed"  // only with -embeddings-model (whatever its name on the backend)
	modelRerank = "live-rerank" // only with -rerank-base-url on a kind serving rerank
	modelSpeech = "live-tts"    // only with -speech-base-url
	modelImage  = "live-image"  // only with -image-base-url
)

// Backend IDs in the generated config: the -base-url backend, the -base-url-2
// backend when there is one, the -embeddings-base-url, -rerank-base-url,
// -speech-base-url and -image-base-url backends when there are.
const (
	backendFirst  = "live"
	backendSecond = "live-2"
	backendEmbed  = "live-embeddings"
	backendRerank = "live-reranker"
	backendSpeech = "live-speech"
	backendImages = "live-images"
)

// rerankDocumentsCap is global.max_rerank_documents with a reranker: above every
// rerank request the checks send but the cap check's, so that one is refused before
// the backend.
const rerankDocumentsCap = 4

// The media caps with a vLLM-Omni server: global.max_images_per_request and
// global.max_speech_batch_items, above every image and batch request the checks send
// but the cap checks', so those are refused before the backend.
const (
	imagesPerRequestCap = 2
	speechBatchItemsCap = 3
)

// The media limit checks' group: a minute's window holds 1 image, and 1 second of
// audio generated — any clip fills it.
const (
	groupMedia        = "live-media"
	mediaLimitImages  = 1
	mediaLimitSeconds = 1
)

// The speech model's metadata: Qwen3-TTS's talker context in vLLM-Omni's bundled
// deploy config (max_model_len 4096), and vLLM-Omni's max_new_tokens default and
// bound as the output limit. The image model's context: Qwen-Image's prompt bound
// (tokenizer_max_length 1024).
const (
	speechContextLength = 4096
	speechOutputDefault = 2048
	speechOutputCeiling = 4096
	imageContextLength  = 1024
)

// Circuit settings with two backends: a stopped backend is out of rotation after two
// failed requests and probed every second, so the failover check runs in seconds.
const (
	failoverThreshold       = 2
	failoverProbeIntervalMS = 1000
)

// buildConfig is the config document for one run: one backend (two with
// -base-url-2, each deploying the chat models; one more with -embeddings-base-url,
// deploying the embeddings model, with -rerank-base-url, deploying the reranker, and
// with -speech-base-url and -image-base-url, vllm-omni backends deploying the speech
// and image models), the models above, and groups with every model allowed: live
// holds the checks' key (hash), live-metered the rate-limit checks' key (meteredHash)
// under 1 request a minute — a limit counts every request of its group — and, with a
// vLLM-Omni server, live-media the media limit checks' key (mediaHash) under 1 image
// and 1 second of audio a minute.
func buildConfig(o options, hash, meteredHash, mediaHash string) ([]byte, error) {
	timeout := o.requestTimeout.Milliseconds()
	backendOf := func(backendType, baseURL, apiKeyEnv string) map[string]any {
		b := map[string]any{"type": backendType, "base_url": baseURL, "first_event_timeout_ms": timeout,
			"response_timeout_ms": timeout, "stall_timeout_ms": timeout}
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
			backends[backendEmbed] = backendOf("openai-compatible", o.embeddingsBaseURL, o.embeddingsAPIKeyEnv)
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
	if o.reranker() {
		backends[backendRerank] = backendOf(o.kind, o.rerankBaseURL, o.rerankAPIKeyEnv)
		rerank := map[string]any{
			"deployments": []any{map[string]any{"backend": backendRerank, "model": o.rerankModel}},
			"metadata": map[string]any{"context_length": 8192,
				"capabilities": map[string]any{"streaming": false, "tools": false, "vision": false, "reasoning": false}},
		}
		if prices != nil {
			rerank["prices"] = prices
		}
		models[modelRerank] = rerank
		global["max_rerank_documents"] = rerankDocumentsCap
	}
	media := func(backend, baseURL, apiKeyEnv, model string, contextLength int, streaming bool) map[string]any {
		backends[backend] = backendOf("vllm-omni", baseURL, apiKeyEnv)
		m := map[string]any{
			"deployments": []any{map[string]any{"backend": backend, "model": model}},
			"metadata": map[string]any{"context_length": contextLength,
				"capabilities": map[string]any{"streaming": streaming, "tools": false, "vision": false, "reasoning": false}},
		}
		if prices != nil {
			m["prices"] = prices
		}
		return m
	}
	if o.speechServer() {
		speech := media(backendSpeech, o.speechBaseURL, o.speechAPIKeyEnv, o.speechModel, speechContextLength, true)
		speech["output_limit"] = map[string]any{"default": speechOutputDefault, "ceiling": speechOutputCeiling}
		models[modelSpeech] = speech
		global["max_speech_batch_items"] = speechBatchItemsCap
	}
	if o.imageServer() {
		models[modelImage] = media(backendImages, o.imageBaseURL, o.imageAPIKeyEnv, o.imageModel, imageContextLength, false)
		global["max_images_per_request"] = imagesPerRequestCap
	}

	groups := map[string]any{
		"live": map[string]any{},
		"live-metered": map[string]any{
			"limits": []any{map[string]any{"type": "requests_per_minute", "value": 1}},
		},
	}
	keys := map[string]any{
		"k-live":         map[string]any{"hash": hash, "group": "live"},
		"k-live-metered": map[string]any{"hash": meteredHash, "group": "live-metered"},
	}
	if o.speechServer() || o.imageServer() {
		groups[groupMedia] = map[string]any{"limits": []any{
			map[string]any{"type": "images_per_minute", "value": mediaLimitImages},
			map[string]any{"type": "audio_seconds_out_per_minute", "value": mediaLimitSeconds},
		}}
		keys["k-"+groupMedia] = map[string]any{"hash": mediaHash, "group": groupMedia}
	}

	return json.MarshalIndent(map[string]any{
		"format_version": 6,
		"global":         global,
		"backends":       backends,
		"models":         models,
		"groups":         groups,
		"keys":           keys,
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

// workspace is the run's temporary directory: built binaries, configs, gateway logs.
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
