package fakebackend

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"unicode"
)

// RerankShape is the server whose rerank answer the backend sends. Both answer the
// model, usage with prompt tokens only and one result per document — its index and
// relevance score — sorted by score.
type RerankShape int

const (
	// VLLMRerank is vLLM's answer (vllm/entrypoints/pooling/scoring/protocol.py,
	// RerankResponse): an id, and each result's document — its text, or its
	// multimodal parts. A top_n above 0 cuts the results to that many; 0 keeps them
	// all.
	VLLMRerank RerankShape = iota
	// LlamaServerRerank is llama-server's (tools/server/server-common.cpp,
	// format_response_rerank, master at b11513): object "list", no id, no document.
	// A top_n cuts the results to that many, 0 to none; without one, all are kept.
	LlamaServerRerank
)

// rerankResult is one document's result; Document is nil in llama-server's shape.
type rerankResult struct {
	Index          int            `json:"index"`
	Document       map[string]any `json:"document,omitempty"`
	RelevanceScore float64        `json:"relevance_score"`
}

// writeRerank answers a rerank request in shape. A document's score is the share of
// the query's words it holds, so a relevant document outranks an irrelevant one; ties
// keep the documents' order. documents that is not a list is one document, as vLLM
// reads it, and a document that is not a string scores 0 (llama-server refuses both;
// the fake answers them in either shape). With context above 0, a request holding a
// pair — the query and one document — of more words than context is refused 400 in
// shape's error.
func writeRerank(w http.ResponseWriter, shape RerankShape, context int, top map[string]json.RawMessage, model string, usage Usage, omitUsage bool) {
	var query string
	_ = json.Unmarshal(top["query"], &query)
	var documents []json.RawMessage
	if raw, ok := top["documents"]; ok && json.Unmarshal(raw, &documents) != nil {
		documents = []json.RawMessage{raw}
	}
	queryWords := words(query)
	for _, raw := range documents {
		var text string
		_ = json.Unmarshal(raw, &text)
		if n := len(queryWords) + len(words(text)); context > 0 && n > context {
			writeJSON(w, http.StatusBadRequest, rerankContextError(shape, n, context))
			return
		}
	}
	results := []rerankResult{}
	for i, raw := range documents {
		result := rerankResult{Index: i}
		var text string
		if json.Unmarshal(raw, &text) != nil {
			if shape == VLLMRerank {
				result.Document = map[string]any{"multi_modal": raw}
			}
			results = append(results, result)
			continue
		}
		if shape == VLLMRerank {
			result.Document = map[string]any{"text": text}
		}
		if len(queryWords) > 0 {
			held := words(text)
			n := 0
			for _, word := range queryWords {
				if slices.Contains(held, word) {
					n++
				}
			}
			result.RelevanceScore = float64(n) / float64(len(queryWords))
		}
		results = append(results, result)
	}
	slices.SortStableFunc(results, func(a, b rerankResult) int {
		switch {
		case a.RelevanceScore > b.RelevanceScore:
			return -1
		case a.RelevanceScore < b.RelevanceScore:
			return 1
		}
		return 0
	})
	var topN *int
	_ = json.Unmarshal(top["top_n"], &topN)
	if topN != nil && *topN >= 0 && *topN < len(results) && (*topN > 0 || shape == LlamaServerRerank) {
		results = results[:*topN]
	}
	answer := map[string]any{"model": model, "results": results}
	if shape == LlamaServerRerank {
		answer["object"] = "list"
	} else {
		answer["id"] = "rerank-fake-1"
	}
	if !omitUsage {
		answer["usage"] = usageBody(usage, rerankPath)
	}
	writeJSON(w, http.StatusOK, answer)
}

// rerankContextError is the server's refusal of a pair of n tokens in a context of
// context tokens. vLLM: a validation error (vllm/renderers/params.py, the token length
// check) in its OpenAI-shaped error, type BadRequestError and the status as code
// (vllm/entrypoints/serve/exception_handling/error_response.py). llama-server: its
// exceed_context_size_error, which carries both counts (tools/server/server-context.cpp
// on a pair longer than a slot's context; server-task.cpp, the error's to_json).
func rerankContextError(shape RerankShape, n, context int) map[string]any {
	if shape == LlamaServerRerank {
		return map[string]any{"error": map[string]any{"code": http.StatusBadRequest,
			"message": fmt.Sprintf("request (%d tokens) exceeds the available context size (%d tokens), try increasing it", n, context),
			"type":    "exceed_context_size_error", "n_prompt_tokens": n, "n_ctx": context}}
	}
	return map[string]any{"error": map[string]any{
		"message": fmt.Sprintf("This model's maximum context length is %d tokens. However, you requested 0 output tokens "+
			"and your prompt contains %d input tokens, for a total of %d tokens. Please reduce the length of the input "+
			"prompt or the number of requested output tokens.", context, n, n),
		"type": "BadRequestError", "param": "input_tokens", "code": http.StatusBadRequest}}
}

// words are text's words, lowercased.
func words(text string) []string {
	return strings.FieldsFunc(strings.ToLower(text), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
}
