package accounting

import (
	"encoding/base64"
	"maps"
	"net/http"
	"slices"
	"testing"
	"time"

	"kaiak/internal/config"
	"kaiak/internal/provider"
)

// speechUnits are a speech record's units: its tokens and seconds of audio
// generated, every other unit 0.
func speechUnits(in, out, seconds int64) Units {
	return withEveryUnit(Units{config.UnitTokensIn: in, config.UnitTokensOut: out, config.UnitAudioSecondsOut: seconds})
}

// speechAnswer is a speech answer's headers as net/http hands them over, names
// canonical whatever case the backend wrote them in: its content type and, when usage
// is set, the two token headers.
func speechAnswer(contentType string, usage ...string) http.Header {
	h := http.Header{"Content-Type": {contentType}}
	if len(usage) == 2 {
		h.Set("X-VLLM-OMNI-INPUT-TOKENS", usage[0])
		h.Set("X-VLLM-OMNI-OUTPUT-TOKENS", usage[1])
	}
	return h
}

// meterSpeech runs a speech answer through a meter: the answer's status and headers,
// then body (in pieces of piece bytes), then settles it.
func meterSpeech(speech Speech, status int, header http.Header, body []byte, piece int, complete bool) (Units, Flags) {
	m := NewMeter(provider.AudioSpeech, 33, speech)
	m.Answered(status, false, header)
	for len(body) > 0 {
		n := min(piece, len(body))
		m.Observe(provider.Event{Data: body[:n]})
		body = body[n:]
	}
	return m.Settle(complete)
}

// wav16 is a whole WAV file of 16-bit mono at rate holding bytes of audio.
func wav16(rate uint32, bytes int) []byte {
	return riff(0, wavChunk("fmt ", fmtBody(1, 1, rate, 2, 16)), wavChunk("data", make([]byte, bytes)))
}

// Speech usage (docs/specs/GATEWAY.md, Accounting → Media usage): the whole clip's
// tokens from its two headers, both present once as decimal integers, else no report: the input estimated, no output, flagged estimated. In
// every case the audio relayed is measured and rounded up once.
func TestSpeechUsageFromHeaders(t *testing.T) {
	clip := wav16(24000, 60000) // 1.25 s
	for _, c := range []struct {
		name      string
		header    http.Header
		want      Units
		estimated bool
	}{
		{"reported", speechAnswer("audio/wav", "21", "83"), speechUnits(21, 83, 2), false},
		{"reported zero", speechAnswer("audio/wav", "0", "0"), speechUnits(0, 0, 2), false},
		{"leading zeros", speechAnswer("audio/wav", "007", "0083"), speechUnits(7, 83, 2), false},
		{"past int64", speechAnswer("audio/wav", "99999999999999999999", "1"), speechUnits(1<<63-1, 1, 2), false},
		{"missing", speechAnswer("audio/wav"), speechUnits(33, 0, 2), true},
		{"input only", http.Header{"Content-Type": {"audio/wav"}, "X-Vllm-Omni-Input-Tokens": {"21"}}, speechUnits(33, 0, 2), true},
		{"not a number", speechAnswer("audio/wav", "21", "lots"), speechUnits(33, 0, 2), true},
		{"negative", speechAnswer("audio/wav", "-21", "83"), speechUnits(33, 0, 2), true},
		{"signed", speechAnswer("audio/wav", "+21", "83"), speechUnits(33, 0, 2), true},
		{"fractional", speechAnswer("audio/wav", "21", "8.5"), speechUnits(33, 0, 2), true},
		{"empty", speechAnswer("audio/wav", "", "83"), speechUnits(33, 0, 2), true},
		{"named twice", http.Header{"Content-Type": {"audio/wav"}, "X-Vllm-Omni-Input-Tokens": {"21", "22"},
			"X-Vllm-Omni-Output-Tokens": {"83"}}, speechUnits(33, 0, 2), true},
	} {
		units, flags := meterSpeech(Speech{}, http.StatusOK, c.header, clip, 1000, true)
		if !maps.Equal(units, c.want) || flags.Estimated != c.estimated || flags.Partial {
			t.Errorf("%s: %v %+v, want %v estimated %v", c.name, units, flags, c.want, c.estimated)
		}
	}
}

