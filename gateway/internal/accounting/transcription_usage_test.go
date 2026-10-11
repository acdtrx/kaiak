package accounting

import (
	"maps"
	"math"
	"net/http"
	"testing"
	"time"

	"kaiak/internal/config"
	"kaiak/internal/provider"
)

// transcriptionUnits is a transcription record's units: its tokens and seconds of
// audio processed.
func transcriptionUnits(in, out, seconds int64) Units {
	return withEveryUnit(Units{config.UnitTokensIn: in, config.UnitTokensOut: out, config.UnitAudioSecondsIn: seconds})
}

// meterTranscription runs a transcription answer through a meter for request — its
// input estimated at 33 tokens, answered status with a JSON body in pieces of piece
// bytes — and settles it.
func meterTranscription(request Transcription, status int, body string, piece int, complete bool) (Units, Flags) {
	m := NewMeter(provider.AudioTranscriptions, 33, MediaRequest{Transcription: request})
	m.Answered(status, false, http.Header{"Content-Type": {"application/json"}})
	for b := []byte(body); len(b) > 0; {
		k := min(piece, len(b))
		m.Observe(provider.Event{Data: b[:k]})
		b = b[k:]
	}
	return m.Settle(complete)
}

// vLLM's answers (speech_to_text/base/serving.py, 0.30.0), and llama-server's
// (tools/server/server-chat.cpp, b11526).
const (
	vllmJSONAnswer    = `{"text":"Hello from the fake","usage":{"type":"duration","seconds":3}}`
	vllmVerboseAnswer = `{"duration":2.5,"language":"en","text":"Hello from the fake",` +
		`"segments":[{"id":0,"seek":0,"start":0.0,"end":2.5,"text":"Hello from the fake","tokens":[1,2],"temperature":0.0,` +
		`"avg_logprob":-0.1,"compression_ratio":1.0,"no_speech_prob":0.0}],"words":null}`
	llamaServerAnswer = `{"type":"transcript.text.done","text":"Hello from the fake",` +
		`"usage":{"type":"tokens","input_tokens":91,"output_tokens":6,"total_tokens":97,` +
		`"input_tokens_details":{"text_tokens":12,"audio_tokens":79}}}`
)

// vLLM reports a transcription's seconds and no tokens: audio_seconds_in is its
// usage's seconds (json, text — JSON too — diarized_json) or, for verbose_json, its
// duration rounded up to a whole second; the tokens are the input estimate, output 0,
// flagged estimated. Members and values are read after decoding: an escaped name or
// value is the one it stands for. The body is read as it passes, in pieces of any
// size (docs/specs/GATEWAY.md, Accounting → Media usage: speech-to-text).
func TestVLLMTranscriptionSeconds(t *testing.T) {
	for _, c := range []struct {
		name    string
		verbose bool
		body    string
		seconds int64
	}{
		{"json", false, vllmJSONAnswer, 3},
		{"a fractional seconds rounded up", false, `{"text":"","usage":{"type":"duration","seconds":2.01}}`, 3},
		{"diarized_json", false, `{"text":"hi","segments":[{"speaker":"A","text":"hi"}],` +
			`"usage":{"type":"duration","seconds":12}}`, 12},
		{"usage before the text", false, `{"usage":{"type":"duration","seconds":7},"text":"Hello"}`, 7},
		{"no audio", false, `{"text":"","usage":{"type":"duration","seconds":0}}`, 0},
		{"verbose_json", true, vllmVerboseAnswer, 3},
		{"verbose_json, whole seconds", true, `{"duration":4,"language":"en","text":"hi","segments":[],"words":null}`, 4},
		{"verbose_json, a hair past", true, `{"duration":4.000001,"text":"hi"}`, 5},
		{"verbose_json, in exponent form", true, `{"duration":1.25e1,"text":"hi"}`, 13},
		{"verbose_json answered with usage: the usage's seconds", true,
			`{"duration":2.5,"text":"hi","usage":{"type":"duration","seconds":9}}`, 9},
		{"verbose_json, its duration named twice: the last", true, `{"duration":99,"text":"hi","duration":2}`, 2},
		{"verbose_json, the name escaped", true, `{"d\u0075ration":6.5,"text":"hi"}`, 7},
		{"usage and seconds escaped", false, `{"text":"hi","\u0075sage":{"type":"d\u0075ration","sec\u006fnds":5}}`, 5},
	} {
		for _, piece := range []int{1, 7, len(c.body)} {
			units, flags := meterTranscription(Transcription{VerboseJSON: c.verbose}, http.StatusOK, c.body, piece, true)
			if !maps.Equal(units, transcriptionUnits(33, 0, c.seconds)) || !flags.Estimated || flags.Partial {
				t.Errorf("%s (pieces of %d): %v %+v, want %d seconds, the input estimate, estimated", c.name, piece, units,
					flags, c.seconds)
			}
		}
	}
}

