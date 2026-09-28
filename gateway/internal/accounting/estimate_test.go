package accounting

import (
	"strings"
	"testing"

	"kaiak/internal/provider"
)

// D1, D2: the input estimate counts text by bytes, each media item as
// InlineMediaTokens and each token ID as one token; a completion batch's largest
// prompt is what one sequence sees.
func TestEstimateInput(t *testing.T) {
	const m = InlineMediaTokens
	payload := strings.Repeat("QUJD", 100_000) // 400 KB of base64
	text := func(s string) int64 { return EstimateTokens(int64(len(s))) }
	for _, c := range []struct {
		name          string
		ep            provider.Endpoint
		body          string
		total, prompt int64
	}{
		{name: "plain text is its bytes", ep: provider.ChatCompletions,
			body:  `{"model":"m","messages":[{"role":"user","content":"hello there"}]}`,
			total: text(`{"model":"m","messages":[{"role":"user","content":"hello there"}]}`)},
		{name: "inline image", ep: provider.ChatCompletions,
			body:  `{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,` + payload + `"}}]}]}`,
			total: text(`{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url"}}]}]}`) + m},
		{name: "remote image URL", ep: provider.ChatCompletions,
			body:  `{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.com/cat.png"}}]}]}`,
			total: text(`{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url"}}]}]}`) + m},
		{name: "image_url as a bare string", ep: provider.ChatCompletions,
			body:  `{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":"https://example.com/cat.png"}]}]}`,
			total: text(`{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url"}]}]}`) + m},
		{name: "audio as raw base64", ep: provider.ChatCompletions,
			body:  `{"model":"m","messages":[{"role":"user","content":[{"type":"input_audio","input_audio":{"data":"` + payload + `","format":"wav"}}]}]}`,
			total: text(`{"model":"m","messages":[{"role":"user","content":[{"type":"input_audio","input_audio":{"data","format":"wav"}}]}]}`) + m},
		{name: "a data URL in any field", ep: provider.ChatCompletions,
			body:  `{"model":"m","messages":[{"role":"user","content":[{"type":"file","file":{"file_data":"data:application/pdf;base64,` + payload + `"}},{"type":"text","text":"x"}]}]}`,
			total: text(`{"model":"m","messages":[{"role":"user","content":[{"type":"file","file":{"file_data"}},{"type":"text","text":"x"}]}]}`) + m},
		{name: "two images", ep: provider.ChatCompletions,
			body:  `{"a":"data:image/jpeg;base64,` + payload + `","b":"data:image/jpeg;base64,/9j/"}`,
			total: text(`{"a","b"}`) + 2*m},
		{name: "text starting with data: is text", ep: provider.ChatCompletions,
			body:  `{"model":"m","messages":[{"role":"user","content":"data: {\"x\":1},data: [DONE]"}]}`,
			total: text(`{"model":"m","messages":[{"role":"user","content":"data: {\"x\":1},data: [DONE]"}]}`)},
		{name: "a url member elsewhere is text", ep: provider.ChatCompletions,
			body:  `{"model":"m","url":"https://example.com/x"}`,
			total: text(`{"model":"m","url":"https://example.com/x"}`)},
		{name: "completion token IDs", ep: provider.Completions,
			body:  `{"model":"m","prompt":[101,2023,2003]}`,
			total: text(`{"model":"m","prompt":[,,]}`) + 3},
		{name: "embedding token-ID lists", ep: provider.Embeddings,
			body:  `{"model":"m","input":[[101,2023],[7]]}`,
			total: text(`{"model":"m","input":[[,],[]]}`) + 3},
		{name: "numbers elsewhere are text", ep: provider.ChatCompletions,
			body:  `{"model":"m","prompt":[101,2023],"temperature":0.5}`,
			total: text(`{"model":"m","prompt":[101,2023],"temperature":0.5}`)},
		{name: "a completion batch: the largest prompt", ep: provider.Completions,
			body:   `{"model":"m","prompt":["` + strings.Repeat("a", 4000) + `","` + strings.Repeat("b", 400) + `"]}`,
			total:  text(`{"model":"m","prompt":["` + strings.Repeat("a", 4000) + `","` + strings.Repeat("b", 400) + `"]}`),
			prompt: text(`{"model":"m","prompt":["` + strings.Repeat("a", 4000) + `"]}`)},
		{name: "a batch of token-ID lists", ep: provider.Completions,
			body:   `{"model":"m","prompt":[[1,2,3,4],[5]]}`,
			total:  text(`{"model":"m","prompt":[[,,,],[]]}`) + 5,
			prompt: text(`{"model":"m","prompt":[[,,,]]}`) + 4},
		{name: "a single string prompt", ep: provider.Completions,
			body:  `{"model":"m","prompt":"hello","suffix":"bye"}`,
			total: text(`{"model":"m","prompt":"hello","suffix":"bye"}`)},
		{name: "not JSON: its bytes", ep: provider.ChatCompletions,
			body:  `not json at all`,
			total: text(`not json at all`)},
	} {
		t.Run(c.name, func(t *testing.T) {
			want := InputEstimate{Total: c.total, LargestPrompt: c.total}
			if c.prompt != 0 {
				want.LargestPrompt = c.prompt
			}
			// The span of a left-out value includes its separator (":" or ","); the
			// text estimates above leave those in, so allow a token's difference.
			got := EstimateInput(c.ep, []byte(c.body))
			if got.Total < want.Total-1 || got.Total > want.Total ||
				got.LargestPrompt < want.LargestPrompt-1 || got.LargestPrompt > want.LargestPrompt {
				t.Errorf("EstimateInput = %+v, want %+v", got, want)
			}
		})
	}
}

// The estimate of an image does not depend on its encoding: the same picture,
// compressed well or badly, counts the same.
func TestInlineMediaEstimateIgnoresTheEncodedSize(t *testing.T) {
	body := func(n int) []byte {
		return []byte(`{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,` +
			strings.Repeat("A", n) + `"}}]}]}`)
	}
	small, large := EstimateInput(provider.ChatCompletions, body(1_000)), EstimateInput(provider.ChatCompletions, body(1_400_000))
	if small != large {
		t.Errorf("estimates differ by encoded size: %+v, %+v", small, large)
	}
}
