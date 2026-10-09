package accounting

import (
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strconv"

	"kaiak/internal/config"
)

// Speech is what a speech request says about the answer it gets, which its meter
// reads (docs/specs/GATEWAY.md, Client API → owned fields: speech).
type Speech struct {
	// SampleRate is the request's sample_rate; 0 when it names none. A PCM answer is
	// 16-bit mono at that rate: sample_rate is required with pcm.
	SampleRate int64
	// RawStream: the request asked for the raw-audio stream, which reports no usage.
	RawStream bool
}

// The headers in which speech's whole clip reports its token usage (vLLM-Omni,
// serving_speech.py, v0.30.0, which writes them upper-case; header names are
// case-insensitive).
const (
	speechInputTokensHeader  = "X-Vllm-Omni-Input-Tokens"
	speechOutputTokensHeader = "X-Vllm-Omni-Output-Tokens"
)

// speechUsage reads speech answers (docs/specs/GATEWAY.md, Accounting → Media usage):
// the whole clip's tokens from its response headers — input_tokens as tokens_in,
// output_tokens as tokens_out, every other token unit 0 — and none on the raw-audio
// stream, which is estimated, its output 0: the output is audio, and a model's
// codec-token rate is unknown to the gateway. In either mode the audio relayed is
// measured as it streams through (audioClip) into audio_seconds_out. An event stream
// answering a speech request carries nothing the meter reads.
type speechUsage struct {
	request Speech
	latestReport
	// clip is the answer's audio, known once the backend answered.
	clip audioClip
}

func newSpeechUsage(request Speech) *speechUsage {
	return &speechUsage{request: request, clip: unmeasuredClip{}}
}

func (u *speechUsage) streamEvent([]byte) {}

// contentMember: none — the body is audio.
func (u *speechUsage) contentMember() string { return "" }

func (u *speechUsage) bodyUsage(json.RawMessage) {}

func (u *speechUsage) bodyContentBytes(json.RawMessage) int64 { return 0 }

func (u *speechUsage) streamContentBytes() int64 { return 0 }

// answered reads a successful answer's headers: its content type says how its audio
// is measured, and on the whole clip they carry its token usage.
func (u *speechUsage) answered(header http.Header) {
	u.clip = newAudioClip(header.Get("Content-Type"), u.request.SampleRate)
	if !u.request.RawStream {
		u.keep(speechHeaderUnits(header))
	}
}

func (u *speechUsage) bodyPiece(p []byte) { u.clip.write(p) }

// media: the audio relayed, in whole seconds rounded up; an answer whose audio cannot
// be measured counts 0, unmeasured.
func (u *speechUsage) media() (Units, bool) {
	d, measured := u.clip.length()
	return Units{config.UnitAudioSecondsOut: wholeSeconds(d)}, measured
}

func (u *speechUsage) mediaSeen() bool { return u.clip.samplesSeen() }

// speechHeaderUnits reads the whole clip's token usage from its headers: both counts
// present, once each, as decimal integers, else no report (a speech server in
// pure-diffusion mode sends none).
func speechHeaderUnits(h http.Header) (Units, bool) {
	in, inOK := headerCount(h, speechInputTokensHeader)
	out, outOK := headerCount(h, speechOutputTokensHeader)
	if !inOK || !outOK {
		return nil, false
	}
	return withEveryUnit(Units{config.UnitTokensIn: in, config.UnitTokensOut: out}), true
}

// headerCount reads header name as a count: one value of decimal digits alone, no
// sign. A count past int64 saturates (the record clamps it to the protocol's bound).
func headerCount(h http.Header, name string) (int64, bool) {
	values := h.Values(name)
	if len(values) != 1 {
		return 0, false
	}
	n, err := strconv.ParseUint(values[0], 10, 63)
	switch {
	case errors.Is(err, strconv.ErrRange):
		return math.MaxInt64, true
	case err != nil:
		return 0, false
	}
	return int64(n), true
}