// llama-server reports tokens and no seconds: its usage of type tokens gives tokens_in
// and tokens_out, every other unit 0, unestimated; its seconds are the upload's
// measured length, and with none measured 0, the record estimated
// (docs/specs/GATEWAY.md, Accounting → Media usage: speech-to-text).
func TestLlamaServerTranscriptionTokens(t *testing.T) {
	for _, c := range []struct {
		name    string
		request Transcription
		seconds int64
		flags   Flags
	}{
		{"no upload measured", Transcription{}, 0, Flags{Estimated: true}},
		{"the upload measured", Transcription{Upload: 2*time.Second + time.Millisecond, UploadMeasured: true}, 3, Flags{}},
		{"an upload of no audio", Transcription{Upload: 0, UploadMeasured: true}, 0, Flags{}},
	} {
		units, flags := meterTranscription(c.request, http.StatusOK, llamaServerAnswer, 5, true)
		if !maps.Equal(units, transcriptionUnits(91, 6, c.seconds)) || flags != c.flags {
			t.Errorf("%s: %v %+v, want 91 in, 6 out, %d seconds, %+v", c.name, units, flags, c.seconds, c.flags)
		}
	}
}

// An answer whose seconds are missing or malformed reports none: its record counts the
// upload's measured length, else 0 flagged estimated; a usage that is no token report
// leaves the tokens estimated. Members are read by their exact names after decoding —
// "Seconds" or "TYPE" is another member — and from where the answer puts them, never
// from a usage nested deeper or written in a string; a duration is read only for a
// verbose_json request (docs/specs/GATEWAY.md, Accounting → Media usage:
// speech-to-text).
func TestTranscriptionMissingOrMalformedUsage(t *testing.T) {
	for _, c := range []struct {
		name    string
		verbose bool
		body    string
	}{
		{"no usage", false, `{"text":"hi"}`},
		{"usage null", false, `{"text":"hi","usage":null}`},
		{"usage a string", false, `{"text":"hi","usage":"3 seconds"}`},
		{"usage a list", false, `{"text":"hi","usage":[{"type":"duration","seconds":3}]}`},
		{"no type", false, `{"text":"hi","usage":{"seconds":3}}`},
		{"type in another case", false, `{"text":"hi","usage":{"type":"Duration","seconds":3}}`},
		{"type under another name", false, `{"text":"hi","usage":{"TYPE":"duration","seconds":3}}`},
		{"seconds under another name", false, `{"text":"hi","usage":{"type":"duration","Seconds":3}}`},
		{"seconds a string", false, `{"text":"hi","usage":{"type":"duration","seconds":"3"}}`},
		{"seconds null", false, `{"text":"hi","usage":{"type":"duration","seconds":null}}`},
		{"seconds past float64", false, `{"text":"hi","usage":{"type":"duration","seconds":1e999}}`},
		{"usage nested in another member", false, `{"text":"hi","result":{"usage":{"type":"duration","seconds":3}}}`},
		{"usage written in the text", false, `{"text":"\"usage\":{\"type\":\"duration\",\"seconds\":3}"}`},
		{"a duration on a json request", false, `{"duration":3,"text":"hi"}`},
		{"verbose_json without duration", true, `{"language":"en","text":"hi","segments":[]}`},
		{"verbose_json, duration a string", true, `{"duration":"2.5","text":"hi"}`},
		{"verbose_json, duration null", true, `{"duration":null,"text":"hi"}`},
		{"verbose_json, Duration", true, `{"Duration":2.5,"text":"hi"}`},
		{"verbose_json, a duration nested", true, `{"text":"hi","segments":[{"duration":2.5}]}`},
		{"not JSON", false, `Hello from the fake`},
		{"a JSON list", false, `[{"usage":{"type":"duration","seconds":3}}]`},
		{"cut before the usage closed", false, `{"text":"hi","usage":{"type":"duration","seconds":3`},
		{"tokens without an input count", false, `{"text":"hi","usage":{"type":"tokens","output_tokens":6}}`},
		{"tokens of another type", false, `{"text":"hi","usage":{"type":"token","input_tokens":91,"output_tokens":6}}`},
		{"tokens a fraction", false, `{"text":"hi","usage":{"type":"tokens","input_tokens":9.5,"output_tokens":6}}`},
		{"tokens under other names", false, `{"text":"hi","usage":{"type":"tokens","INPUT_TOKENS":91,"Output_Tokens":6}}`},
		{"tokens as OpenAI's chat names them", false,
			`{"text":"hi","usage":{"type":"tokens","prompt_tokens":91,"completion_tokens":6}}`},
	} {
		for _, upload := range []Transcription{{}, {Upload: 1500 * time.Millisecond, UploadMeasured: true}} {
			upload.VerboseJSON = c.verbose
			units, flags := meterTranscription(upload, http.StatusOK, c.body, 3, true)
			seconds := int64(0)
			if upload.UploadMeasured {
				seconds = 2
			}
			if !maps.Equal(units, transcriptionUnits(33, 0, seconds)) || !flags.Estimated || flags.Partial {
				t.Errorf("%s (upload measured %v): %v %+v, want %d seconds and the input estimate, estimated", c.name,
					upload.UploadMeasured, units, flags, seconds)
			}
		}
	}
}

