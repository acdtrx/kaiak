// Command fakebackend runs the fake OpenAI-compatible backend as a process, for
// scripts, the live-test kit's self-test and manual runs. Test tooling only.
//
// It serves /v1/ (the OpenAI and Anthropic layout), /openai/v1/ (Azure OpenAI's) and
// /anthropic/v1/ (Claude in Foundry's), prints
// "listening <url>" on stdout once it accepts connections, and stops on SIGINT or
// SIGTERM. Answers honor max_completion_tokens / max_tokens (finish_reason "length"
// when they cut the answer short). Rerank answers in vLLM's shape or llama-server's;
// the server played can lack endpoints the way each server lacks them, and answer a
// path it has no route for as vLLM or llama-server does. Speech, the speech batch,
// image generation and image edits answer as vLLM-Omni does, and a model can be
// played as one that does not speak or does not draw, as vLLM-Omni answers for it.
// Speech-to-text answers in vLLM's shape or llama-server's, and a model can be played
// as one that does not hear audio, as each server answers for it.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"kaiak/internal/fakebackend"
)

// answer is the generated text, one token per word: long enough that a small
// output-limit ceiling cuts it short.
const answer = "Hello from the fake backend. It answers every request with the same text, " +
	"one token per word, so that output limits, streaming and usage reporting can be " +
	"checked without a model: a gateway in front of it should relay these words exactly " +
	"as they are sent and count them."