// The raw-audio stream reports no usage, ever: its tokens are estimated — input from
// the request, output 0 — and flagged, headers or not; its audio is measured as on
// the whole clip, WAV with placeholder sizes or PCM at the request's rate.
func TestRawAudioStreamIsEstimated(t *testing.T) {
	streamedWAV := append(riff(0xFFFFFFFF, wavChunk("fmt ", fmtBody(1, 1, 24000, 2, 16)), dataHeader(0xFFFFFFFF)),
		make([]byte, 96000)...)
	for _, c := range []struct {
		name   string
		speech Speech
		header http.Header
		body   []byte
		want   Units
	}{
		{"wav", Speech{RawStream: true}, speechAnswer("audio/wav", "21", "83"), streamedWAV, speechUnits(33, 0, 2)},
		{"pcm", Speech{RawStream: true, SampleRate: 8000}, speechAnswer("audio/pcm"), make([]byte, 16001), speechUnits(33, 0, 2)},
	} {
		units, flags := meterSpeech(c.speech, http.StatusOK, c.header, c.body, 997, true)
		if !maps.Equal(units, c.want) || !flags.Estimated || flags.Partial {
			t.Errorf("%s: %v %+v, want %v estimated", c.name, units, flags, c.want)
		}
	}
}

// Audio the meter cannot measure — a content type other than WAV and PCM, PCM with no
// rate, a WAV header it cannot read — counts 0 seconds and flags the record
// estimated, its reported tokens kept.
func TestUnmeasurableSpeechAudioIsEstimated(t *testing.T) {
	for name, c := range map[string]struct {
		speech Speech
		header http.Header
		body   []byte
	}{
		"mp3":            {Speech{}, speechAnswer("audio/mpeg", "21", "83"), make([]byte, 4000)},
		"pcm, no rate":   {Speech{}, speechAnswer("audio/pcm", "21", "83"), make([]byte, 4000)},
		"unreadable wav": {Speech{}, speechAnswer("audio/wav", "21", "83"), append([]byte("RIFX"), wav16(24000, 4000)[4:]...)},
	} {
		units, flags := meterSpeech(c.speech, http.StatusOK, c.header, c.body, 1000, true)
		if want := speechUnits(21, 83, 0); !maps.Equal(units, want) || !flags.Estimated {
			t.Errorf("%s: %v %+v, want %v estimated", name, units, flags, want)
		}
	}
}

// A clip broken off counts the audio relayed, rounded up, and keeps the whole clip's
// header usage, which came with its status; an error answer records no units, its
// audio not read.
func TestPartialAndRefusedSpeech(t *testing.T) {
	clip := wav16(24000, 120000)
	units, flags := meterSpeech(Speech{}, http.StatusOK, speechAnswer("audio/wav", "21", "83"), clip[:44+30000], 512, false)
	if want := speechUnits(21, 83, 1); !maps.Equal(units, want) || flags.Estimated || !flags.Partial {
		t.Errorf("broken off after 0.625 s: %v %+v, want %v partial", units, flags, want)
	}
	units, flags = meterSpeech(Speech{}, http.StatusBadRequest, http.Header{"Content-Type": {"application/json"}},
		[]byte(`{"error":{"message":"TTS model did not produce audio output."}}`), 64, true)
	if want := speechUnits(0, 0, 0); !maps.Equal(units, want) || flags.Estimated || flags.Partial {
		t.Errorf("400: %v %+v, want no units", units, flags)
	}
}

// The first audio is the first samples relayed, not a WAV header alone: the moment
// StreamContentSeen turns true times a raw-audio stream's first token.
func TestSpeechContentSeenIsTheFirstSamples(t *testing.T) {
	m := NewMeter(provider.AudioSpeech, 10, Speech{RawStream: true})
	m.Answered(http.StatusOK, false, speechAnswer("audio/wav"))
	m.Observe(provider.Event{Data: riff(0xFFFFFFFF, wavChunk("fmt ", fmtBody(1, 1, 24000, 2, 16)), dataHeader(0xFFFFFFFF))})
	if m.StreamContentSeen() {
		t.Error("a WAV header alone seen as content")
	}
	m.Observe(provider.Event{Data: []byte{0, 0}})
	if !m.StreamContentSeen() {
		t.Error("samples relayed, not seen as content")
	}
}

// speechDelta is a speech.audio.delta event's data: audio, base64 as given, in format.
func speechDelta(format, audio string) []byte {
	return []byte(`{"type":"speech.audio.delta","audio":"` + audio + `","response_format":"` + format + `"}`)
}

// b64 is p in standard base64, padded, as vLLM-Omni encodes a delta's audio.
func b64(p []byte) string { return base64.StdEncoding.EncodeToString(p) }

