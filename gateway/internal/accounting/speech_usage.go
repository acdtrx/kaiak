package accounting

import (
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strconv"

	"kaiak/internal/config"
)

// Speech is what a speech request or a speech batch says about the answer it gets,
// which its meter reads (docs/specs/GATEWAY.md, Client API → owned fields: speech,
// speech batch).
type Speech struct {
	// SampleRate is the request's sample_rate; 0 when it names none. A PCM answer is
	// 16-bit mono at that rate: sample_rate is required with pcm.
	SampleRate int64
	// RawStream: the request asked for the raw-audio stream, which reports no usage.
	RawStream bool
	// ItemSampleRates are a speech batch's items' sample rates, by position: each
	// item's own, else the batch's; 0 where an item gets none.
	ItemSampleRates []int64
}

// The headers in which speech's whole clip reports its token usage (vLLM-Omni,
// serving_speech.py, v0.30.0, which writes them upper-case; header names are
// case-insensitive).
const (
	speechInputTokensHeader  = "X-Vllm-Omni-Input-Tokens"
	speechOutputTokensHeader = "X-Vllm-Omni-Output-Tokens"
)

// speechUsage reads speech answers (docs/specs/GATEWAY.md, Accounting → Media usage):
// input_tokens as tokens_in, output_tokens as tokens_out, every other token unit 0 —
// the whole clip's from its response headers, the SSE stream's from its
// speech.audio.done event, and none on the raw-audio stream, which is estimated, its
// output 0: the output is audio, and a model's codec-token rate is unknown to the
// gateway. In every mode the audio the backend sent while the client was there, the
// last piece read included, is measured as it streams through (audioClip) into
// audio_seconds_out: an audio body's bytes, the SSE stream's speech.audio.delta
// events.
type speechUsage struct {
	request Speech
	latestReport
	// clip is the answer's audio: an audio body's, known from its content type once
	// the backend answered; an event stream's, from its first delta's response_format
	// (format) — until then it relayed no audio.
	clip audioClip
	// deltas: an event stream's first speech.audio.delta was read; format is the
	// response_format it named.
	deltas bool
	format string
}

func newSpeechUsage(request Speech) *speechUsage {
	return &speechUsage{request: request, clip: emptyClip{}}
}

// streamEvent reads one SSE stream event by its data's type, each member by its exact
// name (exactMembers): a speech.audio.delta's audio, decoded as a JSON string, into the
// clip, in its response_format; speech.audio.done's usage. A member of an unexpected
// type is skipped, the others still read; data that is not a JSON object says nothing.
func (u *speechUsage) streamEvent(payload []byte) {
	ev, ok := exactMembers(payload)
	if !ok {
		return
	}
	eventType, _ := memberValue[string](ev, "type")
	switch eventType {
	case "speech.audio.delta":
		audio, _ := memberValue[string](ev, "audio")
		format, _ := memberValue[string](ev, "response_format")
		u.delta(audio, format)
	case "speech.audio.done":
		u.keep(speechEventUnits(ev["usage"]))
	}
}

// delta reads one delta's base64 audio — counted by its decoded size, only a leading
// WAV header decoded (writeBase64) — into the clip of the format the first delta names
// (formatClip: pcm at the request's sample_rate). A delta naming another format, or a
// WAV header that is not base64, leaves the answer's audio unmeasured.
func (u *speechUsage) delta(audio, format string) {
	switch {
	case !u.deltas:
		u.deltas, u.format = true, format
		u.clip = formatClip(format, u.request.SampleRate)
	case format != u.format:
		u.clip = unmeasuredClip{}
	}
	if !writeBase64(u.clip, audio) {
		u.clip = unmeasuredClip{}
	}
}

// contentMember: none — the body is audio.
func (u *speechUsage) contentMember() string { return "" }

func (u *speechUsage) bodyUsage(json.RawMessage) {}

func (u *speechUsage) bodyContentBytes(json.RawMessage) int64 { return 0 }

func (u *speechUsage) streamContentBytes() int64 { return 0 }

// answered reads a successful answer's headers. An audio body's content type says how
// its audio is measured, and on the whole clip they carry its token usage; an event
// stream is read by its events.
func (u *speechUsage) answered(header http.Header, stream bool) {
	if stream {
		return
	}
	u.clip = newAudioClip(header.Get("Content-Type"), u.request.SampleRate)
	if !u.request.RawStream {
		u.keep(speechHeaderUnits(header))
	}
}

func (u *speechUsage) bodyPiece(p []byte) { u.clip.write(p) }

// media: the audio read, in whole seconds rounded up; an answer whose audio cannot be
// measured counts 0, unmeasured.
func (u *speechUsage) media(bool) (Units, bool) {
	d, measured := u.clip.length()
	return Units{config.UnitAudioSecondsOut: wholeSeconds(d)}, measured
}

func (u *speechUsage) mediaSeen() bool { return u.clip.samplesSeen() }

// speechEventUnits reads the SSE stream's token usage, speech.audio.done's usage
// object — a batch result's has its shape — by its members' exact names
// (exactMembers): input_tokens and output_tokens both integers, else no report (a null,
// missing or malformed usage, a count missing, null or not an integer). A negative
// count is 0, as in every usage object the gateway reads.
func speechEventUnits(raw json.RawMessage) (Units, bool) {
	usage, ok := exactMembers(raw)
	if !ok {
		return nil, false
	}
	in, inOK := memberValue[int64](usage, "input_tokens")
	out, outOK := memberValue[int64](usage, "output_tokens")
	if !inOK || !outOK {
		return nil, false
	}
	return withEveryUnit(Units{config.UnitTokensIn: max(in, 0), config.UnitTokensOut: max(out, 0)}), true
}

// exactMembers decodes raw, a JSON object, into its members' raw values by their exact
// names after decoding, as the inbound stage reads a request's. JSON names are
// case-sensitive, but encoding/json matches a struct's fields case-insensitively: a
// member "Audio" or "OUTPUT_TOKENS" would be read as audio or output_tokens, over the
// API's own. A member named twice counts as its last, as encoding/json reads it. ok is
// false when raw is not a JSON object.
func exactMembers(raw []byte) (map[string]json.RawMessage, bool) {
	var members map[string]json.RawMessage
	if json.Unmarshal(raw, &members) != nil || members == nil {
		return nil, false
	}
	return members, true
}

// memberValue decodes the member named name into a T; ok is false when it is absent,
// null or not a T.
func memberValue[T any](members map[string]json.RawMessage, name string) (T, bool) {
	var v *T
	raw, present := members[name]
	if !present || json.Unmarshal(raw, &v) != nil || v == nil {
		var zero T
		return zero, false
	}
	return *v, true
}

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
