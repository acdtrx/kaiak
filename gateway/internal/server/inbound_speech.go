package server

import (
	"encoding/json"
	"fmt"
	"strings"

	"kaiak/internal/config"
)

// speechFormats names the audio formats the gateway serves on speech: the ones whose
// length it measures (docs/specs/GATEWAY.md, Accounting → Media usage).
const speechFormats = "wav and pcm"

// parseSpeechFields reads a speech request's own owned fields (docs/specs/GATEWAY.md,
// Client API → owned fields: speech), past those every format shares
// (parseOwnedFields; max_new_tokens is its output-limit key): the mode, from stream
// and stream_format; the audio format and its sample rate; the reference audio, which
// must be inline. The request counts toward the limits on audio generated, reserving
// nothing: its length is known only once relayed. Everything else — input, voice,
// instructions, task_type, word_timestamps, … — is the backend's and passes untouched.
func parseSpeechFields(rq *request, top map[string]json.RawMessage) *apiError {
	if apiErr := readSpeechMode(rq, top); apiErr != nil {
		return apiErr
	}
	if apiErr := readSpeechAudioFormat(rq, top); apiErr != nil {
		return apiErr
	}
	if apiErr := refuseExternalAudio(top); apiErr != nil {
		return apiErr
	}
	rq.inbound.Media = map[config.Measure]int64{config.MeasureAudioSecondsOut: 0}
	return nil
}

// readSpeechMode reads the mode as vLLM-Omni reads it (is_raw_audio_stream,
// is_sse_stream): stream_format "audio" is the raw-audio stream; else stream true or
// stream_format "sse" is the SSE stream; else the whole clip — any other
// stream_format passes for the backend to refuse, timed as the whole clip. The
// raw-audio stream takes the stream timers and sets gen_ai.request.stream
// (inbound.Stream). The gateway does not serve the SSE stream: it is refused, naming
// the member that asks for it.
func readSpeechMode(rq *request, top map[string]json.RawMessage) *apiError {
	format, apiErr := optionalField[string](top, "stream_format", "stream_format", "a string")
	if apiErr != nil {
		return apiErr
	}
	switch {
	case format != nil && *format == "audio":
		rq.inbound.Stream = true
		rq.inbound.Speech.RawStream = true
	case format != nil && *format == "sse":
		return errSpeechEventStream("stream_format")
	case rq.inbound.Stream:
		return errSpeechEventStream("stream")
	}
	return nil
}

// readSpeechAudioFormat reads response_format — wav or pcm, absent meaning wav
// (vLLM-Omni's default), anything else refused — and sample_rate, required with pcm:
// PCM carries no header, and a model's native rate differs from model to model, so
// the rate the request names is the one its PCM is measured at.
func readSpeechAudioFormat(rq *request, top map[string]json.RawMessage) *apiError {
	format, apiErr := optionalField[string](top, "response_format", "response_format", "a string")
	if apiErr != nil {
		return apiErr
	}
	pcm := false
	if format != nil {
		switch *format {
		case "wav":
		case "pcm":
			pcm = true
		default:
			return errResponseFormatUnsupported("response_format", speechFormats)
		}
	}
	rate, apiErr := optionalField[int64](top, "sample_rate", "sample_rate", "an integer")
	if apiErr != nil {
		return apiErr
	}
	if pcm && rate == nil {
		return errMissingSampleRate("sample_rate")
	}
	if rate != nil {
		rq.inbound.Speech.SampleRate = *rate
	}
	return nil
}

// refuseExternalAudio refuses a reference audio the backend would fetch or read
// (docs/specs/GATEWAY.md, Client API → owned fields: speech): ref_audio (a string, or
// each entry of a list), ref_audio_2 and each references entry's audio_path must be
// inline data. vLLM-Omni resolves them through vLLM's media connector, which fetches
// a URL and reads a file: URI. A null value is absent; references entries are read
// once each (a member named twice is refused).
func refuseExternalAudio(top map[string]json.RawMessage) *apiError {
	if apiErr := refuseExternalRefAudio(top["ref_audio"]); apiErr != nil {
		return apiErr
	}
	second, apiErr := optionalField[string](top, "ref_audio_2", "ref_audio_2", "a string")
	if apiErr != nil {
		return apiErr
	}
	if second != nil && !inlineAudio(*second) {
		return errExternalReference("ref_audio_2")
	}
	return eachObjectStrict(top["references"], "references", func(at string, entry map[string]json.RawMessage) *apiError {
		param := at + ".audio_path"
		path, apiErr := optionalField[string](entry, "audio_path", param, "a string")
		if apiErr != nil {
			return apiErr
		}
		if path != nil && !inlineAudio(*path) {
			return errExternalReference(param)
		}
		return nil
	})
}

// refuseExternalRefAudio holds ref_audio — a string, or a list of strings — to
// inline data.
func refuseExternalRefAudio(raw json.RawMessage) *apiError {
	const want = "a string or an array of strings"
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	if !startsWith(raw, '[') {
		var one string
		if json.Unmarshal(raw, &one) != nil {
			return errInvalidType("ref_audio", want)
		}
		if !inlineAudio(one) {
			return errExternalReference("ref_audio")
		}
		return nil
	}
	var list []json.RawMessage
	if json.Unmarshal(raw, &list) != nil {
		return errInvalidType("ref_audio", want)
	}
	for i, entry := range list {
		param := fmt.Sprintf("ref_audio[%d]", i)
		var one *string
		if json.Unmarshal(entry, &one) != nil || one == nil {
			return errInvalidType(param, "a string")
		}
		if !inlineAudio(*one) {
			return errExternalReference(param)
		}
	}
	return nil
}

// inlineAudio reports whether a reference audio is inline data: a data: URL, its
// scheme in any case, as URL schemes are read. An allowlist: whatever else a
// connector learns to fetch is refused until it is judged.
func inlineAudio(s string) bool {
	const scheme = "data:"
	return len(s) >= len(scheme) && strings.EqualFold(s[:len(scheme)], scheme)
}
