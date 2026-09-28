// Command fakebackend runs the fake OpenAI-compatible backend as a process, for
// scripts, the live-test kit's self-test and manual runs. Test tooling only.
//
// It serves /v1/ (openai-compatible layout) and /openai/v1/ (Azure layout), prints
// "listening <url>" on stdout once it accepts connections, and stops on SIGINT or
// SIGTERM. Answers honor max_completion_tokens / max_tokens (finish_reason "length"
// when they cut the answer short).
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
	auth := flag.String("auth", "none", "credential check: none, bearer (Authorization: Bearer <key>), api-key (api-key: <key>)")
	key := flag.String("key", "", "the credential the auth check expects")
	quiet := flag.Bool("quiet", false, "do not log requests to stderr")
	models := flag.String("models", strings.Join(fakebackend.DefaultModels, ","),
		"comma-separated model IDs the models list lists (completions serve any model name)")
	flag.Parse()

	reply := fakebackend.Reply{Chunks: words(answer), HonorMaxTokens: true}
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
	default:
		log.Fatalf("fakebackend: unknown -auth %q", *auth)
	}

	backend, err := fakebackend.NewAt(*addr)
	if err != nil {
		log.Fatalf("fakebackend: %v", err)
	}
	backend.SetReply(reply)
	backend.SetModels(strings.Split(*models, ",")...)
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
