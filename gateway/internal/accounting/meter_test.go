package accounting

import (
	"maps"
	"testing"

	"kaiak/internal/config"
	"kaiak/internal/provider"
)

func streamMeter(ep provider.Endpoint, requestBytes int, payloads ...string) *Meter {
	m := NewMeter(ep, EstimateTokens(int64(requestBytes)))
	m.Answered(200, true)
	for _, p := range payloads {
		m.Observe(provider.Event{Data: []byte("data: " + p + "\n\n"), Payload: []byte(p)})
	}
	return m
}

func bodyMeter(ep provider.Endpoint, requestBytes, status int, body string) *Meter {
	m := NewMeter(ep, EstimateTokens(int64(requestBytes)))
	m.Answered(status, false)
	// Body pieces arrive in arbitrary sizes.
	for len(body) > 0 {
		n := min(7, len(body))
		m.Observe(provider.Event{Data: []byte(body[:n])})
		body = body[n:]
	}
	return m
}

func expect(t *testing.T, got Units, flags Flags, want Units, wantFlags Flags) {
	t.Helper()
	if !maps.Equal(got, want) {
		t.Errorf("units %v, want %v", got, want)
	}
	if flags != wantFlags {
		t.Errorf("flags %+v, want %+v", flags, wantFlags)
	}
}

func TestUsageMapping(t *testing.T) {
	cases := []struct {
		name  string
		ep    provider.Endpoint
		usage string
		want  Units
	}{
		{"plain", provider.ChatCompletions, `{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}`,
			withEveryTokenUnit(Units{config.UnitTokensIn: 10, config.UnitTokensOut: 5})},
		{"cached and reasoning", provider.ChatCompletions,
			`{"prompt_tokens":100,"completion_tokens":50,"prompt_tokens_details":{"cached_tokens":64},` +
				`"completion_tokens_details":{"reasoning_tokens":30}}`,
			withEveryTokenUnit(Units{config.UnitTokensIn: 36, config.UnitTokensCached: 64, config.UnitTokensOut: 50, config.UnitTokensReasoning: 30})},
		{"null details", provider.ChatCompletions,
			`{"prompt_tokens":8,"completion_tokens":2,"prompt_tokens_details":null,"completion_tokens_details":null}`,
			withEveryTokenUnit(Units{config.UnitTokensIn: 8, config.UnitTokensOut: 2})},
		{"details without the counts", provider.ChatCompletions,
			`{"prompt_tokens":8,"completion_tokens":2,"prompt_tokens_details":{"audio_tokens":0},"completion_tokens_details":{}}`,
			withEveryTokenUnit(Units{config.UnitTokensIn: 8, config.UnitTokensOut: 2})},
		{"inconsistent details clamped", provider.ChatCompletions,
			`{"prompt_tokens":8,"completion_tokens":2,"prompt_tokens_details":{"cached_tokens":20},` +
				`"completion_tokens_details":{"reasoning_tokens":9}}`,
			withEveryTokenUnit(Units{config.UnitTokensCached: 8, config.UnitTokensOut: 2, config.UnitTokensReasoning: 2})},
		{"written to the cache (Azure OpenAI's first call)", provider.ChatCompletions,
			`{"prompt_tokens":2036,"completion_tokens":9,"prompt_tokens_details":{"cached_tokens":0,"cache_write_tokens":2033}}`,
			withEveryTokenUnit(Units{config.UnitTokensIn: 3, config.UnitTokensCacheWrite: 2033, config.UnitTokensOut: 9})},
		{"read and written", provider.ChatCompletions,
			`{"prompt_tokens":3000,"completion_tokens":9,"prompt_tokens_details":{"cached_tokens":2033,"cache_write_tokens":900}}`,
			withEveryTokenUnit(Units{config.UnitTokensIn: 67, config.UnitTokensCached: 2033, config.UnitTokensCacheWrite: 900, config.UnitTokensOut: 9})},
		{"written, no cached count", provider.Completions,
			`{"prompt_tokens":100,"completion_tokens":5,"prompt_tokens_details":{"cache_write_tokens":60}}`,
			withEveryTokenUnit(Units{config.UnitTokensIn: 40, config.UnitTokensCacheWrite: 60, config.UnitTokensOut: 5})},
		{"written above the prompt: clamped to it", provider.ChatCompletions,
			`{"prompt_tokens":8,"completion_tokens":2,"prompt_tokens_details":{"cache_write_tokens":20}}`,
			withEveryTokenUnit(Units{config.UnitTokensCacheWrite: 8, config.UnitTokensOut: 2})},
		{"cached and written above the prompt: cached clamped first, written to the rest", provider.ChatCompletions,
			`{"prompt_tokens":100,"completion_tokens":2,"prompt_tokens_details":{"cached_tokens":70,"cache_write_tokens":50}}`,
			withEveryTokenUnit(Units{config.UnitTokensCached: 70, config.UnitTokensCacheWrite: 30, config.UnitTokensOut: 2})},
		{"cached above the prompt leaves nothing written", provider.ChatCompletions,
			`{"prompt_tokens":100,"completion_tokens":2,"prompt_tokens_details":{"cached_tokens":150,"cache_write_tokens":50}}`,
			withEveryTokenUnit(Units{config.UnitTokensCached: 100, config.UnitTokensOut: 2})},
		{"negative written count is 0", provider.ChatCompletions,
			`{"prompt_tokens":100,"completion_tokens":2,"prompt_tokens_details":{"cached_tokens":10,"cache_write_tokens":-5}}`,
			withEveryTokenUnit(Units{config.UnitTokensIn: 90, config.UnitTokensCached: 10, config.UnitTokensOut: 2})},
		{"completion only", provider.Completions, `{"completion_tokens":4}`, withEveryTokenUnit(Units{config.UnitTokensOut: 4})},
		{"embeddings: prompt tokens only", provider.Embeddings,
			`{"prompt_tokens":12,"completion_tokens":3,"prompt_tokens_details":{"cached_tokens":5,"cache_write_tokens":4}}`,
			withEveryTokenUnit(Units{config.UnitTokensIn: 12})},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body := `{"id":"x","object":"chat.completion","choices":[],"usage":` + c.usage + `}`
			units, flags := bodyMeter(c.ep, 100, 200, body).Settle(true)
			expect(t, units, flags, c.want, Flags{})
		})
	}
}