// meterSpeechEvents runs an SSE speech answer through a meter — its headers, then its
// events' data — and settles it.
func meterSpeechEvents(speech Speech, header http.Header, events [][]byte, complete bool) (Units, Flags) {
	m := NewMeter(provider.AudioSpeech, 33, speech)
	if header == nil {
		header = http.Header{"Content-Type": {"text/event-stream"}}
	}
	m.Answered(http.StatusOK, true, header)
	for _, ev := range events {
		m.Observe(provider.Event{Data: ev, Payload: ev})
	}
	return m.Settle(complete)
}

// The SSE stream's tokens are speech.audio.done's usage, its input_tokens and
// output_tokens both integers, else no report: the input estimated, no output, flagged
// (docs/specs/GATEWAY.md, Accounting → Media usage). Its headers carry none: the
// whole clip's usage headers on an event stream are not read.
func TestSpeechEventStreamUsage(t *testing.T) {
	audio := speechDelta("pcm", b64(make([]byte, 24000))) // 1.5 s at 8 kHz
	done := func(usage string) []byte { return []byte(`{"type":"speech.audio.done","usage":` + usage + `}`) }
	for _, c := range []struct {
		name      string
		done      []byte
		header    http.Header
		want      Units
		estimated bool
	}{
		{"reported", done(`{"input_tokens":21,"output_tokens":83,"total_tokens":104,` +
			`"input_token_details":{"text_tokens":20,"audio_tokens":1}}`), nil, speechUnits(21, 83, 2), false},
		{"reported zero", done(`{"input_tokens":0,"output_tokens":0}`), nil, speechUnits(0, 0, 2), false},
		{"negative", done(`{"input_tokens":-4,"output_tokens":83}`), nil, speechUnits(0, 83, 2), false},
		{"missing", []byte(`{"type":"speech.audio.done"}`), nil, speechUnits(33, 0, 2), true},
		{"null", done(`null`), nil, speechUnits(33, 0, 2), true},
		{"not an object", done(`[21,83]`), nil, speechUnits(33, 0, 2), true},
		{"input only", done(`{"input_tokens":21}`), nil, speechUnits(33, 0, 2), true},
		{"a null count", done(`{"input_tokens":21,"output_tokens":null}`), nil, speechUnits(33, 0, 2), true},
		{"a count as text", done(`{"input_tokens":"21","output_tokens":83}`), nil, speechUnits(33, 0, 2), true},
		{"fractional", done(`{"input_tokens":21,"output_tokens":8.5}`), nil, speechUnits(33, 0, 2), true},
		{"past int64", done(`{"input_tokens":99999999999999999999,"output_tokens":83}`), nil, speechUnits(33, 0, 2), true},
		{"in headers only", []byte(`{"type":"speech.audio.done"}`), speechAnswer("text/event-stream", "21", "83"),
			speechUnits(33, 0, 2), true},
	} {
		units, flags := meterSpeechEvents(Speech{SampleRate: 8000}, c.header, [][]byte{audio, c.done}, true)
		if !maps.Equal(units, c.want) || flags.Estimated != c.estimated || flags.Partial {
			t.Errorf("%s: %v %+v, want %v estimated %v", c.name, units, flags, c.want, c.estimated)
		}
	}
}

// eventClip reads an SSE answer's deltas into a speech reader and returns its audio's
// exact length.
func eventClip(speech Speech, deltas ...[]byte) (time.Duration, bool) {
	u := newSpeechUsage(speech)
	u.answered(http.Header{"Content-Type": {"text/event-stream"}}, true)
	for _, d := range deltas {
		u.streamEvent(d)
	}
	return u.clip.length()
}

