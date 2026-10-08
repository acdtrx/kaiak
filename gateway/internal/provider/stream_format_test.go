package provider

import (
	"testing"
)

// The usage-only chunk stream_options.include_usage asks for is a JSON object with an
// empty "choices" array and a non-null "usage", its keys matched exactly.
func TestOpenAIStreamReadsTheUsageOnlyChunk(t *testing.T) {
	cases := map[string]bool{
		`{"id":"1","choices":[],"usage":{"prompt_tokens":1}}`:             true,
		`{"id":"1","choices":[{"delta":{}}],"usage":{"prompt_tokens":1}}`: false,
		`{"id":"1","choices":[{"delta":{"content":"x"}}],"usage":null}`:   false,
		`{"id":"1","choices":[]}`:                                         false,
		`{"id":"1","Choices":[],"usage":{"prompt_tokens":1}}`:             false,
		`{"id":"1","choices":null,"usage":{"prompt_tokens":1}}`:           false,
		`[DONE]`: false,
		`{"id":"1","choices":[],"usage":{"prompt_tokens":1},"extra":{"a":[1]}}`: true,
	}
	for payload, want := range cases {
		if got := newStreamFormat(FormatOpenAI).observe([]byte(payload)).usageOnly; got != want {
			t.Errorf("%s: got %v, want %v", payload, got, want)
		}
	}
}