func TestStreamUsageComesFromTheLastReport(t *testing.T) {
	m := streamMeter(provider.ChatCompletions, 100,
		`{"choices":[{"delta":{"content":"Hi"}}],"usage":null}`,
		`{"choices":[{"delta":{"content":" there"}}],"usage":{"prompt_tokens":5,"completion_tokens":1}}`,
		`{"choices":[],"usage":{"prompt_tokens":5,"completion_tokens":2}}`,
		`[DONE]`)
	units, flags := m.Settle(true)
	expect(t, units, flags, withEveryTokenUnit(Units{config.UnitTokensIn: 5, config.UnitTokensOut: 2}), Flags{})
}

// A stream's usage chunk carries the cache counts as a body's usage does.
func TestStreamUsageCarriesInputWrittenToTheCache(t *testing.T) {
	m := streamMeter(provider.ChatCompletions, 100,
		`{"choices":[{"delta":{"content":"Hi"}}],"usage":null}`,
		`{"choices":[],"usage":{"prompt_tokens":2036,"completion_tokens":9,`+
			`"prompt_tokens_details":{"cached_tokens":0,"cache_write_tokens":2033}}}`,
		`[DONE]`)
	units, flags := m.Settle(true)
	expect(t, units, flags, withEveryTokenUnit(Units{config.UnitTokensIn: 3, config.UnitTokensCacheWrite: 2033, config.UnitTokensOut: 9}), Flags{})
}

func TestMissingUsageIsEstimatedFromContent(t *testing.T) {
	const requestBytes = 41 // → 11 tokens
	t.Run("chat stream", func(t *testing.T) {
		m := streamMeter(provider.ChatCompletions, requestBytes,
			`{"choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`,
			`{"choices":[{"index":0,"delta":{"reasoning_content":"think","reasoning":"think"}}]}`,
			`{"choices":[{"index":0,"delta":{"content":"héllo"}}]}`,
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"f","arguments":"{\"a\":1}"}}]}}]}`,
			`{"choices":[{"index":0,"delta":{"refusal":"no"}},{"index":1,"delta":{"content":"xyz"}}]}`,
			`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			`[DONE]`)
		// think 5 (counted once) + héllo 6 + f 1 + {"a":1} 7 + no 2 + xyz 3 = 24 bytes → 6.
		units, flags := m.Settle(true)
		expect(t, units, flags, withEveryTokenUnit(Units{config.UnitTokensIn: 11, config.UnitTokensOut: 6}), Flags{Estimated: true})
	})
	t.Run("completions stream", func(t *testing.T) {
		m := streamMeter(provider.Completions, requestBytes,
			`{"choices":[{"text":"abcde","index":0}]}`, `{"choices":[{"text":"fgh","index":0}]}`)
		units, flags := m.Settle(true)
		expect(t, units, flags, withEveryTokenUnit(Units{config.UnitTokensIn: 11, config.UnitTokensOut: 2}), Flags{Estimated: true})
	})
	t.Run("chat body", func(t *testing.T) {
		body := `{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"twelve bytes"},` +
			`"finish_reason":"stop"}]}`
		units, flags := bodyMeter(provider.ChatCompletions, requestBytes, 200, body).Settle(true)
		expect(t, units, flags, withEveryTokenUnit(Units{config.UnitTokensIn: 11, config.UnitTokensOut: 3}), Flags{Estimated: true})
	})
	t.Run("choices over the kept size: their raw size counts", func(t *testing.T) {
		m := NewMeter(provider.Completions, EstimateTokens(int64(requestBytes)))
		m.Answered(200, false)
		m.body = newMemberScanner(map[string]int{"usage": maxUsageBytes, "choices": 4})
		m.Observe(provider.Event{Data: []byte(`{"choices":[{"text":"abcdefgh"}]}`)})
		units, flags := m.Settle(true)
		// [{"text":"abcdefgh"}] is 21 bytes → 6.
		expect(t, units, flags, withEveryTokenUnit(Units{config.UnitTokensIn: 11, config.UnitTokensOut: 6}), Flags{Estimated: true})
	})
	t.Run("embeddings generate nothing", func(t *testing.T) {
		body := `{"object":"list","data":[{"embedding":[0.1,0.2]}]}`
		units, flags := bodyMeter(provider.Embeddings, requestBytes, 200, body).Settle(true)
		expect(t, units, flags, withEveryTokenUnit(Units{config.UnitTokensIn: 11}), Flags{Estimated: true})
	})
	t.Run("malformed usage is no usage", func(t *testing.T) {
		body := `{"choices":[{"text":"abcd"}],"usage":{"total_tokens":"many"}}`
		units, flags := bodyMeter(provider.Completions, requestBytes, 200, body).Settle(true)
		expect(t, units, flags, withEveryTokenUnit(Units{config.UnitTokensIn: 11, config.UnitTokensOut: 1}), Flags{Estimated: true})
	})
}