// A count past what an answer can mean saturates and a negative one is 0, as in every
// usage object the gateway reads: a negative token count is 0, a negative length 0
// seconds; a length past int64 the most an amount holds, which the record clamps
// (docs/specs/GATEWAY.md, Accounting → Protocol bound).
func TestTranscriptionCountsAtTheirEdges(t *testing.T) {
	for _, c := range []struct {
		body             string
		verbose          bool
		in, out, seconds int64
		estimated        bool
	}{
		{`{"usage":{"type":"duration","seconds":-4}}`, false, 33, 0, 0, true},
		{`{"duration":-0.5}`, true, 33, 0, 0, true},
		{`{"usage":{"type":"duration","seconds":1e300}}`, false, 33, 0, math.MaxInt64, true},
		{`{"usage":{"type":"duration","seconds":9223372036854775807}}`, false, 33, 0, math.MaxInt64, true},
		{`{"usage":{"type":"tokens","input_tokens":-3,"output_tokens":-1}}`, false, 0, 0, 0, true},
		{`{"usage":{"type":"tokens","input_tokens":5,"output_tokens":0}}`, false, 5, 0, 0, true},
	} {
		units, flags := meterTranscription(Transcription{VerboseJSON: c.verbose}, http.StatusOK, c.body, 4, true)
		if !maps.Equal(units, transcriptionUnits(c.in, c.out, c.seconds)) || flags.Estimated != c.estimated {
			t.Errorf("%s: %v %+v, want %d in, %d out, %d seconds, estimated %v", c.body, units, flags, c.in, c.out,
				c.seconds, c.estimated)
		}
	}
	for seconds, want := range map[float64]int64{0: 0, 0.0000001: 1, 1: 1, 1.5: 2, 59.999: 60, -1e300: 0,
		math.MaxInt64: math.MaxInt64, 9.2e18: 9_200_000_000_000_000_000} {
		if got := roundedUpSeconds(seconds); got != want {
			t.Errorf("roundedUpSeconds(%g) = %d, want %d", seconds, got, want)
		}
	}
}

// A transcription cut short counts the seconds and tokens its answer reported before
// it broke, else as one that reported none — the upload's measured length, else 0
// estimated — and is flagged partial; an error status counts nothing; an event stream
// answering one is no answer the reader reads; a transcription is never timed to a
// first token (docs/specs/GATEWAY.md, Accounting → Partial; Providers: media answers).
func TestTranscriptionCutShortRefusedOrStreamed(t *testing.T) {
	cut := `{"usage":{"type":"duration","seconds":3},"text":"Hello fr`
	units, flags := meterTranscription(Transcription{}, http.StatusOK, cut, 4, false)
	if !maps.Equal(units, transcriptionUnits(33, 0, 3)) || !flags.Estimated || !flags.Partial {
		t.Errorf("cut after its usage: %v %+v, want its 3 seconds, estimated and partial", units, flags)
	}
	cut = llamaServerAnswer[:40]
	units, flags = meterTranscription(Transcription{Upload: 4 * time.Second, UploadMeasured: true}, http.StatusOK, cut, 4, false)
	if !maps.Equal(units, transcriptionUnits(33, 0, 4)) || !flags.Estimated || !flags.Partial {
		t.Errorf("cut before its usage: %v %+v, want the upload's 4 seconds, estimated input, partial", units, flags)
	}

	units, flags = meterTranscription(Transcription{}, http.StatusBadRequest,
		`{"error":{"message":"Unsupported response_format","type":"BadRequestError","param":null,"code":400}}`, 8, true)
	if !maps.Equal(units, transcriptionUnits(0, 0, 0)) || flags.Estimated || flags.Partial {
		t.Errorf("a 400: %v %+v, want no units", units, flags)
	}

	m := NewMeter(provider.AudioTranscriptions, 33, MediaRequest{Transcription: Transcription{}})
	m.Answered(http.StatusOK, true, http.Header{"Content-Type": {"text/event-stream"}})
	m.Observe(provider.Event{Data: []byte("data: x\n\n"), Payload: []byte(`{"type":"transcript.text.done","text":"hi",` +
		`"usage":{"type":"tokens","input_tokens":91,"output_tokens":6}}`)})
	if m.StreamContentSeen() {
		t.Error("a transcription was timed to a first token")
	}
	units, flags = m.Settle(false)
	if !maps.Equal(units, transcriptionUnits(33, 0, 0)) || !flags.Estimated || !flags.Partial {
		t.Errorf("an event stream: %v %+v, want the input estimate and 0 seconds, estimated and partial", units, flags)
	}
}