// A delta's audio counts by its decoded size, from its base64 length and padding —
// never decoded whole — in the clip its response_format names: PCM at the request's
// sample_rate; WAV from its own header, the only bytes decoded, then the data
// received, at most the size the data chunk declares. vLLM-Omni sends a WAV stream's
// header as a delta of its own; a header split across deltas, or sharing one with its
// audio, reads the same; a chunk before the data is skipped by its size, not decoded
// (docs/specs/GATEWAY.md, Accounting → Media usage).
func TestSpeechDeltaLength(t *testing.T) {
	streamed := riff(0xFFFFFFFF, wavChunk("fmt ", fmtBody(1, 1, 24000, 2, 16)), dataHeader(0xFFFFFFFF))
	data := make([]byte, 30001) // 0.625 s and one byte: 30 001 bytes at 48 000 bytes a second
	wav := func(audio ...string) [][]byte {
		var deltas [][]byte
		for _, a := range audio {
			deltas = append(deltas, speechDelta("wav", a))
		}
		return deltas
	}
	whole := append(slices.Clone(streamed), data...)
	listed := riff(0xFFFFFFFF, wavChunk("fmt ", fmtBody(1, 1, 24000, 2, 16)), wavChunk("LIST", make([]byte, 101)),
		dataHeader(0xFFFFFFFF))
	sized := append(riff(0, wavChunk("fmt ", fmtBody(1, 1, 24000, 2, 16)), wavChunk("data", make([]byte, 48000))),
		wavChunk("junk", make([]byte, 9000))...)
	for _, c := range []struct {
		name   string
		speech Speech
		deltas [][]byte
		want   time.Duration
	}{
		{"pcm", Speech{SampleRate: 8000}, [][]byte{speechDelta("pcm", b64(make([]byte, 10000))),
			speechDelta("pcm", b64(make([]byte, 20000)))}, 1875 * time.Millisecond},
		{"pcm, unpadded", Speech{SampleRate: 8000}, [][]byte{speechDelta("pcm", base64.RawStdEncoding.EncodeToString(make([]byte, 10001))),
			speechDelta("pcm", base64.RawStdEncoding.EncodeToString(make([]byte, 10000)))}, 1250062500 * time.Nanosecond},
		{"pcm, padded by one and two", Speech{SampleRate: 8000}, [][]byte{speechDelta("pcm", b64(make([]byte, 2))),
			speechDelta("pcm", b64(make([]byte, 1)))}, 187500 * time.Nanosecond},
		// Never decoded: a PCM delta counts by its length, whatever its characters.
		{"pcm, counted not decoded", Speech{SampleRate: 8000}, [][]byte{speechDelta("pcm", "!!!!!!!!")}, 375 * time.Microsecond},
		{"pcm, empty", Speech{SampleRate: 8000}, [][]byte{speechDelta("pcm", "")}, 0},
		{"wav, header its own delta", Speech{}, wav(b64(streamed), b64(data[:15000]), b64(data[15000:])), 625020833 * time.Nanosecond},
		{"wav, unpadded", Speech{}, wav(base64.RawStdEncoding.EncodeToString(streamed), base64.RawStdEncoding.EncodeToString(data)),
			625020833 * time.Nanosecond},
		{"wav, header split across deltas", Speech{}, wav(b64(streamed[:3]), b64(streamed[3:13]), b64(streamed[13:40]),
			b64(streamed[40:]), b64(data)), 625020833 * time.Nanosecond},
		{"wav, header and audio in one delta", Speech{}, wav(b64(whole)), 625020833 * time.Nanosecond},
		{"wav, header and audio split mid-header", Speech{}, wav(b64(whole[:20]), b64(whole[20:])), 625020833 * time.Nanosecond},
		{"wav, a chunk before the data", Speech{}, wav(b64(listed[:30]), b64(append(slices.Clone(listed[30:]), data...))),
			625020833 * time.Nanosecond},
		{"wav, at most the declared size", Speech{}, wav(b64(sized[:44]), b64(sized[44:])), time.Second},
		{"wav, the header alone", Speech{}, wav(b64(streamed)), 0},
		{"no delta", Speech{}, nil, 0},
	} {
		d, measured := eventClip(c.speech, c.deltas...)
		if d != c.want || !measured {
			t.Errorf("%s: %s, measured %v; want %s", c.name, d, measured, c.want)
		}
	}
}

// The decoded size of base64 text is three bytes for every four characters, one or
// two for a last group of two or three, padded or not; a lone character past the last
// whole group counts none, as do the padding characters.
func TestBase64Size(t *testing.T) {
	for text, want := range map[string]int64{"": 0, "A": 0, "AA": 1, "AA=": 1, "AA==": 1, "AAA": 2, "AAA=": 2, "AAAA": 3,
		"AAAAA": 3, "AAAAAA": 4, "AAAAAA==": 4, "AAAAAAA=": 5, "AAAAAAAA": 6, "AAAA===": 3} {
		if got := base64Size(base64Text(text)); got != want {
			t.Errorf("%q: %d, want %d", text, got, want)
		}
	}
}