func main() {
	addr := flag.String("addr", "127.0.0.1:8000", "listen address (port 0 picks a free port)")
	profile := flag.String("profile", "normal", "behavior: normal, no-usage (never reports usage), slow (streams one event every 200ms)")
	auth := flag.String("auth", "none", "credential check: none, bearer (Authorization: Bearer <key>), api-key (api-key: <key>), x-api-key (x-api-key: <key>)")
	key := flag.String("key", "", "the credential the auth check expects")
	quiet := flag.Bool("quiet", false, "do not log requests to stderr")
	cachedTokens := flag.Int("cached-tokens", 0, "report this many of the 7 prompt tokens as read from the cache (0-7)")
	models := flag.String("models", strings.Join(fakebackend.DefaultModels, ","),
		"comma-separated model IDs the models list lists (completions serve any model name)")
	rerankShape := flag.String("rerank-shape", "vllm", "the server whose rerank answers and errors the backend sends: vllm, llama-server")
	rerankContext := flag.Int("rerank-context", 0, "the most words a rerank query and one document may hold together; a longer pair is refused 400 in the server's error (0: no limit)")
	noRoute := flag.String("no-route", "", `comma-separated endpoints (paths below /v1/, e.g. chat/completions) the server has no route for, answered as any path it has no route for (-no-route-shape; a vLLM reranker has no chat route, a vLLM chat model no rerank or embeddings route)`)
	noRouteShape := flag.String("no-route-shape", "", `the server whose 404 answers a path the backend has no route for — one it does not serve, or an endpoint in -no-route: vllm, FastAPI's {"detail":"Not Found"} (vLLM, vLLM-Omni); llama-server, {"error":{"message":"File Not Found","type":"not_found_error","code":404}} (default: vllm with -no-route, else an OpenAI-shaped error)`)
	notSupported := flag.String("not-supported", "", "comma-separated endpoints answered as llama-server answers one it was not started for: 501 not_supported_error (rerank without --reranking, embeddings without --embeddings)")
	nonSpeech := flag.String("non-speech-models", "", `comma-separated models whose speech is answered as vLLM-Omni answers it on a model with no speech stage: 400 "TTS model did not produce audio output." (the speech batch: 400, as on a pure-diffusion server)`)
	nonImage := flag.String("non-image-models", "", `comma-separated models whose image generation is answered as vLLM-Omni answers it on a model with no diffusion stage: 503 "No diffusion stage found in multi-stage pipeline."`)
	imageServed := flag.String("image-served-model", "", `the server's served model name, which image generation compares a request's model with, as vLLM-Omni does: another model is answered 400 "Model mismatch" ("": none compared)`)
	transcriptionShape := flag.String("transcription-shape", "vllm", "the server whose speech-to-text answers and errors the backend sends: vllm, llama-server")
	nonAudio := flag.String("non-audio-models", "", `comma-separated models whose transcription is answered as each server answers one on a model that does not hear audio: vLLM 404 {"detail":"Not Found"}, llama-server 501 not_supported_error`)
	audioMillis := flag.Int("audio-millis", 0, "the length in milliseconds of each speech clip and batch item generated, and of the audio vLLM's speech-to-text shape hears in every upload (0: 2500)")
	flag.Parse()

	if *cachedTokens < 0 || *cachedTokens > 7 {
		log.Fatalf("fakebackend: -cached-tokens %d: want 0 to 7", *cachedTokens)
	}
	if *rerankContext < 0 {
		log.Fatalf("fakebackend: -rerank-context %d: want 0 or more", *rerankContext)
	}
	if *audioMillis < 0 {
		log.Fatalf("fakebackend: -audio-millis %d: want 0 or more", *audioMillis)
	}
	if *noRouteShape == "" && *noRoute != "" {
		*noRouteShape = "vllm"
	}
	noRouteBody, ok := map[string]string{"": "", "vllm": `{"detail":"Not Found"}`,
		"llama-server": `{"error":{"message":"File Not Found","type":"not_found_error","code":404}}`}[*noRouteShape]
	if !ok {
		log.Fatalf("fakebackend: unknown -no-route-shape %q", *noRouteShape)
	}
	shape, ok := map[string]fakebackend.RerankShape{"vllm": fakebackend.VLLMRerank, "llama-server": fakebackend.LlamaServerRerank}[*rerankShape]
	if !ok {
		log.Fatalf("fakebackend: unknown -rerank-shape %q", *rerankShape)
	}
	transcription, ok := map[string]fakebackend.TranscriptionShape{"vllm": fakebackend.VLLMTranscription,
		"llama-server": fakebackend.LlamaServerTranscription}[*transcriptionShape]
	if !ok {
		log.Fatalf("fakebackend: unknown -transcription-shape %q", *transcriptionShape)
	}
	reply := fakebackend.Reply{Chunks: words(answer), HonorMaxTokens: true, CachedTokens: *cachedTokens, AudioMillis: *audioMillis}
	switch *profile {
	case "normal":
	case "no-usage":
		reply.OmitUsage = true
	case "slow":
		reply.EventDelay = 200 * time.Millisecond
	default:
		log.Fatalf("fakebackend: unknown -profile %q", *profile)
	}
	switch *auth {
	case "none":
	case "bearer":
		reply.RequireHeader, reply.RequireValue = "Authorization", "Bearer "+*key
	case "api-key":
		reply.RequireHeader, reply.RequireValue = "api-key", *key
	case "x-api-key":
		reply.RequireHeader, reply.RequireValue = "x-api-key", *key
	default:
		log.Fatalf("fakebackend: unknown -auth %q", *auth)
	}

	backend, err := fakebackend.NewAt(*addr)
	if err != nil {
		log.Fatalf("fakebackend: %v", err)
	}
	backend.SetReply(reply)
	backend.SetModels(strings.Split(*models, ",")...)
	backend.SetRerankShape(shape)
	backend.SetRerankContext(*rerankContext)
	if noRouteBody != "" {
		var unrouted []string
		if *noRoute != "" {
			unrouted = strings.Split(*noRoute, ",")
		}
		backend.SetNoRoute(noRouteBody, unrouted...)
	}
	if *notSupported != "" {
		backend.SetNotSupported(strings.Split(*notSupported, ",")...)
	}
	if *nonSpeech != "" {
		backend.SetNonSpeechModels(strings.Split(*nonSpeech, ",")...)
	}
	if *nonImage != "" {
		backend.SetNonImageModels(strings.Split(*nonImage, ",")...)
	}
	backend.SetImageServedModel(*imageServed)
	backend.SetTranscriptionShape(transcription)
	if *nonAudio != "" {
		backend.SetNonAudioModels(strings.Split(*nonAudio, ",")...)
	}
	fmt.Println("listening", backend.URL())

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	for {
		select {
		case req := <-backend.Arrivals():
			if !*quiet {
				log.Printf("%s %s", req.Method, req.Path)
			}
		case <-stop:
			backend.Close()
			return
		}
	}
}

// words splits text into stream chunks: each word keeps its leading space, so the
// chunks join back into text.
func words(text string) []string {
	fields := strings.Fields(text)
	for i := 1; i < len(fields); i++ {
		fields[i] = " " + fields[i]
	}
	return fields
}
