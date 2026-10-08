package fakebackend

import (
	"encoding/json"
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
// the fake answers them in either shape).
func writeRerank(w http.ResponseWriter, shape RerankShape, top map[string]json.RawMessage, model string, usage Usage, omitUsage bool) {
	var query string
	_ = json.Unmarshal(top["query"], &query)
	var documents []json.RawMessage
	if raw, ok := top["documents"]; ok && json.Unmarshal(raw, &documents) != nil {
		documents = []json.RawMessage{raw}
	}
	queryWords := words(query)
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

// words are text's words, lowercased.
func words(text string) []string {
	return strings.FieldsFunc(strings.ToLower(text), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
}
