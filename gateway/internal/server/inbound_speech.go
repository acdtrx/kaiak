package server

import (
	"encoding/json"
	"fmt"

	"kaiak/internal/config"
)

// speechFormats names the audio formats the gateway serves on speech: the ones whose
// length it measures (docs/specs/GATEWAY.md, Accounting → Media usage).
const speechFormats = "wav and pcm"

// parseSpeechFields reads a speech request's own owned fields (docs/specs/GATEWAY.md,
// Client API → owned fields: speech), past those every format shares
// (parseOwnedFields; max_new_tokens is its output-limit key): extra_params, refused
// whatever its value, null included — vLLM-Omni passes it to the model's adapter,
// which resolves some keys as it resolves a reference audio (IndexTTS2's emo_audio),
// and which keys a model fetches the gateway cannot know; the mode, from stream and
// stream_format; the audio format and its sample rate; the reference audio, which must
// be inline. The request counts toward the limits on audio generated, reserving
// nothing: its length is known only once relayed. Everything else — input, voice,
// instructions, task_type, word_timestamps, … — is the backend's and passes untouched.
func parseSpeechFields(rq *request, top map[string]json.RawMessage) *apiError {
	if _, present := top["extra_params"]; present {
		return errExtraParams()
	}
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
// stream_format passes for the backend to refuse, timed as the whole clip. The two
// streams take the stream timers and set gen_ai.request.stream (inbound.Stream, which
// stream true has set already).
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
		rq.inbound.Stream = true
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
	if second != nil && !inlineData(*second) {
		return errExternalReference("ref_audio_2")
	}
	return eachObjectStrict(top["references"], "references", func(at string, entry map[string]json.RawMessage) *apiError {
		param := at + ".audio_path"
		path, apiErr := optionalField[string](entry, "audio_path", param, "a string")
		if apiErr != nil {
			return apiErr
		}
		if path != nil && !inlineData(*path) {
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
		if !inlineData(one) {
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
		if !inlineData(*one) {
			return errExternalReference(param)
		}
	}
	return nil
}

// parseSpeechBatchFields reads a speech batch's own owned fields
// (docs/specs/GATEWAY.md, Client API → owned fields: speech batch), past model and the
// batch's max_new_tokens (parseOwnedFields): items, counted against
// max_speech_batch_items; and, at the batch's level and in each item,
// response_format, sample_rate, ref_audio and max_new_tokens. vLLM-Omni runs each item
// as a speech request of its own, merged with the batch — an item's value wins, else
// the batch's (_merge_batch_item) — so each item's merged format and rate are held to
// speech's rules, the param naming the member that set the value; a reference audio
// must be inline wherever it is set. A batch answer is JSON, never a stream: stream is
// not read. Everything else — each item's input, voice, instructions, … — is the
// backend's and passes untouched, extra_params included: vLLM-Omni's batch and its
// items declare no such field, so it drops it at either level unread, and the merged
// request it runs carries none.
func parseSpeechBatchFields(rq *request, top map[string]json.RawMessage) *apiError {
	batch, apiErr := readSpeechBatchLevel(top, "")
	if apiErr != nil {
		return apiErr
	}
	items, apiErr := speechBatchItems(top["items"], rq.snapshot.MaxSpeechBatchItems)
	if apiErr != nil {
		return apiErr
	}
	rq.inbound.ItemOutputLimits = make([]*int64, len(items))
	rq.inbound.Speech.ItemSampleRates = make([]int64, len(items))
	i := 0
	apiErr = visitObjects(items, "items", true, func(at string, members map[string]json.RawMessage) *apiError {
		item, apiErr := readSpeechBatchLevel(members, at)
		if apiErr != nil {
			return apiErr
		}
		if rq.inbound.ItemOutputLimits[i], apiErr = optionalField[int64](members, "max_new_tokens", at+".max_new_tokens",
			"an integer"); apiErr != nil {
			return apiErr
		}
		rate, apiErr := mergedSpeechAudio(batch, item)
		if apiErr != nil {
			return apiErr
		}
		rq.inbound.Speech.ItemSampleRates[i] = rate
		i++
		return nil
	})
	if apiErr != nil {
		return apiErr
	}
	rq.inbound.Media = map[config.Measure]int64{config.MeasureAudioSecondsOut: 0}
	return nil
}

// speechBatchItems is a speech batch's items, each an element of the list: none when
// it is absent or null (the backend refuses that); a value that is not a list is
// refused, and so is a list longer than max_speech_batch_items — counted without
// decoding (listElements), so a long list of small items costs one pass to refuse,
// none of them held; only a list within the cap is decoded into its items.
func speechBatchItems(raw json.RawMessage, limit int64) ([]json.RawMessage, *apiError) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	if !startsWith(raw, '[') {
		return nil, errInvalidType("items", "an array")
	}
	if n := listElements(raw); n > limit {
		return nil, errTooManyItems(n, limit)
	}
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) != nil {
		return nil, errInvalidType("items", "an array")
	}
	return items, nil
}

// speechBatchLevel is what one level of a speech batch — the batch, or one of its
// items — sets of the audio vLLM-Omni merges: nil where it sets none. at is its path
// ("" for the batch, "items[2]" for an item), which params are named under.
type speechBatchLevel struct {
	at     string
	format *string
	rate   *int64
}

// param is the level's member's name as a param: the member at the batch's level,
// at.member in an item.
func (l speechBatchLevel) param(member string) string {
	if l.at == "" {
		return member
	}
	return l.at + "." + member
}

// readSpeechBatchLevel reads one level of a speech batch (members, at its path at):
// response_format and sample_rate, each of its type, and ref_audio, a string, which
// must be inline data wherever it is set (refuseExternalAudio's rule). Its
// max_new_tokens is the output limit's (the batch's: parseOwnedFields; an item's:
// parseSpeechBatchFields).
func readSpeechBatchLevel(members map[string]json.RawMessage, at string) (speechBatchLevel, *apiError) {
	l := speechBatchLevel{at: at}
	var apiErr *apiError
	if l.format, apiErr = optionalField[string](members, "response_format", l.param("response_format"), "a string"); apiErr != nil {
		return l, apiErr
	}
	if l.rate, apiErr = optionalField[int64](members, "sample_rate", l.param("sample_rate"), "an integer"); apiErr != nil {
		return l, apiErr
	}
	ref, apiErr := optionalField[string](members, "ref_audio", l.param("ref_audio"), "a string")
	if apiErr != nil {
		return l, apiErr
	}
	if ref != nil && !inlineData(*ref) {
		return l, errExternalReference(l.param("ref_audio"))
	}
	return l, nil
}

// mergedSpeechAudio holds an item's audio as vLLM-Omni merges it with the batch to
// speech's rules (readSpeechAudioFormat's): the format the item gets — its own, else
// the batch's, absent meaning wav — must be wav or pcm, and pcm needs the rate the
// item gets. A refusal names the member that set the value: the item's, or the
// batch's. The rate is the one the item's PCM comes at; 0 when it gets none.
func mergedSpeechAudio(batch, item speechBatchLevel) (int64, *apiError) {
	formatSet, rate := item, item.rate
	if item.format == nil {
		formatSet = batch
	}
	if rate == nil {
		rate = batch.rate
	}
	if f := formatSet.format; f != nil && *f != "wav" && *f != "pcm" {
		return 0, errResponseFormatUnsupported(formatSet.param("response_format"), speechFormats)
	}
	if f := formatSet.format; f != nil && *f == "pcm" && rate == nil {
		return 0, errMissingSampleRate(formatSet.param("sample_rate"))
	}
	if rate == nil {
		return 0, nil
	}
	return *rate, nil
}
