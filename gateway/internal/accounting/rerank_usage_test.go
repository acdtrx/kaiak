package accounting

import (
	"strings"
	"testing"

	"kaiak/internal/config"
	"kaiak/internal/provider"
)

// A rerank answer's usage is its top-level prompt_tokens, as tokens_in alone; without
// a report the input is estimated and the record flagged, and the output is 0 however
// much the results carry: nothing is generated (docs/specs/GATEWAY.md, Accounting →
// rerank usage).
func TestRerankUsage(t *testing.T) {
	const requestBytes = 41 // → 11 tokens
	results := `"results":[{"index":1,"document":{"text":"` + strings.Repeat("a panda eats bamboo ", 50) + `"},"relevance_score":0.9},` +
		`{"index":0,"document":{"text":"the sky"},"relevance_score":0.1}]`
	estimated := withEveryTokenUnit(Units{config.UnitTokensIn: 11})
	for _, c := range []struct {
		name  string
		body  string
		want  Units
		flags Flags
	}{
		{"reported", `{"id":"rerank-1","model":"m","usage":{"prompt_tokens":40,"total_tokens":40},` + results + `}`,
			withEveryTokenUnit(Units{config.UnitTokensIn: 40}), Flags{}},
		{"reported after the results", `{"model":"m",` + results + `,"usage":{"prompt_tokens":40,"total_tokens":40}}`,
			withEveryTokenUnit(Units{config.UnitTokensIn: 40}), Flags{}},
		{"prompt tokens only", `{"model":"m","usage":{"prompt_tokens":40,"completion_tokens":5,` +
			`"prompt_tokens_details":{"cached_tokens":10}},` + results + `}`,
			withEveryTokenUnit(Units{config.UnitTokensIn: 40}), Flags{}},
		{"missing", `{"id":"rerank-1","model":"m",` + results + `}`, estimated, Flags{Estimated: true}},
		{"null", `{"model":"m","usage":null,` + results + `}`, estimated, Flags{Estimated: true}},
		{"malformed", `{"model":"m","usage":{"prompt_tokens":"many"},` + results + `}`, estimated, Flags{Estimated: true}},
		{"neither count", `{"model":"m","usage":{"total_tokens":40},` + results + `}`, estimated, Flags{Estimated: true}},
	} {
		t.Run(c.name, func(t *testing.T) {
			units, flags := bodyMeter(provider.Rerank, requestBytes, 200, c.body).Settle(true)
			expect(t, units, flags, c.want, c.flags)
		})
	}
}

// A rerank request's estimate is its body's, by the OpenAI body's rules, plus its
// query's own estimate once more for each document beyond the first — documents
// counted as the cap counts them, the query's media at the flat figure
// (docs/specs/GATEWAY.md, Limits → the input estimate: rerank). Rerank has no output
// limit: the input one sequence sees stays the body's.
func TestEstimateRerankInput(t *testing.T) {
	const m = InlineMediaTokens
	text := func(s string) int64 { return EstimateTokens(int64(len(s))) }
	// The query's own estimate is its value's alone, without the ":" and whitespace
	// before it; a media item's span, which it leaves out of the text, includes them.
	const query = `"what is a panda"`
	const imageQuery = `{"content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,QUJDRA"}}]}`
	// imageQueryText is imageQuery less the image's data URL, which counts m.
	const imageQueryText = `{"content":[{"type":"image_url","image_url":{"url"}}]}`
	for _, c := range []struct {
		name, body string
		// own is the body's estimate; repeats what the repeated query adds.
		own, repeats int64
	}{
		{name: "one document",
			body: `{"model":"m","query":` + query + `,"documents":["the panda is a bear"]}`,
			own:  text(`{"model":"m","query":` + query + `,"documents":["the panda is a bear"]}`)},
		{name: "many documents",
			body:    `{"model":"m","query":` + query + `,"documents":["a","b","c","d"],"top_n":2}`,
			own:     text(`{"model":"m","query":` + query + `,"documents":["a","b","c","d"],"top_n":2}`),
			repeats: 3 * text(query)},
		{name: "the query after the documents",
			body:    `{"model":"m","documents":["a","b","c","d"],"query":` + query + `}`,
			own:     text(`{"model":"m","documents":["a","b","c","d"],"query":` + query + `}`),
			repeats: 3 * text(query)},
		{name: "whitespace around the query's colon",
			body:    `{"model":"m", "query"` + " \n:\t " + query + `, "documents":["a","b"]}`,
			own:     text(`{"model":"m", "query"` + " \n:\t " + query + `, "documents":["a","b"]}`),
			repeats: text(query)},
		{name: "a query that is a media item",
			body:    `{"model":"m","query":"data:image/png;base64,QUJDRA","documents":["a","b","c"]}`,
			own:     text(`{"model":"m","query","documents":["a","b","c"]}`) + m,
			repeats: 2 * m},
		{name: "documents that are not a list are one",
			body: `{"model":"m","query":` + query + `,"documents":"one long document"}`,
			own:  text(`{"model":"m","query":` + query + `,"documents":"one long document"}`)},
		{name: "no documents",
			body: `{"model":"m","query":` + query + `,"documents":[]}`,
			own:  text(`{"model":"m","query":` + query + `,"documents":[]}`)},
		{name: "a query with an image part",
			body:    `{"model":"m","query":` + imageQuery + `,"documents":["a","b","c"]}`,
			own:     text(`{"model":"m","query":`+imageQueryText+`,"documents":["a","b","c"]}`) + m,
			repeats: 2 * (text(imageQueryText) + m)},
		{name: "a document with an image part counts once",
			body: `{"model":"m","query":` + query + `,"documents":["a",{"content":[{"type":"image_url","image_url":{"url":"https://example.com/p.png"}}]}]}`,
			own: text(`{"model":"m","query":`+query+`,"documents":["a",{"content":[{"type":"image_url","image_url":{"url"}}]}]}`) +
				m,
			repeats: text(query)},
	} {
		t.Run(c.name, func(t *testing.T) {
			want := InputEstimate{Total: c.own + c.repeats, LargestPrompt: c.own}
			if got := EstimateInput(provider.Rerank, []byte(c.body)); got != want {
				t.Errorf("EstimateInput = %+v, want %+v", got, want)
			}
		})
	}

	// query and documents are rerank's alone: elsewhere they are text.
	body := `{"model":"m","query":` + query + `,"documents":["a","b","c","d"],"input":"x"}`
	if got, want := EstimateInput(provider.Embeddings, []byte(body)).Total, text(body); got != want {
		t.Errorf("embeddings with query and documents: total %d, want %d", got, want)
	}
}
