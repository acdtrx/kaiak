package fakebackend

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"unicode"
)

// Rerank answers, shaped as vLLM answers them (vllm/entrypoints/pooling/scoring/
// protocol.py, RerankResponse): an id, the model, usage with prompt tokens only, and
// one result per document — its index, the document and its relevance score — sorted
// by score, cut to top_n.

// rerankResult is one document's result.
type rerankResult struct {
	Index          int            `json:"index"`
	Document       map[string]any `json:"document"`
	RelevanceScore float64        `json:"relevance_score"`
}

// writeRerank answers a rerank request. A document's score is the share of the
// query's words it holds, so a relevant document outranks an irrelevant one; ties keep
// the documents' order. documents that is not a list is one document, as vLLM reads
// it; a top_n above 0 cuts the results to that many.
func writeRerank(w http.ResponseWriter, top map[string]json.RawMessage, model string, usage Usage, omitUsage bool) {
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
			result.Document = map[string]any{"multi_modal": raw}
			results = append(results, result)
			continue
		}
		result.Document = map[string]any{"text": text}
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
	var topN int
	if json.Unmarshal(top["top_n"], &topN) == nil && topN > 0 && topN < len(results) {
		results = results[:topN]
	}
	answer := map[string]any{"id": "rerank-fake-1", "model": model, "results": results}
	if !omitUsage {
		answer["usage"] = usageBody(usage, rerankPath)
	}
	writeJSON(w, http.StatusOK, answer)
}

// words are text's words, lowercased.
func words(text string) []string {
	return strings.FieldsFunc(strings.ToLower(text), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
}
