package accounting

import (
	"maps"
	"net/http"
	"testing"

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