func TestPartialUsage(t *testing.T) {
	t.Run("reported usage stays exact", func(t *testing.T) {
		m := streamMeter(provider.ChatCompletions, 40,
			`{"choices":[{"delta":{"content":"Hi"}}],"usage":{"prompt_tokens":5,"completion_tokens":1}}`)
		units, flags := m.Settle(false)
		expect(t, units, flags, withEveryTokenUnit(Units{config.UnitTokensIn: 5, config.UnitTokensOut: 1}), Flags{Partial: true})
	})
	t.Run("otherwise estimated from what was seen", func(t *testing.T) {
		m := streamMeter(provider.ChatCompletions, 40, `{"choices":[{"delta":{"content":"Hello"}}]}`)
		units, flags := m.Settle(false)
		expect(t, units, flags, withEveryTokenUnit(Units{config.UnitTokensIn: 10, config.UnitTokensOut: 2}), Flags{Estimated: true, Partial: true})
	})
	t.Run("a body cut short", func(t *testing.T) {
		units, flags := bodyMeter(provider.ChatCompletions, 40, 200, `{"choices":[{"message":{"content":"abcdefgh`).Settle(false)
		expect(t, units, flags, withEveryTokenUnit(Units{config.UnitTokensIn: 10, config.UnitTokensOut: 8}), Flags{Estimated: true, Partial: true})
	})
}

func TestNoAnswerOrErrorStatusCountsNothing(t *testing.T) {
	units, flags := NewMeter(provider.ChatCompletions, 100).Settle(false)
	expect(t, units, flags, withEveryTokenUnit(Units{}), Flags{Partial: true})

	m := bodyMeter(provider.ChatCompletions, 400, 400, `{"error":{"message":"context too long"}}`)
	units, flags = m.Settle(true)
	expect(t, units, flags, withEveryTokenUnit(Units{}), Flags{})
}

// A request written to the backend in full that gets no answer — a first-event
// or response timeout, the client gone, the drain's cut — counts the input, estimated; one the
// backend refused (its credential) or never got in full counts nothing.
func TestSentUnansweredCountsTheEstimatedInput(t *testing.T) {
	for _, ep := range []provider.Endpoint{provider.ChatCompletions, provider.Embeddings} {
		m := NewMeter(ep, 101)
		m.Sent()
		if !m.SentUnanswered() {
			t.Error("SentUnanswered false after Sent")
		}
		units, flags := m.Settle(false)
		expect(t, units, flags, withEveryTokenUnit(Units{config.UnitTokensIn: 101}), Flags{Estimated: true, Partial: true})
	}

	refused := NewMeter(provider.ChatCompletions, 101)
	refused.Sent()
	refused.Refused()
	units, flags := refused.Settle(true)
	expect(t, units, flags, withEveryTokenUnit(Units{}), Flags{Partial: true})

	answered := NewMeter(provider.ChatCompletions, 101)
	answered.Sent()
	answered.Answered(503, false)
	units, flags = answered.Settle(true)
	expect(t, units, flags, withEveryTokenUnit(Units{}), Flags{})
}

func TestEstimateTokens(t *testing.T) {
	for n, want := range map[int64]int64{0: 0, -3: 0, 1: 1, 4: 1, 5: 2, 400: 100} {
		if got := EstimateTokens(n); got != want {
			t.Errorf("EstimateTokens(%d) = %d, want %d", n, got, want)
		}
	}
}