// An SSE answer whose audio cannot be measured counts 0 seconds and flags the record
// estimated, its reported tokens kept: a format other than WAV and PCM, a delta naming
// none, PCM with no rate, a delta naming another format than the first, a WAV header
// that is not base64 or that the clip cannot read.
func TestUnmeasurableSpeechEventsAreEstimated(t *testing.T) {
	done := []byte(`{"type":"speech.audio.done","usage":{"input_tokens":21,"output_tokens":83}}`)
	streamed := riff(0xFFFFFFFF, wavChunk("fmt ", fmtBody(1, 1, 24000, 2, 16)), dataHeader(0xFFFFFFFF))
	for name, c := range map[string]struct {
		speech Speech
		deltas [][]byte
	}{
		"mp3":           {Speech{}, [][]byte{speechDelta("mp3", b64(make([]byte, 4000)))}},
		"no format":     {Speech{SampleRate: 8000}, [][]byte{[]byte(`{"type":"speech.audio.delta","audio":"AAAA"}`)}},
		"pcm, no rate":  {Speech{}, [][]byte{speechDelta("pcm", b64(make([]byte, 4000)))}},
		"formats mixed": {Speech{SampleRate: 8000}, [][]byte{speechDelta("pcm", b64(make([]byte, 4000))), speechDelta("wav", b64(streamed))}},
		// The data chunk's header is not base64; the next delta's would be readable.
		"wav not base64": {Speech{}, [][]byte{speechDelta("wav", b64(streamed[:36])), speechDelta("wav", "!!!!!!!!!!!!"),
			speechDelta("wav", b64(append(dataHeader(0xFFFFFFFF), make([]byte, 4000)...)))}},
		"wav unreadable": {Speech{}, [][]byte{speechDelta("wav", b64(append([]byte("RIFX"), streamed[4:]...))),
			speechDelta("wav", b64(make([]byte, 4000)))}},
	} {
		units, flags := meterSpeechEvents(c.speech, nil, append(c.deltas, done), true)
		if want := speechUnits(21, 83, 0); !maps.Equal(units, want) || !flags.Estimated || flags.Partial {
			t.Errorf("%s: %v %+v, want %v estimated", name, units, flags, want)
		}
	}
}

// An SSE stream that ends before speech.audio.done — broken off, or ended by an error
// event — counts the audio its deltas relayed, rounded up, with its tokens estimated:
// the input estimate, no output, flagged estimated and partial (docs/specs/GATEWAY.md,
// Accounting → Partial).
func TestSpeechEventStreamBrokenBeforeDone(t *testing.T) {
	streamed := riff(0xFFFFFFFF, wavChunk("fmt ", fmtBody(1, 1, 24000, 2, 16)), dataHeader(0xFFFFFFFF))
	deltas := [][]byte{speechDelta("wav", b64(streamed)), speechDelta("wav", b64(make([]byte, 30000))),
		speechDelta("wav", b64(make([]byte, 30000)))} // 1.25 s
	errorEvent := []byte(`{"type":"speech.audio.error","error":{"message":"x","type":"server_error","param":null,"code":500,` +
		`"partial_audio":true,"action":"discard"}}`)
	for name, events := range map[string][][]byte{"broken off": deltas, "an error event": append(slices.Clone(deltas), errorEvent)} {
		units, flags := meterSpeechEvents(Speech{}, nil, events, false)
		if want := speechUnits(33, 0, 2); !maps.Equal(units, want) || !flags.Estimated || !flags.Partial {
			t.Errorf("%s: %v %+v, want %v estimated, partial", name, units, flags, want)
		}
	}
}

// An SSE stream's first audio is its first delta carrying samples — not a WAV
// header's delta, not an empty one: the moment StreamContentSeen turns true times its
// first token.
func TestSpeechEventContentSeenIsTheFirstSamples(t *testing.T) {
	m := NewMeter(provider.AudioSpeech, 10, Speech{})
	m.Answered(http.StatusOK, true, http.Header{"Content-Type": {"text/event-stream"}})
	for _, ev := range [][]byte{
		speechDelta("wav", b64(riff(0xFFFFFFFF, wavChunk("fmt ", fmtBody(1, 1, 24000, 2, 16)), dataHeader(0xFFFFFFFF)))),
		speechDelta("wav", ""),
	} {
		m.Observe(provider.Event{Data: ev, Payload: ev})
		if m.StreamContentSeen() {
			t.Fatalf("%s seen as content", ev)
		}
	}
	ev := speechDelta("wav", b64([]byte{0, 0}))
	m.Observe(provider.Event{Data: ev, Payload: ev})
	if !m.StreamContentSeen() {
		t.Error("samples relayed, not seen as content")
	}
}
